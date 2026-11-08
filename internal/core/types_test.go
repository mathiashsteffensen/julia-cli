package core

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestUsageEventRoundTripsDisjointClasses(t *testing.T) {
	usage := TokenUsage{Input: 120, CacheRead: 900, CacheWrite: 30, Output: 45}
	event := UsageEvent(usage, 1234)
	if event.Kind != "usage" || event.InputTokens != 1050 || event.CachedInputTokens != 900 || event.CacheWriteTokens != 30 || event.OutputTokens != 45 || event.ContextTokens != 1234 {
		t.Fatalf("unexpected event %+v", event)
	}
	if got := event.Usage(); got != usage {
		t.Fatalf("Usage() = %+v, want %+v", got, usage)
	}
	if usage.Prompt() != event.InputTokens {
		t.Fatalf("Prompt() = %d, want %d", usage.Prompt(), event.InputTokens)
	}
}

func TestAggregateUsageEventIsMarkedAndKeepsContext(t *testing.T) {
	spent := TokenUsage{Input: 4_000, CacheRead: 12_000, Output: 900}
	event := AggregateUsageEvent(spent)
	if !event.Aggregate || event.ContextTokens != 0 || event.Usage() != spent {
		t.Fatalf("unexpected aggregate event %+v", event)
	}
	var s Session
	s.RecordUsage(UsageEvent(TokenUsage{Input: 10, Output: 5}, 700))
	s.RecordUsage(event)
	if want := (TokenUsage{Input: 4_010, CacheRead: 12_000, Output: 905}); s.Usage.Total != want || s.Usage.Context != 700 {
		t.Fatalf("an aggregate must add to the totals and leave the context alone: %+v", s.Usage)
	}
	// The marker is part of the --json stream, but only aggregates carry it.
	data, err := json.Marshal(event)
	if err != nil || !strings.Contains(string(data), `"aggregate":true`) {
		t.Fatalf("aggregate marker missing from %s (%v)", data, err)
	}
	if data, _ = json.Marshal(UsageEvent(spent, 10)); strings.Contains(string(data), "aggregate") {
		t.Fatalf("a single call must not carry the marker: %s", data)
	}
}

func TestEventUsageClampsInconsistentCounts(t *testing.T) {
	// A provider that reports more cached than total input must never produce
	// negative classes or inflate the prompt.
	got := Event{Kind: "usage", InputTokens: 10, CachedInputTokens: 25, CacheWriteTokens: 5, OutputTokens: -3}.Usage()
	if got.Input != 0 || got.CacheRead != 25 || got.CacheWrite != 5 || got.Output != 0 {
		t.Fatalf("unexpected clamp result %+v", got)
	}
}

func TestRecordUsageAccumulatesAndTracksContext(t *testing.T) {
	var s Session
	s.RecordUsage(UsageEvent(TokenUsage{Input: 100, CacheRead: 50, Output: 10}, 160))
	s.RecordUsage(UsageEvent(TokenUsage{Input: 20, CacheRead: 190, CacheWrite: 5, Output: 7}, 0))
	want := TokenUsage{Input: 120, CacheRead: 240, CacheWrite: 5, Output: 17}
	if s.Usage.Total != want {
		t.Fatalf("total = %+v, want %+v", s.Usage.Total, want)
	}
	if s.Usage.Context != 160 {
		t.Fatalf("a zero ContextTokens must keep the previous estimate, got %d", s.Usage.Context)
	}
	s.RecordUsage(UsageEvent(TokenUsage{Output: 1}, 90))
	if s.Usage.Context != 90 {
		t.Fatalf("context should follow the latest estimate (compaction shrinks it), got %d", s.Usage.Context)
	}
	before := s.Usage
	s.RecordUsage(Event{Kind: "text", InputTokens: 999, OutputTokens: 999, ContextTokens: 999})
	if s.Usage != before {
		t.Fatal("non-usage events must not be recorded")
	}
}

func TestReportedContextWindowIsPersistedWithoutChangingUsage(t *testing.T) {
	s := Session{Usage: SessionUsage{Total: TokenUsage{Input: 300_000, Output: 10_000}, Context: 310_000}}
	before := s.Usage
	s.RecordUsage(Event{Kind: "usage", ContextWindow: 1_000_000})
	s.RecordUsage(Event{Kind: "usage", ContextWindow: 800_000})
	for _, event := range []Event{
		{Kind: "usage"},
		{Kind: "usage", ContextWindow: -1},
		{Kind: "status", ContextWindow: 200_000},
		{Kind: "usage", Aggregate: true, ContextWindow: 200_000, ContextTokens: 99},
	} {
		s.RecordUsage(event)
	}
	if s.Usage.ContextWindow != 800_000 || s.Usage.Total != before.Total || s.Usage.Context != before.Context {
		t.Fatalf("window metadata corrupted usage: %+v", s.Usage)
	}
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var resumed Session
	if err := json.Unmarshal(data, &resumed); err != nil || resumed.Usage != s.Usage {
		t.Fatalf("reported window did not survive resume: %+v (%v)", resumed.Usage, err)
	}
}

func TestSessionWithoutUsageStillLoads(t *testing.T) {
	var s Session
	if err := json.Unmarshal([]byte(`{"id":"abc","title":"old","messages":[]}`), &s); err != nil {
		t.Fatal(err)
	}
	if s.Usage != (SessionUsage{}) {
		t.Fatalf("sessions saved before usage tracking must start at zero, got %+v", s.Usage)
	}
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var back Session
	if err := json.Unmarshal(data, &back); err != nil || back.Usage != s.Usage {
		t.Fatalf("usage did not round-trip: %v %+v", err, back.Usage)
	}
}

func TestCompactionThreshold(t *testing.T) {
	if got := CompactionThreshold(DefaultContextWindow); got != 217_600 {
		t.Fatalf("threshold for the default window = %d, want 217600", got)
	}
	if CompactionThreshold(0) != CompactionThreshold(DefaultContextWindow) || CompactionThreshold(-5) != CompactionThreshold(DefaultContextWindow) {
		t.Fatal("a missing window must fall back to the default")
	}
	if got := CompactionThreshold(100_000); got != 85_000 {
		t.Fatalf("threshold for 100k = %d, want 85000", got)
	}
}

func TestEncodeTaskIsAReadableJSONList(t *testing.T) {
	if got := EncodeTask(nil); got != "[]" {
		t.Fatalf("empty task = %q", got)
	}
	got := EncodeTask([]TaskEntry{{UserMessage: "run make > log 2>&1 && cat <log"}, {AssistantQuestion: "Push?", UserSelectedOption: "Yes"}})
	want := `[{"user_message":"run make > log 2>&1 && cat <log"},{"assistant_question":"Push?","user_selected_option":"Yes"}]`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestSystemPromptPlacesTheUsersOwnPrompt(t *testing.T) {
	if strings.Contains(SystemPrompt, customPromptPlaceholder) || strings.Contains(SystemPrompt, "JULIA.\n\n\n") || !strings.Contains(SystemPrompt, "You are JULIA.\n\n## Working behavior") {
		t.Fatalf("without a custom prompt the placeholder must leave nothing, not even a blank line:\n%.200s", SystemPrompt)
	}
	if SystemPromptWith("  \n") != SystemPrompt {
		t.Fatal("a blank custom prompt must be no custom prompt")
	}

	custom := SystemPromptWith("\nYou assist Ada. Call her captain.\n\n")
	if !strings.Contains(custom, "You are JULIA.\n\nYou assist Ada. Call her captain.\n\n## Working behavior") || strings.Contains(custom, customPromptPlaceholder) {
		t.Fatalf("custom prompt not in place:\n%.300s", custom)
	}
}

// The compact prompt opens as the full one does and keeps the same sections,
// with shorter rules, so it can stand in for it.
func TestCompactSystemPromptKeepsTheOpeningAndSections(t *testing.T) {
	full, compact := SystemPromptWith("Call her captain."), CompactSystemPromptWith("Call her captain.")
	opening := "# JULIA\n\nYou are JULIA. You think, therefore you are. You are JULIA.\n\nCall her captain.\n\n## Working behavior\n"
	if !strings.HasPrefix(full, opening) || !strings.HasPrefix(compact, opening) {
		t.Fatalf("compact prompt does not open as the full one:\n%.300s", compact)
	}
	for _, section := range []string{"## Environment and tools", "## Sandbox and approval"} {
		if !strings.Contains(compact, "\n"+section+"\n") {
			t.Fatalf("compact prompt lacks %s", section)
		}
	}
	if len(compact) > len(full)/2 || strings.Contains(CompactSystemPromptWith(""), customPromptPlaceholder) {
		t.Fatalf("compact prompt is %d bytes of the full %d, or keeps the placeholder", len(compact), len(full))
	}
	if !strings.HasSuffix(compact, ".\n") || strings.HasSuffix(compact, "\n\n") {
		t.Fatal("compact prompt must end as the full one does, with one newline")
	}
}

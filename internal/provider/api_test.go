package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openaicompat"
	"jarvis/internal/core"
)

type testTools struct{ calls int }

func (t *testTools) Definitions() []core.ToolDefinition {
	return []core.ToolDefinition{{Name: "List", Description: "List workspace files", Parameters: map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}, "required": []string{"path"}}}}
}

type scriptedAPIModel struct {
	fantasy.LanguageModel
	parts []fantasy.StreamPart
	// steps, when set, scripts each stream in turn instead of repeating parts; the last one repeats.
	steps         [][]fantasy.StreamPart
	streams       int
	streamErr     error
	generations   int
	calls         []fantasy.Call
	generateCalls []fantasy.Call
	generated     *fantasy.Response
}

func (m *scriptedAPIModel) Provider() string { return "test" }
func (m *scriptedAPIModel) Model() string    { return "test" }
func (m *scriptedAPIModel) Stream(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	m.streams++
	m.calls = append(m.calls, call)
	if m.streamErr != nil {
		return nil, m.streamErr
	}
	parts := m.parts
	if len(m.steps) > 0 {
		parts = m.steps[min(m.streams, len(m.steps))-1]
	}
	return func(yield func(fantasy.StreamPart) bool) {
		for _, part := range parts {
			if !yield(part) {
				return
			}
		}
	}, nil
}
func (m *scriptedAPIModel) Generate(_ context.Context, call fantasy.Call) (*fantasy.Response, error) {
	m.generations++
	m.generateCalls = append(m.generateCalls, call)
	if m.generated != nil {
		return m.generated, nil
	}
	return nil, errors.New("unexpected generation")
}

func TestCompatibleRefreshesInstructionsAcrossRuns(t *testing.T) {
	requests := make(chan []map[string]any, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []map[string]any `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		requests <- body.Messages
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"a\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Done.\"},\"finish_reason\":null}]}\n\ndata: {\"id\":\"a\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	backend, err := NewAPI(context.Background(), APIConfig{Provider: "compatible", Model: "fixture", BaseURL: server.URL + "/v1"})
	if err != nil {
		t.Fatal(err)
	}
	session := &core.Session{Instructions: "Runtime policy FIRST_BUNDLE"}
	for _, instructions := range []string{"Runtime policy FIRST_BUNDLE", "Runtime policy REFRESHED_BUNDLE"} {
		session.Instructions = instructions
		if err = backend.Run(context.Background(), session, "Inspect the fixture", &testTools{}, func(core.Event) {}); err != nil {
			t.Fatal(err)
		}
	}
	for i, want := range []string{"Runtime policy FIRST_BUNDLE", "Runtime policy REFRESHED_BUNDLE"} {
		messages := <-requests
		count := 0
		for _, message := range messages {
			if message["role"] == "system" {
				count++
				if message["content"] != want {
					t.Fatalf("request %d system=%v", i, message["content"])
				}
			}
		}
		if count != 1 {
			t.Fatalf("request %d system messages=%d", i, count)
		}
	}
	encoded, err := json.Marshal(session)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "BUNDLE") {
		t.Fatal("transient runtime policy persisted in session history")
	}
}

func TestAPICompactionRetainsRuntimeInstructionsAndIsolatesReview(t *testing.T) {
	const runtimeInstructions = "Runtime policy COMPACTION_POLICY_SENTINEL with current workspace and permissions."
	model := &scriptedAPIModel{parts: []fantasy.StreamPart{{Type: fantasy.StreamPartTypeTextStart, ID: "text"}, {Type: fantasy.StreamPartTypeTextDelta, ID: "text", Delta: "Done."}, {Type: fantasy.StreamPartTypeTextEnd, ID: "text"}, {Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop}}, generated: &fantasy.Response{FinishReason: fantasy.FinishReasonStop, Content: fantasy.ResponseContent{fantasy.TextContent{Text: "Previously inspected the fixture; continue verification."}}}}
	session := &core.Session{Instructions: runtimeInstructions, Messages: []core.Message{{Role: "user", Content: strings.Repeat("prior user context ", 80)}, {Role: "assistant", Content: "Inspected files."}, {Role: "user", Content: "Continue carefully."}}}
	backend := &API{Model: model, ContextTokens: 100, OpenRouter: true, Reasoning: "high"}
	if err := backend.Run(context.Background(), session, "Verify results", &testTools{}, func(core.Event) {}); err != nil {
		t.Fatal(err)
	}
	if model.generations != 1 || model.streams != 1 {
		t.Fatalf("compaction not exercised: generate=%d streams=%d", model.generations, model.streams)
	}
	options := model.generateCalls[0].ProviderOptions[openaicompat.Name].(*openaicompat.ProviderOptions)
	if options.ReasoningEffort != nil {
		t.Fatal("compaction forced an unverified OpenRouter effort")
	}
	if len(model.calls[0].Prompt) == 0 || model.calls[0].Prompt[0].Role != fantasy.MessageRoleSystem {
		t.Fatal("missing system instructions after compaction")
	}
	first, _ := json.Marshal(model.calls[0].Prompt[0])
	if !strings.Contains(string(first), runtimeInstructions) {
		t.Fatal("runtime policy was summarized away")
	}
	if strings.Contains(string(session.State), "COMPACTION_POLICY_SENTINEL") {
		t.Fatal("runtime instructions leaked into compacted history")
	}
	model.generated = &fantasy.Response{Content: fantasy.ResponseContent{fantasy.TextContent{Text: `{"allow":false,"reason":"Not authorized"}`}}}
	approval := core.Approval{Call: core.ToolCall{Name: "Bash", Arguments: json.RawMessage(`{"command":"printf hello"}`)}, Task: "Inspect the workspace", ExecutionMode: "unsandboxed", Directory: "/tmp/fixture", Justification: "Need access outside workspace", Network: true}
	if _, err := backend.Review(context.Background(), approval); err != nil {
		t.Fatal(err)
	}
	request := model.generateCalls[len(model.generateCalls)-1]
	encoded, _ := json.Marshal(request.Prompt)
	if strings.Contains(string(encoded), "COMPACTION_POLICY_SENTINEL") {
		t.Fatal("execution policy leaked into isolated reviewer")
	}
	if request.ToolChoice == nil || *request.ToolChoice != fantasy.ToolChoiceNone {
		t.Fatal("reviewer gained tools")
	}
	var payload map[string]any
	part, ok := request.Prompt[1].Content[0].(fantasy.TextPart)
	if !ok {
		t.Fatal("review payload was not text")
	}
	if err := json.Unmarshal([]byte(part.Text), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["execution_mode"] != "unsandboxed" || payload["directory"] != "/tmp/fixture" || payload["justification"] != "Need access outside workspace" || payload["network"] != true {
		t.Fatalf("reviewer received wrong execution boundary: %+v", payload)
	}
}

func TestUnsupportedOpenRouterEffortErrorIsReturned(t *testing.T) {
	model := &scriptedAPIModel{streamErr: errors.New("unsupported reasoning effort for this model")}
	backend := &API{Model: model, OpenRouter: true, Reasoning: "high"}
	err := backend.Run(context.Background(), &core.Session{}, "Inspect the fixture", &testTools{}, func(core.Event) {})
	if err == nil || !strings.Contains(err.Error(), "unsupported reasoning effort") {
		t.Fatalf("unsupported model effort error was hidden: %v", err)
	}
	if len(model.calls) == 0 || len(model.calls[0].ProviderOptions) == 0 {
		t.Fatal("selected OpenRouter effort was not applied to the model call")
	}
}

func TestAPIRejectsMissingOrUnknownCompletion(t *testing.T) {
	for _, reason := range []fantasy.FinishReason{"", fantasy.FinishReasonUnknown, fantasy.FinishReasonOther, fantasy.FinishReasonToolCalls} {
		t.Run(string(reason), func(t *testing.T) {
			model := &scriptedAPIModel{parts: []fantasy.StreamPart{{Type: fantasy.StreamPartTypeTextStart, ID: "text"}, {Type: fantasy.StreamPartTypeTextDelta, ID: "text", Delta: "Partial answer"}, {Type: fantasy.StreamPartTypeTextEnd, ID: "text"}, {Type: fantasy.StreamPartTypeFinish, FinishReason: reason}}}
			backend := &API{Model: model}
			session := &core.Session{}
			executor := &testTools{}
			err := backend.Run(context.Background(), session, "Inspect the fixture", executor, func(core.Event) {})
			if err == nil {
				t.Fatal("unconfirmed response reported successful")
			}
			if model.streams != 1 || executor.calls != 0 {
				t.Fatalf("unexpected replay: streams=%d tools=%d", model.streams, executor.calls)
			}
			if len(session.Messages) < 2 || session.Messages[0].Content != "Inspect the fixture" || !strings.Contains(session.Messages[1].Content, "Partial answer") {
				t.Fatalf("lost transcript: %+v", session.Messages)
			}
		})
	}
}

func TestAPIPartialStreamFailureRetainsPromptAndText(t *testing.T) {
	model := &scriptedAPIModel{parts: []fantasy.StreamPart{{Type: fantasy.StreamPartTypeTextStart, ID: "text"}, {Type: fantasy.StreamPartTypeTextDelta, ID: "text", Delta: "Verified the first file."}, {Type: fantasy.StreamPartTypeError, Error: errors.New("fixture connection interrupted")}}}
	var checkpoint []byte
	backend := &API{Model: model, Checkpoint: func(session *core.Session) error { checkpoint = append([]byte(nil), session.State...); return nil }}
	session := &core.Session{}
	executor := &testTools{}
	err := backend.Run(context.Background(), session, "Inspect both files", executor, func(core.Event) {})
	if err == nil {
		t.Fatal("interrupted stream reported success")
	}
	if len(session.Messages) != 2 || session.Messages[0].Content != "Inspect both files" || !strings.Contains(session.Messages[1].Content, "Verified the first file.") || !strings.Contains(session.Messages[1].Content, "interrupted") {
		t.Fatalf("lost partial response: %+v", session.Messages)
	}
	if !strings.Contains(string(checkpoint), "Inspect both files") {
		t.Fatal("accepted prompt was not checkpointed before stream failure")
	}
	if executor.calls != 0 {
		t.Fatal("stream failure executed a tool")
	}
}

func TestAPIPendingToolBlocksResumeBeforeAnyActivity(t *testing.T) {
	model := &scriptedAPIModel{}
	executor := &testTools{}
	checkpoints := 0
	backend := &API{Model: model, Checkpoint: func(*core.Session) error { checkpoints++; return nil }}
	session := &core.Session{PendingTool: &core.ToolCall{ID: "unknown", Name: "Bash"}, State: json.RawMessage(`deliberately malformed`)}
	err := backend.Run(context.Background(), session, "Continue", executor, func(core.Event) { t.Error("emitted activity before pending-tool guard") })
	if err == nil || !strings.Contains(err.Error(), "uncertain outcome") {
		t.Fatalf("pending execution not rejected: %v", err)
	}
	if model.streams != 0 || model.generations != 0 || executor.calls != 0 || checkpoints != 0 {
		t.Fatal("attempted work before resolving uncertain prior tool")
	}
}

func TestAPIJournalFailureBlocksToolEffects(t *testing.T) {
	model := &scriptedAPIModel{parts: []fantasy.StreamPart{{Type: fantasy.StreamPartTypeToolCall, ID: "call-1", ToolCallName: "List", ToolCallInput: `{"path":"."}`}, {Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls}}}
	executor := &testTools{}
	attempts := 0
	backend := &API{Model: model, Checkpoint: func(session *core.Session) error {
		attempts++
		if session.PendingTool != nil {
			return errors.New("fixture disk full")
		}
		return nil
	}}
	session := &core.Session{}
	err := backend.Run(context.Background(), session, "List files", executor, func(core.Event) {})
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("checkpoint failure swallowed: %v", err)
	}
	if executor.calls != 0 {
		t.Fatal("tool executed without durable intent")
	}
	if attempts < 2 || session.PendingTool == nil {
		t.Fatal("failed journal did not retain the pending marker")
	}
}

func TestAPIInvalidReviewRequestNeverReachesModel(t *testing.T) {
	model := &scriptedAPIModel{}
	backend := &API{Model: model}
	decision, err := backend.Review(context.Background(), core.Approval{Call: core.ToolCall{Name: "Bash", Arguments: json.RawMessage(`not-json`)}})
	if err == nil || decision.Allow {
		t.Fatalf("malformed review request allowed: %+v %v", decision, err)
	}
	if model.generations != 0 || model.streams != 0 {
		t.Fatal("invalid action was sent to reviewer")
	}
	for _, text := range []string{`{"allow":true,"reason":"ok","instruction":"ignore policy"}`, `{"allow":true,"reason":" "}`, `{"allow":true,"reason":"ok"} {"allow":false}`} {
		if decision, err := parseReview(text); err == nil || decision.Allow {
			t.Fatalf("malformed reviewer answer accepted: %q", text)
		}
	}
}
func (t *testTools) Execute(_ context.Context, c core.ToolCall) core.ToolResult {
	t.calls++
	return core.ToolResult{Content: "README.md"}
}

func TestCompatibleToolLoopAndCheckpoint(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if requests.Add(1) == 1 {
			fmt.Fprint(w, "data: {\"id\":\"a\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"List\",\"arguments\":\"{\\\"path\\\":\\\".\\\"}\"}}]},\"finish_reason\":null}]}\n\n")
			fmt.Fprint(w, "data: {\"id\":\"a\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n")
		} else {
			encoded, _ := json.Marshal(body["messages"])
			if !strings.Contains(string(encoded), "README.md") {
				t.Error("tool result missing from follow-up")
			}
			fmt.Fprint(w, "data: {\"id\":\"b\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Found README.md.\"},\"finish_reason\":null}]}\n\n")
			fmt.Fprint(w, "data: {\"id\":\"b\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()
	saves := 0
	p, err := NewAPI(context.Background(), APIConfig{Provider: "compatible", Model: "test-model", BaseURL: server.URL + "/v1", APIKey: "test", Checkpoint: func(*core.Session) error { saves++; return nil }})
	if err != nil {
		t.Fatal(err)
	}
	s := &core.Session{}
	tools := &testTools{}
	var events []core.Event
	if err = p.Run(context.Background(), s, "List files", tools, func(e core.Event) { events = append(events, e) }); err != nil {
		t.Fatal(err)
	}
	if tools.calls != 1 || requests.Load() != 2 || saves < 3 {
		t.Fatalf("calls=%d requests=%d saves=%d", tools.calls, requests.Load(), saves)
	}
	if len(s.State) == 0 || len(s.Messages) < 4 {
		t.Fatalf("missing persistence: %#v", s)
	}
	var found bool
	for _, e := range events {
		if e.Kind == "text" && e.Text == "Found README.md." {
			found = true
		}
	}
	if !found {
		t.Fatal("text delta not emitted")
	}
}

func TestReviewFailsClosed(t *testing.T) {
	for _, s := range []string{`{}`, `{"reason":"yes"}`, `{"allow":"true","reason":"yes"}`, `{"allow":true}`, `{"allow":false,"reason":"no"} trailing`, "```json\n{\"allow\":true,\"reason\":\"yes\"}\n```"} {
		if _, err := parseReview(s); err == nil {
			t.Errorf("accepted invalid decision %s", s)
		}
	}
	d, err := parseReview(`{"allow":false,"reason":"Task does not authorize deletion"}`)
	if err != nil || d.Allow {
		t.Fatal("denial not preserved")
	}
}

func TestRejectInsecureProviderEndpoint(t *testing.T) {
	for _, u := range []string{"http://example.com/v1", "https://user:secret@example.com", "file:///tmp/api"} {
		if _, err := NewAPI(context.Background(), APIConfig{Provider: "compatible", Model: "x", BaseURL: u}); err == nil {
			t.Errorf("accepted %s", u)
		}
	}
}

func textStep(text string, u fantasy.Usage) []fantasy.StreamPart {
	return []fantasy.StreamPart{{Type: fantasy.StreamPartTypeTextStart, ID: "text"}, {Type: fantasy.StreamPartTypeTextDelta, ID: "text", Delta: text}, {Type: fantasy.StreamPartTypeTextEnd, ID: "text"}, {Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop, Usage: u}}
}

func toolStep(u fantasy.Usage) []fantasy.StreamPart {
	return []fantasy.StreamPart{{Type: fantasy.StreamPartTypeToolCall, ID: "call-1", ToolCallName: "List", ToolCallInput: `{"path":"."}`}, {Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls, Usage: u}}
}

func usageEvents(events []core.Event) []core.Event {
	var usage []core.Event
	for _, e := range events {
		if e.Kind == "usage" {
			usage = append(usage, e)
		}
	}
	return usage
}

func decodeHistory(t *testing.T, s *core.Session) []fantasy.Message {
	t.Helper()
	var history []fantasy.Message
	if err := json.Unmarshal(s.State, &history); err != nil {
		t.Fatal(err)
	}
	return history
}

func priorConversation(size int) []core.Message {
	return []core.Message{{Role: "user", Content: strings.Repeat("x", size)}, {Role: "assistant", Content: "Inspected files."}, {Role: "user", Content: "Continue carefully."}}
}

func TestNormalizeUsage(t *testing.T) {
	for _, tt := range []struct {
		name, provider string
		in             fantasy.Usage
		want           core.TokenUsage
	}{
		{"anthropic reports uncached input", "anthropic", fantasy.Usage{InputTokens: 100, OutputTokens: 20, CacheReadTokens: 900, CacheCreationTokens: 50}, core.TokenUsage{Input: 100, CacheRead: 900, CacheWrite: 50, Output: 20}},
		{"openai reports uncached input", "openai", fantasy.Usage{InputTokens: 40, OutputTokens: 5, CacheReadTokens: 960}, core.TokenUsage{Input: 40, CacheRead: 960, Output: 5}},
		{"compatible reports uncached input", "compatible", fantasy.Usage{InputTokens: 7, OutputTokens: 3, CacheReadTokens: 7}, core.TokenUsage{Input: 7, CacheRead: 7, Output: 3}},
		{"google prompt count includes cached content", "google", fantasy.Usage{InputTokens: 1000, OutputTokens: 30, CacheReadTokens: 600}, core.TokenUsage{Input: 400, CacheRead: 600, Output: 30}},
		{"google without cache", "google", fantasy.Usage{InputTokens: 1000, OutputTokens: 30}, core.TokenUsage{Input: 1000, Output: 30}},
		// fantasy keeps the first streamed prompt count but sums every chunk's cache reads.
		{"google cache reads repeated on several chunks", "google", fantasy.Usage{InputTokens: 10_000, CacheReadTokens: 27_000, OutputTokens: 37}, core.TokenUsage{CacheRead: 10_000, Output: 37}},
		{"google cached beyond prompt", "google", fantasy.Usage{InputTokens: 100, CacheReadTokens: 250}, core.TokenUsage{CacheRead: 100}},
		{"reasoning is already part of output", "openai", fantasy.Usage{InputTokens: 10, OutputTokens: 90, ReasoningTokens: 60}, core.TokenUsage{Input: 10, Output: 90}},
		{"negative counts are clamped", "anthropic", fantasy.Usage{InputTokens: -5, OutputTokens: -1, CacheReadTokens: -2, CacheCreationTokens: -3}, core.TokenUsage{}},
		{"nothing reported", "", fantasy.Usage{}, core.TokenUsage{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizeUsage(tt.provider, tt.in); got != tt.want {
				t.Fatalf("normalizeUsage(%q, %+v) = %+v, want %+v", tt.provider, tt.in, got, tt.want)
			}
		})
	}
}

func TestEstimateTokensIsConservativeAndRoundsUp(t *testing.T) {
	if got := estimateTokens(strings.Repeat("a", 398)); got != 100 {
		t.Fatalf("400 bytes of JSON should be 100 tokens, got %d", got)
	}
	if got := estimateTokens(strings.Repeat("a", 399)); got != 101 {
		t.Fatalf("401 bytes of JSON should round up to 101 tokens, got %d", got)
	}
	if estimateToolResults([]fantasy.Message{fantasy.NewUserMessage("only an assistant or user message")}) != 0 {
		t.Fatal("messages other than tool results must not be counted in the tail")
	}
}

func TestNewAPIPassesContextWindowThrough(t *testing.T) {
	backend, err := NewAPI(context.Background(), APIConfig{Provider: "compatible", Model: "fixture", BaseURL: "https://example.com/v1", ContextTokens: 123_456})
	if err != nil {
		t.Fatal(err)
	}
	if backend.ContextTokens != 123_456 {
		t.Fatalf("context window = %d", backend.ContextTokens)
	}
}

func TestAPIRecordsUsageAfterEveryStepBeforeCheckpointing(t *testing.T) {
	model := &scriptedAPIModel{steps: [][]fantasy.StreamPart{
		toolStep(fantasy.Usage{InputTokens: 100, CacheReadTokens: 900, CacheCreationTokens: 20, OutputTokens: 30}),
		textStep("Done.", fantasy.Usage{InputTokens: 50, CacheReadTokens: 1100, OutputTokens: 10}),
	}}
	var snapshots []core.SessionUsage
	backend := &API{Model: model, Provider: "anthropic", Checkpoint: func(s *core.Session) error { snapshots = append(snapshots, s.Usage); return nil }}
	session := &core.Session{}
	var events []core.Event
	if err := backend.Run(context.Background(), session, "List files", &testTools{}, func(e core.Event) { events = append(events, e) }); err != nil {
		t.Fatal(err)
	}
	history := decodeHistory(t, session)
	if len(history) != 4 || history[2].Role != fantasy.MessageRoleTool {
		t.Fatalf("unexpected history shape: %d messages", len(history))
	}
	firstContext := 1050 + estimateTokens(history[2:3])
	usage := usageEvents(events)
	if len(usage) != 2 {
		t.Fatalf("want one usage event per step, got %+v", usage)
	}
	if got := usage[0]; got.InputTokens != 1020 || got.CachedInputTokens != 900 || got.CacheWriteTokens != 20 || got.OutputTokens != 30 || got.ContextTokens != firstContext {
		t.Fatalf("first usage event %+v, want context %d", got, firstContext)
	}
	if got := usage[1]; got.InputTokens != 1150 || got.CachedInputTokens != 1100 || got.CacheWriteTokens != 0 || got.OutputTokens != 10 || got.ContextTokens != 1160 {
		t.Fatalf("second usage event %+v", got)
	}
	wantTotal := core.TokenUsage{Input: 150, CacheRead: 2000, CacheWrite: 20, Output: 40}
	if session.Usage.Total != wantTotal || session.Usage.Context != 1160 {
		t.Fatalf("session usage %+v, want total %+v context 1160", session.Usage, wantTotal)
	}
	var first, last core.SessionUsage
	for _, s := range snapshots {
		if s.Total == (core.TokenUsage{Input: 100, CacheRead: 900, CacheWrite: 20, Output: 30}) {
			first = s
		}
		last = s
	}
	if first.Context != firstContext {
		t.Fatalf("usage of the first step was not in the checkpoint that followed it: %+v", snapshots)
	}
	if last != session.Usage {
		t.Fatalf("final checkpoint %+v lost usage %+v", last, session.Usage)
	}
}

func TestAPINormalizesUsageForTheConfiguredProvider(t *testing.T) {
	model := &scriptedAPIModel{parts: textStep("Done.", fantasy.Usage{InputTokens: 1000, CacheReadTokens: 600, OutputTokens: 30})}
	session := &core.Session{}
	var events []core.Event
	if err := (&API{Model: model, Provider: "google"}).Run(context.Background(), session, "Inspect", &testTools{}, func(e core.Event) { events = append(events, e) }); err != nil {
		t.Fatal(err)
	}
	if want := (core.TokenUsage{Input: 400, CacheRead: 600, Output: 30}); session.Usage.Total != want {
		t.Fatalf("google total = %+v, want %+v", session.Usage.Total, want)
	}
	if usage := usageEvents(events); len(usage) != 1 || usage[0].InputTokens != 1000 || usage[0].ContextTokens != 1030 {
		t.Fatalf("the event must carry the whole prompt once: %+v", usage)
	}
}

func TestAPIMovesContextGaugeWhenProviderReportsNoUsage(t *testing.T) {
	const instructions = "Runtime policy USAGE_ESTIMATE"
	model := &scriptedAPIModel{parts: textStep("Done, with no usage block.", fantasy.Usage{})}
	session := &core.Session{Instructions: instructions}
	var events []core.Event
	if err := (&API{Model: model}).Run(context.Background(), session, "Inspect the fixture", &testTools{}, func(e core.Event) { events = append(events, e) }); err != nil {
		t.Fatal(err)
	}
	history := decodeHistory(t, session)
	want := estimateTokens(history[:1]) + estimateTokens(instructions) + estimateTokens(history[1:])
	usage := usageEvents(events)
	if len(usage) != 1 || usage[0].Usage() != (core.TokenUsage{}) || usage[0].ContextTokens != want {
		t.Fatalf("usage events %+v, want an empty report with context %d", usage, want)
	}
	if !session.Usage.Total.IsZero() || session.Usage.Context != want {
		t.Fatalf("session usage %+v", session.Usage)
	}
}

func TestAPIDecidesCompactionFromTheContextEstimate(t *testing.T) {
	small, large := priorConversation(10), priorConversation(8000)
	for _, tt := range []struct {
		name     string
		window   int
		persists int
		messages []core.Message
		compacts bool
	}{
		{"persisted figure below threshold wins over a large history", 1000, 100, large, false},
		{"persisted figure plus the new prompt crosses the threshold", 1000, 849, small, true},
		{"no persisted figure estimates the history from scratch", 1000, 0, large, true},
		{"no persisted figure and a small history", 1_000_000, 0, small, false},
		{"a prompt and one earlier message are too short to summarize", 1000, 0, large[:1], false},
		{"nothing earlier than the prompt is too short to summarize", 1000, 50_000, nil, false},
		{"zero window assumes the default", 0, core.CompactionThreshold(core.DefaultContextWindow) - 1, small, true},
		{"zero window leaves headroom below the default threshold", 0, core.CompactionThreshold(core.DefaultContextWindow) - 100, small, false},
		{"negative window assumes the default", -1, core.CompactionThreshold(core.DefaultContextWindow) - 100, small, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			model := &scriptedAPIModel{parts: textStep("Done.", fantasy.Usage{InputTokens: 10, OutputTokens: 2}), generated: completeSummary("Continue the review.")}
			session := &core.Session{Messages: tt.messages, Usage: core.SessionUsage{Context: tt.persists}}
			if err := (&API{Model: model, ContextTokens: tt.window}).Run(context.Background(), session, "Verify results", &testTools{}, func(core.Event) {}); err != nil {
				t.Fatal(err)
			}
			if compacted := model.generations == 1; compacted != tt.compacts || model.generations > 1 {
				t.Fatalf("compactions = %d, want compaction=%t", model.generations, tt.compacts)
			}
		})
	}
}

func TestAPICompactionIsCountedAndShrinksTheContext(t *testing.T) {
	const instructions = "Runtime policy COMPACTION_USAGE"
	summary := completeSummary("Inspected the fixture; continue verification.")
	summary.Usage = fantasy.Usage{InputTokens: 700, CacheReadTokens: 300, OutputTokens: 40}
	model := &scriptedAPIModel{parts: textStep("Done.", fantasy.Usage{InputTokens: 60, OutputTokens: 9}), generated: summary}
	var snapshots []core.SessionUsage
	backend := &API{Model: model, ContextTokens: 100, Checkpoint: func(s *core.Session) error { snapshots = append(snapshots, s.Usage); return nil }}
	session := &core.Session{Instructions: instructions, Messages: priorConversation(2000), Usage: core.SessionUsage{Total: core.TokenUsage{Input: 5, Output: 5}, Context: 90_000}}
	var events []core.Event
	if err := backend.Run(context.Background(), session, "Verify results", &testTools{}, func(e core.Event) { events = append(events, e) }); err != nil {
		t.Fatal(err)
	}
	history := decodeHistory(t, session)
	compactedContext := estimateTokens(history[:2]) + estimateTokens(instructions)
	usage := usageEvents(events)
	if len(usage) != 2 {
		t.Fatalf("want a compaction event and a step event, got %+v", usage)
	}
	if got := usage[0]; got.InputTokens != 1000 || got.CachedInputTokens != 300 || got.OutputTokens != 40 || got.ContextTokens != compactedContext {
		t.Fatalf("compaction event %+v, want context %d", got, compactedContext)
	}
	if usage[1].InputTokens != 60 || usage[1].ContextTokens != 69 {
		t.Fatalf("step event %+v", usage[1])
	}
	if want := (core.TokenUsage{Input: 765, CacheRead: 300, Output: 54}); session.Usage.Total != want || session.Usage.Context != 69 {
		t.Fatalf("session usage %+v, want total %+v", session.Usage, want)
	}
	persisted := false
	for _, s := range snapshots {
		persisted = persisted || (s.Total == core.TokenUsage{Input: 705, CacheRead: 300, Output: 45} && s.Context == compactedContext)
	}
	if !persisted {
		t.Fatalf("compaction usage was not checkpointed with the compacted history: %+v", snapshots)
	}
}

func TestAPIFailedCompactionStillCountsSpentTokens(t *testing.T) {
	truncated := &fantasy.Response{FinishReason: fantasy.FinishReasonLength, Content: fantasy.ResponseContent{fantasy.TextContent{Text: "Partial"}}, Usage: fantasy.Usage{InputTokens: 500, CacheReadTokens: 100, OutputTokens: 8192}}
	model := &compactionModel{responses: []*fantasy.Response{truncated, truncated}}
	backend := &API{Model: model, ContextTokens: 100}
	session := &core.Session{Messages: priorConversation(2000), Usage: core.SessionUsage{Context: 50_000}}
	var events []core.Event
	err := backend.Run(context.Background(), session, "Verify results", &testTools{}, func(e core.Event) { events = append(events, e) })
	if err == nil || !strings.Contains(err.Error(), "compact context") {
		t.Fatalf("compaction failure not reported: %v", err)
	}
	if model.streams != 0 || len(model.generateCalls) != 2 {
		t.Fatalf("streams=%d summaries=%d", model.streams, len(model.generateCalls))
	}
	if want := (core.TokenUsage{Input: 1000, CacheRead: 200, Output: 16384}); session.Usage.Total != want {
		t.Fatalf("both failed attempts must be counted: %+v, want %+v", session.Usage.Total, want)
	}
	if session.Usage.Context != 50_000 {
		t.Fatalf("a failed compaction must leave the context estimate alone, got %d", session.Usage.Context)
	}
	if usage := usageEvents(events); len(usage) != 1 || usage[0].InputTokens != 1200 || usage[0].ContextTokens != 0 {
		t.Fatalf("usage events %+v", usage)
	}
}

// promptSizedModel reports each step's usage as the size of the prompt it was
// sent, as a real provider does; the first toolSteps steps call a tool.
type promptSizedModel struct {
	scriptedAPIModel
	toolSteps int
}

func (m *promptSizedModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	usage := fantasy.Usage{InputTokens: int64(estimateTokens(call.Prompt)), OutputTokens: 2}
	m.parts = textStep("Done.", usage)
	if m.streams < m.toolSteps {
		m.parts = toolStep(usage)
	}
	return m.scriptedAPIModel.Stream(ctx, call)
}

func TestAPITaskLargerThanTheThresholdDoesNotCompactOnEveryStep(t *testing.T) {
	// The task is retained verbatim, so no summary can bring the context under the
	// threshold; compacting after every step would only repeat the same spend.
	const window = 32_000
	task := strings.Repeat("alpha beta ", 10_000)
	if estimateTokens(task) < core.CompactionThreshold(window) {
		t.Fatal("test no longer exercises a task over the threshold")
	}
	for _, toolSteps := range []int{0, 5} {
		t.Run(fmt.Sprintf("%d tool steps", toolSteps), func(t *testing.T) {
			model := &promptSizedModel{scriptedAPIModel: scriptedAPIModel{generated: completeSummary("Continue the review.")}, toolSteps: toolSteps}
			session := &core.Session{Messages: priorConversation(10)}
			var events []core.Event
			if err := (&API{Model: model, ContextTokens: window}).Run(context.Background(), session, task, &testTools{}, func(e core.Event) { events = append(events, e) }); err != nil {
				t.Fatal(err)
			}
			if model.generations != 1 || model.streams != toolSteps+1 {
				t.Fatalf("compactions=%d streams=%d, want 1 and %d", model.generations, model.streams, toolSteps+1)
			}
			if got := usageEvents(events)[len(usageEvents(events))-1].InputTokens; got < core.CompactionThreshold(window) {
				t.Fatalf("the steps must report a prompt over the threshold, got %d", got)
			}
		})
	}
}

// compactionTrackingModel notes how many compactions had run when each step began.
type compactionTrackingModel struct {
	scriptedAPIModel
	compactionsAtStep []int
}

func (m *compactionTrackingModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	m.compactionsAtStep = append(m.compactionsAtStep, m.generations)
	return m.scriptedAPIModel.Stream(ctx, call)
}

func TestAPICompactsWhenReportedUsageCrossesTheThresholdMidRun(t *testing.T) {
	const window = 1000 // compaction threshold 850
	quiet := fantasy.Usage{InputTokens: 100, OutputTokens: 5}
	final := textStep("Done.", fantasy.Usage{InputTokens: 30, OutputTokens: 5})
	for _, tt := range []struct {
		name       string
		steps      [][]fantasy.StreamPart
		compacting []int // compactions that had run when each step began
	}{
		{"below the threshold throughout", [][]fantasy.StreamPart{toolStep(quiet), toolStep(quiet), final}, []int{0, 0, 0}},
		{"uncached prompt crosses after the first step", [][]fantasy.StreamPart{toolStep(fantasy.Usage{InputTokens: 900, OutputTokens: 5}), final}, []int{0, 1}},
		{"prompt served entirely from cache still counts", [][]fantasy.StreamPart{toolStep(fantasy.Usage{CacheReadTokens: 900, OutputTokens: 5}), final}, []int{0, 1}},
		{"prompt written to the cache still counts", [][]fantasy.StreamPart{toolStep(fantasy.Usage{CacheCreationTokens: 900, OutputTokens: 5}), final}, []int{0, 1}},
		{"crosses after the second step", [][]fantasy.StreamPart{toolStep(quiet), toolStep(fantasy.Usage{InputTokens: 200, CacheReadTokens: 700, OutputTokens: 5}), final}, []int{0, 0, 1}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			model := &compactionTrackingModel{scriptedAPIModel: scriptedAPIModel{steps: tt.steps, generated: completeSummary("Continue the review.")}}
			session := &core.Session{Instructions: "Short runtime policy."}
			var events []core.Event
			if err := (&API{Model: model, ContextTokens: window}).Run(context.Background(), session, "List files", &testTools{}, func(e core.Event) { events = append(events, e) }); err != nil {
				t.Fatal(err)
			}
			if fmt.Sprint(model.compactionsAtStep) != fmt.Sprint(tt.compacting) {
				t.Fatalf("compactions before each step = %v, want %v", model.compactionsAtStep, tt.compacting)
			}
			compacted := tt.compacting[len(tt.compacting)-1] > 0
			if model.generations > 1 || (model.generations == 1) != compacted {
				t.Fatalf("summaries = %d, want compaction=%t", model.generations, compacted)
			}
			if !compacted {
				return
			}
			// The big step's usage, then the notice, then the shrunken context.
			var order []string
			for _, e := range events {
				switch {
				case e.Kind == "status" && strings.HasPrefix(e.Text, "Compacting"):
					order = append(order, "compacting")
				case e.Kind == "usage" && e.ContextTokens >= core.CompactionThreshold(window):
					order = append(order, "full")
				case e.Kind == "usage" && e.ContextTokens > 0 && e.ContextTokens < core.CompactionThreshold(window) && len(order) > 0 && order[len(order)-1] == "compacting":
					order = append(order, "shrunk")
				}
			}
			if got := strings.Join(order, " "); !strings.HasSuffix(got, "full compacting shrunk") {
				t.Fatalf("event order %q, want the oversized step, the compaction notice, then the shrunken context", got)
			}
			history := decodeHistory(t, session)
			if len(history) != 3 || !strings.HasPrefix(history[0].Content[0].(fantasy.TextPart).Text, "Continuation notes") {
				t.Fatalf("history after compaction has %d messages, want the notes, the task and the final answer", len(history))
			}
			if session.Usage.Context != 35 {
				t.Fatalf("context = %d, want the final step's prompt and output (35)", session.Usage.Context)
			}
		})
	}
}

func TestAPIContextOverflowForcesCompactionOnTheNextRun(t *testing.T) {
	// A provider that reports no usage leaves only the byte estimate, which can
	// undercount; the provider's own refusal is the one reliable correction.
	const window = 100_000
	model := &scriptedAPIModel{streamErr: &fantasy.ProviderError{Message: "context_length_exceeded", ContextTooLargeErr: true}, generated: completeSummary("Continue the review.")}
	backend := &API{Model: model, ContextTokens: window}
	session := &core.Session{Messages: priorConversation(10), Usage: core.SessionUsage{Context: 1_000}}
	err := backend.Run(context.Background(), session, "Verify results", &testTools{}, func(core.Event) {})
	if err == nil || !strings.Contains(err.Error(), "provider stream") {
		t.Fatalf("overflow not reported: %v", err)
	}
	if got := core.CompactionThreshold(window); session.Usage.Context != got {
		t.Fatalf("context after an overflow = %d, want the threshold %d", session.Usage.Context, got)
	}
	model.streamErr, model.parts = nil, textStep("Done.", fantasy.Usage{InputTokens: 50, OutputTokens: 5})
	if err = backend.Run(context.Background(), session, "Verify again", &testTools{}, func(core.Event) {}); err != nil {
		t.Fatal(err)
	}
	if model.generations != 1 {
		t.Fatalf("the run after an overflow made %d summaries, want 1", model.generations)
	}
	// Any other stream failure leaves the estimate alone.
	other := &scriptedAPIModel{streamErr: errors.New("connection reset")}
	session = &core.Session{Usage: core.SessionUsage{Context: 1_000}}
	if err = (&API{Model: other, ContextTokens: window}).Run(context.Background(), session, "Verify results", &testTools{}, func(core.Event) {}); err == nil || session.Usage.Context != 1_000 {
		t.Fatalf("an unrelated failure moved the estimate: err=%v context=%d", err, session.Usage.Context)
	}
}

func TestAPIGoogleRepeatedCacheReadsDoNotInflateTheContext(t *testing.T) {
	// Three streamed usage chunks each reported 9000 cached of a 10000-token prompt.
	model := &scriptedAPIModel{parts: textStep("Done.", fantasy.Usage{InputTokens: 10_000, CacheReadTokens: 27_000, OutputTokens: 37})}
	session := &core.Session{}
	var events []core.Event
	if err := (&API{Model: model, Provider: "google"}).Run(context.Background(), session, "Inspect", &testTools{}, func(e core.Event) { events = append(events, e) }); err != nil {
		t.Fatal(err)
	}
	usage := usageEvents(events)
	if len(usage) != 1 || usage[0].InputTokens != 10_000 || usage[0].ContextTokens != 10_037 {
		t.Fatalf("the prompt must be counted once, not once per chunk: %+v", usage)
	}
	if want := (core.TokenUsage{CacheRead: 10_000, Output: 37}); session.Usage.Total != want {
		t.Fatalf("total = %+v, want %+v", session.Usage.Total, want)
	}
}

func TestAPIReviewsNeverTouchSessionUsage(t *testing.T) {
	model := &scriptedAPIModel{parts: textStep("Done.", fantasy.Usage{InputTokens: 10, OutputTokens: 2})}
	backend := &API{Model: model}
	session := &core.Session{}
	var events []core.Event
	if err := backend.Run(context.Background(), session, "Inspect", &testTools{}, func(e core.Event) { events = append(events, e) }); err != nil {
		t.Fatal(err)
	}
	before, emitted := session.Usage, len(events)
	model.generated = &fantasy.Response{Content: fantasy.ResponseContent{fantasy.TextContent{Text: `{"allow":true,"reason":"Authorized"}`}}, Usage: fantasy.Usage{InputTokens: 900, OutputTokens: 50}}
	approval := core.Approval{Task: "Inspect", Call: core.ToolCall{Name: "Bash", Arguments: json.RawMessage(`{"command":"pwd"}`)}}
	if _, err := backend.Review(context.Background(), approval); err != nil {
		t.Fatal(err)
	}
	if session.Usage != before || len(events) != emitted {
		t.Fatalf("approval review leaked into the conversation: usage %+v -> %+v, events %d -> %d", before, session.Usage, emitted, len(events))
	}
}

func TestAPIStepOutputLimitLeavesRoomForReasoning(t *testing.T) {
	for _, tc := range []struct {
		window int
		want   int64
	}{{0, 32_000}, {1_000_000, maxStepOutputTokens}, {100_000, 12_500}, {32_000, minStepOutputTokens}} {
		model := &scriptedAPIModel{parts: textStep("Done.", fantasy.Usage{InputTokens: 10, OutputTokens: 2})}
		if err := (&API{Model: model, ContextTokens: tc.window}).Run(context.Background(), &core.Session{}, "Review", &testTools{}, func(core.Event) {}); err != nil {
			t.Fatal(err)
		}
		if got := model.calls[0].MaxOutputTokens; got == nil || *got != tc.want {
			t.Fatalf("window %d: max output %v, want %d", tc.window, got, tc.want)
		}
		if tc.want > int64(max(tc.window, core.DefaultContextWindow)-core.CompactionThreshold(tc.window)) {
			t.Fatalf("window %d: limit %d exceeds the headroom above the compaction threshold", tc.window, tc.want)
		}
	}
}

func TestAPIReasoningOnlyTruncationSaysSo(t *testing.T) {
	usage := fantasy.Usage{InputTokens: 10, OutputTokens: 32_000}
	model := &scriptedAPIModel{parts: []fantasy.StreamPart{
		{Type: fantasy.StreamPartTypeReasoningStart, ID: "r"}, {Type: fantasy.StreamPartTypeReasoningDelta, ID: "r", Delta: "Thinking"}, {Type: fantasy.StreamPartTypeReasoningEnd, ID: "r"},
		{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonLength, Usage: usage},
	}}
	err := (&API{Model: model}).Run(context.Background(), &core.Session{}, "Review", &testTools{}, func(core.Event) {})
	if err == nil || !strings.Contains(err.Error(), "reasoning") || !strings.Contains(err.Error(), "32000") {
		t.Fatalf("reasoning-only truncation not explained: %v", err)
	}
}

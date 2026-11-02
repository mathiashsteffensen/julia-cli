package miner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"julia/internal/core"
)

var testTools = []core.ToolDefinition{{Name: "Read", Parameters: map[string]any{
	"type": "object", "required": []string{"path"}, "additionalProperties": false,
	"properties": map[string]any{"path": map[string]any{"type": "string"}, "options": map[string]any{"type": "object", "properties": map[string]any{"mode": map[string]any{"type": "string", "enum": []string{"one", "two"}}}, "additionalProperties": false}},
}}}

func id(n string) string { return strings.Repeat(n, 24) }
func turn(text, callID, args string) []core.Message {
	return []core.Message{{Role: "user", Content: text}, {Role: "assistant", ToolCalls: []core.ToolCall{{ID: callID, Name: "Read", Arguments: json.RawMessage(args)}}}, {Role: "tool", ToolCallID: callID, Content: "output"}, {Role: "assistant", Content: "Done."}}
}
func writeSession(t *testing.T, dir string, s core.Session) []byte {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, s.ID+".json"), b, 0600); err != nil {
		t.Fatal(err)
	}
	return b
}
func sessionFor(id string, messages []core.Message) core.Session {
	return core.Session{ID: id, Directory: "/work", Provider: "test", Model: "m", Title: "title", UpdatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Messages: messages}
}

func TestValidateMessages(t *testing.T) {
	valid := turn("inspect", "a", `{"path":"file","options":{"mode":"one"}}`)
	cases := []struct {
		name     string
		messages []core.Message
		valid    bool
	}{
		{"valid", valid, true},
		{"assistant progress", append([]core.Message{{Role: "user", Content: "inspect"}, {Role: "assistant", Content: "I will inspect it."}}, valid[1:]...), true},
		{"orphan", []core.Message{{Role: "user", Content: "hi"}, {Role: "tool", ToolCallID: "a"}, {Role: "assistant", Content: "done"}}, false},
		{"dangling", valid[:2], false},
		{"interrupted", []core.Message{{Role: "user", Content: "hi"}, {Role: "assistant", Content: "[Response interrupted]"}}, false},
		{"image marker", []core.Message{{Role: "user", Content: "hi"}, {Role: "assistant", Content: "[image: PNG]"}}, false},
		{"image", []core.Message{{Role: "user", Content: "hi", Images: []core.Image{{MediaType: "image/png"}}}, {Role: "assistant", Content: "done"}}, false},
		{"unknown", turn("inspect", "a", `{"path":"file"}`), false},
		{"missing", turn("inspect", "a", `{}`), false},
		{"wrong type", turn("inspect", "a", `{"path":3}`), false},
		{"nested enum", turn("inspect", "a", `{"path":"file","options":{"mode":"three"}}`), false},
		{"nested extra", turn("inspect", "a", `{"path":"file","options":{"extra":true}}`), false},
		{"secret", turn("inspect", "a", `{"path":"password=abcd1234"}`), false},
		// Short in raw text, long enough once JSON escapes the line break.
		{"secret lengthened by JSON", []core.Message{{Role: "user", Content: "What does export token=$1\nexec ./run do?"}, {Role: "assistant", Content: "It runs ./run."}}, false},
		// Export writes arguments as a JSON string, escaping their quotes again.
		{"secret lengthened by export", turn("inspect", "a", `{"path":"token=abc"}`), false},
		{"escaped secret", turn("inspect", "a", `{"path":"password=\u0061bcd1234"}`), false},
	}
	cases[7].messages[1].ToolCalls[0].Name = "Unknown"
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateMessages(tc.messages, testTools)
			if (err == nil) != tc.valid {
				t.Fatalf("validation error = %v", err)
			}
		})
	}
	private := turn("inspect", "x", `{"path":"x"}`)
	private[1].ToolCalls[0].Name = "Slack"
	defs := append(append([]core.ToolDefinition{}, testTools...), core.ToolDefinition{Name: "Slack", Parameters: testTools[0].Parameters})
	if err := ValidateMessages(private, defs); err == nil {
		t.Fatal("accepted private messaging tool")
	}
}

func TestScanDedupGroupFeedbackAndReadOnly(t *testing.T) {
	dir := t.TempDir()
	parent := sessionFor(id("1"), nil)
	parent.Directory = "/other"
	writeSession(t, dir, parent)
	first := sessionFor(id("2"), append(turn("Inspect this", "call-one", `{"path":"file", "options":{"mode":"one"}}`), core.Message{Role: "user", Content: "No, you missed the issue."}, core.Message{Role: "assistant", Content: "I will check."}))
	first.ParentID = parent.ID
	before := writeSession(t, dir, first)
	duplicate := sessionFor(id("3"), turn("Inspect this", "different", `{"options":{"mode":"one"},"path":"file"}`))
	writeSession(t, dir, duplicate)
	if err := os.WriteFile(filepath.Join(dir, id("4")+".json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, first.ID+".json"), filepath.Join(dir, id("5")+".json")); err != nil {
		t.Fatal(err)
	}
	r, err := Scan(context.Background(), dir, ScanOptions{Workspace: "/work"}, testTools)
	if err != nil {
		t.Fatal(err)
	}
	if r.Stats.Duplicates != 1 || r.Stats.Skipped["invalid_session"] != 1 || r.Stats.Skipped["non_regular"] != 1 {
		t.Fatalf("stats: %+v", r.Stats)
	}
	var found *Candidate
	for i := range r.Candidates {
		if r.Candidates[i].Source.SessionID == first.ID && r.Candidates[i].Source.Start == 0 {
			found = &r.Candidates[i]
		}
	}
	if found == nil || found.Source.GroupID != parent.ID || found.Feedback != "No, you missed the issue." || found.Source.End != 4 {
		t.Fatalf("candidate: %+v", found)
	}
	if len(found.Messages) != 4 {
		t.Fatal("feedback leaked into transcript")
	}
	after, err := os.ReadFile(filepath.Join(dir, first.ID+".json"))
	if err != nil || string(before) != string(after) {
		t.Fatal("source changed")
	}
	again, err := Scan(context.Background(), dir, ScanOptions{Workspace: "/work"}, testTools)
	if err != nil || !reflect.DeepEqual(r.Candidates, again.Candidates) {
		t.Fatalf("unstable order: %v", err)
	}
}

func TestScanRejectsUnsafeAndOversizedTurns(t *testing.T) {
	dir := t.TempDir()
	// One session each: an unusable turn would otherwise also make every turn
	// after it unusable as context.
	for i, messages := range [][]core.Message{
		{{Role: "user", Content: "secret"}, {Role: "assistant", Content: "password=abc12345"}},
		{{Role: "user", Content: "image"}, {Role: "assistant", Content: "[image: PNG]"}},
		{{Role: "user", Content: "orphan"}, {Role: "tool", ToolCallID: "x"}, {Role: "assistant", Content: "done"}},
		{{Role: "user", Content: "large"}, {Role: "assistant", Content: strings.Repeat("x", 1000)}},
		{{Role: "user", Content: "good"}, {Role: "assistant", Content: "done"}, {Role: "user", Content: "No, password=abc12345"}, {Role: "assistant", Content: "done"}},
	} {
		writeSession(t, dir, sessionFor(id(fmt.Sprint(i+1)), messages))
	}
	r, err := Scan(context.Background(), dir, ScanOptions{MaxBytes: 300}, testTools)
	if err != nil {
		t.Fatal(err)
	}
	if r.Stats.Skipped["invalid_turn"] < 4 || r.Stats.Skipped["too_large"] != 1 || r.Stats.Skipped["sensitive_feedback"] != 1 || len(r.Candidates) != 0 {
		t.Fatalf("stats/candidates: %+v %+v", r.Stats, r.Candidates)
	}
}

func TestScanGroupingBoundsAndCancellation(t *testing.T) {
	dir := t.TempDir()
	a := sessionFor(id("a"), []core.Message{{Role: "user", Content: "a"}, {Role: "assistant", Content: "answer"}})
	b := sessionFor(id("b"), a.Messages)
	a.ParentID = b.ID
	b.ParentID = a.ID
	writeSession(t, dir, a)
	writeSession(t, dir, b)
	orphan := sessionFor(id("c"), []core.Message{{Role: "user", Content: "c"}, {Role: "assistant", Content: "answer"}})
	orphan.ParentID = id("d")
	writeSession(t, dir, orphan)
	r, err := Scan(context.Background(), dir, ScanOptions{}, testTools)
	if err != nil {
		t.Fatal(err)
	}
	if r.Stats.Skipped["cyclic_parent"] != 2 || len(r.Candidates) != 1 || r.Candidates[0].Source.GroupID != id("d") {
		t.Fatalf("groups: %+v %+v", r.Stats, r.Candidates)
	}
	if _, err := Scan(context.Background(), dir, ScanOptions{Limit: maxCandidates + 1}, testTools); err == nil {
		t.Fatal("accepted bad limit")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Scan(ctx, dir, ScanOptions{}, testTools); err != context.Canceled {
		t.Fatalf("cancel: %v", err)
	}
}

func TestScanSkipsSteeredTurnsWhole(t *testing.T) {
	dir := t.TempDir()
	steer := "Actually, also update the changelog."
	messages := append(turn("Fix the parser.", "r1", `{"path":"parser.go"}`)[:3],
		core.Message{Role: "assistant", Content: "Found it. Next I will edit parser.go."},
		core.Message{Role: "user", Content: steer},
		core.Message{Role: "assistant", Content: "Done: parser and changelog updated."},
		core.Message{Role: "user", Content: "What is 2 + 2?"},
		core.Message{Role: "assistant", Content: "4."})
	session := sessionFor(id("9"), messages)
	session.Audit = []core.AuditEntry{{Event: core.Event{Kind: "steer", Text: steer}}}
	writeSession(t, dir, session)

	report, err := Scan(context.Background(), dir, ScanOptions{}, testTools)
	if err != nil {
		t.Fatal(err)
	}
	if report.Stats.Skipped["steered_turn"] != 1 || report.Stats.Turns != 2 || len(report.Candidates) != 1 {
		t.Fatalf("steered turn was split or kept: %+v %+v", report.Stats, report.Candidates)
	}
	if kept := report.Candidates[0]; kept.Source.Start != 6 || kept.Messages[0].Content != "What is 2 + 2?" {
		t.Fatalf("wrong turn kept: %+v", kept)
	}
	// The steered turn is no target, but the next turn's model saw it.
	if prior := report.Candidates[0].Context; len(prior) != 6 || prior[4].Content != steer {
		t.Fatalf("steered turn missing from context: %+v", prior)
	}
}

func TestScanCarriesEarlierTurnsAsContext(t *testing.T) {
	dir := t.TempDir()
	messages := append(turn("Read parser.go", "read", `{"path":"parser.go"}`),
		core.Message{Role: "assistant", ToolCalls: []core.ToolCall{{ID: "reflection-0-1", Name: "Memory", Arguments: json.RawMessage(`{"action":"save"}`)}}},
		core.Message{Role: "tool", ToolCallID: "reflection-0-1", Content: "Saved a note."},
		core.Message{Role: "user", Content: "Summarize the parser."},
		core.Message{Role: "assistant", Content: "It tokenizes, then\n\n[Response interrupted]"},
		core.Message{Role: "user", Content: "Go ahead and fix the bug you found."},
		core.Message{Role: "assistant", Content: "Fixed."})
	writeSession(t, dir, sessionFor(id("1"), messages))
	// The same final turn without the conversation before it is another example.
	writeSession(t, dir, sessionFor(id("2"), messages[8:]))

	report, err := Scan(context.Background(), dir, ScanOptions{}, testTools)
	if err != nil {
		t.Fatal(err)
	}
	if report.Stats.Duplicates != 0 || report.Stats.Skipped["invalid_turn"] != 1 || len(report.Candidates) != 3 {
		t.Fatalf("stats/candidates: %+v %+v", report.Stats, report.Candidates)
	}
	var later *Candidate
	for i, c := range report.Candidates {
		if c.Source.Start == 0 && len(c.Context) != 0 {
			t.Fatalf("a first turn has context: %+v", c)
		}
		if c.Source.Start == 8 {
			later = &report.Candidates[i]
		}
	}
	if later == nil || later.Source.End != 10 || len(later.Messages) != 2 {
		t.Fatalf("later turn missing or misplaced: %+v", later)
	}
	// The interrupted reply is no target, but it is what the model saw; the
	// reflection bookkeeping is not.
	if len(later.Context) != 6 || later.Context[0].Content != "Read parser.go" || !strings.Contains(later.Context[5].Content, "[Response interrupted]") {
		t.Fatalf("wrong context: %+v", later.Context)
	}

	// The bound counts the context: here the turn fits alone, not with it.
	turnJSON, _ := json.Marshal(messages[8:])
	bounded, err := Scan(context.Background(), dir, ScanOptions{MaxBytes: len(turnJSON) + len(`""`)}, testTools)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range bounded.Candidates {
		if len(c.Context) != 0 {
			t.Fatalf("context exceeded the byte bound: %+v", c)
		}
	}
	if len(bounded.Candidates) != 1 || bounded.Stats.Skipped["too_large"] != 2 {
		t.Fatalf("bounded stats: %+v", bounded.Stats)
	}
}

func TestScanKeepsImagesOutOfContext(t *testing.T) {
	dir := t.TempDir()
	const imageBytes = "image-bytes-sentinel"
	attached := core.Image{MediaType: "image/png", Width: 4, Height: 2, Data: []byte(imageBytes)}
	screenshot := turn("Open the page.", "shot", `{"path":"page.png"}`)
	screenshot[0].Images = []core.Image{attached}
	screenshot[2].Content = "[image: PNG · 1440×900 · 312 KB]"
	messages := append(screenshot,
		core.Message{Role: "user", Images: []core.Image{attached}},
		core.Message{Role: "assistant", Content: "A chart."},
		core.Message{Role: "user", Content: "What is 2 + 2?"},
		core.Message{Role: "assistant", Content: "4."})
	writeSession(t, dir, sessionFor(id("1"), messages))

	report, err := Scan(context.Background(), dir, ScanOptions{}, testTools)
	if err != nil {
		t.Fatal(err)
	}
	// A turn with an image is no target, only context.
	if report.Stats.Skipped["invalid_turn"] != 2 || report.Stats.Skipped["invalid_context"] != 0 || len(report.Candidates) != 1 {
		t.Fatalf("stats/candidates: %+v %+v", report.Stats, report.Candidates)
	}
	prior := report.Candidates[0].Context
	if len(prior) != 6 || prior[0].Content != "Open the page.\n[image: PNG · 4×2 · 20 B]" || prior[2].Content != screenshot[2].Content || prior[4].Content != "[image: PNG · 4×2 · 20 B]" {
		t.Fatalf("images not elided to markers: %+v", prior)
	}

	data, _ := json.Marshal(report)
	encoded, _ := json.Marshal([]byte(imageBytes))
	if strings.Contains(string(data), strings.Trim(string(encoded), `"`)) {
		t.Fatal("image bytes reached the report")
	}
	var prompt string
	model := &miningTestCompleter{run: func(_ context.Context, _, input string, _ any) (string, core.TokenUsage, error) {
		prompt = input
		answer, _ := json.Marshal(judgedAssessment())
		return string(answer), core.TokenUsage{}, nil
	}}
	if err := Judge(context.Background(), report, model, time.Second, func() error { return nil }, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "[image: PNG") || strings.Contains(prompt, strings.Trim(string(encoded), `"`)) {
		t.Fatalf("judge prompt: %s", prompt)
	}

	// The bound counts the marker, not the bytes it replaced.
	huge := attached
	huge.Data = make([]byte, 1<<20)
	messages[0].Images = []core.Image{huge}
	writeSession(t, dir, sessionFor(id("1"), messages))
	bounded, err := Scan(context.Background(), dir, ScanOptions{MaxBytes: 4096}, testTools)
	if err != nil || len(bounded.Candidates) != 1 {
		t.Fatalf("image bytes counted against the bound: %v %+v", err, bounded.Stats)
	}
}

func TestScanChecksTextAsJudgeAndExportEncodeIt(t *testing.T) {
	dir := t.TempDir()
	secret := func() []core.Message {
		return []core.Message{{Role: "user", Content: "What does export token=$1\nexec ./run do?"}, {Role: "assistant", Content: "It runs ./run."}}
	}
	clean := func() []core.Message {
		return []core.Message{{Role: "user", Content: "And 3 + 3?"}, {Role: "assistant", Content: "6."}}
	}
	writeSession(t, dir, sessionFor(id("8"), secret()))
	writeSession(t, dir, sessionFor(id("9"), []core.Message{
		{Role: "user", Content: "What is 2 + 2?"}, {Role: "assistant", Content: "4."},
		{Role: "user", Content: "No, use token=$1\ninstead."}, {Role: "assistant", Content: "Understood."},
	}))
	writeSession(t, dir, sessionFor(id("7"), clean()))
	writeSession(t, dir, sessionFor(id("6"), append(secret(), clean()...)))

	report, err := Scan(context.Background(), dir, ScanOptions{}, testTools)
	if err != nil {
		t.Fatal(err)
	}
	// Three turns carry the secret, and one is followed by it. The clean turn
	// after a secret would carry it as context.
	if report.Stats.Skipped["invalid_turn"] != 3 || report.Stats.Skipped["sensitive_feedback"] != 1 || report.Stats.Skipped["invalid_context"] != 1 || len(report.Candidates) != 1 {
		t.Fatalf("escaped secrets reached the report: %+v", report.Stats)
	}
	// Every candidate a scan keeps must reach the model: one the judge's own
	// check refuses would stop each run at the same card.
	model := &miningTestCompleter{run: func(context.Context, string, string, any) (string, core.TokenUsage, error) {
		data, _ := json.Marshal(judgedAssessment())
		return string(data), core.TokenUsage{}, nil
	}}
	if err := Judge(context.Background(), report, model, time.Second, func() error { return nil }, nil); err != nil || model.calls != 1 {
		t.Fatalf("judge stopped: %v, calls %d", err, model.calls)
	}
}

func TestScanBalancesCategories(t *testing.T) {
	dir := t.TempDir()
	var messages []core.Message
	for _, word := range []string{"one", "two", "three"} {
		messages = append(messages, turn(word, word, `{"path":"file"}`)...)
	}
	messages = append(messages, core.Message{Role: "user", Content: "direct question"}, core.Message{Role: "assistant", Content: "answer"})
	writeSession(t, dir, sessionFor(id("f"), messages))
	r, err := Scan(context.Background(), dir, ScanOptions{Limit: 2}, testTools)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Candidates) != 2 || r.Stats.Omitted != 2 || len(r.Candidates[1].Messages) != 2 {
		t.Fatalf("not balanced: %+v %+v", r.Stats, r.Candidates)
	}
}

func TestScanBoundsIncludeFeedbackAndLargeFiles(t *testing.T) {
	dir := t.TempDir()
	messages := []core.Message{{Role: "user", Content: "question"}, {Role: "assistant", Content: "answer"}, {Role: "user", Content: "No, " + strings.Repeat("wrong ", 100)}, {Role: "assistant", Content: "corrected"}}
	writeSession(t, dir, sessionFor(id("1"), messages))
	f, err := os.Create(filepath.Join(dir, id("2")+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(maxSessionBytes + 1); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := Scan(context.Background(), dir, ScanOptions{MaxBytes: 200}, testTools)
	if err != nil {
		t.Fatal(err)
	}
	if r.Stats.Skipped["invalid_session"] != 1 || r.Stats.Skipped["too_large"] != 2 || len(r.Candidates) != 0 {
		t.Fatalf("bounds: %+v", r.Stats)
	}
}

func TestTranscriptHashKeepsLargeNumbersDistinct(t *testing.T) {
	a := turn("question", "a", `{"path":"x","n":9007199254740992}`)
	b := turn("question", "b", `{"path":"x","n":9007199254740993}`)
	if transcriptHash(a) == transcriptHash(b) {
		t.Fatal("different JSON numbers collapsed")
	}
}

func TestSensitiveTextCommonFormats(t *testing.T) {
	for _, text := range []string{"OPENAI_API_KEY=abcdefgh123", "Authorization: Bearer abcdefghijkl", "-----BEGIN PRIVATE KEY-----", "token=abcdefgh"} {
		if !SensitiveText(text) {
			t.Errorf("missed %q", text)
		}
	}
	if SensitiveText("tokens used: 300") {
		t.Fatal("ordinary token count flagged")
	}
}

func TestScanExcludesPrivateCorrespondenceAcrossTurns(t *testing.T) {
	dir := t.TempDir()
	messages := turn("read my mail", "mail-1", `{"path":"unused"}`)
	messages[1].ToolCalls[0].Name = "Mail"
	messages = append(messages, core.Message{Role: "user", Content: "Summarize what you just read."}, core.Message{Role: "assistant", Content: "Private content quoted without a recognizable secret."})
	writeSession(t, dir, sessionFor(id("e"), messages))
	report, err := Scan(context.Background(), dir, ScanOptions{}, testTools)
	if err != nil || len(report.Candidates) != 0 || report.Stats.Skipped["private_session"] != 1 {
		t.Fatalf("private later turn escaped filtering: %+v %v", report, err)
	}
}

func TestScanExcludesPrivateChildGroupsBeforeSourceFiltering(t *testing.T) {
	dir := t.TempDir()
	parent := sessionFor(id("a"), []core.Message{{Role: "user", Content: "Summarize the subagent result."}, {Role: "assistant", Content: "A quotation from private correspondence."}})
	parent.Provider = "codex"
	writeSession(t, dir, parent)
	child := sessionFor(id("b"), turn("read correspondence", "private-call", `{"path":"unused"}`))
	child.ParentID, child.Provider = parent.ID, "another-provider"
	child.Messages[1].ToolCalls[0].Name = "Mail"
	writeSession(t, dir, child)
	report, err := Scan(context.Background(), dir, ScanOptions{Provider: "codex"}, testTools)
	if err != nil || len(report.Candidates) != 0 || report.Stats.Skipped["private_session"] != 1 {
		t.Fatalf("parent escaped child privacy filtering: %+v %v", report, err)
	}
}

func TestScanRemovesOnlyCompletedBackgroundReflection(t *testing.T) {
	for _, ordinal := range []string{"1", "2"} {
		t.Run("operation-"+ordinal, func(t *testing.T) {
			dir := t.TempDir()
			messages := turn("Read this file", "read", `{"path":"file"}`)
			callID := "reflection-0-" + ordinal
			messages = append(messages,
				core.Message{Role: "assistant", ToolCalls: []core.ToolCall{{ID: callID, Name: "Memory", Arguments: json.RawMessage(`{"action":"save"}`)}}},
				core.Message{Role: "tool", ToolCallID: callID, Content: "Saved a memory."},
				core.Message{Role: "user", Content: "Thanks, that is enough."}, core.Message{Role: "assistant", Content: "Understood."})
			writeSession(t, dir, sessionFor(id("c"), messages))
			report, err := Scan(context.Background(), dir, ScanOptions{}, testTools)
			if err != nil || len(report.Candidates) != 2 {
				t.Fatalf("lost completed turn: %+v %v", report, err)
			}
			var found bool
			for _, c := range report.Candidates {
				if c.Source.Start == 0 {
					found = true
					if c.Source.End != 4 || len(c.Messages) != 4 || c.Feedback != "Thanks, that is enough." {
						t.Fatalf("reflection leaked or offsets changed: %+v", c)
					}
				}
			}
			if !found {
				t.Fatal("original completed turn missing")
			}
		})
	}
	unfinished := append(turn("Read", "r", `{"path":"file"}`),
		core.Message{Role: "assistant", ToolCalls: []core.ToolCall{{ID: "ordinary-memory-call", Name: "Memory", Arguments: json.RawMessage(`{}`)}}},
		core.Message{Role: "tool", ToolCallID: "ordinary-memory-call", Content: "Saved"})
	if len(trimReflection(unfinished, 0)) != len(unfinished) {
		t.Fatal("trimmed an ordinary unfinished tool turn")
	}
}

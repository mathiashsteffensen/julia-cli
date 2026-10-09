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
	"jarvis/internal/core"
)

type testTools struct{ calls int }

func (t *testTools) Definitions() []core.ToolDefinition {
	return []core.ToolDefinition{{Name: "List", Description: "List workspace files", Parameters: map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}, "required": []string{"path"}}}}
}

type scriptedAPIModel struct {
	fantasy.LanguageModel
	parts         []fantasy.StreamPart
	streams       int
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
	return func(yield func(fantasy.StreamPart) bool) {
		for _, part := range m.parts {
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
	model := &scriptedAPIModel{parts: []fantasy.StreamPart{{Type: fantasy.StreamPartTypeTextStart, ID: "text"}, {Type: fantasy.StreamPartTypeTextDelta, ID: "text", Delta: "Done."}, {Type: fantasy.StreamPartTypeTextEnd, ID: "text"}, {Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop}}, generated: &fantasy.Response{Content: fantasy.ResponseContent{fantasy.TextContent{Text: "Previously inspected the fixture; continue verification."}}}}
	session := &core.Session{Instructions: runtimeInstructions, Messages: []core.Message{{Role: "user", Content: strings.Repeat("prior user context ", 80)}, {Role: "assistant", Content: "Inspected files."}, {Role: "user", Content: "Continue carefully."}}}
	backend := &API{Model: model, ContextBytes: 100}
	if err := backend.Run(context.Background(), session, "Verify results", &testTools{}, func(core.Event) {}); err != nil {
		t.Fatal(err)
	}
	if model.generations != 1 || model.streams != 1 {
		t.Fatalf("compaction not exercised: generate=%d streams=%d", model.generations, model.streams)
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

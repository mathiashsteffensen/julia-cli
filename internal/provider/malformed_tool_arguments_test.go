package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"charm.land/fantasy"
	"julia/internal/core"
	"julia/internal/session"
)

// Each scripted reply is one model response on the OpenAI-compatible wire, so
// these tests run the real provider, agent, API adapter and session store.
type wireReply struct {
	toolArguments string // sent as the arguments of a List call when text is empty
	text          string
	finishReason  string
}

// wireModel serves the replies in turn and keeps every request body it received.
type wireModel struct {
	t       *testing.T
	replies []wireReply

	mu       sync.Mutex
	requests []string
}

func (m *wireModel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		m.t.Error(err)
	}

	m.mu.Lock()
	m.requests = append(m.requests, string(body))
	turn := len(m.requests)
	m.mu.Unlock()

	if turn > len(m.replies) {
		m.t.Errorf("request %d has no scripted reply", turn)
		http.Error(w, "no scripted reply", http.StatusBadRequest)
		return
	}
	reply := m.replies[turn-1]

	delta := map[string]any{"role": "assistant", "content": reply.text}
	if reply.text == "" {
		delta = map[string]any{"role": "assistant", "tool_calls": []map[string]any{{
			"index": 0, "id": fmt.Sprintf("call_%d", turn), "type": "function",
			"function": map[string]any{"name": "List", "arguments": reply.toolArguments},
		}}}
	}

	w.Header().Set("Content-Type", "text/event-stream")
	writeChunk(m.t, w, delta, nil)
	writeChunk(m.t, w, map[string]any{}, reply.finishReason)
	fmt.Fprint(w, "data: [DONE]\n\n")
}

// request returns the body of a request the model received, counted from zero.
func (m *wireModel) request(i int) string {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.requests[i]
}

func writeChunk(t *testing.T, w http.ResponseWriter, delta map[string]any, finishReason any) {
	t.Helper()

	chunk, err := json.Marshal(map[string]any{"id": "reply", "choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": finishReason}}})
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(w, "data: %s\n\n", chunk)
}

// sentToolCalls returns the arguments of every tool call a request carried back to
// the model, and the text of every tool result in it.
func sentToolCalls(t *testing.T, request string) (arguments, results []string) {
	t.Helper()

	var body struct {
		Messages []struct {
			Role      string `json:"role"`
			Content   any    `json:"content"`
			ToolCalls []struct {
				Function struct {
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(request), &body); err != nil {
		t.Fatal(err)
	}

	for _, message := range body.Messages {
		for _, call := range message.ToolCalls {
			arguments = append(arguments, call.Function.Arguments)
		}
		if message.Role == "tool" {
			results = append(results, fmt.Sprint(message.Content))
		}
	}
	return arguments, results
}

// recordingTools keeps the arguments of every call that reached execution.
type recordingTools struct {
	testTools
	executed []string
}

func (t *recordingTools) Execute(ctx context.Context, call core.ToolCall) core.ToolResult {
	t.executed = append(t.executed, string(call.Arguments))
	return t.testTools.Execute(ctx, call)
}

// wireRun is one API run against a wireModel, with checkpoints in a real store.
type wireRun struct {
	model   *wireModel
	store   *session.Store
	session *core.Session
	tools   *recordingTools
	events  []core.Event
	backend *API
}

func newWireRun(t *testing.T, replies ...wireReply) *wireRun {
	t.Helper()

	model := &wireModel{t: t, replies: replies}
	server := httptest.NewServer(model)
	t.Cleanup(server.Close)

	store := &session.Store{Directory: t.TempDir()}
	t.Cleanup(func() { store.Close() })

	backend, err := NewAPI(context.Background(), APIConfig{Provider: "compatible", Model: "test-model", BaseURL: server.URL + "/v1", APIKey: "test", Checkpoint: store.Save})
	if err != nil {
		t.Fatal(err)
	}

	return &wireRun{model: model, store: store, session: session.New(t.TempDir(), "compatible", "test-model"), tools: &recordingTools{}, backend: backend}
}

func (r *wireRun) run(prompt string) error {
	return r.backend.Run(context.Background(), r.session, core.Prompt{Text: prompt}, r.tools, func(event core.Event) { r.events = append(r.events, event) })
}

func TestAPIMalformedToolArgumentsAreRejectedAndCorrected(t *testing.T) {
	cases := map[string]string{
		// A repair could guess valid arguments for these three.
		"literal newline": "{\"path\":\"a\nb\"}",
		"unescaped quote": `{"path":"say "hi""}`,
		"cut short":       `{"path":"ab`,

		// No repair can make these two a valid call.
		"literal newline, required field missing": "{\"file\":\"a\nb\"}",
		"not JSON": "ls\n-la",
	}

	const corrected = `{"path":"."}`

	for name, malformed := range cases {
		t.Run(name, func(t *testing.T) {
			run := newWireRun(t,
				wireReply{toolArguments: malformed, finishReason: "tool_calls"},
				wireReply{toolArguments: corrected, finishReason: "tool_calls"},
				wireReply{text: "Done.", finishReason: "stop"},
			)

			if err := run.run("List files"); err != nil {
				t.Fatalf("a malformed tool call ended the session: %v", err)
			}

			// Only the corrected call runs.
			if got := strings.Join(run.tools.executed, " "); got != corrected {
				t.Fatalf("executed %q, want only the corrected call", got)
			}
			if run.session.PendingTool != nil {
				t.Fatalf("left a pending tool marker: %+v", run.session.PendingTool)
			}

			// The model is told, and every call it is sent back is valid JSON.
			arguments, results := sentToolCalls(t, run.model.request(1))
			if len(results) != 1 || !strings.Contains(results[0], "invalid JSON input") {
				t.Fatalf("model was not told about the invalid arguments: %q", results)
			}
			if len(arguments) != 1 || !json.Valid([]byte(arguments[0])) {
				t.Fatalf("resent arguments are not valid JSON: %q", arguments)
			}

			// The event stream stays encodable. The rejected call shows as a warning
			// that does not repeat its text, and not as a tool that started.
			starts, warnings := 0, 0
			for _, event := range run.events {
				encoded, err := json.Marshal(event)
				if err != nil {
					t.Fatalf("event %s does not encode: %v", event.Kind, err)
				}
				switch event.Kind {
				case "tool_start":
					starts++
				case "warning":
					warnings++
					if strings.Contains(event.Text, malformed) || strings.Contains(string(encoded), "invalid_arguments") {
						t.Fatalf("warning repeats the rejected text: %s", encoded)
					}
				}
			}
			if starts != 1 || warnings != 1 {
				t.Fatalf("got %d tool_start and %d warning events, want 1 of each", starts, warnings)
			}

			// The checkpoint loads, and it keeps the rejected text as a string.
			saved, err := run.store.Load(run.session.ID)
			if err != nil {
				t.Fatalf("checkpoint does not load: %v", err)
			}
			if saved.PendingTool != nil {
				t.Fatalf("checkpoint kept a pending tool marker: %+v", saved.PendingTool)
			}
			if got := rejectedText(t, saved); got != malformed {
				t.Fatalf("diagnostic holds %q, want %q", got, malformed)
			}
		})
	}
}

// rejectedText returns the original text of the one rejected call in a session.
func rejectedText(t *testing.T, s *core.Session) string {
	t.Helper()

	var rejected []string
	for _, message := range s.Messages {
		for _, call := range message.ToolCalls {
			var arguments struct {
				Invalid *string `json:"invalid_arguments"`
			}
			if err := json.Unmarshal(call.Arguments, &arguments); err != nil {
				t.Fatalf("saved arguments are not JSON: %q", call.Arguments)
			}
			if arguments.Invalid != nil {
				rejected = append(rejected, *arguments.Invalid)
			}
		}
	}
	if len(rejected) != 1 {
		t.Fatalf("found %d rejected calls in the transcript, want 1", len(rejected))
	}
	return rejected[0]
}

func TestAPISessionResumesAfterMalformedToolArguments(t *testing.T) {
	run := newWireRun(t,
		wireReply{toolArguments: "{\"file\":\"a\nb\"}", finishReason: "tool_calls"},
		wireReply{text: "I give up.", finishReason: "stop"},
		wireReply{text: "Still here.", finishReason: "stop"},
	)

	if err := run.run("List files"); err != nil {
		t.Fatal(err)
	}

	saved, err := run.store.Load(run.session.ID)
	if err != nil {
		t.Fatal(err)
	}
	run.session = saved

	if err := run.run("Are you there?"); err != nil {
		t.Fatalf("the saved session does not resume: %v", err)
	}

	arguments, results := sentToolCalls(t, run.model.request(2))
	if len(arguments) != 1 || !json.Valid([]byte(arguments[0])) {
		t.Fatalf("resumed history resent %q", arguments)
	}
	if len(results) != 1 || !strings.Contains(results[0], "invalid JSON input") {
		t.Fatalf("resumed history lost the tool error: %q", results)
	}
}

func TestAPIEscapedNewlineInToolArgumentsStillRuns(t *testing.T) {
	escaped := `{"path":"a\nb"}`
	run := newWireRun(t,
		wireReply{toolArguments: escaped, finishReason: "tool_calls"},
		wireReply{text: "Done.", finishReason: "stop"},
	)

	if err := run.run("List files"); err != nil {
		t.Fatal(err)
	}

	if len(run.tools.executed) != 1 || run.tools.executed[0] != escaped {
		t.Fatalf("executed %q, want the arguments unchanged", run.tools.executed)
	}

	saved, err := run.store.Load(run.session.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The store indents what it saves, so compare the JSON and not its layout.
	for _, message := range saved.Messages {
		for _, call := range message.ToolCalls {
			var compact bytes.Buffer
			if err := json.Compact(&compact, call.Arguments); err != nil {
				t.Fatal(err)
			}
			if compact.String() != escaped {
				t.Fatalf("transcript holds %s, want the arguments unchanged", compact.String())
			}
		}
	}
}

func TestAPIRepeatedMalformedToolArgumentsStopAtTheStepLimit(t *testing.T) {
	malformed := wireReply{toolArguments: "{\"path\":\"a\nb\"}", finishReason: "tool_calls"}
	run := newWireRun(t, malformed, malformed)
	run.backend.MaxSteps = 2

	err := run.run("List files")
	if err == nil || !strings.Contains(err.Error(), "reached 2 steps") {
		t.Fatalf("got %v, want the step limit", err)
	}
	if len(run.tools.executed) != 0 {
		t.Fatalf("executed %q", run.tools.executed)
	}

	saved, loadErr := run.store.Load(run.session.ID)
	if loadErr != nil {
		t.Fatalf("checkpoint does not load: %v", loadErr)
	}
	if saved.PendingTool != nil {
		t.Fatalf("checkpoint kept a pending tool marker: %+v", saved.PendingTool)
	}
}

// A step cut off by the output limit keeps the call it was writing, unfinished.
func TestAPIToolCallCutOffByTheOutputLimitReportsTheLimit(t *testing.T) {
	model := &scriptedAPIModel{steps: [][]fantasy.StreamPart{
		{
			{Type: fantasy.StreamPartTypeToolCall, ID: "call-1", ToolCallName: "List", ToolCallInput: `{"path":"a`},
			{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonLength},
		},
		textStep("Still here.", fantasy.Usage{}),
	}}

	store := &session.Store{Directory: t.TempDir()}
	defer store.Close()

	s := session.New(t.TempDir(), "compatible", "test-model")
	tools := &recordingTools{}
	backend := &API{Model: model, Checkpoint: store.Save}

	err := backend.Run(context.Background(), s, core.Prompt{Text: "List files"}, tools, func(core.Event) {})
	if err == nil || !strings.Contains(err.Error(), "output limit") {
		t.Fatalf("got %v, want the output limit", err)
	}
	if len(tools.executed) != 0 {
		t.Fatalf("executed %q", tools.executed)
	}

	saved, loadErr := store.Load(s.ID)
	if loadErr != nil {
		t.Fatalf("checkpoint does not load: %v", loadErr)
	}
	if got := rejectedText(t, saved); got != `{"path":"a` {
		t.Fatalf("diagnostic holds %q", got)
	}

	if err := backend.Run(context.Background(), saved, core.Prompt{Text: "Continue"}, tools, func(core.Event) {}); err != nil {
		t.Fatalf("the saved session does not resume: %v", err)
	}
	var calls, results int
	for _, message := range model.calls[1].Prompt {
		for _, part := range message.Content {
			switch p := part.(type) {
			case fantasy.ToolCallPart:
				if p.ToolCallID == "call-1" && json.Valid([]byte(p.Input)) {
					calls++
				}
			case fantasy.ToolResultPart:
				if p.ToolCallID == "call-1" {
					if out, ok := p.Output.(fantasy.ToolResultOutputContentError); !ok || !strings.Contains(out.Error.Error(), "did not run") {
						t.Fatalf("unexpected tool result: %+v", p)
					}
					results++
				}
			}
		}
	}
	if calls != 1 || results != 1 {
		t.Fatalf("follow-up contains %d calls and %d results, want one of each", calls, results)
	}
}

func TestAPIAbnormalFinishPreservesProviderCallsAndClosesClientCalls(t *testing.T) {
	for _, reason := range []fantasy.FinishReason{fantasy.FinishReasonLength, fantasy.FinishReasonContentFilter, fantasy.FinishReasonError, fantasy.FinishReasonUnknown} {
		t.Run(string(reason), func(t *testing.T) {
			model := &scriptedAPIModel{steps: [][]fantasy.StreamPart{
				{
					{Type: fantasy.StreamPartTypeToolCall, ID: "client-1", ToolCallName: "List", ToolCallInput: `{"path":"."}`},
					{Type: fantasy.StreamPartTypeToolCall, ID: "client-2", ToolCallName: "List", ToolCallInput: `{"path":"`},
					{Type: fantasy.StreamPartTypeToolCall, ID: "server", ToolCallName: "search", ToolCallInput: `{}`, ProviderExecuted: true},
					{Type: fantasy.StreamPartTypeToolResult, ID: "server", ToolCallName: "search", ProviderExecuted: true},
					{Type: fantasy.StreamPartTypeFinish, FinishReason: reason},
				},
				textStep("Done.", fantasy.Usage{}),
			}}
			store := &session.Store{Directory: t.TempDir()}
			defer store.Close()
			s := session.New(t.TempDir(), "compatible", "test-model")
			tools := &recordingTools{}
			backend := &API{Model: model, Checkpoint: store.Save}
			err := backend.Run(context.Background(), s, core.Prompt{Text: "List"}, tools, func(core.Event) {})
			if reason == fantasy.FinishReasonUnknown {
				// An unknown finish with a call continues within the same run.
				if err != nil {
					t.Fatal(err)
				}
			} else {
				if err == nil {
					t.Fatal("abnormal finish did not return an error")
				}
				saved, loadErr := store.Load(s.ID)
				if loadErr != nil {
					t.Fatal(loadErr)
				}
				if err := backend.Run(context.Background(), saved, core.Prompt{Text: "Continue"}, tools, func(core.Event) {}); err != nil {
					t.Fatal(err)
				}
			}
			if len(tools.executed) != 0 {
				t.Fatalf("executed %q", tools.executed)
			}
			calls, results := map[string]fantasy.ToolCallPart{}, map[string][]fantasy.ToolResultPart{}
			for _, message := range model.calls[1].Prompt {
				for _, part := range message.Content {
					switch p := part.(type) {
					case fantasy.ToolCallPart:
						calls[p.ToolCallID] = p
					case fantasy.ToolResultPart:
						results[p.ToolCallID] = append(results[p.ToolCallID], p)
					}
				}
			}
			for _, id := range []string{"client-1", "client-2"} {
				if !json.Valid([]byte(calls[id].Input)) || len(results[id]) != 1 {
					t.Fatalf("call %s or its result is missing: %+v, %+v", id, calls[id], results[id])
				}
				if out, ok := results[id][0].Output.(fantasy.ToolResultOutputContentError); !ok || !strings.Contains(out.Error.Error(), "did not run") {
					t.Fatalf("call %s result = %+v", id, results[id][0])
				}
			}
			if !calls["server"].ProviderExecuted || len(results["server"]) != 1 || !results["server"][0].ProviderExecuted {
				t.Fatalf("provider call or result changed: %+v, %+v", calls["server"], results["server"])
			}
		})
	}
}

func TestCompleteUnansweredToolCallsKeepsExistingResults(t *testing.T) {
	messages := []fantasy.Message{
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{
			fantasy.ToolCallPart{ToolCallID: "done", ToolName: "List", Input: `{}`},
			fantasy.ToolCallPart{ToolCallID: "server", ToolName: "search", Input: `{}`, ProviderExecuted: true},
			fantasy.ToolCallPart{ToolCallID: "missing", ToolName: "List", Input: `{}`},
		}},
		{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{
			fantasy.ToolResultPart{ToolCallID: "done", Output: fantasy.ToolResultOutputContentText{Text: "original"}},
		}},
	}
	got := completeUnansweredToolCalls(messages, fantasy.FinishReasonLength)
	if len(got) != 2 || len(got[1].Content) != 2 {
		t.Fatalf("unexpected messages: %+v", got)
	}
	result, ok := got[1].Content[1].(fantasy.ToolResultPart)
	if !ok || result.ToolCallID != "missing" {
		t.Fatalf("unexpected result: %+v", got[1].Content[1])
	}
	original := got[1].Content[0].(fantasy.ToolResultPart).Output.(fantasy.ToolResultOutputContentText)
	if original.Text != "original" {
		t.Fatalf("existing result changed: %+v", original)
	}
}

func TestAPICutOffToolCallFollowUpOnWire(t *testing.T) {
	run := newWireRun(t, wireReply{text: "Still here.", finishReason: "stop"})
	wireModel := run.backend.Model
	run.backend.Model = &scriptedAPIModel{parts: []fantasy.StreamPart{
		{Type: fantasy.StreamPartTypeToolCall, ID: "call-1", ToolCallName: "List", ToolCallInput: `{"path":"a`},
		{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonLength},
	}}
	if err := run.run("List files"); err == nil || !strings.Contains(err.Error(), "output limit") {
		t.Fatalf("got %v, want the output limit", err)
	}
	saved, err := run.store.Load(run.session.ID)
	if err != nil {
		t.Fatal(err)
	}
	run.session = saved
	run.backend.Model = wireModel
	if err := run.run("Continue"); err != nil {
		t.Fatal(err)
	}
	var body struct {
		Messages []struct {
			Role       string `json:"role"`
			ToolCallID string `json:"tool_call_id"`
			Content    any    `json:"content"`
			ToolCalls  []struct {
				ID       string `json:"id"`
				Function struct {
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(run.model.request(0)), &body); err != nil {
		t.Fatal(err)
	}
	var calls, results int
	for _, message := range body.Messages {
		for _, call := range message.ToolCalls {
			if call.ID == "call-1" && json.Valid([]byte(call.Function.Arguments)) {
				calls++
			}
		}
		if message.Role == "tool" && message.ToolCallID == "call-1" && strings.Contains(fmt.Sprint(message.Content), "did not run") {
			results++
		}
	}
	if calls != 1 || results != 1 || len(run.tools.executed) != 0 {
		t.Fatalf("follow-up has %d calls and %d results; executed %q", calls, results, run.tools.executed)
	}
}

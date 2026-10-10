package provider

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"charm.land/fantasy"
	"jarvis/internal/core"
)

// questionTools offers only AskUserQuestion and records the session's pending
// marker as seen by the tool while it is waiting on the human.
type questionTools struct {
	session *core.Session
	pending *core.ToolCall
	calls   int
}

func (q *questionTools) Definitions() []core.ToolDefinition {
	return []core.ToolDefinition{{Name: core.QuestionTool, Description: "ask", Parameters: map[string]any{"type": "object", "properties": map[string]any{"question": map[string]any{"type": "string"}}, "required": []string{"question"}}}}
}

func (q *questionTools) Execute(context.Context, core.ToolCall) core.ToolResult {
	q.calls++
	q.pending = q.session.PendingTool
	return core.ToolResult{Content: `{"answer":"yes","source":"freeform"}`}
}

// A question has no side effects and can wait for hours, so closing the terminal
// while one is open must not leave a session that refuses to continue.
func TestAPIQuestionIsNotJournaledAsAnUncertainTool(t *testing.T) {
	model := &scriptedAPIModel{steps: [][]fantasy.StreamPart{
		{{Type: fantasy.StreamPartTypeToolCall, ID: "q1", ToolCallName: core.QuestionTool, ToolCallInput: `{"question":"Proceed?"}`}, {Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls}},
		textStep("Proceeding.", fantasy.Usage{}),
	}}
	session := &core.Session{}
	tools := &questionTools{session: session}
	var checkpoints []bool
	backend := &API{Model: model, Checkpoint: func(s *core.Session) error { checkpoints = append(checkpoints, s.PendingTool != nil); return nil }}
	if err := backend.Run(context.Background(), session, "decide", tools, func(core.Event) {}); err != nil {
		t.Fatal(err)
	}
	if tools.calls != 1 || tools.pending != nil {
		t.Fatalf("calls=%d; the session was marked pending while waiting for the user: %+v", tools.calls, tools.pending)
	}
	for i, pending := range checkpoints {
		if pending {
			t.Fatalf("checkpoint %d recorded an uncertain tool", i)
		}
	}
	if session.PendingTool != nil {
		t.Fatal("pending marker left behind")
	}
}

func TestAPIOtherToolsStillMarkThemselvesPending(t *testing.T) {
	model := &scriptedAPIModel{steps: [][]fantasy.StreamPart{toolStep(fantasy.Usage{}), textStep("Done.", fantasy.Usage{})}}
	session := &core.Session{}
	seen := false
	executor := &pendingProbe{session: session, seen: &seen}
	if err := (&API{Model: model}).Run(context.Background(), session, "list", executor, func(core.Event) {}); err != nil {
		t.Fatal(err)
	}
	if !seen {
		t.Fatal("side-effecting tools must keep their pending marker")
	}
}

type pendingProbe struct {
	testTools
	session *core.Session
	seen    *bool
}

func (p *pendingProbe) Execute(ctx context.Context, call core.ToolCall) core.ToolResult {
	*p.seen = p.session.PendingTool != nil && p.session.PendingTool.ID == call.ID
	return p.testTools.Execute(ctx, call)
}

func TestCodexQuestionIsNotJournaledAsAnUncertainTool(t *testing.T) {
	f := newCodexFixture(t)
	call := core.ToolCall{ID: "ask-1", Name: core.QuestionTool, Arguments: json.RawMessage(`{"question":"Proceed?"}`)}
	session := &core.Session{Directory: t.TempDir()}
	tools := &questionTools{session: session}
	var intents []string
	f.turn = func(f *codexFixture) {
		f.push(map[string]any{"id": 21, "method": "item/tool/call", "params": map[string]any{"threadId": "thread-1", "turnId": "turn-1", "callId": call.ID, "tool": call.Name, "arguments": call.Arguments}})
		f.finish("completed")
	}
	backend := &Codex{start: f.start}
	// The journal lives in the session state; look at it from inside the tool.
	probe := &journalProbe{questionTools: tools, session: session, intents: &intents}
	if err := backend.Run(context.Background(), session, "decide", probe, nil); err != nil {
		t.Fatal(err)
	}
	if tools.calls != 1 {
		t.Fatalf("question executed %d times", tools.calls)
	}
	if len(intents) != 0 {
		t.Fatalf("an unresolved journal entry existed while the user was answering: %v", intents)
	}
	// A new run on the same session must not be blocked by this call.
	f2 := newCodexFixture(t)
	f2.turn = func(f *codexFixture) { f.finish("completed") }
	if err := (&Codex{start: f2.start}).Run(context.Background(), session, "continue", &codexTestExecutor{}, nil); err != nil && strings.Contains(err.Error(), "uncertain outcome") {
		t.Fatalf("session was left unresumable: %v", err)
	}
}

type journalProbe struct {
	*questionTools
	session *core.Session
	intents *[]string
}

func (p *journalProbe) Execute(ctx context.Context, call core.ToolCall) core.ToolResult {
	if journal, err := loadCodexJournal(p.session.State); err == nil {
		for id, entry := range journal.Calls {
			if entry.Result == nil {
				*p.intents = append(*p.intents, id)
			}
		}
	}
	return p.questionTools.Execute(ctx, call)
}

// A step with several tool calls returns all their results in one fantasy
// message; every result must reach the saved transcript, or an answered question
// that shared a step with another call is lost from resumed history.
func TestAPITranscriptKeepsEveryResultOfAMultiToolStep(t *testing.T) {
	var s core.Session
	appendTranscript(&s, []fantasy.Message{
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{
			fantasy.ToolCallPart{ToolCallID: "q", ToolName: core.QuestionTool, Input: `{"question":"Proceed?"}`},
			fantasy.ToolCallPart{ToolCallID: "r", ToolName: "Read", Input: `{"path":"a"}`},
		}},
		{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{
			fantasy.ToolResultPart{ToolCallID: "q", Output: fantasy.ToolResultOutputContentText{Text: `{"answer":"yes","source":"freeform"}`}},
			fantasy.ToolResultPart{ToolCallID: "r", Output: fantasy.ToolResultOutputContentError{Error: errors.New("boom")}},
		}},
	})
	if len(s.Messages) != 3 {
		t.Fatalf("want assistant plus two tool messages, got %+v", s.Messages)
	}
	if got := s.Messages[0]; len(got.ToolCalls) != 2 || got.Role != "assistant" {
		t.Fatalf("assistant message %+v", got)
	}
	for i, want := range []struct{ id, content string }{{"q", `{"answer":"yes","source":"freeform"}`}, {"r", "boom"}} {
		if m := s.Messages[i+1]; m.Role != "tool" || m.ToolCallID != want.id || m.Content != want.content {
			t.Fatalf("message %d = %+v, want %+v", i+1, m, want)
		}
	}
}

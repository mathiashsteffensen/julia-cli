package main

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"

	"jarvis/internal/core"
)

// approvalTasks records the task context each approval request carried.
type approvalTasks struct {
	mu    sync.Mutex
	tasks []string
}

func (a *approvalTasks) approve(_ context.Context, request core.Approval) (core.Decision, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.tasks = append(a.tasks, request.Task)
	return core.Decision{Allow: false, Reason: "test denial"}, nil
}

func (a *approvalTasks) last(t *testing.T) string {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.tasks) == 0 {
		t.Fatal("no approval was requested")
	}
	return a.tasks[len(a.tasks)-1]
}

const writeCall = `{"path":"notes.txt","content":"x"}`

func askModeRunner(t *testing.T, backend core.Backend, ask core.Asker, subagents bool) (*runtimeBackend, *core.Session, *approvalTasks) {
	t.Helper()
	runner, s := newQuestionRunner(backend, ask, subagents, t.TempDir())
	runner.cfg.Mode = "ask"
	recorded := &approvalTasks{}
	runner.approve = recorded.approve
	return runner, s, recorded
}

func TestAnswerReachesTheNextApprovalInTheSameTurn(t *testing.T) {
	ask := func(context.Context, core.Question) (core.Answer, error) {
		return core.Answer{Text: "Yes, run it", Option: 1}, nil
	}
	backend := &questionBackend{run: func(ctx context.Context, _ *core.Session, _ string, executor core.ToolExecutor) error {
		before := executor.Execute(ctx, core.ToolCall{ID: "w0", Name: "Write", Arguments: json.RawMessage(writeCall)})
		if !before.IsError {
			t.Error("ask mode should have denied the write")
		}
		result := askQuestion(ctx, executor, `{"question":"Run the production migration now?","options":[{"label":"Yes, run it"},{"label":"Not yet"}]}`)
		if result.IsError {
			t.Errorf("question failed: %s", result.Content)
		}
		executor.Execute(ctx, core.ToolCall{ID: "w1", Name: "Write", Arguments: json.RawMessage(writeCall)})
		return nil
	}}
	runner, s, approvals := askModeRunner(t, backend, ask, false)
	if err := runQuestionBackend(runner, s); err != nil {
		t.Fatal(err)
	}
	if len(approvals.tasks) != 2 {
		t.Fatalf("expected two approvals, got %d", len(approvals.tasks))
	}
	if strings.Contains(approvals.tasks[0], "assistant_question") {
		t.Fatalf("the first approval predates the answer but carries it:\n%s", approvals.tasks[0])
	}
	for _, want := range []string{"task", "Run the production migration now?", `"user_selected_option":"Yes, run it"`} {
		if !strings.Contains(approvals.tasks[1], want) {
			t.Fatalf("the approval after the answer is missing %q:\n%s", want, approvals.tasks[1])
		}
	}
}

func TestSkippedAndFailedQuestionsAddNothingToTheReviewerContext(t *testing.T) {
	for name, tc := range map[string]struct {
		answer core.Answer
		err    error
	}{
		"skipped": {core.Answer{Skipped: true}, nil},
		"failed":  {core.Answer{}, context.DeadlineExceeded},
	} {
		ask := func(context.Context, core.Question) (core.Answer, error) { return tc.answer, tc.err }
		backend := &questionBackend{run: func(ctx context.Context, _ *core.Session, _ string, executor core.ToolExecutor) error {
			askQuestion(ctx, executor, `{"question":"Proceed?"}`)
			executor.Execute(ctx, core.ToolCall{ID: "w", Name: "Write", Arguments: json.RawMessage(writeCall)})
			return nil
		}}
		runner, s, approvals := askModeRunner(t, backend, ask, false)
		if err := runQuestionBackend(runner, s); err != nil {
			t.Fatal(err)
		}
		if got := approvals.last(t); got != "task" {
			t.Fatalf("%s: reviewer context changed to %q", name, got)
		}
	}
}

func TestAnswersFromEarlierTurnsStillInformTheReviewer(t *testing.T) {
	backend := &questionBackend{run: func(ctx context.Context, _ *core.Session, _ string, executor core.ToolExecutor) error {
		executor.Execute(ctx, core.ToolCall{ID: "w", Name: "Write", Arguments: json.RawMessage(writeCall)})
		return nil
	}}
	runner, s, approvals := askModeRunner(t, backend, func(context.Context, core.Question) (core.Answer, error) { return core.Answer{Skipped: true}, nil }, false)
	arguments, _ := json.Marshal(deployQuestion)
	s.Messages = []core.Message{
		{Role: "user", Content: "migrate the schema"},
		{Role: "assistant", ToolCalls: []core.ToolCall{{ID: "old", Name: core.QuestionTool, Arguments: arguments}}},
		{Role: "tool", ToolCallID: "old", Content: questionResultJSON(core.QuestionResult{Answer: "Yes, run it", Source: "option", Option: 1})},
		{Role: "assistant", Content: "Done."},
	}
	if err := runQuestionBackend(runner, s); err != nil {
		t.Fatal(err)
	}
	got := approvals.last(t)
	for _, want := range []string{"migrate the schema", `"user_selected_option":"Yes, run it"`, "User follow-up:\ntask"} {
		if !strings.Contains(got, want) {
			t.Fatalf("resumed context is missing %q:\n%s", want, got)
		}
	}
}

func TestSubagentApprovalsSeeTheParentsAnswers(t *testing.T) {
	ask := func(context.Context, core.Question) (core.Answer, error) {
		return core.Answer{Text: "only the cache"}, nil
	}
	backend := &questionBackend{}
	backend.run = func(ctx context.Context, s *core.Session, _ string, executor core.ToolExecutor) error {
		if s.ParentID != "" {
			executor.Execute(ctx, core.ToolCall{ID: "child-write", Name: "Write", Arguments: json.RawMessage(writeCall)})
			return nil
		}
		askQuestion(ctx, executor, `{"question":"Which directories may I clean?"}`)
		executor.Execute(ctx, core.ToolCall{ID: "sub", Name: "Subagent", Arguments: json.RawMessage(`{"tasks":["clean up"]}`)})
		return nil
	}
	runner, s, approvals := askModeRunner(t, backend, ask, true)
	if err := runQuestionBackend(runner, s); err != nil {
		t.Fatal(err)
	}
	if got := approvals.last(t); !strings.Contains(got, `"user_typed_answer":"only the cache"`) {
		t.Fatalf("a delegated task was judged without the user's answer:\n%s", got)
	}
}

func TestHeadlessRuntimeKeepsTheOriginalTaskContext(t *testing.T) {
	backend := &questionBackend{run: func(ctx context.Context, _ *core.Session, _ string, executor core.ToolExecutor) error {
		executor.Execute(ctx, core.ToolCall{ID: "w", Name: "Write", Arguments: json.RawMessage(writeCall)})
		return nil
	}}
	runner, s, approvals := askModeRunner(t, backend, nil, false)
	if err := runQuestionBackend(runner, s); err != nil {
		t.Fatal(err)
	}
	if got := approvals.last(t); got != "task" {
		t.Fatalf("got %q", got)
	}
}

// The tool is registered for every run, so a session's tool catalog (which Codex
// fixes per thread) is identical in the TUI and headless; headless skips at once.
func TestHeadlessQuestionsAreSkippedAndTheCatalogMatchesTheTUI(t *testing.T) {
	var result core.ToolResult
	backend := &questionBackend{run: func(ctx context.Context, _ *core.Session, _ string, executor core.ToolExecutor) error {
		result = askQuestion(ctx, executor, `{"question":"Proceed?"}`)
		return nil
	}}
	headless, s := newQuestionRunner(backend, headlessAsker, true, t.TempDir())
	if err := runQuestionBackend(headless, s); err != nil {
		t.Fatal(err)
	}
	got, err := decodeQuestionResult(result.Content)
	if err != nil || result.IsError || !got.Skipped || got.Note == "" {
		t.Fatalf("a headless question must be skipped with a note: %+v %v", result, err)
	}
	interactive, _ := newQuestionRunner(backend, func(context.Context, core.Question) (core.Answer, error) { return core.Answer{}, nil }, true, t.TempDir())
	if h, i := toolNames(headless.Definitions()), toolNames(interactive.Definitions()); !reflect.DeepEqual(h, i) {
		t.Fatalf("catalogs differ between modes: %v vs %v", h, i)
	}
}

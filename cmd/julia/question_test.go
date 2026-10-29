package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"julia/agents"
	"julia/internal/config"
	"julia/internal/core"
	"julia/internal/session"
	"julia/internal/ui"
)

type questionBackend struct {
	run func(ctx context.Context, s *core.Session, prompt core.Prompt, executor core.ToolExecutor) error
}

func (b *questionBackend) Name() string { return "fake" }
func (b *questionBackend) Run(ctx context.Context, s *core.Session, prompt core.Prompt, executor core.ToolExecutor, _ core.Emit) error {
	return b.run(ctx, s, prompt, executor)
}

func newQuestionRunner(backend core.Backend, ask core.Asker, subagents bool, directory string) (*runtimeBackend, *core.Session) {
	cfg := config.Default()
	cfg.Mode = "read-only"
	runner := &runtimeBackend{backend: backend, cfg: cfg, ask: ask, timeout: 30 * time.Second, parent: context.Background()}
	if subagents {
		runner.roles = agents.Builtin()
		runner.makeBackend = func(context.Context, config.Config) (core.Backend, core.Approver, error) {
			return backend, nil, nil
		}
	}
	return runner, session.New(directory, "fake", "model")
}

func runQuestionBackend(runner *runtimeBackend, s *core.Session) error {
	return runner.Run(context.Background(), s, core.Prompt{Text: "task"}, nil, func(core.Event) {})
}

func hasTool(definitions []core.ToolDefinition, name string) bool {
	for _, definition := range definitions {
		if definition.Name == name {
			return true
		}
	}
	return false
}

func toolNames(definitions []core.ToolDefinition) []string {
	names := make([]string, len(definitions))
	for i, definition := range definitions {
		names[i] = definition.Name
	}
	return names
}

func decodeQuestionResult(content string) (core.QuestionResult, error) {
	var result core.QuestionResult
	decoder := json.NewDecoder(strings.NewReader(content))
	decoder.DisallowUnknownFields()
	err := decoder.Decode(&result)
	return result, err
}

func askQuestion(ctx context.Context, executor core.ToolExecutor, arguments string) core.ToolResult {
	return executor.Execute(ctx, core.ToolCall{ID: "call-1", Name: core.QuestionTool, Arguments: json.RawMessage(arguments)})
}

// askerBlocked reports whether any goroutine is inside the UI bridge, which has
// no worker goroutines: a frame there means a caller is still waiting.
func askerBlocked() bool {
	buf := make([]byte, 1<<20)
	return bytes.Contains(buf[:runtime.Stack(buf, true)], []byte("ui.NewBridge"))
}

func eventually(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRuntimeQuestionToolDeliversQuestionAndAnswer(t *testing.T) {
	const withOptions = `{"question":"Which database?","options":[{"label":"Postgres (Recommended)","description":"Relational and battle-tested"},{"label":"SQLite"}]}`
	const openEnded = `{"question":"What should the project be called?"}`
	options := []core.QuestionOption{{Label: "Postgres (Recommended)", Description: "Relational and battle-tested"}, {Label: "SQLite"}}
	tests := []struct {
		name      string
		arguments string
		question  core.Question
		answer    core.Answer
		want      core.QuestionResult
	}{
		{"option pick", withOptions, core.Question{Text: "Which database?", Options: options}, core.Answer{Text: "SQLite", Option: 2}, core.QuestionResult{Answer: "SQLite", Source: "option", Option: 2}},
		{"freeform answer to options", withOptions, core.Question{Text: "Which database?", Options: options}, core.Answer{Text: "Use DuckDB"}, core.QuestionResult{Answer: "Use DuckDB", Source: "freeform"}},
		{"freeform question", openEnded, core.Question{Text: "What should the project be called?"}, core.Answer{Text: "julia"}, core.QuestionResult{Answer: "julia", Source: "freeform"}},
		{"skip", withOptions, core.Question{Text: "Which database?", Options: options}, core.Answer{Skipped: true}, core.QuestionResult{Skipped: true}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var asked []core.Question
			ask := func(_ context.Context, q core.Question) (core.Answer, error) {
				asked = append(asked, q)
				return tc.answer, nil
			}
			var result core.ToolResult
			backend := &questionBackend{run: func(ctx context.Context, _ *core.Session, _ core.Prompt, executor core.ToolExecutor) error {
				if !hasTool(executor.Definitions(), core.QuestionTool) {
					return fmt.Errorf("question tool missing from %v", toolNames(executor.Definitions()))
				}
				result = askQuestion(ctx, executor, tc.arguments)
				return nil
			}}
			runner, s := newQuestionRunner(backend, ask, true, t.TempDir())
			if err := runQuestionBackend(runner, s); err != nil {
				t.Fatal(err)
			}
			if len(asked) != 1 || !reflect.DeepEqual(asked[0], tc.question) {
				t.Fatalf("asker received %+v, want one %+v", asked, tc.question)
			}
			if result.IsError {
				t.Fatalf("question failed: %s", result.Content)
			}
			got, err := decodeQuestionResult(result.Content)
			if err != nil {
				t.Fatalf("result %q does not decode as QuestionResult: %v", result.Content, err)
			}
			if tc.want.Skipped {
				if got.Note == "" || got.Answer != "" || got.Source != "" || got.Option != 0 {
					t.Fatalf("skip result = %+v, want only skipped and a note", got)
				}
				tc.want.Note = got.Note
			}
			if got != tc.want {
				t.Fatalf("result = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestRuntimeQuestionToolRoundTripsThroughUIBridge(t *testing.T) {
	ask, requests := ui.NewAsker()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case request := <-requests:
			request.Reply <- core.Answer{Text: request.Question.Options[0].Label, Option: 1}
		case <-stop:
		}
	}()
	var result core.ToolResult
	backend := &questionBackend{run: func(ctx context.Context, _ *core.Session, _ core.Prompt, executor core.ToolExecutor) error {
		result = askQuestion(ctx, executor, `{"question":"Pick one","options":[{"label":"First"},{"label":"Second"}]}`)
		return nil
	}}
	runner, s := newQuestionRunner(backend, ask, true, t.TempDir())
	if err := runQuestionBackend(runner, s); err != nil {
		t.Fatal(err)
	}
	got, err := decodeQuestionResult(result.Content)
	if err != nil || result.IsError || got != (core.QuestionResult{Answer: "First", Source: "option", Option: 1}) {
		t.Fatalf("result = %q (error=%v, decode=%v)", result.Content, result.IsError, err)
	}
}

func TestRuntimeWithoutAskerDoesNotOfferQuestionTool(t *testing.T) {
	for _, subagents := range []bool{false, true} {
		t.Run(fmt.Sprintf("subagents=%v", subagents), func(t *testing.T) {
			backend := &questionBackend{run: func(ctx context.Context, _ *core.Session, _ core.Prompt, executor core.ToolExecutor) error {
				if hasTool(executor.Definitions(), core.QuestionTool) {
					return fmt.Errorf("question tool advertised: %v", toolNames(executor.Definitions()))
				}
				result := askQuestion(ctx, executor, `{"question":"Anyone there?"}`)
				if !result.IsError || !strings.Contains(result.Content, "unknown tool") {
					return fmt.Errorf("question tool callable without an asker: %+v", result)
				}
				return nil
			}}
			runner, s := newQuestionRunner(backend, nil, subagents, t.TempDir())
			runner.headless = true
			if hasTool(runner.Definitions(), core.QuestionTool) {
				t.Fatalf("sidebar registry lists the question tool: %v", toolNames(runner.Definitions()))
			}
			if err := runQuestionBackend(runner, s); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRuntimeSubagentChildrenCannotAskTheUser(t *testing.T) {
	var asks atomic.Int32
	ask := func(context.Context, core.Question) (core.Answer, error) {
		asks.Add(1)
		return core.Answer{Text: "yes"}, nil
	}
	var childRan, childSawTool bool
	var childResult core.ToolResult
	backend := &questionBackend{}
	backend.run = func(ctx context.Context, s *core.Session, _ core.Prompt, executor core.ToolExecutor) error {
		if s.ParentID != "" {
			childRan = true
			childSawTool = hasTool(executor.Definitions(), core.QuestionTool)
			childResult = askQuestion(ctx, executor, `{"question":"May I?"}`)
			s.Messages = append(s.Messages, core.Message{Role: "assistant", Content: "child done"})
			return nil
		}
		if !hasTool(executor.Definitions(), core.QuestionTool) {
			return fmt.Errorf("parent lacks the question tool: %v", toolNames(executor.Definitions()))
		}
		if result := askQuestion(ctx, executor, `{"question":"May the parent ask?"}`); result.IsError || !strings.Contains(result.Content, `"yes"`) {
			return fmt.Errorf("parent question failed: %+v", result)
		}
		result := executor.Execute(ctx, core.ToolCall{ID: "call-2", Name: "Subagent", Arguments: json.RawMessage(`{"tasks":[{"role":"delegate","task":"try to ask the user"}]}`)})
		if result.IsError || strings.Contains(result.Content, `"error"`) || !strings.Contains(result.Content, "child done") {
			return fmt.Errorf("subagent failed: %+v", result)
		}
		return nil
	}
	runner, s := newQuestionRunner(backend, ask, true, t.TempDir())
	if err := runQuestionBackend(runner, s); err != nil {
		t.Fatal(err)
	}
	if !childRan {
		t.Fatal("child never ran")
	}
	if childSawTool {
		t.Fatal("child was offered the question tool")
	}
	if !childResult.IsError || !strings.Contains(childResult.Content, "unknown tool") {
		t.Fatalf("child question did not fail as an unknown tool: %+v", childResult)
	}
	if asks.Load() != 1 {
		t.Fatalf("asker called %d times, want only the parent's question", asks.Load())
	}
}

func TestRuntimeDefinitionsMatchExecutorChain(t *testing.T) {
	for _, tc := range []struct {
		name      string
		asker     bool
		subagents bool
		skills    bool
		wantTail  []string
	}{
		{"interactive", true, true, false, []string{"Subagent", core.QuestionTool}},
		{"interactive without subagents", true, false, false, []string{"WebFetch", core.QuestionTool}},
		{"interactive with skills", true, true, true, []string{"Subagent", "Skill", core.QuestionTool}},
		{"headless", false, true, false, []string{"Subagent"}},
		{"headless without subagents", false, false, false, []string{"WebFetch"}},
		{"headless with skills", false, false, true, []string{"WebFetch", "Skill"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var chain []core.ToolDefinition
			backend := &questionBackend{run: func(_ context.Context, _ *core.Session, _ core.Prompt, executor core.ToolExecutor) error {
				chain = executor.Definitions()
				return nil
			}}
			var ask core.Asker
			if tc.asker {
				ask = func(context.Context, core.Question) (core.Answer, error) { return core.Answer{Skipped: true}, nil }
			}
			runner, s := newQuestionRunner(backend, ask, tc.subagents, t.TempDir())
			runner.skillsDir = t.TempDir()
			if tc.skills {
				writeSkill(t, runner.skillsDir, "style", "")
				writeSkill(t, runner.skillsDir, "deploy", "disable-model-invocation: true\n")
			}

			if err := runQuestionBackend(runner, s); err != nil {
				t.Fatal(err)
			}
			registry := runner.Definitions()
			if !reflect.DeepEqual(toolNames(registry), toolNames(chain)) {
				t.Fatalf("registry %v differs from executor chain %v", toolNames(registry), toolNames(chain))
			}
			if !reflect.DeepEqual(registry, chain) {
				t.Fatal("registry definitions differ from executor chain definitions")
			}
			names := toolNames(registry)
			if tail := names[len(names)-len(tc.wantTail):]; !reflect.DeepEqual(tail, tc.wantTail) {
				t.Fatalf("registry ends with %v, want %v (all: %v)", tail, tc.wantTail, names)
			}
		})
	}
}

// writeSkill adds the skill name to dir, with extra frontmatter lines.
func writeSkill(t *testing.T, dir, name, extra string) {
	t.Helper()
	content := "---\nname: " + name + "\ndescription: The " + name + " skill.\n" + extra + "---\nDo the " + name + " work."
	if err := os.MkdirAll(filepath.Join(dir, name), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name, "SKILL.md"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeQuestionCancellationReleasesAsker(t *testing.T) {
	for _, tc := range []struct {
		name        string
		takeRequest bool
	}{
		{"UI has not taken the question", false},
		{"UI is waiting for the user's reply", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ask, requests := ui.NewAsker()
			var result core.ToolResult
			backend := &questionBackend{run: func(ctx context.Context, _ *core.Session, _ core.Prompt, executor core.ToolExecutor) error {
				result = askQuestion(ctx, executor, `{"question":"Still there?"}`)
				return nil
			}}
			runner, s := newQuestionRunner(backend, ask, true, t.TempDir())
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			runner.parent = parent
			done := make(chan error, 1)
			go func() { done <- runQuestionBackend(runner, s) }()
			if tc.takeRequest {
				select {
				case request := <-requests:
					if request.Question.Text != "Still there?" {
						t.Fatalf("unexpected question %+v", request.Question)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("question never reached the UI")
				}
			}
			eventually(t, "the asker to block", askerBlocked)
			started := time.Now()
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("Run did not return after cancellation while the asker was blocked")
			}
			if elapsed := time.Since(started); elapsed > time.Second {
				t.Fatalf("cancellation took %s", elapsed)
			}
			if !result.IsError || !strings.Contains(result.Content, context.Canceled.Error()) {
				t.Fatalf("cancelled question result = %+v, want an error result", result)
			}
			if askerBlocked() {
				t.Fatal("a goroutine is still blocked in the asker after cancellation")
			}
		})
	}
}

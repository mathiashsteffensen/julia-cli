package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"julia/internal/config"
	"julia/internal/core"
	"julia/internal/memory"
	"julia/internal/session"
	"julia/internal/tools"
	"julia/internal/ui"
)

// reflectingBackend is a provider that answers each turn with a fixed reply
// and, as a Completer, with a fixed reflection.
type reflectingBackend struct {
	instructions []string
	completions  []completion
	reply        string
	answer       string
	spent        core.TokenUsage
	completeErr  error
	// taint, when set, has the turn call the named tool before replying.
	taint string
}

type completion struct {
	instructions, prompt string
	schema               any
}

func (b *reflectingBackend) Name() string { return "fake" }
func (b *reflectingBackend) Run(ctx context.Context, s *core.Session, prompt core.Prompt, executor core.ToolExecutor, emit core.Emit) error {
	b.instructions = append(b.instructions, s.Instructions)
	s.Messages = append(s.Messages, core.Message{Role: "user", Content: prompt.Text})
	if b.taint != "" {
		executor.Execute(ctx, core.ToolCall{ID: "t", Name: b.taint, Arguments: json.RawMessage(`{"tasks":[]}`)})
	}
	s.Messages = append(s.Messages, core.Message{Role: "assistant", Content: b.reply})
	emit(core.Event{Kind: "text", Text: b.reply})
	return nil
}
func (b *reflectingBackend) Complete(_ context.Context, instructions, prompt string, schema any) (string, core.TokenUsage, error) {
	b.completions = append(b.completions, completion{instructions, prompt, schema})
	return b.answer, b.spent, b.completeErr
}

// plainBackend answers turns but cannot complete an isolated prompt.
type plainBackend struct{}

func (plainBackend) Name() string { return "fake" }
func (plainBackend) Run(_ context.Context, s *core.Session, prompt core.Prompt, _ core.ToolExecutor, _ core.Emit) error {
	s.Messages = append(s.Messages, core.Message{Role: "user", Content: prompt.Text}, core.Message{Role: "assistant", Content: "ok"})
	return nil
}

const reflectionAnswer = `{"operations":[{"action":"save","scope":"workspace","name":"make-check","type":"feedback","description":"Run make check before reporting done","body":"**Why:** asked on 2026-10-16.","paths":[],"commands":["^go test"],"keywords":["tests"],"evidence":"user: run make check"}],"helped":[],"misled":[]}`

func reflectionRunner(t *testing.T, backend core.Backend, mode string) (*runtimeBackend, *memory.Store, *core.Session) {
	t.Helper()
	cfg := config.Default()
	cfg.SkillPaths = []string{}
	cfg.Mode = mode
	store := &memory.Store{Directory: t.TempDir()}
	runner := &runtimeBackend{backend: backend, cfg: cfg, timeout: time.Minute, parent: context.Background(), memory: store}
	s := session.New(t.TempDir(), "fake", "model-a")
	return runner, store, s
}

func TestReflectionSavesWhatTheTurnTaught(t *testing.T) {
	backend := &reflectingBackend{reply: "Done, sir.", answer: reflectionAnswer, spent: core.TokenUsage{Input: 900, Output: 40}}
	runner, store, s := reflectionRunner(t, backend, "auto")
	var events []core.Event
	emit := func(e core.Event) { events = append(events, e) }
	if err := runner.Run(context.Background(), s, core.Prompt{Text: "Fix the build and always run make check first"}, nil, emit); err != nil {
		t.Fatal(err)
	}
	if len(backend.completions) != 1 {
		t.Fatalf("reflection ran %d times", len(backend.completions))
	}
	c := backend.completions[0]
	if c.instructions != memory.ReflectionInstructions || c.schema == nil || !strings.Contains(c.prompt, "## The turn, as JSON") || !strings.Contains(c.prompt, "always run make check first") || !strings.Contains(c.prompt, "Done, sir.") || strings.Contains(c.prompt, "outside the user's own words") {
		t.Fatalf("reflection prompt:\n%s", c.prompt)
	}
	note, err := store.Load(memory.Scope{Kind: memory.ScopeWorkspace, Workspace: s.Directory}, "make-check")
	if err != nil || note.Source != memory.SourceReflection || note.Session != s.ID || note.Tainted || note.Triggers.Commands[0] != "^go test" {
		t.Fatalf("saved note %+v %v", note, err)
	}
	var started, ended, aggregate bool
	for _, e := range events {
		switch {
		case e.Kind == "tool_start" && e.Call != nil && e.Call.Name == tools.MemoryTool && e.Call.ID == "reflection-0-1" && e.Text == "reflection":
			started = true
		case e.Kind == "tool_end" && e.Call != nil && e.Call.ID == "reflection-0-1" && e.Result != nil && e.Result.Content == "Saved workspace note make-check" && !e.Result.IsError:
			ended = true
		case e.Kind == "usage" && e.Aggregate && e.OutputTokens == 40:
			aggregate = true
		}
	}
	if !started || !ended || !aggregate || s.Usage.Total.Output != 40 {
		t.Fatalf("events %+v usage %+v", events, s.Usage)
	}
	// The change stays in the transcript, so a resumed session shows it.
	if n := len(s.Messages); n != 4 || s.Messages[2].Role != "assistant" || len(s.Messages[2].ToolCalls) != 1 || s.Messages[2].ToolCalls[0].ID != "reflection-0-1" || s.Messages[3].ToolCallID != "reflection-0-1" || s.Messages[3].Content != "Saved workspace note make-check" {
		t.Fatalf("transcript %+v", s.Messages)
	}
	// The next turn sees the note, in full when the request names its keyword.
	if err := runner.Run(context.Background(), s, core.Prompt{Text: "Run the tests again"}, nil, emit); err != nil {
		t.Fatal(err)
	}
	if text := backend.instructions[1]; !strings.Contains(text, "## Memory\n\nYou get better at this work over months") || !strings.Contains(text, "### Relevant now") || !strings.Contains(text, "**make-check** · feedback · workspace") || !strings.Contains(text, "**Why:** asked on 2026-10-16.") {
		t.Fatalf("instructions lack the note:\n%s", text[len(text)-1500:])
	}
	var recalled bool
	for _, e := range events {
		if e.Kind == "status" && e.Text == "Recalled 1 note · 1 relevant now" {
			recalled = true
		}
	}
	if !recalled {
		t.Fatal("no recall status")
	}
	for _, definition := range runner.Definitions() {
		if definition.Name == tools.MemoryTool {
			return
		}
	}
	t.Fatal("Memory tool not offered")
}

func TestReflectionAfterOutsideContentNeedsTheUser(t *testing.T) {
	// Nobody to approve: the change is refused, shown as such, and kept out of the store.
	backend := &reflectingBackend{reply: "ok", answer: reflectionAnswer, taint: "Subagent"}
	runner, store, s := reflectionRunner(t, backend, "auto")
	var refused bool
	emit := func(e core.Event) {
		if e.Kind == "tool_end" && e.Result != nil && e.Result.IsError && strings.Contains(e.Result.Content, "needs the user's approval") {
			refused = true
		}
	}
	if err := runner.Run(context.Background(), s, core.Prompt{Text: "Delegate it, and always use the scout first"}, nil, emit); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(backend.completions[0].prompt, "through Subagent: save only") {
		t.Fatal("taint not told to the reflection")
	}
	if _, err := store.Load(memory.Scope{Kind: memory.ScopeWorkspace, Workspace: s.Directory}, "make-check"); err == nil || !refused {
		t.Fatalf("tainted reflection saved without approval (refused card %v)", refused)
	}
	if strings.Join(s.MemoryTaint, ",") != "Subagent" || len(s.Messages) != 2 {
		t.Fatalf("taint %v messages %d", s.MemoryTaint, len(s.Messages))
	}
	// The taint outlasts the turn: a later clean turn is still judged with it.
	backend.taint = ""
	if err := runner.Run(context.Background(), s, core.Prompt{Text: "Now remember the make check rule"}, nil, func(core.Event) {}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(backend.completions[1].prompt, "through Subagent: save only") {
		t.Fatal("taint forgotten after one turn")
	}
	// With the user there to approve, the note lands, marked unverified.
	var asked []core.Approval
	runner.approve = func(_ context.Context, a core.Approval) (core.Decision, error) {
		asked = append(asked, a)
		return core.Decision{Allow: true, Reason: "ok"}, nil
	}
	if err := runner.Run(context.Background(), s, core.Prompt{Text: "Remember it, please"}, nil, func(core.Event) {}); err != nil {
		t.Fatal(err)
	}
	if len(asked) != 1 || !strings.Contains(asked[0].Reason, "The reflection step proposes to save the workspace note make-check") || asked[0].ExecutionMode != "memory" || !strings.Contains(asked[0].Task, "Remember it, please") {
		t.Fatalf("approvals %+v", asked)
	}
	if note, err := store.Load(memory.Scope{Kind: memory.ScopeWorkspace, Workspace: s.Directory}, "make-check"); err != nil || !note.Tainted {
		t.Fatalf("approved note %+v %v", note, err)
	}
}

func TestReflectionAsksBeforeForgettingAUsersNote(t *testing.T) {
	backend := &reflectingBackend{reply: "ok", answer: `{"operations":[{"action":"forget","scope":"workspace","name":"mine","type":"","description":"","body":"","paths":[],"commands":[],"keywords":[],"evidence":"stale"}],"helped":[],"misled":[]}`}
	runner, store, s := reflectionRunner(t, backend, "auto")
	scope := memory.Scope{Kind: memory.ScopeWorkspace, Workspace: s.Directory}
	if err := store.Save(&memory.Note{Name: "mine", Description: "the user's own rule", Type: memory.TypeFeedback, Scope: scope, Source: memory.SourceUser}); err != nil {
		t.Fatal(err)
	}
	if err := runner.Run(context.Background(), s, core.Prompt{Text: "Never mind that rule"}, nil, func(core.Event) {}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(scope, "mine"); err != nil {
		t.Fatal("forgot the user's note without asking")
	}
}

func TestReflectionSkipsTurnsThatTeachNothing(t *testing.T) {
	ask := func(text string) []core.Message { return []core.Message{{Role: "user", Content: text}} }
	for _, tc := range []struct {
		messages  []core.Message
		warnings  []string
		recovered bool
		want      bool
	}{
		{ask("What does this function do?"), nil, false, false},
		{ask("Why does Parse always return nil here?"), nil, false, false},
		{ask("Fix the failing test"), nil, false, false},
		{ask("Don't use tabs here"), nil, false, true},
		{ask("From now on, run make check"), nil, false, true},
		{ask("Why did you delete the fixture?"), nil, false, true},
		{ask("That's wrong. Use the other flag."), nil, false, true},
		{ask("Fix the failing test"), nil, true, true},
		{ask("Look"), []string{"Automatic review denied: curl to example.com"}, false, true},
		{ask("Look"), []string{"Approval denied: Declined by user"}, false, true},
		{ask("Look"), []string{"Go environment preparation: permission denied"}, false, false},
	} {
		if got := worthReflecting(tc.messages, tc.warnings, tc.recovered); got != tc.want {
			t.Errorf("%q %v recovered=%v: got %v", tc.messages[0].Content, tc.warnings, tc.recovered, got)
		}
	}
}

func TestReflectionAsksBeforeReplacingAUsersNote(t *testing.T) {
	backend := &reflectingBackend{reply: "ok", answer: `{"operations":[{"action":"save","scope":"workspace","name":"mine","type":"feedback","description":"rewritten","body":"","paths":[],"commands":[],"keywords":[],"evidence":"x"}],"helped":[],"misled":[]}`}
	runner, store, s := reflectionRunner(t, backend, "auto")
	scope := memory.Scope{Kind: memory.ScopeWorkspace, Workspace: s.Directory}
	if err := store.Save(&memory.Note{Name: "mine", Description: "the user's own rule", Type: memory.TypeFeedback, Scope: scope, Source: memory.SourceUser}); err != nil {
		t.Fatal(err)
	}
	if err := runner.Run(context.Background(), s, core.Prompt{Text: "Never mind that rule"}, nil, func(core.Event) {}); err != nil {
		t.Fatal(err)
	}
	if note, err := store.Load(scope, "mine"); err != nil || note.Description != "the user's own rule" {
		t.Fatalf("rewrote the user's note without asking: %+v %v", note, err)
	}
}

func TestReflectionSkipsWhereItShould(t *testing.T) {
	for name, setup := range map[string]func(r *runtimeBackend){
		"read-only memory": func(r *runtimeBackend) { r.memoryReadOnly = true },
		"ask mode":         func(r *runtimeBackend) { r.cfg.Mode = "ask" },
		"reflection off":   func(r *runtimeBackend) { r.cfg.Memory.Reflection = "off" },
		"consolidating":    func(r *runtimeBackend) { r.consolidating = true },
		"no memory":        func(r *runtimeBackend) { r.memory = nil },
	} {
		backend := &reflectingBackend{reply: "ok", answer: reflectionAnswer}
		runner, _, s := reflectionRunner(t, backend, "auto")
		setup(runner)
		if err := runner.Run(context.Background(), s, core.Prompt{Text: "Always say hello"}, nil, func(core.Event) {}); err != nil {
			t.Fatal(name, err)
		}
		if len(backend.completions) != 0 {
			t.Fatalf("%s: reflection ran", name)
		}
	}
}

func TestReflectionFailuresAreWarningsNotErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		backend core.Backend
		want    string
	}{
		"provider error": {&reflectingBackend{reply: "ok", completeErr: errors.New("boom")}, "Reflection failed: boom"},
		"not json":       {&reflectingBackend{reply: "ok", answer: "I would save a note."}, "Reflection discarded"},
		"no completer":   {plainBackend{}, "Reflection is unavailable on fake/model-a"},
	} {
		runner, _, s := reflectionRunner(t, tc.backend, "auto")
		var texts []string
		err := runner.Run(context.Background(), s, core.Prompt{Text: "Never do that again"}, nil, func(e core.Event) {
			if e.Kind == "warning" || e.Kind == "status" {
				texts = append(texts, e.Text)
			}
		})
		if err != nil {
			t.Fatal(name, err)
		}
		found := false
		for _, text := range texts {
			if strings.Contains(text, tc.want) {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s: %v", name, texts)
		}
	}
}

func TestReflectionSettingsFollowTheMemoryModel(t *testing.T) {
	cfg := config.Default()
	cfg.CompatibleBaseURL = "https://openrouter.ai/api/v1"
	cfg.APIKeyEnv = "MY_KEY"
	parent := cfg
	parent.Reasoning = "high"
	// The session's model, at the reflection's own low effort.
	if got := reflectionSettings(cfg, parent); got.Provider != "codex" || got.Reasoning != "low" {
		t.Fatalf("no model: %+v", got)
	}
	cfg.Memory.Model = &config.ModelEntry{ID: "compatible/vendor/small", Reasoning: "low"}
	got := reflectionSettings(cfg, parent)
	// The configured api_key_env names the compatible endpoint's key, as it does for a subagent.
	if got.Provider != "compatible" || got.Model != "vendor/small" || got.Reasoning != "low" || got.APIKeyEnv != "MY_KEY" {
		t.Fatalf("configured model: %+v", got)
	}
}

// gatedBackend is a reflectingBackend whose reflection waits for gate, or
// for its context to end, as a slow model call would.
type gatedBackend struct {
	*reflectingBackend
	gate    chan struct{}
	entered chan struct{}
}

func (b *gatedBackend) Complete(ctx context.Context, instructions, prompt string, schema any) (string, core.TokenUsage, error) {
	b.entered <- struct{}{}
	select {
	case <-b.gate:
		return b.reflectingBackend.Complete(ctx, instructions, prompt, schema)
	case <-ctx.Done():
		return "", core.TokenUsage{}, ctx.Err()
	}
}

// afterTurnRunner is a runner whose reflections run after the turn, with the
// followups it announces collected in order.
func afterTurnRunner(t *testing.T, mode string) (*runtimeBackend, *gatedBackend, *memory.Store, *core.Session, chan ui.Followup) {
	t.Helper()
	backend := &gatedBackend{reflectingBackend: &reflectingBackend{reply: "Done, sir.", answer: reflectionAnswer, spent: core.TokenUsage{Input: 900, Output: 40}}, gate: make(chan struct{}), entered: make(chan struct{}, 4)}
	runner, store, s := reflectionRunner(t, backend, mode)
	followups := make(chan ui.Followup, 16)
	runner.followups, runner.closed = followups, make(chan struct{})
	t.Cleanup(func() { runner.finish(context.Background(), nil) })
	return runner, backend, store, s, followups
}

func next(t *testing.T, followups chan ui.Followup) ui.Followup {
	t.Helper()
	select {
	case f := <-followups:
		return f
	case <-time.After(5 * time.Second):
		t.Fatal("no followup")
		return ui.Followup{}
	}
}

func TestReflectionRunsAfterTheTurnAndJoinsTheNextOne(t *testing.T) {
	runner, backend, store, s, followups := afterTurnRunner(t, "auto")
	if err := runner.Run(context.Background(), s, core.Prompt{Text: "Always run make check first"}, nil, func(core.Event) {}); err != nil {
		t.Fatal(err)
	}
	// The turn is over while the reflection still waits on its model.
	<-backend.entered
	started := next(t, followups)
	if started.Done || started.SessionID != s.ID || !strings.HasPrefix(started.Status, "Reflecting on your last turn · ") || !strings.HasSuffix(started.Status, " · Esc skips") || started.Cancel == nil {
		t.Fatalf("started %+v", started)
	}
	if len(s.Messages) != 2 || s.Usage.Total.Output != 0 {
		t.Fatalf("the session took in a reflection that has not run: %d messages", len(s.Messages))
	}
	close(backend.gate)
	done := next(t, followups)
	if !done.Done || len(done.Events) != 2 || done.Events[0].Kind != "tool_start" || done.Events[0].Text != "reflection" || done.Events[1].Result.Content != "Saved workspace note make-check" {
		t.Fatalf("done %+v", done)
	}
	if _, err := store.Load(memory.Scope{Kind: memory.ScopeWorkspace, Workspace: s.Directory}, "make-check"); err != nil {
		t.Fatal("note not kept:", err)
	}
	// The next turn takes the reflection in before its own messages.
	var usage []core.Event
	if err := runner.Run(context.Background(), s, core.Prompt{Text: "Thanks"}, nil, func(e core.Event) {
		if e.Kind == "usage" && e.Aggregate {
			usage = append(usage, e)
		}
	}); err != nil {
		t.Fatal(err)
	}
	roles := []string{}
	for _, m := range s.Messages {
		roles = append(roles, m.Role)
	}
	if strings.Join(roles, ",") != "user,assistant,assistant,tool,user,assistant" || s.Messages[2].ToolCalls[0].ID != "reflection-0-1" || s.Messages[4].Content != "Thanks" {
		t.Fatalf("transcript %v", roles)
	}
	if len(usage) != 1 || usage[0].OutputTokens != 40 || s.Usage.Total.Output != 40 {
		t.Fatalf("usage %+v total %+v", usage, s.Usage.Total)
	}
	audited := 0
	for _, entry := range s.Audit {
		if entry.Event.Call != nil && entry.Event.Call.ID == "reflection-0-1" {
			audited++
		}
	}
	if audited != 2 {
		t.Fatalf("audit has %d reflection events", audited)
	}
}

func TestTheNextTurnWaitsForAReflectionStillRunning(t *testing.T) {
	runner, backend, _, s, followups := afterTurnRunner(t, "auto")
	if err := runner.Run(context.Background(), s, core.Prompt{Text: "Never push without asking"}, nil, func(core.Event) {}); err != nil {
		t.Fatal(err)
	}
	<-backend.entered
	next(t, followups)
	statuses := make(chan string, 8)
	finished := make(chan error, 1)
	go func() {
		finished <- runner.Run(context.Background(), s, core.Prompt{Text: "Next"}, nil, func(e core.Event) {
			if e.Kind == "status" {
				statuses <- e.Text
			}
		})
	}()
	if status := <-statuses; status != "Finishing the reflection on your last turn" {
		t.Fatalf("status %q", status)
	}
	select {
	case <-finished:
		t.Fatal("the next turn ran before the reflection on the last one ended")
	case <-time.After(100 * time.Millisecond):
	}
	close(backend.gate)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if s.Messages[2].Role != "assistant" || len(s.Messages[2].ToolCalls) != 1 || s.Messages[4].Content != "Next" {
		t.Fatalf("transcript out of order: %+v", s.Messages)
	}
}

func TestEscSkipsTheReflectionAfterTheTurn(t *testing.T) {
	runner, backend, store, s, followups := afterTurnRunner(t, "auto")
	if err := runner.Run(context.Background(), s, core.Prompt{Text: "Always run make check first"}, nil, func(core.Event) {}); err != nil {
		t.Fatal(err)
	}
	<-backend.entered
	next(t, followups).Cancel()
	if done := next(t, followups); !done.Done || len(done.Events) != 0 {
		t.Fatalf("skipped reflection reported %+v", done)
	}
	if _, err := store.Load(memory.Scope{Kind: memory.ScopeWorkspace, Workspace: s.Directory}, "make-check"); err == nil {
		t.Fatal("a skipped reflection saved a note")
	}
	if err := runner.Run(context.Background(), s, core.Prompt{Text: "Thanks"}, nil, func(core.Event) {}); err != nil {
		t.Fatal(err)
	}
	if len(s.Messages) != 4 {
		t.Fatalf("transcript %+v", s.Messages)
	}
}

func TestExitKeepsTheReflectionOnTheLastTurn(t *testing.T) {
	runner, backend, _, s, followups := afterTurnRunner(t, "auto")
	sessions := &session.Store{Directory: t.TempDir()}
	t.Cleanup(func() { sessions.Close() })
	if err := runner.Run(context.Background(), s, core.Prompt{Text: "Always run make check first"}, nil, func(core.Event) {}); err != nil {
		t.Fatal(err)
	}
	<-backend.entered
	next(t, followups)
	// The interface has gone: the wait says so, and the reflection is kept.
	notified := make(chan struct{})
	pending := make(chan map[string][]*afterTurn, 1)
	go func() {
		pending <- runner.finish(context.Background(), func() { close(notified) })
	}()
	<-notified
	close(backend.gate)
	if err := keepAfterTurns(<-pending, s, sessions); err != nil {
		t.Fatal(err)
	}
	saved, err := sessions.Load(s.ID)
	if err != nil || len(saved.Messages) != 4 || saved.Messages[3].Content != "Saved workspace note make-check" || saved.Usage.Total.Output != 40 {
		t.Fatalf("saved %+v %v", saved, err)
	}
}

func TestExitKeepsAReflectionOnAConversationLeftByClear(t *testing.T) {
	runner, backend, _, s, followups := afterTurnRunner(t, "auto")
	sessions := &session.Store{Directory: t.TempDir()}
	t.Cleanup(func() { sessions.Close() })
	if err := runner.Run(context.Background(), s, core.Prompt{Text: "Always run make check first"}, nil, func(core.Event) {}); err != nil {
		t.Fatal(err)
	}
	if err := sessions.Save(s); err != nil {
		t.Fatal(err)
	}
	<-backend.entered
	next(t, followups)
	close(backend.gate)
	next(t, followups)
	cleared := session.New(s.Directory, "fake", "model-a")
	if err := keepAfterTurns(runner.finish(context.Background(), nil), cleared, sessions); err != nil {
		t.Fatal(err)
	}
	saved, err := sessions.Load(s.ID)
	if err != nil || len(saved.Messages) != 4 {
		t.Fatalf("left-behind session %+v %v", saved, err)
	}
	if len(cleared.Messages) != 0 {
		t.Fatal("the reflection joined the wrong conversation")
	}
}

func TestExitCanSkipAReflection(t *testing.T) {
	runner, backend, store, s, followups := afterTurnRunner(t, "auto")
	if err := runner.Run(context.Background(), s, core.Prompt{Text: "Always run make check first"}, nil, func(core.Event) {}); err != nil {
		t.Fatal(err)
	}
	<-backend.entered
	next(t, followups)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Ctrl+C after the interface closed
	pending := runner.finish(ctx, nil)
	if turns := pending[s.ID]; len(turns) != 1 || len(turns[0].outcome.messages) != 0 {
		t.Fatalf("pending %+v", pending)
	}
	if _, err := store.Load(memory.Scope{Kind: memory.ScopeWorkspace, Workspace: s.Directory}, "make-check"); err == nil {
		t.Fatal("a skipped reflection saved a note")
	}
}

func TestATaintedTurnStillReflectsWithinTheTurn(t *testing.T) {
	runner, backend, _, s, followups := afterTurnRunner(t, "auto")
	backend.taint = "Subagent"
	close(backend.gate)
	var asked int
	runner.approve = func(context.Context, core.Approval) (core.Decision, error) {
		asked++
		return core.Decision{Allow: true}, nil
	}
	var cards int
	if err := runner.Run(context.Background(), s, core.Prompt{Text: "Delegate it, and always use the scout"}, nil, func(e core.Event) {
		if e.Kind == "tool_end" && e.Call != nil && strings.HasPrefix(e.Call.ID, "reflection-") {
			cards++
		}
	}); err != nil {
		t.Fatal(err)
	}
	if asked != 1 || cards != 1 || len(s.Messages) != 4 {
		t.Fatalf("asked %d cards %d messages %d", asked, cards, len(s.Messages))
	}
	select {
	case f := <-followups:
		t.Fatalf("a reflection that needed the user ran after the turn: %+v", f)
	default:
	}
}

func TestAfterTheTurnAWeightyChangeIsRefusedNotAsked(t *testing.T) {
	runner, backend, store, s, followups := afterTurnRunner(t, "auto")
	backend.answer = `{"operations":[{"action":"forget","scope":"workspace","name":"mine","type":"","description":"","body":"","paths":[],"commands":[],"keywords":[],"evidence":"x"}],"helped":[],"misled":[]}`
	scope := memory.Scope{Kind: memory.ScopeWorkspace, Workspace: s.Directory}
	if err := store.Save(&memory.Note{Name: "mine", Description: "the user's own rule", Type: memory.TypeFeedback, Scope: scope, Source: memory.SourceUser}); err != nil {
		t.Fatal(err)
	}
	runner.approve = func(context.Context, core.Approval) (core.Decision, error) {
		t.Error("asked after the turn")
		return core.Decision{}, nil
	}
	close(backend.gate)
	if err := runner.Run(context.Background(), s, core.Prompt{Text: "Never mind that rule"}, nil, func(core.Event) {}); err != nil {
		t.Fatal(err)
	}
	next(t, followups)
	done := next(t, followups)
	if len(done.Events) != 2 || !done.Events[1].Result.IsError || !strings.Contains(done.Events[1].Result.Content, "cannot be asked once the turn has ended") {
		t.Fatalf("done %+v", done.Events)
	}
	if _, err := store.Load(scope, "mine"); err != nil {
		t.Fatal("forgot the user's note")
	}
}

// A closed window cancels a turn that waits for the last turn's reflection,
// after the interface has stopped listening: the turn must still end.
func TestAWaitingTurnEndsWhenTheInterfaceHasGone(t *testing.T) {
	backend := &gatedBackend{reflectingBackend: &reflectingBackend{reply: "ok", answer: reflectionAnswer}, gate: make(chan struct{}), entered: make(chan struct{}, 4)}
	runner, _, s := reflectionRunner(t, backend, "auto")
	followups := make(chan ui.Followup)
	runner.followups, runner.closed = followups, make(chan struct{})
	t.Cleanup(func() { runner.finish(context.Background(), nil) })
	listening := make(chan struct{})
	go func() {
		<-followups // the reflection starts; then the interface goes away
		close(listening)
	}()
	if err := runner.Run(context.Background(), s, core.Prompt{Text: "Always run make check first"}, nil, func(core.Event) {}); err != nil {
		t.Fatal(err)
	}
	<-backend.entered
	<-listening
	ctx, cancel := context.WithCancel(context.Background())
	waiting := make(chan struct{})
	ended := make(chan error, 1)
	go func() {
		ended <- runner.Run(ctx, s, core.Prompt{Text: "Next"}, nil, func(e core.Event) {
			if e.Kind == "status" && e.Text == "Finishing the reflection on your last turn" {
				close(waiting)
			}
		})
	}()
	<-waiting
	cancel()
	select {
	case err := <-ended:
		if err == nil {
			t.Fatal("a cancelled turn reported success")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the turn never ended")
	}
}

// What needs the user is decided on the notes as they are when the
// reflection answers, not as they were when it began.
func TestAReflectionSeesNotesWrittenWhileItThought(t *testing.T) {
	runner, backend, store, s, followups := afterTurnRunner(t, "auto")
	if err := runner.Run(context.Background(), s, core.Prompt{Text: "Always run make check first"}, nil, func(core.Event) {}); err != nil {
		t.Fatal(err)
	}
	<-backend.entered
	next(t, followups)
	scope := memory.Scope{Kind: memory.ScopeWorkspace, Workspace: s.Directory}
	if err := store.Save(&memory.Note{Name: "make-check", Description: "the user's own words", Type: memory.TypeFeedback, Scope: scope, Source: memory.SourceUser}); err != nil {
		t.Fatal(err)
	}
	close(backend.gate)
	done := next(t, followups)
	if len(done.Events) != 2 || !done.Events[1].Result.IsError {
		t.Fatalf("done %+v", done.Events)
	}
	if note, _ := store.Load(scope, "make-check"); note.Description != "the user's own words" {
		t.Fatal("rewrote a note the user wrote while the reflection thought")
	}
}

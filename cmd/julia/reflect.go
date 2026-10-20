package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"julia/internal/config"
	"julia/internal/core"
	"julia/internal/memory"
	"julia/internal/session"
	"julia/internal/tools"
	"julia/internal/ui"
)

// reflectionTimeout bounds the reflection step: one model call over the
// turn's transcript, after the answer is already on screen.
const reflectionTimeout = 2 * time.Minute

// reflectionEffort is the reasoning effort the reflection step runs at when
// the memory setting gives none: it reads one turn and answers in a few
// lines, so thinking at length buys nothing but latency.
const reflectionEffort = "low"

// reflectionSettings is what the reflection step runs on: the model the
// memory setting assigns it, else the session's own, at the setting's effort
// or reflectionEffort.
func reflectionSettings(cfg, parent config.Config) config.Config {
	child := parent
	child.Reasoning = reflectionEffort
	if cfg.Memory.Model == nil {
		return child
	}
	child.Provider, child.Model = config.SplitModel(cfg.Memory.Model.ID)
	if cfg.Memory.Model.Reasoning != "" {
		child.Reasoning = cfg.Memory.Model.Reasoning
	}
	return child
}

// reflects reports whether a finished turn is reviewed by the reflection
// step: memory is on and writable, the setting allows it, the mode writes
// notes without asking (in ask mode the agent's own saves are approved one
// by one, and a step after the turn would have nobody to ask), this is not
// itself the consolidation pass, and the turn ended of its own accord.
func (r *runtimeBackend) reflects(ctx context.Context, err error) bool {
	return r.memory != nil && !r.memoryReadOnly && !r.consolidating && r.cfg.ReflectionEnabled() && (r.cfg.Mode == "auto" || r.cfg.Mode == "yolo") && err == nil && ctx.Err() == nil
}

// correctionCue finds the words a user corrects or instructs with, and
// questionCue the ones a question corrects with: "why does it always fail?"
// asks, "why did you delete it?" corrects.
var (
	correctionCue = regexp.MustCompile(`(?i)\b(don'?t|do not|never|always|instead|prefer|stop|wrong|remember|forget|from now on|next time|should(n'?t| not)? have|why did you|rather than|not like that|in future|going forward)\b`)
	questionCue   = regexp.MustCompile(`(?i)\b(wrong|should(n'?t| not)? have|why did you|didn'?t I (say|ask|tell))\b`)
	sentence      = regexp.MustCompile(`[^.!?\n]+[.!?\n]*`)
)

// corrects reports whether a user's message corrects or instructs: a cue in
// a statement, or a corrective cue in a question.
func corrects(text string) bool {
	for _, s := range sentence.FindAllString(text, -1) {
		if strings.Contains(s, "?") {
			if questionCue.MatchString(s) {
				return true
			}
		} else if correctionCue.MatchString(s) {
			return true
		}
	}
	return false
}

// worthReflecting reports whether a turn can have taught something durable:
// the user corrected or instructed, a review denied an action, or a tool
// failed and the work went on, which is where an approach that works is
// found. Most turns teach nothing, and a reflection call after every reply
// would cost a model call and the user's wait for nothing.
func worthReflecting(messages []core.Message, warnings []string, recovered bool) bool {
	if recovered {
		return true
	}
	for _, warning := range warnings {
		if strings.Contains(warning, "review denied") || strings.HasPrefix(warning, "Approval denied") {
			return true
		}
	}
	for _, message := range messages {
		if message.Role == "user" && corrects(message.Content) {
			return true
		}
	}
	return false
}

// reflectionInput is what the reflection step reads of a finished turn,
// copied, so it can run after the session has moved on.
type reflectionInput struct {
	session, directory string
	// messages are the turn's; start is where they began in the transcript
	// and after where the turn ended, which is where what the reflection
	// changed joins it.
	messages     []core.Message
	start, after int
	warnings     []string
	tainted      []string
	task         string
	selected     config.Config
	backend      core.Backend
	// canAsk says whether a change that needs the user's approval may be
	// put to them: within the turn it may; after it, no one is asked.
	canAsk bool
}

// reflectionOutcome is what a reflection did: the cards and warnings to
// show, the messages that join the transcript, and the tokens it spent.
type reflectionOutcome struct {
	events   []core.Event
	messages []core.Message
	usage    core.TokenUsage
}

// reflection runs the reflection step over a finished turn and applies what
// it asks for. It is a separate, tool-free model call on a backend made for
// it, at low effort unless the memory setting says otherwise; started is
// told the model once the call begins. In auto mode, a change after content
// from outside the user's words, or one that rewrites or forgets a global
// note or a note the user wrote, goes to the user as the Memory tool's
// would, and where no one can be asked it is refused. Each change becomes a
// Memory card, marked as the reflection's, and a message of the transcript;
// a failure is a warning, never an error of the turn, whose work is done.
func (r *runtimeBackend) reflection(ctx context.Context, in reflectionInput, started func(model string)) reflectionOutcome {
	var out reflectionOutcome
	warn := func(text string) { out.events = append(out.events, core.Event{Kind: "warning", Text: text}) }
	ctx, cancel := context.WithTimeout(ctx, reflectionTimeout)
	defer cancel()
	recall, err := r.memory.Recall(in.directory)
	if err != nil {
		warn("Reflection skipped: " + err.Error())
		return out
	}
	completer, ok := in.backend.(core.Completer)
	model := config.JoinModel(in.selected.Provider, in.selected.Model)
	if r.makeBackend != nil {
		settings := reflectionSettings(r.cfg, in.selected)
		model = config.JoinModel(settings.Provider, settings.Model)
		made, _, err := r.makeBackend(ctx, settings)
		if err != nil {
			warn("Reflection skipped: " + model + ": " + err.Error())
			return out
		}
		completer, ok = made.(core.Completer)
	}
	if !ok {
		out.events = append(out.events, core.Event{Kind: "status", Text: "Reflection is unavailable on " + model})
		return out
	}
	started(model)
	turn := memory.Turn{Workspace: in.directory, Messages: in.messages, Warnings: in.warnings, Tainted: in.tainted, At: time.Now()}
	text, usage, err := completer.Complete(ctx, memory.ReflectionInstructions, memory.ReflectionPrompt(turn, recall), memory.ReflectionSchema)
	out.usage = usage
	if err != nil {
		if ctx.Err() == nil {
			warn("Reflection failed: " + err.Error())
		}
		return out
	}
	reflection, err := memory.ParseReflection(text)
	if err != nil {
		warn("Reflection discarded: " + err.Error())
		return out
	}
	// The call can take a while, and a note may have been written meanwhile:
	// what needs the user is decided on the notes as they are now.
	if fresh, err := r.memory.Recall(in.directory); err == nil {
		recall = fresh
	}
	var calls []core.ToolCall
	var results []core.Message
	for i, a := range r.applyReflection(ctx, in, reflection, recall) {
		arguments, _ := json.Marshal(a.Operation)
		call := core.ToolCall{ID: fmt.Sprintf("reflection-%d-%d", in.start, i+1), Name: tools.MemoryTool, Arguments: arguments}
		result := core.ToolResult{}
		if a.Err == nil && a.Note == nil {
			a.Err = errors.New("it was not applied")
		}
		if a.Err != nil {
			result.Content, result.IsError = "Reflection could not "+a.Operation.Action+" "+a.Operation.Name+": "+a.Err.Error(), true
		} else {
			result.Content = fmt.Sprintf("%s %s note %s", a.Verb, a.Note.Scope, a.Note.Name)
			if a.Verb == "Forgot" {
				result.Content += ": " + a.Note.Description
			}
			calls = append(calls, call)
			results = append(results, core.Message{Role: "tool", ToolCallID: call.ID, Content: result.Content})
		}
		out.events = append(out.events, core.Event{Kind: "tool_start", Call: &call, Text: "reflection"}, core.Event{Kind: "tool_end", Call: &call, Result: &result})
	}
	// What the reflection changed stays in the conversation, so a resumed
	// session shows it and a later turn knows it happened.
	if len(calls) > 0 {
		out.messages = append([]core.Message{{Role: "assistant", ToolCalls: calls}}, results...)
	}
	return out
}

// settle adds what a reflection changed to its session: its messages where
// its turn ended, its spend to the totals, and, when audit is set, its
// events to the audit, which a reflection within the turn writes as it
// emits them.
func settle(s *core.Session, after int, out reflectionOutcome, audit bool) {
	at := min(max(after, 0), len(s.Messages))
	s.Messages = slices.Concat(s.Messages[:at:at], out.messages, s.Messages[at:])
	if !out.usage.IsZero() {
		s.RecordUsage(core.AggregateUsageEvent(out.usage))
	}
	if audit {
		for _, event := range out.events {
			s.Audit = append(s.Audit, core.AuditEntry{At: time.Now(), Event: event})
		}
	}
}

// afterTurn is a reflection running, or run, after its turn ended. What it
// changed waits here until its session takes it in: at the start of the
// session's next turn, or when JULIA exits.
type afterTurn struct {
	after   int
	done    chan struct{}
	cancel  context.CancelFunc
	outcome reflectionOutcome // written before done closes
}

// reflectLater runs the reflection after its turn has ended, so the
// interface is free as soon as the answer is: it says when the reflection
// starts, which Esc can then skip, and shows its cards when it ends.
func (r *runtimeBackend) reflectLater(in reflectionInput) {
	ctx, cancel := context.WithCancel(r.parent)
	turn := &afterTurn{after: in.after, done: make(chan struct{}), cancel: cancel}
	r.afterMu.Lock()
	if r.afterTurns == nil {
		r.afterTurns = map[string][]*afterTurn{}
	}
	r.afterTurns[in.session] = append(r.afterTurns[in.session], turn)
	r.afterMu.Unlock()
	go func() {
		defer cancel()
		turn.outcome = r.reflection(ctx, in, func(string) {
			r.announce(ctx, ui.Followup{SessionID: in.session, Status: "Reflecting on your last turn · Esc skips", Cancel: cancel})
		})
		// The outcome is published before it is shown: a turn waiting for
		// it must not depend on an interface that may already have gone.
		close(turn.done)
		r.announce(nil, ui.Followup{SessionID: in.session, Done: true, Events: turn.outcome.events})
	}()
}

// announce hands a followup to the interface, unless none listens any more,
// or, when ctx is given, the work it announces has been cancelled.
func (r *runtimeBackend) announce(ctx context.Context, f ui.Followup) {
	var cancelled <-chan struct{}
	if ctx != nil {
		cancelled = ctx.Done()
	}
	select {
	case r.followups <- f:
	case <-r.closed:
	case <-cancelled:
	}
}

// settleEarlier takes into a session, as its next turn starts, what
// reflections on its earlier turns changed, waiting for one still running
// so the transcript keeps its order. A cancelled wait skips the reflection.
func (r *runtimeBackend) settleEarlier(ctx context.Context, s *core.Session, emit core.Emit) {
	r.afterMu.Lock()
	turns := r.afterTurns[s.ID]
	delete(r.afterTurns, s.ID)
	r.afterMu.Unlock()
	for _, turn := range turns {
		select {
		case <-turn.done:
		default:
			emit(core.Event{Kind: "status", Text: "Finishing the reflection on your last turn"})
			select {
			case <-turn.done:
			case <-ctx.Done():
				turn.cancel()
				<-turn.done
			}
		}
		if !turn.outcome.usage.IsZero() {
			emit(core.AggregateUsageEvent(turn.outcome.usage))
		}
		settle(s, turn.after, turn.outcome, true)
	}
}

// finish ends the runtime's work once the interface has gone: a reflection
// still running is waited for, so what it learned is kept, until it ends or
// ctx stops it; notify is called first when there is one to wait for. It
// returns, by session, what has yet to join the transcript.
func (r *runtimeBackend) finish(ctx context.Context, notify func()) map[string][]*afterTurn {
	r.afterMu.Lock()
	if r.closed != nil {
		select {
		case <-r.closed:
		default:
			close(r.closed)
		}
	}
	pending := r.afterTurns
	r.afterTurns = nil
	r.afterMu.Unlock()
	running := false
	for _, turns := range pending {
		for _, turn := range turns {
			select {
			case <-turn.done:
			default:
				running = true
			}
		}
	}
	if running && notify != nil {
		notify()
	}
	for _, turns := range pending {
		for _, turn := range turns {
			select {
			case <-turn.done:
			case <-ctx.Done():
				turn.cancel()
				<-turn.done
			}
		}
	}
	return pending
}

// keepAfterTurns saves into each session, once the interface has gone, what
// reflections after its last turns changed: into s, the session on screen
// at the end, or into one left behind by /clear, loaded from the store.
func keepAfterTurns(pending map[string][]*afterTurn, s *core.Session, store *session.Store) error {
	var failures []error
	for id, turns := range pending {
		target := s
		if id != s.ID {
			loaded, err := store.Load(id)
			if err != nil {
				failures = append(failures, fmt.Errorf("keep the reflection on session %s: %w", id, err))
				continue
			}
			target = loaded
		}
		for _, turn := range turns {
			settle(target, turn.after, turn.outcome, true)
		}
		if err := store.Save(target); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

// applyReflection carries out the reflection's operations, putting to the
// user, in auto mode, each one the Memory tool would put to them, and
// refusing those no one can approve. The notes it judged are tallied either
// way: a tally changes no lesson.
func (r *runtimeBackend) applyReflection(ctx context.Context, in reflectionInput, reflection memory.Reflection, recall memory.Recall) []memory.Applied {
	out := make([]memory.Applied, len(reflection.Operations))
	approved := memory.Reflection{Helped: reflection.Helped, Misled: reflection.Misled}
	var positions []int
	for i, op := range reflection.Operations {
		if err := r.approveReflection(ctx, in, op, recall); err != nil {
			out[i] = memory.Applied{Operation: op, Err: err}
			continue
		}
		approved.Operations = append(approved.Operations, op)
		positions = append(positions, i)
	}
	for j, a := range r.memory.Apply(approved, in.directory, in.session, len(in.tainted) > 0) {
		if j < len(positions) {
			out[positions[j]] = a
		}
	}
	return out
}

// approveReflection decides one operation of the reflection step as the
// Memory tool decides its own: free in yolo mode and in an untainted auto
// turn, but put to the user after outside content and before rewriting or
// forgetting a global note or one the user wrote.
func (r *runtimeBackend) approveReflection(ctx context.Context, in reflectionInput, op memory.Operation, recall memory.Recall) error {
	if r.cfg.Mode != "auto" {
		return nil
	}
	// Forgetting or rewriting a global note, or one the user wrote, is
	// weighty; adding a new note is not.
	weighty := false
	for _, note := range append(append([]*memory.Note(nil), recall.Workspace...), recall.Global...) {
		if note.Name == op.Name && note.Scope.String() == op.Scope && (note.Scope.Kind == memory.ScopeGlobal || note.Source == memory.SourceUser) {
			weighty = true
		}
	}
	if len(in.tainted) == 0 && !weighty {
		return nil
	}
	reason := fmt.Sprintf("The reflection step proposes to %s the %s note %s.", op.Action, op.Scope, op.Name)
	if len(in.tainted) > 0 {
		reason = "This conversation read content from outside the user's words (" + strings.Join(in.tainted, ", ") + "). " + reason
	}
	if !in.canAsk {
		return errors.New("needs your approval, which cannot be asked once the turn has ended; ask JULIA to do it")
	}
	if r.headless || r.approve == nil {
		return errors.New("this change needs the user's approval, and no one can give it in this run")
	}
	arguments, _ := json.Marshal(op)
	decision, err := r.approve(ctx, core.Approval{Call: core.ToolCall{ID: "reflection", Name: tools.MemoryTool, Arguments: arguments}, Task: in.task, Directory: in.directory, ExecutionMode: "memory", Reason: reason})
	if err != nil {
		return fmt.Errorf("approval: %w", err)
	}
	if !decision.Allow {
		return fmt.Errorf("approval denied: %s", decision.Reason)
	}
	return nil
}

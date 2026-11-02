package provider

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"julia/internal/core"
)

// liveClaudeBinary is the Claude Code CLI the live tests drive, or a skip.
func liveClaudeBinary(t *testing.T) string {
	t.Helper()
	binary := os.Getenv("JULIA_CLAUDE_LIVE_BINARY")
	if binary == "" {
		t.Skip("set JULIA_CLAUDE_LIVE_BINARY to the Claude Code CLI to test it")
	}
	return binary
}

// TestClaudeLiveProtocol drives the installed Claude Code binary through the
// real process plumbing: the model catalog, the account's usage, and one turn
// with JULIA's tools registered, which needs no sign-in to reach the point
// where the binary either answers or reports itself signed out. It makes no
// inference request on a signed-out binary.
func TestClaudeLiveProtocol(t *testing.T) {
	binary := liveClaudeBinary(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	backend := &Claude{Binary: binary, Model: "haiku", Reasoning: "low"}
	models, err := backend.Models(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	hasDefault := false
	for _, m := range models {
		ids = append(ids, m.ID)
		hasDefault = hasDefault || m.IsDefault
	}
	if len(models) == 0 || !hasDefault {
		t.Fatalf("models %v", ids)
	}
	t.Logf("models: %v", ids)
	usage, err := backend.AccountUsage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if usage != nil {
		t.Logf("account usage: weekly=%+v five-hour=%+v", usage.Weekly, usage.FiveHour)
	} else {
		t.Log("account usage unavailable")
	}
	session := &core.Session{Directory: t.TempDir()}
	var events []core.Event
	err = backend.Run(ctx, session, core.Prompt{Text: "Call the Read tool on README.md, then answer with one word."}, &claudeTestExecutor{}, func(e core.Event) { events = append(events, e) })
	switch {
	case errors.Is(err, errClaudeSignedOut):
		t.Logf("signed out: the handshake, session open and sign-in check ran against the real binary")
		// Only the first model call tells, and Claude Code holds the prompt
		// by then, so the transcript keeps it too, as TestClaudeSignedOut has it.
		if len(session.Messages) != 1 || session.RemoteID == "" {
			t.Fatalf("a signed-out turn lost the prompt Claude Code holds: %+v", session)
		}
	case err != nil:
		t.Fatal(err)
	default:
		t.Logf("turn completed: %d messages, %+v", len(session.Messages), session.Usage)
		if len(session.Messages) < 2 {
			t.Fatalf("transcript %+v", session.Messages)
		}
	}
}

// TestClaudeLiveSideCalls runs the isolated calls, the reflection's Complete
// and the approval review, on the installed binary. They are never saved, so
// Claude Code makes up their session ID, which a fixture can only imitate.
func TestClaudeLiveSideCalls(t *testing.T) {
	binary := liveClaudeBinary(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	backend := &Claude{Binary: binary, Model: "haiku", ReviewModel: "haiku"}

	schema := map[string]any{"type": "object", "properties": map[string]any{"word": map[string]any{"type": "string"}}, "required": []string{"word"}, "additionalProperties": false}
	answer, spent, err := backend.Complete(ctx, "Answer with the word the request names.", "The word is pineapple.", schema)
	if errors.Is(err, errClaudeSignedOut) {
		t.Skip("signed out: side calls need a model call")
	}
	if err != nil {
		t.Fatal(err)
	}
	var structured struct {
		Word string `json:"word"`
	}
	if err := json.Unmarshal([]byte(answer), &structured); err != nil || structured.Word == "" || spent.Output == 0 {
		t.Fatalf("answer %q, spent %+v, %v", answer, spent, err)
	}

	decision, err := backend.Review(ctx, core.Approval{Call: core.ToolCall{Name: "Bash", Arguments: json.RawMessage(`{"command":"ls"}`)}, Task: "List the files in the workspace"})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("review decision: %+v", decision)
}

// TestClaudeLiveSteering steers a turn while its tool runs: the message must
// join the turn and be answered, not interrupt it.
func TestClaudeLiveSteering(t *testing.T) {
	binary := liveClaudeBinary(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	inbox := core.NewInbox()
	steerOnce := func(e core.Event) {
		if e.Kind == "tool_start" {
			inbox.Send(core.Prompt{Text: "Also end your answer with the word PINEAPPLE."})
		}
	}
	session := &core.Session{Directory: t.TempDir()}
	prompt := core.Prompt{Text: "Call the Read tool on README.md once, then answer with one word.", Steering: inbox}
	err := (&Claude{Binary: binary, Model: "haiku", Reasoning: "low"}).Run(ctx, session, prompt, &claudeTestExecutor{}, steerOnce)
	if errors.Is(err, errClaudeSignedOut) {
		t.Skip("signed out: steering needs a model call")
	}
	if err != nil {
		t.Fatal(err)
	}

	steered := slices.IndexFunc(session.Messages, func(m core.Message) bool { return m.Role == "user" && strings.Contains(m.Content, "PINEAPPLE") })
	if steered < 0 {
		t.Fatalf("steer never delivered: %+v", session.Messages)
	}
	var answer strings.Builder
	for _, m := range session.Messages[steered+1:] {
		if m.Role == "assistant" {
			answer.WriteString(m.Content)
		}
	}
	if !strings.Contains(strings.ToLower(answer.String()), "pineapple") {
		t.Fatalf("steer not answered: %+v", session.Messages)
	}
}

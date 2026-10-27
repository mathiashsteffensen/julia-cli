package provider

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"julia/internal/core"
)

// TestClaudeLiveProtocol drives the installed Claude Code binary through the
// real process plumbing: the model catalog, the account's usage, and one turn
// with JULIA's tools registered, which needs no sign-in to reach the point
// where the binary either answers or reports itself signed out. It makes no
// inference request on a signed-out binary.
func TestClaudeLiveProtocol(t *testing.T) {
	binary := os.Getenv("JULIA_CLAUDE_LIVE_BINARY")
	if binary == "" {
		t.Skip("set JULIA_CLAUDE_LIVE_BINARY to the Claude Code CLI to test it")
	}
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
	usage, err := backend.WeeklyUsage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("weekly usage: %+v", usage)
	session := &core.Session{Directory: t.TempDir()}
	var events []core.Event
	err = backend.Run(ctx, session, core.Prompt{Text: "Call the Read tool on README.md, then answer with one word."}, &claudeTestExecutor{}, func(e core.Event) { events = append(events, e) })
	switch {
	case errors.Is(err, errClaudeSignedOut):
		t.Logf("signed out: the handshake, session open and sign-in check ran against the real binary")
		if len(session.Messages) != 0 || session.RemoteID != "" {
			t.Fatalf("a refused turn left state: %+v", session)
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

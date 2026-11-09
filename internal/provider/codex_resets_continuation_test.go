package provider

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"julia/internal/core"
)

func TestCodexUsageResetAmbiguousContinuationIsNotReplayed(t *testing.T) {
	f := newCodexFixture(t)
	reads, turns := 0, 0
	f.rpc = func(method string, request map[string]json.RawMessage) bool {
		switch method {
		case "account/rateLimits/read":
			reads++
			f.respond(request["id"], map[string]any{"ordinaryUsageAllowed": reads == 1, "rateLimitResetCredits": map[string]any{"availableCount": 1}})
		case "account/rateLimitResetCredit/consume":
			f.respond(request["id"], map[string]any{"outcome": "reset"})
		case "turn/start":
			turns++
			if turns == 2 {
				// A successful RPC without a turn ID cannot inherit the previous ID.
				f.respond(request["id"], map[string]any{"turn": map[string]any{}})
				return true
			}
			f.respond(request["id"], map[string]any{"turn": map[string]any{"id": "first"}})
			f.push(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": "thread-1", "turn": map[string]any{"id": "first", "status": "failed", "error": map[string]any{"message": "quota exhausted", "codexErrorInfo": "usageLimitExceeded"}}}})
		default:
			return false
		}
		return true
	}
	session := &core.Session{Directory: t.TempDir()}
	backend := &Codex{start: f.start, UseUsageResets: true}
	err := backend.Run(context.Background(), session, core.Prompt{Text: "original"}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "outcome uncertain") {
		t.Fatalf("missing continuation ID must stop the run: %v", err)
	}
	err = backend.Run(context.Background(), session, core.Prompt{Text: "continue"}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "uncertain outcome") {
		t.Fatalf("ambiguous continuation must not be replayed: %v", err)
	}
	if turns != 2 {
		t.Fatalf("started %d turns, want 2", turns)
	}
}

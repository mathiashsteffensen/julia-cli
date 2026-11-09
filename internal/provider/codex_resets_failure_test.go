package provider

import (
	"context"
	"encoding/json"
	"testing"

	"julia/internal/core"
)

func TestCodexUsageResetFailureStopsWithoutAnotherAttempt(t *testing.T) {
	for _, tt := range []struct {
		name                               string
		enabled                            bool
		outcome                            string
		rpcError                           bool
		wantReads, wantConsumes, wantTurns int
	}{
		{name: "disabled", wantTurns: 1},
		{name: "refused", enabled: true, outcome: "noCredit", wantReads: 2, wantConsumes: 1, wantTurns: 1},
		{name: "unknown", enabled: true, outcome: "future", wantReads: 2, wantConsumes: 1, wantTurns: 1},
		{name: "error", enabled: true, rpcError: true, wantReads: 2, wantConsumes: 1, wantTurns: 1},
		{name: "quota fails again", enabled: true, outcome: "reset", wantReads: 2, wantConsumes: 1, wantTurns: 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newCodexFixture(t)
			reads, consumes, turns := 0, 0, 0
			f.rpc = func(method string, request map[string]json.RawMessage) bool {
				switch method {
				case "account/rateLimits/read":
					reads++
					f.respond(request["id"], map[string]any{"ordinaryUsageAllowed": reads == 1, "rateLimitResetCredits": map[string]any{"availableCount": 5}})
				case "account/rateLimitResetCredit/consume":
					consumes++
					if tt.rpcError {
						f.push(map[string]any{"id": request["id"], "error": map[string]any{"code": -1, "message": "unknown outcome"}})
					} else {
						f.respond(request["id"], map[string]any{"outcome": tt.outcome})
					}
				case "turn/start":
					turns++
					if turns > 2 {
						t.Fatal("unbounded continuation")
					}
					id := "first"
					if turns == 2 {
						id = "second"
					}
					f.respond(request["id"], map[string]any{"turn": map[string]any{"id": id}})
					f.push(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": "thread-1", "turn": map[string]any{"id": id, "status": "failed", "error": map[string]any{"message": "quota exhausted", "codexErrorInfo": "usageLimitExceeded"}}}})
				default:
					return false
				}
				return true
			}
			err := (&Codex{start: f.start, UseUsageResets: tt.enabled}).Run(context.Background(), &core.Session{Directory: t.TempDir()}, core.Prompt{Text: "hello"}, nil, nil)
			if err == nil {
				t.Fatal("quota failure was hidden")
			}
			if reads != tt.wantReads || consumes != tt.wantConsumes || turns != tt.wantTurns {
				t.Fatalf("reads=%d consumes=%d turns=%d", reads, consumes, turns)
			}
		})
	}
}

package provider

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"julia/internal/core"
)

func TestCodexUsageResetEligibility(t *testing.T) {
	for _, tt := range []struct {
		name                    string
		enabled                 bool
		limits                  string
		outcome                 string
		readError               bool
		consumeError            bool
		wantReads, wantConsumes int
	}{
		{name: "disabled", limits: `{"ordinaryUsageAllowed":false,"rateLimitResetCredits":{"availableCount":1}}`},
		{name: "enabled", enabled: true, limits: `{"ordinaryUsageAllowed":false,"rateLimitResetCredits":{"availableCount":1}}`, outcome: "reset", wantReads: 1, wantConsumes: 1},
		{name: "no credits", enabled: true, limits: `{"ordinaryUsageAllowed":false,"rateLimitResetCredits":{"availableCount":0}}`, wantReads: 1},
		{name: "usage allowed", enabled: true, limits: `{"ordinaryUsageAllowed":true,"rateLimitResetCredits":{"availableCount":1}}`, wantReads: 1},
		{name: "unknown permission", enabled: true, limits: `{"ordinaryUsageAllowed":null,"rateLimitResetCredits":{"availableCount":1}}`, wantReads: 1},
		{name: "old server", enabled: true, limits: `{}`, wantReads: 1},
		{name: "unsupported", enabled: true, readError: true, wantReads: 1},
		{name: "consume error", enabled: true, limits: `{"ordinaryUsageAllowed":false,"rateLimitResetCredits":{"availableCount":1}}`, consumeError: true, wantReads: 1, wantConsumes: 1},
		{name: "refused", enabled: true, limits: `{"ordinaryUsageAllowed":false,"rateLimitResetCredits":{"availableCount":1}}`, outcome: "noCredit", wantReads: 1, wantConsumes: 1},
		{name: "nothing to reset", enabled: true, limits: `{"ordinaryUsageAllowed":false,"rateLimitResetCredits":{"availableCount":1}}`, outcome: "nothingToReset", wantReads: 1, wantConsumes: 1},
		{name: "already redeemed", enabled: true, limits: `{"ordinaryUsageAllowed":false,"rateLimitResetCredits":{"availableCount":1}}`, outcome: "alreadyRedeemed", wantReads: 1, wantConsumes: 1},
		{name: "unknown outcome", enabled: true, limits: `{"ordinaryUsageAllowed":false,"rateLimitResetCredits":{"availableCount":1}}`, outcome: "future", wantReads: 1, wantConsumes: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newCodexFixture(t)
			reads, consumes := 0, 0
			f.rpc = func(method string, request map[string]json.RawMessage) bool {
				switch method {
				case "account/rateLimits/read":
					reads++
					var params map[string]any
					if err := json.Unmarshal(request["params"], &params); err != nil {
						t.Fatal(err)
					}
					if params["supportsLunaReserve"] != false || params["excludeResetCreditDetails"] != true {
						t.Fatalf("unsafe usage read: %s", request["params"])
					}
					if tt.readError {
						f.push(map[string]any{"id": request["id"], "error": map[string]any{"code": -32601, "message": "unsupported"}})
					} else {
						f.respond(request["id"], json.RawMessage(tt.limits))
					}
				case "account/rateLimitResetCredit/consume":
					consumes++
					var params map[string]string
					if err := json.Unmarshal(request["params"], &params); err != nil {
						t.Fatal(err)
					}
					if len(params) != 1 || len(params["idempotencyKey"]) != 36 {
						t.Fatalf("unsafe consume params: %s", request["params"])
					}
					if tt.consumeError {
						f.push(map[string]any{"id": request["id"], "error": map[string]any{"code": -1, "message": "unavailable"}})
					} else {
						f.respond(request["id"], map[string]any{"outcome": tt.outcome})
					}
				default:
					return false
				}
				return true
			}
			f.turn = func(f *codexFixture) { f.finish("completed") }
			backend := &Codex{start: f.start, UseUsageResets: tt.enabled}
			if err := backend.Run(context.Background(), &core.Session{Directory: t.TempDir()}, core.Prompt{Text: "hello"}, nil, nil); err != nil {
				t.Fatal(err)
			}
			if reads != tt.wantReads || consumes != tt.wantConsumes {
				t.Fatalf("reads %d, consumes %d", reads, consumes)
			}
		})
	}
}

func TestCodexUsageResetContinuesWithoutReplayingTools(t *testing.T) {
	f := newCodexFixture(t)
	reads, consumes, turns := 0, 0, 0
	f.rpc = func(method string, request map[string]json.RawMessage) bool {
		switch method {
		case "account/rateLimits/read":
			reads++
			f.respond(request["id"], map[string]any{"ordinaryUsageAllowed": reads == 1, "rateLimitResetCredits": map[string]any{"availableCount": 2}})
		case "account/rateLimitResetCredit/consume":
			consumes++
			f.respond(request["id"], map[string]any{"outcome": "reset"})
		case "turn/start":
			turns++
			id := "turn-1"
			if turns == 2 {
				id = "turn-2"
				if strings.Contains(string(request["params"]), "unique original request") {
					t.Fatal("replayed original request")
				}
				if !strings.Contains(string(request["params"]), "saved progress and tool results") {
					t.Fatal("missing continuation context")
				}
			}
			f.respond(request["id"], map[string]any{"turn": map[string]any{"id": id}})
			f.push(map[string]any{"id": "tool-request", "method": "item/tool/call", "params": map[string]any{"threadId": "thread-1", "turnId": id, "callId": "same-call", "tool": "Read", "arguments": map[string]any{"path": "README.md"}}})
			status := "failed"
			var failure any = map[string]any{"message": "quota", "codexErrorInfo": "usageLimitExceeded"}
			if turns == 2 {
				status, failure = "completed", nil
			}
			f.push(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": "thread-1", "turn": map[string]any{"id": id, "status": status, "error": failure}}})
		default:
			return false
		}
		return true
	}
	executor := &codexTestExecutor{}
	session := &core.Session{Directory: t.TempDir()}
	if err := (&Codex{start: f.start, UseUsageResets: true}).Run(context.Background(), session, core.Prompt{Text: "unique original request"}, executor, nil); err != nil {
		t.Fatal(err)
	}
	if reads != 2 || consumes != 1 || turns != 2 || executor.calls != 1 {
		t.Fatalf("reads=%d consumes=%d turns=%d tool calls=%d", reads, consumes, turns, executor.calls)
	}
	users := 0
	for _, message := range session.Messages {
		if message.Role == "user" {
			users++
		}
	}
	if users != 1 {
		t.Fatalf("user requests=%d", users)
	}
}

func TestCodexUsageResetDoesNotRetryOtherFailures(t *testing.T) {
	for _, code := range []string{"rateLimitExceeded", "sessionBudgetExceeded", "other", ""} {
		t.Run(code, func(t *testing.T) {
			f := newCodexFixture(t)
			reads := 0
			f.rpc = func(method string, request map[string]json.RawMessage) bool {
				if method != "account/rateLimits/read" {
					return false
				}
				reads++
				f.respond(request["id"], map[string]any{"ordinaryUsageAllowed": true})
				return true
			}
			f.turn = func(f *codexFixture) {
				f.push(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": "thread-1", "turn": map[string]any{"id": "turn-1", "status": "failed", "error": map[string]any{"message": "usageLimitExceeded quota exhausted", "codexErrorInfo": code}}}})
			}
			err := (&Codex{start: f.start, UseUsageResets: true}).Run(context.Background(), &core.Session{Directory: t.TempDir()}, core.Prompt{Text: "hello"}, nil, nil)
			if err == nil || reads != 1 {
				t.Fatalf("error=%v reads=%d", err, reads)
			}
		})
	}
}

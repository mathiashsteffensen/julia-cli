package provider

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"strings"
	"testing"
	"time"
)

func TestCodexWeeklyUsageSelectsWeekAndPreservesUnavailable(t *testing.T) {
	for _, tc := range []struct {
		name, data  string
		remaining   float64
		reset       int64
		unavailable bool
	}{
		{name: "legacy secondary", data: `{"rateLimits":{"primary":{"usedPercent":30,"windowDurationMins":300},"secondary":{"usedPercent":74,"windowDurationMins":10080,"resetsAt":1800000000}}}`, remaining: 26, reset: 1800000000},
		{name: "weekly primary zero usage", data: `{"rateLimits":{"primary":{"usedPercent":0,"windowDurationMins":10080}}}`, remaining: 100},
		{name: "codex overrides legacy", data: `{"rateLimits":{"primary":{"usedPercent":0,"windowDurationMins":10080}},"rateLimitsByLimitId":{"codex":{"secondary":{"usedPercent":96,"windowDurationMins":10080}},"other":{"primary":{"usedPercent":1,"windowDurationMins":10080}}}}`, remaining: 4},
		{name: "absent codex falls back", data: `{"rateLimits":{"primary":{"usedPercent":25,"windowDurationMins":10080}},"rateLimitsByLimitId":{"other":{"primary":{"usedPercent":1,"windowDurationMins":10080}}}}`, remaining: 75},
		{name: "null map falls back", data: `{"rateLimits":{"primary":{"usedPercent":25,"windowDurationMins":10080}},"rateLimitsByLimitId":null}`, remaining: 75},
		{name: "negative clamped", data: `{"rateLimits":{"primary":{"usedPercent":-10,"windowDurationMins":10080}}}`, remaining: 100},
		{name: "over limit clamped", data: `{"rateLimits":{"primary":{"usedPercent":150,"windowDurationMins":10080}}}`, remaining: 0},
		{name: "fractional usage", data: `{"rateLimits":{"secondary":{"usedPercent":12.5,"windowDurationMins":10080,"resetsAt":null}}}`, remaining: 87.5},
		{name: "null codex is authoritative", data: `{"rateLimits":{"primary":{"usedPercent":0,"windowDurationMins":10080}},"rateLimitsByLimitId":{"codex":null}}`, unavailable: true},
		{name: "incomplete codex no legacy merge", data: `{"rateLimits":{"primary":{"usedPercent":0,"windowDurationMins":10080}},"rateLimitsByLimitId":{"codex":{"secondary":{"windowDurationMins":10080}}}}`, unavailable: true},
		{name: "missing used", data: `{"rateLimits":{"secondary":{"windowDurationMins":10080}}}`, unavailable: true},
		{name: "null used", data: `{"rateLimits":{"secondary":{"usedPercent":null,"windowDurationMins":10080}}}`, unavailable: true},
		{name: "missing duration", data: `{"rateLimits":{"secondary":{"usedPercent":25}}}`, unavailable: true},
		{name: "null duration", data: `{"rateLimits":{"secondary":{"usedPercent":25,"windowDurationMins":null}}}`, unavailable: true},
		{name: "daily window", data: `{"rateLimits":{"secondary":{"usedPercent":25,"windowDurationMins":1440}}}`, unavailable: true},
		{name: "empty response", data: `{}`, unavailable: true},
		{name: "null windows", data: `{"rateLimits":{"primary":null,"secondary":null}}`, unavailable: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			usage, err := parseCodexWeeklyUsage([]byte(tc.data))
			if err != nil {
				t.Fatal(err)
			}
			if tc.unavailable {
				if usage != nil {
					t.Fatalf("unavailable quota became %+v", usage)
				}
				return
			}
			if usage == nil || usage.RemainingPercent != tc.remaining {
				t.Fatalf("usage=%+v, expected remaining %v", usage, tc.remaining)
			}
			if tc.reset != 0 {
				if usage.ResetsAt.Unix() != tc.reset {
					t.Fatal("reset timestamp changed")
				}
			} else if !usage.ResetsAt.IsZero() {
				t.Fatal("missing reset became a real date")
			}
		})
	}
	for _, data := range []string{`{`, `{"rateLimits":{"primary":{"usedPercent":"unknown","windowDurationMins":10080}}}`, `{"rateLimits":{"primary":{"usedPercent":1e999,"windowDurationMins":10080}}}`} {
		if usage, err := parseCodexWeeklyUsage([]byte(data)); err == nil || usage != nil {
			t.Fatal("invalid response accepted")
		}
	}
}

type usageFixture struct {
	t         *testing.T
	data      json.RawMessage
	requests  []string
	directory string
	closed    bool
	fail      bool
	cancel    context.CancelFunc
}

func (f *usageFixture) start(ctx context.Context, _ string, directory string) (*codexClient, error) {
	f.directory = directory
	if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 30*time.Second {
		f.t.Fatal("missing usage timeout")
	}
	incoming := make(chan codexRead, 8)
	return &codexClient{incoming: incoming, stop: func() { f.closed = true }, input: codexTestWriter(func(data []byte) (int, error) {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params map[string]any  `json:"params"`
		}
		if err := json.Unmarshal(data, &request); err != nil {
			return 0, err
		}
		f.requests = append(f.requests, request.Method)
		var result any = map[string]any{}
		switch request.Method {
		case "initialize":
		case "initialized":
			return len(data), nil
		case "account/rateLimits/read":
			if request.Params["excludeResetCreditDetails"] != true || request.Params["supportsLunaReserve"] != false {
				f.t.Fatal("usage lookup requested extra/reset capability")
			}
			if f.cancel != nil {
				f.cancel()
				return len(data), nil
			}
			if f.fail {
				incoming <- codexRead{Message: codexTestMessage(f.t, map[string]any{"id": request.ID, "error": map[string]any{"code": -32000, "message": "usage unavailable"}})}
				return len(data), nil
			}
			result = f.data
		default:
			f.t.Errorf("usage lookup attempted unexpected %s", request.Method)
			return 0, errors.New("unexpected request")
		}
		incoming <- codexRead{Message: codexTestMessage(f.t, map[string]any{"id": request.ID, "result": result})}
		return len(data), nil
	})}, nil
}

func TestCodexWeeklyUsageOnlyReadsMetadata(t *testing.T) {
	f := &usageFixture{t: t, data: json.RawMessage(`{"rateLimitsByLimitId":{"codex":{"secondary":{"usedPercent":96,"windowDurationMins":10080}}}}`)}
	usage, err := (&Codex{start: f.start}).WeeklyUsage(context.Background())
	if err != nil || usage == nil || usage.RemainingPercent != 4 {
		t.Fatalf("usage=%+v, error=%v", usage, err)
	}
	if strings.Join(f.requests, ",") != "initialize,initialized,account/rateLimits/read" || !f.closed {
		t.Fatal("unexpected lifecycle")
	}
	if _, err := os.Stat(f.directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("temporary workspace not removed")
	}
}

func TestCodexWeeklyUsageErrorAndCancellation(t *testing.T) {
	f := &usageFixture{t: t, fail: true}
	if usage, err := (&Codex{start: f.start}).WeeklyUsage(context.Background()); err == nil || usage != nil || !f.closed {
		t.Fatal("server failure lost")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f = &usageFixture{t: t}
	if _, err := (&Codex{start: f.start}).WeeklyUsage(ctx); !errors.Is(err, context.Canceled) || f.directory != "" {
		t.Fatal("started cancelled lookup")
	}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	f.cancel = cancel
	if _, err := (&Codex{start: f.start}).WeeklyUsage(ctx); !errors.Is(err, context.Canceled) || !f.closed {
		t.Fatal("in-flight cancellation failed")
	}
}

// Reads only account quota metadata; never starts a turn or consumes reset credits.
func TestCodexWeeklyUsageLive(t *testing.T) {
	if os.Getenv("JARVIS_CODEX_USAGE_LIVE") != "1" {
		t.Skip("set JARVIS_CODEX_USAGE_LIVE=1 for installed account usage smoke")
	}
	binary := os.Getenv("JARVIS_CODEX_BINARY")
	if binary == "" {
		binary = "/Applications/ChatGPT.app/Contents/Resources/codex-cli/CodexCLI.app/Contents/MacOS/codex"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	usage, err := (&Codex{Binary: binary}).WeeklyUsage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if usage == nil {
		t.Log("weekly quota unavailable for current account")
		return
	}
	if math.IsNaN(usage.RemainingPercent) || usage.RemainingPercent < 0 || usage.RemainingPercent > 100 {
		t.Fatal("invalid remaining percentage")
	}
	t.Logf("weekly remaining %.1f%%; resets %s", usage.RemainingPercent, usage.ResetsAt.UTC().Format(time.RFC3339))
}

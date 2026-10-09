package provider

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

type catalogFixture struct {
	t          *testing.T
	configured string
	pages      []any
	requests   []map[string]json.RawMessage
	directory  string
	closed     bool
	cancelList context.CancelFunc
	failList   bool
}

func (f *catalogFixture) start(ctx context.Context, _ string, directory string) (*codexClient, error) {
	f.directory = directory
	if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 30*time.Second {
		f.t.Fatal("catalog setup has no bounded timeout")
	}
	incoming := make(chan codexRead, 16)
	page := 0
	return &codexClient{incoming: incoming, stop: func() { f.closed = true }, input: codexTestWriter(func(data []byte) (int, error) {
		var request map[string]json.RawMessage
		if err := json.Unmarshal(data, &request); err != nil {
			return 0, err
		}
		f.requests = append(f.requests, request)
		var method string
		_ = json.Unmarshal(request["method"], &method)
		var result any
		switch method {
		case "initialize":
			result = map[string]any{}
		case "initialized":
			return len(data), nil
		case "config/read":
			result = map[string]any{"config": map[string]any{"model": f.configured}}
		case "model/list":
			if f.cancelList != nil {
				f.cancelList()
				return len(data), nil
			}
			if f.failList {
				incoming <- codexRead{Message: codexTestMessage(f.t, map[string]any{"id": request["id"], "error": map[string]any{"code": -32000, "message": "catalog unavailable"}})}
				return len(data), nil
			}
			if page >= len(f.pages) {
				return 0, errors.New("unexpected additional catalog page")
			}
			result = f.pages[page]
			page++
		default:
			f.t.Errorf("metadata lookup attempted unexpected %s", method)
			return 0, errors.New("unexpected request")
		}
		incoming <- codexRead{Message: codexTestMessage(f.t, map[string]any{"id": request["id"], "result": result})}
		return len(data), nil
	})}, nil
}

func TestCodexModelsReadCatalogAndConfiguredDefault(t *testing.T) {
	for _, configured := range []string{"catalog-b", "actual-bravo", "missing-model", ""} {
		t.Run("configured-"+configured, func(t *testing.T) {
			f := &catalogFixture{t: t, configured: configured, pages: []any{
				map[string]any{"data": []any{
					map[string]any{"id": "catalog-a", "model": "actual-alpha", "displayName": "Alpha", "description": "Fast model", "isDefault": true, "defaultReasoningEffort": "low", "supportedReasoningEfforts": []any{map[string]any{"reasoningEffort": "low"}}},
					map[string]any{"id": "hidden", "model": "hidden-model", "hidden": true},
					map[string]any{"id": "not-a-model", "model": ""},
				}, "nextCursor": "page-two"},
				map[string]any{"data": []any{
					map[string]any{"id": "catalog-b", "model": "actual-bravo", "description": "Deep model", "defaultReasoningEffort": "future-effort", "supportedReasoningEfforts": []any{map[string]any{"reasoningEffort": "medium"}, map[string]any{"reasoningEffort": "future-effort"}, map[string]any{"reasoningEffort": "medium"}}},
					map[string]any{"id": "alias", "model": "actual-alpha", "displayName": "Duplicate"},
				}, "nextCursor": nil},
			}}
			models, err := (&Codex{start: f.start}).Models(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(models) != 2 || models[0].ID != "actual-alpha" || models[0].Name != "Alpha" || models[1].ID != "actual-bravo" || models[1].Name != "actual-bravo" || models[1].Description != "Deep model" {
				t.Fatalf("wrong selectable models: %+v", models)
			}
			if !reflect.DeepEqual(models[1].ReasoningEfforts, []string{"medium", "future-effort"}) || models[1].DefaultReasoning != "future-effort" {
				t.Fatal("advertised reasoning choices were changed")
			}
			configuredMatch := configured == "catalog-b" || configured == "actual-bravo"
			if models[1].IsDefault != configuredMatch || models[0].IsDefault == configuredMatch {
				t.Fatalf("wrong effective default: %+v", models)
			}
			pages := 0
			for _, request := range f.requests {
				if string(request["method"]) != `"model/list"` {
					continue
				}
				var params map[string]any
				_ = json.Unmarshal(request["params"], &params)
				if params["includeHidden"] != false {
					t.Fatal("requested hidden catalog options")
				}
				if pages == 1 && params["cursor"] != "page-two" {
					t.Fatal("pagination cursor missing")
				}
				pages++
			}
			if pages != 2 || !f.closed {
				t.Fatal("catalog process was not completed and closed")
			}
			if _, err := os.Stat(f.directory); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("temporary catalog workspace was not removed")
			}
		})
	}
}

func TestCodexModelsRejectBrokenCatalogAndClose(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		pages      []any
		fail       bool
	}{
		{name: "malformed", pages: []any{"not a catalog"}, want: "decode Codex model catalog"},
		{name: "repeated cursor", pages: []any{map[string]any{"nextCursor": "repeat"}, map[string]any{"nextCursor": "repeat"}}, want: "repeated a pagination cursor"},
		{name: "server error", fail: true, want: "catalog unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &catalogFixture{t: t, pages: tc.pages, failList: tc.fail}
			models, err := (&Codex{start: f.start}).Models(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.want) || models != nil || !f.closed {
				t.Fatalf("catalog failure not surfaced/closed: %v, %v", models, err)
			}
		})
	}
}

func TestCodexModelsHonorCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := &catalogFixture{t: t}
	if _, err := (&Codex{start: f.start}).Models(ctx); !errors.Is(err, context.Canceled) || f.directory != "" {
		t.Fatal("already-cancelled lookup started app-server")
	}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	f.cancelList = cancel
	if _, err := (&Codex{start: f.start}).Models(ctx); !errors.Is(err, context.Canceled) || !f.closed {
		t.Fatal("catalog cancellation was not propagated or process not closed")
	}
}

// This opt-in smoke only reads app-server metadata; it never starts a thread or
// turn and therefore does not consume an inference request.
func TestCodexModelsLive(t *testing.T) {
	if os.Getenv("JARVIS_CODEX_MODELS_LIVE") != "1" {
		t.Skip("set JARVIS_CODEX_MODELS_LIVE=1 for installed Codex catalog smoke")
	}
	binary := os.Getenv("JARVIS_CODEX_BINARY")
	if binary == "" {
		binary = "/Applications/ChatGPT.app/Contents/Resources/codex-cli/CodexCLI.app/Contents/MacOS/codex"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	models, err := (&Codex{Binary: binary}).Models(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) == 0 {
		t.Fatal("installed Codex returned no selectable models")
	}
	ids := make([]string, 0, len(models))
	defaults := 0
	for _, model := range models {
		if strings.TrimSpace(model.ID) == "" {
			t.Fatal("catalog contains an empty model ID")
		}
		ids = append(ids, model.ID)
		if model.IsDefault {
			defaults++
			foundDefault := false
			for _, effort := range model.ReasoningEfforts {
				if effort == "" {
					t.Fatal("default model contains an empty supported effort")
				}
				foundDefault = foundDefault || effort == model.DefaultReasoning
			}
			if !foundDefault || model.DefaultReasoning == "" {
				t.Fatal("default model has no advertised default reasoning choice")
			}
			t.Logf("configured default: %s; default effort: %s; supported: %s", model.ID, model.DefaultReasoning, strings.Join(model.ReasoningEfforts, ", "))
		}
	}
	if defaults != 1 {
		t.Fatalf("expected one effective default, found %d", defaults)
	}
	t.Logf("selectable models: %s", strings.Join(ids, ", "))
}

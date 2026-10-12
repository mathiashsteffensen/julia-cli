package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jarvis/internal/core"
)

func TestConfigRejectsUnknownAndUnsafeModes(t *testing.T) {
	for _, data := range []string{`{"mode":"yolo"}`, `{"provider":"typo"}`, `{"approve_everything":true}`, `{"openrouter_models":["vendor/model"]}`, `{"timeout":"0s"}`, `{} {}`} {
		p := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(p, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(p); err == nil {
			t.Fatalf("accepted %s", data)
		}
	}
}

func TestCompatibleModelConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	data := `{"compatible_models":["anthropic/claude-sonnet", "openai/gpt-model"]}`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil || len(cfg.CompatibleModels) != 2 || cfg.CompatibleModels[1].ID != "openai/gpt-model" {
		t.Fatalf("saved compatible models not loaded: %#v %v", cfg.CompatibleModels, err)
	}
	for _, models := range [][]ModelEntry{{{ID: ""}}, {{ID: " model"}}, {{ID: "bad\nmodel"}}, {{ID: "duplicate"}, {ID: "duplicate"}}} {
		cfg := Default()
		cfg.CompatibleModels = models
		if err := cfg.Validate(); err == nil {
			t.Fatalf("accepted invalid saved models %#v", models)
		}
	}
}

func TestModelAndReasoningConfiguration(t *testing.T) {
	for _, effort := range []string{"", "none", "minimal", "medium", "xhigh", "future-effort_v2"} {
		cfg := Default()
		cfg.Model, cfg.Reasoning = "provider/future-model", effort
		if err := cfg.Validate(); err != nil {
			t.Fatalf("valid model/effort %q: %v", effort, err)
		}
	}
	for _, effort := range []string{" ", "HIGH", "medium\n", "bad effort"} {
		cfg := Default()
		cfg.Reasoning = effort
		if err := cfg.Validate(); err == nil {
			t.Fatalf("accepted invalid reasoning %q", effort)
		}
	}
	for _, provider := range []string{"openai", "anthropic", "google", "compatible"} {
		cfg := Default()
		cfg.Provider, cfg.Reasoning = provider, "medium"
		if err := cfg.Validate(); err == nil {
			t.Fatalf("silently accepted unmapped reasoning for %s", provider)
		}
		cfg.Reasoning = ""
		if err := cfg.Validate(); err != nil {
			t.Fatalf("provider without effort: %v", err)
		}
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"model":"gpt-6-astra","reasoning":"medium"}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil || cfg.Model != "gpt-6-astra" || cfg.Reasoning != "medium" {
		t.Fatalf("settings not loaded: %#v %v", cfg, err)
	}
}

func writeConfig(t *testing.T, data string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestContextTokensDefaultAndOverride(t *testing.T) {
	if got := Default().ContextTokens; got != 256000 {
		t.Fatalf("default context_tokens = %d, want 256000", got)
	}
	if Default().ContextBytes != 0 {
		t.Fatal("deprecated context_bytes must not be part of the defaults")
	}
	if cfg, err := Load(filepath.Join(t.TempDir(), "missing.json")); err != nil || cfg.ContextTokens != core.DefaultContextWindow {
		t.Fatalf("missing config: %d %v", cfg.ContextTokens, err)
	}
	cfg, err := Load(writeConfig(t, `{"context_tokens":400000}`))
	if err != nil || cfg.ContextTokens != 400000 {
		t.Fatalf("context_tokens override not loaded: %d %v", cfg.ContextTokens, err)
	}
}

func TestContextTokensMinimum(t *testing.T) {
	for _, tokens := range []int{MinContextTokens, MinContextTokens + 1, core.DefaultContextWindow} {
		cfg := Default()
		cfg.ContextTokens = tokens
		if err := cfg.Validate(); err != nil {
			t.Fatalf("context_tokens %d rejected: %v", tokens, err)
		}
	}
	for _, tokens := range []int{0, -1, MinContextTokens - 1} {
		cfg := Default()
		cfg.ContextTokens = tokens
		err := cfg.Validate()
		if err == nil || err.Error() != "context_tokens must be at least 32000" {
			t.Fatalf("context_tokens %d: got %v", tokens, err)
		}
	}
	if _, err := Load(writeConfig(t, `{"context_tokens":31999}`)); err == nil {
		t.Fatal("loaded context_tokens below the minimum")
	}
	if _, err := Load(writeConfig(t, `{"context_tokens":0}`)); err == nil {
		t.Fatal("an explicit zero must not silently fall back to the default")
	}
}

func TestLegacyContextBytesLoadsButIsIgnored(t *testing.T) {
	// Configs written by earlier 'config init' runs contain context_bytes.
	// Whatever value they hold, including ones the old validation rejected,
	// they must keep loading and must not influence the token budget.
	for _, data := range []string{`{"context_bytes":160000}`, `{"context_bytes":5}`, `{"context_bytes":0}`, `{"context_bytes":-1}`} {
		cfg, err := Load(writeConfig(t, data))
		if err != nil || cfg.ContextTokens != 256000 {
			t.Fatalf("%s: context_tokens=%d err=%v", data, cfg.ContextTokens, err)
		}
	}
	cfg, err := Load(writeConfig(t, `{"context_bytes":160000,"context_tokens":64000}`))
	if err != nil || cfg.ContextTokens != 64000 {
		t.Fatalf("context_tokens must win over legacy context_bytes: %d %v", cfg.ContextTokens, err)
	}
	// A full legacy file as 'config init' used to write it.
	legacy := `{"provider":"codex","mode":"auto","codex_binary":"codex","max_steps":256,"context_bytes":160000,"timeout":"4h"}`
	cfg, err = Load(writeConfig(t, legacy))
	if err != nil || cfg.ContextTokens != 256000 || cfg.MaxSteps != 256 {
		t.Fatalf("legacy config: %+v %v", cfg, err)
	}
}

func TestInitWritesContextTokensWithoutLegacyBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jarvis", "config.json")
	if err := Init(path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if v, ok := raw["context_tokens"].(float64); !ok || int(v) != core.DefaultContextWindow {
		t.Fatalf("context_tokens not written correctly: %v", raw["context_tokens"])
	}
	if _, ok := raw["context_bytes"]; ok {
		t.Fatalf("deprecated context_bytes must not be written: %s", data)
	}
	if cfg, err := Load(path); err != nil || cfg.ContextTokens != core.DefaultContextWindow {
		t.Fatalf("initialized config does not load: %+v %v", cfg, err)
	}
}

func TestUnknownKeysStillRejectedAlongsideContextKeys(t *testing.T) {
	for _, data := range []string{`{"context_bytes":160000,"context_window":256000}`, `{"context_tokens":256000,"context_byte":1}`} {
		if _, err := Load(writeConfig(t, data)); err == nil {
			t.Fatalf("accepted unknown key in %s", data)
		}
	}
}

func TestBrowserConfiguration(t *testing.T) {
	t.Setenv(BrowserEnv, "")
	cfg, err := Load(writeConfig(t, `{"browser":"/Applications/Chromium.app"}`))
	if err != nil || cfg.Browser != "/Applications/Chromium.app" || cfg.BrowserPath() != "/Applications/Chromium.app" {
		t.Fatalf("browser not loaded: %+v %v", cfg, err)
	}
	t.Setenv(BrowserEnv, "~/chrome/chrome")
	if got := cfg.BrowserPath(); got != "~/chrome/chrome" {
		t.Fatalf("JARVIS_BROWSER does not override the configuration: %q", got)
	}
	if Default().Browser != "" {
		t.Fatal("the default must find a browser, not name one")
	}
	for _, data := range []string{`{"browser":" /usr/bin/chromium"}`, `{"browser":"/usr/bin/chro\nmium"}`, `{"browser_path":"/usr/bin/chromium"}`} {
		if _, err := Load(writeConfig(t, data)); err == nil {
			t.Fatalf("accepted %s", data)
		}
	}
	// A config written before the key existed still loads.
	if cfg, err := Load(writeConfig(t, `{"provider":"codex","mode":"auto","max_steps":256,"timeout":"4h"}`)); err != nil || cfg.Browser != "" {
		t.Fatalf("config without browser: %+v %v", cfg, err)
	}
}

func TestReviewerModelConfiguration(t *testing.T) {
	for _, tc := range []struct{ value, provider, model string }{
		{"", "codex", ""},
		{"codex/gpt-6-luna", "codex", "gpt-6-luna"},
		{"compatible/" + CompatibleDecisionsModel, "compatible", CompatibleDecisionsModel},
	} {
		cfg := Default()
		cfg.ReviewerModel, cfg.BaseURL = tc.value, "https://openrouter.ai/api/v1"
		if err := cfg.Validate(); err != nil {
			t.Fatalf("rejected %q: %v", tc.value, err)
		}
		if provider, model := cfg.Reviewer(); provider != tc.provider || model != tc.model {
			t.Fatalf("%q split into %q %q", tc.value, provider, model)
		}
	}
	for _, value := range []string{"gpt-6-luna", "codex/", "openai/gpt-5", " codex/gpt-6-luna", "codex/bad\nmodel"} {
		cfg := Default()
		cfg.ReviewerModel, cfg.BaseURL = value, "https://openrouter.ai/api/v1"
		if err := cfg.Validate(); err == nil {
			t.Fatalf("accepted reviewer_model %q", value)
		}
	}
	cfg := Default()
	cfg.ReviewerModel = "compatible/" + CompatibleDecisionsModel
	if err := cfg.Validate(); err == nil {
		t.Fatal("accepted a compatible reviewer without base_url")
	}
}

func TestModelsAreIDsOrObjectsWithReasoning(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	data := `{"provider":"compatible","base_url":"https://openrouter.ai/api/v1",
		"model":{"id":"mistral/mistral-large-4-0","reasoning":"high"},
		"compatible_models":["deepseek/deepseek-v4.1-flash",{"id":"mistral/mistral-large-4-0","reasoning":"high"},{"id":"vendor/plain"}]}`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Model != "mistral/mistral-large-4-0" || cfg.ModelReasoning != "high" {
		t.Fatalf("model object not loaded: %q %q", cfg.Model, cfg.ModelReasoning)
	}
	want := []ModelEntry{{ID: "deepseek/deepseek-v4.1-flash"}, {ID: "mistral/mistral-large-4-0", Reasoning: "high"}, {ID: "vendor/plain"}}
	if len(cfg.CompatibleModels) != len(want) {
		t.Fatalf("compatible_models = %#v", cfg.CompatibleModels)
	}
	for i := range want {
		if cfg.CompatibleModels[i] != want[i] {
			t.Fatalf("compatible_models[%d] = %#v, want %#v", i, cfg.CompatibleModels[i], want[i])
		}
	}
	if got := cfg.ReasoningFor("compatible", "deepseek/deepseek-v4.1-flash"); got != "" {
		t.Fatal("a bare model ID got reasoning", got)
	}
	if got := cfg.ReasoningFor("codex", "mistral/mistral-large-4-0"); got != "" {
		t.Fatal("the model's reasoning leaked to another provider", got)
	}

	// A string model stays a string; an object model round-trips as one.
	encoded, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var again Config
	if err := json.Unmarshal(encoded, &again); err != nil || again.Model != cfg.Model || again.ModelReasoning != "high" || len(again.CompatibleModels) != 3 {
		t.Fatalf("round trip lost models: %s %v", encoded, err)
	}
	if !strings.Contains(string(encoded), `"deepseek/deepseek-v4.1-flash"`) {
		t.Fatalf("a bare model ID was not written as a string: %s", encoded)
	}
}

func TestModelObjectsAreChecked(t *testing.T) {
	for _, data := range []string{
		`{"model":{"reasoning":"high"}}`,
		`{"model":{"id":"m","effort":"high"}}`,
		`{"model":42}`,
		`{"compatible_models":[{"id":"m","reasoning":"high"}]}`,
		`{"provider":"compatible","base_url":"https://openrouter.ai/api/v1","compatible_models":[{"id":"m","reasoning":"turbo"}]}`,
		`{"provider":"openai","model":{"id":"m","reasoning":"high"}}`,
	} {
		p := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(p, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(p); err == nil {
			t.Fatalf("accepted %s", data)
		}
	}
}

func TestModelReasoningBeatsRememberedReasoning(t *testing.T) {
	remembered := Preferences{Reasoning: map[string]string{"compatible": "low"}}
	cfg := Default()
	cfg.Provider, cfg.BaseURL = "compatible", "https://openrouter.ai/api/v1"
	cfg.Model, cfg.ModelReasoning = "vendor/a", "high"
	cfg.ApplyStartingReasoning(remembered)
	if cfg.Reasoning != "high" {
		t.Fatal("the model's reasoning did not win", cfg.Reasoning)
	}
	cfg.Model, cfg.ModelReasoning, cfg.Reasoning = "vendor/b", "", ""
	cfg.CompatibleModels = []ModelEntry{{ID: "vendor/b", Reasoning: "max"}}
	cfg.ApplyStartingReasoning(remembered)
	if cfg.Reasoning != "max" {
		t.Fatal("the compatible_models entry's reasoning did not apply", cfg.Reasoning)
	}
	cfg.Model, cfg.Reasoning = "vendor/c", ""
	cfg.ApplyStartingReasoning(remembered)
	if cfg.Reasoning != "low" {
		t.Fatal("a model without reasoning lost the remembered effort", cfg.Reasoning)
	}
}

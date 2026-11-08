package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"julia/internal/core"
)

func TestConfigRejectsUnknownAndUnsafeModes(t *testing.T) {
	for _, data := range []string{`{"mode":"unattended"}`, `{"mode":"YOLO"}`, `{"workspace_instructions":["../AGENTS.md"]}`, `{"workspace_instructions":["docs/AGENTS.md"]}`, `{"workspace_instructions":[""]}`, `{"provider":"typo"}`, `{"approve_everything":true}`, `{"openrouter_models":["vendor/model"]}`, `{"timeout":"0s"}`, `{"disabled_tools":[""]}`, `{"disabled_tools":["Web Search"]}`, `{"system_prompt":"short"}`, `{} {}`} {
		p := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(p, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(p); err == nil {
			t.Fatalf("accepted %s", data)
		}
	}
}

func TestYoloModeAndWorkspaceInstructionsLoad(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(`{"mode":"yolo","workspace_instructions":["AGENTS.md","CLAUDE.md"],"disabled_tools":["WebSearch","Subagent"],"system_prompt":"compact"}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Mode != "yolo" || len(cfg.WorkspaceInstructions) != 2 || cfg.WorkspaceInstructions[1] != "CLAUDE.md" || !slices.Equal(cfg.DisabledTools, []string{"WebSearch", "Subagent"}) || cfg.SystemPrompt != "compact" {
		t.Fatalf("loaded %+v", cfg)
	}
	if Default().Mode != "auto" || Default().WorkspaceInstructions != nil {
		t.Fatal("yolo or repository instructions must be opt-in")
	}
}

// openRouterProvider is the "providers" entry tests name openrouter.
const openRouterProvider = `"providers":[{"id":"openrouter","base_url":"https://openrouter.ai/api/v1","api_key_env":"OPENROUTER_API_KEY"}]`

// openRouter is openRouterProvider as a ProviderConfig.
var openRouter = ProviderConfig{ID: "openrouter", BaseURL: "https://openrouter.ai/api/v1", APIKeyEnv: "OPENROUTER_API_KEY"}

func TestEnabledModelConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	data := `{"enabled_models":["anthropic/claude-sonnet", "openrouter/openai/gpt-model", "codex/gpt-6-astra"],` + openRouterProvider + `}`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil || len(cfg.EnabledModels) != 3 || cfg.EnabledModels[1].ID != "openrouter/openai/gpt-model" {
		t.Fatalf("enabled models not loaded: %#v %v", cfg.EnabledModels, err)
	}
	for _, models := range [][]ModelEntry{{{ID: ""}}, {{ID: "codex"}}, {{ID: "vendor/model"}}, {{ID: "openai/ model"}}, {{ID: "openai/bad\nmodel"}}, {{ID: "openai/m"}, {ID: "openai/m"}}, {{ID: "openrouter/" + CompatibleDecisionsModel}}, {{ID: "compatible/vendor/model"}}, {{ID: "local/vendor/model"}}} {
		cfg := Default()
		cfg.EnabledModels, cfg.Providers = models, []ProviderConfig{openRouter}
		if err := cfg.Validate(); err == nil {
			t.Fatalf("accepted invalid enabled models %#v", models)
		}
	}
}

func TestOldModelKeysAreRejected(t *testing.T) {
	for _, data := range []string{`{"provider":"codex"}`, `{"compatible_models":["vendor/m"]}`, `{"base_url":"https://openrouter.ai/api/v1"}`, `{"model":"gpt-6-astra"}`, `{"compatible_base_url":"https://openrouter.ai/api/v1"}`, `{"api_key_env":"OPENROUTER_API_KEY"}`} {
		if _, err := Load(writeConfig(t, data)); err == nil {
			t.Fatalf("accepted %s", data)
		}
	}
}

func TestModelAndReasoningConfiguration(t *testing.T) {
	for _, effort := range []string{"", "none", "minimal", "medium", "xhigh", "future-effort_v2"} {
		cfg := Default()
		cfg.Model, cfg.Reasoning = "future-model", effort
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
	for _, provider := range []string{"openai", "anthropic", "google"} {
		cfg := Default()
		cfg.Provider, cfg.Model, cfg.Reasoning = provider, "vendor-model", "medium"
		if err := cfg.Validate(); err == nil {
			t.Fatalf("silently accepted unmapped reasoning for %s", provider)
		}
		cfg.Reasoning = ""
		if err := cfg.Validate(); err != nil {
			t.Fatalf("provider without effort: %v", err)
		}
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"model":"codex/gpt-6-astra","reasoning":"medium"}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil || cfg.Provider != "codex" || cfg.Model != "gpt-6-astra" || cfg.Reasoning != "medium" {
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
	legacy := `{"model":"codex","mode":"auto","codex_binary":"codex","max_steps":256,"context_bytes":160000,"timeout":"4h"}`
	cfg, err = Load(writeConfig(t, legacy))
	if err != nil || cfg.ContextTokens != 256000 || cfg.MaxSteps != 256 {
		t.Fatalf("legacy config: %+v %v", cfg, err)
	}
}

func TestInitWritesContextTokensWithoutLegacyBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "julia", "config.json")
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
		t.Fatalf("JULIA_BROWSER does not override the configuration: %q", got)
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
	if cfg, err := Load(writeConfig(t, `{"model":"codex","mode":"auto","max_steps":256,"timeout":"4h"}`)); err != nil || cfg.Browser != "" {
		t.Fatalf("config without browser: %+v %v", cfg, err)
	}
}

func TestBrowserAuthConfiguration(t *testing.T) {
	cfg, err := Load(writeConfig(t, `{"browser_auth": {
  "https://monitoring.example.com": {
    "Authorization": "Bearer $(gcloud secrets versions access latest --secret=grafana-token)",
    "User-Agent": "$USER"
  },
  "http://localhost:3000": {"X-Grafana-Org-Id": "1"}
}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.BrowserAuth["https://monitoring.example.com"]["Authorization"]; got != "Bearer $(gcloud secrets versions access latest --secret=grafana-token)" {
		t.Fatalf("value not kept as written: %q", got)
	}

	for data, want := range map[string]string{
		`{"browser_auth": {"https://Monitoring.example.com/": {"A": "b"}}}`:       `is written "https://monitoring.example.com"`,
		`{"browser_auth": {"https://monitoring.example.com:443": {"A": "b"}}}`:    `is written "https://monitoring.example.com"`,
		`{"browser_auth": {"https://monitoring.example.com/d/x": {"A": "b"}}}`:    "is not an origin",
		`{"browser_auth": {"monitoring.example.com": {"A": "b"}}}`:                "is not an origin",
		`{"browser_auth": {"ftp://monitoring.example.com": {"A": "b"}}}`:          "is not an origin",
		`{"browser_auth": {"https://u:p@monitoring.example.com": {"A": "b"}}}`:    "is not an origin",
		`{"browser_auth": {"https://monitoring.example.com": {}}}`:                "needs between 1 and 16 headers",
		`{"browser_auth": {"https://monitoring.example.com": {"Bad Name": "b"}}}`: "is not a header name",
		`{"browser_auth": {"https://monitoring.example.com": {"Bearer:": "b"}}}`:  "is not a header name",
		`{"browser_auth": {"https://monitoring.example.com": {"A": ""}}}`:         "1 to 4096 characters on one line",
		`{"browser_auth": {"https://monitoring.example.com": {"A": "b\nc"}}}`:     "1 to 4096 characters on one line",
	} {
		if _, err := Load(writeConfig(t, data)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", data, err, want)
		}
	}
}

func TestReviewerModelConfiguration(t *testing.T) {
	for _, tc := range []struct{ value, provider, model string }{
		{"", "codex", ""},
		{"codex/gpt-6-luna", "codex", "gpt-6-luna"},
		{"openrouter/" + CompatibleDecisionsModel, "openrouter", CompatibleDecisionsModel},
	} {
		cfg := Default()
		cfg.ReviewerModel, cfg.Providers = tc.value, []ProviderConfig{openRouter}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("rejected %q: %v", tc.value, err)
		}
		if provider, model := cfg.Reviewer(); provider != tc.provider || model != tc.model {
			t.Fatalf("%q split into %q %q", tc.value, provider, model)
		}
	}
	for _, value := range []string{"gpt-6-luna", "codex/", "openai/gpt-5", " codex/gpt-6-luna", "codex/bad\nmodel", "openrouter/", "compatible/" + CompatibleDecisionsModel} {
		cfg := Default()
		cfg.ReviewerModel, cfg.Providers = value, []ProviderConfig{openRouter}
		if err := cfg.Validate(); err == nil {
			t.Fatalf("accepted reviewer_model %q", value)
		}
	}
	cfg := Default()
	cfg.ReviewerModel = "openrouter/" + CompatibleDecisionsModel
	if err := cfg.Validate(); err == nil {
		t.Fatal("accepted a reviewer on a provider that is not configured")
	}
}

func TestModelsAreIDsOrObjectsWithReasoning(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	data := `{` + openRouterProvider + `,
		"model":{"id":"openrouter/mistral/mistral-large-4-0","reasoning":"high"},
		"enabled_models":["openrouter/deepseek/deepseek-v4.1-flash",{"id":"openrouter/mistral/mistral-large-4-0","reasoning":"high"},{"id":"codex/gpt-6-astra","reasoning":"xhigh"}]}`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "openrouter" || cfg.Model != "mistral/mistral-large-4-0" || cfg.ModelReasoning != "high" {
		t.Fatalf("model object not loaded: %q %q %q", cfg.Provider, cfg.Model, cfg.ModelReasoning)
	}
	want := []ModelEntry{{ID: "openrouter/deepseek/deepseek-v4.1-flash"}, {ID: "openrouter/mistral/mistral-large-4-0", Reasoning: "high"}, {ID: "codex/gpt-6-astra", Reasoning: "xhigh"}}
	if len(cfg.EnabledModels) != len(want) {
		t.Fatalf("enabled_models = %#v", cfg.EnabledModels)
	}
	for i := range want {
		if cfg.EnabledModels[i] != want[i] {
			t.Fatalf("enabled_models[%d] = %#v, want %#v", i, cfg.EnabledModels[i], want[i])
		}
	}
	if got := cfg.ReasoningFor("openrouter", "deepseek/deepseek-v4.1-flash"); got != "" {
		t.Fatal("a bare model ID got reasoning", got)
	}
	if got := cfg.ReasoningFor("codex", "gpt-6-astra"); got != "xhigh" {
		t.Fatal("an enabled Codex model lost its reasoning", got)
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
	if err := json.Unmarshal(encoded, &again); err != nil || again.Provider != "openrouter" || again.Model != cfg.Model || again.ModelReasoning != "high" || len(again.EnabledModels) != 3 {
		t.Fatalf("round trip lost models: %s %v", encoded, err)
	}
	if !strings.Contains(string(encoded), `"openrouter/deepseek/deepseek-v4.1-flash"`) {
		t.Fatalf("a bare model ID was not written as a string: %s", encoded)
	}
}

func TestModelObjectsAreChecked(t *testing.T) {
	for _, data := range []string{
		`{"model":{"reasoning":"high"}}`,
		`{"model":{"id":"codex/m","effort":"high"}}`,
		`{"model":42}`,
		`{"model":"openai"}`,
		`{"enabled_models":[{"id":"openrouter/m","reasoning":"high"}]}`,
		`{` + openRouterProvider + `,"enabled_models":[{"id":"openrouter/m","reasoning":"turbo"}]}`,
		`{"model":{"id":"openai/m","reasoning":"high"}}`,
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
	remembered := Preferences{Reasoning: map[string]string{"openrouter": "low"}}
	cfg := Default()
	cfg.Provider, cfg.Providers = "openrouter", []ProviderConfig{openRouter}
	cfg.Model, cfg.ModelReasoning = "vendor/a", "high"
	cfg.ApplyStartingReasoning(remembered)
	if cfg.Reasoning != "high" {
		t.Fatal("the model's reasoning did not win", cfg.Reasoning)
	}
	cfg.Model, cfg.ModelReasoning, cfg.Reasoning = "vendor/b", "", ""
	cfg.EnabledModels = []ModelEntry{{ID: "openrouter/vendor/b", Reasoning: "max"}}
	cfg.ApplyStartingReasoning(remembered)
	if cfg.Reasoning != "max" {
		t.Fatal("the enabled_models entry's reasoning did not apply", cfg.Reasoning)
	}
	cfg.Model, cfg.Reasoning = "vendor/c", ""
	cfg.ApplyStartingReasoning(remembered)
	if cfg.Reasoning != "low" {
		t.Fatal("a model without reasoning lost the remembered effort", cfg.Reasoning)
	}
}

func TestSubagentModelConfiguration(t *testing.T) {
	cfg, err := Load(writeConfig(t, `{
  "model": "codex/gpt-6-astra",
  "providers": [{"id": "openrouter", "base_url": "https://openrouter.ai/api/v1", "api_key_env": "OPENROUTER_API_KEY"}],
  "subagents": {
    "reviewer": "openrouter/mistralai/mistral-large-4-0",
    "scout": {"id": "openrouter/minimax/minimax-m3", "reasoning": "low"},
    "oracle": "codex/gpt-6-astra",
    "my-role": "codex"
  },
  "reviewer_model": "openrouter/typesafe/jev-1.13"
}`))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]ModelEntry{
		"reviewer": {ID: "openrouter/mistralai/mistral-large-4-0"},
		"scout":    {ID: "openrouter/minimax/minimax-m3", Reasoning: "low"},
		"oracle":   {ID: "codex/gpt-6-astra"},
		"my-role":  {ID: "codex"},
	}
	if len(cfg.Subagents) != len(want) {
		t.Fatalf("subagents %v, want %v", cfg.Subagents, want)
	}
	for role, entry := range want {
		if cfg.Subagents[role] != entry {
			t.Fatalf("subagents.%s = %+v, want %+v", role, cfg.Subagents[role], entry)
		}
	}
	data, err := json.Marshal(cfg)
	if err != nil || !strings.Contains(string(data), `"subagents":{"my-role":"codex","oracle":"codex/gpt-6-astra","reviewer":"openrouter/mistralai/mistral-large-4-0","scout":{"id":"openrouter/minimax/minimax-m3","reasoning":"low"}}`) {
		t.Fatalf("subagents do not round-trip: %s %v", data, err)
	}
	for _, tc := range []struct{ subagents, want string }{
		{`{"scout": "minimax/m3"}`, `subagents.scout "minimax/m3" must be provider/<model-id>`},
		{`{"scout": "openai"}`, `subagents.scout "openai" needs a model ID`},
		{`{"scout": "openrouter/` + CompatibleDecisionsModel + `"}`, "decisions-only"},
		{`{"scout": {"id": "openai/m", "reasoning": "high"}}`, "reasoning is configurable for Codex, Claude Code and your providers"},
		{`{"scout": {"id": "openrouter/m", "reasoning": "extreme"}}`, "reasoning for a named provider must be one of"},
		{`{"scout": {"id": "codex/m", "effort": "high"}}`, `unknown field "effort"`},
		{`{"Scout": "codex/m"}`, `subagents "Scout" is not a role name, which is at most 32 lowercase letters, digits or hyphens, starting with a letter`},
		{`{"2nd": "codex/m"}`, `subagents "2nd" is not a role name`},
		{`{"": "codex/m"}`, `subagents "" is not a role name`},
		{`{"scout": ""}`, `subagents.scout "" must be provider/<model-id>`},
	} {
		_, err := Load(writeConfig(t, `{`+openRouterProvider+`,"subagents":`+tc.subagents+`}`))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("subagents %s: %v, want %q", tc.subagents, err, tc.want)
		}
	}
	// A role on a provider that is not configured names no provider.
	if _, err := Load(writeConfig(t, `{"subagents":{"scout":"openrouter/vendor/m"}}`)); err == nil || !strings.Contains(err.Error(), `subagents.scout "openrouter/vendor/m" must be provider/<model-id>`) {
		t.Fatalf("role on an unconfigured provider: %v", err)
	}
}

func TestMemoryConfiguration(t *testing.T) {
	if Default().Memory.Reflection != "on" || !Default().ReflectionEnabled() || Default().Memory.Model != nil || Default().Memory.IndexBytes != 0 {
		t.Fatalf("defaults %+v", Default().Memory)
	}
	// A config written before the key existed, and one that names the key
	// without saying anything, both reflect.
	for _, data := range []string{`{"mode":"auto"}`, `{"memory":{}}`, `{"memory":{"reflection":""}}`} {
		cfg, err := Load(writeConfig(t, data))
		if err != nil || !cfg.ReflectionEnabled() {
			t.Fatalf("%s: %+v %v", data, cfg.Memory, err)
		}
	}
	cfg, err := Load(writeConfig(t, `{`+openRouterProvider+`,"memory":{"reflection":"off","model":{"id":"openrouter/minimax/minimax-m3","reasoning":"low"},"index_bytes":8192}}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ReflectionEnabled() || cfg.Memory.Model == nil || *cfg.Memory.Model != (ModelEntry{ID: "openrouter/minimax/minimax-m3", Reasoning: "low"}) || cfg.Memory.IndexBytes != 8192 {
		t.Fatalf("memory not loaded: %+v", cfg.Memory)
	}
	data, err := json.Marshal(cfg)
	if err != nil || !strings.Contains(string(data), `"memory":{"reflection":"off","model":{"id":"openrouter/minimax/minimax-m3","reasoning":"low"},"index_bytes":8192}`) {
		t.Fatalf("memory does not round-trip: %s %v", data, err)
	}
	if cfg, err := Load(writeConfig(t, `{"memory":{"model":"codex"}}`)); err != nil || cfg.Memory.Model.ID != "codex" {
		t.Fatalf("bare codex reflection model: %+v %v", cfg.Memory, err)
	}
	for _, tc := range []struct{ memory, want string }{
		{`{"reflection":"yes"}`, `memory.reflection must be on or off, not "yes"`},
		{`{"reflection":"On"}`, `memory.reflection must be on or off`},
		{`{"model":"minimax/m3"}`, `memory.model "minimax/m3" must be provider/<model-id>`},
		{`{"model":"openai"}`, `memory.model "openai" needs a model ID`},
		{`{"model":"openrouter/` + CompatibleDecisionsModel + `"}`, "decisions-only"},
		{`{"model":{"id":"openai/m","reasoning":"high"}}`, "reasoning is configurable for Codex, Claude Code and your providers"},
		{`{"model":{"id":"openrouter/m","reasoning":"extreme"}}`, "reasoning for a named provider must be one of"},
		{`{"model":{"id":"codex/m","effort":"high"}}`, `unknown field "effort"`},
		{`{"model":""}`, `memory.model "" must be provider/<model-id>`},
		{`{"index_bytes":4095}`, "memory.index_bytes must be between 4096 and 65536"},
		{`{"index_bytes":-1}`, "memory.index_bytes must be between 4096 and 65536"},
		{`{"index_bytes":65537}`, "memory.index_bytes must be between 4096 and 65536"},
		{`{"reflect":"on"}`, `unknown field "reflect"`},
		{`{"enabled":true}`, `unknown field "enabled"`},
	} {
		_, err := Load(writeConfig(t, `{`+openRouterProvider+`,"memory":`+tc.memory+`}`))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("memory %s: %v, want %q", tc.memory, err, tc.want)
		}
	}
	if _, err := Load(writeConfig(t, `{"memory":{"model":"openrouter/vendor/m"}}`)); err == nil || !strings.Contains(err.Error(), `memory.model "openrouter/vendor/m" must be provider/<model-id>`) {
		t.Fatalf("reflection model on an unconfigured provider: %v", err)
	}
	for _, bytes := range []int{0, MinMemoryIndexBytes, 16384, MaxMemoryIndexBytes} {
		cfg := Default()
		cfg.Memory.IndexBytes = bytes
		if err := cfg.Validate(); err != nil {
			t.Fatalf("index_bytes %d rejected: %v", bytes, err)
		}
	}
}

func TestInitWritesReflectionOn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "julia", "config.json")
	if err := Init(path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if string(raw["memory"]) != `{"reflection":"on"}` && !strings.Contains(string(raw["memory"]), `"reflection": "on"`) {
		t.Fatalf("memory not written for the user to see: %s", raw["memory"])
	}
	if cfg, err := Load(path); err != nil || !cfg.ReflectionEnabled() {
		t.Fatalf("initialized config does not load: %+v %v", cfg.Memory, err)
	}
}

func TestNamedProviders(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "")
	cfg, err := Load(writeConfig(t, `{
  "model": "openrouter/vendor/model",
  "providers": [
    {"id": "openrouter", "base_url": "https://openrouter.ai/api/v1", "api_key_env": "OPENROUTER_API_KEY"},
    {"id": "local", "base_url": "http://localhost:1234/v1"}
  ],
  "enabled_models": [{"id": "openrouter/vendor/model", "reasoning": "high"}, "local/qwen/coder"],
  "reviewer_model": "local/`+CompatibleDecisionsModel+`"
}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "openrouter" || cfg.Model != "vendor/model" || len(cfg.Providers) != 2 || cfg.Providers[0] != openRouter {
		t.Fatalf("providers not loaded: %+v", cfg)
	}
	if want := []string{"codex", "claude", "openai", "anthropic", "google", "openrouter", "local"}; !slices.Equal(cfg.ProviderIDs(), want) {
		t.Fatalf("provider ids %v, want %v", cfg.ProviderIDs(), want)
	}
	if !cfg.IsNamed("local") || cfg.IsNamed("codex") || cfg.IsNamed("compatible") {
		t.Fatal("IsNamed does not match the configured providers")
	}
	if p, ok := cfg.Endpoint("local"); !ok || p.BaseURL != "http://localhost:1234/v1" || p.APIKeyEnv != "" {
		t.Fatalf("local endpoint %+v %v", p, ok)
	}
	// Each key belongs to one provider; a named provider without one is keyless.
	for provider, want := range map[string]string{"openrouter": "OPENROUTER_API_KEY", "local": "", "openai": "OPENAI_API_KEY", "anthropic": "ANTHROPIC_API_KEY"} {
		if got := cfg.KeyEnv(provider); got != want {
			t.Errorf("KeyEnv(%s) = %q, want %q", provider, got, want)
		}
	}
	// Codex and every named provider take a reasoning effort.
	for provider, want := range map[string]bool{"codex": true, "openrouter": true, "local": true, "openai": false, "missing": false} {
		if got := cfg.SupportsReasoning(provider); got != want {
			t.Errorf("SupportsReasoning(%s) = %v, want %v", provider, got, want)
		}
	}
	if cfg.ReasoningFor("openrouter", "vendor/model") != "high" {
		t.Fatal("the named provider's model lost its reasoning")
	}
	// A named provider off OpenRouter takes an effort too, from the same set.
	if _, err := Load(writeConfig(t, `{"providers":[{"id":"local","base_url":"http://localhost:1234/v1"}],"enabled_models":[{"id":"local/m","reasoning":"high"}]}`)); err != nil {
		t.Fatalf("effort on a provider that is not OpenRouter: %v", err)
	}
	if _, err := Load(writeConfig(t, `{"providers":[{"id":"local","base_url":"http://localhost:1234/v1"}],"enabled_models":[{"id":"local/m","reasoning":"turbo"}]}`)); err == nil || !strings.Contains(err.Error(), "reasoning for a named provider must be one of") {
		t.Fatalf("unknown effort on a named provider: %v", err)
	}
	// The decisions-only model reviews on any named provider but cannot chat.
	if _, err := Load(writeConfig(t, `{"providers":[{"id":"local","base_url":"http://localhost:1234/v1"}],"model":"local/`+CompatibleDecisionsModel+`"}`)); err == nil || !strings.Contains(err.Error(), "decisions-only") {
		t.Fatalf("decisions-only chat model: %v", err)
	}

	for _, tc := range []struct{ providers, want string }{
		{`[{"id":"","base_url":"https://a.example/v1"}]`, `providers id "" must be 1 to 64 lowercase letters`},
		{`[{"id":"OpenRouter","base_url":"https://a.example/v1"}]`, `providers id "OpenRouter" must be`},
		{`[{"id":"1router","base_url":"https://a.example/v1"}]`, `providers id "1router" must be`},
		{`[{"id":"open/router","base_url":"https://a.example/v1"}]`, `providers id "open/router" must be`},
		{`[{"id":"` + strings.Repeat("a", 65) + `","base_url":"https://a.example/v1"}]`, `must be 1 to 64 lowercase letters`},
		{`[{"id":"codex","base_url":"https://a.example/v1"}]`, `providers id "codex" is a built-in provider`},
		{`[{"id":"openai","base_url":"https://a.example/v1"}]`, `providers id "openai" is a built-in provider`},
		{`[{"id":"a","base_url":"https://a.example/v1"},{"id":"a","base_url":"https://b.example/v1"}]`, `providers contains "a" twice`},
		{`[{"id":"a"}]`, `providers "a" needs a base_url`},
		{`[{"id":"a","base_url":"ftp://a.example/v1"}]`, `providers "a" needs a base_url`},
		{`[{"id":"a","base_url":"https:///v1"}]`, `providers "a" needs a base_url`},
		{`[{"id":"a","base_url":"localhost:1234/v1"}]`, `providers "a" needs a base_url`},
		{`[{"id":"a","base_url":" https://a.example/v1"}]`, `providers "a" needs a base_url`},
		{`[{"id":"a","base_url":"https://a.example/v1","api_key_env":"MY-KEY"}]`, `providers "a" api_key_env "MY-KEY" is not an environment variable name`},
		{`[{"id":"a","base_url":"https://a.example/v1","api_key_env":"1KEY"}]`, `api_key_env "1KEY" is not an environment variable name`},
		{`[{"id":"a","base_url":"https://a.example/v1","key":"secret"}]`, `unknown field "key"`},
	} {
		_, err := Load(writeConfig(t, `{"providers":`+tc.providers+`}`))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("providers %s: %v, want %q", tc.providers, err, tc.want)
		}
	}
	// "compatible" is not a provider of its own any more.
	if _, err := Load(writeConfig(t, `{"model":"compatible/vendor/model"}`)); err == nil || !strings.Contains(err.Error(), "must be provider/<model-id>") {
		t.Fatalf("accepted the compatible provider: %v", err)
	}
}

func TestModelContextWindow(t *testing.T) {
	data := `{` + openRouterProvider + `,
		"context_tokens": 200000,
		"model":{"id":"openrouter/vendor/large","context_window":1000000},
		"enabled_models":["openrouter/vendor/plain",{"id":"openrouter/vendor/large","reasoning":"high","context_window":400000},{"id":"codex/gpt-6-astra","context_window":272000}],
		"subagents":{"scout":{"id":"openrouter/vendor/plain","context_window":128000}},
		"memory":{"model":{"id":"codex/gpt-6-astra","context_window":64000}}}`
	cfg, err := Load(writeConfig(t, data))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "openrouter" || cfg.Model != "vendor/large" || cfg.ModelContextWindow != 1_000_000 {
		t.Fatalf("model object not loaded: %q %q %d", cfg.Provider, cfg.Model, cfg.ModelContextWindow)
	}
	want := []ModelEntry{{ID: "openrouter/vendor/plain"}, {ID: "openrouter/vendor/large", Reasoning: "high", ContextWindow: 400_000}, {ID: "codex/gpt-6-astra", ContextWindow: 272_000}}
	for i := range want {
		if cfg.EnabledModels[i] != want[i] {
			t.Fatalf("enabled_models[%d] = %#v, want %#v", i, cfg.EnabledModels[i], want[i])
		}
	}
	if cfg.Subagents["scout"].ContextWindow != 128_000 || cfg.Memory.Model.ContextWindow != 64_000 {
		t.Fatalf("subagent or memory model lost its window: %#v %#v", cfg.Subagents, cfg.Memory.Model)
	}
	for _, tc := range []struct {
		provider, model string
		want            int
	}{
		{"openrouter", "vendor/large", 1_000_000}, // the model entry beats its enabled_models entry
		{"openrouter", "vendor/plain", 200_000},   // an entry without one assumes context_tokens
		{"codex", "gpt-6-astra", 272_000},
		{"openrouter", "gpt-6-astra", 200_000}, // a window never leaks to another provider
		{"anthropic", "unknown", 200_000},
	} {
		if got := cfg.ContextWindowFor(tc.provider, tc.model); got != tc.want {
			t.Errorf("ContextWindowFor(%s, %s) = %d, want %d", tc.provider, tc.model, got, tc.want)
		}
	}
	if got := Default().ContextWindowFor("codex", ""); got != core.DefaultContextWindow {
		t.Fatalf("the default window changed: %d", got)
	}

	// Windows round-trip, and an entry with only a window is written as an object.
	encoded, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var again Config
	if err := json.Unmarshal(encoded, &again); err != nil || again.ModelContextWindow != 1_000_000 || again.EnabledModels[2].ContextWindow != 272_000 || again.EnabledModels[0] != want[0] {
		t.Fatalf("round trip lost windows: %s %v", encoded, err)
	}
	if !strings.Contains(string(encoded), `{"id":"codex/gpt-6-astra","context_window":272000}`) || !strings.Contains(string(encoded), `"openrouter/vendor/plain"`) {
		t.Fatalf("windows not written as configured: %s", encoded)
	}
}

func TestModelContextWindowMinimum(t *testing.T) {
	for _, data := range []string{
		`{"model":{"id":"codex/m","context_window":31999}}`,
		`{"model":{"id":"codex/m","context_window":-1}}`,
		`{"enabled_models":[{"id":"codex/m","context_window":1000}]}`,
		`{"subagents":{"scout":{"id":"codex/m","context_window":1000}}}`,
		`{"memory":{"model":{"id":"codex/m","context_window":1000}}}`,
		`{"model":{"id":"codex/m","context_window":"256k"}}`,
		`{"model":{"id":"codex/m","context_tokens":256000}}`,
	} {
		if _, err := Load(writeConfig(t, data)); err == nil {
			t.Errorf("accepted %s", data)
		}
	}
	for _, data := range []string{
		`{"model":{"id":"codex/m","context_window":32000}}`,
		`{"model":{"id":"codex/m","context_window":0}}`,
		`{"enabled_models":[{"id":"codex/m","context_window":2000000}]}`,
	} {
		if _, err := Load(writeConfig(t, data)); err != nil {
			t.Errorf("rejected %s: %v", data, err)
		}
	}
}

func TestClaudeProviderConfiguration(t *testing.T) {
	for _, data := range []string{`{"model":"claude"}`, `{"model":"claude/sonnet"}`, `{"model":{"id":"claude/opus","reasoning":"xhigh"}}`, `{"model":"codex","reviewer_model":"claude/sonnet"}`, `{"model":"claude/opus","reviewer_model":"codex/gpt-6-luna"}`, `{"model":"openai/m","subagents":{"scout":{"id":"claude/haiku","reasoning":"low"}}}`, `{"model":"codex","memory":{"model":"claude/sonnet"}}`} {
		cfg, err := Load(writeConfig(t, data))
		if err != nil {
			t.Fatalf("%s: %v", data, err)
		}
		if cfg.ClaudeBinary != "claude" {
			t.Fatalf("%s: claude_binary %q", data, cfg.ClaudeBinary)
		}
	}
	cfg, err := Load(writeConfig(t, `{"model":{"id":"claude/opus","reasoning":"high"},"claude_binary":"/opt/claude/bin/claude","reviewer_model":"claude/sonnet"}`))
	if err != nil || cfg.Provider != "claude" || cfg.Model != "opus" || cfg.ModelReasoning != "high" || cfg.ClaudeBinary != "/opt/claude/bin/claude" {
		t.Fatalf("claude config: %+v %v", cfg, err)
	}
	if reviewer, model := cfg.Reviewer(); reviewer != "claude" || model != "sonnet" {
		t.Fatal("claude reviewer", reviewer, model)
	}
	if !cfg.SupportsReasoning("claude") || !IsCLIProvider("claude") || IsCLIProvider("anthropic") || CLIProviderName("claude") != "Claude Code" || cfg.KeyEnv("claude") != "" {
		t.Fatal("claude is a CLI provider without a key")
	}
	for data, want := range map[string]string{
		`{"model":{"id":"claude/opus","reasoning":"turbo"}}`: "reasoning for Claude Code must be one of low, medium, high, xhigh, or max",
		`{"model":{"id":"claude/opus","reasoning":"none"}}`:  "reasoning for Claude Code must be one of",
		`{"model":"codex","reviewer_model":"claude"}`:        "reviewer_model must be provider/<model-id>",
		`{"model":"claude/ opus"}`:                           "surrounding whitespace",
		`{"enabled_models":["claude"]}`:                      "needs a model ID",
	} {
		if _, err := Load(writeConfig(t, data)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want %q", data, err, want)
		}
	}
}

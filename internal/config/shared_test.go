package config

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func writeLayer(t *testing.T, dir, name, contents string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSharedMergeMixedFormats(t *testing.T) {
	dir := t.TempDir()
	writeLayer(t, dir, "config.shared.toml", `
model = { id = "codex/shared", reasoning = "high", context_window = 64000 }
enabled_models = ["codex/old"]
workspace_instructions = ["AGENTS.md"]
context_bytes = 42
[browser_auth."https://example.com"]
Authorization = "shared"
X-Shared = "kept"
[subagents]
scout = { id = "codex/scout", reasoning = "high" }
[memory]
reflection = "off"
index_bytes = 8192
`)
	writeLayer(t, dir, "config.work.json", `{
		"model":{"reasoning":"","context_window":0},
		"enabled_models":[],"workspace_instructions":[],"context_bytes":0,
		"browser_auth":{"https://example.com":{"Authorization":"profile"}},
		"subagents":{"scout":{"reasoning":"low"}},
		"memory":{"index_bytes":0}
	}`)
	cfg, err := LoadProfile(dir, "work")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Model != "shared" || cfg.ModelReasoning != "" || cfg.ModelContextWindow != 0 ||
		len(cfg.EnabledModels) != 0 || len(cfg.WorkspaceInstructions) != 0 || cfg.ContextBytes != 0 ||
		cfg.Memory.Reflection != "off" || cfg.Memory.IndexBytes != 0 ||
		cfg.Subagents["scout"] != (ModelEntry{ID: "codex/scout", Reasoning: "low"}) {
		t.Fatalf("merged config: %+v", cfg)
	}
	want := map[string]string{"Authorization": "profile", "X-Shared": "kept"}
	if !reflect.DeepEqual(cfg.BrowserAuth["https://example.com"], want) {
		t.Fatalf("headers: %v", cfg.BrowserAuth)
	}

	// Strings replace objects and vice versa, without inheriting object fields.
	writeLayer(t, dir, "config.work.json", `{"model":"claude/new","subagents":{"scout":"codex/new"}}`)
	cfg, err = LoadProfile(dir, "work")
	if err != nil || cfg.Provider != "claude" || cfg.Model != "new" || cfg.ModelReasoning != "" || cfg.Subagents["scout"].Reasoning != "" {
		t.Fatalf("string replacement: %+v, %v", cfg, err)
	}
	writeLayer(t, dir, "config.shared.toml", `model = "codex/old"`)
	writeLayer(t, dir, "config.work.json", `{"model":{"id":"codex/new","reasoning":"low"}}`)
	cfg, err = LoadProfile(dir, "work")
	if err != nil || cfg.Model != "new" || cfg.ModelReasoning != "low" {
		t.Fatalf("object replacement: %+v, %v", cfg, err)
	}
}

func TestMergeConfigPreservesExplicitValues(t *testing.T) {
	base := map[string]any{"bool": true, "number": 1, "string": "x", "array": []any{1}, "object": map[string]any{"left": 1, "right": 1}}
	mergeConfig(base, map[string]any{"bool": false, "number": 0, "string": "", "array": []any{}, "object": map[string]any{"right": 0}})
	want := map[string]any{"bool": false, "number": 0, "string": "", "array": []any{}, "object": map[string]any{"left": 1, "right": 0}}
	if !reflect.DeepEqual(base, want) {
		t.Fatalf("got %#v, want %#v", base, want)
	}
}

func TestSharedResolutionAndExplicitPaths(t *testing.T) {
	dir := t.TempDir()
	sharedJSON := writeLayer(t, dir, "config.shared.json", `{"mode":"ask","model":"codex/json"}`)
	cfg, err := LoadProfile(dir, DefaultProfile)
	if err != nil || cfg.Mode != "ask" || cfg.Model != "json" {
		t.Fatalf("shared only: %+v %v", cfg, err)
	}
	if _, err := LoadProfile(dir, "missing"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing profile: %v", err)
	}
	writeLayer(t, dir, "config.shared.toml", "mode = \"read-only\"\nmodel = \"codex/toml\"\n")
	writeLayer(t, dir, "config.toml", "model = \"codex/default\"\n")
	writeLayer(t, dir, "config.work.toml", "model = \"codex/work\"\n")
	custom := writeLayer(t, dir, "custom.json", `{"model":"codex/custom"}`)
	for _, tc := range []struct{ path, model string }{
		{filepath.Join(dir, "config.toml"), "default"},
		{filepath.Join(dir, "config.work.toml"), "work"},
		{custom, "custom"},
	} {
		cfg, err := Load(tc.path)
		if err != nil || cfg.Mode != "read-only" || cfg.Model != tc.model {
			t.Fatalf("%s: %+v %v", tc.path, cfg, err)
		}
	}
	cfg, err = Load(sharedJSON)
	if err != nil || cfg.Mode != "ask" || cfg.Model != "json" {
		t.Fatalf("explicit shared JSON should load only itself: %+v %v", cfg, err)
	}
	names, err := Profiles(dir)
	if err != nil || !slices.Equal(names, []string{"default", "work"}) {
		t.Fatalf("profiles: %v %v", names, err)
	}
	if _, err := LoadProfile(dir, "shared"); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("shared profile: %v", err)
	}
}

func TestSharedSchemaAndFinalValidation(t *testing.T) {
	for _, tc := range []struct {
		name, shared, profile string
		wantError             bool
	}{
		{"unknown overridden", `{"memory":{"typo":true}}`, `{"memory":null}`, true},
		{"unknown model overridden", `{"model":{"id":"codex","typo":1}}`, `{"model":"codex"}`, true},
		{"wrong type overridden", `{"max_steps":"bad"}`, `{"max_steps":10}`, true},
		{"invalid semantic overridden", `{"mode":"invalid"}`, `{"mode":"ask"}`, false},
		{"invalid semantic remains", `{"mode":"invalid"}`, `{}`, true},
		{"partial shared model", `{"model":{"reasoning":"low"}}`, `{"model":{"id":"codex/test"}}`, false},
		{"partial profile model", `{"model":{"id":"codex/test"}}`, `{"model":{"reasoning":"low"}}`, false},
		{"incomplete combined model", `{"model":{"reasoning":"low"}}`, `{}`, true},
		{"invalid profile type", `{"max_steps":10}`, `{"max_steps":"bad"}`, true},
		{"trailing shared data", `{} {}`, `{}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeLayer(t, dir, "config.shared.json", tc.shared)
			path := writeLayer(t, dir, "config.json", tc.profile)
			_, err := Load(path)
			if (err != nil) != tc.wantError {
				t.Fatalf("error = %v, want error %v", err, tc.wantError)
			}
		})
	}
}

func TestSharedErrorsAndInit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	writeLayer(t, dir, "config.shared.json", `{"mode":"ask"}`)
	if err := Init(path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || strings.Contains(string(data), `mode = "ask"`) {
		t.Fatalf("init flattened shared: %s, %v", data, err)
	}
	writeLayer(t, dir, "config.shared.toml", "mode = [")
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "config.shared.toml") {
		t.Fatalf("invalid preferred shared TOML: %v", err)
	}
	writeLayer(t, dir, "config.shared.toml", strings.Repeat(" ", (1<<20)+1))
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "1 MiB") {
		t.Fatalf("oversize shared: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, "config.shared.toml")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("missing", filepath.Join(dir, "config.shared.toml")); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("dangling shared TOML must not fall back to JSON")
	}
}

func TestConfigRejectsDuplicateSchemaFields(t *testing.T) {
	for _, tc := range []struct {
		name, data, field string
	}{
		{"mode alias", `{"mode":"yolo","MODE":"read-only"}`, "mode"},
		{"mode null", `{"mode":"read-only","mode":null}`, "mode"},
		{"memory objects", `{"memory":{"reflection":"off"},"memory":{"index_bytes":8192}}`, "memory"},
		{"memory field alias", `{"memory":{"reflection":"off","REFLECTION":"on"}}`, "reflection"},
		{"escaped alias", `{"mode":"ask","\u006dode":"read-only"}`, "mode"},
		{"model object", `{"model":{"id":"codex/a","ID":"codex/b"}}`, "id"},
		{"memory model", `{"memory":{"model":{"id":"codex/a","reasoning":"low","REASONING":"high"}}}`, "reasoning"},
		{"subagent model", `{"subagents":{"scout":{"id":"codex/a","ID":"codex/b"}}}`, "id"},
		{"enabled model", `{"enabled_models":[{"id":"codex/a","context_window":64000,"CONTEXT_WINDOW":128000}]}`, "context_window"},
		{"provider", `{"providers":[{"id":"router","base_url":"https://example.com","BASE_URL":"https://other.com"}]}`, "base_url"},
	} {
		for _, shared := range []bool{false, true} {
			t.Run(tc.name+map[bool]string{false: "/profile", true: "/shared"}[shared], func(t *testing.T) {
				dir := t.TempDir()
				profile := tc.data
				if shared {
					writeLayer(t, dir, "config.shared.json", tc.data)
					// A profile override must not hide a duplicate in the shared layer.
					profile = `{"mode":"ask","memory":null,"model":"codex","subagents":{},"enabled_models":[],"providers":[]}`
				}
				path := writeLayer(t, dir, "config.json", profile)
				_, err := Load(path)
				if err == nil || !strings.Contains(err.Error(), `duplicate configuration field "`+tc.field+`"`) {
					t.Fatalf("error = %v, want duplicate field %q", err, tc.field)
				}
			})
		}
	}
}

func TestConfigRejectsTOMLFieldAliases(t *testing.T) {
	for _, data := range []string{
		"mode = \"yolo\"\nMODE = \"read-only\"\n",
		"[memory]\nreflection = \"off\"\nREFLECTION = \"on\"\n",
		"model = { id = \"codex/a\", ID = \"codex/b\" }\n",
	} {
		path := writeLayer(t, t.TempDir(), "config.toml", data)
		if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "duplicate configuration field") {
			t.Fatalf("input %q: error = %v", data, err)
		}
	}
}

func TestDynamicMapKeysRemainCaseSensitive(t *testing.T) {
	path := writeLayer(t, t.TempDir(), "config.json", `{
		"subagents":{"scout":{"id":"codex/a"},"Scout":{"ID":"codex/b"}},
		"browser_auth":{"https://example.com":{"X-Key":"one","x-key":"two"}}
	}`)
	values, err := readConfigLayer(path)
	if err != nil {
		t.Fatal(err)
	}
	roles := values["subagents"].(map[string]any)
	headers := values["browser_auth"].(map[string]any)["https://example.com"].(map[string]any)
	if len(roles) != 2 || roles["scout"].(map[string]any)["id"] != "codex/a" ||
		roles["Scout"].(map[string]any)["id"] != "codex/b" ||
		len(headers) != 2 || headers["X-Key"] != "one" || headers["x-key"] != "two" {
		t.Fatalf("roles = %#v, headers = %#v", roles, headers)
	}
}

func TestSharedMergeJSONFieldNamesAndMapKeys(t *testing.T) {
	dir := t.TempDir()
	writeLayer(t, dir, "config.shared.json", `{"MODE":"ask","MEMORY":{"REFLECTION":"off"},"browser_auth":{"https://example.com":{"X-Key":"one","x-key":"two"}}}`)
	path := writeLayer(t, dir, "config.work.toml", "mode = \"read-only\"\n[memory]\nreflection = \"on\"\n")
	cfg, err := Load(path)
	if err != nil || cfg.Mode != "read-only" || cfg.Memory.Reflection != "on" || len(cfg.BrowserAuth["https://example.com"]) != 2 {
		t.Fatalf("field names and map keys: %+v, %v", cfg, err)
	}
}

func TestModelAllowlistSharedAndEmptyProfileOverride(t *testing.T) {
	dir := t.TempDir()
	writeLayer(t, dir, "config.shared.toml", `enabled_models = ["codex/allowed", "claude/sonnet"]`)
	writeLayer(t, dir, "config.work.json", `{}`)
	cfg, err := LoadProfile(dir, "work")
	if err != nil || !cfg.ModelSelectable("codex", "allowed") || !cfg.ModelSelectable("claude", "sonnet") || cfg.ModelSelectable("codex", "other") || cfg.ModelSelectable("codex", "") {
		t.Fatalf("shared allowlist: %+v, %v", cfg.EnabledModels, err)
	}
	writeLayer(t, dir, "config.work.json", `{"enabled_models":[]}`)
	cfg, err = LoadProfile(dir, "work")
	if err != nil || !cfg.ModelSelectable("codex", "other") {
		t.Fatalf("empty profile override should remove restriction: %+v, %v", cfg.EnabledModels, err)
	}
}

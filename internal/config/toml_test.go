package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestTOMLLoadsSameConfigurationAsJSON(t *testing.T) {
	dir := t.TempDir()
	tomlData := `# A TOML file can mix model strings and inline tables.
model = { id = "codex/gpt-test", reasoning = "high", context_window = 128000 }
mode = "ask"
max_steps = 42
enabled_models = ["codex/gpt-test", { id = "router/vendor/model", reasoning = "low", context_window = 64000 }]
workspace_instructions = ["AGENTS.md"]
[[providers]]
id = "router"
base_url = "https://router.example/v1"
api_key_env = "ROUTER_KEY"
[browser_auth."https://example.com"]
Authorization = "Bearer $TOKEN"
[subagents]
scout = "codex/gpt-test"
reviewer = { id = "router/vendor/model", reasoning = "high" }
[memory]
reflection = "off"
model = { id = "codex/gpt-test", reasoning = "low" }
index_bytes = 8192
`
	jsonData := `{
"model":{"id":"codex/gpt-test","reasoning":"high","context_window":128000},
"mode":"ask","max_steps":42,
"enabled_models":["codex/gpt-test",{"id":"router/vendor/model","reasoning":"low","context_window":64000}],
"workspace_instructions":["AGENTS.md"],
"providers":[{"id":"router","base_url":"https://router.example/v1","api_key_env":"ROUTER_KEY"}],
"browser_auth":{"https://example.com":{"Authorization":"Bearer $TOKEN"}},
"subagents":{"scout":"codex/gpt-test","reviewer":{"id":"router/vendor/model","reasoning":"high"}},
"memory":{"reflection":"off","model":{"id":"codex/gpt-test","reasoning":"low"},"index_bytes":8192}
}`
	var configs []Config
	for name, data := range map[string]string{"custom.toml": tomlData, "custom.json": jsonData} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		configs = append(configs, cfg)
	}
	if !reflect.DeepEqual(configs[0], configs[1]) {
		t.Fatalf("formats differ: %+v, %+v", configs[0], configs[1])
	}
}

func TestDanglingConfigurationSymlinkDoesNotUseDefaults(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "julia")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", root)

	for _, base := range []string{"config", "config.work"} {
		if err := os.WriteFile(filepath.Join(dir, base+".json"), []byte(`{"mode":"read-only"}`), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("missing.toml", filepath.Join(dir, base+".toml")); err != nil {
			t.Fatal(err)
		}
	}

	path, err := Path()
	if err != nil || path != filepath.Join(dir, "config.toml") {
		t.Fatalf("default path %q: %v", path, err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("loading the dangling default TOML symlink must fail")
	}
	for _, name := range []string{DefaultProfile, "work"} {
		if _, err := LoadProfile(dir, name); err == nil {
			t.Fatalf("loading profile %q through a dangling TOML symlink must fail", name)
		}
	}
	if _, err := Load(filepath.Join(dir, "config.work.toml")); err == nil {
		t.Fatal("loading an explicit dangling TOML symlink must fail")
	}
	if cfg, err := Load(filepath.Join(dir, "absent.toml")); err != nil || !reflect.DeepEqual(cfg, Default()) {
		t.Fatalf("an absent file must still use defaults: %+v, %v", cfg, err)
	}

	if err := os.WriteFile(filepath.Join(dir, "valid.toml"), []byte("mode = \"read-only\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	validLink := filepath.Join(dir, "valid-link.toml")
	if err := os.Symlink("valid.toml", validLink); err != nil {
		t.Fatal(err)
	}
	if cfg, err := Load(validLink); err != nil || cfg.Mode != "read-only" {
		t.Fatalf("valid symlink: %+v, %v", cfg, err)
	}
}

func TestTOMLRejectsInvalidConfiguration(t *testing.T) {
	for name, data := range map[string]string{
		"syntax":                   "model = [",
		"duplicate":                "mode = \"auto\"\nmode = \"ask\"",
		"unknown":                  "unknown = true",
		"unknown model key":        "model = { id = \"codex\", typo = true }",
		"wrong type":               "max_steps = \"42\"",
		"float instead of integer": "max_steps = 42.0",
		"date instead of string":   "timeout = 1979-05-27",
		"invalid value":            "mode = \"sometimes\"",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "custom.toml")
			if err := os.WriteFile(path, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil {
				t.Fatalf("accepted %s", data)
			}
		})
	}
}

func TestInitFormatsAndDoesNotOverwrite(t *testing.T) {
	for _, ext := range []string{".toml", ".json"} {
		t.Run(ext, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "julia", "config"+ext)
			if err := Init(path); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if ext == ".toml" && (!strings.Contains(string(data), "max_steps = 256") || strings.Contains(string(data), "256.0")) {
				t.Fatalf("TOML must contain integers: %s", data)
			}
			if cfg, err := Load(path); err != nil || !reflect.DeepEqual(cfg, Default()) {
				t.Fatalf("round trip: %+v, %v", cfg, err)
			}
			if err := Init(path); !os.IsExist(err) {
				t.Fatalf("init overwrote file or returned wrong error: %v", err)
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != string(data) {
				t.Fatalf("file changed after second init: %v", err)
			}
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatalf("file permissions: %v, %v", info, err)
			}
		})
	}
}

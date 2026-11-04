package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestProfileNamesAndPaths(t *testing.T) {
	dir := "/home/me/.config/julia"
	for name, file := range map[string]string{DefaultProfile: "config.toml", "work": "config.work.toml", "Side_Project-2": "config.Side_Project-2.toml"} {
		path := ProfilePath(dir, name)
		if path != filepath.Join(dir, file) {
			t.Fatalf("profile %s is at %s", name, path)
		}
		if got, ok := ProfileName(path); !ok || got != name {
			t.Fatalf("%s holds profile %q, %v", path, got, ok)
		}
	}
	for _, file := range []string{"config.default.json", "config.default.toml", "config..json", "config.a.b.json", "config.work.yaml", "settings.json", "config.json.bak"} {
		if name, ok := ProfileName(filepath.Join(dir, file)); ok {
			t.Fatalf("%s is no profile, yet named %q", file, name)
		}
	}
	for _, name := range []string{"", "a.b", "../x", "with space", strings.Repeat("x", 65)} {
		if ValidProfileName(name) {
			t.Fatalf("accepted profile name %q", name)
		}
	}
}

func TestProfilesListsDefaultFirstThenByName(t *testing.T) {
	dir := t.TempDir()
	for _, file := range []string{"config.zeta.json", "config.json", "config.alpha.json", "config.default.json", "config.bad.name.json", "firecrawl.json", "linear.json"} {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(`{}`), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "config.folder.json"), 0700); err != nil {
		t.Fatal(err)
	}
	names, err := Profiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"default", "alpha", "zeta"}; !slices.Equal(names, want) {
		t.Fatalf("profiles %v, want %v", names, want)
	}
	// Without a configuration directory there is still the default profile.
	if names, err := Profiles(filepath.Join(dir, "missing")); err != nil || !slices.Equal(names, []string{"default"}) {
		t.Fatalf("profiles of a missing directory: %v, %v", names, err)
	}
}

func TestLoadProfile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.work.json"), []byte(`{"mode":"ask","max_steps":12}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadProfile(dir, "work")
	if err != nil || cfg.Mode != "ask" || cfg.MaxSteps != 12 {
		t.Fatalf("loaded %+v, %v", cfg, err)
	}
	// The default profile without config.json is the defaults, as before profiles.
	if cfg, err := LoadProfile(dir, DefaultProfile); err != nil || cfg.Mode != Default().Mode {
		t.Fatalf("default profile: %+v, %v", cfg, err)
	}
	if _, err := LoadProfile(dir, "wrok"); err == nil || !strings.Contains(err.Error(), "config.wrok.toml") {
		t.Fatalf("a missing profile must name the file to create: %v", err)
	}
	if _, err := LoadProfile(dir, "../work"); err == nil {
		t.Fatal("loaded a profile outside the configuration directory")
	}
	if err := os.WriteFile(filepath.Join(dir, "config.broken.json"), []byte(`{"mode":"sometimes"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadProfile(dir, "broken"); err == nil || !strings.Contains(err.Error(), "profile broken") {
		t.Fatalf("an invalid profile must say which: %v", err)
	}
}

func TestProfileResolutionPrefersTOMLAndListsOnce(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"config.json":         `{"model":"codex/json"}`,
		"config.toml":         `model = "codex/toml"`,
		"config.work.json":    `{"model":"codex/json-work"}`,
		"config.work.toml":    `model = "codex/toml-work"`,
		"config.legacy.json":  `{"model":"codex/legacy"}`,
		"config.invalid.toml": `model = [`,
		"config.invalid.json": `{"model":"codex/valid"}`,
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	names, err := Profiles(dir)
	if err != nil || !slices.Equal(names, []string{"default", "invalid", "legacy", "work"}) {
		t.Fatalf("profiles %v: %v", names, err)
	}
	for name, want := range map[string]string{"default": "toml", "work": "toml-work", "legacy": "legacy"} {
		cfg, err := LoadProfile(dir, name)
		if err != nil || cfg.Model != want {
			t.Fatalf("%s: %+v, %v", name, cfg, err)
		}
	}
	if _, err := LoadProfile(dir, "invalid"); err == nil {
		t.Fatal("invalid TOML must not fall back to JSON")
	}
	for _, file := range []string{"config.json", "config.toml", "config.work.json", "config.work.toml"} {
		if _, ok := ProfileName(file); !ok {
			t.Fatalf("%s not recognized", file)
		}
	}
	t.Setenv("XDG_CONFIG_HOME", dir)
	path, err := Path()
	if err != nil || path != filepath.Join(dir, "julia", "config.toml") {
		t.Fatalf("new default path %q, %v", path, err)
	}
}

func TestDefaultPathUsesExistingJSONUntilTOMLExists(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", root)
	dir := filepath.Join(root, "julia")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	jsonPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(jsonPath, []byte(`{"model":"codex/legacy"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if path, err := Path(); err != nil || path != jsonPath {
		t.Fatalf("legacy path %q, %v", path, err)
	}
	if err := Init(jsonPath); !os.IsExist(err) {
		t.Fatalf("init must refuse an existing JSON file: %v", err)
	}
	tomlPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(tomlPath, []byte(`model = "codex/new"`), 0600); err != nil {
		t.Fatal(err)
	}
	if path, err := Path(); err != nil || path != tomlPath {
		t.Fatalf("preferred path %q, %v", path, err)
	}
	if cfg, err := Load(jsonPath); err != nil || cfg.Model != "legacy" {
		t.Fatalf("explicit JSON path: %+v, %v", cfg, err)
	}
}

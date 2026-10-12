package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRememberedReasoningIsPerProvider(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jarvis", "preferences.json")
	if p, err := LoadPreferences(path); err != nil || len(p.Reasoning) != 0 {
		t.Fatalf("missing file: %+v %v", p, err)
	}
	if err := RememberReasoning(path, "codex", "high"); err != nil {
		t.Fatal(err)
	}
	if err := RememberReasoning(path, "compatible", ""); err != nil {
		t.Fatal(err)
	}
	if err := RememberReasoning(path, "codex", "low"); err != nil {
		t.Fatal(err)
	}
	p, err := LoadPreferences(path)
	if err != nil {
		t.Fatal(err)
	}
	if effort, ok := p.Reasoning["compatible"]; !ok || effort != "" {
		t.Fatalf("default choice not remembered: %+v", p)
	}

	cfg := Default()
	cfg.Reasoning = "medium"
	cfg.ApplyStartingReasoning(p)
	if cfg.Reasoning != "low" {
		t.Fatalf("codex reasoning = %q", cfg.Reasoning)
	}
	cfg = Default()
	cfg.Provider, cfg.Model, cfg.Reasoning = "openai", "gpt", ""
	cfg.ApplyStartingReasoning(p)
	if cfg.Reasoning != "" {
		t.Fatalf("another provider's effort leaked: %q", cfg.Reasoning)
	}
}

func TestRememberedReasoningIgnoredWhenInvalid(t *testing.T) {
	cfg := Default()
	cfg.Provider, cfg.Model, cfg.BaseURL = "compatible", "vendor/model", "https://example.com/v1"
	cfg.ApplyStartingReasoning(Preferences{Reasoning: map[string]string{"compatible": "high"}})
	if cfg.Reasoning != "" {
		t.Fatalf("applied unsupported effort %q", cfg.Reasoning)
	}
}

func TestCorruptPreferencesAreReplaced(t *testing.T) {
	path := filepath.Join(t.TempDir(), "preferences.json")
	if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPreferences(path); err == nil {
		t.Fatal("accepted corrupt preferences")
	}
	if err := RememberReasoning(path, "codex", "high"); err != nil {
		t.Fatal(err)
	}
	if p, err := LoadPreferences(path); err != nil || p.Reasoning["codex"] != "high" {
		t.Fatalf("%+v %v", p, err)
	}
}

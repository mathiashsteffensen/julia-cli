package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConfigRejectsUnknownAndUnsafeModes(t *testing.T) {
	for _, data := range []string{`{"mode":"yolo"}`, `{"provider":"typo"}`, `{"approve_everything":true}`, `{"timeout":"0s"}`, `{} {}`} {
		p := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(p, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(p); err == nil {
			t.Fatalf("accepted %s", data)
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

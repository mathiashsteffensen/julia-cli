package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUsageResetsAreOptIn(t *testing.T) {
	if Default().UseUsageResets {
		t.Fatal("usage resets must be disabled by default")
	}
	for _, tc := range []struct {
		name, data string
		want       bool
	}{
		{"omitted.json", "{}", false},
		{"disabled.json", `{"use_usage_resets":false}`, false},
		{"enabled.json", `{"use_usage_resets":true}`, true},
		{"omitted.toml", "", false},
		{"disabled.toml", "use_usage_resets = false", false},
		{"enabled.toml", "use_usage_resets = true", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), tc.name)
			if err := os.WriteFile(path, []byte(tc.data), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.UseUsageResets != tc.want {
				t.Fatalf("UseUsageResets = %v, want %v", cfg.UseUsageResets, tc.want)
			}
		})
	}
	data, err := defaultTOML()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "use_usage_resets = false") {
		t.Fatal("generated configuration must show resets disabled")
	}
}

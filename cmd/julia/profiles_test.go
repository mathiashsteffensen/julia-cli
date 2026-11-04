package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"julia/internal/config"
	"julia/internal/core"
)

func writeProfile(t *testing.T, dir, name, data string) {
	t.Helper()
	file := "config." + name + ".json"
	if name == config.DefaultProfile {
		file = "config.json"
	}
	if err := os.WriteFile(filepath.Join(dir, file), []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestSwitchedProfileKeepsLaunchOverridesAndChecksItCanRun(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "work", `{"model":"codex/gpt-work","mode":"auto","timeout":"1h","enabled_models":["codex/gpt-work","codex/gpt-fast"]}`)
	cfg, err := loadSwitchedProfile(dir, "work", launchOverrides{mode: "read-only", timeout: "5m"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Model != "gpt-work" || cfg.Mode != "read-only" || cfg.Timeout != "5m" || len(cfg.EnabledModels) != 2 {
		t.Fatalf("switched profile %+v", cfg)
	}

	writeProfile(t, dir, "keyless", `{"model":"openai/gpt-api"}`)
	t.Setenv("OPENAI_API_KEY", "")
	if _, err := loadSwitchedProfile(dir, "keyless", launchOverrides{}, ""); err == nil || !strings.Contains(err.Error(), "OPENAI_API_KEY") {
		t.Fatalf("a profile whose provider has no key must not switch: %v", err)
	}
	writeProfile(t, dir, "roles", `{"subagents":{"nobody":"codex/gpt-work"}}`)
	if _, err := loadSwitchedProfile(dir, "roles", launchOverrides{}, ""); err == nil || !strings.Contains(err.Error(), "nobody") {
		t.Fatalf("a profile assigning an unknown role must not switch: %v", err)
	}
	if _, err := loadSwitchedProfile(dir, "absent", launchOverrides{}, ""); err == nil {
		t.Fatal("switched to a profile that does not exist")
	}
}

func TestSwitchedProfileStartsAtRememberedReasoning(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "work", `{"model":"codex/gpt-work"}`)
	prefs := filepath.Join(t.TempDir(), "preferences.json")
	if err := config.RememberReasoning(prefs, "codex", "xhigh"); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadSwitchedProfile(dir, "work", launchOverrides{}, prefs)
	if err != nil || cfg.Reasoning != "xhigh" {
		t.Fatalf("reasoning %q, %v", cfg.Reasoning, err)
	}
}

func TestProfileChoicesDescribeEachProfile(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, config.DefaultProfile, `{"model":"codex/gpt-home"}`)
	writeProfile(t, dir, "work", `{"model":"codex/gpt-work","mode":"ask"}`)
	writeProfile(t, dir, "broken", `{"mode":"sometimes"}`)
	choices, err := profileChoices(dir)
	if err != nil {
		t.Fatal(err)
	}
	details := map[string]string{}
	var names []string
	for _, choice := range choices {
		details[choice.Name] = choice.Detail
		names = append(names, choice.Name)
	}
	if strings.Join(names, " ") != "default broken work" {
		t.Fatalf("profiles listed as %v", names)
	}
	if details["default"] != "config.json · codex/gpt-home · auto" || details["work"] != "config.work.json · codex/gpt-work · ask" {
		t.Fatalf("details %v", details)
	}
	if !strings.Contains(details["broken"], "does not load: unknown approval mode") {
		t.Fatalf("a broken profile must say so: %q", details["broken"])
	}
}

func TestProfileLabel(t *testing.T) {
	for path, want := range map[string]string{"/c/julia/config.json": "default", "/c/julia/config.work.json": "work", "/elsewhere/custom.json": "custom.json"} {
		if got := profileLabel(path); got != want {
			t.Fatalf("%s labelled %q, want %q", path, got, want)
		}
	}
}

// A switched profile reaches the next turn: its mode and timeout, not the
// launch profile's.
func TestRunnerTurnsUseTheSwitchedProfile(t *testing.T) {
	launch := config.Default()
	var modes []string
	var deadlines []time.Duration
	runner := &runtimeBackend{cfg: launch, timeout: time.Hour, parent: context.Background(), makeBackend: func(ctx context.Context, selected config.Config) (core.Backend, core.Approver, error) {
		modes = append(modes, selected.Mode)
		deadline, _ := ctx.Deadline()
		deadlines = append(deadlines, time.Until(deadline).Round(time.Minute))
		return plainBackend{}, nil, nil
	}}
	current := &core.Session{Directory: t.TempDir(), Provider: "codex"}
	if err := runner.Run(context.Background(), current, core.Prompt{Text: "first"}, nil, func(core.Event) {}); err != nil {
		t.Fatal(err)
	}
	work := config.Default()
	work.Mode, work.Timeout = "read-only", "10m"
	runner.switchProfile(work)
	if err := runner.Run(context.Background(), current, core.Prompt{Text: "second"}, nil, func(core.Event) {}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(modes, " ") != "auto read-only" || deadlines[0] != time.Hour || deadlines[1] != 10*time.Minute {
		t.Fatalf("turns ran with modes %v and timeouts %v", modes, deadlines)
	}
}

// Switching to a profile on another provider starts a fresh conversation and
// saves the old one, as choosing that model would.
func TestSwitchingProfileProviderStartsFreshConversation(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "router", `{"providers":[{"id":"router","base_url":"https://router.example/v1"}],"model":"router/vendor/model"}`)
	cfg, err := loadSwitchedProfile(dir, "router", launchOverrides{}, "")
	if err != nil {
		t.Fatal(err)
	}
	current := &core.Session{ID: "old", Directory: t.TempDir(), Provider: "codex", Messages: []core.Message{{Role: "user", Content: "hello"}}}
	var saved []string
	save := func(s *core.Session) error { saved = append(saved, s.ID); return nil }
	if err := applySessionSettings(cfg, current, cfg.Provider, cfg.Model, cfg.Reasoning, save); err != nil {
		t.Fatal(err)
	}
	if current.ID == "old" || current.Provider != "router" || current.Model != "vendor/model" || len(current.Messages) != 0 || len(saved) != 2 || saved[0] != "old" {
		t.Fatalf("session after switch %+v, saved %v", current, saved)
	}
}

func TestProfileChoicesAndSwitchUseTOMLPrecedence(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "work", `{"model":"codex/json"}`)
	if err := os.WriteFile(filepath.Join(dir, "config.work.toml"), []byte("model = \"codex/toml\"\nmode = \"ask\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	choices, err := profileChoices(dir)
	if err != nil || len(choices) != 2 || choices[1].Detail != "config.work.toml · codex/toml · ask" {
		t.Fatalf("choices: %+v, %v", choices, err)
	}
	cfg, err := loadSwitchedProfile(dir, "work", launchOverrides{}, "")
	if err != nil || cfg.Model != "toml" || cfg.Mode != "ask" {
		t.Fatalf("switched profile: %+v, %v", cfg, err)
	}
	if label := profileLabel(filepath.Join(dir, "config.work.toml")); label != "work" {
		t.Fatalf("label: %s", label)
	}
}

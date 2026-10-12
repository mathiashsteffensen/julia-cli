package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"julia/internal/config"
	"julia/internal/core"
)

type instructionBackend struct {
	t         *testing.T
	seen      []string
	skillPath string
}

func (b *instructionBackend) Name() string { return "fake" }
func (b *instructionBackend) Run(ctx context.Context, s *core.Session, prompt core.Prompt, tools core.ToolExecutor, emit core.Emit) error {
	b.seen = append(b.seen, s.Instructions)
	args, _ := json.Marshal(map[string]any{"path": b.skillPath})
	if result := tools.Execute(ctx, core.ToolCall{Name: "Read", Arguments: args}); !result.IsError {
		b.t.Fatal("host skill loading widened workspace tool access")
	}
	return nil
}

func TestRuntimeRefreshesInstructionsWithoutExpandingToolAccess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "SKILL.md")
	if err := os.WriteFile(path, []byte("style revision one"), 0600); err != nil {
		t.Fatal(err)
	}
	backend := &instructionBackend{t: t, skillPath: path}
	cfg := config.Default()
	cfg.SkillPaths = []string{path}
	cfg.Reasoning = "medium"
	runner := &runtimeBackend{backend: backend, cfg: cfg, timeout: time.Second, parent: context.Background()}
	s := &core.Session{Directory: t.TempDir(), Provider: "fake", Model: "first-model", Reasoning: "medium"}
	emit := func(core.Event) {}
	if err := runner.Run(context.Background(), s, core.Prompt{Text: "Inspect this project"}, nil, emit); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("style revision two"), 0600); err != nil {
		t.Fatal(err)
	}
	runner.cfg.Mode, runner.headless = "read-only", true
	s.Reasoning, s.Model = "high", "second-model"
	if err := runner.Run(context.Background(), s, core.Prompt{Text: "Continue"}, nil, emit); err != nil {
		t.Fatal(err)
	}
	if len(backend.seen) != 2 || !strings.Contains(backend.seen[0], "style revision one") || strings.Contains(backend.seen[1], "style revision one") || !strings.Contains(backend.seen[1], "style revision two") || !strings.Contains(backend.seen[1], `"human_approval_available": false`) || !strings.Contains(backend.seen[1], `"approval_mode": "read-only"`) {
		t.Fatal("runtime did not supply refreshed instructions")
	}
	for i, expected := range []struct{ model, effort string }{{"first-model", "medium"}, {"second-model", "high"}} {
		if !strings.Contains(backend.seen[i], `"model": "`+expected.model+`"`) || !strings.Contains(backend.seen[i], `"reasoning_effort": "`+expected.effort+`"`) {
			t.Fatal("runtime instructions lost current model/reasoning settings")
		}
	}
	runner.cfg.SkillPaths = []string{filepath.Join(t.TempDir(), "missing.md")}
	if err := runner.Run(context.Background(), s, core.Prompt{Text: "Continue"}, nil, emit); err == nil {
		t.Fatal("missing explicit skill did not stop the turn")
	}
	if len(backend.seen) != 2 {
		t.Fatal("provider ran with missing configured instructions")
	}
}

func TestCLIReasoningAndModelOverridesConfigurationWithoutProviderCalls(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"model":"codex/gpt-6-astra","reasoning":"medium","skill_paths":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	output, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	oldArgs, oldStdout := os.Args, os.Stdout
	defer func() { os.Args, os.Stdout = oldArgs, oldStdout }()
	os.Stdout = output
	for _, tc := range []struct {
		name      string
		flags     []string
		wantError string
	}{
		{"configured defaults", nil, ""},
		{"reasoning override", []string{"--reasoning", "high"}, ""},
		{"future model", []string{"--model", "codex/future-model"}, ""},
		{"bare codex model", []string{"--model", "codex"}, ""},
		{"unknown provider", []string{"--model", "vendor/model"}, "provider/<model-id>"},
		{"invalid effort override", []string{"--reasoning", "bad effort"}, "reasoning must"},
		{"invalid model override", []string{"--model", "codex/ leading-space"}, "model IDs must"},
		{"API cannot ignore configured effort", []string{"--model", "compatible/vendor/model", "--compatible-base-url", "http://127.0.0.1:1/v1"}, "Codex and OpenRouter-compatible sessions only"},
		{"explicit empty clears configured effort", []string{"--model", "compatible/vendor/model", "--compatible-base-url", "http://127.0.0.1:1/v1", "--reasoning="}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			os.Args = append([]string{"julia", "--config", path, "--dir", dir, "--snapshot", "40"}, tc.flags...)
			err := run()
			if tc.wantError == "" && err != nil {
				t.Fatal(err)
			}
			if tc.wantError != "" && (err == nil || !strings.Contains(err.Error(), tc.wantError)) {
				t.Fatalf("got %v; want %q", err, tc.wantError)
			}
		})
	}
}

func TestResumeModelAndReasoningPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name, configured, saved, modelOverride string
		explicitEmpty                          bool
		want                                   string
	}{
		{name: "retain saved", saved: "high", want: "high"},
		{name: "configured overrides saved", configured: "medium", saved: "high", want: "medium"},
		{name: "explicit empty inherits Codex", saved: "high", explicitEmpty: true, want: ""},
		{name: "explicit Codex model switch", saved: "medium", modelOverride: "new-model", want: "medium"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.Reasoning = tc.configured
			s := &core.Session{Directory: "/workspace", Provider: "codex", Model: "saved-model", Reasoning: tc.saved}
			if err := selectSessionModel(&cfg, s, "/workspace", "", tc.modelOverride, tc.explicitEmpty, true); err != nil {
				t.Fatal(err)
			}
			if cfg.Reasoning != tc.want || s.Reasoning != tc.want {
				t.Fatalf("reasoning = cfg %q/session %q, want %q", cfg.Reasoning, s.Reasoning, tc.want)
			}
			wantModel := "saved-model"
			if tc.modelOverride != "" {
				wantModel = tc.modelOverride
			}
			if cfg.Model != wantModel || s.Model != wantModel {
				t.Fatalf("model override lost: %q %q", cfg.Model, s.Model)
			}
		})
	}
	cfg := config.Default()
	s := &core.Session{Directory: "/workspace", Provider: "compatible", Model: "saved-api"}
	if err := selectSessionModel(&cfg, s, "/workspace", "", "other-api", false, true); err == nil {
		t.Fatal("API model switch accepted without history migration")
	}
}

func TestModelSettingsDescribeInheritance(t *testing.T) {
	cfg := config.Default()
	model, effort := modelSettings(cfg)
	if model != "inherited from Codex" || effort != "inherited from Codex" {
		t.Fatal("unresolved defaults presented as actual settings")
	}
	cfg.Model, cfg.Reasoning = "gpt-6-astra", "medium"
	model, effort = modelSettings(cfg)
	if model != cfg.Model || effort != cfg.Reasoning {
		t.Fatal("explicit settings hidden")
	}
}

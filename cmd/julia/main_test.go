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
	"julia/internal/tools"
)

type instructionBackend struct {
	t         *testing.T
	seen      []string
	loaded    []string
	skillPath string
}

func (b *instructionBackend) Name() string { return "fake" }
func (b *instructionBackend) Run(ctx context.Context, s *core.Session, prompt core.Prompt, executor core.ToolExecutor, emit core.Emit) error {
	b.seen = append(b.seen, s.Instructions)
	args, _ := json.Marshal(map[string]any{"path": b.skillPath})
	if result := executor.Execute(ctx, core.ToolCall{Name: "Read", Arguments: args}); !result.IsError {
		b.t.Fatal("host skill loading widened workspace tool access")
	}
	loaded := executor.Execute(ctx, core.ToolCall{Name: tools.SkillTool, Arguments: json.RawMessage(`{"name":"style"}`)})
	if loaded.IsError {
		b.t.Fatalf("the Skill tool did not load a listed skill: %s", loaded.Content)
	}
	b.loaded = append(b.loaded, loaded.Content)
	return nil
}

func writeStyleSkill(t *testing.T, path, revision string) {
	t.Helper()
	content := "---\nname: style\ndescription: Style " + revision + " for every task.\n---\nFollow policy " + revision + "."
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeRefreshesInstructionsWithoutExpandingToolAccess(t *testing.T) {
	skillsDir := t.TempDir()
	path := filepath.Join(skillsDir, "style", "SKILL.md")
	if err := os.Mkdir(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	writeStyleSkill(t, path, "one")
	backend := &instructionBackend{t: t, skillPath: path}
	cfg := config.Default()
	cfg.Reasoning = "medium"
	runner := &runtimeBackend{backend: backend, cfg: cfg, timeout: time.Second, parent: context.Background(), skillsDir: skillsDir}
	s := &core.Session{Directory: t.TempDir(), Provider: "fake", Model: "first-model", Reasoning: "medium"}
	emit := func(core.Event) {}
	if err := runner.Run(context.Background(), s, core.Prompt{Text: "Inspect this project"}, nil, emit); err != nil {
		t.Fatal(err)
	}
	writeStyleSkill(t, path, "two")
	runner.cfg.Mode, runner.headless = "read-only", true
	s.Reasoning, s.Model = "high", "second-model"
	if err := runner.Run(context.Background(), s, core.Prompt{Text: "Continue"}, nil, emit); err != nil {
		t.Fatal(err)
	}
	if len(backend.seen) != 2 || !strings.Contains(backend.seen[0], "- style: Style one") || strings.Contains(backend.seen[1], "Style one") || !strings.Contains(backend.seen[1], "- style: Style two") || !strings.Contains(backend.seen[1], `"human_approval_available": false`) || !strings.Contains(backend.seen[1], `"approval_mode": "read-only"`) {
		t.Fatal("runtime did not supply refreshed instructions")
	}
	if strings.Contains(backend.seen[1], "Follow policy") {
		t.Fatal("a skill's instructions belong to the Skill tool, not every turn")
	}
	if len(backend.loaded) != 2 || !strings.Contains(backend.loaded[0], "Follow policy one.") || !strings.Contains(backend.loaded[1], "Follow policy two.") {
		t.Fatalf("the Skill tool loaded %q", backend.loaded)
	}
	for i, expected := range []struct{ model, effort string }{{"first-model", "medium"}, {"second-model", "high"}} {
		if !strings.Contains(backend.seen[i], `"model": "`+expected.model+`"`) || !strings.Contains(backend.seen[i], `"reasoning_effort": "`+expected.effort+`"`) {
			t.Fatal("runtime instructions lost current model/reasoning settings")
		}
	}
}

func TestCLIReasoningAndModelOverridesConfigurationWithoutProviderCalls(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"model":"codex/gpt-6-astra","reasoning":"medium","providers":[{"id":"local","base_url":"http://127.0.0.1:1/v1"}]}`), 0600); err != nil {
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
		{"API cannot ignore configured effort", []string{"--model", "openai/vendor-model"}, "Codex, Claude Code and your providers only"},
		{"explicit empty clears configured effort", []string{"--model", "local/vendor/model", "--reasoning="}, ""},
		{"the compatible provider is gone", []string{"--model", "compatible/vendor/model", "--reasoning="}, "provider/<model-id>"},
		{"removed endpoint flag", []string{"--compatible-base-url", "http://127.0.0.1:1/v1"}, "flag provided but not defined"},
		{"removed key flag", []string{"--api-key-env", "OPENROUTER_API_KEY"}, "flag provided but not defined"},
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
	s := &core.Session{Directory: "/workspace", Provider: "openai", Model: "saved-api"}
	if err := selectSessionModel(&cfg, s, "/workspace", "", "other-api", false, true); err == nil {
		t.Fatal("API model switch accepted without history migration")
	}
}

func TestResumeModelOverrideClearsTheReportedWindow(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		for _, override := range []string{"", "saved-model", "new-model"} {
			t.Run(provider+"/"+override, func(t *testing.T) {
				cfg := config.Default()
				s := &core.Session{Directory: "/workspace", Provider: provider, Model: "saved-model", Usage: core.SessionUsage{ContextWindow: 1_000_000}}
				if err := selectSessionModel(&cfg, s, "/workspace", "", override, false, true); err != nil {
					t.Fatal(err)
				}
				want := 1_000_000
				if override == "new-model" {
					want = 0
				}
				if s.Usage.ContextWindow != want {
					t.Fatalf("resumed window = %d, want %d", s.Usage.ContextWindow, want)
				}
			})
		}
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

type invocationBackend struct {
	t      *testing.T
	text   string
	loaded core.ToolResult
}

func (b *invocationBackend) Name() string { return "fake" }
func (b *invocationBackend) Run(ctx context.Context, s *core.Session, prompt core.Prompt, executor core.ToolExecutor, emit core.Emit) error {
	b.text = prompt.Text
	b.loaded = executor.Execute(ctx, core.ToolCall{Name: tools.SkillTool, Arguments: json.RawMessage(`{"name":"implement","file":"CHECKLIST.md"}`)})
	return nil
}

// A skill the user runs reaches the model with its instructions, even one the
// model is never offered, and its files can be loaded for that turn.
func TestRuntimeRunsTheSkillTheUserInvoked(t *testing.T) {
	skillsDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(skillsDir, "implement"), 0700); err != nil {
		t.Fatal(err)
	}
	writeFile := func(name, content string) {
		if err := os.WriteFile(filepath.Join(skillsDir, "implement", name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeFile("SKILL.md", "---\nname: implement\ndescription: Implement a spec.\ndisable-model-invocation: true\n---\nWrite the tests first, then see CHECKLIST.md.")
	writeFile("CHECKLIST.md", "Run make check.")

	backend := &invocationBackend{t: t}
	cfg := config.Default()
	cfg.Mode = "read-only"
	runner := &runtimeBackend{backend: backend, cfg: cfg, timeout: time.Second, parent: context.Background(), skillsDir: skillsDir}
	s := &core.Session{Directory: t.TempDir(), Provider: "fake", Model: "m"}
	if err := runner.Run(context.Background(), s, core.Prompt{Text: "/implement docs/plan.md", Skill: "implement"}, nil, func(core.Event) {}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(backend.text, "/implement docs/plan.md\n\n<skill name=\"implement\"") || !strings.Contains(backend.text, "Write the tests first") {
		t.Fatalf("the model was sent %q", backend.text)
	}
	if backend.loaded.IsError || backend.loaded.Content != "Run make check." {
		t.Fatalf("the invoked skill's file was not loadable: %+v", backend.loaded)
	}
	if s.Title != "/implement docs/plan.md" {
		t.Fatalf("title %q", s.Title)
	}

	err := runner.Run(context.Background(), s, core.Prompt{Text: "/gone", Skill: "gone"}, nil, func(core.Event) {})
	if err == nil || !strings.Contains(err.Error(), `skill "gone"`) {
		t.Fatalf("a skill that has gone must stop the turn with a reason: %v", err)
	}
}

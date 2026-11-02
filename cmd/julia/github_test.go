package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"julia/agents"
	"julia/internal/config"
	"julia/internal/core"
	"julia/internal/session"
	"julia/internal/tools"
)

func TestGitHubDoctorDoesNotResolveCredentials(t *testing.T) {
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	dir := t.TempDir()
	t.Setenv("PATH", dir)

	if got := githubSummary(); !strings.Contains(got, "off;") {
		t.Fatalf("unconfigured: %s", got)
	}
	// Discovery may find gh, but must not invoke it to show doctor or the tools.
	marker := filepath.Join(dir, "called")
	script := "#!/bin/sh\nprintf called > '" + marker + "'\nprintf gho_fake_0123456789\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	if got := githubSummary(); !strings.Contains(got, "gh login") || strings.Contains(got, "gho_fake") {
		t.Fatalf("gh source: %s", got)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("doctor ran gh: %v", err)
	}

	t.Setenv("GH_TOKEN", "gho_environment_fixture_0123456789")
	if got := githubSummary(); !strings.Contains(got, "GH_TOKEN") || strings.Contains(got, "gho_environment") {
		t.Fatalf("environment source: %s", got)
	}
	t.Setenv("GH_TOKEN", "invalid secret")
	if got := githubSummary(); !strings.Contains(got, "auth unavailable") || strings.Contains(got, "invalid secret") {
		t.Fatalf("invalid source: %s", got)
	}
}

func TestRuntimeGitHubSharesApprovalWithChildrenAndProfiles(t *testing.T) {
	t.Setenv("GH_TOKEN", "gho_runtime_fixture_0123456789")
	auth, err := tools.LoadGitHubAuth()
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Mode = "auto"
	cfg.Memory.Reflection = "off"
	s := session.New(t.TempDir(), "fake", "model")
	s.Messages = []core.Message{{Role: "user", Content: "Inspect the GitHub issue. Do not comment."}}

	var reviews int
	review := func(_ context.Context, a core.Approval) (core.Decision, error) {
		reviews++
		if a.Call.Name != "GitHub" || a.ExecutionMode != "github" || !a.Network {
			return core.Decision{}, fmt.Errorf("wrong review: %+v", a)
		}
		if !strings.Contains(a.Task, "Do not comment") || strings.Contains(a.Task, "child proposes") {
			return core.Decision{}, fmt.Errorf("wrong authorization context: %s", a.Task)
		}
		return core.Decision{Reason: "No comment requested"}, nil
	}
	backend := &questionBackend{run: func(ctx context.Context, s *core.Session, _ core.Prompt, executor core.ToolExecutor) error {
		if !hasTool(executor.Definitions(), "GitHub") {
			return fmt.Errorf("GitHub missing from runtime tools")
		}
		if s.ParentID == "" {
			result := executor.Execute(ctx, core.ToolCall{Name: "Subagent", Arguments: json.RawMessage(`{"tasks":[{"role":"delegate","task":"child proposes a comment"}]}`)})
			if result.IsError || strings.Contains(result.Content, `"error"`) {
				return fmt.Errorf("delegation: %s", result.Content)
			}
		}
		// Parent and child propose different comments: an action already denied
		// this turn goes to a human without another review.
		issue := "1"
		if s.ParentID != "" {
			issue = "2"
		}
		result := executor.Execute(ctx, core.ToolCall{Name: "GitHub", Arguments: json.RawMessage(`{"method":"POST","path":"/repos/example/project/issues/` + issue + `/comments","body":"{\"body\":\"An unsolicited comment\"}"}`)})
		if !result.IsError || !(strings.Contains(result.Content, "denied") || strings.Contains(result.Content, "read-only")) {
			return fmt.Errorf("mutation escaped policy: %+v", result)
		}
		return nil
	}}
	runner := &runtimeBackend{
		backend: backend, cfg: cfg, github: auth, roles: agents.Builtin(),
		timeout: 30 * time.Second, parent: context.Background(), review: review,
		makeBackend: func(context.Context, config.Config) (core.Backend, core.Approver, error) {
			return backend, review, nil
		},
	}
	if !hasTool(runner.Definitions(), "GitHub") {
		t.Fatal("sidebar registry lacks GitHub")
	}
	if err := runner.Run(context.Background(), s, core.Prompt{Text: "Continue inspecting"}, nil, func(core.Event) {}); err != nil {
		t.Fatal(err)
	}
	if reviews != 2 {
		t.Fatalf("parent and child made %d reviews, want 2", reviews)
	}

	cfg.Mode = "read-only"
	runner.switchProfile(cfg)
	if err := runner.Run(context.Background(), s, core.Prompt{Text: "Continue inspecting"}, nil, func(core.Event) {}); err != nil {
		t.Fatal(err)
	}
	if reviews != 2 {
		t.Fatal("read-only profile asked the reviewer instead of blocking")
	}
	runner.github = tools.GitHubAuth{}
	if hasTool(runner.Definitions(), "GitHub") {
		t.Fatal("unconfigured registry exposes GitHub")
	}
}

func TestUnusableGitHubSourceTurnsOffOnlyTheTool(t *testing.T) {
	t.Run("invalid token", func(t *testing.T) {
		t.Setenv("GITHUB_TOKEN", "")
		t.Setenv("GH_TOKEN", "invalid secret")

		// A gh login on PATH may be another account, so it is not a fallback.
		dir := t.TempDir()
		t.Setenv("PATH", dir)
		if err := os.WriteFile(filepath.Join(dir, "gh"), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
			t.Fatal(err)
		}

		auth, warning := loadGitHubAuth()
		if auth.Enabled() || !strings.Contains(warning, "GH_TOKEN") || strings.Contains(warning, "invalid secret") {
			t.Fatalf("enabled=%v warning=%q", auth.Enabled(), warning)
		}
	})

	t.Run("gh through a relative PATH entry", func(t *testing.T) {
		t.Setenv("GH_TOKEN", "")
		t.Setenv("GITHUB_TOKEN", "")
		dir := t.TempDir()
		t.Chdir(dir)
		t.Setenv("PATH", ".")
		if err := os.WriteFile(filepath.Join(dir, "gh"), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
			t.Fatal(err)
		}

		auth, warning := loadGitHubAuth()
		if auth.Enabled() || !strings.Contains(warning, "GitHub tool off") {
			t.Fatalf("enabled=%v warning=%q", auth.Enabled(), warning)
		}
	})

	t.Run("usable source gives no warning", func(t *testing.T) {
		t.Setenv("GH_TOKEN", "gho_usable_fixture_0123456789")
		if auth, warning := loadGitHubAuth(); !auth.Enabled() || warning != "" {
			t.Fatalf("enabled=%v warning=%q", auth.Enabled(), warning)
		}
	})
}

func TestGitHubWarningIsShownOnce(t *testing.T) {
	cfg := config.Default()
	cfg.Mode = "auto"
	cfg.Memory.Reflection = "off"

	backend := &questionBackend{run: func(context.Context, *core.Session, core.Prompt, core.ToolExecutor) error { return nil }}
	runner := &runtimeBackend{
		backend: backend, cfg: cfg, githubWarning: "GitHub tool off: fixture",
		timeout: 30 * time.Second, parent: context.Background(),
	}
	if hasTool(runner.Definitions(), "GitHub") {
		t.Fatal("registry offers a tool that is off")
	}

	shown := 0
	s := session.New(t.TempDir(), "fake", "model")
	for range 2 {
		err := runner.Run(context.Background(), s, core.Prompt{Text: "Continue"}, nil, func(event core.Event) {
			if event.Kind == "warning" && event.Text == "GitHub tool off: fixture" {
				shown++
			}
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if shown != 1 {
		t.Fatalf("warning shown %d times, want once", shown)
	}
}

func TestCustomRoleCanSelectGitHub(t *testing.T) {
	t.Setenv("GH_TOKEN", "gho_role_fixture_0123456789")
	path := filepath.Join(t.TempDir(), "config.json")
	dir := rolesDir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	role := `---
name: github-reader
description: Read GitHub context.
tools: GitHub
---
Read the requested GitHub resources and report evidence.
`
	if err := os.WriteFile(filepath.Join(dir, "github-reader.md"), []byte(role), 0600); err != nil {
		t.Fatal(err)
	}
	roles, err := loadRoles(path, config.Default())
	if err != nil {
		t.Fatal(err)
	}
	if role, ok := agents.Find(roles, "github-reader"); !ok || len(role.Tools) != 1 || role.Tools[0] != "GitHub" {
		t.Fatalf("custom role: %+v, found %v", role, ok)
	}
}

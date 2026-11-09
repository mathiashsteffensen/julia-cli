package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"julia/agents"
	"julia/internal/config"
	"julia/internal/core"
	"julia/internal/session"
	"julia/internal/tools"
)

func TestBugsnagDoctorKeepsCredentialsPrivate(t *testing.T) {
	t.Setenv(tools.BugsnagTokenEnv, "")
	path := filepath.Join(t.TempDir(), "work.toml")
	authFile := bugsnagAuthFile(path)
	if got := bugsnagSummary(path); !strings.Contains(got, authFile) || !strings.Contains(got, tools.BugsnagTokenEnv) {
		t.Fatalf("setup instruction: %s", got)
	}

	token := "bugsnag_doctor_fixture_0123456789"
	for _, host := range []string{"api.bugsnag.com", "api.bugsnag.smartbear.com"} {
		data := fmt.Sprintf(`{"auth_token":%q,"endpoint":"https://%s"}`, token, host)
		if err := os.WriteFile(authFile, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		got := bugsnagSummary(path)
		if !strings.Contains(got, host) || !strings.Contains(got, "read-only") || strings.Contains(got, token) {
			t.Fatalf("configured host: %s", got)
		}
	}

	t.Setenv(tools.BugsnagTokenEnv, "invalid secret")
	got := bugsnagSummary(path)
	if !strings.Contains(got, "auth unavailable") || strings.Contains(got, "invalid secret") || strings.Contains(got, token) {
		t.Fatalf("invalid credential: %s", got)
	}
}

func TestBugsnagStartupRejectsInvalidCredentials(t *testing.T) {
	t.Setenv(tools.BugsnagTokenEnv, "")
	t.Setenv(tools.LinearTokenEnv, "")
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bugsnagAuthFile(path), []byte(`{"auth_token":"invalid secret"}`), 0600); err != nil {
		t.Fatal(err)
	}

	root := newCommand(path)
	root.SetArgs([]string{"--config", path, "--dir", dir, "--snapshot", "80", "--memory", "off"})
	err := root.ExecuteContext(context.Background())
	if err == nil || !strings.Contains(err.Error(), "bugsnag auth:") || strings.Contains(err.Error(), "invalid secret") {
		t.Fatalf("startup with invalid credentials: %v", err)
	}
}

func TestBugsnagCustomRoleUsesConfiguredCatalog(t *testing.T) {
	t.Setenv(tools.BugsnagTokenEnv, "")
	path := filepath.Join(t.TempDir(), "work.toml")
	if err := os.MkdirAll(rolesDir(path), 0700); err != nil {
		t.Fatal(err)
	}
	role := "---\nname: errors\ndescription: Inspect application errors.\ntools: Bugsnag\n---\nRead the reported error and its events.\n"
	if err := os.WriteFile(filepath.Join(rolesDir(path), "errors.md"), []byte(role), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadRoles(path, config.Default()); err == nil || !strings.Contains(err.Error(), `tool "Bugsnag"`) {
		t.Fatalf("unconfigured role should reject unavailable tool: %v", err)
	}

	if err := os.WriteFile(bugsnagAuthFile(path), []byte(`{"auth_token":"bugsnag_role_fixture_0123456789"}`), 0600); err != nil {
		t.Fatal(err)
	}
	roles, err := loadRoles(path, config.Default())
	if err != nil {
		t.Fatal(err)
	}
	got, ok := agents.Find(roles, "errors")
	if !ok || !slices.Equal(got.Tools, []string{"Bugsnag"}) {
		t.Fatalf("configured custom role: %+v", got)
	}
}

func TestRuntimeBugsnagReachesParentAndChildren(t *testing.T) {
	t.Setenv(tools.BugsnagTokenEnv, "bugsnag_runtime_fixture_0123456789")
	auth, err := tools.LoadBugsnagAuth("")
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Memory.Reflection = "off"
	cfg.Mode = "read-only"
	wantBugsnag := true
	var parentRuns, childRuns atomic.Int32
	backend := &questionBackend{run: func(ctx context.Context, s *core.Session, _ core.Prompt, executor core.ToolExecutor) error {
		if got := hasTool(executor.Definitions(), "Bugsnag"); got != wantBugsnag {
			return fmt.Errorf("Bugsnag in executor catalog: %v, want %v", got, wantBugsnag)
		}
		if s.ParentID != "" {
			childRuns.Add(1)
			if wantBugsnag && !slices.Equal(toolNames(executor.Definitions()), []string{"Bugsnag"}) {
				return fmt.Errorf("custom role gained tools: %v", toolNames(executor.Definitions()))
			}
			return nil
		}
		parentRuns.Add(1)
		result := executor.Execute(ctx, core.ToolCall{Name: "Subagent", Arguments: json.RawMessage(`{"tasks":[{"role":"errors","task":"Inspect the latest error"}]}`)})
		if result.IsError || strings.Contains(result.Content, `"error"`) {
			return fmt.Errorf("delegation: %s", result.Content)
		}
		return nil
	}}
	runner := &runtimeBackend{
		backend: backend, cfg: cfg, timeout: 30 * time.Second, parent: context.Background(),
		skillsDir: t.TempDir(),
		roles:     []agents.Role{{Name: "errors", Description: "Inspect application errors.", Prompt: "Read the reported error.", Tools: []string{"Bugsnag"}}},
		makeBackend: func(context.Context, config.Config) (core.Backend, core.Approver, error) {
			return backend, nil, nil
		},
	}
	if hasTool(runner.Definitions(), "Bugsnag") {
		t.Fatal("unconfigured runtime offers Bugsnag")
	}
	runner.bugsnag = auth
	if !hasTool(runner.Definitions(), "Bugsnag") {
		t.Fatal("configured runtime lacks Bugsnag")
	}
	s := session.New(t.TempDir(), "fake", "model")
	if err := runner.Run(context.Background(), s, core.Prompt{Text: "Inspect the error"}, nil, func(core.Event) {}); err != nil {
		t.Fatal(err)
	}

	cfg.DisabledTools = []string{"Bugsnag"}
	runner.switchProfile(cfg)
	wantBugsnag = false
	if hasTool(runner.Definitions(), "Bugsnag") {
		t.Fatal("disabled tool remains in runtime catalog")
	}
	if err := runner.Run(context.Background(), s, core.Prompt{Text: "Continue inspecting"}, nil, func(core.Event) {}); err != nil {
		t.Fatal(err)
	}
	if parentRuns.Load() != 2 || childRuns.Load() != 2 {
		t.Fatalf("parent runs %d, child runs %d, want 2 each", parentRuns.Load(), childRuns.Load())
	}
}

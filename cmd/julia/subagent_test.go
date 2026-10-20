package main

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"julia/agents"
	"julia/internal/config"
	"julia/internal/core"
	"julia/internal/session"
)

type delegationBackend struct{ parentID string }

func (b *delegationBackend) Name() string { return "fake" }
func (b *delegationBackend) Run(ctx context.Context, s *core.Session, prompt core.Prompt, executor core.ToolExecutor, emit core.Emit) error {
	if s.ParentID == "" {
		result := executor.Execute(ctx, core.ToolCall{Name: "Subagent", Arguments: json.RawMessage(`{"tasks":[{"role":"delegate","task":"delegated task one"},{"role":"delegate","task":"delegated task two"}]}`)})
		if result.IsError || strings.Contains(result.Content, `"error"`) {
			return errors.New(result.Content)
		}
		if !strings.Contains(result.Content, "child answer") {
			return errors.New("child response missing")
		}
		return nil
	}
	if s.ParentID != b.parentID || len(s.Messages) != 0 || s.RemoteID != "" || len(s.State) != 0 {
		return errors.New("child session not isolated")
	}
	if s.Model != "selected-model" || s.Reasoning != "high" || !strings.Contains(s.Instructions, "delegated subagent") {
		return errors.New("child settings/instructions lost")
	}
	for _, def := range executor.Definitions() {
		if def.Name == "Subagent" {
			return errors.New("recursive delegation exposed")
		}
	}
	result := executor.Execute(ctx, core.ToolCall{Name: "Write", Arguments: json.RawMessage(`{"path":"blocked.txt","content":"test"}`)})
	if !result.IsError {
		return errors.New("approval denial bypassed")
	}
	emit(core.Event{Kind: "text", Text: "private child text"})
	emit(core.Event{Kind: "status", Text: "child status"})
	s.Messages = append(s.Messages, core.Message{Role: "user", Content: prompt.Text}, core.Message{Role: "assistant", Content: "child answer"})
	return nil
}

func TestRuntimeSubagentsInheritSettingsAndAuthorization(t *testing.T) {
	cfg := config.Default()
	cfg.SkillPaths = []string{}
	cfg.Mode = "ask"
	s := session.New(t.TempDir(), "fake", "selected-model")
	s.Reasoning = "high"
	s.Messages = []core.Message{{Role: "user", Content: "original user request"}}
	backend := &delegationBackend{parentID: s.ID}
	var mu sync.Mutex
	saved := map[string]core.Session{}
	approvalCalls := 0
	runner := &runtimeBackend{backend: backend, cfg: cfg, roles: agents.Builtin(), timeout: 5 * time.Second, parent: context.Background(),
		makeBackend: func(_ context.Context, selected config.Config) (core.Backend, core.Approver, error) {
			if selected.Model != "selected-model" || selected.Reasoning != "high" || selected.Mode != "ask" {
				return nil, nil, errors.New("factory settings lost")
			}
			return backend, nil, nil
		},
		approve: func(_ context.Context, a core.Approval) (core.Decision, error) {
			if !strings.Contains(a.Task, "original user request") || !strings.Contains(a.Task, "current follow-up") || strings.Contains(a.Task, "delegated task") {
				return core.Decision{}, errors.New("wrong authorization context")
			}
			approvalCalls++
			return core.Decision{Allow: false, Reason: "test denial"}, nil
		},
		checkpoint: func(child *core.Session) error { mu.Lock(); defer mu.Unlock(); saved[child.ID] = *child; return nil },
	}
	// A child's events reach the parent stream only marked as the child's,
	// opened by subagent_start with the task and closed by subagent_end.
	var started, ended, childText int
	err := runner.Run(context.Background(), s, core.Prompt{Text: "current follow-up"}, nil, func(event core.Event) {
		if event.Subagent == nil {
			if event.Kind == "text" || event.Kind == "subagent_start" || event.Kind == "subagent_end" {
				t.Errorf("%s event reached the parent stream unmarked", event.Kind)
			}
			return
		}
		if event.Subagent.Role != "delegate" || event.Subagent.Model != "fake/selected-model" || event.Subagent.ID == "" {
			t.Errorf("child event names %+v", *event.Subagent)
		}
		switch event.Kind {
		case "subagent_start":
			started++
			if !strings.HasPrefix(event.Text, "delegated task") {
				t.Errorf("subagent_start carries %q, want the task", event.Text)
			}
		case "subagent_end":
			ended++
			if event.Text != "" {
				t.Errorf("a completed child ended with %q", event.Text)
			}
		case "text":
			childText++
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if started != 2 || ended != 2 || childText != 2 {
		t.Fatalf("started=%d ended=%d text=%d, want 2 each", started, ended, childText)
	}
	if approvalCalls != 2 || len(saved) != 2 {
		t.Fatalf("approvals=%d children=%d", approvalCalls, len(saved))
	}
	for _, child := range saved {
		if len(child.Audit) != 1 || len(child.Messages) != 2 || child.Role != "delegate" {
			t.Fatal("child final transcript not saved")
		}
	}
	for _, event := range s.Audit {
		if event.Event.Subagent != nil {
			t.Fatalf("a child's %s event was audited in the parent", event.Event.Kind)
		}
	}
	if err := selectSessionModel(&cfg, &core.Session{ParentID: s.ID}, s.Directory, "", "", false, true); err == nil {
		t.Fatal("child allowed to resume independently")
	}
}

// roleBackend delegates its tasks, then records what each child ran with: its
// session and its tools.
type roleBackend struct {
	tasks    string
	mu       sync.Mutex
	children map[string]roleChild // by task
	result   string
}

type roleChild struct {
	session core.Session
	tools   []string
}

func (b *roleBackend) Name() string { return "fake" }
func (b *roleBackend) Run(ctx context.Context, s *core.Session, prompt core.Prompt, executor core.ToolExecutor, _ core.Emit) error {
	if s.ParentID == "" {
		result := executor.Execute(ctx, core.ToolCall{Name: "Subagent", Arguments: json.RawMessage(b.tasks)})
		if result.IsError {
			return errors.New(result.Content)
		}
		b.result = result.Content
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.children[prompt.Text] = roleChild{session: *s, tools: toolNames(executor.Definitions())}
	s.Messages = append(s.Messages, core.Message{Role: "assistant", Content: "report on " + prompt.Text})
	return nil
}

func TestRuntimeSubagentRolesRunOnTheirModels(t *testing.T) {
	cfg := config.Default()
	cfg.SkillPaths = []string{}
	cfg.Providers = []config.ProviderConfig{{ID: "openrouter", BaseURL: "https://openrouter.ai/api/v1", APIKeyEnv: "ROUTER_KEY"}}
	cfg.EnabledModels = []config.ModelEntry{{ID: "openrouter/vendor/scout-model", Reasoning: "xhigh"}}
	cfg.Subagents = map[string]config.ModelEntry{
		"scout":    {ID: "openrouter/vendor/scout-model"},
		"reviewer": {ID: "codex/review-model", Reasoning: "low"},
		"oracle":   {ID: "codex/parent-model"},
	}
	backend := &roleBackend{children: map[string]roleChild{}, tasks: `{"tasks":[
		{"role":"scout","task":"map it"},
		{"role":"reviewer","task":"check it"},
		{"role":"oracle","task":"judge it"}]}`}
	var mu sync.Mutex
	made := map[string]config.Config{} // by provider/model
	runner := &runtimeBackend{backend: backend, cfg: cfg, roles: agents.Builtin(), timeout: 5 * time.Second, parent: context.Background(),
		makeBackend: func(_ context.Context, selected config.Config) (core.Backend, core.Approver, error) {
			mu.Lock()
			defer mu.Unlock()
			made[config.JoinModel(selected.Provider, selected.Model)] = selected
			return backend, nil, nil
		},
	}
	s := session.New(t.TempDir(), "codex", "parent-model")
	s.Reasoning = "medium"
	if err := runner.Run(context.Background(), s, core.Prompt{Text: "delegate"}, nil, func(core.Event) {}); err != nil {
		t.Fatal(err)
	}
	// The roles' tools, in the registry's order.
	readOnly := []string{"Read", "ViewImage", "List", "Grep", "Bash"}
	for _, tc := range []struct {
		task, role, provider, model, reasoning, keyEnv string
		tools                                          []string
	}{
		// The model's configured effort, and the key that belongs to the named provider.
		{"map it", "scout", "openrouter", "vendor/scout-model", "xhigh", "ROUTER_KEY", readOnly},
		// Codex has no key, and never takes the named provider's.
		{"check it", "reviewer", "codex", "review-model", "low", "", readOnly},
		// The parent's own model keeps the parent's effort.
		{"judge it", "oracle", "codex", "parent-model", "medium", "", readOnly},
	} {
		child, ok := backend.children[tc.task]
		if !ok {
			t.Fatalf("%s never ran", tc.role)
		}
		modelID := config.JoinModel(tc.provider, tc.model)
		settings, ok := made[modelID]
		if !ok || settings.Reasoning != tc.reasoning || settings.KeyEnv(settings.Provider) != tc.keyEnv {
			t.Errorf("%s: backend made with %+v, want %s at %s with key %s", tc.role, settings, modelID, tc.reasoning, tc.keyEnv)
		}
		if child.session.Provider != tc.provider || child.session.Model != tc.model || child.session.Reasoning != tc.reasoning {
			t.Errorf("%s: child session on %s/%s at %q", tc.role, child.session.Provider, child.session.Model, child.session.Reasoning)
		}
		role, _ := agents.Find(agents.Builtin(), tc.role)
		for _, want := range []string{`"model": "` + tc.model + `"`, `"reasoning_effort": "` + tc.reasoning + `"`, "## Subagent role: " + tc.role + "\n\n" + role.Prompt} {
			if !strings.Contains(child.session.Instructions, want) {
				t.Errorf("%s: instructions lack %q", tc.role, want)
			}
		}
		if child.session.Title != "Subagent "+tc.role+": "+tc.task {
			t.Errorf("%s: title %q", tc.role, child.session.Title)
		}
		if !slices.Equal(child.tools, tc.tools) {
			t.Errorf("%s: tools %v, want %v", tc.role, child.tools, tc.tools)
		}
		if !strings.Contains(backend.result, "("+tc.role+" on "+modelID+")") {
			t.Errorf("%s: result does not say who answered: %s", tc.role, backend.result)
		}
	}
}

// A child whose backend cannot be made, such as one whose provider's key is
// unset, fails with the reason before it is saved or announced.
func TestRuntimeSubagentThatCannotStartIsNeitherSavedNorAnnounced(t *testing.T) {
	t.Setenv("JULIA_TEST_UNSET_KEY", "")
	cfg := config.Default()
	cfg.SkillPaths = []string{}
	cfg.Providers = []config.ProviderConfig{{ID: "openrouter", BaseURL: "https://openrouter.ai/api/v1", APIKeyEnv: "JULIA_TEST_UNSET_KEY"}}
	cfg.Subagents = map[string]config.ModelEntry{"scout": {ID: "openrouter/vendor/m"}}
	backend := &roleBackend{children: map[string]roleChild{}, tasks: `{"tasks":[{"role":"scout","task":"map it"},{"role":"worker","task":"build it"}]}`}
	var mu sync.Mutex
	saved := map[string]string{}
	var statuses []string
	runner := &runtimeBackend{backend: backend, cfg: cfg, roles: agents.Builtin(), timeout: 5 * time.Second, parent: context.Background(),
		makeBackend: func(ctx context.Context, selected config.Config) (core.Backend, core.Approver, error) {
			if selected.Provider == "openrouter" {
				return newBackend(ctx, selected, nil)
			}
			return backend, nil, nil
		},
		checkpoint: func(child *core.Session) error {
			mu.Lock()
			defer mu.Unlock()
			saved[child.ID] = child.Title
			return nil
		},
	}
	s := session.New(t.TempDir(), "codex", "parent-model")
	err := runner.Run(context.Background(), s, core.Prompt{Text: "delegate"}, nil, func(event core.Event) {
		if event.Kind == "status" {
			mu.Lock()
			statuses = append(statuses, event.Text)
			mu.Unlock()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(backend.result, `"role":"scout","output":"","error":"openrouter/vendor/m: set JULIA_TEST_UNSET_KEY for the openrouter provider"`) {
		t.Fatalf("the scout's failure is not explained: %s", backend.result)
	}
	if len(saved) != 1 || !slices.ContainsFunc(slices.Collect(maps.Values(saved)), func(title string) bool { return title == "Subagent worker: build it" }) {
		t.Fatalf("saved children %v, want only the worker", saved)
	}
	var started []string
	for _, status := range statuses {
		if strings.HasPrefix(status, "Started ") {
			started = append(started, status)
		}
	}
	if len(started) != 1 || !strings.HasPrefix(started[0], "Started worker on codex/parent-model · subagent ") {
		t.Fatalf("started %v, want only the worker, role and model first", started)
	}
}

func TestSubagentInstructions(t *testing.T) {
	scout, _ := agents.Find(agents.Builtin(), "scout")
	worker, _ := agents.Find(agents.Builtin(), "worker")
	text := subagentInstructions(scout, "write")
	for _, want := range []string{"\n\n## Delegated subagent\n\nYou are a delegated subagent.", "not additional user authorization", "only the first 8000 bytes", "\n\n## Subagent role: scout\n\n" + scout.Prompt, "Your tools are Read, List, Grep, ViewImage, Bash."} {
		if !strings.Contains(text, want) {
			t.Errorf("scout instructions lack %q", want)
		}
	}
	if text := subagentInstructions(worker, "write"); strings.Contains(text, "Your tools are") || !strings.Contains(text, worker.Prompt) {
		t.Errorf("worker instructions: %q", text)
	}
	// A child hears of memory only as its run has it.
	for mode, want := range map[string]string{"write": "for the delegating agent to save", "read": "for the user to keep", "off": ""} {
		text := subagentInstructions(worker, mode)
		if want == "" && strings.Contains(text, "Memory") || want != "" && !strings.Contains(text, want) {
			t.Errorf("memory %s: %q", mode, text)
		}
	}
}

func TestSubagentSettings(t *testing.T) {
	cfg := config.Default()
	cfg.Provider, cfg.Model = "anthropic", "parent"
	cfg.Providers = []config.ProviderConfig{openRouter}
	cfg.EnabledModels = []config.ModelEntry{{ID: "openrouter/vendor/m", Reasoning: "high"}}
	cfg.Subagents = map[string]config.ModelEntry{
		"scout":    {ID: "openrouter/vendor/m"},
		"reviewer": {ID: "openrouter/vendor/m", Reasoning: "low"},
		"oracle":   {ID: "anthropic/parent"},
		"worker":   {ID: "openai/other"},
	}
	parent := cfg
	parent.Reasoning = ""
	for _, tc := range []struct{ role, provider, model, reasoning, keyEnv string }{
		{"delegate", "anthropic", "parent", "", "ANTHROPIC_API_KEY"},
		// Each provider reads its own key: Anthropic's never goes to OpenRouter
		// or OpenAI, nor OpenRouter's to a built-in provider.
		{"scout", "openrouter", "vendor/m", "high", "OPENROUTER_API_KEY"},
		{"reviewer", "openrouter", "vendor/m", "low", "OPENROUTER_API_KEY"},
		{"oracle", "anthropic", "parent", "", "ANTHROPIC_API_KEY"},
		{"worker", "openai", "other", "", "OPENAI_API_KEY"},
	} {
		got := subagentSettings(cfg, parent, tc.role)
		if got.Provider != tc.provider || got.Model != tc.model || got.Reasoning != tc.reasoning || got.KeyEnv(got.Provider) != tc.keyEnv {
			t.Errorf("%s: %s/%s at %q with key %q, want %s/%s at %q with key %q", tc.role, got.Provider, got.Model, got.Reasoning, got.KeyEnv(got.Provider), tc.provider, tc.model, tc.reasoning, tc.keyEnv)
		}
	}
}

func TestLoadRolesChecksAssignments(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	cfg := config.Default()
	cfg.Subagents = map[string]config.ModelEntry{"scout": {ID: "codex/x"}}
	roles, err := loadRoles(path, cfg)
	if err != nil || !slices.Equal(agents.Names(roles), agents.Names(agents.Builtin())) {
		t.Fatalf("built-in roles: %v, %v", agents.Names(roles), err)
	}
	cfg.Subagents["auditor"] = config.ModelEntry{ID: "codex/x"}
	if _, err := loadRoles(path, cfg); err == nil || !strings.Contains(err.Error(), `"auditor", which is not a subagent role`) {
		t.Fatalf("a model for a role that does not exist: %v", err)
	}
	os.Mkdir(rolesDir(path), 0o700)
	os.WriteFile(filepath.Join(rolesDir(path), "auditor.md"), []byte("---\nname: auditor\ndescription: Audits.\ntools: Read, Grep\n---\nAudit.\n"), 0o600)
	roles, err = loadRoles(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if role, ok := agents.Find(roles, "auditor"); !ok || role.Source != filepath.Join(rolesDir(path), "auditor.md") {
		t.Fatalf("the user's role was not loaded: %+v", role)
	}
	os.WriteFile(filepath.Join(rolesDir(path), "spy.md"), []byte("---\nname: spy\ndescription: Asks.\ntools: AskUserQuestion\n---\nAsk.\n"), 0o600)
	if _, err := loadRoles(path, cfg); err == nil || !strings.Contains(err.Error(), `role spy lists tool "AskUserQuestion"`) {
		t.Fatalf("a tool children do not have: %v", err)
	}
}

func TestRolesSummary(t *testing.T) {
	cfg := config.Default()
	cfg.Providers = []config.ProviderConfig{openRouter}
	cfg.EnabledModels = []config.ModelEntry{{ID: "openrouter/vendor/b", Reasoning: "xhigh"}}
	cfg.Subagents = map[string]config.ModelEntry{"scout": {ID: "openrouter/vendor/m", Reasoning: "low"}, "reviewer": {ID: "openrouter/vendor/b"}, "oracle": {ID: "openai/c"}}
	roles := append(agents.Builtin(), agents.Role{Name: "auditor", Source: "/home/me/.config/julia/agents/auditor.md"})
	got := strings.Join(rolesSummary(cfg, roles), "\n")
	for _, want := range []string{
		"Subagent  scout       openrouter/vendor/m · reasoning low",
		// The effort enabled_models gives the model.
		"Subagent  reviewer    openrouter/vendor/b · reasoning xhigh",
		"Subagent  oracle      openai/c\n",
		"Subagent  worker      the delegating agent's model",
		"Subagent  auditor     the delegating agent's model · /home/me/.config/julia/agents/auditor.md",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("summary lacks %q:\n%s", want, got)
		}
	}
}

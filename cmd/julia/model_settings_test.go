package main

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"julia/internal/config"
	"julia/internal/core"
	"julia/internal/provider"
	"julia/internal/session"
)

func settingsSession() *core.Session {
	return &core.Session{ID: "0123456789abcdef01234567", Directory: "/workspace", Provider: "codex", Model: "first-model", Reasoning: "medium", RemoteID: "remote-thread", State: json.RawMessage(`{"continuation":"keep"}`), PendingTool: &core.ToolCall{ID: "pending", Name: "Read", Arguments: json.RawMessage(`{"path":"README.md"}`)}, Messages: []core.Message{{Role: "user", Content: "Keep this conversation"}}, Audit: []core.AuditEntry{{At: time.Unix(1, 0).UTC(), Event: core.Event{Kind: "status", Text: "keep audit"}}}}
}
func TestApplySessionSettingsSaveFailureIsAtomic(t *testing.T) {
	current := settingsSession()
	before := *current
	cfg := config.Default()
	saveFailure := errors.New("disk full")
	saved := 0
	err := applySessionSettings(cfg, current, "codex", "second-model", "high", func(candidate *core.Session) error {
		saved++
		if candidate == current {
			t.Fatal("save received live session")
		}
		if candidate.Model != "second-model" || candidate.Reasoning != "high" || !candidate.ReasoningSet {
			t.Fatal("wrong candidate settings", candidate)
		}
		if !reflect.DeepEqual(*current, before) {
			t.Fatal("published settings before save succeeded")
		}
		return saveFailure
	})
	if !errors.Is(err, saveFailure) || saved != 1 {
		t.Fatal(err, saved)
	}
	if !reflect.DeepEqual(*current, before) {
		t.Fatal("failed save changed session or continuation state")
	}
}
func TestApplySessionSettingsPersistsSelectionAndPreservesCodexContinuation(t *testing.T) {
	current := settingsSession()
	before := *current
	store := &session.Store{Directory: t.TempDir()}
	defer store.Close()
	if err := applySessionSettings(config.Default(), current, "codex", "second-model", "", store.Save); err != nil {
		t.Fatal(err)
	}
	saved, err := store.Load(current.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Model != "second-model" || saved.Reasoning != "" || !saved.ReasoningSet {
		t.Fatal("explicit inherited effort selection not persisted", saved)
	}
	before.Model, before.Reasoning, before.ReasoningSet = "second-model", "", true
	if !reflect.DeepEqual(*current, before) {
		t.Fatal("model selection discarded continuation state")
	}
	if saved.RemoteID != before.RemoteID || !equalSettingsJSON(saved.State, before.State) || saved.PendingTool == nil || saved.PendingTool.ID != before.PendingTool.ID || saved.PendingTool.Name != before.PendingTool.Name || !equalSettingsJSON(saved.PendingTool.Arguments, before.PendingTool.Arguments) || !reflect.DeepEqual(saved.Messages, before.Messages) {
		t.Fatal("saved continuation state changed")
	}
}
func TestModelSettingsInvalidateOnlyTheReportedWindow(t *testing.T) {
	for _, tc := range []struct {
		name, provider, model string
		window                int
	}{
		{"reasoning only", "codex", "first-model", 1_000_000},
		{"model switch", "codex", "second-model", 0},
		{"provider switch", "claude", "first-model", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// An empty transcript also needs invalidation, without a new session.
			current := &core.Session{Provider: "codex", Model: "first-model", Usage: core.SessionUsage{
				Total: core.TokenUsage{Input: 123}, Context: 100, ContextWindow: 1_000_000,
			}}
			before := current.Usage
			var saved core.SessionUsage
			if err := applySessionSettings(config.Default(), current, tc.provider, tc.model, "high", func(s *core.Session) error {
				saved = s.Usage
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			want := before
			want.ContextWindow = tc.window
			if current.Usage != want || saved != want {
				t.Fatalf("model settings kept a stale window or changed usage: current %+v, saved %+v, want %+v", current.Usage, saved, want)
			}
		})
	}
}

func TestApplySessionSettingsValidationDoesNotPersist(t *testing.T) {
	for _, tt := range []struct{ name, provider, model, effort string }{{"surrounding model whitespace", "codex", " model", "high"}, {"model control character", "codex", "model\n", "high"}, {"invalid effort", "codex", "model", "BAD effort"}, {"API effort unsupported", "openai", "model", "high"}, {"API empty model", "openai", "", ""}, {"unconfigured named provider", "openrouter", "model", ""}} {
		t.Run(tt.name, func(t *testing.T) {
			current := settingsSession()
			current.Provider = tt.provider
			before := *current
			calls := 0
			err := applySessionSettings(config.Default(), current, tt.provider, tt.model, tt.effort, func(*core.Session) error { calls++; return nil })
			if err == nil || calls != 0 || !reflect.DeepEqual(*current, before) {
				t.Fatal("invalid setting changed session", err, calls)
			}
		})
	}
}
func TestIncompatibleModelSwitchStartsFreshAndPreservesPreviousSession(t *testing.T) {
	for _, tt := range []struct {
		name, fromProvider, toProvider, model, reasoning string
	}{
		{"OpenRouter model change", "openrouter", "openrouter", "new-model", ""},
		{"switch to OpenRouter", "codex", "openrouter", "vendor/model", ""},
		{"switch back to Codex", "openrouter", "codex", "codex-model", "medium"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			current := settingsSession()
			current.Provider, current.Model = tt.fromProvider, "old-model"
			before := *current
			store := &session.Store{Directory: t.TempDir()}
			defer store.Close()
			if err := applySessionSettings(openRouterConfig(), current, tt.toProvider, tt.model, tt.reasoning, store.Save); err != nil {
				t.Fatal(err)
			}
			if current.ID == before.ID || current.Provider != tt.toProvider || current.Model != tt.model || len(current.Messages) != 0 || len(current.State) != 0 || current.RemoteID != "" || current.PendingTool != nil {
				t.Fatal("model/provider switch did not start a clean session", current)
			}
			previous, err := store.Load(before.ID)
			if err != nil {
				t.Fatal(err)
			}
			if previous.Provider != before.Provider || previous.Model != before.Model || !equalSettingsJSON(previous.State, before.State) || !reflect.DeepEqual(previous.Messages, before.Messages) || !reflect.DeepEqual(previous.Audit, before.Audit) {
				t.Fatal("previous conversation was not preserved", previous)
			}
			if before.PendingTool != nil && (previous.PendingTool == nil || previous.PendingTool.ID != before.PendingTool.ID || previous.PendingTool.Name != before.PendingTool.Name || !equalSettingsJSON(previous.PendingTool.Arguments, before.PendingTool.Arguments)) {
				t.Fatal("previous pending tool was not preserved", previous.PendingTool)
			}
			fresh, err := store.Load(current.ID)
			if err != nil {
				t.Fatal(err)
			}
			if fresh.Provider != tt.toProvider || fresh.Model != tt.model || len(fresh.Messages) != 0 || len(fresh.State) != 0 {
				t.Fatal("new conversation was not persisted", fresh)
			}
		})
	}
}

// openRouter is the named provider tests configure for OpenRouter.
var openRouter = config.ProviderConfig{ID: "openrouter", BaseURL: "https://openrouter.ai/api/v1", APIKeyEnv: "OPENROUTER_API_KEY"}

// openRouterConfig is the default configuration with openRouter configured.
func openRouterConfig() config.Config {
	cfg := config.Default()
	cfg.Providers = []config.ProviderConfig{openRouter}
	return cfg
}

func TestNamedProviderReadsOnlyItsOwnAPIKey(t *testing.T) {
	cfg := openRouterConfig()
	cfg.Providers = append(cfg.Providers, config.ProviderConfig{ID: "local", BaseURL: "http://localhost:1234/v1"})
	cfg.Provider, cfg.Model = "openrouter", "provider/model"
	cfg.EnabledModels = []config.ModelEntry{{ID: "openrouter/provider/model"}}
	// Another provider's key is never a stand-in for the named provider's.
	t.Setenv("OPENROUTER_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "openai-key")
	if _, _, err := newBackend(context.Background(), cfg, nil); err == nil || !strings.Contains(err.Error(), "set OPENROUTER_API_KEY for the openrouter provider") {
		t.Fatal("OpenRouter startup did not report its missing key", err)
	}
	// Nor is a named provider's key a built-in provider's.
	t.Setenv("OPENROUTER_API_KEY", "router-key")
	t.Setenv("OPENAI_API_KEY", "")
	cfg.Provider = "openai"
	if _, _, err := newBackend(context.Background(), cfg, nil); err == nil || !strings.Contains(err.Error(), "set OPENAI_API_KEY for the openai provider") {
		t.Fatal("OpenAI startup did not report its missing key", err)
	}
	if key, err := apiKey(cfg, "openrouter"); err != nil || key != "router-key" {
		t.Fatal("the named provider's key", key, err)
	}
	// A provider without api_key_env is called keyless.
	cfg.Provider, cfg.Model = "local", "qwen/coder"
	backend, _, err := newBackend(context.Background(), cfg, nil)
	if err != nil || backend.Name() != "local" {
		t.Fatal("keyless provider did not start", backend, err)
	}
	if key, err := apiKey(cfg, "local"); err != nil || key != "" {
		t.Fatal("a keyless provider got a key", key, err)
	}
	if _, err := apiKey(cfg, "codex"); err == nil {
		t.Fatal("Codex has no API key to read")
	}
}

func TestProviderSwitchStartsAtTheModelsConfiguredReasoning(t *testing.T) {
	cfg := openRouterConfig()
	cfg.EnabledModels = []config.ModelEntry{{ID: "openrouter/vendor/model", Reasoning: "xhigh"}, {ID: "openrouter/vendor/plain"}}
	for _, tt := range []struct{ name, model, passed, want string }{
		{"configured effort survives the switch", "vendor/model", "xhigh", "xhigh"},
		{"configured effort is used when none is passed", "vendor/model", "", "xhigh"},
		{"the old provider's effort does not carry", "vendor/plain", "medium", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			current := settingsSession()
			if err := applySessionSettings(cfg, current, "openrouter", tt.model, tt.passed, nil); err != nil {
				t.Fatal(err)
			}
			if current.Provider != "openrouter" || current.Reasoning != tt.want || !current.ReasoningSet {
				t.Fatalf("provider switch set reasoning %q, want %q", current.Reasoning, tt.want)
			}
		})
	}
}

func TestUnchangedAPIModelKeepsContinuation(t *testing.T) {
	current := settingsSession()
	current.Provider, current.Model, current.Reasoning, current.ReasoningSet = "openrouter", "vendor/model", "", true
	before := *current
	if err := applySessionSettings(openRouterConfig(), current, "openrouter", current.Model, "", nil); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(*current, before) {
		t.Fatal("same API model should keep its current continuation")
	}
}
func TestResumePersistedTUIEffortOverridesConfigUnlessCLIExplicit(t *testing.T) {
	for _, tt := range []struct {
		name, saved, configured, cli, want string
		explicit                           bool
	}{{name: "saved TUI effort", saved: "high", configured: "medium", want: "high"}, {name: "saved inheritance", saved: "", configured: "medium", want: ""}, {name: "CLI wins", saved: "high", configured: "medium", cli: "low", explicit: true, want: "low"}, {name: "CLI inheritance wins", saved: "high", configured: "medium", cli: "", explicit: true, want: ""}} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.Reasoning = tt.configured
			if tt.explicit {
				cfg.Reasoning = tt.cli
			}
			current := &core.Session{Directory: "/workspace", Provider: "codex", Model: "saved-model", Reasoning: tt.saved, ReasoningSet: true}
			if err := selectSessionModel(&cfg, current, "/workspace", "", "", tt.explicit, true); err != nil {
				t.Fatal(err)
			}
			if cfg.Reasoning != tt.want || current.Reasoning != tt.want || !current.ReasoningSet {
				t.Fatal("settings precedence lost", cfg.Reasoning, current.Reasoning, tt.want)
			}
		})
	}
}

type settingsTurnBackend struct {
	t          *testing.T
	selected   config.Config
	agentCalls *[][2]string
}

func (b *settingsTurnBackend) Name() string { return "codex" }
func (b *settingsTurnBackend) Run(ctx context.Context, current *core.Session, _ core.Prompt, executor core.ToolExecutor, _ core.Emit) error {
	*b.agentCalls = append(*b.agentCalls, [2]string{b.selected.Model, b.selected.Reasoning})
	if current.Model != b.selected.Model || current.Reasoning != b.selected.Reasoning {
		b.t.Fatal("agent and live session settings differ")
	}
	for _, field := range []string{`"model": "` + b.selected.Model + `"`, `"reasoning_effort": "` + b.selected.Reasoning + `"`} {
		if !strings.Contains(current.Instructions, field) {
			b.t.Fatal("instructions do not describe selected settings", field)
		}
	}
	result := executor.Execute(ctx, core.ToolCall{Name: "Bash", Arguments: json.RawMessage(`{"command":"printf must-not-execute","network":true}`)})
	if !result.IsError {
		b.t.Fatal("fake reviewer denial should stop actual execution")
	}
	return nil
}
func TestRuntimeChangesAgentSettingsWithoutChangingFixedReviewer(t *testing.T) {
	cfg := config.Default()
	cfg.Model, cfg.Reasoning = "stale-default", "low"
	var factories, agents, reviewers [][2]string
	runner := &runtimeBackend{cfg: cfg, parent: context.Background(), timeout: 5 * time.Second, makeBackend: func(_ context.Context, selected config.Config) (core.Backend, core.Approver, error) {
		pair := [2]string{selected.Model, selected.Reasoning}
		factories = append(factories, pair)
		return &settingsTurnBackend{t: t, selected: selected, agentCalls: &agents}, func(context.Context, core.Approval) (core.Decision, error) {
			reviewers = append(reviewers, [2]string{provider.CodexReviewerModel, provider.CodexReviewerReasoning})
			return core.Decision{Allow: false, Reason: "test never executes a shell"}, nil
		}, nil
	}}
	current := &core.Session{Directory: t.TempDir(), Provider: "codex", Model: "first-model", Reasoning: "medium", ReasoningSet: true}
	emit := func(core.Event) {}
	if err := runner.Run(context.Background(), current, core.Prompt{Text: "First task"}, nil, emit); err != nil {
		t.Fatal(err)
	}
	if err := applySessionSettings(cfg, current, "codex", "second-model", "high", nil); err != nil {
		t.Fatal(err)
	}
	if err := runner.Run(context.Background(), current, core.Prompt{Text: "Second task"}, nil, emit); err != nil {
		t.Fatal(err)
	}
	want := [][2]string{{"first-model", "medium"}, {"second-model", "high"}}
	for _, got := range [][][2]string{factories, agents} {
		if !reflect.DeepEqual(got, want) {
			t.Fatal("per-turn settings diverged", got, want)
		}
	}
	wantReviewer := [][2]string{{provider.CodexReviewerModel, provider.CodexReviewerReasoning}, {provider.CodexReviewerModel, provider.CodexReviewerReasoning}}
	if !reflect.DeepEqual(reviewers, wantReviewer) {
		t.Fatal("main settings changed fixed reviewer", reviewers, wantReviewer)
	}
}

func equalSettingsJSON(left, right json.RawMessage) bool {
	var a, b any
	if json.Unmarshal(left, &a) != nil || json.Unmarshal(right, &b) != nil {
		return false
	}
	return reflect.DeepEqual(a, b)
}

package main

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"jarvis/internal/config"
	"jarvis/internal/core"
	"jarvis/internal/provider"
	"jarvis/internal/session"
)

func settingsSession() *core.Session {
	return &core.Session{ID: "0123456789abcdef01234567", Directory: "/workspace", Provider: "codex", Model: "first-model", Reasoning: "medium", RemoteID: "remote-thread", State: json.RawMessage(`{"continuation":"keep"}`), PendingTool: &core.ToolCall{ID: "pending", Name: "Read", Arguments: json.RawMessage(`{"path":"README.md"}`)}, Messages: []core.Message{{Role: "user", Content: "Keep this conversation"}}, Audit: []core.AuditEntry{{At: time.Unix(1, 0), Event: core.Event{Kind: "status", Text: "keep audit"}}}}
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
func TestApplySessionSettingsValidationDoesNotPersist(t *testing.T) {
	for _, tt := range []struct{ name, provider, model, effort string }{{"surrounding model whitespace", "codex", " model", "high"}, {"model control character", "codex", "model\n", "high"}, {"invalid effort", "codex", "model", "BAD effort"}, {"API effort unsupported", "compatible", "model", "high"}, {"API empty model", "compatible", "", ""}} {
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
		{"OpenRouter model change", "compatible", "compatible", "new-model", ""},
		{"switch to OpenRouter", "codex", "compatible", "openrouter/model", ""},
		{"switch back to Codex", "compatible", "codex", "codex-model", "medium"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			current := settingsSession()
			current.Provider, current.Model = tt.fromProvider, "old-model"
			before := *current
			store := &session.Store{Directory: t.TempDir()}
			defer store.Close()
			if err := applySessionSettings(config.Default(), current, tt.toProvider, tt.model, tt.reasoning, store.Save); err != nil {
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

func TestCompatibleCatalogRequiresItsConfiguredAPIKey(t *testing.T) {
	cfg := config.Default()
	cfg.Provider, cfg.Model, cfg.BaseURL = "compatible", "provider/model", "https://openrouter.ai/api/v1"
	cfg.APIKeyEnv, cfg.CompatibleModels = "OPENROUTER_API_KEY", []string{"provider/model"}
	t.Setenv("OPENROUTER_API_KEY", "")
	if _, _, err := newBackend(context.Background(), cfg, nil); err == nil || !strings.Contains(err.Error(), "OPENROUTER_API_KEY") {
		t.Fatal("OpenRouter startup did not report its missing key", err)
	}
}

func TestUnchangedAPIModelKeepsContinuation(t *testing.T) {
	current := settingsSession()
	current.Provider, current.Model, current.Reasoning, current.ReasoningSet = "compatible", "openrouter/model", "", true
	before := *current
	if err := applySessionSettings(config.Default(), current, "compatible", current.Model, "", nil); err != nil {
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
func (b *settingsTurnBackend) Run(ctx context.Context, current *core.Session, _ string, executor core.ToolExecutor, _ core.Emit) error {
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
	if err := runner.Run(context.Background(), current, "First task", nil, emit); err != nil {
		t.Fatal(err)
	}
	if err := applySessionSettings(cfg, current, "codex", "second-model", "high", nil); err != nil {
		t.Fatal(err)
	}
	if err := runner.Run(context.Background(), current, "Second task", nil, emit); err != nil {
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

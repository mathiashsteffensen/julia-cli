package ui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"jarvis/internal/core"
)

func settingsConfig() Config {
	return Config{
		Session: &core.Session{ID: "test", Directory: "/workspace", Provider: "codex", Model: "current", Reasoning: "high"},
		ApplySettings: func(s *core.Session, provider, model, reasoning string) error {
			s.Provider, s.Model, s.Reasoning, s.ReasoningSet = provider, model, reasoning, true
			return nil
		},
		Models: func(context.Context) ([]core.ModelOption, error) {
			return []core.ModelOption{
				{ID: "current", Name: "Current model", ReasoningEfforts: []string{"medium", "high"}, DefaultReasoning: "medium"},
				{ID: "next", Name: "Fast model", Description: "A different model", ReasoningEfforts: []string{"low", "medium"}, DefaultReasoning: "medium"},
			}, nil
		},
	}
}

func fetchPicker(t *testing.T, m *model, cmd tea.Cmd) {
	t.Helper()
	if cmd == nil {
		t.Fatal("missing asynchronous catalog command")
	}
	batch, ok := cmd().(tea.BatchMsg)
	if !ok || len(batch) < 2 {
		t.Fatal("catalog should load independently of cursor blinking")
	}
	m.Update(batch[len(batch)-1]())
}

func TestPickerPreservesDraftAndCancelsStaleCatalog(t *testing.T) {
	started, cancelled := make(chan struct{}), make(chan struct{})
	cfg := settingsConfig()
	cfg.Models = func(ctx context.Context) ([]core.ModelOption, error) {
		close(started)
		<-ctx.Done()
		close(cancelled)
		return nil, ctx.Err()
	}
	m := testModel(t, cfg)
	m.input.SetValue("Keep this\nmultiline draft")
	_, cmd := m.Update(tea.KeyPressMsg{Code: 'p', Mod: tea.ModCtrl})
	batch := cmd().(tea.BatchMsg)
	result := make(chan tea.Msg, 1)
	go func() { result <- batch[len(batch)-1]() }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("catalog did not start")
	}
	firstSequence := m.picker.sequence
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("Esc did not cancel catalog request")
	}
	if m.picker != nil || m.input.Value() != "Keep this\nmultiline draft" {
		t.Fatal("closing picker discarded draft")
	}
	m.Update(<-result)
	if len(m.models) != 0 {
		t.Fatal("closed picker accepted stale results")
	}
	m.cfg.Models = settingsConfig().Models
	m.openPicker("model")
	m.Update(modelsMsg{sequence: firstSequence, options: []core.ModelOption{{ID: "stale"}}})
	if !m.picker.loading || len(m.models) != 0 {
		t.Fatal("reopened picker accepted old request")
	}
}

func TestShiftTabCyclesSupportedReasoningEffortsAndPreservesDraft(t *testing.T) {
	m := testModel(t, settingsConfig())
	m.input.SetValue("Keep this draft")
	key := tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift}
	want := []string{"", "medium", "high", ""}
	for i, effort := range want {
		_, cmd := m.Update(key)
		if i == 0 {
			if cmd == nil {
				t.Fatal("first cycle did not load the Codex catalog")
			}
			_, cmd = m.Update(cmd())
		} else if cmd != nil {
			t.Fatal("cached catalog unexpectedly started another request")
		}
		if m.cfg.Session.Reasoning != effort || !m.cfg.Session.ReasoningSet {
			t.Fatalf("cycle %d selected effort %q (set=%v), want %q", i+1, m.cfg.Session.Reasoning, m.cfg.Session.ReasoningSet, effort)
		}
		if m.cfg.Session.Model != "current" || m.input.Value() != "Keep this draft" {
			t.Fatal("reasoning cycle changed the model or discarded the draft")
		}
	}
}

func TestShiftTabQueuesRepeatedPressesWhileCatalogLoads(t *testing.T) {
	m := testModel(t, settingsConfig())
	key := tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift}
	_, cmd := m.Update(key)
	if cmd == nil {
		t.Fatal("missing catalog request")
	}
	if _, queued := m.Update(key); queued != nil {
		t.Fatal("queued cycle started a duplicate catalog request")
	}
	m.Update(cmd())
	if m.cfg.Session.Reasoning != "medium" {
		t.Fatalf("queued cycles selected %q, want medium", m.cfg.Session.Reasoning)
	}
}

func TestShiftTabReportsCatalogFailureWithoutChangingSettings(t *testing.T) {
	cfg := settingsConfig()
	cfg.Models = func(context.Context) ([]core.ModelOption, error) { return nil, errors.New("catalog unavailable") }
	m := testModel(t, cfg)
	m.input.SetValue("Keep this draft")
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
	if cmd == nil {
		t.Fatal("missing catalog request")
	}
	m.Update(cmd())
	if m.cfg.Session.Reasoning != "high" || !strings.Contains(m.notice, "catalog unavailable") || m.input.Value() != "Keep this draft" {
		t.Fatal("catalog failure changed settings, lost feedback, or discarded the draft")
	}
}

func TestPickerSwitchesBetweenCodexAndSavedCompatibleModels(t *testing.T) {
	cfg := settingsConfig()
	cfg.CompatibleModels = []core.ModelOption{
		{ID: "vendor/or-model-a", Name: "Compatible A", Description: "Saved model"},
		{ID: "vendor/or-model-b", Name: "Compatible B"},
	}
	m := testModel(t, cfg)
	fetchPicker(t, m, m.openPicker("model"))
	if m.picker.provider != "codex" || len(m.pickerChoices()) != 2 {
		t.Fatal("picker did not open the live Codex catalog")
	}
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	fetchPicker(t, m, cmd)
	if m.picker.provider != "compatible" || len(m.pickerChoices()) != 2 {
		t.Fatal("Tab did not switch to saved compatible models")
	}
	m.input.SetValue("Keep this draft")
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.picker != nil || m.cfg.Session.Provider != "compatible" || m.cfg.Session.Model != "vendor/or-model-a" || m.cfg.Session.Reasoning != "" {
		t.Fatal("Compatible selection did not update provider/model or clear Codex reasoning")
	}
	if m.input.Value() != "Keep this draft" {
		t.Fatal("provider selection discarded the draft")
	}
	fetchPicker(t, m, m.openPicker("model"))
	_, cmd = m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	fetchPicker(t, m, cmd)
	if m.picker.provider != "codex" || len(m.pickerChoices()) != 2 {
		t.Fatal("Tab did not switch back to Codex models")
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.cfg.Session.Provider != "codex" || m.cfg.Session.Model != "current" || m.cfg.Session.Reasoning != "medium" {
		t.Fatal("Codex selection did not restore its model and default effort")
	}
}

func TestCompatibleModelStartsAtItsConfiguredReasoning(t *testing.T) {
	cfg := settingsConfig()
	cfg.CompatibleReasoning = true
	cfg.CompatibleModels = []core.ModelOption{{ID: "vendor/a", Name: "vendor/a", DefaultReasoning: "high"}, {ID: "vendor/b", Name: "vendor/b"}}
	m := testModel(t, cfg)
	if err := m.applySettings("compatible", "vendor/a", ""); err != nil {
		t.Fatal(err)
	}
	if m.cfg.Session.Reasoning != "high" {
		t.Fatal("switching providers ignored the model's reasoning", m.cfg.Session.Reasoning)
	}
	if err := m.applySettings("compatible", "vendor/b", "high"); err != nil {
		t.Fatal(err)
	}
	if m.cfg.Session.Reasoning != "" {
		t.Fatal("an effort carried to a model without one", m.cfg.Session.Reasoning)
	}
	if err := m.applySettings("compatible", "vendor/a", ""); err != nil {
		t.Fatal(err)
	}
	if m.cfg.Session.Reasoning != "high" {
		t.Fatal("switching models ignored the model's reasoning", m.cfg.Session.Reasoning)
	}
}

func TestCompatibleReasoningChoicesAreGenericAndUnverified(t *testing.T) {
	const modelID = "vendor/model"
	cfg := settingsConfig()
	cfg.Session.Provider, cfg.Session.Model, cfg.Session.Reasoning = "compatible", modelID, "high"
	cfg.CompatibleReasoning = true
	cfg.CompatibleModels = []core.ModelOption{{ID: modelID, Name: modelID}}
	m := testModel(t, cfg)
	fetchPicker(t, m, m.openPicker("reasoning"))

	choices := m.pickerChoices()
	want := []string{"", "none", "minimal", "low", "medium", "high", "xhigh", "max"}
	if len(choices) != len(want) {
		t.Fatalf("got %d generic reasoning choices, want %d: %#v", len(choices), len(want), choices)
	}
	for i, value := range want {
		if choices[i].value != value {
			t.Fatalf("choice %d is %q, want %q", i, choices[i].value, value)
		}
		if i > 0 && !strings.Contains(choices[i].detail, "unverified") {
			t.Fatalf("choice %q does not disclose model support is unverified: %q", value, choices[i].detail)
		}
	}
	if option := m.currentModelOption(); option == nil || len(option.ReasoningEfforts) != 0 {
		t.Fatal("generic choices were incorrectly added to the model-advertised catalog")
	}

	// The keyboard cycle uses the same explicit generic choices without claiming
	// that any effort is supported by this particular model.
	m.closePicker()
	m.cfg.Session.Reasoning = ""
	m.cycleReasoning()
	if m.cfg.Session.Reasoning != "none" {
		t.Fatalf("generic reasoning cycle selected %q, want none", m.cfg.Session.Reasoning)
	}
}

func TestPickerSearchAndApplySelectSupportedEffort(t *testing.T) {
	for _, effort := range []string{"high", ""} {
		m := testModel(t, settingsConfig())
		m.cfg.Session.Reasoning = effort
		m.input.SetValue("A draft for the selected model")
		fetchPicker(t, m, m.openPicker("model"))
		m.Update(tea.PasteMsg{Content: "fast"})
		if choices := m.pickerChoices(); len(choices) != 1 || choices[0].value != "next" {
			t.Fatalf("search failed: %#v", choices)
		}
		m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		if m.picker != nil || m.cfg.Session.Model != "next" || m.cfg.Session.Reasoning != "medium" || !m.cfg.Session.ReasoningSet {
			t.Fatal("selection did not apply target model default")
		}
		if m.input.Value() != "A draft for the selected model" || !strings.Contains(m.notice, "adjusted to medium") {
			t.Fatal("lost draft or effort adjustment notice")
		}
	}
}

func TestProviderSwitchClearsVisibleHistoryAfterStartingNewSession(t *testing.T) {
	cfg := settingsConfig()
	cfg.Session.Messages = []core.Message{{Role: "user", Content: "old task"}}
	cfg.ApplySettings = func(s *core.Session, provider, model, reasoning string) error {
		*s = core.Session{ID: "new-session", Directory: s.Directory, Provider: provider, Model: model, Reasoning: reasoning, ReasoningSet: true}
		return nil
	}
	m := testModel(t, cfg)
	m.entries = []entry{{role: "user", text: "old task"}}
	m.toolCount, m.inputTokens, m.outputTokens, m.lastDuration = 2, 10, 20, time.Minute
	m.cfg.Session.Usage = core.SessionUsage{Total: core.TokenUsage{Input: 5, CacheRead: 6, Output: 7}, Context: 8}
	m.live = core.SessionUsage{Total: core.TokenUsage{Input: 1}, Context: 9}
	if err := m.applySettings("compatible", "vendor/model", ""); err != nil {
		t.Fatal(err)
	}
	if m.cfg.Session.ID != "new-session" || m.cfg.Session.Provider != "compatible" || len(m.cfg.Session.Messages) != 0 || len(m.entries) != 0 || m.toolCount != 0 || m.inputTokens != 0 || m.outputTokens != 0 || m.lastDuration != 0 {
		t.Fatal("provider switch did not isolate the new conversation")
	}
	if m.usage() != (core.SessionUsage{}) {
		t.Fatalf("new conversation inherited token usage: %+v", m.usage())
	}
	if !strings.Contains(m.notice, "Previous conversation saved") {
		t.Fatal("provider switch did not explain where the prior conversation went")
	}
}

func TestPickerPersistenceErrorLeavesLiveSessionAndDraft(t *testing.T) {
	cfg := settingsConfig()
	cfg.ApplySettings = func(s *core.Session, provider, model, reasoning string) error {
		s.Provider, s.Model, s.Reasoning = provider, model, reasoning // Even a callback failure cannot mutate live state.
		s.Messages = append(s.Messages, core.Message{Content: "uncommitted"})
		return errors.New("disk full")
	}
	m := testModel(t, cfg)
	m.input.SetValue("Keep me")
	fetchPicker(t, m, m.openPicker("model"))
	m.picker.selected = 1
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.cfg.Session.Model != "current" || m.cfg.Session.Reasoning != "high" || len(m.cfg.Session.Messages) != 0 {
		t.Fatal("failed persistence changed session")
	}
	if m.picker == nil || !strings.Contains(m.picker.err, "disk full") || m.input.Value() != "Keep me" {
		t.Fatal("error lost picker or draft")
	}
}

func TestBusyAndApprovalKeepSettingsKeysAwayFromComposer(t *testing.T) {
	m := testModel(t, settingsConfig())
	m.busy = true
	m.input.SetValue("/model next")
	m.submit()
	m.Update(tea.KeyPressMsg{Code: 'p', Mod: tea.ModCtrl})
	m.Update(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
	if m.picker != nil || m.cfg.Session.Model != "current" || m.cfg.Session.Reasoning != "high" || m.input.Value() != "/model next" || !strings.Contains(m.notice, "Draft kept") {
		t.Fatal("busy setting request changed state")
	}
	request := ApprovalRequest{Reply: make(chan core.Decision, 1)}
	m.pending = &request
	m.Update(tea.KeyPressMsg{Code: 'p', Mod: tea.ModCtrl})
	if decision := <-request.Reply; decision.Allow {
		t.Fatal("settings shortcut approved instead of denying")
	}
	if m.picker != nil || m.input.Value() != "/model next" {
		t.Fatal("approval changed settings or consumed draft")
	}
}

func TestDirectSettingsCommandsAndExplicitDefault(t *testing.T) {
	m := testModel(t, settingsConfig())
	m.cfg.Models = nil // Manual settings remain available without metadata.
	for _, step := range []struct{ command, model, effort string }{
		{"/model future-model", "future-model", "high"},
		{"/reasoning ultra", "future-model", "ultra"},
		{"/reasoning default", "future-model", ""},
	} {
		m.input.SetValue(step.command)
		m.submit()
		if m.cfg.Session.Model != step.model || m.cfg.Session.Reasoning != step.effort || !m.cfg.Session.ReasoningSet || m.input.Value() != "" {
			t.Fatalf("direct command failed: %s", step.command)
		}
	}
	m.input.SetValue("/clear")
	m.submit()
	if !m.cfg.Session.ReasoningSet || m.cfg.Session.Reasoning != "" {
		t.Fatal("clear lost explicit default choice")
	}
	m.cfg.Session.Provider = "openai"
	m.input.SetValue("/reasoning high")
	m.submit()
	if m.cfg.Session.Reasoning != "" || !strings.Contains(m.notice, "unavailable with this provider") {
		t.Fatal("reasoning silently accepted by API provider")
	}
}

func TestCatalogErrorDoesNotChangeSettingsAndManualCommandStillWorks(t *testing.T) {
	cfg := settingsConfig()
	cfg.Models = func(context.Context) ([]core.ModelOption, error) { return nil, errors.New("catalog unavailable") }
	m := testModel(t, cfg)
	fetchPicker(t, m, m.openPicker("model"))
	if m.cfg.Session.Model != "current" || m.picker.err == "" {
		t.Fatal("catalog error changed settings or was hidden")
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	m.input.SetValue("/model next")
	m.submit()
	if m.cfg.Session.Model != "next" {
		t.Fatal("catalog error prevented direct setting")
	}
}

func TestDemoSettingsNeverCallExternalCallbacks(t *testing.T) {
	cfg := settingsConfig()
	cfg.Demo = true
	cfg.Models = func(context.Context) ([]core.ModelOption, error) { t.Fatal("demo fetched catalog"); return nil, nil }
	cfg.ApplySettings = func(*core.Session, string, string, string) error { t.Fatal("demo persisted settings"); return nil }
	m := testModel(t, cfg)
	m.openPicker("model")
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.cfg.Session.Model != "preview-balanced" || !m.cfg.Session.ReasoningSet {
		t.Fatal("offline selection failed")
	}
}

func TestPickerFitsNarrowTerminalAndSanitizesCatalog(t *testing.T) {
	for _, size := range [][2]int{{30, 15}, {40, 15}, {50, 20}, {100, 30}, {128, 40}} {
		m := testModel(t, settingsConfig())
		m.resize(size[0], size[1])
		fetchPicker(t, m, m.openPicker("model"))
		m.models[1].Description = "\x1b]52;c;c2VjcmV0\a" + strings.Repeat("long description ", 30)
		m.models[1].ID += "\n\u202e"
		m.picker.selected = 1
		view := m.View().Content
		if strings.Contains(view, "c2VjcmV0") || strings.ContainsRune(view, '\u202e') {
			t.Fatal("unsafe catalog text reached terminal")
		}
		lines := strings.Split(view, "\n")
		if len(lines) > size[1] {
			t.Fatalf("%v: height overflow", size)
		}
		for _, line := range lines {
			if ansi.StringWidth(line) > size[0] {
				t.Fatalf("%v: width overflow", size)
			}
		}
		if !strings.Contains(ansi.Strip(view), "enter") || !strings.Contains(ansi.Strip(view), "esc back") || !strings.Contains(ansi.Strip(view), "SEARCH") {
			t.Fatalf("%v: picker controls hidden", size)
		}
	}
}

func TestPickerLoadingAndEmptyStatesFitSmallTerminal(t *testing.T) {
	m := testModel(t, settingsConfig())
	m.resize(30, 15)
	m.openPicker("model")
	for _, loading := range []bool{true, false} {
		m.picker.loading = loading
		m.picker.search.SetValue("no matching option")
		view := ansi.Strip(m.View().Content)
		if !strings.Contains(view, "SEARCH") || !strings.Contains(view, "esc back") || len(strings.Split(view, "\n")) > 15 {
			t.Fatal("small picker lost search or exit controls")
		}
	}
}

type settingsBackend struct{ received chan core.Session }

func (b settingsBackend) Name() string { return "fake" }
func (b settingsBackend) Run(_ context.Context, s *core.Session, _ core.Prompt, _ core.ToolExecutor, _ core.Emit) error {
	b.received <- *s
	return nil
}

func TestNextTurnReceivesChosenSettings(t *testing.T) {
	cfg := settingsConfig()
	b := settingsBackend{received: make(chan core.Session, 1)}
	cfg.Backend = b
	m := testModel(t, cfg)
	m.input.SetValue("/model next")
	m.submit()
	m.input.SetValue("/reasoning medium")
	m.submit()
	m.input.SetValue("Do the next task")
	m.submit()
	select {
	case s := <-b.received:
		if s.Model != "next" || s.Reasoning != "medium" || !s.ReasoningSet {
			t.Fatal("backend received old settings")
		}
	case <-time.After(time.Second):
		t.Fatal("next turn never started")
	}
}

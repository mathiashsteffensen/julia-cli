package ui

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"julia/internal/config"
	"julia/internal/core"
	"julia/internal/throughput"
)

type settingsPicker struct {
	kind     string
	search   textinput.Model
	selected int
	loading  bool
	err      string
	sequence uint64
	cancel   context.CancelFunc
}

type modelsMsg struct {
	sequence uint64
	options  []core.ModelOption
	err      error
}

type reasoningCycleMsg struct {
	sequence  uint64
	sessionID string
	model     string
	options   []core.ModelOption
	err       error
}

type settingChoice struct {
	value, label, detail string
	// note is a short figure set against the label's right edge, such as a
	// model's recorded speed.
	note   string
	active bool
}

func (m *model) openPicker(kind string) tea.Cmd {
	if m.busy {
		m.notice = "Draft kept. Wait for this task, or Esc to interrupt before changing settings."
		return nil
	}
	if kind == "reasoning" && !m.reasoningSettings() {
		m.notice = "Reasoning controls are unavailable with this provider."
		return nil
	}
	if m.cfg.ApplySettings == nil && !m.cfg.Demo {
		m.notice = "Settings are unavailable in this session."
		return nil
	}
	m.invalidateReasoningCycle()
	m.closePicker()
	m.rememberModel(m.cfg.Session.Provider, m.cfg.Session.Model)
	placeholder := "Search models by name or ID"
	if kind == "reasoning" {
		placeholder = "Filter reasoning levels"
	}
	m.picker = &settingsPicker{kind: kind, search: m.pickerSearch(placeholder)}
	m.input.Blur()
	m.notice = ""
	if m.cfg.Demo {
		m.models = mergeCatalog(m.cfg.EnabledModels, previewModelOptions(m.cfg.Session.Provider))
		m.throughput = previewThroughput(m.cfg.Session.Provider, m.now)
		return textinput.Blink
	}
	if m.cfg.Throughput != nil {
		m.throughput = m.cfg.Throughput()
	}
	return m.loadPickerModels(true)
}

// pickerSearch is a picker's focused search field.
func (m *model) pickerSearch(placeholder string) textinput.Model {
	search := textinput.New()
	search.Prompt = "› "
	search.Placeholder = placeholder
	search.SetStyles(searchStyles())
	search.CharLimit = 256
	search.SetVirtualCursor(true)
	search.SetWidth(max(8, m.mainWidth-8))
	search.Focus()
	return search
}

// rememberModel keeps session models available when selection is unrestricted.
// A nonempty enabled_models list still filters them out of the model picker.
func (m *model) rememberModel(provider, model string) {
	if model == "" {
		return
	}
	for _, option := range m.knownModels {
		if option.Provider == provider && option.ID == model {
			return
		}
	}
	m.knownModels = append(m.knownModels, core.ModelOption{Provider: provider, ID: model, Name: model, Description: "Current model · not in enabled_models"})
}

// catalogFetch lists every model the picker offers in one list: the enabled
// and remembered models first, then the rest of Codex's live catalog, whose
// efforts also fill in the enabled Codex models. A Codex catalog failure still
// returns the other models alongside the error.
func (m *model) catalogFetch() (func(context.Context) ([]core.ModelOption, error), error) {
	known := append([]core.ModelOption(nil), m.knownModels...)
	codex := m.cfg.Models
	if codex == nil && len(known) == 0 {
		return nil, fmt.Errorf("no models to choose from; add provider/<model-id> entries to enabled_models in your JULIA config")
	}
	return func(ctx context.Context) ([]core.ModelOption, error) {
		if codex == nil {
			return known, nil
		}
		catalog, err := codex(ctx)
		if err != nil {
			return known, err
		}
		return mergeCatalog(known, catalog), nil
	}, nil
}

// mergeCatalog adds the CLI providers' catalogs to the known models; an entry
// without a provider is Codex's. A known model takes the catalog's details and
// efforts but keeps its own starting effort.
func mergeCatalog(known, catalog []core.ModelOption) []core.ModelOption {
	options := append([]core.ModelOption(nil), known...)
	for _, entry := range catalog {
		if entry.Provider == "" {
			entry.Provider = "codex"
		}
		if option := modelOption(options, entry.Provider, entry.ID); option != nil && option.ID == entry.ID {
			reasoning := option.DefaultReasoning
			*option = entry
			if reasoning != "" {
				option.DefaultReasoning = reasoning
			}
			continue
		}
		options = append(options, entry)
	}
	return options
}

func (m *model) loadPickerModels(blink bool) tea.Cmd {
	if m.picker == nil {
		return nil
	}
	if m.picker.cancel != nil {
		m.picker.cancel()
		m.picker.cancel = nil
	}
	m.pickerSequence++
	m.picker.sequence = m.pickerSequence
	m.picker.err = ""
	m.models = nil
	fetch, err := m.catalogFetch()
	if err != nil {
		m.picker.loading = false
		m.stopPickerSpinner()
		if m.picker.kind == "reasoning" {
			m.picker.err = "Effort catalog unavailable. Esc, then use /reasoning LEVEL."
		} else {
			m.picker.err = oneLine(err.Error()) + ". Esc, then use /model ID."
		}
		if blink {
			return textinput.Blink
		}
		return nil
	}
	m.picker.loading = true
	ctx, cancel := context.WithTimeout(m.lifetime, 30*time.Second)
	m.picker.cancel = cancel
	sequence := m.picker.sequence
	load := func() tea.Msg {
		defer cancel()
		options, err := fetch(ctx)
		options = append([]core.ModelOption(nil), options...)
		for i := range options {
			options[i].ReasoningEfforts = append([]string(nil), options[i].ReasoningEfforts...)
		}
		return modelsMsg{sequence: sequence, options: options, err: err}
	}
	if blink {
		return tea.Batch(textinput.Blink, m.spinner.start(), load)
	}
	return tea.Batch(m.spinner.start(), load)
}

func (m *model) reasoningSettings() bool {
	return m.reasoningSettingsFor(m.cfg.Session.Provider)
}

func (m *model) reasoningSettingsFor(provider string) bool {
	return m.cfg.Demo || showsReasoning(provider, m.cfg.NamedProviders)
}

func (m *model) closePicker() {
	if m.picker != nil && m.picker.cancel != nil {
		m.picker.cancel()
	}
	m.picker = nil
	m.stopPickerSpinner()
	m.input.Focus()
}

// stopPickerSpinner ends the picker's loading animation. The spinner is shared
// with the running task's activity stream, so while a task is busy it is the
// task's, and the picker leaves it alone: opening the fleet, or anything else
// that closes the picker, must not freeze the stream.
func (m *model) stopPickerSpinner() {
	if !m.busy {
		m.spinner.stop()
	}
}

func (m *model) receiveModels(msg modelsMsg) tea.Cmd {
	if m.picker == nil || m.picker.sequence != msg.sequence || m.quitting {
		return nil
	}
	m.picker.loading = false
	m.stopPickerSpinner()
	m.picker.cancel = nil
	if msg.err != nil {
		m.picker.err = "Could not load models: " + oneLine(msg.err.Error()) + ". Esc to use a direct command."
		if len(msg.options) == 0 {
			return nil
		}
	}
	m.models = msg.options
	m.picker.selected = 0
	if m.picker.search.Value() == "" {
		for i, choice := range m.pickerChoices() {
			if choice.active {
				m.picker.selected = i
				break
			}
		}
	}
	return nil
}

func (m *model) currentModelOption() *core.ModelOption {
	return modelOption(m.models, m.cfg.Session.Provider, m.cfg.Session.Model)
}

// modelOption finds a provider's model; an empty model is its default.
func modelOption(options []core.ModelOption, provider, model string) *core.ModelOption {
	for i := range options {
		if options[i].Provider == provider && (options[i].ID == model || (model == "" && options[i].IsDefault)) {
			return &options[i]
		}
	}
	return nil
}

func previewModelOptions(provider string) []core.ModelOption {
	return []core.ModelOption{
		{Provider: provider, ID: "preview-balanced", Name: "Balanced", Description: "Offline example · everyday coding and research", ReasoningEfforts: []string{"low", "medium", "high"}, DefaultReasoning: "medium", IsDefault: true},
		{Provider: provider, ID: "preview-deep", Name: "Deep reasoning", Description: "Offline example · demanding, extended tasks", ReasoningEfforts: []string{"medium", "high"}, DefaultReasoning: "high"},
	}
}

// previewThroughput gives the offline models a week of made-up speed, and one
// of the enabled models an older record, as the picker shows them for real.
func previewThroughput(provider string, now time.Time) map[string]throughput.Stats {
	return map[string]throughput.Stats{
		config.JoinModel(provider, "preview-balanced"): {TokensPerSecond: 64.2, Calls: 38, LastUsed: now.Add(-2 * time.Hour)},
		config.JoinModel(provider, "preview-deep"):     {TokensPerSecond: 23.8, Calls: 7, LastUsed: now.Add(-25 * 24 * time.Hour)},
	}
}

// throughputNote is a model's speed as the picker shows it: tokens per second
// over its recorded calls, and how long ago it was last used once that is
// beyond the week the record covers.
func throughputNote(stats throughput.Stats, now time.Time) string {
	speed := fmt.Sprintf("%.0f", stats.TokensPerSecond)
	if stats.TokensPerSecond < 10 {
		speed = fmt.Sprintf("%.1f", stats.TokensPerSecond)
	}
	calls := "calls"
	if stats.Calls == 1 {
		calls = "call"
	}
	note := fmt.Sprintf("%s tok/s · %d %s", speed, stats.Calls, calls)
	if idle := now.Sub(stats.LastUsed); idle > throughput.Window {
		note += " · " + roughAge(idle) + " ago"
	}
	return note
}

// roughAge is a duration in whole days, weeks or months, as "12d".
func roughAge(d time.Duration) string {
	days := int(d.Hours() / 24)
	switch {
	case days >= 60:
		return fmt.Sprintf("%dmo", days/30)
	case days >= 14:
		return fmt.Sprintf("%dw", days/7)
	default:
		return fmt.Sprintf("%dd", max(days, 1))
	}
}

func (m *model) cycleReasoning() tea.Cmd {
	if m.busy {
		m.notice = "Draft kept. Wait for this task, or Esc to interrupt before changing settings."
		return nil
	}
	if !m.reasoningSettings() {
		m.notice = "Reasoning controls are unavailable with this provider."
		return nil
	}
	if m.reasoningCyclePending > 0 {
		m.reasoningCyclePending++
		return nil
	}
	if m.cfg.Demo && m.currentModelOption() == nil {
		m.models = previewModelOptions(m.cfg.Session.Provider)
	}
	if m.cfg.NamedProviders[m.cfg.Session.Provider] {
		if err := m.cycleReasoningBy(1); err != nil {
			m.notice = "Settings unchanged: " + oneLine(err.Error())
		}
		return nil
	}
	if option := m.currentModelOption(); option != nil {
		if len(option.ReasoningEfforts) == 0 {
			m.notice = "No reasoning efforts are listed for this model. Use /reasoning LEVEL to set one directly."
			return nil
		}
		if err := m.cycleReasoningBy(1); err != nil {
			m.notice = "Settings unchanged: " + oneLine(err.Error())
		}
		return nil
	}
	if m.cfg.Models == nil {
		m.notice = "Reasoning catalog unavailable. Use /reasoning LEVEL to set one directly."
		return nil
	}
	if m.cfg.ApplySettings == nil && !m.cfg.Demo {
		m.notice = "Settings are unavailable in this session."
		return nil
	}
	m.reasoningCycleSequence++
	sequence := m.reasoningCycleSequence
	sessionID, modelID := m.cfg.Session.ID, m.cfg.Session.Model
	m.reasoningCyclePending = 1
	m.notice = "Loading reasoning options…"
	ctx, cancel := context.WithTimeout(m.lifetime, 30*time.Second)
	m.reasoningCycleCancel = cancel
	return func() tea.Msg {
		defer cancel()
		options, err := m.cfg.Models(ctx)
		options = append([]core.ModelOption(nil), options...)
		for i := range options {
			if options[i].Provider == "" {
				options[i].Provider = "codex"
			}
			options[i].ReasoningEfforts = append([]string(nil), options[i].ReasoningEfforts...)
		}
		return reasoningCycleMsg{sequence: sequence, sessionID: sessionID, model: modelID, options: options, err: err}
	}
}

func (m *model) receiveReasoningCycle(msg reasoningCycleMsg) tea.Cmd {
	if msg.sequence != m.reasoningCycleSequence || m.reasoningCyclePending == 0 {
		return nil
	}
	steps := m.reasoningCyclePending
	m.reasoningCyclePending = 0
	m.reasoningCycleCancel = nil
	if m.quitting || m.busy || m.picker != nil || msg.sessionID != m.cfg.Session.ID || msg.model != m.cfg.Session.Model {
		if m.notice == "Loading reasoning options…" {
			m.notice = ""
		}
		return nil
	}
	if msg.err != nil {
		m.notice = "Could not load reasoning options: " + oneLine(msg.err.Error())
		return nil
	}
	m.models = msg.options
	if m.currentModelOption() == nil {
		m.notice = "Current model is missing from the reasoning catalog. Use /reasoning LEVEL to set one directly."
		return nil
	}
	if err := m.cycleReasoningBy(steps); err != nil {
		m.notice = "Settings unchanged: " + oneLine(err.Error())
	}
	return nil
}

func (m *model) cycleReasoningBy(steps int) error {
	var efforts []string
	if m.cfg.NamedProviders[m.cfg.Session.Provider] {
		efforts = config.OpenRouterReasoningEfforts()
	} else if option := m.currentModelOption(); option != nil {
		efforts = option.ReasoningEfforts
	}
	if len(efforts) == 0 {
		return fmt.Errorf("no reasoning efforts are listed for this model")
	}
	choices := []string{""}
	seen := map[string]bool{"": true}
	for _, effort := range efforts {
		if effort != "" && !seen[effort] {
			choices = append(choices, effort)
			seen[effort] = true
		}
	}
	if len(choices) < 2 {
		return fmt.Errorf("no reasoning efforts are listed for this model")
	}
	current := slices.Index(choices, m.cfg.Session.Reasoning)
	if current < 0 {
		current = 0
	}
	next := choices[(current+steps%len(choices))%len(choices)]
	return m.applySettings(m.cfg.Session.Provider, m.cfg.Session.Model, next)
}

func (m *model) invalidateReasoningCycle() {
	m.reasoningCycleSequence++
	m.reasoningCyclePending = 0
	if m.reasoningCycleCancel != nil {
		m.reasoningCycleCancel()
		m.reasoningCycleCancel = nil
	}
	if m.notice == "Loading reasoning options…" {
		m.notice = ""
	}
}

func (m *model) pickerChoices() []settingChoice {
	if m.picker == nil {
		return nil
	}
	var choices []settingChoice
	switch m.picker.kind {
	case "profile":
		for _, profile := range m.profiles {
			choices = append(choices, settingChoice{value: profile.Name, label: profile.Name, detail: profile.Detail, active: profile.Name == m.cfg.Profile})
		}
	case "reasoning":
		detail := "Use the provider/model default"
		if option := m.currentModelOption(); option != nil && option.DefaultReasoning != "" {
			detail += " · model default: " + option.DefaultReasoning
		}
		choices = append(choices, settingChoice{value: "", label: "Default", detail: detail, active: m.cfg.Session.Reasoning == ""})
		if m.cfg.NamedProviders[m.cfg.Session.Provider] {
			for _, effort := range config.OpenRouterReasoningEfforts() {
				choices = append(choices, settingChoice{value: effort, label: effort, detail: "reasoning_effort · model support unverified", active: effort == m.cfg.Session.Reasoning})
			}
		} else if option := m.currentModelOption(); option != nil {
			for _, effort := range option.ReasoningEfforts {
				if effort != "" {
					choices = append(choices, settingChoice{value: effort, label: effort, detail: "Model-supported reasoning effort", active: effort == m.cfg.Session.Reasoning})
				}
			}
		}
	default:
		for _, option := range m.models {
			if !m.modelSelectable(option.Provider, option.ID) {
				continue
			}
			detail := option.Description
			if option.Name != "" && option.Name != option.ID {
				detail = option.Name + " · " + detail
			}
			if option.IsDefault {
				detail = "Provider default · " + detail
			}
			id := config.JoinModel(option.Provider, option.ID)
			note := ""
			if stats, ok := m.throughput[id]; ok {
				note = throughputNote(stats, m.now)
			}
			choices = append(choices, settingChoice{value: id, label: id, detail: detail, note: note, active: option.Provider == m.cfg.Session.Provider && (option.ID == m.cfg.Session.Model || (m.cfg.Session.Model == "" && option.IsDefault))})
		}
	}
	query := strings.ToLower(strings.TrimSpace(m.picker.search.Value()))
	return slices.DeleteFunc(choices, func(choice settingChoice) bool {
		return query != "" && !strings.Contains(strings.ToLower(oneLine(choice.label+" "+choice.detail)), query)
	})
}

func (m *model) updatePicker(msg tea.Msg) tea.Cmd {
	if m.picker == nil {
		return nil
	}
	if key, ok := msg.(tea.KeyPressMsg); ok {
		switch key.String() {
		case "esc":
			m.closePicker()
			return m.input.Focus()
		case "tab", "shift+tab":
			if m.picker.kind != "reasoning" {
				return nil
			}
			m.picker.search.Reset()
			m.picker.selected = 0
			m.picker.kind = "model"
			m.picker.search.Placeholder = "Search models by name or ID"
			for i, choice := range m.pickerChoices() {
				if choice.active {
					m.picker.selected = i
					break
				}
			}
			return nil
		case "up", "ctrl+p":
			m.picker.selected = max(0, m.picker.selected-1)
			return nil
		case "down", "ctrl+n":
			m.picker.selected = min(max(0, len(m.pickerChoices())-1), m.picker.selected+1)
			return nil
		case "pgup", "pgdown":
			delta := max(1, (m.bodyHeight-3)/2)
			if key.String() == "pgup" {
				delta = -delta
			}
			m.picker.selected = max(0, min(len(m.pickerChoices())-1, m.picker.selected+delta))
			return nil
		case "enter":
			if m.picker.loading {
				return nil
			}
			choices := m.pickerChoices()
			if len(choices) == 0 {
				return nil
			}
			choice := choices[min(m.picker.selected, len(choices)-1)]
			if m.picker.kind == "profile" {
				if err := m.switchProfile(choice.value); err != nil {
					m.picker.err = oneLine(err.Error())
					return nil
				}
				m.closePicker()
				return m.input.Focus()
			}
			provider, model, reasoning := m.cfg.Session.Provider, m.cfg.Session.Model, m.cfg.Session.Reasoning
			if m.picker.kind == "model" {
				provider, model = config.SplitModel(choice.value)
			} else {
				reasoning = choice.value
			}
			if err := m.applySettings(provider, model, reasoning); err != nil {
				m.picker.err = oneLine(err.Error())
				return nil
			}
			m.closePicker()
			return m.input.Focus()
		}
	}
	before := m.picker.search.Value()
	var cmd tea.Cmd
	m.picker.search, cmd = m.picker.search.Update(msg)
	if m.picker.search.Value() != before {
		m.picker.selected = 0
	}
	return cmd
}

func (m *model) settingsCommand(prompt string) (bool, tea.Cmd) {
	parts := strings.Fields(prompt)
	if parts[0] != "/model" && parts[0] != "/reasoning" {
		return false, nil
	}
	kind := strings.TrimPrefix(parts[0], "/")
	if len(parts) == 1 {
		cmd := m.openPicker(kind)
		if m.picker != nil {
			m.input.Reset()
		}
		return true, cmd
	}
	if len(parts) != 2 {
		m.notice = "Use /model provider/ID or /reasoning LEVEL|default."
		return true, nil
	}
	if kind == "reasoning" && !m.reasoningSettings() {
		m.notice = "Reasoning controls are unavailable with this provider."
		return true, nil
	}
	provider, model, reasoning := m.cfg.Session.Provider, m.cfg.Session.Model, m.cfg.Session.Reasoning
	if kind == "model" {
		// provider/ID switches provider; an ID without a known provider
		// prefix is a model of the current provider.
		model = parts[1]
		if prefix, id := config.SplitModel(parts[1]); slices.Contains(config.BuiltinProviders, prefix) || m.isNamed(prefix) {
			provider, model = prefix, id
		}
		if !m.modelSelectable(provider, model) {
			m.notice = "Settings unchanged: " + config.JoinModel(provider, model) + " is not in enabled_models."
			return true, nil
		}
	} else {
		reasoning = parts[1]
		if reasoning == "default" {
			reasoning = ""
		}
	}
	if err := m.applySettings(provider, model, reasoning); err != nil {
		m.notice = "Settings unchanged: " + oneLine(err.Error())
		return true, nil
	}
	m.input.Reset()
	return true, nil
}

func (m *model) applySettings(provider, model, reasoning string) error {
	if m.busy || m.pending != nil {
		return fmt.Errorf("wait for the active task before changing settings")
	}
	reset := false
	providerChanged := provider != m.cfg.Session.Provider
	modelChanged := model != m.cfg.Session.Model
	if (providerChanged || modelChanged) && !m.modelSelectable(provider, model) {
		return fmt.Errorf("%s is not in enabled_models", config.JoinModel(provider, model))
	}
	if !m.reasoningSettingsFor(provider) || providerChanged {
		// Never carry an effort across providers. A destination model's default
		// can be restored below from its own catalog entry.
		reasoning = ""
	}
	if option := modelOption(m.cfg.EnabledModels, provider, model); option != nil && option.DefaultReasoning != "" && (modelChanged || providerChanged) {
		// An enabled model starts at the effort its entry gives.
		reasoning = option.DefaultReasoning
	} else if m.isNamed(provider) && (modelChanged || providerChanged) {
		// A named provider's model support is not in the catalog, so do not carry a
		// potentially unsupported effort to a different model.
		reasoning = ""
	} else if config.IsCLIProvider(provider) && (modelChanged || providerChanged) {
		if option := modelOption(m.models, provider, model); option != nil && option.ID == model && (reasoning == "" || (len(option.ReasoningEfforts) > 0 && !slices.Contains(option.ReasoningEfforts, reasoning))) {
			reasoning, reset = option.DefaultReasoning, true
		}
	}
	next := cloneSession(m.cfg.Session)
	if m.cfg.Demo {
		next.Provider, next.Model, next.Reasoning, next.ReasoningSet = provider, model, reasoning, true
		if providerChanged || modelChanged {
			next.Usage.ContextWindow = 0
		}
	} else {
		if m.cfg.ApplySettings == nil {
			return fmt.Errorf("settings are unavailable in this session")
		}
		if err := m.cfg.ApplySettings(next, provider, model, reasoning); err != nil {
			return err
		}
	}
	m.invalidateReasoningCycle()
	// A preview's readings are all it will ever have: it reads nothing new.
	if providerChanged && !m.cfg.Demo {
		m.resetAccountUsage()
	}
	newConversation := next.ID != m.cfg.Session.ID
	*m.cfg.Session = *next
	if !m.cfg.Demo {
		m.rememberModel(provider, model)
	}
	if newConversation {
		m.entries = nil
		m.resetCounters()
		m.refresh(true)
		m.notice = "New " + m.modelProviderLabel(provider) + " conversation · " + model + ". Previous conversation saved."
	} else {
		m.notice = "Next turn: " + config.JoinModel(provider, model)
		if m.reasoningSettingsFor(provider) {
			effort := reasoning
			if effort == "" {
				effort = "default"
			}
			m.notice += " · reasoning " + effort
		}
	}
	if reset && !newConversation {
		m.notice = "Reasoning adjusted to " + reasoning + " for " + model + "."
	}
	if m.cfg.Demo {
		m.notice = "Preview only · " + m.notice
	}
	return nil
}

func (m *model) modelSelectable(provider, model string) bool {
	if len(m.cfg.EnabledModels) == 0 {
		return true
	}
	return slices.ContainsFunc(m.cfg.EnabledModels, func(option core.ModelOption) bool {
		return option.Provider == provider && option.ID == model
	})
}

// cycleModel switches to the enabled model after the current one, as
// shift+tab cycles reasoning efforts.
func (m *model) cycleModel() tea.Cmd {
	if m.busy {
		m.notice = "Draft kept. Wait for this task, or Esc to interrupt before changing settings."
		return nil
	}
	enabled := m.cfg.EnabledModels
	if len(enabled) == 0 {
		m.notice = "No enabled models to cycle. Add provider/<model-id> entries to enabled_models in your JULIA config."
		return nil
	}
	current := slices.IndexFunc(enabled, func(option core.ModelOption) bool {
		return option.Provider == m.cfg.Session.Provider && option.ID == m.cfg.Session.Model
	})
	if current >= 0 && len(enabled) == 1 {
		m.notice = "This is the only enabled model."
		return nil
	}
	next := enabled[(current+1)%len(enabled)]
	if err := m.applySettings(next.Provider, next.ID, m.cfg.Session.Reasoning); err != nil {
		m.notice = "Settings unchanged: " + oneLine(err.Error())
	}
	return nil
}

func (m *model) modelProviderLabel(provider string) string {
	return config.CLIProviderName(provider)
}

// isNamed reports whether provider is one of the configured named providers.
func (m *model) isNamed(provider string) bool {
	_, ok := m.cfg.NamedProviders[provider]
	return ok
}

func oneLine(value string) string { return strings.Join(strings.Fields(sanitize(value)), " ") }

// searchStyles dress the picker's search field as the composer: the field sits
// on the composer's panel, so every part of it carries that background.
func searchStyles() textinput.Styles {
	s := textinput.DefaultDarkStyles()
	s.Focused.Prompt = ink(panel, coral)
	s.Focused.Text = ink(panel, cream)
	s.Focused.Placeholder = ink(panel, muted)
	s.Focused.Suggestion = ink(panel, faint)
	s.Blurred = s.Focused
	s.Cursor.Color = coral
	return s
}

func (m *model) pickerView() string {
	p := m.picker
	width := m.mainWidth
	title, subtitle := "CHOOSE A MODEL", "Search · ctrl+n cycles enabled models · API changes start fresh"
	if p.kind == "reasoning" {
		name := m.cfg.Session.Model
		if option := m.currentModelOption(); option != nil {
			name = option.ID
		}
		if name == "" {
			name = "configured model"
		}
		title, subtitle = "REASONING EFFORT", "For "+name+" · applies to the next turn"
	}
	if p.kind == "profile" {
		title, subtitle = "CHOOSE A PROFILE", "From config.<name>.json · each brings its own model · applies to the next turn"
	}
	heading := violetStyle.Bold(true).Render(title)
	var lines []string
	if p.loading {
		lines = append(lines, heading, mutedStyle.Render(ansi.Truncate(oneLine(subtitle), width, "…")), "", m.spinner.View()+mutedStyle.Render(" Loading available models…"))
	} else {
		choices := m.pickerChoices()
		selected := min(p.selected, max(0, len(choices)-1))
		// The position rides on the heading's row, right-aligned, as the
		// question card's does.
		count := fmt.Sprintf("%d available", len(choices))
		if len(choices) > 0 {
			count = fmt.Sprintf("%d / %d", selected+1, len(choices))
		}
		if fill := width - ansi.StringWidth(heading) - ansi.StringWidth(count); fill >= 2 {
			heading += strings.Repeat(" ", fill) + faintStyle.Render(count)
		}
		lines = append(lines, heading, mutedStyle.Render(ansi.Truncate(oneLine(subtitle), width, "…")), "")
		if m.bodyHeight < 6 {
			// Too short for chrome: the heading alone, or just the choices.
			lines = lines[:min(1, m.bodyHeight-1)]
		}
		// Tall enough, each choice is two rows and a row of air; otherwise
		// the rows pack, then drop their detail.
		rowHeight, spaced := 2, m.bodyHeight >= 14
		if spaced {
			rowHeight = 3
		} else if m.bodyHeight < 10 {
			rowHeight = 1
		}
		reserved := len(lines)
		if p.err != "" {
			reserved += 2
		}
		slots := max(1, (m.bodyHeight-reserved)/rowHeight)
		start := max(0, selected-slots+1)
		if len(choices) == 0 {
			empty := "No matching models. Esc to use /model ID."
			if len(m.cfg.EnabledModels) > 0 {
				empty = "No matching enabled models."
			}
			switch p.kind {
			case "reasoning":
				empty = "No matching levels. Esc to use /reasoning LEVEL."
			case "profile":
				empty = "No matching profiles. Esc to use /profile NAME."
			}
			lines = append(lines, mutedStyle.Render(ansi.Truncate(empty, width, "…")))
		}
		// Each choice is lit like a question's option: a coral bar down its
		// rows and a panel behind the chosen one, its detail brought up from
		// faint to muted so the eye lands there.
		for i := start; i < min(len(choices), start+slots); i++ {
			choice := choices[i]
			if spaced && i > start {
				lines = append(lines, "")
			}
			bg := background
			bar, label, detail := " ", creamStyle, faintStyle
			if i == selected {
				bg = panel
				bar, label, detail = ink(panel, coral).Render("▌"), ink(panel, cream).Bold(true), ink(panel, muted)
			}
			lead := ink(bg, cream).Render(" ") + bar + ink(bg, cream).Render("  ")
			mark := ""
			if choice.active {
				mark = ink(bg, green).Render("  ✓ current")
			}
			name := ansi.Truncate(oneLine(choice.label), max(1, width-4-ansi.StringWidth(mark)), "…")
			row := lead + label.Render(name) + mark
			// The speed sits against the right edge, in the detail's ink,
			// when the row has room for it after the name.
			if choice.note != "" {
				if fill := width - ansi.StringWidth(row) - ansi.StringWidth(choice.note) - 1; fill >= 2 {
					row += ink(bg, cream).Render(strings.Repeat(" ", fill)) + detail.Render(choice.note) + ink(bg, cream).Render(" ")
				}
			}
			lines = append(lines, paint(bg, width, row))
			if rowHeight >= 2 {
				lines = append(lines, paint(bg, width, lead+detail.Render(ansi.Truncate(oneLine(choice.detail), max(1, width-5), "…"))))
			}
		}
		if p.err != "" {
			lines = append(lines, "", coralStyle.Render(ansi.Truncate(p.err, width, "…")))
		}
	}
	if len(lines) > m.bodyHeight {
		lines = lines[:m.bodyHeight]
	}
	return lipgloss.NewStyle().Width(width).Height(m.bodyHeight).Render(strings.Join(lines, "\n"))
}

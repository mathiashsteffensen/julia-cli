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
	"jarvis/internal/core"
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

type settingChoice struct {
	value, label, detail string
	active               bool
}

func (m *model) openPicker(kind string) tea.Cmd {
	if m.busy {
		m.notice = "Draft kept. Wait for this task, or Esc to interrupt before changing settings."
		return nil
	}
	if kind == "reasoning" && !m.codexSettings() {
		m.notice = "Reasoning controls are available with Codex."
		return nil
	}
	if m.cfg.ApplySettings == nil && !m.cfg.Demo {
		m.notice = "Settings are unavailable in this session."
		return nil
	}
	m.closePicker()
	m.pickerSequence++
	search := textinput.New()
	search.Prompt = "› "
	search.Placeholder = "Search models by name or ID"
	if kind == "reasoning" {
		search.Placeholder = "Filter reasoning levels"
	}
	search.CharLimit = 256
	search.SetVirtualCursor(true)
	search.SetWidth(max(8, m.mainWidth-8))
	search.Focus()
	m.picker = &settingsPicker{kind: kind, search: search, sequence: m.pickerSequence}
	m.input.Blur()
	m.notice = ""
	if m.cfg.Demo {
		m.models = []core.ModelOption{
			{ID: "preview-balanced", Name: "Balanced", Description: "Offline example · everyday coding and research", ReasoningEfforts: []string{"low", "medium", "high"}, DefaultReasoning: "medium", IsDefault: true},
			{ID: "preview-deep", Name: "Deep reasoning", Description: "Offline example · demanding, extended tasks", ReasoningEfforts: []string{"medium", "high"}, DefaultReasoning: "high"},
		}
		return textinput.Blink
	}
	if m.cfg.Models == nil {
		m.picker.err = "Model catalog unavailable. Esc, then use /model ID."
		if kind == "reasoning" {
			m.picker.err = "Effort catalog unavailable. Esc, then use /reasoning LEVEL."
		}
		return textinput.Blink
	}
	m.picker.loading = true
	ctx, cancel := context.WithTimeout(m.lifetime, 30*time.Second)
	m.picker.cancel = cancel
	sequence, fetch := m.picker.sequence, m.cfg.Models
	return tea.Batch(textinput.Blink, func() tea.Msg {
		defer cancel()
		options, err := fetch(ctx)
		// Detach slices from provider-owned catalog storage.
		options = append([]core.ModelOption(nil), options...)
		for i := range options {
			options[i].ReasoningEfforts = append([]string(nil), options[i].ReasoningEfforts...)
		}
		return modelsMsg{sequence: sequence, options: options, err: err}
	})
}

func (m *model) codexSettings() bool {
	return m.cfg.Demo || m.cfg.Session.Provider == "codex"
}

func (m *model) closePicker() {
	if m.picker != nil && m.picker.cancel != nil {
		m.picker.cancel()
	}
	m.picker = nil
	m.input.Focus()
}

func (m *model) receiveModels(msg modelsMsg) tea.Cmd {
	if m.picker == nil || m.picker.sequence != msg.sequence || m.quitting {
		return nil
	}
	m.picker.loading = false
	m.picker.cancel = nil
	if msg.err != nil {
		m.picker.err = "Could not load models: " + oneLine(msg.err.Error()) + ". Esc to use a direct command."
		return nil
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
	for i := range m.models {
		option := &m.models[i]
		if option.ID == m.cfg.Session.Model || (m.cfg.Session.Model == "" && option.IsDefault) {
			return option
		}
	}
	return nil
}

func (m *model) pickerChoices() []settingChoice {
	if m.picker == nil {
		return nil
	}
	var choices []settingChoice
	if m.picker.kind == "reasoning" {
		detail := "Use the provider's configured default"
		if option := m.currentModelOption(); option != nil {
			if option.DefaultReasoning != "" {
				detail += " · model default: " + option.DefaultReasoning
			}
		}
		choices = append(choices, settingChoice{value: "", label: "Default", detail: detail, active: m.cfg.Session.Reasoning == ""})
		if option := m.currentModelOption(); option != nil {
			for _, effort := range option.ReasoningEfforts {
				if effort != "" {
					choices = append(choices, settingChoice{value: effort, label: effort, detail: "Reasoning effort", active: effort == m.cfg.Session.Reasoning})
				}
			}
		}
	} else {
		for _, option := range m.models {
			detail := option.Description
			if option.Name != "" && option.Name != option.ID {
				detail = option.Name + " · " + detail
			}
			if option.IsDefault {
				detail = "Provider default · " + detail
			}
			choices = append(choices, settingChoice{value: option.ID, label: option.ID, detail: detail, active: option.ID == m.cfg.Session.Model || (m.cfg.Session.Model == "" && option.IsDefault)})
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
		case "tab":
			if !m.codexSettings() {
				return nil
			}
			kind := "reasoning"
			if m.picker.kind == "reasoning" {
				kind = "model"
			}
			m.picker.kind = kind
			m.picker.search.Reset()
			m.picker.search.Placeholder = "Search models by name or ID"
			if kind == "reasoning" {
				m.picker.search.Placeholder = "Filter reasoning levels"
			}
			m.picker.selected = 0
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
			model, reasoning := m.cfg.Session.Model, m.cfg.Session.Reasoning
			if m.picker.kind == "model" {
				model = choice.value
			} else {
				reasoning = choice.value
			}
			if err := m.applySettings(model, reasoning); err != nil {
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
		m.notice = "Use /model ID or /reasoning LEVEL|default."
		return true, nil
	}
	if kind == "reasoning" && !m.codexSettings() {
		m.notice = "Reasoning controls are available with Codex."
		return true, nil
	}
	model, reasoning := m.cfg.Session.Model, m.cfg.Session.Reasoning
	if kind == "model" {
		model = parts[1]
	} else {
		reasoning = parts[1]
		if reasoning == "default" {
			reasoning = ""
		}
	}
	if err := m.applySettings(model, reasoning); err != nil {
		m.notice = "Settings unchanged: " + oneLine(err.Error())
		return true, nil
	}
	m.input.Reset()
	return true, nil
}

func (m *model) applySettings(model, reasoning string) error {
	if m.busy || m.pending != nil {
		return fmt.Errorf("wait for the active task before changing settings")
	}
	reset := false
	if model != m.cfg.Session.Model {
		for _, option := range m.models {
			if option.ID == model && (reasoning == "" || (len(option.ReasoningEfforts) > 0 && !slices.Contains(option.ReasoningEfforts, reasoning))) {
				reasoning, reset = option.DefaultReasoning, true
				break
			}
		}
	}
	next := cloneSession(m.cfg.Session)
	if m.cfg.Demo {
		next.Model, next.Reasoning, next.ReasoningSet = model, reasoning, true
	} else {
		if m.cfg.ApplySettings == nil {
			return fmt.Errorf("settings are unavailable in this session")
		}
		if err := m.cfg.ApplySettings(next, model, reasoning); err != nil {
			return err
		}
	}
	*m.cfg.Session = *next
	m.notice = "Next turn: " + model
	if m.codexSettings() {
		effort := reasoning
		if effort == "" {
			effort = "default"
		}
		m.notice += " · reasoning " + effort
	}
	if reset {
		m.notice = "Reasoning adjusted to " + reasoning + " for " + model + "."
	}
	if m.cfg.Demo {
		m.notice = "Preview only · " + m.notice
	}
	return nil
}

func oneLine(value string) string { return strings.Join(strings.Fields(sanitize(value)), " ") }

func (m *model) pickerView() string {
	p := m.picker
	width := m.mainWidth
	title, subtitle := "CHOOSE A MODEL", "Search by name or ID · applies to the next turn"
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
	lines := []string{violetStyle.Bold(true).Render(title), mutedStyle.Render(ansi.Truncate(oneLine(subtitle), width, "…"))}
	if p.loading {
		lines = append(lines, "", m.spinner.View()+mutedStyle.Render(" Loading available models…"))
	} else {
		choices := m.pickerChoices()
		rowHeight := 2
		if m.bodyHeight < 9 {
			rowHeight = 1
		}
		reserved := 3
		if p.err != "" {
			reserved++
		}
		slots := max(1, (m.bodyHeight-reserved)/rowHeight)
		selected := min(p.selected, max(0, len(choices)-1))
		start := max(0, selected-slots+1)
		if len(choices) == 0 {
			empty := "No matching models. Esc to use /model ID."
			if p.kind == "reasoning" {
				empty = "No matching levels. Esc to use /reasoning LEVEL."
			}
			lines = append(lines, mutedStyle.Render(ansi.Truncate(empty, width, "…")))
		}
		for i := start; i < min(len(choices), start+slots); i++ {
			choice := choices[i]
			prefix, suffix := "  ", ""
			if i == selected {
				prefix = "› "
			}
			if choice.active {
				suffix = "  ✓"
			}
			label := ansi.Truncate(prefix+oneLine(choice.label)+suffix, width, "…")
			style := creamStyle
			if i == selected {
				style = lipgloss.NewStyle().Foreground(coral).Background(panel).Bold(true).Width(width)
			}
			lines = append(lines, style.Render(label))
			if rowHeight == 2 {
				lines = append(lines, faintStyle.Render(ansi.Truncate("  "+oneLine(choice.detail), width, "…")))
			}
		}
		if p.err != "" {
			lines = append(lines, coralStyle.Render(ansi.Truncate(p.err, width, "…")))
		} else {
			count := fmt.Sprintf("%d available", len(choices))
			if len(choices) > 0 {
				count = fmt.Sprintf("%d / %d", selected+1, len(choices))
			}
			lines = append(lines, faintStyle.Render(count))
		}
	}
	if len(lines) > m.bodyHeight {
		lines = lines[:m.bodyHeight]
	}
	return lipgloss.NewStyle().Width(width).Height(m.bodyHeight).Render(strings.Join(lines, "\n"))
}

package ui

import (
	"errors"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"julia/internal/config"
	"julia/internal/core"
)

// Profile is a configuration profile as the profile picker lists it.
type Profile struct {
	Name string
	// Detail describes the profile: its model and approval mode, or why it
	// does not load.
	Detail string
}

// ProfileSettings are what switching profile changes in the interface; the
// session's model and reasoning change with it, in the session itself.
type ProfileSettings struct {
	Mode           string
	ContextWindow  int
	EnabledModels  []core.ModelOption
	NamedProviders map[string]bool
}

// profileCommand answers /profile: alone it opens the profile picker, and
// with a name it switches to that profile.
func (m *model) profileCommand(prompt string) (bool, tea.Cmd) {
	parts := strings.Fields(prompt)
	if parts[0] != "/profile" {
		return false, nil
	}
	switch len(parts) {
	case 1:
		cmd := m.openProfilePicker()
		if m.picker != nil {
			m.input.Reset()
		}
		return true, cmd
	case 2:
		if err := m.switchProfile(parts[1]); err != nil {
			m.notice = "Profile unchanged: " + oneLine(err.Error())
			return true, nil
		}
		m.input.Reset()
	default:
		m.notice = "Use /profile or /profile NAME."
	}
	return true, nil
}

func (m *model) openProfilePicker() tea.Cmd {
	if m.busy {
		m.notice = "Draft kept. Wait for this task, or Esc to interrupt before changing settings."
		return nil
	}
	profiles := demoProfiles()
	if !m.cfg.Demo {
		if m.cfg.Profiles == nil || m.cfg.SwitchProfile == nil {
			m.notice = "Profiles are unavailable in this session."
			return nil
		}
		var err error
		if profiles, err = m.cfg.Profiles(); err != nil {
			m.notice = "Could not list profiles: " + oneLine(err.Error())
			return nil
		}
	}
	m.invalidateReasoningCycle()
	m.closePicker()
	m.profiles = profiles
	m.picker = &settingsPicker{kind: "profile", search: m.pickerSearch("Filter profiles")}
	for i, choice := range m.pickerChoices() {
		if choice.active {
			m.picker.selected = i
			break
		}
	}
	m.input.Blur()
	m.notice = ""
	return textinput.Blink
}

// demoProfiles are the offline preview's profiles.
func demoProfiles() []Profile {
	return []Profile{
		{Name: config.DefaultProfile, Detail: "config.json · codex · auto"},
		{Name: "work", Detail: "config.work.json · openrouter/preview-deep · ask"},
	}
}

// switchProfile makes name the active profile: its configuration applies
// from the next turn, and the session moves to its model as /model would,
// starting a fresh conversation where a model change must.
func (m *model) switchProfile(name string) error {
	if m.busy || m.pending != nil {
		return errors.New("wait for the active task before changing settings")
	}
	if m.cfg.Demo {
		m.cfg.Profile = name
		m.notice = "Preview only · Profile " + name
		return nil
	}
	if m.cfg.SwitchProfile == nil {
		return errors.New("profiles are unavailable in this session")
	}
	next := cloneSession(m.cfg.Session)
	settings, err := m.cfg.SwitchProfile(next, name)
	if err != nil {
		return err
	}
	m.invalidateReasoningCycle()
	m.resetAccountUsage()
	newConversation := next.ID != m.cfg.Session.ID
	*m.cfg.Session = *next
	m.cfg.Profile, m.cfg.Mode, m.cfg.ContextWindow = name, settings.Mode, settings.ContextWindow
	m.cfg.EnabledModels, m.cfg.NamedProviders = settings.EnabledModels, settings.NamedProviders
	// The models another profile enabled may name providers this one lacks.
	m.knownModels = append([]core.ModelOption(nil), settings.EnabledModels...)
	m.rememberModel(next.Provider, next.Model)
	m.models = nil
	model := config.JoinModel(next.Provider, next.Model)
	if newConversation {
		m.entries = nil
		m.resetCounters()
		m.refresh(true)
		m.notice = "Profile " + name + " · new " + m.modelProviderLabel(next.Provider) + " conversation · " + model + ". Previous conversation saved."
	} else {
		m.notice = "Profile " + name + " · next turn: " + model + " · " + settings.Mode
	}
	return nil
}

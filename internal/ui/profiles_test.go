package ui

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"julia/internal/core"
)

func profilesConfig(switched *[]string) Config {
	cfg := settingsConfig()
	cfg.Mode, cfg.Profile = "auto", "default"
	cfg.Profiles = func() ([]Profile, error) {
		return []Profile{{Name: "default", Detail: "config.json · codex · auto"}, {Name: "work", Detail: "config.work.json · codex/work-model · ask"}}, nil
	}
	cfg.SwitchProfile = func(s *core.Session, name string) (ProfileSettings, error) {
		*switched = append(*switched, name)
		if name != "work" {
			s.Model = "mutated before failing" // The interface must not keep this.
			return ProfileSettings{}, errors.New("profile " + name + " not found")
		}
		s.Model, s.Reasoning = "work-model", ""
		return ProfileSettings{Mode: "ask", ContextWindow: 64000, EnabledModels: []core.ModelOption{{Provider: "codex", ID: "work-model"}, {Provider: "codex", ID: "work-fast"}}, NamedProviders: map[string]bool{}}, nil
	}
	return cfg
}

func TestProfilePickerListsProfilesAndSwitches(t *testing.T) {
	var switched []string
	m := testModel(t, profilesConfig(&switched))
	m.input.SetValue("/profile")
	m.submit()
	if m.picker == nil || m.picker.kind != "profile" || m.input.Value() != "" {
		t.Fatal("/profile did not open the profile picker")
	}
	choices := m.pickerChoices()
	if len(choices) != 2 || !choices[0].active || choices[1].active || m.picker.selected != 0 {
		t.Fatalf("picker choices %+v, selected %d", choices, m.picker.selected)
	}
	view := ansi.Strip(m.pickerView())
	for _, want := range []string{"CHOOSE A PROFILE", "config.work.json · codex/work-model · ask", "✓ current"} {
		if !strings.Contains(view, want) {
			t.Fatalf("picker lacks %q:\n%s", want, view)
		}
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.picker != nil || len(switched) != 1 || switched[0] != "work" {
		t.Fatalf("enter switched %v and left picker %v", switched, m.picker)
	}
	if m.cfg.Profile != "work" || m.cfg.Mode != "ask" || m.cfg.ContextWindow != 64000 || m.cfg.Session.Model != "work-model" || len(m.cfg.EnabledModels) != 2 {
		t.Fatalf("profile switch left %+v, session %+v", m.cfg, m.cfg.Session)
	}
	// The previous profile's models give way to this profile's.
	if len(m.knownModels) != 2 || m.knownModels[1].ID != "work-fast" {
		t.Fatalf("known models %+v", m.knownModels)
	}
	if !strings.Contains(m.notice, "Profile work") || !strings.Contains(m.notice, "codex/work-model") {
		t.Fatalf("notice %q", m.notice)
	}
	if !strings.Contains(ansi.Strip(m.sidebar()), "profile: work") {
		t.Fatal("sidebar does not show the active profile")
	}
}

func TestDirectProfileCommandAndFailureLeaveStateAlone(t *testing.T) {
	var switched []string
	m := testModel(t, profilesConfig(&switched))
	m.input.SetValue("/profile wrok")
	m.submit()
	if m.cfg.Profile != "default" || m.cfg.Session.Model != "current" || m.cfg.Mode != "auto" || m.input.Value() != "/profile wrok" || !strings.Contains(m.notice, "not found") {
		t.Fatalf("failed switch changed state: profile %s, model %s, notice %q", m.cfg.Profile, m.cfg.Session.Model, m.notice)
	}
	m.input.SetValue("/profile work")
	m.submit()
	if m.cfg.Profile != "work" || m.input.Value() != "" {
		t.Fatal("/profile NAME did not switch")
	}
	m.input.SetValue("/profile a b")
	m.submit()
	if !strings.Contains(m.notice, "/profile NAME") {
		t.Fatalf("usage notice %q", m.notice)
	}
}

func TestProfileSwitchWaitsForTheTask(t *testing.T) {
	var switched []string
	m := testModel(t, profilesConfig(&switched))
	m.busy = true
	m.input.SetValue("/profile work")
	m.submit()
	m.openProfilePicker()
	if len(switched) != 0 || m.picker != nil || m.input.Value() != "/profile work" {
		t.Fatal("profile switched or picker opened during a task")
	}
}

func TestProfileSwitchToNewConversationClearsTranscript(t *testing.T) {
	var switched []string
	cfg := profilesConfig(&switched)
	cfg.SwitchProfile = func(s *core.Session, name string) (ProfileSettings, error) {
		*s = core.Session{ID: "fresh", Directory: s.Directory, Provider: "router", Model: "vendor/model"}
		return ProfileSettings{Mode: "auto", NamedProviders: map[string]bool{"router": true}}, nil
	}
	m := testModel(t, cfg)
	m.entries = []entry{{role: "user", text: "old"}}
	m.toolCount = 3
	if err := m.switchProfile("router"); err != nil {
		t.Fatal(err)
	}
	if m.cfg.Session.ID != "fresh" || len(m.entries) != 0 || m.toolCount != 0 || !strings.Contains(m.notice, "Previous conversation saved") {
		t.Fatalf("new conversation kept the old transcript: %d entries, notice %q", len(m.entries), m.notice)
	}
	if !m.reasoningSettings() {
		t.Fatal("the profile's named provider is not known to the interface")
	}
}

func TestDemoProfilesNeverCallExternalCallbacks(t *testing.T) {
	cfg := settingsConfig()
	cfg.Demo, cfg.Profile = true, "default"
	cfg.Profiles = func() ([]Profile, error) { t.Fatal("demo listed profiles"); return nil, nil }
	cfg.SwitchProfile = func(*core.Session, string) (ProfileSettings, error) {
		t.Fatal("demo switched profile")
		return ProfileSettings{}, nil
	}
	m := testModel(t, cfg)
	m.openProfilePicker()
	m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.cfg.Profile != "work" || !strings.Contains(m.notice, "Preview only") {
		t.Fatalf("demo profile %q, notice %q", m.cfg.Profile, m.notice)
	}
}

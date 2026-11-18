package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func TestCtrlSTogglesSidebarAndRelayoutsConversation(t *testing.T) {
	m := testModel(t, Config{Demo: true})
	m.notice = ""
	m.cfg.Session.Model = "test-model"
	m.input.SetValue("Draft kept while toggling")
	m.resize(120, 40)
	mainWidth, inputWidth := m.mainWidth, m.input.Width()
	sidebarX := m.margin() + m.mainWidth + 4
	if !m.hasSidebar() || !m.overSidebar(sidebarX, 5) {
		t.Fatal("wide terminal should show the sidebar by default")
	}

	m.Update(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	if m.hasSidebar() || m.overSidebar(sidebarX, 5) {
		t.Fatal("hidden sidebar still occupies screen space")
	}
	if m.mainWidth != mainWidth+sidebarWidth+2 || m.viewport.Width() != m.mainWidth || m.input.Width() != inputWidth+sidebarWidth+2 {
		t.Fatal("hiding the sidebar did not widen the conversation and composer")
	}
	view := ansi.Strip(m.View().Content)
	if strings.Contains(view, "CONTEXT") || !strings.Contains(view, "Draft kept while toggling") {
		t.Fatalf("hidden-sidebar view still shows the sidebar or lost the composer:\n%s", view)
	}
	for _, want := range []string{"ctrl+s sidebar", "ctrl+f fleet", "test-model", "46%"} {
		if row := ansi.Strip(m.statusRow()); !strings.Contains(row, want) {
			t.Fatalf("hidden sidebar status row is missing %q: %s", want, row)
		}
	}
	if !strings.HasPrefix(ansi.Strip(m.masthead()), "JULIA") {
		t.Fatal("hiding the sidebar did not move the name to the masthead")
	}

	m.Update(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	if !m.hasSidebar() || m.mainWidth != mainWidth || m.input.Width() != inputWidth {
		t.Fatal("second press did not restore the original layout")
	}
	if !strings.Contains(ansi.Strip(m.View().Content), "CONTEXT") {
		t.Fatal("restored sidebar was not rendered")
	}
	if strings.HasPrefix(ansi.Strip(m.masthead()), "JULIA") {
		t.Fatal("restoring the sidebar left a duplicate name in the masthead")
	}
}

func TestSidebarVisibilitySurvivesTerminalResizes(t *testing.T) {
	m := testModel(t, Config{})
	m.entries = []entry{{role: "assistant", text: "A conversation."}}
	m.resize(120, 40)
	m.Update(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	for _, width := range []int{160, 80, 120} {
		m.Update(tea.WindowSizeMsg{Width: width, Height: 40})
		if m.hasSidebar() || m.mainWidth != width-2*m.margin() {
			t.Fatalf("resize to %d restored the hidden sidebar", width)
		}
	}

	m.Update(tea.WindowSizeMsg{Width: 80, Height: 40})
	m.Update(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	if m.hasSidebar() {
		t.Fatal("enabling the sidebar forced it into a narrow terminal")
	}
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	if !m.hasSidebar() {
		t.Fatal("sidebar enabled in a narrow terminal did not return when widened")
	}
}

func TestSidebarToggleKeepsLiveFollowAndHistoryPosition(t *testing.T) {
	for _, follow := range []bool{true, false} {
		m := testModel(t, Config{})
		m.entries = []entry{{role: "assistant", text: strings.Repeat("A short transcript row.\n\n", 80)}}
		m.resize(120, 40)
		m.viewport.GotoBottom()
		if !follow {
			m.viewport.ScrollUp(12)
		}
		offset := m.viewport.YOffset()

		for range 2 {
			m.Update(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
			if follow && !m.viewport.AtBottom() {
				t.Fatal("sidebar toggle lost live follow")
			}
			if !follow && m.viewport.YOffset() != offset {
				t.Fatal("sidebar toggle moved the history reader")
			}
		}
	}
}

func TestCtrlSWhileBusyDoesNotSendDraftOrSteerQueue(t *testing.T) {
	m, _ := busyModel(t, false)
	sendText(m, "queued message")
	m.input.SetValue("unfinished draft")

	for range 2 {
		wasVisible := m.hasSidebar()
		m.Update(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
		if m.hasSidebar() == wasVisible {
			t.Fatal("sidebar did not toggle during a running task")
		}
		if m.input.Value() != "unfinished draft" {
			t.Fatal("sidebar toggle changed the draft")
		}
		if len(m.queued) != 1 || m.queued[0].prompt.Text != "queued message" || m.queued[0].steering {
			t.Fatalf("sidebar toggle changed the queue: %+v", m.queued)
		}
		if got := m.inbox.Take(); len(got) != 0 {
			t.Fatalf("sidebar toggle sent messages to the running turn: %+v", got)
		}
	}
}

func TestSidebarTogglePreservesTheWelcomeLayout(t *testing.T) {
	m := testModel(t, Config{})
	m.resize(120, 40)

	for range 2 {
		m.Update(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
		if m.hasSidebar() || m.mainWidth != m.width-2*m.margin() {
			t.Fatal("sidebar toggle changed the welcome layout")
		}
		if !strings.Contains(ansi.Strip(m.View().Content), tagline) {
			t.Fatal("sidebar toggle hid the welcome poster")
		}
	}

	m.Update(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	sendText(m, "/help")
	if m.hasSidebar() || !m.sidebarHidden || len(m.entries) == 0 {
		t.Fatal("starting a conversation ignored the hidden sidebar setting")
	}
	if row := ansi.Strip(m.statusRow()); !strings.Contains(row, "ctrl+s sidebar") || !strings.Contains(row, "provider default") {
		t.Fatalf("hidden sidebar lost its shortcut or model: %s", row)
	}

	sendText(m, "/clear")
	if !m.welcoming() || !m.sidebarHidden {
		t.Fatal("clearing the conversation lost the hidden sidebar setting")
	}
	sendText(m, "/help")
	if m.hasSidebar() {
		t.Fatal("a new conversation restored the hidden sidebar")
	}
	m.Update(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	if !m.hasSidebar() {
		t.Fatal("sidebar could not be restored in the new conversation")
	}
}

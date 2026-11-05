package ui

import (
	"time"

	tea "charm.land/bubbletea/v2"
)

func (m *model) toggleThinking() tea.Cmd {
	m.showThinking = !m.showThinking
	m.selection = nil
	m.refresh(false)

	notice := "Thinking hidden"
	if m.showThinking {
		notice = "Thinking shown when supplied by the provider"
	}
	m.notice = notice
	return tea.Tick(3*time.Second, func(time.Time) tea.Msg {
		return copyNoticeDoneMsg{notice}
	})
}

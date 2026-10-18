package ui

import (
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"julia/internal/core"
)

// Followup is work the backend finishes after a turn has ended, such as the
// reflection that reviews the turn: the turn ends when its answer does, so
// the next message need not wait. It is announced when it starts, so the
// interface can say so and Esc can skip it, and announced again when it ends,
// with the events to show. What it changed joins the session through the
// backend, at the start of the next turn or when JULIA exits; the interface
// only shows it.
type Followup struct {
	// SessionID is the conversation the work belongs to.
	SessionID string
	// Status says what is under way while Done is false; Cancel skips it.
	Status string
	Cancel func()
	// Done marks the end; Events are shown as a turn's own are.
	Done   bool
	Events []core.Event
}

type followupMsg struct{ followup Followup }

func (m *model) waitFollowup() tea.Cmd {
	if m.cfg.Followups == nil || m.followupListening {
		return nil
	}
	m.followupListening = true
	return func() tea.Msg {
		select {
		case followup, ok := <-m.cfg.Followups:
			if !ok {
				return nil
			}
			return followupMsg{followup}
		case <-m.lifetime.Done():
			return nil
		}
	}
}

// receiveFollowup shows a followup: while it runs, as the status of an idle
// interface; when it ends, its cards in the conversation, above a message
// sent while it ran, where the transcript keeps them. Work on a conversation
// no longer on screen, after /clear, is summed up in notes, since its cards
// belong to a transcript that is not shown.
func (m *model) receiveFollowup(f Followup) {
	current := m.cfg.Session != nil && f.SessionID == m.cfg.Session.ID
	if !f.Done {
		if current {
			m.followup = &f
			m.refresh(false)
		}
		return
	}
	if m.followup != nil && m.followup.SessionID == f.SessionID {
		m.followup = nil
	}
	if !current {
		for _, event := range f.Events {
			if event.Kind != "tool_end" || event.Result == nil {
				continue
			}
			text := "The reflection on the previous conversation: " + lowerFirst(event.Result.Content)
			if event.Result.IsError {
				text = "The reflection on the previous conversation " + strings.TrimPrefix(event.Result.Content, "Reflection ")
			}
			m.entries = append(m.entries, entry{role: "note", text: text})
		}
		m.refresh(false)
		return
	}
	cards := followupEntries(f.Events)
	for _, card := range cards {
		if card.role == "tool" {
			m.toolCount++
		}
	}
	if m.busy && m.turnStart >= 0 && m.turnStart <= len(m.entries) {
		// The turn under way began after the work this followup did.
		m.entries = slices.Concat(m.entries[:m.turnStart:m.turnStart], cards, m.entries[m.turnStart:])
		m.turnStart += len(cards)
	} else {
		m.entries = append(m.entries, cards...)
	}
	m.refresh(false)
}

// followupEntries are the entries a followup's events make: each tool call
// a finished card, each warning its own entry.
func followupEntries(events []core.Event) []entry {
	var out []entry
	for _, event := range events {
		switch event.Kind {
		case "tool_start":
			out = append(out, entry{role: "tool", call: event.Call, text: event.Text})
		case "tool_end":
			for i := len(out) - 1; i >= 0; i-- {
				if out[i].role == "tool" && !out[i].done && (event.Call == nil || out[i].call != nil && out[i].call.ID == event.Call.ID) {
					out[i].done, out[i].result = true, event.Result
					break
				}
			}
		case "warning":
			out = append(out, entry{role: "warning", text: event.Text})
		case "status":
			out = append(out, entry{role: "note", text: event.Text})
		}
	}
	return out
}

// lowerFirst lowers a sentence's first letter, to continue another.
func lowerFirst(text string) string {
	if text == "" {
		return text
	}
	return strings.ToLower(text[:1]) + text[1:]
}

// skipFollowup cancels the followup under way when Esc came while its status
// is the one on screen: an Esc that dismisses a notice, or comes while
// reading history, skips nothing the user cannot see.
func (m *model) skipFollowup() bool {
	if m.busy || m.notice != "" || !m.viewport.AtBottom() || m.followup == nil || m.followup.Cancel == nil {
		return false
	}
	m.followup.Cancel()
	m.followup = nil
	m.notice = "Reflection skipped."
	m.refresh(false)
	return true
}

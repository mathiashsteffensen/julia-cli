package ui

import (
	"encoding/json"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"julia/internal/core"
)

func reflectionEvents() []core.Event {
	call := &core.ToolCall{ID: "reflection-0-1", Name: "Memory", Arguments: json.RawMessage(`{"action":"save","scope":"workspace","name":"make-check","type":"feedback","description":"Run make check"}`)}
	return []core.Event{
		{Kind: "tool_start", Call: call, Text: "reflection"},
		{Kind: "tool_end", Call: call, Result: &core.ToolResult{Content: "Saved workspace note make-check"}},
	}
}

// After a turn the interface is free: the reflection still running says so
// where "Ready" would be, Esc skips it, and its cards join the conversation
// when it ends.
func TestAReflectionAfterTheTurnLeavesTheInterfaceFree(t *testing.T) {
	m := testModel(t, Config{})
	m.resize(100, 30)
	skipped := false
	m.receiveFollowup(Followup{SessionID: m.cfg.Session.ID, Status: "Reflecting on your last turn · codex/gpt-6 · Esc skips", Cancel: func() { skipped = true }})
	if line := ansi.Strip(m.statusLine()); !strings.HasPrefix(line, "◌ Reflecting on your last turn · codex/gpt-6 · Esc skips") {
		t.Fatalf("status %q", line)
	}
	m.input.SetValue("my next message")
	if m.busy {
		t.Fatal("busy while only the reflection runs")
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if !skipped || m.followup != nil || ansi.Strip(m.statusLine()) != "Reflection skipped." || m.input.Value() != "my next message" {
		t.Fatalf("esc: skipped %v status %q draft %q", skipped, ansi.Strip(m.statusLine()), m.input.Value())
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape}) // a second Esc clears the notice
	tools := m.toolCount
	m.receiveFollowup(Followup{SessionID: m.cfg.Session.ID, Done: true, Events: reflectionEvents()})
	if m.followup != nil || !strings.HasPrefix(ansi.Strip(m.statusLine()), "● Ready") || m.status != "Ready" || m.toolCount != tools+1 {
		t.Fatalf("after: followup %v status %q %q tools %d", m.followup, ansi.Strip(m.statusLine()), m.status, m.toolCount)
	}
	last := m.entries[len(m.entries)-1]
	if card := ansi.Strip(m.renderEntryContent(last)); !strings.Contains(card, "Memory  reflection · save · make-check") || !strings.Contains(card, "Saved workspace note make-check") {
		t.Fatalf("card:\n%s", card)
	}
}

// A reflection on a conversation /clear left behind is summed up, not shown
// as cards of the new one.
func TestAReflectionOnAnEarlierConversationIsANote(t *testing.T) {
	m := testModel(t, Config{})
	m.receiveFollowup(Followup{SessionID: "earlier", Status: "Reflecting", Cancel: func() {}})
	if m.followup != nil {
		t.Fatal("an earlier conversation's reflection took the status line")
	}
	m.receiveFollowup(Followup{SessionID: "earlier", Done: true, Events: reflectionEvents()})
	last := m.entries[len(m.entries)-1]
	if last.role != "note" || last.text != "The reflection on the previous conversation: saved workspace note make-check" {
		t.Fatalf("note %+v", last)
	}
}

// Followups reach the model through its channel, as approvals do.
func TestFollowupsArriveThroughTheChannel(t *testing.T) {
	followups := make(chan Followup, 1)
	m := testModel(t, Config{Followups: followups})
	followups <- Followup{SessionID: m.cfg.Session.ID, Status: "Reflecting"}
	msg := m.waitFollowup()()
	if _, cmd := m.Update(msg); cmd == nil || m.followup == nil || m.followup.Status != "Reflecting" {
		t.Fatalf("followup not received: %+v", m.followup)
	}
}

// An Esc that dismisses a notice skips nothing the user could not see.
func TestEscDismissesANoticeBeforeItSkipsTheReflection(t *testing.T) {
	m := testModel(t, Config{})
	m.resize(100, 30)
	skipped := false
	m.receiveFollowup(Followup{SessionID: m.cfg.Session.ID, Status: "Reflecting on your last turn · Esc skips", Cancel: func() { skipped = true }})
	m.notice = "New conversation."
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if skipped || m.notice != "" || m.followup == nil {
		t.Fatalf("first Esc: skipped %v notice %q", skipped, m.notice)
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if !skipped {
		t.Fatal("second Esc did not skip")
	}
}

// Cards that arrive while the next turn waits go above its message, where
// the transcript keeps them, and leave its status alone.
func TestCardsArrivingDuringTheNextTurnGoAboveItsMessage(t *testing.T) {
	m := testModel(t, Config{})
	m.entries = []entry{{role: "user", text: "Always run make check"}, {role: "assistant", text: "Done."}}
	m.turnStart = len(m.entries)
	m.entries = append(m.entries, entry{role: "user", text: "Next"})
	m.busy, m.status = true, "Finishing the reflection on your last turn"
	m.receiveFollowup(Followup{SessionID: m.cfg.Session.ID, Done: true, Events: reflectionEvents()})
	var order []string
	for _, e := range m.entries {
		order = append(order, e.role+":"+e.text)
	}
	if strings.Join(order, ",") != "user:Always run make check,assistant:Done.,tool:reflection,user:Next" || m.status != "Finishing the reflection on your last turn" || !m.entries[2].done {
		t.Fatalf("order %v status %q", order, m.status)
	}
	// A second followup in the same turn keeps its place too.
	m.receiveFollowup(Followup{SessionID: m.cfg.Session.ID, Done: true, Events: []core.Event{{Kind: "warning", Text: "Reflection failed: x"}}})
	if m.entries[3].role != "warning" || m.entries[4].text != "Next" {
		t.Fatalf("second followup out of place: %+v", m.entries)
	}
}

func TestANoteOnAnEarlierConversationNamesRefusalsToo(t *testing.T) {
	m := testModel(t, Config{})
	call := &core.ToolCall{ID: "reflection-0-2", Name: "Memory", Arguments: json.RawMessage(`{"action":"forget","name":"mine"}`)}
	m.receiveFollowup(Followup{SessionID: "earlier", Done: true, Events: []core.Event{{Kind: "tool_start", Call: call}, {Kind: "tool_end", Call: call, Result: &core.ToolResult{Content: "Reflection could not forget mine: needs your approval", IsError: true}}}})
	if last := m.entries[len(m.entries)-1]; last.text != "The reflection on the previous conversation could not forget mine: needs your approval" {
		t.Fatalf("note %q", last.text)
	}
}

package ui

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"julia/internal/core"
)

// /memory is answered by the hook: alone it lists, a name shows that note and
// "forget NAME" deletes it, "pin NAME" and "unpin NAME" pin and unpin it, and the Markdown that comes back joins the
// conversation under its own heading, as a reply would.
func TestMemoryCommandCallsHook(t *testing.T) {
	var calls [][2]string
	m := testModel(t, Config{Memory: func(action, name string) (string, error) {
		calls = append(calls, [2]string{action, name})
		return "**process-group-cancel** · feedback · workspace\n\nCancelling a task must signal the ordinary process group.", nil
	}})
	m.resize(100, 30)
	for _, prompt := range []string{"/memory", "/memory process-group-cancel", "/memory forget process-group-cancel", "/memory pin process-group-cancel", "/memory unpin process-group-cancel"} {
		m.input.SetValue(prompt)
		m.submit()
		if m.input.Value() != "" {
			t.Errorf("%s: composer kept %q", prompt, m.input.Value())
		}
	}
	if want := [][2]string{{"", ""}, {"show", "process-group-cancel"}, {"forget", "process-group-cancel"}, {"pin", "process-group-cancel"}, {"unpin", "process-group-cancel"}}; !slices.Equal(calls, want) {
		t.Fatalf("hook saw %v, want %v", calls, want)
	}
	if len(m.entries) != 5 {
		t.Fatalf("entries = %d, want one per command", len(m.entries))
	}
	for _, item := range m.entries {
		if item.role != "assistant" || item.label != "✦ MEMORY" {
			t.Errorf("entry %q under %q, want an assistant entry under ✦ MEMORY", item.role, item.label)
		}
	}
	view := ansi.Strip(m.View().Content)
	if !strings.Contains(view, "✦ MEMORY") || !strings.Contains(view, "Cancelling a task must signal the ordinary process group.") || strings.Contains(view, "✦ JULIA") {
		t.Fatalf("the note is not shown under the MEMORY heading:\n%s", view)
	}
}

// The hook's error is a notice, and nothing joins the conversation.
func TestMemoryCommandReportsErrors(t *testing.T) {
	m := testModel(t, Config{Memory: func(action, name string) (string, error) {
		return "", errors.New("no note is named " + name)
	}})
	m.resize(100, 30)
	m.input.SetValue("/memory missing")
	m.submit()
	if m.notice != "Memory: no note is named missing" || len(m.entries) != 0 {
		t.Fatalf("notice %q, %d entries", m.notice, len(m.entries))
	}
	m.input.SetValue("/memory forget")
	m.submit()
	if !strings.HasPrefix(m.notice, "Use /memory") || len(m.entries) != 0 {
		t.Fatalf("a forget without a name: notice %q, %d entries", m.notice, len(m.entries))
	}
}

// Without a hook the command says so, in the preview's own words when the
// interface is the offline demo.
func TestMemoryCommandWithoutHook(t *testing.T) {
	for demo, want := range map[bool]string{false: "Memory is not configured.", true: "Memory is unavailable in this preview."} {
		m := testModel(t, Config{Demo: demo})
		m.resize(100, 30)
		before := len(m.entries)
		m.input.SetValue("/memory")
		m.submit()
		if m.notice != want || len(m.entries) != before {
			t.Errorf("demo %v: notice %q, %d entries, want %q and %d", demo, m.notice, len(m.entries), want, before)
		}
	}
}

// The help lists the command beside the other slash commands.
func TestHelpMentionsMemory(t *testing.T) {
	m := testModel(t, Config{})
	m.resize(100, 40)
	m.input.SetValue("/help")
	m.submit()
	if len(m.entries) != 1 || !strings.Contains(m.entries[0].text, "`/memory forget NAME` deletes it") {
		t.Fatalf("help lacks the /memory bullet")
	}
}

// A note the reflection step saved says so on its card, live and after a
// resume, where the card is rebuilt from the transcript.
func TestReflectionCardsAreMarked(t *testing.T) {
	call := core.ToolCall{ID: "reflection-0-1", Name: "Memory", Arguments: json.RawMessage(`{"action":"save","scope":"workspace","name":"make-check","type":"feedback","description":"Run make check"}`)}
	s := &core.Session{ID: "r", Directory: "/tmp/julia", Provider: "fake", Messages: []core.Message{
		{Role: "user", Content: "Fix the build"}, {Role: "assistant", Content: "Done, sir."},
		{Role: "assistant", ToolCalls: []core.ToolCall{call}}, {Role: "tool", ToolCallID: call.ID, Content: "Saved workspace note make-check"},
	}}
	m := testModel(t, Config{Session: s})
	m.resize(120, 30)
	var card string
	for _, e := range m.entries {
		if e.call != nil && e.call.ID == call.ID {
			card = ansi.Strip(m.renderEntryContent(e))
		}
	}
	if !strings.Contains(card, "Memory  reflection · save · make-check · feedback") || !strings.Contains(card, "Saved workspace note make-check") {
		t.Fatalf("resumed card:\n%s", card)
	}
	live := m.renderEntryContent(entry{role: "tool", text: "reflection", call: &core.ToolCall{ID: "x", Name: "Memory", Arguments: call.Arguments}, done: true})
	if !strings.Contains(ansi.Strip(live), "Memory  reflection · save") {
		t.Fatalf("live card:\n%s", ansi.Strip(live))
	}
	own := m.renderEntryContent(entry{role: "tool", call: &core.ToolCall{ID: "call_1", Name: "Memory", Arguments: call.Arguments}, done: true})
	if strings.Contains(ansi.Strip(own), "reflection") {
		t.Fatal("the agent's own save marked as the reflection's")
	}
}

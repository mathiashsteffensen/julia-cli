package ui

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"julia/internal/core"
)

var ctrlF = tea.KeyPressMsg{Code: 'f', Mod: tea.ModCtrl}

// childEvents is a subagent's run as the parent stream carries it: opened by
// subagent_start, a tool call, a report, and closed by subagent_end.
func childEvents(id, role, task string) []core.Event {
	ref := &core.Subagent{ID: id, Role: role, Model: "fake/model"}
	call := &core.ToolCall{ID: id + "-call", Name: "Grep", Arguments: json.RawMessage(`{"pattern":"Approve","path":"internal/"}`)}
	usage := core.UsageEvent(core.TokenUsage{Input: 1_000, Output: 50}, 0)
	usage.Subagent = ref
	return []core.Event{
		{Kind: "subagent_start", Text: task, Subagent: ref},
		{Kind: "status", Text: "Working · step 1", Subagent: ref},
		{Kind: "tool_start", Call: call, Subagent: ref},
		{Kind: "tool_end", Call: call, Result: &core.ToolResult{Content: "3 matches"}, Subagent: ref},
		usage,
		{Kind: "text", Text: "**Found it** in tools.go.", Subagent: ref},
		{Kind: "subagent_end", Subagent: ref},
	}
}

// A child's events build its agent and never touch the conversation or the
// session's usage, which the parent's roll-up carries.
func TestSubagentEventsBuildTheFleetNotTheConversation(t *testing.T) {
	m := testModel(t, Config{})
	m.resize(120, 40)
	m.entries = []entry{{role: "assistant", text: "Delegating."}}
	for _, event := range childEvents("scout-1", "scout", "Find the approval path.") {
		m.applyEvent(event)
	}
	if len(m.entries) != 1 || m.toolCount != 0 || !m.live.Total.IsZero() {
		t.Fatalf("child events reached the conversation: %d entries, %d tools, usage %+v", len(m.entries), m.toolCount, m.live.Total)
	}
	if len(m.fleet) != 1 {
		t.Fatalf("fleet has %d agents", len(m.fleet))
	}
	a := m.fleet[0]
	if a.role != "scout" || a.model != "fake/model" || a.task != "Find the approval path." || a.state != agentDone || a.calls != 1 {
		t.Fatalf("agent = %+v", *a)
	}
	if a.usage.Input != 1_000 || a.usage.Output != 50 {
		t.Fatalf("agent usage = %+v", a.usage)
	}
	roles := []string{}
	for _, item := range a.entries {
		roles = append(roles, item.role)
	}
	if strings.Join(roles, " ") != "user tool assistant" || !a.entries[1].done || a.entries[1].result.Content != "3 matches" || a.entries[0].label != "TASK" || a.entries[2].label != "✦ scout" {
		t.Fatalf("agent transcript = %v", roles)
	}
	failed := &core.Subagent{ID: "oracle-1", Role: "oracle", Model: "fake/model"}
	m.applyEvent(core.Event{Kind: "subagent_start", Text: "Second opinion.", Subagent: failed})
	m.applyEvent(core.Event{Kind: "subagent_end", Text: "deadline exceeded", Subagent: failed})
	if m.fleet[1].state != agentFailed || m.fleet[1].entries[len(m.fleet[1].entries)-1].role != "warning" {
		t.Fatalf("failed agent = %+v", *m.fleet[1])
	}
	if m.fleetSummary() != "1 done · 1 failed" {
		t.Fatalf("summary = %q", m.fleetSummary())
	}
}

// Ctrl+F opens the window over the conversation with the draft kept, the
// arrows move through the agents, and Esc closes it and returns the composer.
func TestFleetOpensOnTheRunningAgentAndCycles(t *testing.T) {
	m := testModel(t, Config{})
	m.resize(120, 40)
	m.input.SetValue("my draft")
	for _, id := range []string{"a", "b"} {
		for _, event := range childEvents(id, "scout", "Task "+id) {
			m.applyEvent(event)
		}
	}
	running := &core.Subagent{ID: "c", Role: "worker", Model: "fake/model"}
	m.applyEvent(core.Event{Kind: "subagent_start", Text: "Task c", Subagent: running})
	m.applyEvent(core.Event{Kind: "subagent_start", Text: "Task d", Subagent: &core.Subagent{ID: "d", Role: "reviewer", Model: "fake/model"}})
	m.Update(ctrlF)
	if m.fleetView == nil || m.fleetView.selected != 2 || !m.fleetView.follow {
		t.Fatalf("fleet did not open on the first running agent: %+v", m.fleetView)
	}
	if m.input.Focused() {
		t.Fatal("composer still focused under the fleet")
	}
	view := ansi.Strip(m.View().Content)
	for _, want := range []string{"FLEET", "4 subagents", "2 running · 2 done", "3 / 4", "worker", "Task c", "DRAFT", "esc close"} {
		if !strings.Contains(view, want) {
			t.Errorf("fleet view omits %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "Task a") {
		t.Fatalf("another agent's transcript is shown:\n%s", view)
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	if m.fleetView.selected != 0 || m.fleetView.follow {
		t.Fatalf("up did not move to the first, finished agent: %+v", m.fleetView)
	}
	if view := ansi.Strip(m.View().Content); !strings.Contains(view, "Task a") || !strings.Contains(view, "Found it") || !strings.Contains(view, "1 / 4") {
		t.Fatalf("the first agent's transcript is not shown:\n%s", view)
	}
	for range 10 {
		m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	}
	if m.fleetView.selected != 3 {
		t.Fatalf("down ran past the last agent: %d", m.fleetView.selected)
	}
	m.Update(tea.KeyPressMsg{Code: 'x'})
	if m.input.Value() != "my draft" {
		t.Fatalf("a key under the fleet reached the draft: %q", m.input.Value())
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.fleetView != nil || !m.input.Focused() || m.input.Value() != "my draft" {
		t.Fatalf("esc did not close the fleet and return the composer: view %v focused %v draft %q", m.fleetView, m.input.Focused(), m.input.Value())
	}
	if view := ansi.Strip(m.View().Content); !strings.Contains(view, "ctrl+f fleet") || !strings.Contains(view, "FLEET") {
		t.Fatalf("the conversation does not point at the fleet:\n%s", view)
	}
}

// The window fits the terminal at every size, with or without agents, and a
// narrow one keeps the transcript by dropping the list.
func TestFleetFitsEverySize(t *testing.T) {
	for _, populated := range []bool{false, true} {
		for _, size := range [][2]int{{40, 15}, {60, 18}, {90, 30}, {120, 40}, {160, 50}} {
			m := testModel(t, Config{})
			if populated {
				m.fleet = m.demoFleet()
			}
			m.resize(size[0], size[1])
			m.Update(ctrlF)
			view := m.View().Content
			lines := strings.Split(view, "\n")
			if len(lines) > size[1] {
				t.Errorf("%v populated %v: %d rows", size, populated, len(lines))
			}
			for row, line := range lines {
				if ansi.StringWidth(line) > size[0] {
					t.Errorf("%v populated %v: row %d overflows: %q", size, populated, row, line)
				}
			}
			plain := ansi.Strip(view)
			if !strings.Contains(plain, "╭") || !strings.Contains(plain, "DRAFT") {
				t.Errorf("%v populated %v: window or composer missing:\n%s", size, populated, plain)
			}
			if populated && !strings.Contains(plain, "worker") {
				t.Errorf("%v: the running agent is not shown:\n%s", size, plain)
			}
			if !populated && !strings.Contains(plain, "No subagents yet") {
				t.Errorf("%v: no empty state:\n%s", size, plain)
			}
		}
	}
}

// A running agent's pane follows its newest activity until scrolled up, and
// Ctrl+End follows again.
func TestFleetFollowsARunningAgent(t *testing.T) {
	m := testModel(t, Config{})
	m.resize(100, 24)
	ref := &core.Subagent{ID: "w", Role: "worker", Model: "fake/model"}
	m.applyEvent(core.Event{Kind: "subagent_start", Text: "Edit many files.", Subagent: ref})
	for i := range 30 {
		call := &core.ToolCall{ID: fmt.Sprint(i), Name: "Read", Arguments: json.RawMessage(fmt.Sprintf(`{"path":"file-%d.go"}`, i))}
		m.applyEvent(core.Event{Kind: "tool_start", Call: call, Subagent: ref})
		m.applyEvent(core.Event{Kind: "tool_end", Call: call, Result: &core.ToolResult{Content: "ok"}, Subagent: ref})
	}
	m.Update(ctrlF)
	if view := ansi.Strip(m.View().Content); !strings.Contains(view, "file-29.go") || strings.Contains(view, "file-0.go") {
		t.Fatalf("pane does not follow the newest call:\n%s", view)
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyPgUp})
	m.Update(tea.KeyPressMsg{Code: tea.KeyPgUp})
	if m.fleetView.follow {
		t.Fatal("scrolling up kept following")
	}
	if view := ansi.Strip(m.View().Content); strings.Contains(view, "file-29.go") || !strings.Contains(view, "Reading the agent's work") {
		t.Fatalf("pane did not scroll up:\n%s", view)
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnd, Mod: tea.ModCtrl})
	if view := ansi.Strip(m.View().Content); !m.fleetView.follow || !strings.Contains(view, "file-29.go") {
		t.Fatalf("ctrl+end did not follow again:\n%s", view)
	}
}

// A resumed session's fleet comes from its children's saved transcripts.
func TestFleetLoadsFromChildSessions(t *testing.T) {
	now := time.Now()
	children := []*core.Session{
		{ID: "later", ParentID: "parent", Role: "reviewer", Provider: "codex", Model: "m", CreatedAt: now.Add(time.Minute), UpdatedAt: now.Add(2 * time.Minute),
			Messages: []core.Message{{Role: "user", Content: "Review it."}, {Role: "assistant", Content: "Looks fine."}}},
		{ID: "first", ParentID: "parent", Title: "Subagent scout: Find things", Provider: "codex", Model: "m", CreatedAt: now, UpdatedAt: now.Add(time.Minute), Usage: core.SessionUsage{Total: core.TokenUsage{Input: 10}},
			Messages: []core.Message{{Role: "user", Content: "Find things."}, {Role: "assistant", ToolCalls: []core.ToolCall{{ID: "c1", Name: "Grep", Arguments: json.RawMessage(`{"pattern":"x"}`)}}}, {Role: "tool", ToolCallID: "c1", Content: "2 matches"}, {Role: "assistant", Content: "Found two."}}},
	}
	m := testModel(t, Config{Session: &core.Session{ID: "parent", Provider: "codex"}, Children: func(parent string) ([]*core.Session, error) {
		if parent != "parent" {
			t.Errorf("asked for %q's children", parent)
		}
		return children, nil
	}})
	if len(m.fleet) != 2 || m.fleet[0].id != "first" || m.fleet[1].id != "later" {
		t.Fatalf("fleet = %v", m.fleet)
	}
	first := m.fleet[0]
	if first.role != "scout" || first.task != "Find things." || first.calls != 1 || first.state != agentDone || first.model != "codex/m" || first.usage.Input != 10 {
		t.Fatalf("first agent = %+v", *first)
	}
	if first.entries[1].result == nil || first.entries[1].result.Content != "2 matches" || first.entries[2].text != "Found two." {
		t.Fatalf("first agent transcript = %+v", first.entries)
	}
	m.resize(120, 40)
	m.Update(ctrlF)
	if view := ansi.Strip(m.View().Content); !strings.Contains(view, "2 / 2") || !strings.Contains(view, "Review it.") {
		t.Fatalf("loaded fleet opens on %s", view)
	}
}

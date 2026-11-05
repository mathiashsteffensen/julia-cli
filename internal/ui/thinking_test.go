package ui

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"julia/internal/core"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func TestThinkingToggleKeepsStreamAndDraftSeparate(t *testing.T) {
	m := testModel(t, Config{})
	m.resize(100, 30)
	m.input.SetValue("keep this draft")
	m.busy = true
	m.applyEvent(core.Event{Kind: "reasoning", Text: "First thought. "})
	m.applyEvent(core.Event{Kind: "reasoning", Text: "Second thought."})

	if m.showThinking || strings.Contains(ansi.Strip(m.viewport.View()), "First thought") {
		t.Fatal("thinking is visible by default")
	}
	if len(m.entries) != 1 || m.entries[0].text != "First thought. Second thought." {
		t.Fatalf("reasoning deltas were lost or split: %+v", m.entries)
	}
	m.Update(tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})
	if !m.showThinking || m.expandTools || !m.busy || m.input.Value() != "keep this draft" {
		t.Fatal("thinking toggle changed tools, task, or draft")
	}
	if !strings.Contains(ansi.Strip(m.viewport.View()), "First thought. Second thought.") {
		t.Fatal("toggle did not reveal thinking received while hidden")
	}

	m.applyEvent(core.Event{Kind: "reasoning", Text: " A live update."})
	m.applyEvent(core.Event{Kind: "text", Text: "The reply."})
	if len(m.entries) != 2 || m.entries[1].role != "assistant" || m.entries[1].text != "The reply." {
		t.Fatalf("thinking entered the reply: %+v", m.entries)
	}
	if !strings.Contains(ansi.Strip(m.viewport.View()), "A live update.") {
		t.Fatal("visible thinking did not update its cached layout")
	}
	m.Update(tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl})
	m.Update(tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})
	view := ansi.Strip(m.viewport.View())
	if m.showThinking || !m.expandTools || strings.Contains(view, "First thought") || !strings.Contains(view, "The reply.") {
		t.Fatalf("hiding thinking changed reply or tools: %s", view)
	}
	m.Update(tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})
	if !strings.Contains(ansi.Strip(m.viewport.View()), "A live update.") {
		t.Fatal("second expansion lost thinking")
	}
}

func TestThinkingHistoryIsHiddenWithoutAddingRows(t *testing.T) {
	call := core.ToolCall{ID: "read", Name: "Read"}
	session := &core.Session{Messages: []core.Message{
		{Role: "assistant", ToolCalls: []core.ToolCall{call}},
		{Role: "assistant", Reasoning: "Saved thinking only."},
		{Role: "assistant", ToolCalls: []core.ToolCall{call}},
		{Role: "assistant", Reasoning: "Saved thinking with an answer.", Content: "Saved reply."},
	}}
	m := testModel(t, Config{Session: session})
	m.resize(100, 30)
	hidden := strings.Join(m.lines, "\n")
	if strings.Contains(hidden, "Saved thinking") || len(m.lines) != len(m.rows) {
		t.Fatal("hidden history leaked thinking or misaligned copy rows")
	}
	for _, row := range m.rows {
		if row.entry >= 0 && m.entries[row.entry].role == "reasoning" {
			t.Fatal("hidden thinking has a copyable row")
		}
	}

	plain := testModel(t, Config{})
	plain.entries = slices.DeleteFunc(slices.Clone(m.entries), func(e entry) bool { return e.role == "reasoning" })
	plain.resize(100, 30)
	if hidden != strings.Join(plain.lines, "\n") {
		t.Fatal("hidden thinking changed spacing between visible entries")
	}

	m.Update(tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})
	shown := ansi.Strip(strings.Join(m.lines, "\n"))
	for _, text := range []string{"Saved thinking only.", "Saved thinking with an answer.", "Saved reply."} {
		if !strings.Contains(shown, text) {
			t.Fatalf("restored transcript lost %q", text)
		}
	}
	if len(m.lines) != len(m.rows) {
		t.Fatal("expanded history misaligned copy rows")
	}
}

func TestThinkingLayoutIsCompleteCopyableAndSafe(t *testing.T) {
	m := testModel(t, Config{})
	m.showThinking = true
	text := strings.Repeat("A longer thought with Unicode 日本語.\n", 20) + "Last thought."
	item := entry{role: "reasoning", text: text + "\x1b]52;c;bad\a"}
	for _, width := range []int{22, 40, 80, 160} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			m.resize(width, 25)
			layout := m.layoutEntry(item)
			if !strings.Contains(layout.text, "Last thought.") || strings.Contains(layout.text, "]52;c;bad") {
				t.Fatal("thinking was truncated or terminal controls were retained")
			}
			for _, line := range strings.Split(layout.text, "\n") {
				if ansi.StringWidth(line) > m.mainWidth {
					t.Fatalf("thinking exceeds column width: %q", line)
				}
			}
			if len(layout.targets) != 1 || layout.targets[0].label != "thinking" || layout.targets[0].text != item.text {
				t.Fatal("thinking has no full copy target")
			}
		})
	}
}

func TestThinkingToggleAndStreamPreserveScrolledReader(t *testing.T) {
	m := testModel(t, Config{})
	m.entries = []entry{{role: "assistant", text: strings.Repeat("History row.\n", 60)}}
	m.resize(80, 20)
	m.viewport.ScrollUp(12)
	before := m.viewport.YOffset()
	m.applyEvent(core.Event{Kind: "reasoning", Text: strings.Repeat("Thinking row.\n", 30)})
	m.Update(tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})
	if m.viewport.YOffset() != before {
		t.Fatal("showing thinking yanked the scrolled reader")
	}
	m.applyEvent(core.Event{Kind: "reasoning", Text: "More thinking."})
	if m.viewport.YOffset() != before {
		t.Fatal("streaming thinking yanked the scrolled reader")
	}
	m.viewport.GotoBottom()
	m.Update(tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})
	if !m.viewport.AtBottom() {
		t.Fatal("hiding thinking lost live follow")
	}
}

func TestFleetThinkingLiveAndRestored(t *testing.T) {
	for _, restored := range []bool{false, true} {
		t.Run(fmt.Sprint(restored), func(t *testing.T) {
			cfg := Config{}
			if restored {
				cfg.Children = func(string) ([]*core.Session, error) {
					return []*core.Session{{ID: "child", Role: "scout", Messages: []core.Message{
						{Role: "user", Content: "Inspect."},
						{Role: "assistant", Reasoning: "Child thinking.", Content: "Child reply."},
					}}}, nil
				}
			}
			m := testModel(t, cfg)
			m.resize(140, 40)
			if !restored {
				ref := &core.Subagent{ID: "child", Role: "scout"}
				m.applyEvent(core.Event{Kind: "subagent_start", Subagent: ref, Text: "Inspect."})
				m.applyEvent(core.Event{Kind: "reasoning", Subagent: ref, Text: "Child "})
				m.applyEvent(core.Event{Kind: "reasoning", Subagent: ref, Text: "thinking."})
				m.applyEvent(core.Event{Kind: "text", Subagent: ref, Text: "Child reply."})
			}
			if len(m.entries) != 0 {
				t.Fatal("child reasoning entered the main conversation")
			}
			m.openFleet()
			if strings.Contains(ansi.Strip(m.fleetBody()), "Child thinking.") {
				t.Fatal("fleet thinking is visible by default")
			}
			m.Update(tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})
			if !strings.Contains(ansi.Strip(m.fleetBody()), "Child thinking.") || !strings.Contains(ansi.Strip(m.fleetBody()), "Child reply.") {
				t.Fatal("fleet toggle lost thinking or reply")
			}
			m.Update(tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})
			if strings.Contains(ansi.Strip(m.fleetBody()), "Child thinking.") {
				t.Fatal("fleet toggle did not hide cached thinking")
			}
		})
	}
}

func TestThinkingFleetShortcutFitsTheFooter(t *testing.T) {
	m := testModel(t, Config{Demo: true})
	m.openFleet()
	for _, width := range []int{70, 80, 108, 120, 140} {
		m.resize(width, 24)
		footer := ansi.Strip(m.fleetFooter())
		if ansi.StringWidth(footer) > m.mainWidth || !strings.Contains(footer, "ctrl+t thinking") || !strings.HasSuffix(footer, "esc close") {
			t.Fatalf("width %d: footer does not fit: %q", width, footer)
		}
	}
}

func TestThinkingDoesNotEnterHeadlessReply(t *testing.T) {
	var reply, work strings.Builder
	transcript := newTranscript(
		transcriptOutput{writer: &reply, width: func() int { return 80 }},
		transcriptOutput{writer: &work, width: func() int { return 80 }},
	)
	transcript.Show(core.Event{Kind: "reasoning", Text: "Display-only thinking."})
	transcript.Show(core.Event{Kind: "text", Text: "Answer."})
	if reply.String() != "Answer." || work.String() != "" {
		t.Fatalf("thinking leaked into the headless transcript: reply %q, work %q", reply.String(), work.String())
	}
}

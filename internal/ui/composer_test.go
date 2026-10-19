package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// A multi-line draft grows the composer and takes rows from the transcript,
// but the composer panel never covers more than half the screen.
func TestTheComposerGrowsUpToHalfTheScreen(t *testing.T) {
	m := testModel(t, Config{})
	m.resize(100, 40)
	body := m.bodyHeight
	if m.input.Height() != 2 {
		t.Fatalf("empty composer has %d rows", m.input.Height())
	}
	newline := tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModShift}
	for range 3 {
		m.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
		m.Update(newline)
	}
	if m.input.Height() != 4 || m.bodyHeight != body-2 {
		t.Fatalf("four lines: %d rows, body %d (was %d)", m.input.Height(), m.bodyHeight, body)
	}
	for range 40 {
		m.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
		m.Update(newline)
	}
	if got := strings.Count(m.input.Value(), "\n"); got != 43 {
		t.Fatalf("draft has %d newlines, want 43", got)
	}
	composer := m.composerStyle().GetVerticalFrameSize() + 1 + m.input.Height()
	if composer > m.height/2 || composer < m.height/2-1 {
		t.Fatalf("composer is %d rows on a %d row screen", composer, m.height)
	}
	if got := strings.Count(m.View().Content, "\n") + 1; got != m.height {
		t.Fatalf("view is %d rows, want %d", got, m.height)
	}
	m.input.Reset()
	m.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
	if m.input.Height() != 2 || m.bodyHeight != body {
		t.Fatalf("after reset: %d rows, body %d (want %d)", m.input.Height(), m.bodyHeight, body)
	}
}

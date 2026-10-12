package ui

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/atotto/clipboard"
	"github.com/charmbracelet/x/ansi"
)

// The terminal's own selection cannot work here: mouse reporting is on, and a
// row carries borders, gutters, and the sidebar. JULIA selects and copies
// itself, from a map of every conversation row back to the text it shows.
//
//   drag          copies the selection, without gutters or card padding
//   click a code block   copies its code
//   double-click  copies the whole message, command, or output under the cursor

var selectionStyle = lipgloss.NewStyle().Background(lipgloss.Color("#4A3F6B")).Foreground(cream)

const doubleClick = 400 * time.Millisecond

type copyTarget struct {
	label string
	text  string
}

// rowMeta maps one rendered row of an entry back to copyable text.
type rowMeta struct {
	gutter int  // decoration cells before the text
	end    int  // cell where the text ends, 0 for the row's end; code cards pad past it
	cont   bool // continues a wrapped row; a copy joins them without a newline
	skip   bool // a label or card edge: never part of a dragged selection
	target int  // index into the entry's targets that a double-click copies
	code   bool // inside a code card: a single click copies the target
}

type entryLayout struct {
	text    string
	rows    []rowMeta
	targets []copyTarget
}

// rowRef locates a conversation row; entry is -1 for the gap between entries.
type rowRef struct {
	entry int
	rowMeta
}

type cell struct{ row, col int }

type selection struct {
	anchor, head cell
	dragging     bool
	moved        bool
	flash        *rowRef // set while a copied message or block is lit up
	flashSeq     uint64
}

type copiedMsg struct {
	label, text string
	err         error
}

type flashDoneMsg struct{ seq uint64 }
type copyNoticeDoneMsg struct{ notice string }

var errRemoteSession = errors.New("remote session: the local clipboard is not this machine's")

// systemClipboard writes through pbcopy and friends. Over SSH that would land on
// the wrong machine, so the terminal's OSC 52 is used instead.
func systemClipboard(text string) error {
	if os.Getenv("SSH_TTY") != "" || os.Getenv("SSH_CONNECTION") != "" {
		return errRemoteSession
	}
	return clipboard.WriteAll(text)
}

func (m *model) copyText(label, text string) tea.Cmd {
	text = strings.Trim(sanitize(text), "\n")
	if strings.TrimSpace(text) == "" {
		m.notice = "Nothing to copy there."
		return nil
	}
	write := m.clipboard
	return func() tea.Msg { return copiedMsg{label: label, text: text, err: write(text)} }
}

func (m *model) receiveCopied(msg copiedMsg) tea.Cmd {
	lines := strings.Count(msg.text, "\n") + 1
	notice := "✓ Copied " + msg.label
	if lines > 1 {
		notice += fmt.Sprintf(" · %d lines", lines)
	} else {
		notice += fmt.Sprintf(" · %d characters", len([]rune(msg.text)))
	}
	m.notice = notice
	clear := tea.Tick(3*time.Second, func(time.Time) tea.Msg { return copyNoticeDoneMsg{notice} })
	if msg.err != nil {
		return tea.Batch(tea.SetClipboard(msg.text), clear)
	}
	return clear
}

// conversationShown reports whether the body is the scrollable conversation.
func (m *model) conversationShown() bool {
	return m.pending == nil && m.picker == nil && m.question == nil && len(m.entries) > 0
}

// cellAt maps a terminal position to a conversation cell. The header takes
// three rows; the body starts at the left margin.
func (m *model) cellAt(x, y int) (cell, bool) {
	row, col := y-3, max(0, x-m.margin()) // a press in the margin starts at the edge
	if !m.conversationShown() || row < 0 || row >= m.viewport.Height() || col >= m.mainWidth {
		return cell{}, false
	}
	row += m.viewport.YOffset()
	if row >= len(m.rows) {
		return cell{}, false
	}
	return cell{row, col}, true
}

// dragCell follows a drag past the body's edges, scrolling as it goes.
func (m *model) dragCell(x, y int) cell {
	row := y - 3
	if row < 0 {
		m.viewport.ScrollUp(1)
		row = 0
	} else if row >= m.viewport.Height() {
		m.viewport.ScrollDown(1)
		row = m.viewport.Height() - 1
	}
	row = min(row+m.viewport.YOffset(), len(m.rows)-1)
	return cell{max(0, row), min(max(0, x-m.margin()), m.mainWidth-1)}
}

func (m *model) mouseDown(msg tea.MouseClickMsg) tea.Cmd {
	m.selection = nil
	if msg.Button != tea.MouseLeft {
		return nil
	}
	if at, ok := m.cellAt(msg.X, msg.Y); ok {
		m.selection = &selection{anchor: at, head: at, dragging: true}
	}
	return nil
}

func (m *model) mouseDrag(msg tea.MouseMotionMsg) tea.Cmd {
	s := m.selection
	if s == nil || !s.dragging || len(m.rows) == 0 {
		return nil
	}
	if at := m.dragCell(msg.X, msg.Y); at != s.head {
		s.head, s.moved = at, true
	}
	return nil
}

func (m *model) mouseUp(msg tea.MouseReleaseMsg) tea.Cmd {
	s := m.selection
	if s == nil || !s.dragging {
		return nil
	}
	s.dragging = false
	if s.moved {
		return m.copyText("selection", m.selectedText())
	}
	m.selection = nil
	ref := m.rows[s.anchor.row]
	double := time.Since(m.lastClick.at) < doubleClick && m.lastClick.row == s.anchor.row
	m.lastClick.at, m.lastClick.row = time.Now(), s.anchor.row
	if ref.entry < 0 || !(double || ref.code) {
		return nil
	}
	layout := m.entries[ref.entry].layout
	if ref.target >= len(layout.targets) {
		return nil
	}
	target := layout.targets[ref.target]
	m.flashSeq++
	m.selection = &selection{flash: &ref, flashSeq: m.flashSeq}
	seq := m.flashSeq
	return tea.Batch(m.copyText(target.label, target.text), tea.Tick(450*time.Millisecond, func(time.Time) tea.Msg { return flashDoneMsg{seq} }))
}

// span returns the selected cells of a content row, or ok=false.
func (m *model) span(row int, width int) (from, to int, ok bool) {
	s := m.selection
	if s == nil || row >= len(m.rows) {
		return 0, 0, false
	}
	ref := m.rows[row]
	if s.flash != nil {
		if ref.entry != s.flash.entry || (s.flash.target != 0 && ref.target != s.flash.target) {
			return 0, 0, false
		}
		if ref.code {
			return 1, ref.end + 1, true // the whole card, which starts after the reply's one-cell indent
		}
		from, to = ref.gutter, width
		if ref.end > 0 {
			to = min(to, ref.end)
		}
		return from, to, from < to
	}
	start, end := s.anchor, s.head
	if end.row < start.row || end.row == start.row && end.col < start.col {
		start, end = end, start
	}
	if !s.moved || ref.skip || row < start.row || row > end.row {
		return 0, 0, false
	}
	from, to = ref.gutter, width
	if row == start.row {
		from = max(from, start.col)
	}
	if row == end.row {
		to = min(to, end.col+1)
	}
	if ref.end > 0 {
		to = min(to, ref.end)
	}
	return from, to, from < to
}

func (m *model) selectedText() string {
	s := m.selection
	if s == nil {
		return ""
	}
	first, last := min(s.anchor.row, s.head.row), max(s.anchor.row, s.head.row)
	var out []string
	joinNext := false
	for row := first; row <= last && row < len(m.lines); row++ {
		plain := ansi.Strip(m.lines[row])
		from, to, ok := m.span(row, ansi.StringWidth(plain))
		if m.rows[row].skip {
			joinNext = false
			continue
		}
		text := ""
		if ok {
			text = ansi.Cut(plain, from, to)
		}
		continues := row+1 <= last && row+1 < len(m.rows) && m.rows[row+1].cont
		if !continues {
			text = strings.TrimRight(text, " ")
		}
		if joinNext && m.rows[row].cont && len(out) > 0 {
			out[len(out)-1] += text
		} else {
			out = append(out, text)
		}
		joinNext = true
	}
	return strings.Join(out, "\n")
}

// highlightSelection paints the selection onto the visible conversation rows.
func (m *model) highlightSelection(body string) string {
	if m.selection == nil || !m.conversationShown() {
		return body
	}
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		row := m.viewport.YOffset() + i
		width := ansi.StringWidth(line)
		if row < len(m.lines) {
			width = min(width, ansi.StringWidth(strings.TrimRight(ansi.Strip(m.lines[row]), " ")))
		}
		from, to, ok := m.span(row, width)
		if !ok {
			continue
		}
		lines[i] = ansi.Cut(line, 0, from) + selectionStyle.Render(ansi.Strip(ansi.Cut(line, from, to))) + ansi.Cut(line, to, ansi.StringWidth(line))
	}
	return strings.Join(lines, "\n")
}

// copyCommand handles /copy (the last reply) and /copy code (the last code block).
func (m *model) copyCommand(prompt string) (bool, tea.Cmd) {
	arg, ok := strings.CutPrefix(prompt, "/copy")
	if !ok || arg != "" && arg[0] != ' ' {
		return false, nil
	}
	wantCode := strings.TrimSpace(arg) == "code"
	if !wantCode && strings.TrimSpace(arg) != "" {
		m.notice = "Use /copy for the last reply or /copy code for the last code block."
		return true, nil
	}
	m.input.Reset()
	for i := len(m.entries) - 1; i >= 0; i-- {
		item := &m.entries[i]
		if item.role != "assistant" {
			continue
		}
		layout := m.renderEntry(item)
		if !wantCode {
			return true, m.copyText(layout.targets[0].label, layout.targets[0].text)
		}
		if len(layout.targets) > 1 {
			target := layout.targets[len(layout.targets)-1]
			return true, m.copyText(target.label, target.text)
		}
	}
	m.notice = "No reply to copy yet."
	if wantCode {
		m.notice = "No code block to copy yet."
	}
	return true, nil
}

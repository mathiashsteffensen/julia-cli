package ui

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// A Terminal result is drawn as the screen it describes: a card like a code
// block's, titled with the terminal and its state, holding the screen's rows
// with the cursor lit. The UI reads the same text the model does, so a resumed
// session, which keeps only that text, draws the same card.

// terminalSummary titles a Terminal call by its action, terminal and input, as
// in "start · npm run dev" or "send t1 · ctrl+c".
func terminalSummary(arguments json.RawMessage) (string, bool) {
	var a struct {
		Action, ID, Command, Text string
		Network                   bool
		Keys                      []string
		Cols, Rows                int
		Mouse                     *struct {
			Action, Text string
			Row, Col     int
		}
	}
	if json.Unmarshal(arguments, &a) != nil || a.Action == "" {
		return "", false
	}
	title := a.Action
	if a.ID != "" {
		title += " " + a.ID
	}
	var parts []string
	switch a.Action {
	case "start":
		parts = append(parts, typedLine(a.Command))
		if a.Network {
			parts = append(parts, "network")
		}
	case "send":
		if a.Text != "" {
			parts = append(parts, typedLine(a.Text))
		}
		if len(a.Keys) > 0 {
			parts = append(parts, strings.Join(a.Keys, " "))
		}
		switch m := a.Mouse; {
		case m != nil && m.Text != "":
			parts = append(parts, m.Action+" "+strconv.Quote(m.Text))
		case m != nil:
			parts = append(parts, fmt.Sprintf("%s %d,%d", m.Action, m.Row, m.Col))
		}
	case "resize":
		parts = append(parts, fmt.Sprintf("%d×%d", a.Cols, a.Rows))
	}
	for _, part := range parts {
		if part != "" {
			title += " · " + part
		}
	}
	return title, true
}

// typedLine shows newlines as ↵, which is also how typed text presses Enter.
func typedLine(text string) string {
	return strings.TrimSpace(strings.ReplaceAll(text, "\n", " ↵ "))
}

// terminalScreen is a Terminal result as parsed from its text: the label line,
// notes about the wait, the snapshot's header, and its rows.
type terminalScreen struct {
	label, header, state string
	notes                []string
	scrollback, rows     []string
	blank                string // the note for trailing blank rows, such as "rows 6–30 blank"
	cursorRow, cursorCol int    // 1-based; zero when the cursor is hidden
	size                 string // "100×30"
}

var (
	screenRowPattern    = regexp.MustCompile(`^(-?\d+)\t(.*)$`)
	screenHeaderPattern = regexp.MustCompile(`^screen (\d+×\d+)( · |$)`)
	screenCursorPattern = regexp.MustCompile(` · cursor row (\d+), col (\d+)`)
)

func parseTerminalScreen(content string) (terminalScreen, bool) {
	var s terminalScreen
	lines := strings.Split(content, "\n")
	at := -1
	for i, line := range lines {
		if m := screenHeaderPattern.FindStringSubmatch(line); m != nil {
			at, s.size = i, m[1]
			break
		}
	}
	if at < 1 {
		return s, false
	}
	s.label, s.header = lines[0], lines[at]
	for _, note := range lines[1:at] {
		if note = strings.TrimSpace(note); note != "" {
			s.notes = append(s.notes, note)
		}
	}
	for _, part := range strings.Split(s.header, " · ") {
		if part == "running" || part == "stopped" || strings.HasPrefix(part, "exited") || strings.HasPrefix(part, "killed") {
			s.state = part
		}
	}
	if m := screenCursorPattern.FindStringSubmatch(s.header); m != nil {
		s.cursorRow, _ = strconv.Atoi(m[1])
		s.cursorCol, _ = strconv.Atoi(m[2])
	}
	for _, line := range lines[at+1:] {
		if m := screenRowPattern.FindStringSubmatch(line); m != nil {
			if strings.HasPrefix(m[1], "-") {
				s.scrollback = append(s.scrollback, m[2])
			} else {
				s.rows = append(s.rows, m[2])
			}
		} else if strings.HasPrefix(line, "[") && strings.HasSuffix(line, " blank]") {
			s.blank = strings.Trim(line, "[]")
		}
	}
	return s, true
}

// text is what copying the card gives: the rows as they read, history first.
func (s terminalScreen) text() string {
	return strings.Join(append(append([]string(nil), s.scrollback...), s.rows...), "\n")
}

// summary is the collapsed card's one row: the terminal, its state, and any
// note, such as a wait that ran out.
func (s terminalScreen) summary() string {
	row := faintStyle.Render(s.label)
	if s.state != "" {
		row += faintStyle.Render(" · ") + stateStyle(s.state).Render(s.state)
	}
	for _, note := range s.notes {
		row += faintStyle.Render(" · ") + mutedStyle.Italic(true).Render(note)
	}
	return row
}

// stateStyle colors a program's state: green while it runs, muted when it
// finished or was stopped on purpose, coral when it failed or was killed.
func stateStyle(state string) lipgloss.Style {
	switch state {
	case "running":
		return lipgloss.NewStyle().Foreground(green)
	case "exited with code 0", "stopped":
		return mutedStyle
	}
	return coralStyle
}

// maxCardScrollback is the most history a card shows above the screen.
const maxCardScrollback = 40

// card draws the screen in at most width cells: a header naming the terminal
// and its state, the scrollback above a rule, then the screen with its cursor.
// The card is as wide as the screen, so its right edge is the program's; a
// screen too wide for it is cut cleanly at the edge, not wrapped, since a
// screen is a grid, and the header says how much is hidden. edge marks header
// and padding rows, which are never selected.
func (s terminalScreen) card(width int) (rows []string, edge []bool) {
	width = max(codeGutter+8, width)
	size, hidden := s.size, 0
	var cols, lines int
	if _, err := fmt.Sscanf(s.size, "%d×%d", &cols, &lines); err == nil && cols > 0 {
		if hidden = cols - (width - codeGutter - 1); hidden > 0 {
			size += fmt.Sprintf(" · %d cols hidden", hidden)
		} else {
			// The header keeps room for the terminal's name and state.
			header := codeGutter + ansi.StringWidth(s.label) + ansi.StringWidth(s.state) + ansi.StringWidth(size) + 6
			width = min(width, max(cols+codeGutter+1, header))
		}
	}
	inner := width - codeGutter - 1
	fill := lipgloss.NewStyle().Background(codeBackground)
	header := lipgloss.NewStyle().Background(codeHeaderColor)
	pad := func(row string) string {
		return row + fill.Render(strings.Repeat(" ", max(0, width-ansi.StringWidth(row))))
	}
	add := func(row string, isEdge bool) {
		rows, edge = append(rows, row), append(edge, isEdge)
	}

	// The right of the header gives way to the terminal's name: first what
	// is hidden, then the size, then the state.
	var right string
	for _, choice := range [][2]string{{s.state, size}, {s.state, s.size}, {s.state, ""}, {}} {
		var parts []string
		if choice[0] != "" {
			parts = append(parts, header.Inherit(stateStyle(choice[0])).Render(choice[0]))
		}
		if choice[1] != "" {
			parts = append(parts, header.Foreground(muted).Render(choice[1]))
		}
		right = ""
		if len(parts) > 0 {
			right = strings.Join(parts, header.Foreground(faint).Render(" · ")) + header.Render(" ")
		}
		if width-codeGutter-ansi.StringWidth(right)-2 >= min(ansi.StringWidth(s.label), 12) {
			break
		}
	}
	left := header.Render(strings.Repeat(" ", codeGutter)) + header.Foreground(violet).Bold(true).Render(ansi.Truncate(s.label, max(1, width-codeGutter-ansi.StringWidth(right)-2), "…"))
	add(left+header.Render(strings.Repeat(" ", max(0, width-ansi.StringWidth(left)-ansi.StringWidth(right))))+right, true)
	add(fill.Render(strings.Repeat(" ", width)), true)
	gutter := fill.Render(strings.Repeat(" ", codeGutter))
	// History is context for the screen, so a long one shows only its end.
	scrollback := s.scrollback
	if hidden := len(scrollback) - maxCardScrollback; hidden > 0 {
		add(pad(gutter+fill.Foreground(faint).Italic(true).Render(fmt.Sprintf("%d earlier lines", hidden))), true)
		scrollback = scrollback[hidden:]
	}
	for _, line := range scrollback {
		add(pad(gutter+fill.Foreground(muted).Render(ansi.Truncate(line, inner, ""))), false)
	}
	if len(s.scrollback) > 0 {
		add(gutter+fill.Foreground(faint).Render(strings.Repeat("┄", inner))+fill.Render(" "), true)
	}
	text := fill.Foreground(cream)
	for i, line := range s.rows {
		line = ansi.Truncate(line, inner, "")
		if i+1 != s.cursorRow || s.state != "running" || s.cursorCol > inner {
			add(pad(gutter+text.Render(line)), false)
			continue
		}
		// The cursor is drawn as a lit cell, past the text when it ends there.
		col := s.cursorCol - 1
		line += strings.Repeat(" ", max(0, col+1-ansi.StringWidth(line)))
		under := ansi.Cut(line, col, col+1)
		if under == "" {
			under = " "
		}
		cursor := lipgloss.NewStyle().Background(cream).Foreground(codeBackground).Render(under)
		add(pad(gutter+text.Render(ansi.Cut(line, 0, col))+cursor+text.Render(ansi.TruncateLeft(line, col+ansi.StringWidth(under), ""))), false)
	}
	if s.blank != "" {
		add(pad(gutter+fill.Foreground(faint).Italic(true).Render(s.blank)), true)
	}
	add(fill.Render(strings.Repeat(" ", width)), true)
	return rows, edge
}

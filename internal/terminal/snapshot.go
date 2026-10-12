package terminal

import (
	"fmt"
	"image/color"
	"strconv"
	"strings"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
)

// Position is a 1-based cell position.
type Position struct{ Row, Col int }

// Snapshot is the terminal at one instant: its text for a model, and the
// styled cells behind Render.
type Snapshot struct {
	Cols, Rows int
	// Lines are the visible rows with trailing spaces trimmed. A wide
	// character occupies two columns but appears once.
	Lines []string
	// Scrollback holds the last lines that scrolled off the main screen,
	// oldest first, when requested.
	Scrollback     []string
	Cursor         Position
	CursorVisible  bool
	AltScreen      bool
	Mouse          string // tracking the program enabled: "", "press", "click", "drag" or "motion"
	MouseEncoding  string // "x10", "utf8", "urxvt", "sgr" or "sgr-pixels"
	BracketedPaste bool
	AppCursorKeys  bool
	Title          string
	State          State
	// Foreground names the command in the terminal's foreground when it is
	// not the program itself: what a shell is still running.
	Foreground string
	// Frozen means the emulator failed on the program's output: the screen
	// is as it was then and will not change again.
	Frozen bool

	cells       []cell // Rows×Cols, row-major
	fg, bg, cur color.RGBA
	cursorKind  vt.CursorStyle
}

type cell struct {
	text       string
	width      int // 0 for the second column of a wide character
	fg, bg, ul color.RGBA
	attrs      uint8
	underline  ansi.Underline
}

// Snapshot captures the screen and up to scrollback lines of history.
func (s *Session) Snapshot(scrollback int) Snapshot {
	foreground := s.foreground()
	s.mu.Lock()
	defer s.mu.Unlock()
	var snap Snapshot
	if s.frozen != nil {
		snap = *s.frozen
		snap.Scrollback = snap.Scrollback[max(0, len(snap.Scrollback)-scrollback):]
	} else {
		snap = s.capture(scrollback)
	}
	snap.State = s.state
	if !s.state.Exited && !s.reaped {
		snap.Foreground = foreground
	}
	return snap
}

// capture reads the emulator; s.mu must be held.
func (s *Session) capture(scrollback int) Snapshot {
	emu := s.emu
	pos := emu.CursorPosition()
	snap := Snapshot{
		Cols: s.cols, Rows: s.rows,
		Lines:          screenLines(emu, s.cols, s.rows),
		Cursor:         Position{Row: pos.Y + 1, Col: pos.X + 1},
		CursorVisible:  s.cursorOn,
		AltScreen:      emu.IsAltScreen(),
		BracketedPaste: s.dec[2004],
		AppCursorKeys:  s.dec[1],
		Title:          s.title,
		fg:             rgba(emu.ForegroundColor()),
		bg:             rgba(emu.BackgroundColor()),
		cur:            rgba(emu.CursorColor()),
		cursorKind:     s.cursorKind,
	}
	snap.Mouse, snap.MouseEncoding = s.mouseMode()
	if sb := emu.Scrollback(); sb != nil && scrollback > 0 {
		lines := sb.Lines()
		for _, line := range lines[max(0, len(lines)-scrollback):] {
			snap.Scrollback = append(snap.Scrollback, lineText(line))
		}
	}
	snap.cells = make([]cell, 0, s.cols*s.rows)
	for y := range s.rows {
		for x := range s.cols {
			c := emu.CellAt(x, y)
			if c == nil {
				snap.cells = append(snap.cells, cell{text: " ", width: 1, fg: snap.fg, bg: snap.bg})
				continue
			}
			snap.cells = append(snap.cells, cell{
				text: c.Content, width: c.Width, attrs: c.Style.Attrs, underline: c.Style.Underline,
				fg: s.resolve(c.Style.Fg, snap.fg),
				bg: s.resolve(c.Style.Bg, snap.bg),
				ul: s.resolve(c.Style.UnderlineColor, color.RGBA{}),
			})
		}
	}
	return snap
}

// mouseMode names the strongest tracking mode set and its encoding.
func (s *Session) mouseMode() (tracking, encoding string) {
	switch {
	case s.dec[1003]:
		tracking = "motion"
	case s.dec[1002]:
		tracking = "drag"
	case s.dec[1000], s.dec[1001]:
		tracking = "click"
	case s.dec[9]:
		tracking = "press"
	}
	switch {
	case s.dec[1016]:
		encoding = "sgr-pixels"
	case s.dec[1006]:
		encoding = "sgr"
	case s.dec[1015]:
		encoding = "urxvt"
	case s.dec[1005]:
		encoding = "utf8"
	default:
		encoding = "x10"
	}
	return tracking, encoding
}

// resolve maps a cell colour to RGB through the emulator's palette, which the
// program may have changed with OSC 4.
func (s *Session) resolve(c color.Color, fallback color.RGBA) color.RGBA {
	switch c := c.(type) {
	case nil:
		return fallback
	case ansi.BasicColor:
		return rgba(s.emu.IndexedColor(int(c)))
	case ansi.IndexedColor:
		return rgba(s.emu.IndexedColor(int(c)))
	}
	if _, _, _, a := c.RGBA(); a == 0 {
		return fallback
	}
	return rgba(c)
}

func rgba(c color.Color) color.RGBA {
	if c == nil {
		return color.RGBA{}
	}
	return color.RGBAModel.Convert(c).(color.RGBA)
}

func screenLines(emu *vt.Emulator, cols, rows int) []string {
	lines := make([]string, rows)
	line := make(uv.Line, cols)
	for y := range rows {
		for x := range cols {
			if c := emu.CellAt(x, y); c != nil {
				line[x] = *c
			} else {
				line[x] = uv.EmptyCell
			}
		}
		lines[y] = lineText(line)
	}
	return lines
}

func lineText(line uv.Line) string {
	var b strings.Builder
	for _, c := range line {
		switch {
		case c.Width == 0 && c.Content == "": // the second column of a wide character
		case c.Content == "":
			b.WriteByte(' ')
		default:
			b.WriteString(c.Content)
		}
	}
	return strings.TrimRight(b.String(), " ")
}

// Text renders the snapshot for a model: a one-line header, then each row
// numbered from 1 and separated from its text by a tab, as the Read tool
// numbers lines. Trailing blank rows are collapsed into a note, and a last
// note lists the highlighted runs, because TUIs mark the selected item and
// the focused pane with colour alone.
//
//	screen 80×24 · cursor row 3, col 3 · running · title "demo"
//	1	╭────────╮
//	2	│ hello  │
//	3	╰────────╯
//	[rows 4–24 blank]
//	[highlighted: row 2, cols 3–7]
func (s Snapshot) Text() string {
	var b strings.Builder
	b.WriteString(s.header())
	b.WriteByte('\n')
	if n := len(s.Scrollback); n > 0 {
		fmt.Fprintf(&b, "scrollback, last %d lines, numbered up from the screen:\n", n)
		for i, line := range s.Scrollback {
			fmt.Fprintf(&b, "%d\t%s\n", i-n, line)
		}
		b.WriteString("screen:\n")
	}
	last := len(s.Lines) - 1
	for last >= 0 && s.Lines[last] == "" {
		last--
	}
	for i := range last + 1 {
		fmt.Fprintf(&b, "%d\t%s\n", i+1, s.Lines[i])
	}
	switch blank := s.Rows - last - 1; {
	case last < 0:
		fmt.Fprintf(&b, "[all %d rows blank]", s.Rows)
	case blank == 1:
		fmt.Fprintf(&b, "[row %d blank]", s.Rows)
	case blank > 1:
		fmt.Fprintf(&b, "[rows %d–%d blank]", last+2, s.Rows)
	}
	text := strings.TrimSuffix(b.String(), "\n")
	if runs := s.highlights(); len(runs) > 0 {
		text += "\n[highlighted: " + strings.Join(runs, "; ") + "]"
	}
	return text
}

// Runs shorter than minHighlight cells are not reported, nor more than
// maxHighlights of them: a TUI's own cursor or a coloured glyph is noise.
const (
	minHighlight  = 3
	maxHighlights = 8
)

// highlights describes the runs of cells whose background, after reverse
// video, differs from the screen's commonest one, as "row 6, cols 2–33".
func (s Snapshot) highlights() []string {
	if len(s.cells) != s.Rows*s.Cols {
		return nil
	}
	background := func(c *cell) color.RGBA {
		if c.attrs&uv.AttrReverse != 0 {
			return c.fg
		}
		return c.bg
	}
	counts := map[color.RGBA]int{}
	for i := range s.cells {
		counts[background(&s.cells[i])]++
	}
	common := s.bg
	for c, n := range counts {
		if n > counts[common] {
			common = c
		}
	}
	var runs []string
	more := 0
	for row := range s.Rows {
		for col := 0; col < s.Cols; {
			start := col
			for col < s.Cols && background(&s.cells[row*s.Cols+col]) != common {
				col++
			}
			switch {
			case col-start < minHighlight:
				col++
			case len(runs) == maxHighlights:
				more++
			default:
				runs = append(runs, fmt.Sprintf("row %d, cols %d–%d", row+1, start+1, col))
			}
		}
	}
	if more > 0 {
		runs = append(runs, fmt.Sprintf("%d more", more))
	}
	return runs
}

func (s Snapshot) header() string {
	parts := []string{fmt.Sprintf("screen %d×%d", s.Cols, s.Rows)}
	if s.CursorVisible {
		parts = append(parts, fmt.Sprintf("cursor row %d, col %d", s.Cursor.Row, s.Cursor.Col))
	} else {
		parts = append(parts, "cursor hidden")
	}
	if s.AltScreen {
		parts = append(parts, "alt screen")
	}
	if s.Mouse != "" {
		parts = append(parts, "mouse on")
	}
	if s.Frozen {
		parts = append(parts, "frozen")
	}
	parts = append(parts, s.State.String())
	if s.Foreground != "" {
		parts = append(parts, "foreground job "+strconv.Quote(s.Foreground))
	}
	if s.Title != "" {
		parts = append(parts, "title "+strconv.Quote(s.Title))
	}
	return strings.Join(parts, " · ")
}

// Find returns the position of the first occurrence of text on the screen,
// scanning rows top to bottom, or only row when it is not 0, so a caller can
// aim the mouse at a label.
func (s Snapshot) Find(text string, row int) (Position, bool) {
	if text == "" {
		return Position{}, false
	}
	for y, line := range s.Lines {
		if row != 0 && y+1 != row {
			continue
		}
		if i := strings.Index(line, text); i >= 0 {
			return Position{Row: y + 1, Col: ansi.StringWidth(line[:i]) + 1}, true
		}
	}
	return Position{}, false
}

package ui

import (
	"context"
	"io"
	"strings"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// cardScreen is the screen of a review that goes through cards one at a
// time, such as julia mine review: a title, a selection line, the card in a
// framed viewport that scrolls, a status line and two help rows. A short
// terminal drops rows and keeps as much of the card as fits.
type cardScreen struct {
	viewport      viewport.Model
	width, height int
	// notice is the status line: what the last key did.
	notice string
	// wordWrap breaks lines between words, for cards of prose.
	wordWrap bool
}

func newCardScreen() cardScreen {
	return cardScreen{viewport: viewport.New(), width: 80, height: 24}
}

// runCardReview runs a card review as its own program, without a chat session.
func runCardReview(ctx context.Context, review tea.Model, input io.Reader, output io.Writer) error {
	options := []tea.ProgramOption{}
	if ctx != nil {
		options = append(options, tea.WithContext(ctx))
	}
	if input != nil {
		options = append(options, tea.WithInput(input))
	}
	if output != nil {
		options = append(options, tea.WithOutput(output))
	}
	_, err := tea.NewProgram(review, options...).Run()
	return err
}

func (s *cardScreen) framed() bool { return s.width >= 4 && s.height >= 8 }

// showCard sizes the viewport to the space the rows render draws leave, and
// shows content in it.
func (s *cardScreen) showCard(content string) {
	bodyWidth := s.width
	if s.framed() {
		bodyWidth -= 2
	}
	reserved := 1 // title
	if s.height >= 4 {
		reserved++ // selection
	}
	if s.height >= 5 {
		reserved++ // status
	}
	if s.height >= 3 {
		reserved++ // first help row
	}
	if s.height >= 8 {
		reserved++ // second help row
	}
	if s.framed() {
		reserved += 2
	}
	s.viewport.SetWidth(max(1, bodyWidth))
	s.viewport.SetHeight(max(1, s.height-reserved))

	content = strings.ReplaceAll(sanitize(content), "\t", "    ")
	if s.wordWrap {
		s.viewport.SetContent(ansi.Wrap(content, max(1, bodyWidth), "/"))
		return
	}
	// Hard wrapping preserves even long tool arguments and results, including
	// spaces, instead of truncating them at the viewport edge.
	s.viewport.SetContent(ansi.Hardwrap(content, max(1, bodyWidth), true))
}

// scrollCard scrolls the card for the mouse wheel or a scrolling key, and
// reports whether msg was one.
func (s *cardScreen) scrollCard(msg tea.Msg) bool {
	page := max(1, s.viewport.Height()-1)
	switch msg := msg.(type) {
	case tea.MouseWheelMsg:
		switch msg.Button {
		case tea.MouseWheelUp:
			s.viewport.ScrollUp(3)
		case tea.MouseWheelDown:
			s.viewport.ScrollDown(3)
		}
		return true
	case tea.KeyPressMsg:
		switch msg.String() {
		case "up":
			s.viewport.ScrollUp(1)
		case "down":
			s.viewport.ScrollDown(1)
		case "home":
			s.viewport.GotoTop()
		case "end":
			s.viewport.GotoBottom()
		case "pgup":
			s.viewport.ScrollUp(page)
		case "pgdown":
			s.viewport.ScrollDown(page)
		default:
			return false
		}
		return true
	}
	return false
}

// render lays out the screen: the title and selection lines, the card, the
// notice or a hint that the card goes on, and the two help rows.
func (s *cardScreen) render(title, selection, help, moreHelp string) tea.View {
	width, height := s.width, s.height
	status := s.notice
	if status == "" && !s.viewport.AtBottom() {
		status = "More below ↓"
	}

	rows := []string{violetStyle.Render(ansi.Truncate(sanitize(title), width, ""))}
	if height >= 4 {
		rows = append(rows, mutedStyle.Render(ansi.Truncate(sanitize(selection), width, "")))
	}
	body := s.viewport.View()
	if s.framed() {
		body = lipgloss.NewStyle().Background(panel).Border(cardBorder).BorderForeground(border).
			BorderTopBackground(background).BorderBottomBackground(background).
			BorderLeftBackground(panel).BorderRightBackground(panel).Render(body)
	}
	rows = append(rows, body)
	if height >= 5 {
		rows = append(rows, coralStyle.Render(ansi.Truncate(oneLine(status), width, "")))
	}
	if height >= 3 {
		rows = append(rows, mutedStyle.Render(ansi.Truncate(help, width, "")))
	}
	if height >= 8 {
		rows = append(rows, mutedStyle.Render(ansi.Truncate(moreHelp, width, "")))
	}

	// Extremely short terminals retain the header and as much body as fits.
	lines := strings.Split(strings.Join(rows, "\n"), "\n")
	if len(lines) > height {
		lines = lines[:height]
	}
	view := tea.NewView(strings.Join(lines, "\n"))
	view.AltScreen = true
	view.MouseMode = tea.MouseModeCellMotion
	return view
}

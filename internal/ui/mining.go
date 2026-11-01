package ui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"julia/internal/miner"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// MiningReviewConfig supplies a report working copy and its persistence operations.
// Save is called after every decision; Export is called only after confirmation.
type MiningReviewConfig struct {
	Report *miner.Report
	Save   func() error
	Export func() (string, error)
	Input  io.Reader
	Output io.Writer
}

// RunMiningReview runs the standalone review interface without a chat session.
func RunMiningReview(ctx context.Context, cfg MiningReviewConfig) error {
	if cfg.Report == nil {
		return errors.New("mining review requires a report")
	}
	options := []tea.ProgramOption{}
	if ctx != nil {
		options = append(options, tea.WithContext(ctx))
	}
	if cfg.Input != nil {
		options = append(options, tea.WithInput(cfg.Input))
	}
	if cfg.Output != nil {
		options = append(options, tea.WithOutput(cfg.Output))
	}
	_, err := tea.NewProgram(newMiningReview(cfg), options...).Run()
	if err != nil {
		return fmt.Errorf("run mining review: %w", err)
	}
	return nil
}

type miningReview struct {
	cfg        MiningReviewConfig
	viewport   viewport.Model
	width      int
	height     int
	selected   int
	confirming bool
	notice     string
}

func newMiningReview(cfg MiningReviewConfig) *miningReview {
	m := &miningReview{cfg: cfg, viewport: viewport.New(), width: 80, height: 24}
	for i, candidate := range m.candidates() {
		if candidate.Review.Decision == "" {
			m.selected = i
			break
		}
	}
	m.refresh()
	return m
}

func (m *miningReview) Init() tea.Cmd { return nil }

func (m *miningReview) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = max(1, msg.Width), max(1, msg.Height)
		m.refresh()
	case tea.MouseWheelMsg:
		switch msg.Button {
		case tea.MouseWheelUp:
			m.viewport.ScrollUp(3)
		case tea.MouseWheelDown:
			m.viewport.ScrollDown(3)
		}
	case tea.KeyPressMsg:
		key := msg.String()
		if key == "q" || key == "esc" || key == "ctrl+c" {
			return m, tea.Quit
		}
		if m.confirming {
			switch key {
			case "y":
				m.confirming = false
				if m.cfg.Export == nil {
					m.notice = "Export unavailable."
				} else if path, err := m.cfg.Export(); err != nil {
					m.notice = "Export failed: " + err.Error()
				} else {
					m.notice = "Exported: " + path
				}
				m.refresh()
			case "n":
				m.confirming = false
				m.notice = "Export cancelled."
				m.refresh()
			case "up":
				m.viewport.ScrollUp(1)
			case "down":
				m.viewport.ScrollDown(1)
			case "pgup":
				m.viewport.ScrollUp(max(1, m.viewport.Height()-1))
			case "pgdown":
				m.viewport.ScrollDown(max(1, m.viewport.Height()-1))
			}
			return m, nil
		}
		switch key {
		case "e":
			m.confirming = true
			m.notice = ""
			m.refresh()
			m.viewport.GotoTop()
		case "left", "p":
			m.selectRelative(-1)
		case "right", "n":
			m.selectRelative(1)
		case "up":
			m.viewport.ScrollUp(1)
		case "down":
			m.viewport.ScrollDown(1)
		case "home":
			m.viewport.GotoTop()
		case "end":
			m.viewport.GotoBottom()
		case "pgup":
			m.viewport.ScrollUp(max(1, m.viewport.Height()-1))
		case "pgdown":
			m.viewport.ScrollDown(max(1, m.viewport.Height()-1))
		case "1", "2", "3", "4", "5":
			m.decide(miner.Review{Decision: "keep", Rank: int(key[0] - '0')})
		case "r":
			m.decide(miner.Review{Decision: "reject"})
		case "d":
			m.decide(miner.Review{Decision: "defer"})
		case "u":
			m.decide(miner.Review{})
		}
	}
	return m, nil
}

func (m *miningReview) candidates() []miner.Candidate {
	if m.cfg.Report == nil {
		return nil
	}
	return m.cfg.Report.Candidates
}

func (m *miningReview) selectRelative(delta int) {
	n := len(m.candidates())
	if n == 0 {
		return
	}
	m.selected = (m.selected + delta + n) % n
	m.notice = ""
	m.refresh()
	m.viewport.GotoTop()
}

func (m *miningReview) decide(review miner.Review) {
	candidates := m.candidates()
	if len(candidates) == 0 {
		return
	}
	candidate := &candidates[m.selected]
	if review.Decision == "keep" {
		if reason := miner.ExportBlock(*candidate); reason != "" {
			m.notice = "Cannot accept: " + reason
			return
		}
	}
	if m.cfg.Save == nil {
		m.notice = "Save unavailable; decision not changed."
		return
	}
	previous := candidate.Review
	candidate.Review = review
	if err := m.cfg.Save(); err != nil {
		var durability *miner.DurabilityError
		if errors.As(err, &durability) {
			m.notice = "Decision replaced on disk; durability check failed."
			m.refresh()
		} else {
			candidate.Review = previous
			m.notice = "Save failed: " + err.Error()
		}
		return
	}
	m.notice = "Decision saved."
	// Clearing a decision leaves this card open. Other decisions move to the
	// next undecided card, wrapping only when one actually exists.
	if review.Decision != "" {
		for step := 1; step < len(candidates); step++ {
			i := (m.selected + step) % len(candidates)
			if candidates[i].Review.Decision == "" {
				m.selected = i
				break
			}
		}
	}
	m.refresh()
	m.viewport.GotoTop()
}

type miningCounts struct{ keep, reject, deferCount, undecided int }

func (m *miningReview) counts() miningCounts {
	var counts miningCounts
	for _, candidate := range m.candidates() {
		switch candidate.Review.Decision {
		case "keep":
			counts.keep++
		case "reject":
			counts.reject++
		case "defer":
			counts.deferCount++
		default:
			counts.undecided++
		}
	}
	return counts
}

func (m *miningReview) refresh() {
	framed := m.width >= 4 && m.height >= 8
	bodyWidth := m.width
	if framed {
		bodyWidth -= 2
	}
	reserved := 1 // title
	if m.height >= 4 {
		reserved++ // selection
	}
	if m.height >= 5 {
		reserved++ // status
	}
	if m.height >= 3 {
		reserved++ // first help row
	}
	if m.height >= 8 {
		reserved++ // second help row
	}
	if framed {
		reserved += 2
	}
	m.viewport.SetWidth(max(1, bodyWidth))
	m.viewport.SetHeight(max(1, m.height-reserved))

	var content string
	if m.confirming {
		counts := m.counts()
		content = fmt.Sprintf("CONFIRM EXPORT\n\nAccepted: %d  Rejected: %d  Deferred: %d  Undecided: %d\n\nOnly explicitly accepted examples are eligible for export. Historical system instructions and tool definitions are missing; the report uses current tool definitions. Following user feedback is NOT EXPORTED.\n\nPress y to export, n to cancel, or q to quit.", counts.keep, counts.reject, counts.deferCount, counts.undecided)
	} else {
		content = m.cardContent()
	}
	// Hard wrapping preserves even long tool arguments and results, including
	// spaces, instead of truncating them at the viewport edge.
	content = strings.ReplaceAll(sanitize(content), "\t", "    ")
	m.viewport.SetContent(ansi.Hardwrap(content, max(1, bodyWidth), true))
}

func (m *miningReview) cardContent() string {
	candidates := m.candidates()
	if len(candidates) == 0 {
		return "No candidates to review."
	}
	c := candidates[m.selected]
	var b strings.Builder
	fmt.Fprintf(&b, "%s · %s/%s\n", c.Source.Title, c.Source.Provider, c.Source.Model)
	if len(c.Messages) > 0 && c.Messages[0].Role == "user" {
		fmt.Fprintf(&b, "\nREQUEST\n%s\n", c.Messages[0].Content)
	}
	b.WriteString("\nLLM ASSESSMENT\n")
	if a := c.Assessment; a != nil {
		fmt.Fprintf(&b, "Summary: %s\nCategory: %s\nRecommendation: %s\nReason: %s\nInitiative: %d/5  Completion: %d/5  Restraint: %d/5\nMissing context: %t  Sensitive: %t\nEvidence:\n", a.Summary, a.Category, a.Recommendation, a.Reason, a.Initiative, a.Completion, a.Restraint, a.MissingContext, a.Sensitive)
		if len(a.Evidence) == 0 {
			b.WriteString("(none)\n")
		}
		for _, evidence := range a.Evidence {
			fmt.Fprintf(&b, "• %s\n", evidence)
		}
	} else {
		b.WriteString("Not assessed.\n")
	}
	if c.JudgeError != "" {
		fmt.Fprintf(&b, "Judge error: %s\n", c.JudgeError)
	}
	b.WriteString("\nHEURISTIC SIGNALS\n")
	if len(c.Signals) == 0 {
		b.WriteString("(none)\n")
	}
	for _, signal := range c.Signals {
		fmt.Fprintf(&b, "• %s\n", signal)
	}
	b.WriteString("\nWARNING: Historical system instructions and tool definitions are missing. Current tool definitions may differ.\n")
	b.WriteString("\nTRANSCRIPT (scroll for all content)\n")
	if len(c.Messages) == 0 {
		b.WriteString("(empty)\n")
	}
	for i, message := range c.Messages {
		fmt.Fprintf(&b, "\n[%d] %s", i, message.Role)
		if message.ToolCallID != "" {
			fmt.Fprintf(&b, " · tool call ID: %s", message.ToolCallID)
		}
		fmt.Fprintf(&b, "\n%s\n", message.Content)
		for _, call := range message.ToolCalls {
			fmt.Fprintf(&b, "\nTool call: %s\nID: %s\nArguments:\n%s\n", call.Name, call.ID, string(call.Arguments))
		}
		for _, image := range message.Images {
			fmt.Fprintf(&b, "[Image: %s, %d×%d; image data not displayed]\n", image.MediaType, image.Width, image.Height)
		}
	}
	b.WriteString("\nFOLLOWING USER FEEDBACK — NOT EXPORTED\n")
	if c.Feedback == "" {
		b.WriteString("(none)\n")
	} else {
		fmt.Fprintf(&b, "%s\n", c.Feedback)
	}
	fmt.Fprintf(&b, "\nSOURCE\nID: %s\nSession: %s\nGroup: %s\nDirectory: %s\nMessages: %d–%d (end exclusive)\nUpdated: %s\nPriority: %d\n",
		c.ID, c.Source.SessionID, c.Source.GroupID, c.Source.Directory, c.Source.Start, c.Source.End, c.Source.UpdatedAt.Format("2006-01-02 15:04 MST"), c.Priority)
	return b.String()
}

func (m *miningReview) View() tea.View {
	width, height := m.width, m.height
	counts := m.counts()
	title := fmt.Sprintf("MINING REVIEW · %d cards · %d kept · %d rejected · %d deferred · %d open", len(m.candidates()), counts.keep, counts.reject, counts.deferCount, counts.undecided)
	selection := "No candidate selected"
	if len(m.candidates()) > 0 {
		r := m.candidates()[m.selected].Review
		decision := r.Decision
		if decision == "" {
			decision = "undecided"
		}
		selection = fmt.Sprintf("Card %d/%d · %s", m.selected+1, len(m.candidates()), decision)
		if r.Decision == "keep" {
			selection += fmt.Sprintf(" · rank %d/5", r.Rank)
		}
	}
	footer := "←/→ p/n card · ↑/↓ pgup/pgdown scroll · 1–5 keep (1 low, 5 best)"
	footer2 := "r reject · d defer · u clear · e export · q/esc quit"
	if m.confirming {
		footer = "y confirm export · n cancel · q/esc quit"
		footer2 = "↑/↓ pgup/pgdown scroll"
	}
	status := m.notice
	if status == "" && !m.viewport.AtBottom() {
		status = "More below ↓"
	}
	rows := []string{violetStyle.Render(ansi.Truncate(sanitize(title), width, ""))}
	if height >= 4 {
		rows = append(rows, mutedStyle.Render(ansi.Truncate(sanitize(selection), width, "")))
	}
	framed := width >= 4 && height >= 8
	body := m.viewport.View()
	if framed {
		body = lipgloss.NewStyle().Background(panel).Border(cardBorder).BorderForeground(border).
			BorderTopBackground(background).BorderBottomBackground(background).
			BorderLeftBackground(panel).BorderRightBackground(panel).Render(body)
	}
	rows = append(rows, body)
	if height >= 5 {
		rows = append(rows, coralStyle.Render(ansi.Truncate(oneLine(status), width, "")))
	}
	if height >= 3 {
		rows = append(rows, mutedStyle.Render(ansi.Truncate(footer, width, "")))
	}
	if height >= 8 {
		rows = append(rows, mutedStyle.Render(ansi.Truncate(footer2, width, "")))
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

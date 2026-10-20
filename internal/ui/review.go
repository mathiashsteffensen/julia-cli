package ui

import (
	"encoding/json"
	"fmt"
	"image/color"
	"strings"
	"unicode"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"julia/internal/core"
)

var (
	// Severity pills: dark ink on a solid swatch, so the level reads before the words.
	pillInk   = background
	pillP0    = coral
	pillP1    = codeGold
	pillP2    = lipgloss.Color("#4A4458")
	pillP2Ink = lipgloss.Color("#D9D1E6")
	pillQuiet = lipgloss.Color("#34313C")
	// chipBG is an unlit choice: a step above the open finding's own background.
	chipBG = lipgloss.Color("#3E3A48")
)

// reviewChoice is one of the four things the user can say about a finding.
type reviewChoice struct {
	decision string
	key      string
	label    string
	short    string
	tag      string // the decided row's marker
	color    color.Color
}

var reviewChoices = []reviewChoice{
	{core.TriageFix, "f", "Fix", "Fix", "● Fix", coral},
	{core.TriageIgnore, "i", "Ignore", "Ignore", "○ Ignore", muted},
	{core.TriageFixed, "d", "I fixed it", "Fixed", "✓ Fixed", green},
	{core.TriageReply, "r", "Reply…", "Reply", "✎ Reply", violet},
}

func choiceIndex(decision string) int {
	for i, choice := range reviewChoices {
		if choice.decision == decision {
			return i
		}
	}
	return -1
}

// reviewState is a review being triaged. Rows 0..len(findings)-1 are the
// findings and row len(findings) is "send"; the selected finding is the open
// one, and choice is the chip lit inside it.
type reviewState struct {
	review   core.Review
	triage   []core.Triage
	selected int
	choice   int
	// discarding is set by a first Esc once decisions exist: a second Esc
	// throws them away, anything else keeps working on them.
	discarding bool
	hits       []reviewHit
}

// reviewHit maps card lines (inclusive) to a row, and optionally a chip on one
// line of it between columns x0 and x1 (exclusive) of the card.
type reviewHit struct {
	top, bottom, row int
	chip, x0, x1     int
}

func newReviewState(review core.Review) *reviewState {
	return &reviewState{review: review, triage: make([]core.Triage, len(review.Findings))}
}

func (r *reviewState) findings() []core.Finding { return r.review.Findings }
func (r *reviewState) sendRow() int             { return len(r.review.Findings) }
func (r *reviewState) rows() int                { return len(r.review.Findings) + 1 }

func decided(t core.Triage) bool { return t.Decision != "" && t.Decision != core.TriageOpen }

func (r *reviewState) decidedCount() int {
	n := 0
	for _, t := range r.triage {
		if decided(t) {
			n++
		}
	}
	return n
}

// focus moves to row and lights the chip of its current decision, or Fix.
func (r *reviewState) focus(row int) {
	r.selected = (row + r.rows()) % r.rows()
	r.choice = 0
	if r.selected < r.sendRow() {
		if i := choiceIndex(r.triage[r.selected].Decision); i >= 0 {
			r.choice = i
		}
	}
}

// advance moves to the next finding without a decision, or to send when every
// finding has one, so a run of Enters walks the whole review.
func (r *reviewState) advance() {
	n := len(r.triage)
	for step := 1; step <= n; step++ {
		if i := (r.selected + step) % n; !decided(r.triage[i]) {
			r.focus(i)
			return
		}
	}
	r.focus(r.sendRow())
}

func (m *model) sendReview(q *questionState) tea.Cmd {
	return m.answerQuestion(core.Answer{Triage: append([]core.Triage(nil), q.review.triage...)})
}

func (m *model) reviewKey(q *questionState, msg tea.KeyPressMsg) tea.Cmd {
	r := q.review
	q.notice = ""
	name := msg.String()
	if q.writing {
		switch name {
		case "esc":
			q.writing = false
			q.input.Blur()
			q.input.Reset()
			return nil
		case "enter":
			text := strings.TrimSpace(q.input.Value())
			if text == "" {
				q.notice = "Write a reply first · esc to go back"
				return nil
			}
			r.triage[r.selected] = core.Triage{Decision: core.TriageReply, Reply: text}
			q.writing = false
			q.input.Blur()
			q.input.Reset()
			r.advance()
			return nil
		}
		var cmd tea.Cmd
		q.input, cmd = q.input.Update(msg)
		return cmd
	}
	if name != "esc" {
		r.discarding = false
	}
	onFinding := r.selected < r.sendRow()
	switch name {
	case "esc":
		if r.decidedCount() > 0 && !r.discarding {
			r.discarding = true
			q.notice = "Esc again discards your decisions · ctrl+s sends them"
			return nil
		}
		return m.answerQuestion(core.Answer{Skipped: true})
	case "up", "ctrl+p", "shift+tab", "k":
		r.focus(r.selected - 1)
	case "down", "ctrl+n", "tab", "j":
		r.focus(r.selected + 1)
	case "left", "h":
		if onFinding {
			r.choice = (r.choice - 1 + len(reviewChoices)) % len(reviewChoices)
		}
	case "right", "l":
		if onFinding {
			r.choice = (r.choice + 1) % len(reviewChoices)
		}
	case "home":
		r.focus(0)
	case "end":
		r.focus(r.sendRow())
	case "pgup":
		m.viewport.ScrollUp(max(1, m.viewport.Height()-1))
	case "pgdown":
		m.viewport.ScrollDown(max(1, m.viewport.Height()-1))
	case "ctrl+s":
		return m.sendReview(q)
	case "enter":
		if !onFinding {
			return m.sendReview(q)
		}
		return m.decideFinding(q, r.choice)
	case "backspace", "delete", "u":
		if onFinding && decided(r.triage[r.selected]) {
			r.triage[r.selected] = core.Triage{}
			r.choice = 0
			q.notice = "Back to undecided"
		}
	case "F", "shift+f":
		// Every finding still open gets Fix; the ones already decided keep theirs.
		count := 0
		for i := range r.triage {
			if !decided(r.triage[i]) {
				r.triage[i] = core.Triage{Decision: core.TriageFix}
				count++
			}
		}
		if count > 0 {
			r.focus(r.sendRow())
			q.notice = fmt.Sprintf("Marked %d more to fix", count)
		}
	default:
		if onFinding {
			for i, choice := range reviewChoices {
				if name == choice.key {
					r.choice = i
					return m.decideFinding(q, i)
				}
			}
		}
	}
	return nil
}

// decideFinding applies a chip to the open finding: Reply opens its field, the
// others settle it and move on to the next open finding.
func (m *model) decideFinding(q *questionState, index int) tea.Cmd {
	r := q.review
	choice := reviewChoices[index]
	if choice.decision == core.TriageReply {
		q.writing = true
		q.input.Reset()
		q.input.SetHeight(1)
		if previous := r.triage[r.selected]; previous.Decision == core.TriageReply {
			q.input.SetValue(previous.Reply)
		}
		return q.input.Focus()
	}
	r.triage[r.selected] = core.Triage{Decision: choice.decision}
	r.advance()
	return nil
}

func (m *model) reviewPaste(q *questionState, msg tea.PasteMsg) tea.Cmd {
	r := q.review
	var cmd tea.Cmd
	if !q.writing {
		if r.selected >= r.sendRow() {
			return nil
		}
		r.choice = choiceIndex(core.TriageReply)
		cmd = m.decideFinding(q, r.choice)
	}
	var update tea.Cmd
	q.input, update = q.input.Update(msg)
	return tea.Batch(cmd, update)
}

// clickReview selects the row under a click, or applies the chip under it.
func (m *model) clickReview(q *questionState, line, x int) tea.Cmd {
	r := q.review
	for _, hit := range r.hits {
		if line < hit.top || line > hit.bottom {
			continue
		}
		if hit.chip >= 0 && (x < hit.x0 || x >= hit.x1) {
			continue
		}
		switch {
		case hit.row == r.sendRow() && r.selected == hit.row:
			return m.sendReview(q)
		case hit.chip >= 0:
			if q.writing {
				q.writing = false
				q.input.Blur()
			}
			r.choice = hit.chip
			return m.decideFinding(q, hit.chip)
		case hit.row != r.selected:
			if q.writing {
				q.writing = false
				q.input.Blur()
				q.input.Reset()
			}
			r.focus(hit.row)
		}
		return nil
	}
	return nil
}

// reviewRequest expands /review into the request a user would type: alone it
// reviews the uncommitted changes, with words after it whatever they name.
func reviewRequest(prompt string) (string, bool) {
	rest, ok := strings.CutPrefix(prompt, "/review")
	if !ok || rest != "" && !unicode.IsSpace(rune(rest[0])) {
		return "", false
	}
	target := strings.TrimSpace(rest)
	if target == "" {
		target = "the uncommitted changes in this workspace (staged, unstaged and untracked)"
	}
	return "Review " + target + " for correctness bugs, regressions, unhandled edge cases and missing tests. " +
		"Verify each finding against the code, then hand them to me with ReviewFindings and act on my decisions.", true
}

// --- the card ---------------------------------------------------------------

// reviewPlan trades detail for height until the card fits.
type reviewPlan struct {
	airy    bool // blank rows inside the frame, above and below
	summary int  // lines of the review's summary
	title   int  // lines of the open finding's title
	detail  int  // lines of the open finding's evidence
	quotes  bool // decided rows show the reply they carry
	spaced  bool // a blank row around the open finding
	field   int
}

// The summary goes before the open finding's evidence does: the finding and
// its choices are what the card is for.
var reviewPlans = []reviewPlan{
	{true, 3, 3, 8, true, true, 4},
	{true, 2, 2, 6, true, true, 3},
	{false, 2, 2, 4, true, true, 3},
	{false, 1, 2, 3, false, true, 2},
	{false, 0, 2, 3, false, true, 2},
	{false, 0, 2, 2, false, false, 2},
	{false, 0, 1, 1, false, false, 1},
	{false, 0, 1, 0, false, false, 1},
}

func (m *model) reviewCard() ([]string, int) {
	q := m.question
	width := m.mainWidth - 4
	if width < 30 {
		width = m.mainWidth
	}
	width = max(8, width)
	budget := m.bodyHeight
	if m.bodyHeight >= 14 {
		budget -= 3
	}
	if budget < 9 || width < 30 {
		q.review.hits = nil
		return m.compactReview(q, width, max(1, budget)), width
	}
	// Roomy plans first with every row shown; past those, keep the open finding
	// readable and scroll the list rather than squeezing the evidence away.
	for _, plan := range reviewPlans[:3] {
		if card, hits, whole := m.assembleReview(q, width, plan, budget, false); whole && len(card) <= budget {
			q.review.hits = hits
			return card, width
		}
	}
	for _, plan := range reviewPlans {
		if card, hits, whole := m.assembleReview(q, width, plan, budget, true); whole && len(card) <= budget {
			q.review.hits = hits
			return card, width
		}
	}
	// Even alone the open finding does not fit: show what does of it.
	card, _, _ := m.assembleReview(q, width, reviewPlans[len(reviewPlans)-1], budget, true)
	q.review.hits = nil
	return card[:min(len(card), budget)], width
}

// reviewIndent is where a finding's text starts: bar, number, pill, gaps.
const reviewIndent = 14

// assembleReview builds the card for one plan. whole reports that the open
// finding, choices included, made it onto the card uncut.
func (m *model) assembleReview(q *questionState, width int, plan reviewPlan, budget int, window bool) (card []string, hits []reviewHit, whole bool) {
	r := q.review
	iw := width - 2
	blank := paint(cardBG, iw, "")
	edge := func(s string) string { return lipgloss.NewStyle().Foreground(cardEdge).Render(s) }
	frame := func(inner string) string { return edge("│") + inner + edge("│") }
	pad := ink(cardBG, cream).Render("  ")

	var head []string
	if plan.airy {
		head = append(head, blank)
	}
	if summary := r.review.Summary; summary != "" && plan.summary > 0 {
		for _, line := range clampLines(wrapPlain(summary, max(4, iw-4)), plan.summary, max(4, iw-4)) {
			head = append(head, paint(cardBG, iw, pad+ink(cardBG, cream).Render(line)))
		}
		head = append(head, blank)
	}
	var foot []string
	if plan.airy {
		foot = append(foot, blank)
	}
	foot = append(foot, paint(cardBG, iw, pad+m.reviewHint(q, iw-4)))
	if plan.airy {
		foot = append(foot, blank)
	}

	blocks := make([][]string, 0, r.rows())
	chips := make([][]reviewHit, r.rows())
	for i := range r.findings() {
		var block []string
		if i == r.selected {
			block, chips[i] = m.openFinding(q, i, iw, plan)
		} else {
			block = m.findingRow(q, i, iw, plan)
		}
		blocks = append(blocks, block)
	}
	blocks = append(blocks, m.sendBlock(q, iw, plan))

	avail := budget - 2 - len(head) - len(foot)
	visible, first, above, below := blocks, 0, 0, 0
	if window {
		visible, first, above, below = windowBlocks(blocks, r.selected, avail)
	}
	whole = true
	if at := r.selected - first; at >= 0 && at < len(visible) {
		whole = len(visible[at]) == len(blocks[r.selected])
	}
	var body []string
	if above > 0 {
		body = append(body, paint(cardBG, iw, pad+ink(cardBG, faint).Render(fmt.Sprintf("    ↑ %d more", above))))
	}
	for i, block := range visible {
		row := first + i
		// Card line 0 is the top rule, then the head rows, then the body.
		top := 1 + len(head) + len(body)
		hits = append(hits, reviewHit{top: top, bottom: top + len(block) - 1, row: row, chip: -1})
		for _, chip := range chips[row] {
			// Chips are recorded relative to their block; the frame adds a column.
			chip.top += top
			chip.bottom += top
			chip.x0++
			chip.x1++
			// Chip hits go first so they win over their block's row hit.
			hits = append([]reviewHit{chip}, hits...)
		}
		body = append(body, block...)
	}
	if below > 0 {
		body = append(body, paint(cardBG, iw, pad+ink(cardBG, faint).Render(fmt.Sprintf("    ↓ %d more", below))))
	}

	lines := []string{m.reviewTop(q, width)}
	for _, part := range [][]string{head, body, foot} {
		for _, line := range part {
			lines = append(lines, frame(line))
		}
	}
	return append(lines, edge("╰"+strings.Repeat("─", width-2)+"╯")), hits, whole
}

// severityPill draws a finding's level as a solid four-cell badge. A quiet pill
// is for findings the user has set aside.
func severityPill(severity string, quiet bool) string {
	bg, fg := pillP2, pillP2Ink
	switch severity {
	case "P0":
		bg, fg = pillP0, pillInk
	case "P1":
		bg, fg = pillP1, pillInk
	}
	if quiet {
		bg, fg = pillQuiet, muted
	}
	label := ansi.Truncate(severity, 2, "")
	return ink(bg, fg).Bold(true).Render(" " + label + strings.Repeat(" ", 3-ansi.StringWidth(label)))
}

// rowLead is the start of a finding's first line: bar, number and pill.
func rowLead(bg color.Color, selected bool, index int, severity string, quiet bool) string {
	numberStyle := ink(bg, faint)
	if selected {
		numberStyle = ink(bg, coral).Bold(true)
	}
	return ink(bg, cream).Render("  ") + gutter(bg, selected) + ink(bg, cream).Render(" ") +
		numberStyle.Render(fmt.Sprintf("%2d", index+1)) + ink(bg, cream).Render("  ") +
		severityPill(severity, quiet) + ink(bg, cream).Render("  ")
}

// rowIndent continues a finding below its first line, keeping the bar.
func rowIndent(bg color.Color, selected bool) string {
	return ink(bg, cream).Render("  ") + gutter(bg, selected) + ink(bg, cream).Render(strings.Repeat(" ", reviewIndent-3))
}

func decisionTag(t core.Triage, bg color.Color) string {
	i := choiceIndex(t.Decision)
	if i < 0 {
		return ""
	}
	return ink(bg, reviewChoices[i].color).Bold(t.Decision == core.TriageFix).Render(reviewChoices[i].tag)
}

// findingRow is a finding that is not open: one line with its decision on the
// right, and the reply it carries when the plan has room.
func (m *model) findingRow(q *questionState, index, iw int, plan reviewPlan) []string {
	r := q.review
	finding := r.findings()[index]
	t := r.triage[index]
	quiet := t.Decision == core.TriageIgnore || t.Decision == core.TriageFixed
	titleStyle := ink(cardBG, cream)
	if quiet {
		titleStyle = ink(cardBG, faint)
	}
	tag := decisionTag(t, cardBG)
	room := max(4, iw-reviewIndent-2-ansi.StringWidth(tag)-2)
	title := ansi.Truncate(oneLine(finding.Title), room, "…")
	line := rowLead(cardBG, false, index, finding.Severity, quiet) + titleStyle.Render(title)
	if tag != "" {
		gap := iw - 2 - ansi.StringWidth(line) - ansi.StringWidth(tag)
		line += ink(cardBG, cream).Render(strings.Repeat(" ", max(2, gap))) + tag
	}
	lines := []string{paint(cardBG, iw, line)}
	if t.Decision == core.TriageReply && plan.quotes {
		quote := ansi.Truncate(oneLine(t.Reply), max(4, iw-reviewIndent-6), "…")
		lines = append(lines, paint(cardBG, iw, rowIndent(cardBG, false)+ink(cardBG, cardQuote).Italic(true).Render("“"+quote+"”")))
	}
	return lines
}

// openFinding is the selected finding in full: title, location, evidence, any
// reply, and the row of choices. It returns the chips' hits within the block.
func (m *model) openFinding(q *questionState, index, iw int, plan reviewPlan) ([]string, []reviewHit) {
	r := q.review
	finding := r.findings()[index]
	t := r.triage[index]
	bg := cardSelect
	textWidth := max(4, iw-reviewIndent-2)
	indent := rowIndent(bg, true)
	var lines []string
	if plan.spaced {
		lines = append(lines, paint(bg, iw, ink(bg, cream).Render("  ")+gutter(bg, true)))
	}
	for i, text := range clampLines(wrapPlain(oneLine(finding.Title), textWidth), plan.title, textWidth) {
		lead := indent
		if i == 0 {
			lead = rowLead(bg, true, index, finding.Severity, false)
		}
		lines = append(lines, paint(bg, iw, lead+ink(bg, cream).Bold(true).Render(text)))
	}
	if finding.Location != "" {
		lines = append(lines, paint(bg, iw, indent+ink(bg, cyan).Render(ansi.Truncate(finding.Location, textWidth, "…"))))
	}
	if finding.Detail != "" && plan.detail > 0 {
		for _, text := range clampLines(wrapPlain(finding.Detail, textWidth), plan.detail, textWidth) {
			lines = append(lines, paint(bg, iw, indent+ink(bg, muted).Render(text)))
		}
	}
	if t.Decision == core.TriageReply && !q.writing {
		quote := clampLines(wrapPlain(t.Reply, max(4, textWidth-2)), 3, max(4, textWidth-2))
		for i, text := range quote {
			mark := "  "
			if i == 0 {
				mark = "↳ "
			}
			lines = append(lines, paint(bg, iw, indent+ink(bg, violet).Render(mark)+ink(bg, cardQuote).Italic(true).Render(text)))
		}
	}
	if plan.spaced {
		lines = append(lines, paint(bg, iw, ink(bg, cream).Render("  ")+gutter(bg, true)))
	}
	chipLines, hits := m.choiceChips(q, iw, len(lines))
	lines = append(lines, chipLines...)
	if q.writing {
		if plan.spaced {
			lines = append(lines, paint(bg, iw, ink(bg, cream).Render("  ")+gutter(bg, true)))
		}
		lines = append(lines, m.fieldBlock(q, iw, reviewIndent, 2, plan.field, bg, gutter(bg, true))...)
	}
	if plan.spaced {
		lines = append(lines, paint(bg, iw, ink(bg, cream).Render("  ")+gutter(bg, true)))
	}
	return lines, hits
}

// choiceChips lays the four choices out from the finding's indent, wrapping
// when the card is narrow. Each is a pill holding its key and label: the lit
// one is solid in its own color, the finding's current decision is set in that
// color with a dot, and the rest are quiet.
func (m *model) choiceChips(q *questionState, iw, firstLine int) ([]string, []reviewHit) {
	r := q.review
	bg := cardSelect
	room := iw - reviewIndent - 2
	full := 0
	for _, choice := range reviewChoices {
		full += ansi.StringWidth(choice.label) + 7 + 2
	}
	short := full > room
	current := choiceIndex(r.triage[r.selected].Decision)
	var lines []string
	var hits []reviewHit
	line, used := rowIndent(bg, true), 0
	for i, choice := range reviewChoices {
		label := choice.label
		if short {
			label = choice.short
		}
		pill := chipBG
		keyStyle, labelStyle := ink(pill, faint), ink(pill, muted)
		var mark string
		lit := i == r.choice && !q.writing || q.writing && choice.decision == core.TriageReply
		switch {
		case lit:
			pill = choice.color
			keyStyle, labelStyle = ink(pill, pillInk), ink(pill, pillInk).Bold(true)
		case i == current:
			keyStyle, labelStyle = ink(pill, choice.color), ink(pill, choice.color).Bold(true)
		}
		// Every cell carries the pill's color, or the terminal shows through.
		space := ink(pill, cream).Render(" ")
		mark = space + space
		if i == current {
			mark = space + ink(pill, labelStyle.GetForeground()).Render("●")
		}
		// The key sits a shade back from the label, so the word leads.
		chip := space + keyStyle.Render(choice.key) + space + space + labelStyle.Render(label) + mark + space
		chipWidth := ansi.StringWidth(chip)
		gap := 0
		if used > 0 {
			gap = 2
		}
		if used > 0 && used+gap+chipWidth > room {
			lines = append(lines, paint(bg, iw, line))
			line, used, gap = rowIndent(bg, true), 0, 0
		}
		if gap > 0 {
			line += ink(bg, cream).Render(strings.Repeat(" ", gap))
		}
		x0 := reviewIndent + used + gap
		hits = append(hits, reviewHit{top: firstLine + len(lines), bottom: firstLine + len(lines), row: r.selected, chip: i, x0: x0, x1: x0 + chipWidth})
		line += chip
		used += gap + chipWidth
	}
	lines = append(lines, paint(bg, iw, line))
	return lines, hits
}

// sendBlock is the last row: send the decisions, with a tally of them.
func (m *model) sendBlock(q *questionState, iw int, plan reviewPlan) []string {
	r := q.review
	selected := r.selected == r.sendRow()
	bg := cardBG
	if selected {
		bg = cardSelect
	}
	iconStyle, labelStyle := ink(bg, faint), ink(bg, muted)
	if selected {
		iconStyle, labelStyle = ink(bg, coral).Bold(true), ink(bg, cream).Bold(true)
	}
	label := "Send decisions"
	if r.decidedCount() == len(r.triage) {
		label = "Send to JULIA"
	}
	line := ink(bg, cream).Render("  ") + gutter(bg, selected) + ink(bg, cream).Render(" ") + iconStyle.Render(" →") + ink(bg, cream).Render("  ") + labelStyle.Render(label)
	if tally := m.reviewTally(r, bg); tally != "" {
		if gap := iw - 2 - ansi.StringWidth(line) - ansi.StringWidth(tally); gap >= 2 {
			line += ink(bg, cream).Render(strings.Repeat(" ", gap)) + tally
		}
	}
	var lines []string
	if plan.airy || plan.spaced {
		lines = append(lines, paint(cardBG, iw, ""))
	}
	return append(lines, paint(bg, iw, line))
}

// reviewTally counts the decisions so far, each in its choice's color.
func (m *model) reviewTally(r *reviewState, bg color.Color) string {
	counts := map[string]int{}
	for _, t := range r.triage {
		decision := t.Decision
		if !decided(t) {
			decision = core.TriageOpen
		}
		counts[decision]++
	}
	var parts []string
	for _, choice := range reviewChoices {
		if n := counts[choice.decision]; n > 0 {
			word := strings.ToLower(choice.short)
			parts = append(parts, ink(bg, choice.color).Render(fmt.Sprintf("%d %s", n, word)))
		}
	}
	if n := counts[core.TriageOpen]; n > 0 {
		parts = append(parts, ink(bg, faint).Render(fmt.Sprintf("%d open", n)))
	}
	return strings.Join(parts, ink(bg, faint).Render(" · "))
}

// reviewTop is the card's top rule: the title, and one dot per finding colored
// by its decision so progress reads at a glance.
func (m *model) reviewTop(q *questionState, width int) string {
	r := q.review
	edge := lipgloss.NewStyle().Foreground(cardEdge)
	count := fmt.Sprintf("%d findings", len(r.triage))
	if len(r.triage) == 1 {
		count = "1 finding"
	}
	left := edge.Render("╭─ ") + violetStyle.Bold(true).Render("✦ JULIA REVIEWS") + faintStyle.Render(" · "+count) + " "
	// One dot per finding: filled in its decision's color once decided, the
	// open finding's ring lit in coral.
	var dots []string
	for i, t := range r.triage {
		style, dot := lipgloss.NewStyle().Foreground(border), "○"
		if c := choiceIndex(t.Decision); c >= 0 {
			style, dot = lipgloss.NewStyle().Foreground(reviewChoices[c].color), "●"
		}
		if i == r.selected && !decided(t) {
			style = coralStyle.Bold(true)
		}
		dots = append(dots, style.Render(dot))
	}
	progress := faintStyle.Render(fmt.Sprintf("  %d/%d", r.decidedCount(), len(r.triage)))
	right := " " + strings.Join(dots, " ") + progress + " "
	if ansi.StringWidth(right)*3 > width {
		right = " " + strings.Join(dots, "") + progress + " "
	}
	fill := width - ansi.StringWidth(left) - ansi.StringWidth(right) - 2
	if fill < 1 {
		right = " " + faintStyle.Render(fmt.Sprintf("%d/%d", r.decidedCount(), len(r.triage))) + " "
		fill = width - ansi.StringWidth(left) - ansi.StringWidth(right) - 2
	}
	if fill < 1 {
		left = edge.Render("╭─ ") + violetStyle.Bold(true).Render("✦ REVIEW") + " "
		fill = width - ansi.StringWidth(left) - ansi.StringWidth(right) - 2
	}
	if fill < 1 {
		right = ""
		fill = max(0, width-ansi.StringWidth(left)-2)
	}
	return left + edge.Render(strings.Repeat("─", fill)) + right + edge.Render("─╮")
}

func (m *model) reviewHint(q *questionState, width int) string {
	r := q.review
	if q.notice != "" {
		return ink(cardBG, coral).Render(q.notice)
	}
	switch {
	case q.writing:
		return renderHint(width,
			[]hintPart{{"enter", "save reply"}, {"alt+enter", "new line"}, {"esc", "back"}},
			[]hintPart{{"enter", "save"}, {"esc", "back"}})
	case r.selected == r.sendRow():
		return renderHint(width,
			[]hintPart{{"enter", "send"}, {"↑", "back to the findings"}, {"esc", "skip the review"}},
			[]hintPart{{"enter", "send"}, {"↑", "back"}, {"esc", "skip"}})
	}
	return renderHint(width,
		[]hintPart{{"↑↓", "finding"}, {"←→", "choose"}, {"enter", "set"}, {"f i d r", "shortcuts"}, {"F", "fix all open"}, {"ctrl+s", "send"}, {"esc", "skip"}},
		[]hintPart{{"↑↓", "finding"}, {"←→", "choose"}, {"enter", "set"}, {"F", "fix all"}, {"ctrl+s", "send"}, {"esc", "skip"}},
		[]hintPart{{"↑↓", "finding"}, {"←→", "choose"}, {"enter", "set"}, {"ctrl+s", "send"}},
		[]hintPart{{"↑↓", "move"}, {"enter", "set"}, {"ctrl+s", "send"}})
}

// compactReview is the few-line fallback for very small terminals.
func (m *model) compactReview(q *questionState, width, budget int) []string {
	r := q.review
	var head, choices string
	if r.selected == r.sendRow() {
		head = violetStyle.Bold(true).Render("✦ ") + creamStyle.Bold(true).Render("Send decisions") + faintStyle.Render(fmt.Sprintf("  %d/%d", r.decidedCount(), len(r.triage)))
		choices = m.reviewTally(r, background)
	} else {
		finding := r.findings()[r.selected]
		head = violetStyle.Bold(true).Render(fmt.Sprintf("✦ %d/%d ", r.selected+1, len(r.triage))) + severityPill(finding.Severity, false) + " " + creamStyle.Bold(true).Render(oneLine(finding.Title))
		var parts []string
		for i, choice := range reviewChoices {
			style := mutedStyle
			if i == r.choice {
				style = ink(choice.color, pillInk).Bold(true)
			} else if choice.decision == r.triage[r.selected].Decision {
				style = lipgloss.NewStyle().Foreground(choice.color).Bold(true)
			}
			parts = append(parts, style.Render(" "+choice.short+" "))
		}
		choices = strings.Join(parts, " ")
		if q.writing {
			q.input.SetWidth(max(4, width-2))
			q.input.MaxHeight = 1
			q.input.SetHeight(1)
			view := strings.Split(strings.TrimRight(q.input.View(), "\n"), "\n")
			choices = paint(cardInset, width, view[0])
		}
	}
	hint := mutedStyle.Render("↑↓ ←→ enter · ctrl+s send")
	if q.writing {
		hint = mutedStyle.Render("enter save · esc back")
	}
	if q.notice != "" {
		hint = coralStyle.Render(q.notice)
	}
	lines := []string{head, choices, hint}
	if budget < 3 {
		lines = lines[:2]
	}
	if budget < 2 {
		lines = lines[1:2]
	}
	for i := range lines {
		lines[i] = ansi.Truncate(lines[i], width, "…")
	}
	return lines
}

// --- transcript -------------------------------------------------------------

// renderReviewEntry shows a review call in the conversation: each finding with
// the user's decision beside it. It reports false when the call cannot be decoded.
func (m *model) renderReviewEntry(item entry, w int) (string, bool) {
	var review core.Review
	if item.call == nil || json.Unmarshal(item.call.Arguments, &review) != nil || len(review.Findings) == 0 {
		return "", false
	}
	box := lipgloss.NewStyle().BorderLeft(true).BorderStyle(lipgloss.NormalBorder()).BorderForeground(cardEdge).PaddingLeft(1)
	count := fmt.Sprintf("%d findings", len(review.Findings))
	if len(review.Findings) == 1 {
		count = "1 finding"
	}
	if !item.done && item.result == nil {
		return box.Render(violetStyle.Render("◌") + " " + mutedStyle.Render("waiting for your decisions on "+count+"…")), true
	}
	icon := violetStyle.Render("✦")
	var result core.ReviewResult
	resultOK := item.result != nil && !item.result.IsError && json.Unmarshal([]byte(item.result.Content), &result) == nil
	if item.result != nil && item.result.IsError {
		icon = coralStyle.Render("!")
	}
	out := []string{icon + " " + creamStyle.Bold(true).Render("Review") + faintStyle.Render(" · "+count)}
	decisions := make(map[int]core.FindingDecision)
	for _, decision := range result.Decisions {
		decisions[decision.Finding] = decision
	}
	if resultOK && !result.Skipped {
		r := &reviewState{triage: make([]core.Triage, len(review.Findings))}
		for i := range r.triage {
			r.triage[i] = core.Triage{Decision: decisions[i+1].Decision}
		}
		if tally := m.reviewTally(r, background); tally != "" {
			out[0] += faintStyle.Render("  ") + tally
		}
	}
	inner := max(12, w-2)
	for i, finding := range review.Findings {
		decision, has := decisions[i+1]
		t := core.Triage{Decision: decision.Decision, Reply: decision.Reply}
		quiet := t.Decision == core.TriageIgnore || t.Decision == core.TriageFixed
		tag := ""
		if has {
			tag = decisionTag(t, background)
		}
		titleStyle := creamStyle
		if quiet {
			titleStyle = faintStyle
		}
		room := max(4, inner-2-4-2-ansi.StringWidth(tag)-2)
		line := "  " + severityPill(finding.Severity, quiet) + "  " + titleStyle.Render(ansi.Truncate(oneLine(finding.Title), room, "…"))
		if tag != "" {
			line += strings.Repeat(" ", max(2, inner-ansi.StringWidth(line)-ansi.StringWidth(tag))) + tag
		}
		out = append(out, line)
		if t.Decision == core.TriageReply && t.Reply != "" {
			for j, text := range clampLines(wrapPlain(t.Reply, max(8, inner-10)), 3, max(8, inner-10)) {
				mark := "  "
				if j == 0 {
					mark = violetStyle.Render("↳ ")
				}
				out = append(out, strings.Repeat(" ", 8)+mark+lipgloss.NewStyle().Foreground(cardQuote).Italic(true).Render(text))
			}
		}
	}
	switch {
	case item.result == nil:
		out = append(out, faintStyle.Render("  no decisions recorded"))
	case item.result.IsError:
		first, _, _ := strings.Cut(strings.TrimSpace(sanitize(item.result.Content)), "\n")
		out = append(out, coralStyle.Render("  "+ansi.Truncate(first, inner, "…")))
	case !resultOK:
		out = append(out, faintStyle.Render("  "+ansi.Truncate(oneLine(item.result.Content), inner, "…")))
	case result.Skipped:
		out = append(out, mutedStyle.Italic(true).Render("  ↳ skipped · no decisions sent"))
	}
	return box.Render(strings.Join(out, "\n")), true
}

// --- demo -------------------------------------------------------------------

func (m *model) recordDemoReview(q *questionState, answer core.Answer) {
	arguments, _ := json.Marshal(q.review.review)
	result := core.ReviewResult{Skipped: answer.Skipped}
	for i, t := range answer.Triage {
		decision := core.FindingDecision{Finding: i + 1, Title: q.review.findings()[i].Title, Decision: t.Decision, Reply: t.Reply}
		if !decided(t) {
			decision.Decision = core.TriageOpen
		}
		result.Decisions = append(result.Decisions, decision)
	}
	content, _ := json.Marshal(result)
	call := &core.ToolCall{ID: "demo-review", Name: core.ReviewTool, Arguments: arguments}
	m.entries = append(m.entries, entry{role: "tool", call: call, done: true, result: &core.ToolResult{Content: string(content)}})
	m.toolCount++
	m.notice = "Preview only · nothing was sent"
}

func demoReview() core.Review {
	return core.Review{
		Summary: "Reviewed the uncommitted cancellation change across internal/ui and internal/tools. Two problems are worth fixing before it lands; the rest are notes.",
		Findings: []core.Finding{
			{Severity: "P0", Title: "Cancelling during an approval leaves the reviewer call running", Location: "internal/tools/sandbox.go:212",
				Detail: "cancelTask replies to the pending approval, but review() was started with the turn's parent context, so a slow reviewer keeps the tool slot until the 4h deadline. Pass the call's ctx into review() and return on ctx.Done()."},
			{Severity: "P1", Title: "Retry backoff ignores the server's Retry-After", Location: "internal/provider/upstream.go:148",
				Detail: "The loop doubles from 500 ms regardless of a 429's Retry-After header, so a rate-limited turn burns all six retries inside the window and fails. Read the header and wait at least that long."},
			{Severity: "P1", Title: "--timeout reaches the parent runner but not subagents", Location: "cmd/julia/main.go:289",
				Detail: "Children built by the factory get the config default instead of r.timeout. Thread the parsed duration into makeBackend's config."},
			{Severity: "P2", Title: "Comment still describes the old process-group signal", Location: "internal/tools/terminal.go:77",
				Detail: "The comment says SIGTERM goes to the ordinary process group; the code now signals the session leader first. Update the comment."},
			{Severity: "P2", Title: "No test covers a reply that races cancellation", Location: "internal/ui/question_bridge.go:31",
				Detail: "The select prefers ctx.Err() once a reply arrives, but nothing exercises it. A test with a context cancelled before the reply would pin it."},
		},
	}
}

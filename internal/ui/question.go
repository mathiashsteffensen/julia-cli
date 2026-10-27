package ui

import (
	"encoding/json"
	"fmt"
	"image/color"
	"strconv"
	"strings"
	"time"
	"unicode"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"julia/internal/core"
)

// questionGuard ignores keys right after a question appears, so a message the
// user was typing ahead (letters, Enter) is never taken as an answer.
const questionGuard = 350 * time.Millisecond

var (
	cardBG = panel
	// Near-grays with a blue tint can snap to navy in 256-color terminals; this
	// one maps to a true gray there (checked against colorprofile.ANSI256).
	cardSelect = lipgloss.Color("#2C2738")
	cardInset  = lipgloss.Color("#100E14")
	cardEdge   = lipgloss.Color("#8E81C2")
	cardQuote  = lipgloss.Color("#B9AEE3")
	cardDim    = lipgloss.Color("#7A7586")
)

type questionMsg struct{ request QuestionRequest }

// questionState is the card the user is answering. With options, rows
// 0..len(options)-1 are the choices and row len(options) is "write your own".
type questionState struct {
	request  QuestionRequest
	selected int
	writing  bool
	input    textarea.Model
	armedAt  time.Time
	demo     bool
	notice   string
	card     []string
	hits     []cardHit
	cardLeft int
	cardCols int
	// cut marks options whose label the last render did not show whole, or did
	// not show at all. A shortcut never answers with one of those: it highlights
	// it first, so the user sees the full text before choosing.
	cut []bool
	// review is set when the card triages review findings instead of asking.
	review *reviewState
}

// cardHit maps a run of card lines (inclusive) to the option row it shows.
type cardHit struct{ top, bottom, row int }

func newQuestionState(request QuestionRequest) *questionState {
	q := &questionState{request: request, armedAt: time.Now()}
	input := textarea.New()
	input.Prompt = ""
	input.SetPromptFunc(1, func(textarea.PromptInfo) string { return " " })
	input.ShowLineNumbers = false
	input.CharLimit = 8192
	input.DynamicHeight = true
	input.MinHeight = 1
	input.MaxHeight = 3
	input.SetHeight(1)
	input.SetVirtualCursor(true)
	input.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("alt+enter", "shift+enter", "ctrl+j"))
	input.SetStyles(questionInputStyles())
	input.Placeholder = "Write your own answer…"
	if request.Review != nil {
		q.review = newReviewState(*request.Review)
		input.Placeholder = "Tell JULIA what to do about this finding…"
	} else if q.freeform() {
		input.Placeholder = "Type your answer…"
		q.writing = true
	}
	q.input = input
	return q
}

func questionInputStyles() textarea.Styles {
	s := textarea.DefaultDarkStyles()
	base := lipgloss.NewStyle().Background(cardInset).Foreground(cream)
	s.Focused.Base = base
	s.Focused.Text = base
	s.Focused.Prompt = base
	s.Focused.Placeholder = lipgloss.NewStyle().Background(cardInset).Foreground(muted)
	s.Focused.CursorLine = base
	s.Focused.EndOfBuffer = base
	s.Blurred = s.Focused
	s.Cursor.Color = coral
	return s
}

func (q *questionState) options() []core.QuestionOption { return q.request.Question.Options }
func (q *questionState) freeform() bool                 { return len(q.options()) == 0 && q.review == nil }
func (q *questionState) ownRow() int                    { return len(q.options()) }
func (q *questionState) rows() int {
	if q.freeform() {
		return 0
	}
	return len(q.options()) + 1
}

func replyQuestion(request QuestionRequest, answer core.Answer) {
	select {
	case request.Reply <- answer:
	default:
	}
}

func (m *model) waitQuestion() tea.Cmd {
	if m.cfg.Questions == nil || m.questionListening {
		return nil
	}
	m.questionListening = true
	return func() tea.Msg {
		select {
		case request, ok := <-m.cfg.Questions:
			if !ok {
				return nil
			}
			return questionMsg{request}
		case <-m.lifetime.Done():
			return nil
		}
	}
}

func (m *model) openQuestion(request QuestionRequest, demo bool) tea.Cmd {
	q := newQuestionState(request)
	q.demo = demo
	m.question = q
	m.input.Blur()
	var cmd tea.Cmd
	if q.writing {
		cmd = q.input.Focus()
	}
	m.relayoutQuestion()
	m.viewport.GotoBottom()
	if demo {
		return cmd
	}
	return tea.Batch(cmd, m.notifyQuestion(request))
}

// closeQuestion removes the card and promotes whatever was waiting behind it.
func (m *model) closeQuestion() tea.Cmd {
	m.question = nil
	var focus tea.Cmd
	if m.picker == nil {
		focus = m.input.Focus()
	}
	m.syncQuestionLayout()
	m.refresh(false)
	return tea.Batch(focus, m.waitQuestion(), m.promoteInteraction())
}

// promoteInteraction shows a request that arrived while another was on screen.
// Approvals and questions never share the screen; the later one waits its turn.
func (m *model) promoteInteraction() tea.Cmd {
	if m.pending != nil || m.question != nil {
		return nil
	}
	if request := m.deferredApproval; request != nil {
		m.deferredApproval = nil
		m.pending = request
		m.refresh(true)
		return m.notifyApproval(request.Approval)
	}
	if request := m.deferredQuestion; request != nil {
		m.deferredQuestion = nil
		return m.openQuestion(*request, false)
	}
	return nil
}

// dismissInteractions releases every question or approval that is waiting on
// the user, because the task they belong to is ending.
func (m *model) dismissInteractions(reason string) {
	if q := m.question; q != nil {
		if !q.demo {
			replyQuestion(q.request, core.Answer{Skipped: true})
		}
		m.question = nil
		m.input.Focus()
	}
	if request := m.deferredQuestion; request != nil {
		replyQuestion(*request, core.Answer{Skipped: true})
		m.deferredQuestion = nil
	}
	if request := m.deferredApproval; request != nil {
		reply(*request, false, reason)
		m.deferredApproval = nil
	}
	m.syncQuestionLayout()
}

func (m *model) answerQuestion(answer core.Answer) tea.Cmd {
	q := m.question
	if q == nil {
		return nil
	}
	switch {
	case q.demo && q.review != nil:
		m.recordDemoReview(q, answer)
	case q.demo:
		m.recordDemoQuestion(q, answer)
	default:
		replyQuestion(q.request, answer)
	}
	return m.closeQuestion()
}

func (m *model) recordDemoQuestion(q *questionState, answer core.Answer) {
	arguments, _ := json.Marshal(q.request.Question)
	result := core.QuestionResult{Skipped: answer.Skipped}
	switch {
	case answer.Skipped:
	case answer.Option > 0:
		result.Answer, result.Source, result.Option = answer.Text, "option", answer.Option
	default:
		result.Answer, result.Source = answer.Text, "freeform"
	}
	content, _ := json.Marshal(result)
	call := &core.ToolCall{ID: "demo-question", Name: core.QuestionTool, Arguments: arguments}
	m.entries = append(m.entries, entry{role: "tool", call: call, done: true, result: &core.ToolResult{Content: string(content)}})
	m.toolCount++
	m.notice = "Preview only · nothing was sent"
}

func demoQuestion(freeform bool) core.Question {
	if freeform {
		return core.Question{Text: "What should the new flag be called, and should it be on by default?"}
	}
	return core.Question{
		Text: "The runner cancels by signalling the ordinary process group. How far should I go with the cancellation change?",
		Options: []core.QuestionOption{
			{Label: "Keep the process-group signal (Recommended)", Description: "Smallest change. Add a regression test for the cancellation path only."},
			{Label: "Move to a context-scoped supervisor", Description: "Cleaner shutdown ordering, but it touches every call site that starts a command."},
			{Label: "Leave cancellation alone", Description: "Report the findings and make no code changes."},
		},
	}
}

// printableKey returns the text a key press would type, or "" for commands.
func printableKey(msg tea.KeyPressMsg) string {
	if msg.Mod&(tea.ModCtrl|tea.ModAlt|tea.ModMeta|tea.ModHyper|tea.ModSuper) != 0 {
		return ""
	}
	if msg.Text != "" {
		return msg.Text
	}
	if unicode.IsPrint(msg.Code) {
		return string(msg.Code)
	}
	return ""
}

func (m *model) updateQuestion(msg tea.Msg) tea.Cmd {
	q := m.question
	if q == nil {
		return nil
	}
	var cmd tea.Cmd
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		if time.Since(q.armedAt) < questionGuard {
			return nil
		}
		if q.review != nil {
			cmd = m.reviewKey(q, msg)
			break
		}
		cmd = m.questionKey(q, msg)
	case tea.PasteMsg:
		if time.Since(q.armedAt) < questionGuard {
			return nil
		}
		if q.review != nil {
			cmd = m.reviewPaste(q, msg)
			q.notice = ""
			break
		}
		if !q.writing {
			cmd = m.beginWriting(q)
		}
		var update tea.Cmd
		q.input, update = q.input.Update(msg)
		cmd = tea.Batch(cmd, update)
		q.notice = ""
	default:
		if q.writing {
			q.input, cmd = q.input.Update(msg)
		}
	}
	if m.question == q {
		m.relayoutQuestion()
	}
	return cmd
}

func (m *model) leaveField(q *questionState) tea.Cmd {
	q.writing = false
	q.input.Blur()
	q.selected = max(0, q.ownRow()-1)
	return nil
}

func (m *model) beginWriting(q *questionState) tea.Cmd {
	q.writing = true
	q.selected = q.ownRow()
	return q.input.Focus()
}

func (m *model) questionKey(q *questionState, msg tea.KeyPressMsg) tea.Cmd {
	q.notice = ""
	name := msg.String()
	if q.writing {
		switch name {
		case "esc":
			if q.freeform() {
				return m.answerQuestion(core.Answer{Skipped: true})
			}
			q.writing = false
			q.input.Blur()
			return nil
		case "enter":
			text := strings.TrimSpace(q.input.Value())
			if text == "" {
				q.notice = "Type an answer first"
				return nil
			}
			return m.answerQuestion(core.Answer{Text: text})
		case "up", "shift+tab":
			if !q.freeform() {
				if name == "shift+tab" || (q.input.Line() == 0 && q.input.LineInfo().RowOffset == 0) {
					return m.leaveField(q)
				}
				// Wide runes can leave the textarea unable to move up; then Up
				// should still lead out of the field instead of doing nothing.
				line, row := q.input.Line(), q.input.LineInfo().RowOffset
				var cmd tea.Cmd
				q.input, cmd = q.input.Update(msg)
				if q.input.Line() == line && q.input.LineInfo().RowOffset == row {
					return m.leaveField(q)
				}
				return cmd
			}
		}
		var cmd tea.Cmd
		q.input, cmd = q.input.Update(msg)
		return cmd
	}
	rows := q.rows()
	switch name {
	case "esc":
		return m.answerQuestion(core.Answer{Skipped: true})
	case "up", "ctrl+p", "shift+tab":
		q.selected = (q.selected - 1 + rows) % rows
	case "down", "ctrl+n", "tab":
		q.selected = (q.selected + 1) % rows
	case "home":
		q.selected = 0
	case "end":
		q.selected = rows - 1
	case "pgup":
		m.viewport.ScrollUp(max(1, m.viewport.Height()-1))
	case "pgdown":
		m.viewport.ScrollDown(max(1, m.viewport.Height()-1))
	case "ctrl+home":
		m.viewport.GotoTop()
	case "ctrl+end":
		m.viewport.GotoBottom()
	case "enter":
		if q.selected == q.ownRow() {
			return m.beginWriting(q)
		}
		return m.pickOption(q, q.selected)
	case "space":
		// Not a way to choose, and a poor way to begin an answer.
	default:
		if len(name) == 1 && name[0] >= '1' && name[0] <= '9' && int(name[0]-'0') <= len(q.options()) {
			return m.pickByShortcut(q, int(name[0]-'1'))
		}
		if text := printableKey(msg); text != "" {
			cmd := m.beginWriting(q)
			q.input.InsertString(text)
			return cmd
		}
	}
	return nil
}

// clickQuestion picks the option row under a left click on the card.
func (m *model) clickQuestion(click tea.MouseClickMsg) tea.Cmd {
	q := m.question
	if q == nil || click.Button != tea.MouseLeft || time.Since(q.armedAt) < questionGuard {
		return nil
	}
	if click.X < q.cardLeft || click.X >= q.cardLeft+q.cardCols {
		return nil
	}
	// The header takes three rows; the card sits below the conversation.
	line := click.Y - 3 - m.viewport.Height()
	if q.review != nil {
		cmd := m.clickReview(q, line, click.X-q.cardLeft)
		if m.question == q {
			m.relayoutQuestion()
		}
		return cmd
	}
	for _, hit := range q.hits {
		if line < hit.top || line > hit.bottom {
			continue
		}
		var cmd tea.Cmd
		if hit.row == q.ownRow() {
			cmd = m.beginWriting(q)
		} else {
			cmd = m.pickByShortcut(q, hit.row)
		}
		if m.question == q {
			m.relayoutQuestion()
		}
		return cmd
	}
	return nil
}

func (m *model) pickOption(q *questionState, index int) tea.Cmd {
	return m.answerQuestion(core.Answer{Text: q.options()[index].Label, Option: index + 1})
}

// pickByShortcut answers with a digit or a click, unless that option's label was
// not shown whole: then it is only highlighted, and Enter confirms what is shown.
func (m *model) pickByShortcut(q *questionState, index int) tea.Cmd {
	if index != q.selected && index < len(q.cut) && q.cut[index] {
		q.writing = false
		q.input.Blur()
		q.selected = index
		q.notice = "Shown whole · enter to choose"
		return nil
	}
	return m.pickOption(q, index)
}

// relayoutQuestion rebuilds the card and gives the conversation the rest of the body.
func (m *model) relayoutQuestion() {
	if m.question == nil {
		m.syncQuestionLayout()
		return
	}
	q := m.question
	if q.review != nil {
		q.card, q.cardCols = m.reviewCard()
		q.hits = nil
	} else {
		q.card, q.hits, q.cardCols = m.questionCard()
	}
	q.cardLeft = m.margin()
	m.syncQuestionLayout()
}

func (m *model) syncQuestionLayout() {
	height := m.bodyHeight
	if m.question != nil {
		height = max(0, m.bodyHeight-len(m.question.card))
	}
	if m.viewport.Height() == height {
		return
	}
	follow := m.viewport.AtBottom()
	m.viewport.SetHeight(height)
	if follow {
		m.viewport.GotoBottom()
	}
}

// questionBody is the body area while a question is open: the conversation
// above, the card pinned to the bottom.
func (m *model) questionBody() string {
	card := strings.Join(m.question.card, "\n")
	if m.viewport.Height() == 0 {
		return card
	}
	return m.viewport.View() + "\n" + card
}

// --- rendering --------------------------------------------------------------

func ink(bg, fg color.Color) lipgloss.Style { return lipgloss.NewStyle().Foreground(fg).Background(bg) }

// paint fits content to width and fills the remainder with the background.
func paint(bg color.Color, width int, content string) string {
	content = ansi.Truncate(content, width, "")
	if pad := width - ansi.StringWidth(content); pad > 0 {
		content += ink(bg, cream).Render(strings.Repeat(" ", pad))
	}
	return content
}

func wrapPlain(text string, width int) []string {
	var out []string
	text = strings.ReplaceAll(sanitize(text), "\t", "    ")
	for _, paragraph := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		paragraph = strings.TrimRight(paragraph, " ")
		if paragraph == "" {
			out = append(out, "")
			continue
		}
		out = append(out, strings.Split(ansi.Wrap(paragraph, max(1, width), ""), "\n")...)
	}
	return out
}

// clampLines keeps at most n lines, marking the cut with an ellipsis.
func clampLines(lines []string, n, width int) []string {
	if n < 1 {
		return nil
	}
	if len(lines) <= n {
		return lines
	}
	lines = append([]string(nil), lines[:n]...)
	lines[n-1] = ansi.Truncate(lines[n-1], max(1, width-1), "") + "…"
	return lines
}

// clampMiddle keeps the start and the last line of a long text and marks the cut
// between them, so the closing sentence of a question, usually the actual ask,
// is never the part that disappears.
func clampMiddle(lines []string, n, width int) []string {
	if len(lines) <= n || n < 3 {
		return clampLines(lines, n, width)
	}
	out := append([]string(nil), lines[:n-2]...)
	return append(out, ansi.Truncate("…", width, ""), lines[len(lines)-1])
}

// splitRecommended separates a trailing "(Recommended)" marker from a label.
func splitRecommended(label string) (string, bool) {
	const marker = "(recommended)"
	trimmed := strings.TrimSpace(label)
	if len(trimmed) > len(marker) && strings.EqualFold(trimmed[len(trimmed)-len(marker):], marker) {
		return strings.TrimSpace(trimmed[:len(trimmed)-len(marker)]), true
	}
	return label, false
}

type hintPart struct{ key, label string }

func renderHint(width int, variants ...[]hintPart) string {
	var last string
	for _, parts := range variants {
		var out []string
		for _, part := range parts {
			out = append(out, ink(cardBG, cardQuote).Render(part.key)+ink(cardBG, muted).Render(" "+part.label))
		}
		last = strings.Join(out, ink(cardBG, faint).Render(" · "))
		if ansi.StringWidth(last) <= width {
			return last
		}
	}
	return last
}

// cardPlan trades detail for height until the card fits.
type cardPlan struct {
	level  int // 0 every description, 1 selected only, 2 none
	spaced bool
	airy   bool
	qLines int
	field  int
}

var cardPlans = []cardPlan{
	{0, true, true, 14, 4},
	{0, false, true, 14, 4},
	{1, false, true, 12, 4},
	{1, false, false, 10, 3},
	{2, false, true, 8, 3},
	{2, false, false, 6, 2},
	{2, false, false, 4, 1},
}

func (m *model) questionCard() ([]string, []cardHit, int) {
	q := m.question
	width := m.mainWidth - 4
	if width < 24 {
		width = m.mainWidth
	}
	width = max(8, width)
	budget := m.bodyHeight
	if m.bodyHeight >= 14 {
		budget -= 3
	}
	if budget < 9 || width < 24 {
		return m.compactCard(q, width, max(1, budget)), nil, width
	}
	for _, plan := range cardPlans[:len(cardPlans)-1] {
		if card, hits := m.assembleCard(q, width, plan, budget, false); len(card) <= budget {
			return card, hits, width
		}
	}
	card, hits := m.assembleCard(q, width, cardPlans[len(cardPlans)-1], budget, true)
	if len(card) > budget {
		card, hits = card[:budget], nil
	}
	return card, hits, width
}

func (m *model) assembleCard(q *questionState, width int, plan cardPlan, budget int, window bool) ([]string, []cardHit) {
	q.cut = make([]bool, len(q.options()))
	iw := width - 2
	textWidth := max(4, iw-4)
	blank := paint(cardBG, iw, "")
	edge := func(s string) string { return lipgloss.NewStyle().Foreground(cardEdge).Render(s) }
	frame := func(inner string) string {
		return ink(cardBG, cardEdge).Render("▏") + inner + ink(cardBG, cardEdge).Render("▕")
	}
	pad := ink(cardBG, cream).Render("  ")

	// Chrome: top rule, bottom rule, hint, optional breathing room.
	var head []string
	if plan.airy {
		head = append(head, blank)
	}
	for _, line := range clampMiddle(wrapPlain(q.request.Question.Text, textWidth), plan.qLines, textWidth) {
		head = append(head, paint(cardBG, iw, pad+ink(cardBG, cream).Bold(true).Render(line)))
	}
	if plan.airy {
		head = append(head, blank)
	}
	var foot []string
	if plan.airy {
		foot = append(foot, blank)
	}
	foot = append(foot, paint(cardBG, iw, pad+m.cardHint(q, iw-4)))
	if plan.airy {
		foot = append(foot, blank)
	}

	var blocks [][]string
	if q.freeform() {
		blocks = append(blocks, m.fieldBlock(q, iw, 2, 2, plan.field, cardBG, ""))
	} else {
		for i := range q.options() {
			blocks = append(blocks, m.optionBlock(q, i, iw, plan))
		}
		blocks = append(blocks, m.ownBlock(q, iw, plan))
	}
	selected := q.selected
	if q.freeform() {
		selected = 0
	}
	avail := budget - 2 - len(head) - len(foot)
	if plan.spaced {
		avail -= len(blocks) - 1
	}
	visible := blocks
	var first, above, below int
	if window {
		visible, first, above, below = windowBlocks(blocks, selected, avail)
		for i := range q.cut {
			if i < first || i >= first+len(visible) {
				q.cut[i] = true
			}
		}
	}
	var body []string
	var hits []cardHit
	if above > 0 {
		body = append(body, paint(cardBG, iw, pad+ink(cardBG, faint).Render(fmt.Sprintf("↑ %d more", above))))
	}
	for i, block := range visible {
		if plan.spaced && i > 0 {
			body = append(body, blank)
		}
		if !q.freeform() {
			// Card line 0 is the top rule, then the head rows, then the body.
			top := 1 + len(head) + len(body)
			hits = append(hits, cardHit{top: top, bottom: top + len(block) - 1, row: first + i})
		}
		body = append(body, block...)
	}
	if below > 0 {
		body = append(body, paint(cardBG, iw, pad+ink(cardBG, faint).Render(fmt.Sprintf("↓ %d more", below))))
	}

	lines := []string{m.cardTop(q, width)}
	for _, line := range head {
		lines = append(lines, frame(line))
	}
	for _, line := range body {
		lines = append(lines, frame(line))
	}
	for _, line := range foot {
		lines = append(lines, frame(line))
	}
	return append(lines, edge(strings.Repeat("▔", width))), hits
}

func (m *model) cardTop(q *questionState, width int) string {
	edge := lipgloss.NewStyle().Foreground(cardEdge)
	left := edge.Render("▁▁ ") + violetStyle.Bold(true).Render("✦ JULIA ASKS") + " "
	right := ""
	if !q.freeform() {
		right = " " + faintStyle.Render(fmt.Sprintf("%d / %d", q.selected+1, q.rows())) + " "
	}
	fill := width - ansi.StringWidth(left) - ansi.StringWidth(right) - 2
	if fill < 1 {
		right = ""
		fill = width - ansi.StringWidth(left) - 2
	}
	if fill < 1 {
		left = edge.Render("▁▁ ") + violetStyle.Bold(true).Render("✦") + " "
		fill = max(0, width-ansi.StringWidth(left)-2)
	}
	return left + edge.Render(strings.Repeat("▁", fill)) + right + edge.Render("▁▁")
}

func (m *model) cardHint(q *questionState, width int) string {
	if q.notice != "" {
		return ink(cardBG, coral).Render(q.notice)
	}
	switch {
	case q.freeform():
		return renderHint(width,
			[]hintPart{{"enter", "send"}, {"alt+enter", "new line"}, {"esc", "skip"}},
			[]hintPart{{"enter", "send"}, {"esc", "skip"}})
	case q.writing:
		return renderHint(width,
			[]hintPart{{"enter", "send"}, {"alt+enter", "new line"}, {"↑", "options"}, {"esc", "back"}},
			[]hintPart{{"enter", "send"}, {"esc", "back"}})
	}
	pick := fmt.Sprintf("1–%d", len(q.options()))
	if len(q.options()) == 1 {
		pick = "1"
	}
	return renderHint(width,
		[]hintPart{{"↑↓", "move"}, {"enter", "choose"}, {pick, "pick"}, {"type", "write your own"}, {"esc", "skip"}},
		[]hintPart{{"↑↓", "move"}, {"enter", "choose"}, {pick, "pick"}, {"esc", "skip"}},
		[]hintPart{{"↑↓", "move"}, {"enter", "choose"}, {"esc", "skip"}},
		[]hintPart{{"enter", "choose"}, {"esc", "skip"}})
}

// optionBlock renders one choice: a label line and, by plan, its description.
func (m *model) optionBlock(q *questionState, index, iw int, plan cardPlan) []string {
	option := q.options()[index]
	selected := index == q.selected && !q.writing
	bg := cardBG
	if selected {
		bg = cardSelect
	}
	name, recommended := splitRecommended(oneLine(option.Label))
	textWidth := max(4, iw-9)
	numberStyle := ink(bg, faint)
	labelStyle := ink(bg, cream)
	descStyle := ink(bg, cardDim)
	if selected {
		numberStyle = ink(bg, coral).Bold(true)
		labelStyle = ink(bg, cream).Bold(true)
		descStyle = ink(bg, muted)
	}
	badge := ""
	if recommended {
		badge = "  ★ recommended"
	}
	// The highlighted option is shown whole, because it is the one about to be
	// chosen; the others may be shortened to keep the list compact.
	limit := 1
	switch {
	case selected:
		limit = 6
	case plan.level < 2:
		limit = 2
	}
	wrapped := wrapPlain(name, textWidth)
	q.cut[index] = len(wrapped) > limit
	nameLines := clampLines(wrapped, limit, textWidth)
	lead := func(i int) string {
		number := " "
		if i == 0 {
			number = numberStyle.Render(strconv.Itoa(index + 1))
		}
		return ink(bg, cream).Render("  ") + gutter(bg, selected) + ink(bg, cream).Render(" ") + number + ink(bg, cream).Render("  ")
	}
	var lines []string
	for i, text := range nameLines {
		line := lead(i) + labelStyle.Render(text)
		if i == len(nameLines)-1 && badge != "" {
			if ansi.StringWidth(text)+ansi.StringWidth(badge) <= textWidth {
				line += ink(bg, green).Render(badge)
			} else {
				lines = append(lines, paint(bg, iw, line))
				line = lead(1) + ink(bg, green).Render(strings.TrimSpace(badge))
			}
		}
		lines = append(lines, paint(bg, iw, line))
	}
	showDesc := plan.level == 0 || (plan.level == 1 && selected)
	if description := oneLine(option.Description); showDesc && description != "" {
		maxLines := 2
		if selected {
			maxLines = 3
		}
		for _, line := range clampLines(wrapPlain(description, textWidth), maxLines, textWidth) {
			lines = append(lines, paint(bg, iw, ink(bg, cream).Render("  ")+gutter(bg, selected)+ink(bg, cream).Render("    ")+descStyle.Render(line)))
		}
	}
	return lines
}

func gutter(bg color.Color, selected bool) string {
	if selected {
		return ink(bg, coral).Render("▌")
	}
	return ink(bg, cream).Render(" ")
}

// ownBlock is the "write your own answer" row, which grows an input when active.
func (m *model) ownBlock(q *questionState, iw int, plan cardPlan) []string {
	selected := q.selected == q.ownRow()
	bg := cardBG
	if selected {
		bg = cardSelect
	}
	iconStyle := ink(bg, faint)
	labelStyle := ink(bg, muted)
	if selected {
		iconStyle = ink(bg, coral).Bold(true)
		labelStyle = ink(bg, cream).Bold(true)
	}
	text := "Write your own answer"
	if q.writing {
		text = "Your answer"
	}
	first := ink(bg, cream).Render("  ") + gutter(bg, selected) + ink(bg, cream).Render(" ") + iconStyle.Render("✎") + ink(bg, cream).Render("  ") + labelStyle.Render(text)
	lines := []string{paint(bg, iw, first)}
	if q.writing {
		lines = append(lines, m.fieldBlock(q, iw, 7, 2, plan.field, bg, gutter(bg, true))...)
	} else if draft := strings.TrimSpace(q.input.Value()); draft != "" && plan.level < 2 {
		line, _, _ := strings.Cut(oneLine(draft), "\n")
		lines = append(lines, paint(bg, iw, ink(bg, cream).Render("  ")+gutter(bg, selected)+ink(bg, cream).Render("    ")+ink(bg, faint).Italic(true).Render("“"+ansi.Truncate(line, max(4, iw-13), "…")+"”")))
	}
	return lines
}

// fieldBlock draws the inset text field. indent is the left offset, rightPad the
// space kept clear on the right; bar, when set, sits in column 2 of every row.
func (m *model) fieldBlock(q *questionState, iw, indent, rightPad, maxRows int, bg color.Color, bar string) []string {
	fieldWidth := max(4, iw-indent-rightPad)
	q.input.SetWidth(fieldWidth)
	// The textarea grows with its content up to maxRows and keeps its own scroll
	// position right; resizing it by hand after it scrolled would hide lines.
	q.input.MaxHeight = max(1, maxRows)
	q.input.SetHeight(max(q.input.Height(), visualLines(q.input.Value(), max(1, fieldWidth-1))))
	rows := q.input.Height()
	view := strings.Split(strings.TrimRight(q.input.View(), "\n"), "\n")
	var lines []string
	for i := 0; i < rows && i < len(view); i++ {
		left := ink(bg, cream).Render(strings.Repeat(" ", indent))
		if bar != "" {
			left = ink(bg, cream).Render("  ") + bar + ink(bg, cream).Render(strings.Repeat(" ", indent-3))
		}
		field := paint(cardInset, fieldWidth, view[i])
		lines = append(lines, paint(bg, iw, left+field))
	}
	return lines
}

// visualLines estimates the wrapped height of text at width columns.
func visualLines(text string, width int) int {
	total := 0
	for _, line := range strings.Split(text, "\n") {
		total += max(1, (ansi.StringWidth(line)+width-1)/width)
	}
	return total
}

// windowBlocks picks the run of blocks around the selection that fits avail
// lines, reserving a line for each "more" marker.
func windowBlocks(blocks [][]string, selected, avail int) (visible [][]string, first, above, below int) {
	selected = min(max(0, selected), len(blocks)-1)
	size := func(from, to int) int {
		total := 0
		for _, block := range blocks[from:to] {
			total += len(block)
		}
		return total
	}
	if size(0, len(blocks)) <= avail {
		return blocks, 0, 0, 0
	}
	markers := func(start, end int) int {
		reserve := 0
		if start > 0 {
			reserve++
		}
		if end < len(blocks) {
			reserve++
		}
		return reserve
	}
	start, end := selected, selected+1
	for {
		grown := false
		if end < len(blocks) && size(start, end+1)+markers(start, end+1) <= avail {
			end++
			grown = true
		}
		if start > 0 && size(start-1, end)+markers(start-1, end) <= avail {
			start--
			grown = true
		}
		if !grown {
			break
		}
	}
	if size(start, end)+markers(start, end) > avail {
		// Even the highlighted option alone does not fit: show as much of it as
		// does, and give up the "more" markers when there is no room for them.
		block := blocks[selected]
		reserve := markers(selected, selected+1)
		if avail-reserve < 1 {
			reserve = 0
		}
		if keep := max(1, avail-reserve); len(block) > keep {
			block = block[:keep]
		}
		if reserve == 0 {
			return [][]string{block}, selected, 0, 0
		}
		return [][]string{block}, selected, selected, len(blocks) - selected - 1
	}
	return blocks[start:end], start, start, len(blocks) - end
}

// compactCard is the few-line fallback for very small terminals.
func (m *model) compactCard(q *questionState, width, budget int) []string {
	field := max(4, width-2)
	var question, answer string
	q.cut = make([]bool, len(q.options()))
	for i := range q.cut {
		q.cut[i] = true
	}
	// The last line of a long question is its ask.
	ask := q.request.Question.Text
	for _, line := range strings.Split(sanitize(ask), "\n") {
		if strings.TrimSpace(line) != "" {
			ask = line
		}
	}
	question = violetStyle.Bold(true).Render("✦ ") + creamStyle.Bold(true).Render(ansi.Truncate(oneLine(ask), max(1, width-2), "…"))
	switch {
	case q.writing:
		q.input.SetWidth(field)
		q.input.MaxHeight = 1
		q.input.SetHeight(1)
		view := strings.Split(strings.TrimRight(q.input.View(), "\n"), "\n")
		answer = paint(cardInset, width, view[0])
	case q.selected == q.ownRow():
		answer = coralStyle.Render("› ✎ ") + creamStyle.Render("Write your own answer") + faintStyle.Render(fmt.Sprintf("  %d/%d", q.selected+1, q.rows()))
	default:
		name, _ := splitRecommended(oneLine(q.options()[q.selected].Label))
		answer = coralStyle.Render("› ") + creamStyle.Bold(true).Render(ansi.Truncate(name, max(1, width-9), "…")) + faintStyle.Render(fmt.Sprintf("  %d/%d", q.selected+1, q.rows()))
	}
	hint := mutedStyle.Render("↑↓ enter esc")
	if q.writing {
		hint = mutedStyle.Render("enter send · esc back")
		if q.freeform() {
			hint = mutedStyle.Render("enter send · esc skip")
		}
	}
	lines := []string{question, answer, hint}
	if budget < 3 {
		lines = []string{answer, hint}
	}
	if budget < 2 {
		lines = []string{answer}
	}
	for i := range lines {
		lines[i] = ansi.Truncate(lines[i], width, "…")
	}
	return lines
}

// --- transcript -------------------------------------------------------------

// renderQuestionEntry shows a finished or pending question as a quote with the
// user's answer underneath. It reports false when the call cannot be decoded.
func (m *model) renderQuestionEntry(item entry, w int) (string, bool) {
	var question core.Question
	if item.call == nil || json.Unmarshal(item.call.Arguments, &question) != nil || strings.TrimSpace(question.Text) == "" {
		return "", false
	}
	icon := violetStyle.Render("?")
	if item.result != nil && item.result.IsError {
		icon = coralStyle.Render("!")
	}
	if !item.done && item.result == nil {
		// The open card shows the question itself; don't repeat it above.
		return lipgloss.NewStyle().BorderLeft(true).BorderStyle(lipgloss.NormalBorder()).BorderForeground(cardEdge).PaddingLeft(1).Render(violetStyle.Render("◌") + " " + mutedStyle.Render("asking you a question…")), true
	}
	lines := clampMiddle(wrapPlain(question.Text, max(8, w-2)), 14, max(8, w-2))
	out := []string{icon + " " + creamStyle.Render(lines[0])}
	for _, line := range lines[1:] {
		out = append(out, "  "+creamStyle.Render(line))
	}
	switch {
	case item.result == nil:
		out = append(out, faintStyle.Render("  no answer recorded"))
	case item.result.IsError:
		first, _, _ := strings.Cut(strings.TrimSpace(sanitize(item.result.Content)), "\n")
		out = append(out, coralStyle.Render("  "+ansi.Truncate(first, max(8, w-2), "…")))
	default:
		var result core.QuestionResult
		if json.Unmarshal([]byte(item.result.Content), &result) != nil {
			out = append(out, faintStyle.Render("  "+ansi.Truncate(oneLine(item.result.Content), max(8, w-2), "…")))
			break
		}
		if result.Skipped {
			out = append(out, mutedStyle.Italic(true).Render("  ↳ skipped"))
			break
		}
		suffix := "written"
		if result.Source == "option" {
			suffix = "option " + strconv.Itoa(result.Option)
		}
		answerLines := clampLines(wrapPlain(result.Answer, max(8, w-4)), 10, max(8, w-4))
		for i, line := range answerLines {
			prefix := "    "
			if i == 0 {
				prefix = "  " + lipgloss.NewStyle().Foreground(green).Render("↳") + " "
			}
			out = append(out, prefix+lipgloss.NewStyle().Foreground(green).Render(line))
		}
		// The source tag rides on the last line when it fits, else gets its own.
		tag := faintStyle.Render("· " + suffix)
		last := answerLines[len(answerLines)-1]
		if 4+ansi.StringWidth(last)+2+ansi.StringWidth(tag) <= max(8, w) {
			out[len(out)-1] += "  " + tag
		} else {
			out = append(out, "    "+tag)
		}
	}
	return lipgloss.NewStyle().BorderLeft(true).BorderStyle(lipgloss.NormalBorder()).BorderForeground(cardEdge).PaddingLeft(1).Render(strings.Join(out, "\n")), true
}

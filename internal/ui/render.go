package ui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"

	"julia/internal/config"
	"julia/internal/core"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/glamour/v2"
	"charm.land/glamour/v2/styles"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

var (
	background  = lipgloss.Color("#15131A")
	panel       = lipgloss.Color("#1E1B26")
	cream       = lipgloss.Color("#F1EBE3")
	muted       = lipgloss.Color("#9A94A6")
	faint       = lipgloss.Color("#4E4860")
	border      = lipgloss.Color("#5A5270")
	coral       = lipgloss.Color("#F49D89")
	violet      = lipgloss.Color("#C6B6F2")
	cyan        = lipgloss.Color("#92CED2")
	green       = lipgloss.Color("#A9C9AD")
	amber       = lipgloss.Color("#F9D399")
	pillBG      = lipgloss.Color("#2A2536") // a raised chip: a tool name
	pillText    = lipgloss.Color("#D9D1E6")
	coralStyle  = lipgloss.NewStyle().Foreground(coral)
	violetStyle = lipgloss.NewStyle().Foreground(violet)
	cyanStyle   = lipgloss.NewStyle().Foreground(cyan)
	mutedStyle  = lipgloss.NewStyle().Foreground(muted)
	faintStyle  = lipgloss.NewStyle().Foreground(faint)
	creamStyle  = lipgloss.NewStyle().Foreground(cream)
)

func composerStyles() textarea.Styles {
	s := textarea.DefaultDarkStyles()
	s.Focused.Base = lipgloss.NewStyle().Background(panel).Foreground(cream)
	s.Focused.Text = creamStyle
	s.Focused.Prompt = coralStyle
	s.Focused.Placeholder = mutedStyle
	s.Focused.CursorLine = lipgloss.NewStyle()
	s.Focused.EndOfBuffer = lipgloss.NewStyle()
	s.Blurred = s.Focused
	s.Cursor.Color = coral
	return s
}

// sanitize prevents provider output, filenames, and command results from
// injecting OSC clipboard writes, terminal control sequences, or bidi controls.
func sanitize(text string) string {
	text = ansi.Strip(strings.ToValidUTF8(text, "�"))
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if unicode.IsControl(r) || r == '\u202a' || r == '\u202b' || r == '\u202c' || r == '\u202d' || r == '\u202e' || r == '\u2066' || r == '\u2067' || r == '\u2068' || r == '\u2069' {
			return -1
		}
		return r
	}, text)
}

func (m *model) resize(width, height int) {
	wasFollowing := m.viewport.AtBottom()
	m.width, m.height = max(20, width), max(8, height)
	contentWidth := m.width - 2*m.margin()
	m.mainWidth = contentWidth
	if m.hasSidebar() {
		m.mainWidth -= 29
	}
	// The whole composer panel, frame and title row included, stays within
	// half the screen however long the draft gets.
	composer := m.composerStyle()
	m.input.MaxHeight = max(2, m.height/2-composer.GetVerticalFrameSize()-1)
	m.input.SetWidth(max(8, m.mainWidth-composer.GetHorizontalFrameSize()))
	m.composerRows = m.input.Height()
	// The placeholder emits just one end-of-buffer space. Fill its whole row
	// here so the textarea viewport cannot pad it with the terminal background.
	inputStyles := m.input.Styles()
	inputStyles.Focused.EndOfBuffer = inputStyles.Focused.EndOfBuffer.Width(m.input.Width())
	inputStyles.Blurred.EndOfBuffer = inputStyles.Blurred.EndOfBuffer.Width(m.input.Width())
	m.input.SetStyles(inputStyles)
	if m.picker != nil {
		m.picker.search.SetWidth(max(8, m.mainWidth-8))
	}
	// Measure the fixed controls so spacing and title changes cannot push the
	// composer or shortcuts off screen. The header always occupies three rows.
	m.bodyHeight = max(1, m.height-3-lipgloss.Height(m.bottomView()))
	m.queueRows = len(m.queueStrip())
	m.viewport.SetWidth(m.mainWidth)
	if m.question == nil {
		// An open question keeps its own height until relayoutQuestion resizes it,
		// so the follow decision above is not made against a taller viewport.
		m.viewport.SetHeight(m.bodyHeight)
	}
	m.refresh(wasFollowing)
}

func (m *model) margin() int {
	if m.width < 60 {
		return 1
	}
	return 2
}

func (m *model) hasSidebar() bool { return m.width >= 108 }

func (m *model) refresh(forceBottom bool) {
	m.relayoutQuestion()
	atBottom := m.viewport.AtBottom()
	if m.pending != nil {
		m.viewport.SetContent(m.approvalView())
		if forceBottom {
			m.viewport.GotoTop()
		}
		return
	}
	m.lines, m.rows = m.lines[:0], m.rows[:0]
	previousRole := ""
	for i := range m.entries {
		layout := m.renderEntry(&m.entries[i])
		if len(layout.rows) == 0 {
			continue
		}
		if previousRole != "" {
			if m.entries[i].role != "tool" || previousRole != "tool" {
				// Consecutive calls read as one run of work, with no row between them.
				m.lines = append(m.lines, "")
				m.rows = append(m.rows, rowRef{entry: -1})
			}
		}
		previousRole = m.entries[i].role
		m.lines = append(m.lines, strings.Split(layout.text, "\n")...)
		for _, row := range layout.rows {
			m.rows = append(m.rows, rowRef{entry: i, rowMeta: row})
		}
	}
	m.activityRow = m.busy && len(m.entries) > 0
	if m.activityRow {
		// Reserve the row under the latest entry; View draws the animated
		// activity there, so ticks never rebuild the conversation.
		m.lines = append(m.lines, "", "")
		m.rows = append(m.rows, rowRef{entry: -1, rowMeta: rowMeta{skip: true}}, rowRef{entry: -1, rowMeta: rowMeta{skip: true}})
	}
	m.viewport.SetContent(strings.Join(m.lines, "\n"))
	if forceBottom || atBottom {
		m.viewport.GotoBottom()
	}
}

// markdownCache keeps the Markdown renderer for the width last asked of it.
type markdownCache struct {
	renderer *glamour.TermRenderer
	width    int
}

func (cache *markdownCache) at(width int) *glamour.TermRenderer {
	if cache.renderer == nil || cache.width != width {
		cache.renderer, cache.width = markdownRenderer(width), width
	}
	return cache.renderer
}

// markdownRenderer renders a reply's Markdown in the interface's colors,
// wrapped to fit a column of width.
func markdownRenderer(width int) *glamour.TermRenderer {
	style := styles.DarkStyleConfig
	margin := uint(0)
	style.Document.Margin = &margin
	style.Document.BlockPrefix = ""
	style.Document.BlockSuffix = ""
	style.Document.Color = str("#F1EBE3")
	style.Heading.Color = str("#C6B6F2")
	style.Heading.Bold = ptr(true)
	style.H1.Color = str("#F49D89")
	style.H1.BackgroundColor = nil
	// Headings are set by weight and color, not by their Markdown markers.
	style.H1.Prefix, style.H1.Suffix = "", ""
	style.H2.Prefix, style.H3.Prefix, style.H4.Prefix, style.H5.Prefix, style.H6.Prefix = "", "", "", "", ""
	style.HorizontalRule.Color = str("#4E4860")
	style.HorizontalRule.Format = "\n────────\n"
	style.BlockQuote.Color = str("#9A94A6")
	style.Item.BlockPrefix = "• "
	style.Enumeration.Color = str("#C6B6F2")
	style.Emph.Color = str("#F1EBE3")
	style.Strong.Color = str("#F1EBE3")
	style.Code.Color = str("#C6B6F2")
	style.Code.BackgroundColor = str("#2A2536")
	style.CodeBlock.Theme = "dracula"
	style.CodeBlock.Margin = &margin
	style.Link.Color = str("#C6B6F2")
	style.LinkText.Color = str("#C6B6F2")
	renderer, _ := glamour.NewTermRenderer(glamour.WithStyles(style), glamour.WithWordWrap(max(10, width-4)))
	return renderer
}

func str(value string) *string { return &value }
func ptr[T any](value T) *T    { return &value }

func (m *model) renderEntry(item *entry) entryLayout {
	return m.renderEntryIn(item, column{width: m.mainWidth, markdown: m.markdown.at(m.mainWidth)})
}

// column is where an entry is laid out: the conversation, or a fleet agent's
// pane, whose Markdown wraps to its own narrower width.
type column struct {
	width    int
	markdown *glamour.TermRenderer
	// wordWrap breaks a message between words. The conversation breaks
	// between cells instead, so a copy can join a wrapped row back exactly.
	wordWrap bool
	// printed marks a column written once to the scrollback, where nothing can
	// be clicked.
	printed bool
	// resultBelow keeps a tool's result off its title row, for a transcript
	// that has printed the title before the result existed.
	resultBelow bool
}

func (m *model) renderEntryIn(item *entry, in column) entryLayout {
	if item.role == "reasoning" && !m.showThinking {
		return entryLayout{}
	}
	if item.layout.text != "" && item.cachedRevision == item.revision && item.cachedWidth == in.width && item.cachedExpanded == m.expandTools && item.cachedGraphics == m.graphicsEpoch {
		return item.layout
	}
	item.layout = m.layoutEntryIn(*item, in)
	item.cachedRevision, item.cachedWidth, item.cachedExpanded, item.cachedGraphics = item.revision, in.width, m.expandTools, m.graphicsEpoch
	return item.layout
}

func (m *model) renderEntryContent(item entry) string { return m.layoutEntry(item).text }

func (m *model) layoutEntry(item entry) entryLayout {
	return m.layoutEntryIn(item, column{width: m.mainWidth, markdown: m.markdown.at(m.mainWidth)})
}

// layoutEntryIn renders an entry together with the map from its rows back to
// the text they show, which is what selection and copying work from.
func (m *model) layoutEntryIn(item entry, in column) entryLayout {
	w := max(10, in.width-4)
	var out entryLayout
	add := func(line string, row rowMeta) {
		out.text += line + "\n"
		out.rows = append(out.rows, row)
	}
	// wrapped adds text hard-wrapped to width, marking the rows a wrap created.
	wrapped := func(text string, width int, row rowMeta, render func(string) string) {
		for _, line := range strings.Split(text, "\n") {
			parts := ansi.Hardwrap(line, width, false)
			if in.wordWrap {
				parts = ansi.Wrap(line, width, "")
			}
			for i, part := range strings.Split(parts, "\n") {
				row.cont = i > 0
				add(render(part), row)
			}
		}
	}
	// marked adds a message with its mark on the first row only, so that the rows
	// a wrap creates line up under the text.
	marked := func(mark string, style lipgloss.Style, text string) {
		lead := mark + " "
		wrapped(sanitize(text), w, rowMeta{gutter: 2}, func(s string) string {
			row := style.Render(lead + s)
			lead = "  "
			return row
		})
	}
	switch item.role {
	case "user":
		out.targets = []copyTarget{{"your message", item.text}}
		// Your words sit on a card with a coral edge, as wide as the column.
		card := func(s string) string {
			return coralStyle.Render("▌") + paint(panel, max(3, in.width-3), ink(panel, cream).Render(" ")+s)
		}
		add(card(ink(panel, coral).Bold(true).Render(item.heading("You"))), rowMeta{gutter: 2, skip: true})
		wrapped(sanitize(item.text), w, rowMeta{gutter: 2}, func(s string) string { return card(highlightImageTokens(s)) })
		rows := imageRows(item.images)
		if len(item.images) > 0 && len(item.imageIDs) == len(item.images) && m.thumbnailsShown() {
			add(card(""), rowMeta{gutter: 2, skip: true})
			rows = m.thumbnailRowsFor(item, w)
		}
		for _, row := range rows {
			add(card(ansi.Truncate(row, w, "…")), rowMeta{gutter: 2, skip: true})
		}
	case "reasoning":
		if !m.showThinking {
			return out
		}
		out.targets = []copyTarget{{"thinking", item.text}}
		add(mutedStyle.Bold(true).Render("◇ Thinking"), rowMeta{gutter: 2, skip: true})
		wrapped(sanitize(item.text), w, rowMeta{gutter: 2}, func(s string) string {
			return mutedStyle.Render("  " + s)
		})
	case "assistant":
		out.targets = []copyTarget{{"reply", item.text}}
		// A reply opens with the spark and runs on without a heading; a labelled
		// reply (an agent's, a memory's) keeps its heading row.
		lead := violetStyle.Render("✦ ")
		if item.noSpark {
			lead = "  "
		}
		if item.label != "" {
			add(lead+violetStyle.Bold(true).Render(strings.TrimPrefix(item.label, "✦ ")), rowMeta{gutter: 2, skip: true})
			lead = "  "
		}
		for i, segment := range splitFences(sanitize(item.text)) {
			if i > 0 || item.label != "" && strings.HasPrefix(strings.TrimLeft(segment.text, " \n"), "#") {
				// A heading straight under the label would read as part of it.
				add("", rowMeta{gutter: 2})
			}
			if segment.code {
				if lead != "  " {
					add(strings.TrimRight(lead, " "), rowMeta{gutter: 2, skip: true})
					lead = "  "
				}
				target := len(out.targets)
				label := "code"
				if segment.language != "" {
					label = segment.language + " code"
				}
				out.targets = append(out.targets, copyTarget{label, segment.text})
				width := max(codeGutter+8, in.width-3)
				hint := copyHint
				if in.printed {
					hint = ""
				}
				rows, continued := renderCodeCard(segment.text, segment.language, width, hint)
				for j, row := range rows {
					edge := j <= 1 || j == len(rows)-1 // header and padding rows
					add("  "+row, rowMeta{gutter: 2 + codeGutter, end: width + 1, cont: continued[j], skip: edge, target: target, code: true})
				}
				continue
			}
			text := segment.text
			if in.markdown != nil {
				if rendered, err := in.markdown.Render(text); err == nil {
					text = trimTrailing(strings.Trim(rendered, "\n"))
				}
			}
			lines := strings.Split(text, "\n")
			for len(lines) > 0 && strings.TrimSpace(ansi.Strip(lines[0])) == "" {
				lines = lines[1:]
			}
			for len(lines) > 0 && strings.TrimSpace(ansi.Strip(lines[len(lines)-1])) == "" {
				lines = lines[:len(lines)-1]
			}
			for _, line := range lines {
				add(lead+line, rowMeta{gutter: 2})
				lead = "  "
			}
		}
	case "tool":
		out.targets = toolTargets(item)
		if item.call != nil && item.call.Name == core.QuestionTool {
			if rendered, ok := m.renderQuestionEntry(item, w); ok {
				for _, line := range strings.Split(rendered, "\n") {
					add(line, rowMeta{gutter: 2})
				}
				break
			}
		}
		if item.call != nil && item.call.Name == core.ReviewTool {
			if rendered, ok := m.renderReviewEntry(item, w); ok {
				for _, line := range strings.Split(rendered, "\n") {
					add(line, rowMeta{gutter: 2})
				}
				break
			}
		}
		name, detail := "Tool", item.text
		if item.call != nil {
			name, detail = item.call.Name, toolSummary(*item.call)
			// The reflection step's changes read as its own, live and resumed.
			if item.text == "reflection" || strings.HasPrefix(item.call.ID, "reflection-") {
				detail = "reflection · " + detail
			}
			// A subagent's call in a transcript goes under the role's name.
			if item.label != "" && detail != "" {
				detail = item.label + " · " + detail
			} else if item.label != "" {
				detail = item.label
			}
		}
		if item.call != nil && item.call.Name == "Browser" && item.result != nil {
			// The result names the elements the call gave only refs for.
			headline, _, _ := strings.Cut(item.result.Content, "\n")
			if summary, ok := browserSummary(item.call.Arguments, headline); ok {
				detail = summary
			}
		}
		// The call is a pill: its name on a raised chip with the target beside
		// it, and a mark before it only while it runs or once it has failed.
		mark, pill := "  ", ink(pillBG, pillText)
		if !item.done {
			mark, pill = violetStyle.Render("◌ "), ink(pillBG, violet)
		}
		if item.result != nil && item.result.IsError {
			mark, pill = coralStyle.Render("! "), ink(pillBG, coral)
		}
		bar := "  "
		title := mark + pill.Render(" "+sanitize(name)+" ")
		if detail != "" {
			title += " " + mutedStyle.Render(ansi.Truncate(sanitize(detail), max(6, w-ansi.StringWidth(name)-6), "…"))
		}
		// The title row waits for the result: a short one joins it on its row.
		titleAdded := false
		flushTitle := func() {
			if !titleAdded {
				add(bar+title, rowMeta{gutter: 5})
				titleAdded = true
			}
		}
		if item.call != nil && item.call.Name == "Subagent" && m.expandTools {
			flushTitle()
			for i, task := range subagentTasks(*item.call) {
				target := len(out.targets)
				out.targets = append(out.targets, copyTarget{fmt.Sprintf("subagent task %d", i+1), task.prompt})
				heading := violetStyle.Bold(true).Render(fmt.Sprintf("Task %d", i+1))
				if task.role != "" {
					heading += mutedStyle.Render(" · ") + cyanStyle.Render(ansi.Truncate(sanitize(task.role), max(1, w-14), "…"))
				}
				add(bar+"  "+heading, rowMeta{gutter: 4, skip: true})
				for _, paragraph := range strings.Split(sanitize(task.prompt), "\n") {
					for lineIndex, line := range strings.Split(ansi.Hardwrap(paragraph, max(1, w-6), false), "\n") {
						add(bar+"    "+creamStyle.Render(line), rowMeta{gutter: 6, cont: lineIndex > 0, target: target})
					}
				}
			}
		}
		if item.call != nil && argumentsShown(item.call.Name) && m.expandTools {
			flushTitle()
			// The arguments in full: the title row shows the message or issue
			// text only as far as the width allows.
			for _, row := range argumentRows(callArguments(item.call.Arguments), w-4) {
				add(bar+"  "+row.text, rowMeta{gutter: 4 + row.indent, cont: row.cont, target: 0})
			}
		}
		if item.result != nil {
			result, markers := splitImageMarkers(strings.TrimSpace(sanitize(item.result.Content)))
			screen, isScreen := terminalScreen{}, false
			if item.call != nil && item.call.Name == "Terminal" && !item.result.IsError {
				screen, isScreen = parseTerminalScreen(result)
			}
			page, isPage := browserPage{}, false
			var other []string
			isOther := false
			if item.call != nil && item.call.Name == "Browser" && !item.result.IsError {
				if page, isPage = parseBrowserPage(result); !isPage {
					var call browserCall
					json.Unmarshal(item.call.Arguments, &call)
					other, isOther = browserRows(call.Action, result, w-2, m.expandTools)
				}
			}
			first, _, _ := strings.Cut(result, "\n")
			merged := false
			if plain := !isOther && !isPage && !isScreen && !m.expandTools && !in.resultBelow && result != ""; plain && ansi.StringWidth(title)+5+ansi.StringWidth(first) <= w {
				style := faintStyle
				if item.result.IsError {
					style = coralStyle
				}
				add(bar+title+style.Render("  ·  "+first), rowMeta{gutter: 5})
				titleAdded, merged = true, true
			}
			flushTitle()
			if isOther {
				for _, row := range other {
					add(bar+"  "+row, rowMeta{gutter: 4, target: 1})
				}
			} else if isPage && m.expandTools {
				if page.headline != "" {
					add(bar+"  "+mutedStyle.Render(ansi.Truncate(page.headline, w-2, "…")), rowMeta{gutter: 4, skip: true})
				}
				for _, line := range page.details {
					add(bar+"  "+creamStyle.Render(ansi.Truncate(line, w-2, "…")), rowMeta{gutter: 4, target: 1})
				}
				// Notes are read whole: why a request was blocked is in its tail.
				for _, note := range page.notes {
					for i, line := range strings.Split(ansi.Wrap(note, max(8, w-4), ""), "\n") {
						lead := map[bool]string{true: "· ", false: "  "}[i == 0]
						add(bar+"  "+noteStyle(note).Render(lead+line), rowMeta{gutter: 6, skip: true})
					}
				}
				for _, err := range page.errs {
					for i, line := range strings.Split(ansi.Wrap(err, max(8, w-4), ""), "\n") {
						lead := map[bool]string{true: "! ", false: "  "}[i == 0]
						add(bar+"  "+coralStyle.Render(lead+line), rowMeta{gutter: 6, skip: true})
					}
				}
				rows, edge := page.card(w - 2)
				for i, row := range rows {
					add(bar+"  "+row, rowMeta{gutter: 4 + codeGutter, end: 4 + ansi.StringWidth(row), skip: edge[i], target: 1})
				}
				if page.shot != "" {
					add(bar+"  "+mutedStyle.Italic(true).Render(ansi.Truncate(page.shot, w-2, "…")), rowMeta{gutter: 4, skip: true})
				}
			} else if isPage {
				add(bar+"  "+page.summary(w-2), rowMeta{gutter: 4, target: 1})
			} else if isScreen && m.expandTools {
				for _, note := range screen.notes {
					add(bar+"  "+mutedStyle.Italic(true).Render(ansi.Truncate(note, w-2, "…")), rowMeta{gutter: 4, skip: true})
				}
				rows, edge := screen.card(w - 2)
				for i, row := range rows {
					add(bar+"  "+row, rowMeta{gutter: 4 + codeGutter, end: 4 + ansi.StringWidth(row), skip: edge[i], target: 1})
				}
			} else if isScreen {
				add(bar+"  "+ansi.Truncate(screen.summary(), w-2, "…"), rowMeta{gutter: 4, target: 1})
			} else if m.expandTools {
				parts := strings.Split(ansi.Hardwrap(result, w-2, false), "\n")
				if len(parts) > 100 {
					parts = append(parts[:100], "… output truncated in the display")
				}
				for _, line := range parts {
					add(bar+mutedStyle.Render("  "+line), rowMeta{gutter: 4, target: 1})
				}
			} else if result != "" && !merged {
				style := faintStyle
				if item.result.IsError {
					style = coralStyle
				}
				add(bar+style.Render("  "+ansi.Truncate(first, w-2, "…")), rowMeta{gutter: 4, target: 1})
			}
			// Images sit under the output as in a user's message: drawn when the
			// terminal can, captioned otherwise or when only a marker was saved.
			if len(item.images) > 0 && len(item.imageIDs) == len(item.images) && m.thumbnailsShown() {
				add(bar, rowMeta{gutter: 2, skip: true})
				for _, row := range m.thumbnailRowsFor(item, m.thumbnailColumns()) {
					add(bar+"  "+ansi.Truncate(row, m.thumbnailColumns(), "…"), rowMeta{gutter: 4, skip: true})
				}
			} else {
				for _, img := range item.images {
					markers = append(markers, img.Info().String())
				}
			}
			for _, description := range markers {
				add(bar+"  "+ansi.Truncate(imageCaption(description), w-2, "…"), rowMeta{gutter: 4, skip: true})
			}
		}
		flushTitle()
	case "warning":
		out.targets = []copyTarget{{"warning", item.text}}
		marked("!", coralStyle, item.text)
	default:
		out.targets = []copyTarget{{"note", item.text}}
		marked("·", mutedStyle.Italic(true), item.text)
	}
	out.text = strings.TrimSuffix(out.text, "\n")
	return out
}

// toolTargets are what a double-click copies: the call's main argument (the
// command, path, or pattern) on its title row, and the output below it.
func toolTargets(item entry) []copyTarget {
	label, text := "tool call", ""
	if item.call != nil {
		label, text = "arguments", string(item.call.Arguments)
		keys := []string{"command", "path", "file_path", "pattern", "query", "directory", "url", "script"}
		if item.call.Name == "Memory" {
			// The lesson is the note; a Linear issue's description stays
			// under its raw arguments, as before.
			keys = []string{"description"}
		}
		var args map[string]json.RawMessage
		if json.Unmarshal(item.call.Arguments, &args) == nil {
			for _, key := range keys {
				var value string
				if json.Unmarshal(args[key], &value) == nil {
					label, text = key, value
					break
				}
			}
		}
	}
	kind, output := "output", ""
	if item.result != nil {
		output = item.result.Content
		if item.call != nil && item.call.Name == "Terminal" {
			if screen, ok := parseTerminalScreen(output); ok {
				kind, output = "screen", screen.text()
			}
		}
		if item.call != nil && item.call.Name == "Browser" && !item.result.IsError {
			if page, ok := parseBrowserPage(output); ok && len(page.lines) > 0 {
				kind, output = "snapshot", page.text()
			}
		}
	}
	return []copyTarget{{label, text}, {kind, output}}
}

func toolSummary(call core.ToolCall) string {
	if call.Name == "Subagent" {
		if tasks := subagentTasks(call); len(tasks) > 0 {
			label := "tasks"
			if len(tasks) == 1 {
				label = "task"
			}
			summary := fmt.Sprintf("%d %s", len(tasks), label)
			if roles := subagentRoles(tasks); roles != "" {
				summary += " · " + roles
			}
			return summary
		}
	}
	if call.Name == "Terminal" {
		if summary, ok := terminalSummary(call.Arguments); ok {
			return summary
		}
	}
	if call.Name == "Browser" {
		if summary, ok := browserSummary(call.Arguments, ""); ok {
			return summary
		}
	}
	if call.Name == "Slack" {
		if summary, ok := slackSummary(call.Arguments); ok {
			return summary
		}
	}
	if call.Name == "GitHub" || call.Name == "Bugsnag" {
		if summary, ok := apiRequestSummary(call.Arguments); ok {
			return summary
		}
	}
	if call.Name == "Linear" {
		if summary, ok := linearSummary(call.Arguments); ok {
			return summary
		}
	}
	if call.Name == "Mail" {
		if summary, ok := mailSummary(call.Arguments); ok {
			return summary
		}
	}
	if call.Name == "Memory" {
		if summary, ok := memorySummary(call.Arguments); ok {
			return summary
		}
	}
	if call.Name == "Skill" {
		// The skill, and the file in its folder when it is not SKILL.md.
		var skill struct{ Name, File string }
		if json.Unmarshal(call.Arguments, &skill) == nil && skill.Name != "" {
			if skill.File == "" || skill.File == "SKILL.md" {
				return skill.Name
			}
			return skill.Name + "/" + skill.File
		}
	}
	var args map[string]json.RawMessage
	if json.Unmarshal(call.Arguments, &args) != nil {
		return string(call.Arguments)
	}
	for _, key := range []string{"path", "file_path", "command", "pattern", "directory", "query", "url"} {
		if raw, ok := args[key]; ok {
			var value string
			if json.Unmarshal(raw, &value) == nil {
				if key == "path" {
					value = scratchPath(value)
				}
				value = strings.ReplaceAll(value, "\n", " ↵ ")
				return value
			}
		}
	}
	if edits, ok := args["edits"]; ok {
		var values []any
		if json.Unmarshal(edits, &values) == nil {
			return fmt.Sprintf("%d exact edits", len(values))
		}
	}
	// A tool with actions, whatever else it takes, at least says which one.
	var action string
	if json.Unmarshal(args["action"], &action) == nil {
		return action
	}
	return ""
}

// argumentRow is one display line of a call's arguments: the key in violet and
// the value in cream, a long value wrapped onto continuation lines aligned under
// its first.
type argumentRow struct {
	text   string
	indent int
	cont   bool
}

func argumentRows(arguments []argument, width int) []argumentRow {
	width = max(8, width)
	keyWidth := 0
	for _, a := range arguments {
		keyWidth = max(keyWidth, ansi.StringWidth(sanitize(a.key)))
	}
	keyWidth = min(keyWidth, 16)
	var rows []argumentRow
	for _, a := range arguments {
		key := ansi.Truncate(sanitize(a.key), keyWidth, "…")
		label := violetStyle.Render(key) + strings.Repeat(" ", keyWidth-ansi.StringWidth(key)) + "  "
		indent := keyWidth + 2
		first := true
		for _, paragraph := range strings.Split(sanitize(a.value), "\n") {
			for _, line := range strings.Split(ansi.Wrap(paragraph, max(1, width-indent), ""), "\n") {
				lead := strings.Repeat(" ", indent)
				if first {
					lead = label
				}
				rows = append(rows, argumentRow{text: lead + creamStyle.Render(line), indent: indent, cont: !first})
				first = false
			}
		}
	}
	return rows
}

// subagentTask is one task of a Subagent call: the role it names and its prompt.
type subagentTask struct{ role, prompt string }

// subagentTasks returns the delegated tasks carried by a Subagent call. A
// session from before roles holds bare prompts, which have no role.
func subagentTasks(call core.ToolCall) []subagentTask {
	var args struct {
		Tasks []json.RawMessage `json:"tasks"`
	}
	if json.Unmarshal(call.Arguments, &args) != nil {
		return nil
	}
	tasks := make([]subagentTask, 0, len(args.Tasks))
	for _, raw := range args.Tasks {
		var prompt string
		var task struct{ Role, Task string }
		if json.Unmarshal(raw, &prompt) == nil {
			tasks = append(tasks, subagentTask{prompt: prompt})
		} else if json.Unmarshal(raw, &task) == nil {
			// The role is the model's text until the tool checks it; it must stay one line.
			tasks = append(tasks, subagentTask{strings.Join(strings.Fields(task.Role), " "), task.Task})
		} else {
			return nil
		}
	}
	return tasks
}

// subagentRoles names the roles tasks were given, in order, each once, with a
// count when it was given more than one task: "scout ×2, reviewer".
func subagentRoles(tasks []subagentTask) string {
	var roles []string
	counts := map[string]int{}
	for _, task := range tasks {
		if task.role == "" {
			continue
		}
		if counts[task.role] == 0 {
			roles = append(roles, task.role)
		}
		counts[task.role]++
	}
	for i, role := range roles {
		if counts[role] > 1 {
			roles[i] = fmt.Sprintf("%s ×%d", role, counts[role])
		}
	}
	return strings.Join(roles, ", ")
}

// scratchPath shows a file in the session's scratch directory, which ViewImage
// may be given as a long temporary path, the way the agent knows it:
// $JULIA_SCRATCH/shot.png.
func scratchPath(path string) string {
	if !filepath.IsAbs(path) {
		return path
	}
	_, session, ok := strings.Cut(filepath.ToSlash(path), "/julia-scratch/")
	if _, inside, ok2 := strings.Cut(session, "/"); ok && ok2 && inside != "" {
		return "$JULIA_SCRATCH/" + inside
	}
	return path
}

func (m *model) View() tea.View {
	if m.width == 0 {
		return tea.NewView("")
	}
	contentWidth := m.width - 2*m.margin()
	// The masthead: the brand's wave as a ribbon across the top, the name set
	// in the same gradient, and the workspace and mode at the right.
	header := ribbon("JULIA")
	right := violetStyle.Render("● " + sanitize(m.cfg.Mode))
	if m.cfg.Demo {
		right = violetStyle.Render("◈ PREVIEW")
	}
	if m.width >= 65 && m.cfg.Session != nil && m.cfg.Session.Directory != "" {
		right = mutedStyle.Render(ansi.Truncate(sanitize(filepath.Base(filepath.Clean(m.cfg.Session.Directory))), 24, "…")+"  ·  ") + right
	}
	header += strings.Repeat(" ", max(1, contentWidth-ansi.StringWidth(header)-ansi.StringWidth(right))) + right
	header = ribbon(strings.Repeat("▀", contentWidth)) + "\n" + ansi.Truncate(header, contentWidth, "") + "\n"
	body := m.withActivity(m.highlightSelection(m.viewport.View()))
	if m.picker != nil && m.pending == nil {
		body = m.pickerView()
	} else if m.question != nil && m.pending == nil {
		body = m.questionBody()
	} else if m.fleetView != nil && m.pending == nil {
		body = m.fleetBody()
	} else if len(m.entries) == 0 && m.pending == nil {
		body = m.welcome()
	}
	body = lipgloss.NewStyle().Width(m.mainWidth).Height(m.bodyHeight).Render(body)
	if m.menuOpen() {
		// The command menu rises from the composer over the conversation.
		body = m.overlayMenu(body, m.mainWidth, m.bodyHeight)
	}
	body += "\n" + m.bottomView()
	if m.hasSidebar() {
		// The sidebar runs from the masthead to the bottom edge, beside the composer.
		body = lipgloss.JoinHorizontal(lipgloss.Top, lipgloss.NewStyle().Width(m.mainWidth).Render(body), "  ", m.sidebar())
	}
	view := header + "\n" + body
	view = lipgloss.NewStyle().Padding(0, m.margin()).Background(background).Foreground(cream).Width(m.width).Render(view)
	// Clamp every final row, including pathological filenames and narrow resizes.
	lines := strings.Split(view, "\n")
	for i := range lines {
		lines[i] = ansi.Truncate(lines[i], m.width, "")
	}
	if len(lines) > m.height {
		lines = lines[:m.height]
	}
	v := tea.NewView(strings.Join(lines, "\n"))
	v.AltScreen = true
	v.BackgroundColor = background
	v.ForegroundColor = cream
	v.WindowTitle = "JULIA"
	v.MouseMode = tea.MouseModeCellMotion
	v.ReportFocus = true // for notify, which speaks only while the terminal is in the background
	return v
}

// cardBorder frames a filled card flush with its fill. One-eighth blocks sit on
// the cell's edge, where a box-drawing stroke sits in its middle and leaves a
// gap inside the frame or a halo outside it, whichever background its cell
// has. The top and bottom lines are drawn on the page, the sides on the fill.
var cardBorder = lipgloss.Border{Top: "▁", Bottom: "▔", Left: "▏", Right: "▕", TopLeft: "▁", TopRight: "▁", BottomLeft: "▔", BottomRight: "▔"}

func (m *model) composerStyle() lipgloss.Style {
	vertical, horizontal := 0, 1
	if m.width >= 60 {
		horizontal = 2
	}
	if m.height >= 36 {
		vertical = 1
	}
	stroke := border
	if m.pending != nil {
		stroke = coral
	}
	return lipgloss.NewStyle().Width(m.mainWidth).Padding(vertical, horizontal).
		Background(panel).Border(cardBorder).BorderForeground(stroke).BorderTopBackground(background).BorderBottomBackground(background).BorderLeftBackground(panel).BorderRightBackground(panel)
}

func (m *model) bottomView() string {
	rows, _ := m.bottomLayout()
	return strings.Join(rows, "\n")
}

// bottomLayout is the rows below the conversation, and the index among them
// of the queue strip's first row, -1 when nothing is queued.
func (m *model) bottomLayout() ([]string, int) {
	style := m.composerStyle()
	innerWidth := max(1, m.mainWidth-style.GetHorizontalFrameSize())
	// Each part of the title sets the panel background itself: a style reset
	// between two parts would otherwise let the terminal background through.
	label, note := ink(panel, violet).Bold(true), ink(panel, muted)
	composerTitle := label.Render("MESSAGE")
	composerInput := m.input.View()
	if m.picker != nil {
		composerTitle = label.Render("SEARCH") + note.Render(" · your message draft is kept")
		if m.mainWidth < 44 {
			composerTitle = label.Render("SEARCH") + note.Render(" · draft kept")
		}
		composerInput = m.picker.search.View() + "\n"
	}
	if m.busy {
		composerTitle = label.Render("MESSAGE") + note.Render(" · Enter queues it until JULIA settles")
		if m.mainWidth < 52 {
			composerTitle = label.Render("MESSAGE") + note.Render(" · Enter queues")
		}
	}
	if m.fleetView != nil {
		composerTitle = label.Render("DRAFT") + note.Render(" · kept while you inspect the fleet")
		if m.mainWidth < 48 {
			composerTitle = label.Render("DRAFT") + note.Render(" · fleet open")
		}
	}
	if m.question != nil {
		composerTitle = label.Render("DRAFT") + note.Render(" · kept while you answer")
		if m.question.review != nil {
			composerTitle = label.Render("DRAFT") + note.Render(" · kept while you review")
		}
	}
	if m.pending != nil {
		composerTitle = ink(panel, coral).Render("APPROVAL · y / Enter")
	}
	if m.picker == nil && m.pending == nil {
		composerTitle += m.attachmentChips(max(0, innerWidth-ansi.StringWidth(composerTitle)))
	}
	composer := style.Render(ansi.Truncate(composerTitle, innerWidth, "…") + "\n" + composerInput)
	if m.pending == nil {
		// The top edge carries the brand's ribbon, as the masthead does; an
		// approval keeps its coral frame whole.
		if lines := strings.Split(composer, "\n"); len(lines) > 0 {
			lines[0] = ribbonOn(background, ansi.Strip(lines[0]))
			composer = strings.Join(lines, "\n")
		}
	}
	var rows []string
	if m.height >= 28 {
		rows = append(rows, "")
	}
	rows = append(rows, m.statusLine())
	if m.height >= 28 {
		rows = append(rows, "")
	}
	queueAt := -1
	if strip := m.queueStrip(); len(strip) > 0 {
		queueAt = len(rows)
		rows = append(rows, strip...)
	}
	rows = append(rows, composer)
	if m.height >= 36 {
		rows = append(rows, "")
	}
	rows = append(rows, m.footer(), "")
	return rows, queueAt
}

// heroGap is the number of cells between the hero's letters.
const heroGap = 2

// hairline is a faint rule, width cells wide, with a small amber ornament at
// its centre.
func hairline(width int, ornament string) string {
	side := (width - 5) / 2
	rule := faintStyle.Render(strings.Repeat("─", side))
	return rule + "  " + lipgloss.NewStyle().Foreground(amber).Render(ornament) + "  " + rule
}

// centred is the top row that puts a block of size block in the middle of a
// span of size total. Spare rows go below before above, so the block sits a
// hair high, where the eye expects a title, and the row under the header
// rule stays clear whenever there is a row to spare.
func centred(total, block int) int {
	free := max(0, total-block)
	if free == 1 {
		return 1
	}
	return free / 2
}

// block pads every row to one odd width, centring each, so that
// lipgloss.Place moves them as one rectangle. The width is odd so that the
// crest, the label and the hairline, which are odd cells wide, share one axis
// cell exactly, and the hero's letters and shadow together, also odd, sit on
// it too; even rows lean half a cell left, as they must.
func block(rows []string) []string {
	width := 0
	for _, row := range rows {
		width = max(width, ansi.StringWidth(row))
	}
	width |= 1
	out := make([]string, len(rows))
	for i, row := range rows {
		pad := width - ansi.StringWidth(row)
		out[i] = strings.Repeat(" ", pad/2) + row + strings.Repeat(" ", pad-pad/2)
	}
	return out
}

// heroMark is the wordmark: the hero rows each led by one space so that the
// letters, not the shadow's extra column, are what the axis balances.
func heroMark() []string {
	var rows []string
	for _, row := range heroLettering(heroGap) {
		rows = append(rows, " "+row)
	}
	return rows
}

// tagline is the welcome's caption: what the name stands for, give or take.
const tagline = "Just Using Language Intelligently, Apparently"

// taglineRows sets the tagline on one row when it fits in width, else on two,
// broken after "Language" so each half still reads as a phrase.
func taglineRows(width int) []string {
	lines := []string{tagline}
	if ansi.StringWidth(tagline) > width {
		lines = []string{"Just Using Language", "Intelligently, Apparently"}
	}

	rows := make([]string, len(lines))
	for i, line := range lines {
		rows[i] = mutedStyle.Render(ansi.Truncate(line, width, "…"))
	}
	return rows
}

// jewelLockup is the small-terminal mark: the quadrant lettering with the
// tagline under it.
func jewelLockup(width int) []string {
	return append(append(logoLettering(), ""), taglineRows(width)...)
}

// welcome is the body before the first message: the wordmark with its
// tagline. With less room the quadrant jewel, and at the last resort two lines of text. The block is
// centred, and never starts on the row under the masthead.
func (m *model) welcome() string {
	w, h := m.mainWidth, m.bodyHeight
	var rows []string
	switch {
	case h >= 7 && w >= heroWidth(heroGap)+6:
		rows = welcomePage(w)
	case w >= 24 && h >= len(jewelLockup(w)):
		rows = jewelLockup(w)
	default:
		rows = append([]string{ribbon("J U L I A")}, taglineRows(w)...)
		rows = rows[:min(len(rows), max(h, 1))]
	}
	rows = block(rows)
	content := strings.Repeat("\n", centred(h, len(rows))) + strings.Join(rows, "\n")
	return lipgloss.Place(w, h, lipgloss.Center, lipgloss.Top, content)
}

// welcomePage is the full welcome: the wordmark with the tagline set directly
// under the shadow's feet.
func welcomePage(w int) []string {
	return append(heroMark(), taglineRows(w)...)
}

// showsReasoning reports whether provider takes a reasoning effort: a CLI
// provider, or a named provider that supports one.
func showsReasoning(provider string, named map[string]bool) bool {
	return config.IsCLIProvider(provider) || named[provider]
}

func reasoningDisplay(provider, effort string, named map[string]bool) string {
	if effort != "" {
		return effort
	}
	if _, ok := named[provider]; ok {
		return "provider default"
	}
	return "inherited"
}

// overSidebar reports whether a screen cell falls inside the sidebar column.
func (m *model) overSidebar(x, y int) bool {
	return m.hasSidebar() && x >= m.margin()+m.mainWidth+2 && y >= 3 && y < 3+m.sidebarHeight()
}

// sidebarHeight is every row below the header's three.
func (m *model) sidebarHeight() int { return max(1, m.height-3) }

// scrollSidebar moves the sidebar by delta rows; sidebar clamps the offset.
func (m *model) scrollSidebar(delta int) {
	m.sidebarOffset = max(0, m.sidebarOffset+delta)
	m.sidebar()
}

func (m *model) sidebar() string {
	const width = 23 // text cells on a row, inside the card and below it
	card, page := paletteOn(panel), paletteOn(background)
	fit := func(style lipgloss.Style, s string) string {
		return style.Render(ansi.Truncate(sanitize(s), width, "…"))
	}
	provider, modelName, reasoning, directory, id := "—", "default", "", "", "new conversation"
	if m.cfg.Session != nil {
		provider, modelName, directory, id = m.cfg.Session.Provider, m.cfg.Session.Model, m.cfg.Session.Directory, m.cfg.Session.ID
		reasoning = m.cfg.Session.Reasoning
	}
	if modelName == "" {
		modelName = "provider default"
	}
	reasoning = reasoningDisplay(provider, reasoning, m.cfg.NamedProviders)
	if directory != "" {
		directory = filepath.Clean(directory)
	}
	connection := []string{card.label("MODEL"), fit(card.text, modelName), fit(card.cyan, provider)}
	if m.cfg.Profile != "" {
		connection = append(connection, fit(card.muted, "profile: "+m.cfg.Profile))
	}
	if showsReasoning(provider, m.cfg.NamedProviders) {
		if !config.IsCLIProvider(provider) && reasoning == "provider default" {
			connection = append(connection, card.muted.Render("reasoning:"), card.muted.Render("provider default"))
		} else {
			connection = append(connection, fit(card.muted, "reasoning: "+reasoning))
		}
		if config.IsCLIProvider(provider) {
			connection = append(connection, m.weeklyUsageLine(card))
			// Only Claude Code reports a 5-hour window.
			if provider == "claude" {
				connection = append(connection, m.fiveHourUsageLine(card))
			}
		}
	}
	// The per-call figures describe the last model call only; the session totals
	// and the context estimate have their own sections.
	activity := []string{card.label("ACTIVITY"), fit(card.text, fmt.Sprintf("%d tool calls", m.toolCount)), card.muted.Render("last call"), fit(card.muted, fmt.Sprintf("%s in · %s out", core.CompactTokens(m.inputTokens), core.CompactTokens(m.outputTokens)))}
	sections := [][]string{m.contextSection(card, width), m.tokenSection(card, width), activity, connection}
	if n := len(m.fleet); n > 0 {
		count := fmt.Sprintf("%d subagents", n)
		if n == 1 {
			count = "1 subagent"
		}
		section := []string{card.label("FLEET"), fit(card.text, count)}
		// The states on one row when they fit, else one per row.
		if summary := m.fleetSummary(); ansi.StringWidth(summary) <= width {
			section = append(section, card.muted.Render(summary))
		} else {
			for _, part := range strings.Split(summary, " · ") {
				section = append(section, fit(card.muted, part))
			}
		}
		sections = append(sections, append(section, card.faint.Render("ctrl+f inspects")))
	}
	// What changes as the session runs sits on one card, framed as cardBorder
	// frames the composer: flush with the fill.
	rows := []string{"", page.faint.Render(strings.Repeat("▁", width+4))}
	for i, section := range sections {
		if i > 0 {
			section = append([]string{""}, section...)
		}
		for _, row := range section {
			rows = append(rows, card.faint.Render("▏")+paint(panel, width+2, ink(panel, cream).Render(" ")+row)+card.faint.Render("▕"))
		}
	}
	rows = append(rows, page.faint.Render(strings.Repeat("▔", width+4)))
	// Below it, what rarely changes, set directly on the page.
	session := []string{page.label("SESSION"), fit(page.text, id)}
	if m.cfg.Version != "" {
		session = append(session, fit(page.faint, "JULIA v"+strings.Join(strings.Fields(m.cfg.Version), " ")))
	}
	below := [][]string{
		{page.label("WORKSPACE"), fit(page.text, filepath.Base(directory)), fit(page.muted, directory)},
		{page.label("PERMISSIONS"), fit(page.green, "● "+m.cfg.Mode), fit(page.muted, permissionCaption(m.cfg.Mode))},
		session,
	}
	if tools := toolRows(m.cfg.ToolDefinitions, width); len(tools) > 0 {
		essentials := []string{page.label("THE ESSENTIALS")}
		for _, row := range tools {
			essentials = append(essentials, page.muted.Render(row))
		}
		below = append(below, essentials)
	}
	for _, section := range below {
		rows = append(rows, "")
		for _, row := range section {
			rows = append(rows, " "+row)
		}
	}
	// The card and every section always render in full; a sidebar taller than
	// the body scrolls on its own under the mouse wheel.
	height := m.sidebarHeight()
	m.sidebarOffset = max(0, min(m.sidebarOffset, len(rows)-height))
	rows = rows[m.sidebarOffset:]
	if len(rows) > height {
		rows = rows[:height]
	}
	return lipgloss.NewStyle().Width(width + 4).Height(height).Render(strings.Join(rows, "\n"))
}

// Pack the registered names into rows; no tool names belong in the view.
func toolRows(definitions []core.ToolDefinition, width int) []string {
	var rows []string
	row := ""
	for _, definition := range definitions {
		name := ansi.Truncate(strings.Join(strings.Fields(sanitize(definition.Name)), " "), width, "…")
		if name == "" {
			continue
		}
		if row == "" {
			row = name
			continue
		}
		if ansi.StringWidth(row+"  "+name) > width {
			rows = append(rows, row)
			row = name
		} else {
			row += "  " + name
		}
	}
	if row != "" {
		rows = append(rows, row)
	}
	return rows
}

func permissionCaption(mode string) string {
	switch mode {
	case "auto", "auto-approve", "autoapprove":
		return "Automatic review on"
	case "read-only", "read_only", "readonly":
		return "Read-only access"
	case "ask":
		return "Ask before changes"
	case "yolo":
		return "Yolo: no sandbox"
	default:
		return "Enforced per action" // must fit the 23-column sidebar on one row
	}
}

// activityLine is the animated stream, what the task is doing, and how long
// it has run, fitted to width with the elapsed time kept visible.
func (m *model) activityLine(width int) string {
	streamWidth := min(activityWidth, max(3, width/5))
	stream := ansi.Truncate(m.spinner.View(), streamWidth, "")
	duration := faintStyle.Render("·  " + elapsed(m.now.Sub(m.started)))
	labelWidth := max(0, width-streamWidth-ansi.StringWidth(duration)-4)
	label := mutedStyle.Render(ansi.Truncate(sanitize(m.status), labelWidth, "…"))
	return stream + "  " + label + "  " + duration
}

// withActivity draws the running task's activity on the row reserved under
// the latest entry. Scrolled back into history, it sticks to the body's last
// row so the task stays visible.
func (m *model) withActivity(body string) string {
	if !m.busy || !m.activityRow || !m.conversationShown() || len(m.lines) == 0 {
		return body
	}
	lines := strings.Split(body, "\n")
	row := min(len(m.lines)-1-m.viewport.YOffset(), len(lines)-1)
	if row < 0 {
		return body
	}
	lines[row] = ansi.Truncate(" "+m.activityLine(m.mainWidth-1), m.mainWidth, "")
	return strings.Join(lines, "\n")
}

func (m *model) statusLine() string {
	status := lipgloss.NewStyle().Foreground(green).Render("●") + mutedStyle.Render(" Ready")
	if m.busy && m.conversationShown() {
		status = "" // the activity is drawn under the latest entry
	} else if m.busy {
		status = m.activityLine(m.mainWidth)
	} else if m.followup != nil {
		// Work after the turn, such as its reflection: the interface is
		// free, and says what is still under way.
		status = violetStyle.Render("◌") + mutedStyle.Render(" "+sanitize(m.followup.Status))
	} else if m.lastDuration > 0 {
		status += faintStyle.Render(" · " + elapsed(m.lastDuration))
	}
	if !m.hasSidebar() && m.cfg.Session != nil {
		model := m.cfg.Session.Model
		if model == "" {
			model = "provider default"
		}
		settings := model
		if showsReasoning(m.cfg.Session.Provider, m.cfg.NamedProviders) {
			settings += " · " + reasoningDisplay(m.cfg.Session.Provider, m.cfg.Session.Reasoning, m.cfg.NamedProviders)
		}
		if remaining := m.mainWidth - ansi.StringWidth(status); remaining > 3 {
			suffix := violetStyle.Render(sanitize(settings))
			if status != "" {
				suffix = mutedStyle.Render(" · ") + suffix
			}
			status += ansi.Truncate(suffix, remaining, "…")
		}
	}
	if m.pending != nil {
		status = coralStyle.Render("◆ Waiting for your approval")
	}
	if m.question != nil {
		status = violetStyle.Render("◆ Waiting for your answer")
		if r := m.question.review; r != nil {
			status = violetStyle.Render("◆ Waiting for your decisions") + mutedStyle.Render(fmt.Sprintf(" · %d of %d findings decided", r.decidedCount(), len(r.triage)))
		}
	}
	if m.notice != "" {
		status = mutedStyle.Render(sanitize(m.notice))
	}
	if m.fleetView != nil && m.fleetView.below > 0 && m.pending == nil && m.question == nil && m.notice == "" {
		status = violetStyle.Render("↑ Reading the agent's work") + mutedStyle.Render(" · Ctrl+End to follow it")
	}
	if !m.viewport.AtBottom() && len(m.entries) > 0 && m.pending == nil && m.question == nil && m.picker == nil && m.fleetView == nil && m.notice == "" {
		status = violetStyle.Render("↑ Reading history") + mutedStyle.Render(" · Ctrl+End to follow output")
	}
	return ansi.Truncate(status, m.mainWidth, "…")
}

func (m *model) footer() string {
	if m.question != nil && m.pending == nil {
		if m.width < 36 {
			return mutedStyle.Render("ctrl+c stops")
		}
		verb, where := "answering", "answer above"
		if m.question.review != nil {
			verb, where = "reviewing", "decide above"
		}
		if m.width < 62 {
			return mutedStyle.Render(where + " · ctrl+c stops")
		}
		if m.width < 78 {
			return violetStyle.Render(verb) + mutedStyle.Render("  ctrl+c stops the task")
		}
		return violetStyle.Render(verb) + mutedStyle.Render("  ctrl+c stops the task  mouse wheel scrolls the conversation")
	}
	if m.pending != nil {
		if m.width < 40 {
			return mutedStyle.Render("y/enter allow · others deny")
		}
		if m.width < 62 {
			return mutedStyle.Render("y/enter allow · any other key denies")
		}
		return coralStyle.Render("y / enter "+strings.ToLower(allowLabel(m.pending.Approval))) + mutedStyle.Render("  any other key denies  mouse wheel scrolls")
	}
	if m.fleetView != nil {
		return m.fleetFooter()
	}
	if m.picker != nil {
		if m.width < 40 {
			return mutedStyle.Render("↑↓ select · enter · esc back")
		}
		if m.width < 62 {
			return mutedStyle.Render("↑↓ choose · enter apply · esc back")
		}
		text := "↑↓ choose  enter apply  esc back"
		if m.picker.kind == "reasoning" {
			text += "  tab models"
		}
		return mutedStyle.Render(text)
	}
	send := "send"
	if m.busy {
		send = "queue"
	}
	if m.width < 62 {
		return coralStyle.Render("enter") + mutedStyle.Render(" "+send+" · ") + violetStyle.Render("ctrl+p") + mutedStyle.Render(" models · ") + violetStyle.Render("/help")
	}
	// Shortcuts join in order of usefulness while they fit beside the sidebar,
	// then show in their usual order.
	extras, priority := []string{"ctrl+t thinking", "ctrl+o tools", "ctrl+p models", "drag copy", "ctrl+v image"}, []int{2, 0, 1, 3, 4}
	if len(m.fleet) > 0 {
		// With agents at work, the fleet is the shortcut worth knowing first.
		extras, priority = []string{"ctrl+f fleet", "ctrl+t thinking", "ctrl+o tools", "ctrl+p models", "drag copy", "ctrl+v image"}, []int{0, 3, 1, 2, 4, 5}
	}
	shown := make([]bool, len(extras))
	used := ansi.StringWidth("enter " + send + "  alt+enter newline  esc interrupt  /help")
	for _, i := range priority {
		if used+2+len(extras[i]) <= m.mainWidth {
			used, shown[i] = used+2+len(extras[i]), true
		}
	}
	left := coralStyle.Render("enter ") + mutedStyle.Render(send) + violetStyle.Render("  alt+enter ") + mutedStyle.Render("newline") + violetStyle.Render("  esc ") + mutedStyle.Render("interrupt") + violetStyle.Render("  /help")
	for i, extra := range extras {
		if shown[i] {
			key, name, _ := strings.Cut(extra, " ")
			left += violetStyle.Render("  "+key+" ") + mutedStyle.Render(name)
		}
	}
	return left
}

func (m *model) approvalView() string {
	request := m.pending.Approval
	// The panel is mainWidth-4 wide with its border and padding; text wrapped
	// any wider would be wrapped again, mid-word.
	w := max(12, m.mainWidth-8)
	var pretty bytes.Buffer
	raw := request.Call.Arguments
	if request.Call.Name == "Browser" {
		raw = withoutPage(raw) // the Page section shows where
	}
	arguments := string(raw)
	if json.Indent(&pretty, raw, "", "  ") == nil {
		arguments = pretty.String()
	}
	text := coralStyle.Bold(true).Render("PERMISSION REQUEST")
	text += "\n" + creamStyle.Bold(true).Render(sanitize(request.Call.Name))
	switch request.Call.Name {
	case "Browser", "Memory", "Mail":
		// What the browser would do, where, and what it would open up; the
		// note that would be kept or forgotten, as its card words it; or the
		// email that would be archived, as the server describes it.
		text += "  " + violetStyle.Render(ansi.Truncate(sanitize(toolSummary(request.Call)), max(6, w-ansi.StringWidth(request.Call.Name)-2), "…"))
	}
	text += "\n\n" + creamStyle.Render(ansi.Wrap(sanitize(request.Reason), w, ""))
	if request.Call.Name == "Browser" {
		page, origins := browserApproval(request.Call.Arguments)
		if page != "" {
			text += "\n\n" + creamStyle.Bold(true).Render("Page") + "\n" + creamStyle.Render(ansi.Hardwrap(sanitize(page), w, false))
		}
		if len(origins) > 0 {
			text += "\n\n" + coralStyle.Bold(true).Render("Approves for the rest of this turn") + "\n" + coralStyle.Render(ansi.Hardwrap(sanitize(strings.Join(origins, "\n")), w, false))
		}
	}
	if request.Call.Name == "Mail" {
		// The message the user decides on, as the server described it.
		if message := mailApprovalMessage(request.Call.Arguments); message != "" {
			text += "\n\n" + creamStyle.Bold(true).Render("Message") + "\n" + creamStyle.Render(ansi.Hardwrap(sanitize(message), w, false))
		}
	}
	if request.ExecutionMode == "unsandboxed" {
		text += "\n\n" + coralStyle.Bold(true).Render("OUTSIDE SANDBOX") + "\n" + coralStyle.Render(ansi.Hardwrap("Host-user filesystem access\nUnrestricted network access\nRuns as your current user; no sudo granted.", w, false))
		text += "\n\n" + creamStyle.Bold(true).Render("Working directory") + "\n" + creamStyle.Render(ansi.Hardwrap(sanitize(request.Directory), w, false))
		if request.Justification != "" {
			text += "\n\n" + creamStyle.Bold(true).Render("Justification") + "\n" + creamStyle.Render(ansi.Wrap(sanitize(request.Justification), w, ""))
		}
	}
	if request.Network && request.ExecutionMode != "unsandboxed" && request.Call.Name != "Browser" {
		text += "\n\n" + coralStyle.Render("Network access requested")
	}
	if request.Network && request.Call.Name == "Browser" {
		text += "\n\n" + coralStyle.Render("Reaches beyond this machine")
	}
	if command, rest, ok := approvalCommand(request.Call, raw); ok {
		// The command itself, as the agent would run it, is what is being
		// judged; it gets the card a reply's code gets. Its other arguments
		// are chips under the card: what the command asks for, at a glance.
		rows, _ := renderCodeCard(command, "bash", w, "")
		text += "\n\n" + strings.Join(rows, "\n")
		if len(rest) > 0 {
			text += "\n" + ansi.Truncate(strings.Join(rest, " "), w, "…")
		}
	} else {
		text += "\n\n" + mutedStyle.Render(ansi.Hardwrap(sanitize(arguments), w, false))
	}
	keys := ink(panel, violet).Bold(true).Render(" y / Enter ") + " " + creamStyle.Render(allowLabel(request)) + "   " + ink(panel, cream).Render(" n / Esc ") + " " + mutedStyle.Render("or any other key denies")
	text += "\n\n" + ansi.Truncate(keys, w, "…") + "\n\n" + faintStyle.Render("Mouse wheel scrolls the complete request")
	// As wide as the composer below it, border included.
	return lipgloss.NewStyle().Width(m.mainWidth - 2).Border(lipgloss.RoundedBorder()).BorderForeground(coral).Padding(1).Render(text)
}

// approvalCommand returns a Bash or Terminal request's command and its other
// arguments as chips, "◆ key value" each on the panel color, sorted by key,
// or ok false when the call carries no command.
func approvalCommand(call core.ToolCall, raw json.RawMessage) (command string, chips []string, ok bool) {
	if call.Name != "Bash" && call.Name != "Terminal" {
		return "", nil, false
	}
	var args map[string]json.RawMessage
	if json.Unmarshal(raw, &args) != nil || json.Unmarshal(args["command"], &command) != nil || strings.TrimSpace(command) == "" {
		return "", nil, false
	}
	// The card must show exactly what would run. A control character would
	// be stripped from the card but run by the shell, and the tool decodes
	// keys case-insensitively, so a second spelling of "command" could win;
	// either way the raw JSON, which escapes and lists everything, is shown.
	if strings.ContainsFunc(command, func(r rune) bool {
		return r != '\n' && r != '\t' && (unicode.IsControl(r) || !unicode.IsPrint(r) && !unicode.IsSpace(r))
	}) {
		return "", nil, false
	}
	for key := range args {
		if key != "command" && strings.EqualFold(key, "command") {
			return "", nil, false
		}
	}
	delete(args, "command")
	for _, key := range slices.Sorted(maps.Keys(args)) {
		value := oneLine(string(args[key]))
		var text string
		if json.Unmarshal(args[key], &text) == nil {
			value = oneLine(text)
		}
		chip := " ◆ " + oneLine(key) + " " + ansi.Truncate(value, 40, "…") + " "
		chips = append(chips, ink(panel, coral).Render(chip))
	}
	return command, chips, true
}

func elapsed(d time.Duration) string {
	seconds := max(0, int(d.Seconds()))
	if seconds >= 60 {
		return fmt.Sprintf("%dm %02ds", seconds/60, seconds%60)
	}
	return fmt.Sprintf("%ds", seconds)
}

// RenderDemo returns an offline, ANSI-styled preview for visual regression
// inspection. It starts neither a terminal program nor a provider.
func RenderDemo(width, height int, definitions ...core.ToolDefinition) string {
	return renderPreview(width, height, true, definitions)
}

// RenderWelcome returns the initial, empty-conversation screen without starting
// a terminal program or contacting a provider.
func RenderWelcome(width, height int, definitions ...core.ToolDefinition) string {
	return renderPreview(width, height, false, definitions)
}

func renderPreview(width, height int, demo bool, definitions []core.ToolDefinition) string {
	m := newModel(Config{
		Session: &core.Session{ID: "a quiet kind of capable", Directory: "~/code/julia-cli", Provider: "codex", Model: "your selected model"},
		Mode:    "auto", Demo: demo, ToolDefinitions: definitions,
	})
	defer m.stop()
	m.resize(width, height)
	return m.View().Content
}

// demoUsage is a plausible mid-session snapshot: a context well below the
// compaction threshold and most of the input served from the prompt cache.
func demoUsage() core.SessionUsage {
	return core.SessionUsage{Total: core.TokenUsage{Input: 38_400, CacheRead: 412_000, CacheWrite: 61_300, Output: 6_840}, Context: 118_600}
}

func demoEntries() []entry {
	return []entry{
		{role: "user", text: "Trace the approval flow and check cancellation of the ordinary process group."},
		{role: "assistant", text: "I'll follow the execution boundary, inspect the cancellation path, and run the focused checks."},
		{role: "tool", call: &core.ToolCall{Name: "Grep", Arguments: json.RawMessage(`{"pattern":"CommandContext|ProcessGroup|Approve","path":"internal/"}`)}, done: true, result: &core.ToolResult{Content: "12 matches across 3 files"}},
		{role: "tool", call: &core.ToolCall{Name: "Read", Arguments: json.RawMessage(`{"path":"internal/sandbox/runner.go"}`)}, done: true, result: &core.ToolResult{Content: "Read 148 lines · execution boundary and process lifecycle"}},
		{role: "tool", call: &core.ToolCall{Name: "Bash", Arguments: json.RawMessage(`{"command":"go test -race ./internal/sandbox/..."}`)}, done: true, result: &core.ToolResult{Content: "ok   julia/internal/sandbox   1.284s"}},
		{role: "tool", call: &core.ToolCall{Name: "Memory", Arguments: json.RawMessage(`{"action":"save","name":"process-group-cancel","description":"Cancelling a task must signal the ordinary process group; check it whenever the sandbox runner changes","type":"feedback","paths":["internal/sandbox/**"]}`)}, done: true, result: &core.ToolResult{Content: "Saved workspace note process-group-cancel"}},
		{role: "reasoning", text: "The approval check runs before the command starts. The cancellation check covers the ordinary process group, and the focused race tests passed."},
		{role: "assistant", text: "**The cancellation path is sound.**\n\nThe runner signals the ordinary process group when a task is interrupted. Approval is checked before execution, and a denied action never reaches the shell.\n\nThe focused race checks passed. No files changed."},
	}
}

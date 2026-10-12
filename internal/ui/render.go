package ui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/glamour/v2"
	"charm.land/glamour/v2/styles"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"jarvis/internal/core"
)

var (
	background  = lipgloss.Color("#17171B")
	panel       = lipgloss.Color("#202026")
	cream       = lipgloss.Color("#ECE5DC")
	muted       = lipgloss.Color("#96929F")
	faint       = lipgloss.Color("#56515F")
	border      = lipgloss.Color("#62536F")
	coral       = lipgloss.Color("#F49D89")
	violet      = lipgloss.Color("#C6B6F2")
	cyan        = lipgloss.Color("#92CED2")
	green       = lipgloss.Color("#A9C9AD")
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
	m.input.SetWidth(max(8, m.mainWidth-m.composerStyle().GetHorizontalFrameSize()))
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
	if m.renderWidth != m.mainWidth {
		style := styles.DarkStyleConfig
		margin := uint(0)
		style.Document.Margin = &margin
		style.Document.BlockPrefix = ""
		style.Document.BlockSuffix = ""
		style.Document.Color = str("#ECE5DC")
		style.Heading.Color = str("#C6B6F2")
		style.H1.Color = str("#F49D89")
		style.H1.BackgroundColor = nil
		style.Code.Color = str("#C6B6F2")
		style.Code.BackgroundColor = str("#27242E")
		style.CodeBlock.Theme = "dracula"
		style.CodeBlock.Margin = &margin
		style.Link.Color = str("#C6B6F2")
		style.LinkText.Color = str("#C6B6F2")
		m.markdown, _ = glamour.NewTermRenderer(glamour.WithStyles(style), glamour.WithWordWrap(max(10, m.mainWidth-4)))
		m.renderWidth = m.mainWidth
	}
	var blocks []string
	m.lines, m.rows = m.lines[:0], m.rows[:0]
	for i := range m.entries {
		layout := m.renderEntry(&m.entries[i])
		if i > 0 {
			m.lines = append(m.lines, "")
			m.rows = append(m.rows, rowRef{entry: -1})
		}
		blocks = append(blocks, layout.text)
		m.lines = append(m.lines, strings.Split(layout.text, "\n")...)
		for _, row := range layout.rows {
			m.rows = append(m.rows, rowRef{entry: i, rowMeta: row})
		}
	}
	m.activityRow = m.busy && len(blocks) > 0
	if m.activityRow {
		// Reserve the row under the latest entry; View draws the animated
		// activity there, so ticks never rebuild the conversation.
		blocks = append(blocks, "")
		m.lines = append(m.lines, "", "")
		m.rows = append(m.rows, rowRef{entry: -1, rowMeta: rowMeta{skip: true}}, rowRef{entry: -1, rowMeta: rowMeta{skip: true}})
	}
	m.viewport.SetContent(strings.Join(blocks, "\n\n"))
	if forceBottom || atBottom {
		m.viewport.GotoBottom()
	}
}

func str(value string) *string { return &value }

func (m *model) renderEntry(item *entry) entryLayout {
	if item.layout.text != "" && item.cachedRevision == item.revision && item.cachedWidth == m.mainWidth && item.cachedExpanded == m.expandTools && item.cachedGraphics == m.graphicsEpoch {
		return item.layout
	}
	item.layout = m.layoutEntry(*item)
	item.cachedRevision, item.cachedWidth, item.cachedExpanded, item.cachedGraphics = item.revision, m.mainWidth, m.expandTools, m.graphicsEpoch
	return item.layout
}

func (m *model) renderEntryContent(item entry) string { return m.layoutEntry(item).text }

// layoutEntry renders an entry together with the map from its rows back to the
// text they show, which is what selection and copying work from.
func (m *model) layoutEntry(item entry) entryLayout {
	w := max(10, m.mainWidth-4)
	var out entryLayout
	add := func(line string, row rowMeta) {
		out.text += line + "\n"
		out.rows = append(out.rows, row)
	}
	// wrapped adds text hard-wrapped to width, marking the rows a wrap created.
	wrapped := func(text string, width int, row rowMeta, render func(string) string) {
		for _, line := range strings.Split(text, "\n") {
			for i, part := range strings.Split(ansi.Hardwrap(line, width, false), "\n") {
				row.cont = i > 0
				add(render(part), row)
			}
		}
	}
	switch item.role {
	case "user":
		out.targets = []copyTarget{{"your message", item.text}}
		bar := lipgloss.NewStyle().Foreground(coral).Render("┃") + " "
		add(bar+coralStyle.Bold(true).Render("YOU"), rowMeta{gutter: 2, skip: true})
		wrapped(sanitize(item.text), w, rowMeta{gutter: 2}, func(s string) string { return bar + highlightImageTokens(s) })
		rows := imageRows(item.images)
		if len(item.images) > 0 && len(item.imageIDs) == len(item.images) && m.thumbnailsShown() {
			add(bar, rowMeta{gutter: 2, skip: true})
			rows = m.thumbnailRowsFor(item, w)
		}
		for _, row := range rows {
			add(bar+ansi.Truncate(row, w, "…"), rowMeta{gutter: 2, skip: true})
		}
	case "assistant":
		out.targets = []copyTarget{{"reply", item.text}}
		add(" "+violetStyle.Bold(true).Render("✦ JARVIS"), rowMeta{gutter: 1, skip: true})
		for i, segment := range splitFences(sanitize(item.text)) {
			if i > 0 {
				add("", rowMeta{gutter: 1})
			}
			if segment.code {
				target := len(out.targets)
				label := "code"
				if segment.language != "" {
					label = segment.language + " code"
				}
				out.targets = append(out.targets, copyTarget{label, segment.text})
				width := max(codeGutter+8, m.mainWidth-2)
				rows, continued := renderCodeBlock(segment.text, segment.language, width)
				for j, row := range rows {
					edge := j <= 1 || j == len(rows)-1 // header and padding rows
					add(" "+row, rowMeta{gutter: 1 + codeGutter, end: width, cont: continued[j], skip: edge, target: target, code: true})
				}
				continue
			}
			text := segment.text
			if m.markdown != nil {
				if rendered, err := m.markdown.Render(text); err == nil {
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
				add(" "+line, rowMeta{gutter: 1})
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
		name, detail := "Tool", item.text
		if item.call != nil {
			name, detail = item.call.Name, toolSummary(*item.call)
		}
		if item.call != nil && item.call.Name == "Browser" && item.result != nil {
			// The result names the elements the call gave only refs for.
			headline, _, _ := strings.Cut(item.result.Content, "\n")
			if summary, ok := browserSummary(item.call.Arguments, headline); ok {
				detail = summary
			}
		}
		icon := violetStyle.Render("◌")
		if item.done {
			icon = lipgloss.NewStyle().Foreground(green).Render("✓")
		}
		if item.result != nil && item.result.IsError {
			icon = coralStyle.Render("!")
		}
		bar := faintStyle.Render("│") + " "
		title := icon + " " + cyanStyle.Bold(true).Render(sanitize(name))
		if detail != "" {
			title += "  " + mutedStyle.Render(ansi.Truncate(sanitize(detail), max(6, w-ansi.StringWidth(name)-5), "…"))
		}
		add(bar+title, rowMeta{gutter: 2})
		if item.call != nil && item.call.Name == "Subagent" && m.expandTools {
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
			} else if result != "" {
				first, _, _ := strings.Cut(result, "\n")
				add(bar+faintStyle.Render("  "+ansi.Truncate(first, w-2, "…")), rowMeta{gutter: 4, target: 1})
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
	case "warning":
		out.targets = []copyTarget{{"warning", item.text}}
		wrapped(sanitize(item.text), w, rowMeta{gutter: 2}, func(s string) string { return coralStyle.Render("! " + s) })
	default:
		out.targets = []copyTarget{{"note", item.text}}
		wrapped(sanitize(item.text), w, rowMeta{gutter: 2}, func(s string) string { return mutedStyle.Italic(true).Render("· " + s) })
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
		var args map[string]json.RawMessage
		if json.Unmarshal(item.call.Arguments, &args) == nil {
			for _, key := range []string{"command", "path", "file_path", "pattern", "query", "directory", "url", "script"} {
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
	var args map[string]json.RawMessage
	if json.Unmarshal(call.Arguments, &args) != nil {
		return string(call.Arguments)
	}
	for _, key := range []string{"path", "file_path", "command", "pattern", "directory", "query"} {
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
	return ""
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
// $JARVIS_SCRATCH/shot.png.
func scratchPath(path string) string {
	if !filepath.IsAbs(path) {
		return path
	}
	_, session, ok := strings.Cut(filepath.ToSlash(path), "/jarvis-scratch/")
	if _, inside, ok2 := strings.Cut(session, "/"); ok && ok2 && inside != "" {
		return "$JARVIS_SCRATCH/" + inside
	}
	return path
}

func (m *model) View() tea.View {
	if m.width == 0 {
		return tea.NewView("")
	}
	contentWidth := m.width - 2*m.margin()
	brand := coralStyle.Bold(true).Render("◆  JARVIS")
	caption := mutedStyle.Render("  YOUR PERSONAL AGENT")
	if m.width < 65 {
		caption = ""
	}
	right := violetStyle.Render("● " + sanitize(m.cfg.Mode))
	if m.cfg.Demo {
		right = violetStyle.Render("◈ PREVIEW")
	}
	header := brand + caption
	header += strings.Repeat(" ", max(1, contentWidth-ansi.StringWidth(header)-ansi.StringWidth(right))) + right
	header = "\n" + ansi.Truncate(header, contentWidth, "") + "\n" + faintStyle.Render(strings.Repeat("─", contentWidth))
	body := m.withActivity(m.highlightSelection(m.viewport.View()))
	if m.picker != nil && m.pending == nil {
		body = m.pickerView()
	} else if m.question != nil && m.pending == nil {
		body = m.questionBody()
	} else if len(m.entries) == 0 && m.pending == nil {
		body = m.welcome()
	}
	body = lipgloss.NewStyle().Width(m.mainWidth).Height(m.bodyHeight).Render(body) + "\n" + m.bottomView()
	if m.hasSidebar() {
		// The sidebar runs from the header rule to the bottom edge, beside the composer.
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
	v.WindowTitle = "JARVIS — personal agent"
	v.MouseMode = tea.MouseModeCellMotion
	return v
}

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
		Background(panel).Border(lipgloss.RoundedBorder()).BorderForeground(stroke).BorderBackground(panel)
}

func (m *model) bottomView() string {
	style := m.composerStyle()
	innerWidth := max(1, m.mainWidth-style.GetHorizontalFrameSize())
	composerTitle := violetStyle.Bold(true).Render("MESSAGE")
	composerInput := m.input.View()
	if m.picker != nil {
		composerTitle = violetStyle.Bold(true).Render("SEARCH") + mutedStyle.Render(" · your message draft is kept")
		if m.mainWidth < 44 {
			composerTitle = violetStyle.Bold(true).Render("SEARCH") + mutedStyle.Render(" · draft kept")
		}
		composerInput = m.picker.search.View() + "\n"
	}
	if m.busy {
		composerTitle = violetStyle.Bold(true).Render("DRAFT") + mutedStyle.Render(" · retained while JARVIS works")
	}
	if m.question != nil {
		composerTitle = violetStyle.Bold(true).Render("DRAFT") + mutedStyle.Render(" · kept while you answer")
	}
	if m.pending != nil {
		composerTitle = coralStyle.Render("APPROVAL · y / Enter")
	}
	if m.picker == nil && m.pending == nil {
		composerTitle += m.attachmentChips(max(0, innerWidth-ansi.StringWidth(composerTitle)))
	}
	composer := style.Render(ansi.Truncate(composerTitle, innerWidth, "…") + "\n" + composerInput)
	var rows []string
	if m.height >= 28 {
		rows = append(rows, "")
	}
	rows = append(rows, m.statusLine())
	if m.height >= 28 {
		rows = append(rows, "")
	}
	rows = append(rows, composer)
	if m.height >= 36 {
		rows = append(rows, "")
	}
	rows = append(rows, m.footer(), "")
	return strings.Join(rows, "\n")
}

func (m *model) welcome() string {
	w := m.mainWidth
	var content string
	if m.bodyHeight >= 15 && w >= 65 {
		logo := coralStyle.Bold(true).Render("✦  J A R V I S")
		if m.wordmarkReady {
			logo = wordmarkPlaceholders()
		}
		content = logo + "\n\n" + creamStyle.Bold(true).Render("A little less supervision. A lot more done.") + "\n\n" + mutedStyle.Render("Think deeply. Work carefully. Keep going.") + "\n\n" + faintStyle.Render("────────────────────────────────────────") + "\n\n" + violetStyle.Render("01") + creamStyle.Render("  Explore a codebase and explain the moving parts") + "\n" + violetStyle.Render("02") + creamStyle.Render("  Investigate a question. Follow the evidence.") + "\n" + violetStyle.Render("03") + creamStyle.Render("  Make a change, verify it, and report back")
	} else {
		content = coralStyle.Bold(true).Render("J A R V I S") + "\n\n" + creamStyle.Render("At your service, sir.") + "\n" + mutedStyle.Render("Give me something worth doing.")
	}
	return lipgloss.Place(w, m.bodyHeight, lipgloss.Center, lipgloss.Center, content)
}

func showsReasoning(provider string, compatibleReasoning bool) bool {
	return provider == "codex" || (provider == "compatible" && compatibleReasoning)
}

func reasoningDisplay(provider, effort string) string {
	if effort != "" {
		return effort
	}
	if provider == "compatible" {
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
	const width = 23
	label := func(s string) string { return violetStyle.Bold(true).Render(s) }
	value := func(s string) string { return creamStyle.Render(ansi.Truncate(sanitize(s), width, "…")) }
	provider, modelName, reasoning, directory, id := "—", "default", "", "", "new conversation"
	if m.cfg.Session != nil {
		provider, modelName, directory, id = m.cfg.Session.Provider, m.cfg.Session.Model, m.cfg.Session.Directory, m.cfg.Session.ID
		reasoning = m.cfg.Session.Reasoning
	}
	if modelName == "" {
		modelName = "provider default"
	}
	reasoning = reasoningDisplay(provider, reasoning)
	if directory != "" {
		directory = filepath.Clean(directory)
	}
	connection := []string{label("CONNECTION"), cyanStyle.Render(ansi.Truncate(sanitize(provider), width, "…")), value(modelName)}
	if showsReasoning(provider, m.cfg.CompatibleReasoning) {
		if provider == "compatible" && reasoning == "provider default" {
			connection = append(connection, mutedStyle.Render("reasoning:"), mutedStyle.Render("provider default"))
		} else {
			connection = append(connection, mutedStyle.Render(ansi.Truncate("reasoning: "+sanitize(reasoning), width, "…")))
		}
		if provider == "codex" {
			connection = append(connection, m.weeklyUsageLine())
		}
	}
	// The per-call figures describe the last model call only; the session totals
	// and the context estimate have their own sections.
	activity := []string{label("ACTIVITY"), value(fmt.Sprintf("%d tool calls", m.toolCount)), mutedStyle.Render("last call"), mutedStyle.Render(ansi.Truncate(fmt.Sprintf("%s in · %s out", core.CompactTokens(m.inputTokens), core.CompactTokens(m.outputTokens)), width, "…"))}
	sections := [][]string{
		{label("WORKSPACE"), value(filepath.Base(directory)), mutedStyle.Render(ansi.Truncate(directory, width, "…"))},
		connection,
		m.contextSection(label, width),
		m.tokenSection(label, width),
		activity,
		{label("PERMISSIONS"), lipgloss.NewStyle().Foreground(green).Render(ansi.Truncate("◈ "+sanitize(m.cfg.Mode), width, "…")), mutedStyle.Render(ansi.Truncate(permissionCaption(m.cfg.Mode), width, "…"))},
		{label("SESSION"), value(id)},
	}
	if rows := toolRows(m.cfg.ToolDefinitions, width); len(rows) > 0 {
		essentials := []string{faintStyle.Render(strings.Repeat("─", width)), "", label("THE ESSENTIALS")}
		for _, row := range rows {
			essentials = append(essentials, mutedStyle.Render(row))
		}
		sections = append(sections, essentials)
	}
	// The masthead and every section always render in full; a sidebar taller than
	// the body scrolls on its own under the mouse wheel.
	lines := slices.Concat([]string{""}, sidebarLogo(m.cfg.Version, width))
	for _, section := range sections {
		lines = append(append(lines, ""), section...)
	}
	height := m.sidebarHeight()
	m.sidebarOffset = max(0, min(m.sidebarOffset, len(lines)-height))
	lines = lines[m.sidebarOffset:]
	if len(lines) > height {
		lines = lines[:height]
	}
	return lipgloss.NewStyle().Width(26).Height(height).BorderLeft(true).BorderStyle(lipgloss.NormalBorder()).BorderForeground(faint).PaddingLeft(2).Render(strings.Join(lines, "\n"))
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
	default:
		return "Enforced per action" // must fit the 23-column sidebar on one row
	}
}

// activityLine is the animated stream, what the task is doing, and how long
// it has run, fitted to width with the elapsed time kept visible.
func (m *model) activityLine(width int) string {
	streamWidth := min(activityWidth, max(3, width/5))
	stream := ansi.Truncate(m.spinner.View(), streamWidth, "")
	duration := mutedStyle.Render(elapsed(m.now.Sub(m.started)))
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
	} else if m.lastDuration > 0 {
		status += faintStyle.Render(" · " + elapsed(m.lastDuration))
	}
	if !m.hasSidebar() && m.cfg.Session != nil {
		model := m.cfg.Session.Model
		if model == "" {
			model = "provider default"
		}
		settings := model
		if showsReasoning(m.cfg.Session.Provider, m.cfg.CompatibleReasoning) {
			settings += " · " + reasoningDisplay(m.cfg.Session.Provider, m.cfg.Session.Reasoning)
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
	}
	if m.notice != "" {
		status = mutedStyle.Render(sanitize(m.notice))
	}
	if !m.viewport.AtBottom() && len(m.entries) > 0 && m.pending == nil && m.question == nil && m.picker == nil && m.notice == "" {
		status = violetStyle.Render("↑ Reading history") + mutedStyle.Render(" · Ctrl+End to follow output")
	}
	return ansi.Truncate(status, m.mainWidth, "…")
}

func (m *model) footer() string {
	if m.question != nil && m.pending == nil {
		if m.width < 36 {
			return mutedStyle.Render("ctrl+c stops")
		}
		if m.width < 62 {
			return mutedStyle.Render("answer above · ctrl+c stops")
		}
		if m.width < 78 {
			return violetStyle.Render("answering") + mutedStyle.Render("  ctrl+c stops the task")
		}
		return violetStyle.Render("answering") + mutedStyle.Render("  ctrl+c stops the task  mouse wheel scrolls the conversation")
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
	if m.width < 62 {
		return coralStyle.Render("enter") + mutedStyle.Render(" send · ") + violetStyle.Render("ctrl+p") + mutedStyle.Render(" models · ") + violetStyle.Render("/help")
	}
	// Shortcuts join in order of usefulness while they fit beside the sidebar,
	// then show in their usual order.
	extras := []string{"ctrl+o tools", "ctrl+p models", "drag copy", "ctrl+v image"}
	shown := make([]bool, len(extras))
	used := ansi.StringWidth("enter send  alt+enter newline  esc interrupt  /help")
	for _, i := range []int{1, 0, 2, 3} {
		if used+2+len(extras[i]) <= m.mainWidth {
			used, shown[i] = used+2+len(extras[i]), true
		}
	}
	left := coralStyle.Render("enter ") + mutedStyle.Render("send") + violetStyle.Render("  alt+enter ") + mutedStyle.Render("newline") + violetStyle.Render("  esc ") + mutedStyle.Render("interrupt") + violetStyle.Render("  /help")
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
	if request.Call.Name == "Browser" {
		// What the browser would do, where, and what it would open up.
		text += "  " + violetStyle.Render(ansi.Truncate(sanitize(toolSummary(request.Call)), max(6, w-9), "…"))
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
	text += "\n\n" + mutedStyle.Render(ansi.Hardwrap(sanitize(arguments), w, false)) + "\n\n" + violetStyle.Bold(true).Render("y / Enter  "+allowLabel(request)) + "\n" + creamStyle.Render("n / Esc / any other key  Deny") + "\n\n" + faintStyle.Render("Mouse wheel scrolls the complete request")
	return lipgloss.NewStyle().Width(m.mainWidth - 4).Border(lipgloss.RoundedBorder()).BorderForeground(coral).Padding(1).Render(text)
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
		Session: &core.Session{ID: "a quiet kind of capable", Directory: "~/code/jarvis-cli", Provider: "codex", Model: "your selected model"},
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
		{role: "tool", call: &core.ToolCall{Name: "Bash", Arguments: json.RawMessage(`{"command":"go test -race ./internal/sandbox/..."}`)}, done: true, result: &core.ToolResult{Content: "ok   jarvis/internal/sandbox   1.284s"}},
		{role: "assistant", text: "**The cancellation path is sound, sir.**\n\nThe runner signals the ordinary process group when a task is interrupted. Approval is checked before execution, and a denied action never reaches the shell.\n\nThe focused race checks passed. No files changed."},
	}
}

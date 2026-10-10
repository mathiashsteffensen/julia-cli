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
	coral       = lipgloss.Color("#F49D89")
	violet      = lipgloss.Color("#C6B6F2")
	green       = lipgloss.Color("#A9C9AD")
	coralStyle  = lipgloss.NewStyle().Foreground(coral)
	violetStyle = lipgloss.NewStyle().Foreground(violet)
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
	m.bodyHeight = max(1, m.height-11)
	m.input.SetWidth(max(8, m.mainWidth-5))
	if m.picker != nil {
		m.picker.search.SetWidth(max(8, m.mainWidth-8))
	}
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
	m.viewport.SetContent(strings.Join(blocks, "\n\n"))
	if forceBottom || atBottom {
		m.viewport.GotoBottom()
	}
}

func str(value string) *string { return &value }

func (m *model) renderEntry(item *entry) entryLayout {
	if item.layout.text != "" && item.cachedRevision == item.revision && item.cachedWidth == m.mainWidth && item.cachedExpanded == m.expandTools {
		return item.layout
	}
	item.layout = m.layoutEntry(*item)
	item.cachedRevision, item.cachedWidth, item.cachedExpanded = item.revision, m.mainWidth, m.expandTools
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
		wrapped(sanitize(item.text), w, rowMeta{gutter: 2}, func(s string) string { return bar + creamStyle.Render(s) })
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
		icon := violetStyle.Render("◌")
		if item.done {
			icon = lipgloss.NewStyle().Foreground(green).Render("✓")
		}
		if item.result != nil && item.result.IsError {
			icon = coralStyle.Render("!")
		}
		bar := faintStyle.Render("│") + " "
		title := icon + " " + creamStyle.Render(sanitize(name))
		if detail != "" {
			title += "  " + mutedStyle.Render(ansi.Truncate(sanitize(detail), max(6, w-ansi.StringWidth(name)-5), "…"))
		}
		add(bar+title, rowMeta{gutter: 2})
		if item.result != nil {
			result := strings.TrimSpace(sanitize(item.result.Content))
			if m.expandTools {
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
			for _, key := range []string{"command", "path", "file_path", "pattern", "query", "directory"} {
				var value string
				if json.Unmarshal(args[key], &value) == nil {
					label, text = key, value
					break
				}
			}
		}
	}
	output := ""
	if item.result != nil {
		output = item.result.Content
	}
	return []copyTarget{{label, text}, {"output", output}}
}

func toolSummary(call core.ToolCall) string {
	var args map[string]json.RawMessage
	if json.Unmarshal(call.Arguments, &args) != nil {
		return string(call.Arguments)
	}
	for _, key := range []string{"path", "file_path", "command", "pattern", "directory", "query"} {
		if raw, ok := args[key]; ok {
			var value string
			if json.Unmarshal(raw, &value) == nil {
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
	body := m.highlightSelection(m.viewport.View())
	if m.picker != nil && m.pending == nil {
		body = m.pickerView()
	} else if m.question != nil && m.pending == nil {
		body = m.questionBody()
	} else if len(m.entries) == 0 && m.pending == nil {
		body = m.welcome()
	}
	if m.hasSidebar() {
		body = lipgloss.JoinHorizontal(lipgloss.Top, lipgloss.NewStyle().Width(m.mainWidth).Height(m.bodyHeight).Render(body), "  ", m.sidebar())
	}
	composerTitle := violetStyle.Render("MESSAGE")
	composerInput := m.input.View()
	if m.picker != nil {
		composerTitle = violetStyle.Render("SEARCH · your message draft is kept")
		if m.mainWidth < 44 {
			composerTitle = violetStyle.Render("SEARCH · draft kept")
		}
		composerInput = m.picker.search.View() + "\n"
	}
	if m.busy {
		composerTitle = mutedStyle.Render("DRAFT · retained while JARVIS works")
	}
	if m.question != nil {
		composerTitle = mutedStyle.Render("DRAFT · kept while you answer")
	}
	if m.pending != nil {
		composerTitle = coralStyle.Render("APPROVAL · y / Enter")
	}
	composer := lipgloss.NewStyle().Width(max(10, m.mainWidth-4)).Padding(0, 1).Background(panel).Border(lipgloss.RoundedBorder()).BorderForeground(faint).Render(composerTitle + "\n" + composerInput)
	status := m.statusLine()
	footer := m.footer()
	view := header + "\n" + body + "\n" + status + "\n" + composer + "\n" + footer
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

func (m *model) sidebar() string {
	const width = 23
	label := func(s string) string { return faintStyle.Bold(true).Render(s) }
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
	// Variants run from fullest to most compact. Without room for the provider
	// row the model alone identifies the connection, and the reasoning effort and
	// weekly allowance are the next to go.
	title, name := label("CONNECTION"), value(modelName)
	providerRow := violetStyle.Render(ansi.Truncate(sanitize(provider), width, "…"))
	connection := [][]string{{title, providerRow, name}, {title, name}}
	if showsReasoning(provider, m.cfg.CompatibleReasoning) {
		effort := []string{mutedStyle.Render(ansi.Truncate("reasoning: "+sanitize(reasoning), width, "…"))}
		if provider == "compatible" && reasoning == "provider default" {
			effort = []string{mutedStyle.Render("reasoning:"), mutedStyle.Render("provider default")}
		}
		var weekly []string
		if provider == "codex" {
			weekly = []string{m.weeklyUsageLine()}
		}
		// The weekly allowance stays in every variant: it is why Codex users look here.
		connection = [][]string{
			slices.Concat([]string{title, providerRow, name}, effort, weekly),
			slices.Concat([]string{title, name}, weekly, effort),
			slices.Concat([]string{title, name}, weekly),
		}
	}
	// The per-call figures describe the last model call only; the session totals
	// and the context estimate have their own sections.
	activity := []string{label("ACTIVITY"), value(fmt.Sprintf("%d tool calls", m.toolCount)), mutedStyle.Render("last call"), mutedStyle.Render(ansi.Truncate(fmt.Sprintf("%s in · %s out", core.CompactTokens(m.inputTokens), core.CompactTokens(m.outputTokens)), width, "…"))}
	var essentials [][]string
	if rows := toolRows(m.cfg.ToolDefinitions, width); len(rows) > 0 {
		lines := []string{faintStyle.Render(strings.Repeat("─", width)), "", label("THE ESSENTIALS")}
		for _, row := range rows {
			lines = append(lines, mutedStyle.Render(row))
		}
		for n := len(lines); n > 3; n-- {
			essentials = append(essentials, lines[:n])
		}
	}
	// Rank is survival order on a short terminal (1 is dropped last). The context
	// gauge outlives everything: it is the figure that decides when to start over.
	lines := fitSidebar([]sidebarBlock{
		{4, [][]string{{label("WORKSPACE"), value(filepath.Base(directory)), mutedStyle.Render(ansi.Truncate(directory, width, "…"))}}},
		{2, connection},
		{1, m.contextVariants(label, width)},
		{3, m.tokenVariants(label, width)},
		{6, [][]string{activity}},
		{5, [][]string{{label("PERMISSIONS"), lipgloss.NewStyle().Foreground(green).Render(ansi.Truncate("◈ "+sanitize(m.cfg.Mode), width, "…")), mutedStyle.Render(ansi.Truncate(permissionCaption(m.cfg.Mode), width, "…"))}}},
		{7, [][]string{{label("SESSION"), value(id)}}},
		{8, essentials},
	}, m.bodyHeight)
	if len(lines) > m.bodyHeight {
		lines = lines[:m.bodyHeight]
	}
	return lipgloss.NewStyle().Width(26).Height(m.bodyHeight).BorderLeft(true).BorderStyle(lipgloss.NormalBorder()).BorderForeground(faint).PaddingLeft(2).Render(strings.Join(lines, "\n"))
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

func (m *model) statusLine() string {
	status := lipgloss.NewStyle().Foreground(green).Render("●") + mutedStyle.Render(" Ready")
	if m.busy {
		status = m.spinner.View() + " " + violetStyle.Render(sanitize(m.status)) + " " + faintStyle.Render(elapsed(m.now.Sub(m.started)))
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
		status += faintStyle.Render(" · " + sanitize(settings))
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
		return coralStyle.Render("y / enter allow once") + mutedStyle.Render("  any other key denies  mouse wheel scrolls")
	}
	if m.picker != nil {
		if m.width < 40 {
			return mutedStyle.Render("↑↓ select · enter · esc back")
		}
		if m.width < 62 {
			return mutedStyle.Render("↑↓ choose · enter apply · esc back")
		}
		text := "↑↓ choose  enter apply  esc back"
		if m.picker.kind == "model" && len(m.modelCatalogProviders()) > 1 {
			text += "  tab provider"
		} else if m.picker.kind == "reasoning" && len(m.modelCatalogProviders()) > 1 {
			text += "  tab models"
		}
		return mutedStyle.Render(text)
	}
	if m.width < 62 {
		return faintStyle.Render("enter send · ctrl+p models · /help")
	}
	left := faintStyle.Render("enter ") + mutedStyle.Render("send") + faintStyle.Render("  alt+enter ") + mutedStyle.Render("newline") + faintStyle.Render("  esc ") + mutedStyle.Render("interrupt") + faintStyle.Render("  /help")
	if m.width >= 90 {
		left += faintStyle.Render("  ctrl+o ") + mutedStyle.Render("tools")
	}
	if m.width >= 74 {
		left += faintStyle.Render("  ctrl+p ") + mutedStyle.Render("models")
	}
	if m.width >= 100 {
		left += faintStyle.Render("  drag ") + mutedStyle.Render("copy")
	}
	return left
}

func (m *model) approvalView() string {
	request := m.pending.Approval
	w := max(12, m.mainWidth-6)
	var pretty bytes.Buffer
	arguments := string(request.Call.Arguments)
	if json.Indent(&pretty, request.Call.Arguments, "", "  ") == nil {
		arguments = pretty.String()
	}
	text := coralStyle.Bold(true).Render("PERMISSION REQUEST")
	text += "\n" + creamStyle.Bold(true).Render(sanitize(request.Call.Name)) + "\n\n" + creamStyle.Render(ansi.Hardwrap(sanitize(request.Reason), w, false))
	if request.ExecutionMode == "unsandboxed" {
		text += "\n\n" + coralStyle.Bold(true).Render("OUTSIDE SANDBOX") + "\n" + coralStyle.Render(ansi.Hardwrap("Host-user filesystem access\nUnrestricted network access\nRuns as your current user; no sudo granted.", w, false))
		text += "\n\n" + creamStyle.Bold(true).Render("Working directory") + "\n" + creamStyle.Render(ansi.Hardwrap(sanitize(request.Directory), w, false))
		if request.Justification != "" {
			text += "\n\n" + creamStyle.Bold(true).Render("Justification") + "\n" + creamStyle.Render(ansi.Hardwrap(sanitize(request.Justification), w, false))
		}
	}
	if request.Network && request.ExecutionMode != "unsandboxed" {
		text += "\n\n" + coralStyle.Render("Network access requested")
	}
	text += "\n\n" + mutedStyle.Render(ansi.Hardwrap(sanitize(arguments), w, false)) + "\n\n" + violetStyle.Bold(true).Render("y / Enter  Allow once") + "\n" + creamStyle.Render("n / Esc / any other key  Deny") + "\n\n" + faintStyle.Render("Mouse wheel scrolls the complete request")
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

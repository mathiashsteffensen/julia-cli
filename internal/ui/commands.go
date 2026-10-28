package ui

import (
	"fmt"
	"image/color"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// command is one slash command the composer offers: a built-in, or one of
// the user's skills.
type command struct {
	// name is the command without its slash, such as "model" or
	// "skill:review".
	name        string
	description string
	// skill is the skill the command runs; empty for a built-in.
	skill string
}

// builtinCommands are JULIA's own commands, in the order the menu lists them.
var builtinCommands = []command{
	{name: "help", description: "Keys and commands"},
	{name: "clear", description: "Start a fresh conversation"},
	{name: "model", description: "Choose a model · /model provider/ID sets one"},
	{name: "reasoning", description: "Choose the reasoning effort · /reasoning LEVEL sets it"},
	{name: "profile", description: "Switch configuration profile · /profile NAME"},
	{name: "review", description: "Review uncommitted changes · /review WHAT reviews what you name"},
	{name: "copy", description: "Copy the last reply · /copy code copies its last code block"},
	{name: "memory", description: "What JULIA remembers here · /memory NAME shows a note"},
	{name: "quit", description: "Exit JULIA"},
	{name: "exit", description: "Exit JULIA"},
}

// skillPrefix marks a skill's command when its name is a built-in's.
const skillPrefix = "skill:"

// commands is every command the composer offers: the built-ins, then a
// command for each of the user's skills, named after it, or skill:<name>
// when a built-in has that name.
func (m *model) commands() []command {
	all := slices.Clone(builtinCommands)
	if m.cfg.Demo {
		all = append(all, command{name: "ask", description: "Preview a question card · /ask text asks for typed text"})
	}
	builtin := len(all)
	for _, skill := range m.skills {
		name := skill.Name
		if slices.ContainsFunc(all[:builtin], func(c command) bool { return c.name == name }) {
			name = skillPrefix + name
		}
		all = append(all, command{name: name, description: skill.Description, skill: skill.Name})
	}
	return all
}

// loadSkills reads the user's skills again, so the commands follow the
// skills folder without a restart.
func (m *model) loadSkills() {
	if m.cfg.Skills != nil {
		m.skills = m.cfg.Skills()
	}
}

// skillCommand returns the skill a prompt runs, when its first word is a
// skill's command.
func (m *model) skillCommand(prompt string) (string, bool) {
	word, _, _ := strings.Cut(strings.TrimSpace(prompt), " ")
	word, _, _ = strings.Cut(word, "\n")
	name, ok := strings.CutPrefix(word, "/")
	if !ok {
		return "", false
	}
	m.loadSkills()
	for _, c := range m.commands() {
		if c.skill != "" && c.name == name {
			return c.skill, true
		}
	}
	return "", false
}

// commandMenu is the composer's command completion: open while the draft is
// a slash and the start of a command's name, and nothing else is open.
type commandMenu struct {
	// query is the typed name the matches and selection were made for.
	query    string
	matches  []command
	selected int
	// dismissed is the draft Esc closed the menu on; it stays closed until
	// the draft changes.
	dismissed string
}

// menuQuery is the command name being typed, and whether the menu applies:
// the draft is a slash and a name with no space after it yet, and no picker,
// card or fleet has the keys.
func (m *model) menuQuery() (string, bool) {
	if m.picker != nil || m.question != nil || m.pending != nil || m.fleetView != nil {
		return "", false
	}
	draft := m.input.Value()
	query, ok := strings.CutPrefix(draft, "/")
	if !ok || strings.ContainsAny(query, " \t\n") || draft == m.menu.dismissed {
		return "", false
	}
	return query, true
}

// syncMenu brings the menu up to date with the draft. It reads the skills
// again each time the menu opens.
func (m *model) syncMenu() {
	query, open := m.menuQuery()
	if !open {
		if m.menu.matches != nil {
			m.menu = commandMenu{dismissed: m.menu.dismissed}
		}
		if m.input.Value() != m.menu.dismissed {
			m.menu.dismissed = ""
		}
		return
	}
	if m.menu.matches != nil && query == m.menu.query {
		return
	}
	if m.menu.matches == nil {
		m.loadSkills()
	}
	m.menu.query, m.menu.matches, m.menu.selected = query, matchCommands(m.commands(), query), 0
}

// matchCommands is the commands whose name starts with query, then those
// that contain it, each group in the commands' own order. A skill's command
// under skill: also matches on the skill's own name.
func matchCommands(all []command, query string) []command {
	query = strings.ToLower(query)
	var starts, contains []command
	for _, c := range all {
		bare := strings.TrimPrefix(c.name, skillPrefix)
		switch {
		case strings.HasPrefix(c.name, query) || strings.HasPrefix(bare, query):
			starts = append(starts, c)
		case strings.Contains(c.name, query):
			contains = append(contains, c)
		}
	}
	return append(starts, contains...)
}

func (m *model) menuOpen() bool { return len(m.menu.matches) > 0 }

// updateMenu handles a key while the menu is open: ↑ and ↓ choose, Tab
// completes the chosen command, Enter runs it, and Esc closes the menu.
// handled is false for every other key, which edits the draft as usual.
func (m *model) updateMenu(msg tea.KeyPressMsg) (handled bool, cmd tea.Cmd) {
	count := len(m.menu.matches)
	switch msg.String() {
	case "up":
		m.menu.selected = (m.menu.selected + count - 1) % count
	case "down":
		m.menu.selected = (m.menu.selected + 1) % count
	case "tab":
		m.complete(" ")
	case "enter":
		// A command typed in full runs as typed; otherwise the chosen one does.
		if !slices.ContainsFunc(m.menu.matches, func(c command) bool { return c.name == m.menu.query }) {
			m.complete("")
		}
		cmd = m.submit()
		m.syncMenu()
	case "esc":
		m.menu = commandMenu{dismissed: m.input.Value()}
	default:
		return false, nil
	}
	return true, cmd
}

// complete puts the chosen command in the draft, followed by after.
func (m *model) complete(after string) {
	m.input.SetValue("/" + m.menu.matches[m.menu.selected].name + after)
	m.input.CursorEnd()
	m.syncMenu()
}

// maxMenuRows bounds the commands the menu shows at once.
const maxMenuRows = 7

// menuRows is the menu, drawn over the bottom of the conversation just above
// the status line: a heading and the matching commands, at most rows tall.
// Each row is width cells wide.
func (m *model) menuRows(width, rows int) []string {
	matches := m.menu.matches
	visible := min(len(matches), maxMenuRows, rows-2)
	if visible < 1 {
		return nil
	}
	selected := m.menu.selected
	start := max(0, selected-visible+1)

	// Names share one column, as wide as the longest shown, within reason.
	nameWidth := 0
	for _, c := range matches[start : start+visible] {
		nameWidth = max(nameWidth, ansi.StringWidth(sanitize(c.name))+1)
	}
	nameWidth = min(nameWidth, max(8, width/3))

	heading := ink(background, violet).Bold(true).Render("COMMANDS")
	position := ""
	if count := len(matches); count > visible {
		position = fmt.Sprintf("  %d / %d", selected+1, count)
	}
	// The longest hint that fits beside the heading.
	hint := ""
	for _, option := range []string{"↑↓ choose · tab completes · enter runs · esc closes", "tab completes · enter runs", ""} {
		if ansi.StringWidth(heading)+2+ansi.StringWidth(option+position) <= width {
			hint = option
			break
		}
	}
	hint = strings.TrimLeft(hint+position, " ")
	heading += ink(background, faint).Render(strings.Repeat(" ", max(2, width-ansi.StringWidth(heading)-ansi.StringWidth(hint))) + hint)
	lines := []string{fitRow(" ", background, width), fitRow(heading, background, width)}

	// The chosen command is lit as a picker's choice is: a coral bar and the
	// panel behind it, its description brought up from faint to muted.
	for i := start; i < start+visible; i++ {
		c := matches[i]
		bg, bar, name, detail := background, " ", ink(background, cream), ink(background, faint)
		if i == selected {
			bg, bar, name, detail = panel, ink(panel, coral).Render("▌"), ink(panel, cream).Bold(true), ink(panel, muted)
		}
		tag := ""
		if c.skill != "" {
			tag = "skill"
		}
		row := ink(bg, cream).Render(" ") + bar + ink(bg, cream).Render(" ")
		row += name.Render(ansi.Truncate("/"+sanitize(c.name), nameWidth, "…"))
		row += ink(bg, cream).Render(strings.Repeat(" ", max(1, nameWidth+2-ansi.StringWidth("/"+sanitize(c.name)))))
		room := width - ansi.StringWidth(row) - len(tag) - 3
		row += detail.Render(ansi.Truncate(oneLine(c.description), max(0, room), "…"))
		if tag != "" {
			row += ink(bg, cream).Render(strings.Repeat(" ", max(1, width-ansi.StringWidth(row)-len(tag)-1))) + ink(bg, violet).Render(tag)
		}
		lines = append(lines, fitRow(row, bg, width))
	}
	return lines
}

// fitRow pads or cuts row to exactly width cells on bg.
func fitRow(row string, bg color.Color, width int) string {
	row = ansi.Truncate(row, width, "")
	if pad := width - ansi.StringWidth(row); pad > 0 {
		row += ink(bg, cream).Render(strings.Repeat(" ", pad))
	}
	return row
}

// overlayMenu draws the menu over the last rows of body, a block of rows
// rows each width cells wide.
func (m *model) overlayMenu(body string, width, rows int) string {
	menu := m.menuRows(width, rows)
	if len(menu) == 0 {
		return body
	}
	lines := strings.Split(body, "\n")
	at := len(lines) - len(menu)
	if at < 0 {
		return body
	}
	copy(lines[at:], menu)
	return strings.Join(lines, "\n")
}

package ui

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// The chrome frames the conversation in its column: the masthead above it,
// and below it the composer with one row of status and keys. Light is spent
// on two things only, what is alive and where the keys go; the rest is quiet.

// masthead is the one row at the top of the column: where you are, a rule of
// the wave, and the permission mode. JULIA's name leads it only when nothing
// else on the screen carries the wordmark, as the sidebar and the welcome do.
func (m *model) masthead() string {
	var left string
	if !m.hasSidebar() && !m.welcoming() {
		left = ribbon("JULIA")
	}
	if where := m.location(); where != "" {
		if left != "" {
			left += "  "
		}
		left += where
	}

	right := modeStyle(m.cfg.Mode).Render("● " + sanitize(m.cfg.Mode))
	if m.cfg.Demo {
		right = violetStyle.Render("◈ PREVIEW")
	}

	// The rule runs between the two, two cells clear of each.
	rule := m.mainWidth - ansi.StringWidth(left) - ansi.StringWidth(right) - 2
	if left != "" {
		rule -= 2
	}
	if rule < 3 {
		return fit(left, right, m.mainWidth)
	}
	if left != "" {
		left += "  "
	}
	return left + m.waveRule(rule) + "  " + right
}

// location is the workspace as the masthead names it: the directory's name in
// full weight and, where there is room, the path that leads to it before it.
func (m *model) location() string {
	if m.cfg.Session == nil || m.cfg.Session.Directory == "" || m.width < 45 {
		return ""
	}
	parent, name := filepath.Split(homeRelative(filepath.Clean(m.cfg.Session.Directory)))
	name = creamStyle.Bold(true).Render(ansi.Truncate(sanitize(name), 24, "…"))
	if m.width < 90 {
		return name
	}

	// A long path keeps its end, which is the part that tells workspaces apart.
	const longest = 28
	parent = sanitize(parent)
	if over := ansi.StringWidth(parent) - longest; over > 0 {
		parent = ansi.TruncateLeft(parent, over+1, "…")
	}
	return subtleStyle.Render(parent) + name
}

// homeRelative writes a path inside the home directory with ~ for the home.
func homeRelative(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" || home == string(filepath.Separator) {
		return path
	}
	if rest, inside := strings.CutPrefix(path, home+string(filepath.Separator)); inside {
		return "~" + string(filepath.Separator) + rest
	}
	return path
}

// modeStyle colors the permission mode: green while something reviews what
// runs, rose for yolo, which nothing does.
func modeStyle(mode string) lipgloss.Style {
	if mode == "yolo" {
		return roseStyle
	}
	return lipgloss.NewStyle().Foreground(green)
}

// pulseFrames is how many frames of the activity animation the masthead's
// pulse takes to cross the rule.
const pulseFrames = 80

// waveRule is the masthead's rule, width cells of the wave sunk halfway into
// the page. While JULIA works, a pulse of its full light travels along it.
func (m *model) waveRule(width int) string {
	var b strings.Builder
	for i := range width {
		keep := 0.5
		if m.busy {
			pulse := float64(m.spinner.frame%pulseFrames) / pulseFrames * float64(width)
			distance := (float64(i) - pulse) / 9
			keep += 0.5 * math.Exp(-distance*distance)
		}
		b.WriteString(lipgloss.NewStyle().Foreground(sink(logoColor(i, width), background, keep)).Render("─"))
	}
	return b.String()
}

// composerChrome is the composer's rows besides the draft's: its top edge and
// the caption row under it.
const composerChrome = 2

// composerPadding is the cells between the composer's side and its text.
func (m *model) composerPadding() int {
	if m.width < 60 {
		return 1
	}
	return 2
}

// composerLit reports whether the composer holds the keyboard. A question, a
// review, an approval or the fleet window takes the keys, and the light with
// them.
func (m *model) composerLit() bool {
	return m.pending == nil && m.question == nil && m.fleetView == nil
}

// composerCaption says what the composer is doing when that is not simply
// taking your message, and names the images attached to the draft.
func (m *model) composerCaption(width int) string {
	word, note := ink(panel, muted), ink(panel, subtle)
	var caption string
	switch {
	case m.pending != nil:
		caption = ink(panel, amber).Render("Approval · y / Enter")
	case m.question != nil && m.question.review != nil:
		caption = word.Render("Draft") + note.Render(" · kept while you review")
	case m.question != nil:
		caption = word.Render("Draft") + note.Render(" · kept while you answer")
	case m.fleetView != nil && m.mainWidth < 48:
		caption = word.Render("Draft") + note.Render(" · fleet open")
	case m.fleetView != nil:
		caption = word.Render("Draft") + note.Render(" · kept while you inspect the fleet")
	case m.picker != nil && m.mainWidth < 44:
		caption = word.Render("Search") + note.Render(" · draft kept")
	case m.picker != nil:
		caption = word.Render("Search") + note.Render(" · your message draft is kept")
	}
	if m.picker == nil && m.pending == nil {
		caption += m.attachmentChips(max(0, width-ansi.StringWidth(caption)))
	}
	return ansi.Truncate(caption, width, "…")
}

// composer is the card you write in: a top edge, a caption row and the draft.
// While it holds the keyboard its edge is a hairline of the wave, whose light
// falls one row into the card; otherwise the edge is dark.
func (m *model) composer() []string {
	width, pad := m.mainWidth, m.composerPadding()
	inner := max(1, width-2*pad)
	side := ink(panel, cream).Render(strings.Repeat(" ", pad))

	edge := ""
	if m.composerLit() {
		edge = ribbonOn(background, strings.Repeat("▁", width))
	}

	under := side + paint(panel, inner, "") + side
	if caption := m.composerCaption(inner); caption != "" {
		under = side + paint(panel, inner, caption) + side
	} else if m.composerLit() {
		under = glow(width)
	}

	draft := m.input.View()
	if m.picker != nil {
		draft = m.picker.search.View() + "\n"
	}
	rows := []string{edge, under}
	for _, line := range strings.Split(draft, "\n") {
		rows = append(rows, side+paint(panel, inner, line)+side)
	}
	return rows
}

// glow is the light a lit edge throws into its card: a row whose upper half
// keeps a tenth of the wave and whose lower half keeps less.
func glow(width int) string {
	var b strings.Builder
	for i := range width {
		light := logoColor(i, width)
		b.WriteString(lipgloss.NewStyle().Foreground(sink(light, panel, 0.10)).Background(sink(light, panel, 0.04)).Render("▀"))
	}
	return b.String()
}

func (m *model) bottomView() string {
	rows, _ := m.bottomLayout()
	return strings.Join(rows, "\n")
}

// bottomLayout is the rows below the conversation, and the index among them
// of the queue strip's first row, -1 when nothing is queued.
func (m *model) bottomLayout() ([]string, int) {
	var rows []string
	if m.height >= 28 {
		rows = append(rows, "")
	}

	queueAt := -1
	if strip := m.queueStrip(); len(strip) > 0 {
		queueAt = len(rows)
		rows = append(rows, strip...)
	}

	rows = append(rows, m.composer()...)
	rows = append(rows, m.statusRow())
	if m.height >= 28 {
		rows = append(rows, "")
	}
	return rows, queueAt
}

// statusRow is the one row under the composer: what JULIA is doing at the
// left and, at the right, the keys that matter now, which give way when both
// do not fit. A narrow row cannot hold both at all: there the keys of an
// approval, a picker or the fleet say more than that it is open.
func (m *model) statusRow() string {
	if m.mainWidth < 60 && (m.pending != nil || m.picker != nil || m.fleetView != nil) {
		return m.footer(m.mainWidth)
	}
	status := m.statusLine()
	return fit(status, m.footer(m.mainWidth-ansi.StringWidth(status)-2), m.mainWidth)
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
		status += faintStyle.Render(" · ") + subtleStyle.Render(elapsed(m.lastDuration))
	}
	if m.pending != nil {
		status = lipgloss.NewStyle().Foreground(amber).Render("◆ Waiting for your approval")
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

// footer is the right of the status row, fitted to width cells: the keys of
// whatever holds the keyboard. A screen with neither sidebar nor welcome
// shows the sidebar's two vitals there instead of the composer's own keys,
// which /help lists.
func (m *model) footer(width int) string {
	switch {
	case m.question != nil && m.pending == nil:
		return keys(width, "ctrl+c", "stops the task", "wheel", "scrolls the conversation")
	case m.pending != nil && width < 44:
		return keys(width, "y/enter", "allow", "others", "deny")
	case m.pending != nil:
		return keys(width, "y / enter", strings.ToLower(allowLabel(m.pending.Approval)), "any other key", "denies", "wheel", "scrolls")
	case m.fleetView != nil:
		return keys(width, "↑↓", "agent", "esc", "close", "ctrl+t", "thinking", "pgup/pgdn", "scroll", "ctrl+o", "expand")
	case m.picker != nil && width < 40:
		return keys(width, "↑↓ enter", "choose", "esc", "back")
	case m.picker != nil && m.picker.kind == "reasoning":
		return keys(width, "↑↓", "choose", "enter", "apply", "esc", "back", "tab", "models")
	case m.picker != nil:
		return keys(width, "↑↓", "choose", "enter", "apply", "esc", "back")
	case !m.hasSidebar() && !m.welcoming():
		// Keep the fleet reachable and show how to restore a hidden sidebar.
		var hints []string
		if len(m.fleet) > 0 {
			hints = append(hints, "ctrl+f", "fleet")
		}
		if m.sidebarHidden && m.width >= 108 {
			hints = append(hints, "ctrl+s", "sidebar")
		}
		shortcuts := keys(width, hints...)
		if shortcuts == "" {
			return m.vitals(width)
		}
		if vitals := m.vitals(width - ansi.StringWidth(shortcuts) - 3); vitals != "" {
			return shortcuts + "   " + vitals
		}
		return shortcuts
	}

	send := "send"
	if m.busy {
		send = "queue"
	}
	return keys(width, "enter", send, "/", "commands", "alt+enter", "newline", "esc", "interrupt", "ctrl+s", "sidebar")
}

// keys sets key hints in a row, each key in muted and what it does in subtle,
// so the row is the quietest text on the screen. The pairs are given most
// important first: those that do not fit in width are left out, and the rest
// stand in that order.
func keys(width int, pairs ...string) string {
	var row string
	for i := 0; i+1 < len(pairs); i += 2 {
		hint := mutedStyle.Render(pairs[i]) + " " + subtleStyle.Render(pairs[i+1])
		if row != "" {
			hint = "   " + hint
		}
		if ansi.StringWidth(row)+ansi.StringWidth(hint) > width {
			break
		}
		row += hint
	}
	return row
}

// vitals is the sidebar's two essentials for a screen without one: the model
// with its reasoning effort, and the context gauge with its percentage. With
// little room the model goes first, then the gauge.
func (m *model) vitals(width int) string {
	window, used := m.contextWindow(), max(m.usage().Context, 0)
	percent := creamStyle.Render(fmt.Sprintf("%d%%", contextPercent(used, window)))
	gauge := waveGauge(used, window, 10, background) + " " + percent

	name := ""
	if m.cfg.Session != nil {
		name = m.cfg.Session.Model
		if name == "" {
			name = "provider default"
		}
		if showsReasoning(m.cfg.Session.Provider, m.cfg.NamedProviders) {
			name += " · " + reasoningDisplay(m.cfg.Session.Provider, m.cfg.Session.Reasoning, m.cfg.NamedProviders)
		}
	}
	for _, row := range []string{subtleStyle.Render(sanitize(name)) + "  " + gauge, gauge, percent} {
		if ansi.StringWidth(row) <= width {
			return row
		}
	}
	return ""
}

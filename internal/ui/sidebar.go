package ui

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"julia/internal/config"
	"julia/internal/core"
)

// The sidebar is JULIA's instrument panel: one surface the height of the
// screen, without a frame, the wordmark at its head. It shows what changes as
// the session runs. Where you are and what is permitted stand in the masthead.

const (
	sidebarCells = 24               // text cells on a row
	sidebarWidth = sidebarCells + 4 // the card: two cells of padding at each side
)

// wantsSidebar reports whether the screen should carry the sidebar: it is wide
// enough, a conversation has begun, and the user has not hidden it. Before the
// first message the screen is a poster without instruments.
func (m *model) wantsSidebar() bool {
	return !m.sidebarHidden && m.width >= 108 && len(m.entries) > 0
}

// hasSidebar reports whether the current layout carries the sidebar, as
// resize last measured it.
func (m *model) hasSidebar() bool { return m.sidebarShown }

// overSidebar reports whether a screen cell falls inside the sidebar column.
func (m *model) overSidebar(x, y int) bool {
	return m.hasSidebar() && x >= m.margin()+m.mainWidth+2 && y >= 0 && y < m.height
}

// sidebarHeight is the card's rows between its two half-row edges.
func (m *model) sidebarHeight() int { return max(1, m.height-2) }

// scrollSidebar moves the sidebar by delta rows; sidebar clamps the offset.
func (m *model) scrollSidebar(delta int) {
	m.sidebarOffset = max(0, m.sidebarOffset+delta)
	m.sidebar()
}

func (m *model) sidebar() string {
	card := paletteOn(panel)

	rows := append([]string{""}, logoLettering(panel)...)
	for _, section := range [][]string{
		m.contextSection(card, sidebarCells),
		m.modelSection(card, sidebarCells),
		m.quotaSection(card, sidebarCells),
		m.sessionSection(card, sidebarCells),
		m.fleetSection(card, sidebarCells),
	} {
		if len(section) > 0 {
			rows = append(append(rows, "", ""), section...)
		}
	}

	// The session and version close the card, on its last row but one when
	// there is room to spare. A sidebar taller than the card scrolls inside it
	// under the mouse wheel.
	height := m.sidebarHeight()
	for len(rows) < height-2 {
		rows = append(rows, "")
	}
	rows = append(rows, m.sessionLine(card, sidebarCells), "")
	m.sidebarOffset = max(0, min(m.sidebarOffset, len(rows)-height))
	rows = rows[m.sidebarOffset:]
	if len(rows) > height {
		rows = rows[:height]
	}

	// The card's top and bottom edges are half a row tall, drawn on the page.
	edge := lipgloss.NewStyle().Foreground(panel)
	side := card.text.Render("  ")
	out := []string{edge.Render(strings.Repeat("▄", sidebarWidth))}
	for _, row := range rows {
		out = append(out, side+paint(panel, sidebarCells, row)+side)
	}
	out = append(out, edge.Render(strings.Repeat("▀", sidebarWidth)))
	return strings.Join(out, "\n")
}

// heading is a section's first row: its label, and a figure or a key at the
// right when it has one.
func (p palette) heading(label string, right string, width int) string {
	gap := max(1, width-ansi.StringWidth(label)-ansi.StringWidth(right))
	return p.label(label) + p.text.Render(strings.Repeat(" ", gap)) + right
}

// modelSection is the MODEL section: the model, then its provider with the
// reasoning effort beside it, or under it when the two do not fit one row,
// then the profile when one is active.
func (m *model) modelSection(p palette, width int) []string {
	provider, name, reasoning := "—", "provider default", ""
	if m.cfg.Session != nil {
		provider, reasoning = m.cfg.Session.Provider, m.cfg.Session.Reasoning
		if m.cfg.Session.Model != "" {
			name = m.cfg.Session.Model
		}
	}
	fit := func(style lipgloss.Style, s string) string {
		return style.Render(ansi.Truncate(sanitize(s), width, "…"))
	}

	rows := []string{p.label("MODEL"), fit(p.text, name), fit(p.muted, provider)}
	if showsReasoning(provider, m.cfg.NamedProviders) {
		effort := sanitize(reasoningDisplay(provider, reasoning, m.cfg.NamedProviders))
		if ansi.StringWidth(sanitize(provider))+3+ansi.StringWidth(effort) <= width {
			rows[2] += p.faint.Render(" · ") + p.muted.Render(effort)
		} else {
			rows = append(rows, p.subtle.Render("effort ")+p.muted.Render(ansi.Truncate(effort, max(1, width-7), "…")))
		}
	}
	if m.cfg.Profile != "" {
		rows = append(rows, p.subtle.Render("profile ")+p.muted.Render(ansi.Truncate(sanitize(m.cfg.Profile), max(1, width-8), "…")))
	}
	return rows
}

// quotaSection is the QUOTA LEFT section: the account's allowance windows,
// which only the CLI providers report, and the 5-hour one only Claude Code.
func (m *model) quotaSection(p palette, width int) []string {
	if m.cfg.Session == nil || !config.IsCLIProvider(m.cfg.Session.Provider) {
		return nil
	}
	rows := []string{p.label("QUOTA LEFT"), m.quotaRow(p, "week", m.accountUsage.weekly, width)}
	if m.cfg.Session.Provider == "claude" {
		rows = append(rows, m.quotaRow(p, "5 hour", m.accountUsage.fiveHour, width))
	}
	return rows
}

// sessionSection is the SESSION section: the tokens the session has used,
// how the input splits into cache hits and everything else (fresh tokens and
// cache writes), and the tool calls made.
func (m *model) sessionSection(p palette, width int) []string {
	total := m.usage().Total
	dot := p.faint.Render(" · ")
	figure := func(n, unit string) string { return p.text.Render(n) + p.muted.Render(" "+unit) }

	tokens := figure(core.CompactTokens(total.Prompt()), "in") + dot + figure(core.CompactTokens(total.Output), "out")
	split := p.muted.Render(compactWhole(total.CacheRead)+" cached") + dot + p.muted.Render(compactWhole(total.Input+total.CacheWrite)+" new")
	calls := fmt.Sprintf("%d tool calls", m.toolCount)
	if m.toolCount == 1 {
		calls = "1 tool call"
	}
	return []string{p.label("SESSION"), ansi.Truncate(tokens, width, "…"), ansi.Truncate(split, width, "…"), p.muted.Render(calls)}
}

// fleetShown is the most agents the sidebar lists; the fleet window has them all.
const fleetShown = 6

// fleetSection is the FLEET section: the latest agents, oldest first, each
// with its state, its role and what it is doing or how long it took.
func (m *model) fleetSection(p palette, width int) []string {
	if len(m.fleet) == 0 {
		return nil
	}
	shown := m.fleet[max(0, len(m.fleet)-fleetShown):]
	roleWidth := 0
	for _, a := range shown {
		roleWidth = max(roleWidth, ansi.StringWidth(oneLine(a.role)))
	}
	roleWidth = min(roleWidth, 9)

	rows := []string{p.heading("FLEET", p.subtle.Render("ctrl+f"), width)}
	room := max(1, width-roleWidth-4)
	for _, a := range shown {
		icon, detail := p.green.Render("✓"), m.elapsedAgent(a)
		switch a.state {
		case agentRunning:
			// What it is doing matters more than for how long.
			doing := strings.TrimPrefix(oneLine(a.activity), "Running ")
			if timed := doing + " · " + detail; ansi.StringWidth(timed) <= room {
				doing = timed
			}
			icon, detail = p.violet.Render("◌"), doing
		case agentFailed:
			icon, detail = p.rose.Render("!"), "failed"
		}
		role := ansi.Truncate(oneLine(a.role), roleWidth, "…")
		role += strings.Repeat(" ", roleWidth-ansi.StringWidth(role))
		rows = append(rows, icon+p.text.Render(" "+role+"  ")+p.muted.Render(ansi.Truncate(detail, room, "…")))
	}
	if earlier := len(m.fleet) - len(shown); earlier > 0 {
		rows = append(rows, p.subtle.Render(fmt.Sprintf("+%d earlier", earlier)))
	}
	return rows
}

// sessionLine names the session and the version of JULIA that runs it.
func (m *model) sessionLine(p palette, width int) string {
	id := "new conversation"
	if m.cfg.Session != nil && m.cfg.Session.ID != "" {
		id = m.cfg.Session.ID
	}
	version := strings.Join(strings.Fields(m.cfg.Version), " ")
	if version == "" {
		return p.subtle.Render(ansi.Truncate(sanitize(id), width, "…"))
	}
	version = "v" + sanitize(version)
	return p.subtle.Render(ansi.Truncate(sanitize(id), max(1, width-ansi.StringWidth(version)-3), "…")) + p.faint.Render(" · ") + p.subtle.Render(version)
}

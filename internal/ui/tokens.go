package ui

import (
	"fmt"
	"image/color"
	"math"
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"julia/internal/core"
)

// usage is what the sidebar reports: the session's persisted usage plus whatever
// the running task has reported so far. The session only receives a run's usage
// when the stream finishes, at which point m.live is cleared, so the two halves
// never overlap and replacing the session (/clear, a new conversation, resume)
// can never leave a stale figure behind.
func (m *model) usage() core.SessionUsage {
	var u core.SessionUsage
	if m.cfg.Session != nil {
		u = m.cfg.Session.Usage
	}
	u.Total.Add(m.live.Total)
	if m.live.Context > 0 {
		u.Context = m.live.Context
	}
	if m.live.ContextWindow > 0 {
		u.ContextWindow = m.live.ContextWindow
	}
	return u
}

// contextWindow prefers the provider's reported capacity. Configuration is a
// fallback until the provider reports a window, or for providers that never do.
func (m *model) contextWindow() int {
	if window := m.usage().ContextWindow; window > 0 {
		return window
	}
	if m.cfg.ContextWindowFor != nil && m.cfg.Session != nil {
		if window := m.cfg.ContextWindowFor(m.cfg.Session.Provider, m.cfg.Session.Model); window > 0 {
			return window
		}
	}
	if m.cfg.ContextWindow > 0 {
		return m.cfg.ContextWindow
	}
	return core.DefaultContextWindow
}

// resetCounters clears what the sidebar tracks per conversation. Token totals
// live in the session, so replacing the session already resets them; only the
// in-flight accumulator and the tool count need clearing here.
func (m *model) resetCounters() {
	m.toolCount = 0
	m.live = core.SessionUsage{}
	m.lastDuration = 0
	m.fleet, m.fleetView = nil, nil
}

// recordUsage notes one model call. An aggregate (a subagent's whole spend) is
// no call at all: it only adds to the totals, and says nothing of the context.
func (m *model) recordUsage(event core.Event) {
	m.live.Total.Add(event.Usage())
	if event.Aggregate {
		return
	}
	if event.ContextTokens > 0 {
		m.live.Context = event.ContextTokens
	}
	if event.ContextWindow > 0 {
		m.live.ContextWindow = event.ContextWindow
	}
}

// palette is the set of text styles for one surface, each carrying the
// surface's background, so a style reset inside a row never lets the page
// show through a card.
type palette struct {
	surface                                                             color.Color
	text, muted, subtle, faint, violet, coral, cyan, green, amber, rose lipgloss.Style
}

func paletteOn(bg color.Color) palette {
	return palette{
		surface: bg,
		text:    ink(bg, cream), muted: ink(bg, muted), subtle: ink(bg, subtle), faint: ink(bg, faint),
		violet: ink(bg, violet), coral: ink(bg, coral), cyan: ink(bg, cyan), green: ink(bg, green), amber: ink(bg, amber), rose: ink(bg, rose),
	}
}

// label is a section heading: quiet, so that the values under it are what
// the eye meets.
func (p palette) label(s string) string { return p.subtle.Render(s) }

// contextSection is the CONTEXT section: the percentage beside the label, a
// gauge filled with the wave, and "used of window". The figure is never
// clamped, only the gauge is. The gauge needs no warning color of its own,
// since its tip warms as the window fills; at or above the compaction
// threshold the figures turn amber too, since the next API turn will
// summarize the history.
func (m *model) contextSection(p palette, width int) []string {
	window, used := m.contextWindow(), max(m.usage().Context, 0)
	figure, amount, limit := p.text, p.muted, p.subtle
	if used >= core.CompactionThreshold(window) {
		figure, amount, limit = p.amber, p.amber, p.amber
	}
	percent := figure.Bold(true).Render(fmt.Sprintf("%d%%", contextPercent(used, window)))
	text := amount.Render(compactWhole(used)) + limit.Render(" of "+compactWhole(window))
	return []string{p.heading("CONTEXT", percent, width), waveGauge(used, window, width, p.surface), ansi.Truncate(text, width, "…")}
}

// contextPercent is how full the context window is, rounded down and never
// clamped at 100: a context over its window says by how much.
func contextPercent(used, window int) int {
	return int(math.Min(math.Floor(float64(used)*100/float64(window)), 99_999))
}

// compactWhole is core.CompactTokens without any decimal below a million, for rows
// where width matters more than precision: 138k, 256k, 1.3M.
func compactWhole(n int) string {
	n = max(n, 0)
	if n < 1000 {
		return strconv.Itoa(n)
	}
	if k := core.RoundDiv(n, 1000); k < 1000 {
		return strconv.Itoa(k) + "k"
	}
	t := core.RoundDiv(n, 100_000)
	if t%10 == 0 {
		return fmt.Sprintf("%dM", t/10)
	}
	return fmt.Sprintf("%d.%dM", t/10, t%10)
}

// gaugeCells is how many of width cells used/window fills. Any non-zero usage
// shows at least one cell, and usage beyond the window fills the gauge without
// overflowing.
func gaugeCells(used, window, width int) int {
	switch {
	case used <= 0:
		return 0
	case window <= 0 || used >= window:
		return width
	}
	return min(max(1, core.RoundDiv(used*width, window)), width)
}

// usageBar draws used/window across width cells as a line.
func usageBar(used, window, width int, filled, empty lipgloss.Style) string {
	n := gaugeCells(used, window, width)
	return filled.Render(strings.Repeat("━", n)) + empty.Render(strings.Repeat("─", width-n))
}

// waveGauge draws used/window across width cells as a bar half a row tall,
// filled with the wave: each cell has the color of its place along it, so the
// bar's tip is violet in a roomy window and amber in a full one.
func waveGauge(used, window, width int, surface color.Color) string {
	n := gaugeCells(used, window, width)
	var b strings.Builder
	for i := range width {
		fill := color.Color(ghost)
		if i < n {
			fill = logoColor(i, width)
		}
		b.WriteString(ink(surface, fill).Render("▄"))
	}
	return b.String()
}

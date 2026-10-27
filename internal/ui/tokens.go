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
	return u
}

// contextWindow is the budget the context gauge is measured against: the
// session's model's own window when the configuration gives it one, else the
// window every other model is assumed to have.
func (m *model) contextWindow() int {
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
// in-flight accumulator and the last-call figures need clearing here.
func (m *model) resetCounters() {
	m.toolCount, m.inputTokens, m.outputTokens = 0, 0, 0
	m.live = core.SessionUsage{}
	m.lastDuration = 0
	m.fleet, m.fleetView = nil, nil
}

// recordUsage notes one model call. A call that reports nothing (a pure context
// update after tool results) must not blank the last-call figures, and an
// aggregate (a subagent's whole spend) is no call at all: it only adds to the totals.
func (m *model) recordUsage(event core.Event) {
	u := event.Usage()
	if !u.IsZero() && !event.Aggregate {
		m.inputTokens, m.outputTokens = u.Prompt(), u.Output
	}
	m.live.Total.Add(u)
	if event.ContextTokens > 0 {
		m.live.Context = event.ContextTokens
	}
}

// palette is the set of text styles for one surface, each carrying the
// surface's background, so a style reset inside a row never lets the page
// show through a card.
type palette struct{ text, muted, faint, violet, coral, cyan, green lipgloss.Style }

func paletteOn(bg color.Color) palette {
	return palette{text: ink(bg, cream), muted: ink(bg, muted), faint: ink(bg, faint), violet: ink(bg, violet), coral: ink(bg, coral), cyan: ink(bg, cyan), green: ink(bg, green)}
}

// label is a section heading.
func (p palette) label(s string) string { return p.violet.Bold(true).Render(s) }

// contextSection is the CONTEXT section: a usage bar and "NN% · used / window".
// The figure is never clamped, only the bar is. At or above the compaction
// threshold both turn coral, since the next API turn will summarize the history.
func (m *model) contextSection(p palette, width int) []string {
	window, used := m.contextWindow(), max(m.usage().Context, 0)
	barStyle, textStyle := p.violet, p.muted
	if used >= core.CompactionThreshold(window) {
		barStyle, textStyle = p.coral, p.coral
	}
	percent := int(math.Min(math.Floor(float64(used)*100/float64(window)), 99_999))
	text := fmt.Sprintf("%d%% · %s / %s", percent, compactWhole(used), compactWhole(window))
	return []string{p.label("CONTEXT"), usageBar(used, window, width, barStyle, p.faint), textStyle.Render(ansi.Truncate(text, width, "…"))}
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

// usageBar draws used/window across width cells. Any non-zero usage shows at
// least one cell, and usage beyond the window fills the bar without overflowing.
func usageBar(used, window, width int, filled, empty lipgloss.Style) string {
	n := 0
	switch {
	case used <= 0:
	case window <= 0 || used >= window:
		n = width
	default:
		n = min(max(1, core.RoundDiv(used*width, window)), width)
	}
	return filled.Render(strings.Repeat("━", n)) + empty.Render(strings.Repeat("─", width-n))
}

// tokenSection is the TOKENS section: session totals, then how the input splits
// into cache hits and everything else (fresh tokens and cache writes).
func (m *model) tokenSection(p palette, width int) []string {
	total := m.usage().Total
	line := p.text.Render(ansi.Truncate(core.CompactTokens(total.Prompt())+" in · "+core.CompactTokens(total.Output)+" out", width, "…"))
	split := p.muted.Render(ansi.Truncate(compactWhole(total.CacheRead)+" cached · "+compactWhole(total.Input+total.CacheWrite)+" new", width, "…"))
	return []string{p.label("TOKENS"), line, split}
}

package ui

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"jarvis/internal/core"
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

// contextWindow is the budget the context gauge is measured against. Every model
// is assumed to have the same window, so this is configuration, not a model fact.
func (m *model) contextWindow() int {
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

// sidebarBlock is one sidebar section. Variants run from the fullest rendering
// to the most compact; each begins with the section label, so a section never
// shows as an anonymous figure under its neighbour. Rank orders survival on a
// short terminal: rank 1 is dropped last.
type sidebarBlock struct {
	rank     int
	variants [][]string
}

// fitSidebar picks, for each block in display order, the variant to show. Blocks
// are admitted by rank at their most compact variant until one does not fit, so a
// short terminal loses the least valuable sections first and never keeps a
// low-value section while dropping a more valuable one. Spare rows then go to
// detail, again by rank: each block takes the fullest variant that still fits, so
// a section's detail never crowds out another section. Blocks are separated by a
// blank row, and a spare row at the top is kept blank.
func fitSidebar(blocks []sidebarBlock, height int) []string {
	order := make([]int, len(blocks))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int { return cmp.Compare(blocks[a].rank, blocks[b].rank) })
	chosen := make([]int, len(blocks)) // index of the variant shown, or -1
	for i := range chosen {
		chosen[i] = -1
	}
	rows := func(i, variant int) int { return len(blocks[i].variants[variant]) }
	used := 0
	var admitted []int
	for _, i := range order {
		compact := -1
		for v := range blocks[i].variants {
			if rows(i, v) > 0 {
				compact = v
			}
		}
		if compact < 0 {
			continue
		}
		separator := 0
		if used > 0 {
			separator = 1
		}
		if used+separator+rows(i, compact) > height {
			break
		}
		chosen[i], used = compact, used+separator+rows(i, compact)
		admitted = append(admitted, i)
	}
	for _, i := range admitted {
		for v := 0; v < chosen[i]; v++ {
			if grown := used - rows(i, chosen[i]) + rows(i, v); rows(i, v) > 0 && grown <= height {
				chosen[i], used = v, grown
				break
			}
		}
	}
	var lines []string
	if used < height {
		lines = append(lines, "")
	}
	first := true
	for i, v := range chosen {
		if v < 0 {
			continue
		}
		if !first {
			lines = append(lines, "")
		}
		first = false
		lines = append(lines, blocks[i].variants[v]...)
	}
	return lines
}

// contextVariants is the CONTEXT section: a usage bar and "NN% · used / window".
// The figure is never clamped, only the bar is. At or above the compaction
// threshold both turn coral, since the next API turn will summarize the history.
func (m *model) contextVariants(label func(string) string, width int) [][]string {
	window, used := m.contextWindow(), max(m.usage().Context, 0)
	barStyle, textStyle := violetStyle, mutedStyle
	if used >= core.CompactionThreshold(window) {
		barStyle, textStyle = coralStyle, coralStyle
	}
	percent := int(math.Min(math.Floor(float64(used)*100/float64(window)), 99_999))
	text := fmt.Sprintf("%d%% · %s / %s", percent, compactWhole(used), compactWhole(window))
	text = textStyle.Render(ansi.Truncate(text, width, "…"))
	bar := usageBar(used, window, width, barStyle)
	// Every variant keeps the label: a bare figure under CONNECTION reads as part of
	// that section. Admission is by rank at the most compact variant, so a gauge that
	// offered a two-row form would lose its bar whenever lower-ranked sections soak up
	// the spare rows; only a genuinely short sidebar gets that form. A one-row sidebar
	// has no room for the label, and then the figure alone beats nothing.
	switch {
	case m.bodyHeight < 2:
		return [][]string{{text}}
	case m.bodyHeight < 8:
		return [][]string{{label("CONTEXT"), bar, text}, {label("CONTEXT"), text}}
	}
	return [][]string{{label("CONTEXT"), bar, text}}
}

// usageBar draws used/window across width cells. Any non-zero usage shows at
// least one cell, and usage beyond the window fills the bar without overflowing.
func usageBar(used, window, width int, filledStyle lipgloss.Style) string {
	filled := 0
	switch {
	case used <= 0:
	case window <= 0 || used >= window:
		filled = width
	default:
		filled = min(max(1, core.RoundDiv(used*width, window)), width)
	}
	return filledStyle.Render(strings.Repeat("━", filled)) + faintStyle.Render(strings.Repeat("─", width-filled))
}

// tokenVariants is the TOKENS section: session totals, then how the input splits
// into cache hits and everything else (fresh tokens and cache writes).
func (m *model) tokenVariants(label func(string) string, width int) [][]string {
	total := m.usage().Total
	line := creamStyle.Render(ansi.Truncate(core.CompactTokens(total.Prompt())+" in · "+core.CompactTokens(total.Output)+" out", width, "…"))
	split := mutedStyle.Render(ansi.Truncate(compactWhole(total.CacheRead)+" cached · "+compactWhole(total.Input+total.CacheWrite)+" new", width, "…"))
	return [][]string{{label("TOKENS"), line, split}, {label("TOKENS"), line}}
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

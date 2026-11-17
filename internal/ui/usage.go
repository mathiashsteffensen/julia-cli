package ui

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"julia/internal/config"
	"julia/internal/core"
)

const (
	usageRefreshInterval = time.Minute
	usageMinimumInterval = 10 * time.Second
)

// accountUsageState holds the account's allowance windows and the lookup that
// refreshes them.
type accountUsageState struct {
	weekly      usageReading
	fiveHour    usageReading
	loading     bool
	next        time.Time
	lastAttempt time.Time
	sequence    uint64
	cancel      context.CancelFunc
}

type usageReading struct {
	value *core.UsageWindow
	stale bool
}

// markStale keeps a reading on show after an update failed, marked as old.
func (reading *usageReading) markStale() {
	reading.stale = reading.value != nil
}

type accountUsageMsg struct {
	sequence uint64
	value    *core.AccountUsage
	err      error
}

// Calls are coalesced on the UI goroutine. A completed turn can bring the next
// refresh forward, but short turns never create overlapping or rapid requests.
func (m *model) refreshAccountUsage(afterTurn bool) tea.Cmd {
	if !config.IsCLIProvider(m.cfg.Session.Provider) || m.cfg.Demo || m.cfg.AccountUsage == nil || m.quitting || m.lifetime.Err() != nil {
		return nil
	}
	if afterTurn {
		due := m.now
		if earliest := m.accountUsage.lastAttempt.Add(usageMinimumInterval); earliest.After(due) {
			due = earliest
		}
		if m.accountUsage.next.IsZero() || due.Before(m.accountUsage.next) {
			m.accountUsage.next = due
		}
	}
	if m.accountUsage.loading || m.now.Before(m.accountUsage.next) {
		return nil
	}
	m.accountUsage.loading = true
	m.accountUsage.lastAttempt = m.now
	m.accountUsage.next = m.now.Add(usageRefreshInterval)
	m.accountUsage.sequence++
	sequence := m.accountUsage.sequence
	ctx, cancel := context.WithTimeout(m.lifetime, 15*time.Second)
	m.accountUsage.cancel = cancel
	fetch, provider := m.cfg.AccountUsage, m.cfg.Session.Provider
	return func() tea.Msg {
		defer cancel()
		value, err := fetch(ctx, provider)
		if value != nil {
			copy := *value
			copy.Weekly = copyUsage(value.Weekly)
			copy.FiveHour = copyUsage(value.FiveHour)
			value = &copy
		}
		return accountUsageMsg{sequence: sequence, value: value, err: err}
	}
}

func copyUsage(value *core.UsageWindow) *core.UsageWindow {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func (m *model) receiveAccountUsage(msg accountUsageMsg) {
	if msg.sequence != m.accountUsage.sequence || !m.accountUsage.loading || !config.IsCLIProvider(m.cfg.Session.Provider) || m.quitting || m.lifetime.Err() != nil {
		return
	}
	m.accountUsage.loading = false
	m.accountUsage.cancel = nil
	if msg.err != nil {
		m.accountUsage.weekly.markStale()
		m.accountUsage.fiveHour.markStale()
		return
	}
	if msg.value == nil {
		msg.value = &core.AccountUsage{}
	}
	m.accountUsage.weekly.receive(msg.value.Weekly)
	m.accountUsage.fiveHour.receive(msg.value.FiveHour)
}

func (reading *usageReading) receive(value *core.UsageWindow) {
	if value == nil {
		reading.value, reading.stale = nil, false
		return
	}
	if math.IsNaN(value.RemainingPercent) || math.IsInf(value.RemainingPercent, 0) || value.RemainingPercent < 0 || value.RemainingPercent > 100 {
		reading.markStale()
		return
	}
	reading.value, reading.stale = value, false
}

func (m *model) cancelAccountUsage() {
	if m.accountUsage.cancel != nil {
		m.accountUsage.cancel()
		m.accountUsage.cancel = nil
	}
	m.accountUsage.loading = false
	m.accountUsage.sequence++
}

// A provider or profile change invalidates both the readings and any request
// started for the previous account. The next tick starts a fresh lookup.
func (m *model) resetAccountUsage() {
	m.cancelAccountUsage()
	m.accountUsage.weekly, m.accountUsage.fiveHour = usageReading{}, usageReading{}
	m.accountUsage.next = time.Time{}
	m.accountUsage.lastAttempt = time.Time{}
}

// outdated reports whether a reading is too old to trust: an update failed after
// it, or its window has since reset.
func (m *model) outdated(reading usageReading) bool {
	return reading.stale || (!reading.value.ResetsAt.IsZero() && !m.now.Before(reading.value.ResetsAt))
}

// quotaRow is one allowance window on the sidebar: its name, a line of what
// is left, and the percentage. The line turns amber under a quarter and rose
// under a tenth. A window that is loading, unknown or stale says so in words
// instead.
func (m *model) quotaRow(p palette, name string, reading usageReading, width int) string {
	const nameWidth, figureWidth = 8, 5 // "5 hour  " and " 100%"
	label := p.muted.Render(name + strings.Repeat(" ", nameWidth-ansi.StringWidth(name)))
	if reading.value == nil {
		if m.accountUsage.loading {
			return label + p.muted.Render("loading…")
		}
		return label + p.subtle.Render("unavailable")
	}

	left := reading.value.RemainingPercent
	figure := fmt.Sprintf("%.0f%%", left)
	if m.outdated(reading) {
		return label + p.muted.Render(figure+" · stale")
	}

	line := ink(p.surface, sink(violet, p.surface, 0.7))
	switch {
	case left < 10:
		line = p.rose
	case left < 25:
		line = p.amber
	}
	bar := usageBar(int(math.Round(left)), 100, width-nameWidth-figureWidth, line, ink(p.surface, ghost))
	return label + bar + p.text.Render(strings.Repeat(" ", figureWidth-ansi.StringWidth(figure))+figure)
}

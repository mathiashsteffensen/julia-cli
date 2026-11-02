package ui

import (
	"context"
	"fmt"
	"math"
	"time"

	tea "charm.land/bubbletea/v2"
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

func (m *model) weeklyUsageLine(p palette) string {
	return m.usageLine(p, "Weekly", m.accountUsage.weekly)
}

func (m *model) fiveHourUsageLine(p palette) string {
	return m.usageLine(p, "5-hour", m.accountUsage.fiveHour)
}

func (m *model) usageLine(p palette, label string, reading usageReading) string {
	if reading.value == nil {
		if m.accountUsage.loading {
			return p.muted.Render(label + " · loading…")
		}
		return p.faint.Render(label + " · unavailable")
	}
	value := reading.value
	stale := reading.stale || (!value.ResetsAt.IsZero() && !m.now.Before(value.ResetsAt))
	if stale {
		return p.muted.Render(fmt.Sprintf("%s · %.0f%% · stale", label, value.RemainingPercent))
	}
	return p.violet.Render(fmt.Sprintf("%s · %.0f%% left", label, value.RemainingPercent))
}

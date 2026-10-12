package ui

import (
	"context"
	"fmt"
	"math"
	"time"

	tea "charm.land/bubbletea/v2"
	"julia/internal/core"
)

const (
	weeklyRefreshInterval = time.Minute
	weeklyMinimumInterval = 10 * time.Second
)

type weeklyUsageState struct {
	value       *core.WeeklyUsage
	loading     bool
	stale       bool
	next        time.Time
	lastAttempt time.Time
	sequence    uint64
	cancel      context.CancelFunc
}

type weeklyUsageMsg struct {
	sequence uint64
	value    *core.WeeklyUsage
	err      error
}

// Calls are coalesced on the UI goroutine. A completed turn can bring the next
// refresh forward, but short turns never create overlapping or rapid requests.
func (m *model) refreshWeeklyUsage(afterTurn bool) tea.Cmd {
	if m.cfg.Session.Provider != "codex" || m.cfg.Demo || m.cfg.WeeklyUsage == nil || m.quitting || m.lifetime.Err() != nil {
		return nil
	}
	if afterTurn {
		due := m.now
		if earliest := m.weekly.lastAttempt.Add(weeklyMinimumInterval); earliest.After(due) {
			due = earliest
		}
		if m.weekly.next.IsZero() || due.Before(m.weekly.next) {
			m.weekly.next = due
		}
	}
	if m.weekly.loading || m.now.Before(m.weekly.next) {
		return nil
	}
	m.weekly.loading = true
	m.weekly.lastAttempt = m.now
	m.weekly.next = m.now.Add(weeklyRefreshInterval)
	m.weekly.sequence++
	sequence := m.weekly.sequence
	ctx, cancel := context.WithTimeout(m.lifetime, 15*time.Second)
	m.weekly.cancel = cancel
	fetch := m.cfg.WeeklyUsage
	return func() tea.Msg {
		defer cancel()
		value, err := fetch(ctx)
		if value != nil {
			copy := *value
			value = &copy
		}
		return weeklyUsageMsg{sequence: sequence, value: value, err: err}
	}
}

func (m *model) receiveWeeklyUsage(msg weeklyUsageMsg) {
	if msg.sequence != m.weekly.sequence || !m.weekly.loading || m.cfg.Session.Provider != "codex" || m.quitting || m.lifetime.Err() != nil {
		return
	}
	m.weekly.loading = false
	m.weekly.cancel = nil
	if msg.err != nil {
		m.weekly.stale = m.weekly.value != nil
		return
	}
	if msg.value == nil {
		m.weekly.value, m.weekly.stale = nil, false
		return
	}
	if math.IsNaN(msg.value.RemainingPercent) || math.IsInf(msg.value.RemainingPercent, 0) || msg.value.RemainingPercent < 0 || msg.value.RemainingPercent > 100 {
		m.weekly.stale = m.weekly.value != nil
		return
	}
	copy := *msg.value
	m.weekly.value = &copy
	m.weekly.stale = false
}

func (m *model) cancelWeeklyUsage() {
	if m.weekly.cancel != nil {
		m.weekly.cancel()
		m.weekly.cancel = nil
	}
	m.weekly.loading = false
	m.weekly.sequence++
}

func (m *model) weeklyUsageLine() string {
	if m.weekly.value == nil {
		if m.weekly.loading {
			return mutedStyle.Render("Weekly · loading…")
		}
		return faintStyle.Render("Weekly · unavailable")
	}
	value := m.weekly.value
	stale := m.weekly.stale || (!value.ResetsAt.IsZero() && !m.now.Before(value.ResetsAt))
	if stale {
		return mutedStyle.Render(fmt.Sprintf("Weekly · %.0f%% · stale", value.RemainingPercent))
	}
	return violetStyle.Render(fmt.Sprintf("Weekly · %.0f%% left", value.RemainingPercent))
}

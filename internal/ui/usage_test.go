package ui

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"julia/internal/core"
)

func usageConfig(fetch func(context.Context) (*core.WeeklyUsage, error)) Config {
	return Config{Session: &core.Session{ID: "usage", Provider: "codex", Model: "selected", Reasoning: "medium"}, WeeklyUsage: fetch}
}

func TestWeeklyUsageOnlyReadsForMainCodexProvider(t *testing.T) {
	for _, provider := range []string{"openai", "anthropic", "google", "compatible", "codex"} {
		calls := 0
		cfg := usageConfig(func(context.Context) (*core.WeeklyUsage, error) {
			calls++
			return &core.WeeklyUsage{RemainingPercent: 61}, nil
		})
		cfg.Session.Provider = provider
		m := testModel(t, cfg)
		m.resize(128, 40)
		cmd := m.refreshWeeklyUsage(false)
		if provider == "codex" {
			if cmd == nil || !strings.Contains(ansi.Strip(m.sidebar()), "Weekly · loading") {
				t.Fatal("missing Codex loading state")
			}
			m.Update(cmd())
			if calls != 1 || !strings.Contains(ansi.Strip(m.sidebar()), "Weekly · 61% left") {
				t.Fatal("Codex allowance missing")
			}
		} else {
			if cmd != nil || calls != 0 || strings.Contains(ansi.Strip(m.sidebar()), "Weekly") {
				t.Fatalf("%s exposed or requested Codex reviewer quota", provider)
			}
			m.now = m.now.Add(2 * time.Minute)
			if m.refreshWeeklyUsage(true) != nil {
				t.Fatal("API turn triggered Codex allowance polling")
			}
		}
	}
}

func TestWeeklyUsageZeroUnavailableAndStale(t *testing.T) {
	value := &core.WeeklyUsage{RemainingPercent: 0}
	var fetchErr error
	m := testModel(t, usageConfig(func(context.Context) (*core.WeeklyUsage, error) { return value, fetchErr }))
	m.Update(m.refreshWeeklyUsage(false)())
	if got := ansi.Strip(m.weeklyUsageLine()); got != "Weekly · 0% left" {
		t.Fatalf("zero treated as unknown: %q", got)
	}
	fetchErr = errors.New("temporary failure")
	m.now = m.now.Add(time.Minute)
	m.Update(m.refreshWeeklyUsage(false)())
	if got := ansi.Strip(m.weeklyUsageLine()); got != "Weekly · 0% · stale" {
		t.Fatalf("failed refresh hid stale state: %q", got)
	}
	fetchErr, value = nil, &core.WeeklyUsage{RemainingPercent: 72, ResetsAt: m.now.Add(time.Hour)}
	m.now = m.now.Add(time.Minute)
	m.Update(m.refreshWeeklyUsage(false)())
	if got := ansi.Strip(m.weeklyUsageLine()); got != "Weekly · 72% left" {
		t.Fatalf("success did not clear stale state: %q", got)
	}
	m.now = value.ResetsAt
	if !strings.Contains(ansi.Strip(m.weeklyUsageLine()), "stale") {
		t.Fatal("reset time silently invented a fresh allowance")
	}
	value = nil
	m.Update(m.refreshWeeklyUsage(false)())
	if got := ansi.Strip(m.weeklyUsageLine()); got != "Weekly · unavailable" {
		t.Fatalf("successful missing-quota response retained old account allowance: %q", got)
	}
	for _, unavailable := range []*core.WeeklyUsage{nil, {RemainingPercent: math.NaN()}, {RemainingPercent: math.Inf(1)}, {RemainingPercent: -1}, {RemainingPercent: 101}} {
		unknown := testModel(t, usageConfig(func(context.Context) (*core.WeeklyUsage, error) { return unavailable, nil }))
		unknown.Update(unknown.refreshWeeklyUsage(false)())
		if got := ansi.Strip(unknown.weeklyUsageLine()); got != "Weekly · unavailable" {
			t.Fatalf("invalid or missing reading inferred a percentage: %q", got)
		}
	}
}

func TestWeeklyUsageCoalescesInitialPeriodicAndCompletedTurnRequests(t *testing.T) {
	calls := 0
	m := testModel(t, usageConfig(func(context.Context) (*core.WeeklyUsage, error) {
		calls++
		return &core.WeeklyUsage{RemainingPercent: 61}, nil
	}))
	initial := m.refreshWeeklyUsage(false)
	if initial == nil {
		t.Fatal("initial request missing")
	}
	for range 3 {
		if m.refreshWeeklyUsage(false) != nil || m.refreshWeeklyUsage(true) != nil {
			t.Fatal("overlapping request")
		}
	}
	m.Update(initial())
	if m.refreshWeeklyUsage(true) != nil {
		t.Fatal("completed turn ignored minimum interval")
	}
	m.now = m.now.Add(weeklyMinimumInterval)
	refresh := m.refreshWeeklyUsage(false)
	if refresh == nil {
		t.Fatal("coalesced completed-turn refresh was lost")
	}
	m.Update(refresh())
	m.now = m.now.Add(weeklyRefreshInterval - time.Second)
	if m.refreshWeeklyUsage(false) != nil {
		t.Fatal("periodic refresh fired early")
	}
	m.now = m.now.Add(time.Second)
	periodic := m.refreshWeeklyUsage(false)
	if periodic == nil {
		t.Fatal("periodic refresh missing")
	}
	m.Update(periodic())
	if calls != 3 {
		t.Fatalf("got %d requests", calls)
	}
}

func TestCompletedTurnSchedulesAllowanceRefresh(t *testing.T) {
	m := testModel(t, usageConfig(func(context.Context) (*core.WeeklyUsage, error) { return nil, nil }))
	m.Update(m.refreshWeeklyUsage(false)())
	m.busy = true
	m.Update(streamMsg{finished: true})
	if want := m.weekly.lastAttempt.Add(weeklyMinimumInterval); !m.weekly.next.Equal(want) {
		t.Fatal("turn completion did not bring refresh forward")
	}
}

func TestQuitCancelsAllowanceReadAndIgnoresLateResults(t *testing.T) {
	for _, busy := range []bool{false, true} {
		started, stopped := make(chan struct{}), make(chan struct{})
		m := testModel(t, usageConfig(func(ctx context.Context) (*core.WeeklyUsage, error) {
			close(started)
			<-ctx.Done()
			close(stopped)
			return &core.WeeklyUsage{RemainingPercent: 99}, nil
		}))
		m.busy = busy
		cmd := m.refreshWeeklyUsage(false)
		result := make(chan tea.Msg, 1)
		go func() { result <- cmd() }()
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("usage request did not start")
		}
		m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
		select {
		case <-stopped:
		case <-time.After(time.Second):
			t.Fatal("exit did not cancel metadata read")
		}
		m.Update(<-result)
		if m.weekly.value != nil || m.weekly.loading {
			t.Fatal("late result changed quitting UI")
		}
		m.now = m.now.Add(2 * time.Minute)
		if m.refreshWeeklyUsage(false) != nil {
			t.Fatal("quitting UI restarted polling")
		}
	}
}

func TestWeeklyUsageDoesNotMutateConversationOrProviderValue(t *testing.T) {
	value := &core.WeeklyUsage{RemainingPercent: 61}
	m := testModel(t, usageConfig(func(context.Context) (*core.WeeklyUsage, error) { return value, nil }))
	m.input.SetValue("keep draft")
	m.entries = []entry{{role: "user", text: "keep transcript"}}
	before, _ := json.Marshal(m.cfg.Session)
	msg := m.refreshWeeklyUsage(false)()
	value.RemainingPercent = 12
	m.Update(msg)
	after, _ := json.Marshal(m.cfg.Session)
	if string(before) != string(after) || m.input.Value() != "keep draft" || len(m.entries) != 1 || m.entries[0].text != "keep transcript" || m.weekly.value.RemainingPercent != 61 {
		t.Fatal("metadata read shared provider storage or changed conversation state")
	}
}

func TestWeeklyUsageDemoIsOfflineAndFitsLayouts(t *testing.T) {
	for _, size := range [][2]int{{30, 15}, {50, 20}, {128, 15}, {128, 16}, {128, 18}, {128, 20}, {128, 40}} {
		cfg := usageConfig(func(context.Context) (*core.WeeklyUsage, error) {
			t.Fatal("demo requested account usage")
			return nil, nil
		})
		cfg.Demo = true
		m := testModel(t, cfg)
		m.resize(size[0], size[1])
		if m.refreshWeeklyUsage(false) != nil || m.refreshWeeklyUsage(true) != nil {
			t.Fatal("demo scheduled account usage read")
		}
		view := m.View().Content
		// The allowance sits under the masthead, so a short sidebar scrolls to it.
		if strings.Contains(ansi.Strip(view), "Weekly · 61% left") && !m.hasSidebar() {
			t.Fatalf("%v: allowance visibility differs from sidebar", size)
		}
		lines := strings.Split(view, "\n")
		if len(lines) > size[1] {
			t.Fatal("allowance overflowed terminal height")
		}
		for _, line := range lines {
			if ansi.StringWidth(line) > size[0] {
				t.Fatal("allowance overflowed terminal width")
			}
		}
	}
	if !strings.Contains(ansi.Strip(RenderDemo(128, 40)), "Weekly · 61% left") {
		t.Fatal("snapshot demo does not use actual Codex provider gating")
	}
}

func TestReviewerOutcomePrecedesEscalationDetails(t *testing.T) {
	m := testModel(t, Config{})
	m.pending = &ApprovalRequest{Approval: core.Approval{ExecutionMode: "unsandboxed", Reason: "Run outside the sandbox? Automatic review deferred or denied: Missing authorization for this command.", Justification: strings.Repeat("Long justification. ", 40), Call: core.ToolCall{Name: "Bash", Arguments: json.RawMessage(`{"command":"go test ./..."}`)}}}
	view := ansi.Strip(m.approvalView())
	if strings.Index(view, "Missing authorization") > strings.Index(view, "OUTSIDE SANDBOX") {
		t.Fatal("reviewer denial is buried beneath generic escalation disclosure")
	}
}

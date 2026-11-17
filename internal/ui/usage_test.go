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

func usageConfig(fetch func(context.Context) (*core.UsageWindow, error)) Config {
	return Config{
		Session: &core.Session{ID: "usage", Provider: "codex", Model: "selected", Reasoning: "medium"},
		AccountUsage: func(ctx context.Context, _ string) (*core.AccountUsage, error) {
			weekly, err := fetch(ctx)
			return &core.AccountUsage{Weekly: weekly}, err
		},
	}
}

// sidebarWords is the sidebar as its words, one row per line: styling and
// gauge lines removed, each run of spaces as one. A quota row reads "week 61%".
func sidebarWords(m *model) string {
	return words(m.sidebar())
}

// weekly and fiveHour are a window's quota row as its words: "week 61%",
// "5 hour 0% · stale", "week unavailable".
func weekly(m *model) string {
	return words(m.quotaRow(paletteOn(background), "week", m.accountUsage.weekly, sidebarCells))
}

func fiveHour(m *model) string {
	return words(m.quotaRow(paletteOn(background), "5 hour", m.accountUsage.fiveHour, sidebarCells))
}

func words(rows string) string {
	var out []string
	for _, row := range strings.Split(strings.NewReplacer("━", "", "─", "").Replace(ansi.Strip(rows)), "\n") {
		out = append(out, strings.Join(strings.Fields(row), " "))
	}
	return strings.Join(out, "\n")
}

func TestWeeklyUsageOnlyReadsForMainCodexProvider(t *testing.T) {
	for _, provider := range []string{"openai", "anthropic", "google", "openrouter", "codex"} {
		calls := 0
		cfg := usageConfig(func(context.Context) (*core.UsageWindow, error) {
			calls++
			return &core.UsageWindow{RemainingPercent: 61}, nil
		})
		cfg.Session.Provider = provider
		m := testModel(t, cfg)
		m.resize(128, 40)
		cmd := m.refreshAccountUsage(false)
		if provider == "codex" {
			if cmd == nil || !strings.Contains(sidebarWords(m), "week loading…") {
				t.Fatal("missing Codex loading state")
			}
			m.Update(cmd())
			if calls != 1 || !strings.Contains(sidebarWords(m), "week 61%") {
				t.Fatal("Codex allowance missing")
			}
		} else {
			if cmd != nil || calls != 0 || strings.Contains(sidebarWords(m), "QUOTA LEFT") {
				t.Fatalf("%s exposed or requested Codex reviewer quota", provider)
			}
			m.now = m.now.Add(2 * time.Minute)
			if m.refreshAccountUsage(true) != nil {
				t.Fatal("API turn triggered Codex allowance polling")
			}
		}
	}
}

func TestWeeklyUsageZeroUnavailableAndStale(t *testing.T) {
	value := &core.UsageWindow{RemainingPercent: 0}
	var fetchErr error
	m := testModel(t, usageConfig(func(context.Context) (*core.UsageWindow, error) { return value, fetchErr }))
	m.Update(m.refreshAccountUsage(false)())
	if got := weekly(m); got != "week 0%" {
		t.Fatalf("zero treated as unknown: %q", got)
	}
	fetchErr = errors.New("temporary failure")
	m.now = m.now.Add(time.Minute)
	m.Update(m.refreshAccountUsage(false)())
	if got := weekly(m); got != "week 0% · stale" {
		t.Fatalf("failed refresh hid stale state: %q", got)
	}
	fetchErr, value = nil, &core.UsageWindow{RemainingPercent: 72, ResetsAt: m.now.Add(time.Hour)}
	m.now = m.now.Add(time.Minute)
	m.Update(m.refreshAccountUsage(false)())
	if got := weekly(m); got != "week 72%" {
		t.Fatalf("success did not clear stale state: %q", got)
	}
	m.now = value.ResetsAt
	if !strings.Contains(weekly(m), "stale") {
		t.Fatal("reset time silently invented a fresh allowance")
	}
	value = nil
	m.Update(m.refreshAccountUsage(false)())
	if got := weekly(m); got != "week unavailable" {
		t.Fatalf("successful missing-quota response retained old account allowance: %q", got)
	}
	for _, unavailable := range []*core.UsageWindow{nil, {RemainingPercent: math.NaN()}, {RemainingPercent: math.Inf(1)}, {RemainingPercent: -1}, {RemainingPercent: 101}} {
		unknown := testModel(t, usageConfig(func(context.Context) (*core.UsageWindow, error) { return unavailable, nil }))
		unknown.Update(unknown.refreshAccountUsage(false)())
		if got := weekly(unknown); got != "week unavailable" {
			t.Fatalf("invalid or missing reading inferred a percentage: %q", got)
		}
	}
}

func TestWeeklyUsageCoalescesInitialPeriodicAndCompletedTurnRequests(t *testing.T) {
	calls := 0
	m := testModel(t, usageConfig(func(context.Context) (*core.UsageWindow, error) {
		calls++
		return &core.UsageWindow{RemainingPercent: 61}, nil
	}))
	initial := m.refreshAccountUsage(false)
	if initial == nil {
		t.Fatal("initial request missing")
	}
	for range 3 {
		if m.refreshAccountUsage(false) != nil || m.refreshAccountUsage(true) != nil {
			t.Fatal("overlapping request")
		}
	}
	m.Update(initial())
	if m.refreshAccountUsage(true) != nil {
		t.Fatal("completed turn ignored minimum interval")
	}
	m.now = m.now.Add(usageMinimumInterval)
	refresh := m.refreshAccountUsage(false)
	if refresh == nil {
		t.Fatal("coalesced completed-turn refresh was lost")
	}
	m.Update(refresh())
	m.now = m.now.Add(usageRefreshInterval - time.Second)
	if m.refreshAccountUsage(false) != nil {
		t.Fatal("periodic refresh fired early")
	}
	m.now = m.now.Add(time.Second)
	periodic := m.refreshAccountUsage(false)
	if periodic == nil {
		t.Fatal("periodic refresh missing")
	}
	m.Update(periodic())
	if calls != 3 {
		t.Fatalf("got %d requests", calls)
	}
}

func TestCompletedTurnSchedulesAllowanceRefresh(t *testing.T) {
	m := testModel(t, usageConfig(func(context.Context) (*core.UsageWindow, error) { return nil, nil }))
	m.Update(m.refreshAccountUsage(false)())
	m.busy = true
	m.Update(streamMsg{finished: true})
	if want := m.accountUsage.lastAttempt.Add(usageMinimumInterval); !m.accountUsage.next.Equal(want) {
		t.Fatal("turn completion did not bring refresh forward")
	}
}

func TestQuitCancelsAllowanceReadAndIgnoresLateResults(t *testing.T) {
	for _, busy := range []bool{false, true} {
		started, stopped := make(chan struct{}), make(chan struct{})
		m := testModel(t, usageConfig(func(ctx context.Context) (*core.UsageWindow, error) {
			close(started)
			<-ctx.Done()
			close(stopped)
			return &core.UsageWindow{RemainingPercent: 99}, nil
		}))
		m.busy = busy
		cmd := m.refreshAccountUsage(false)
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
		if m.accountUsage.weekly.value != nil || m.accountUsage.loading {
			t.Fatal("late result changed quitting UI")
		}
		m.now = m.now.Add(2 * time.Minute)
		if m.refreshAccountUsage(false) != nil {
			t.Fatal("quitting UI restarted polling")
		}
	}
}

func TestWeeklyUsageDoesNotMutateConversationOrProviderValue(t *testing.T) {
	value := &core.UsageWindow{RemainingPercent: 61}
	m := testModel(t, usageConfig(func(context.Context) (*core.UsageWindow, error) { return value, nil }))
	m.input.SetValue("keep draft")
	m.entries = []entry{{role: "user", text: "keep transcript"}}
	before, _ := json.Marshal(m.cfg.Session)
	msg := m.refreshAccountUsage(false)()
	value.RemainingPercent = 12
	m.Update(msg)
	after, _ := json.Marshal(m.cfg.Session)
	if string(before) != string(after) || m.input.Value() != "keep draft" || len(m.entries) != 1 || m.entries[0].text != "keep transcript" || m.accountUsage.weekly.value.RemainingPercent != 61 {
		t.Fatal("metadata read shared provider storage or changed conversation state")
	}
}

func TestWeeklyUsageDemoIsOfflineAndFitsLayouts(t *testing.T) {
	for _, size := range [][2]int{{30, 15}, {50, 20}, {128, 15}, {128, 16}, {128, 18}, {128, 20}, {128, 40}} {
		cfg := usageConfig(func(context.Context) (*core.UsageWindow, error) {
			t.Fatal("demo requested account usage")
			return nil, nil
		})
		cfg.Demo = true
		m := testModel(t, cfg)
		m.resize(size[0], size[1])
		if m.refreshAccountUsage(false) != nil || m.refreshAccountUsage(true) != nil {
			t.Fatal("demo scheduled account usage read")
		}
		view := m.View().Content
		// The allowance sits under the masthead, so a short sidebar scrolls to it.
		if strings.Contains(words(view), "week 61%") && !m.hasSidebar() {
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
	if !strings.Contains(words(RenderDemo(128, 40)), "week 61%") {
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

func TestAccountUsageProviderVisibilityAndCopy(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		t.Run(provider, func(t *testing.T) {
			calls := 0
			usage := &core.AccountUsage{
				Weekly:   &core.UsageWindow{RemainingPercent: 61},
				FiveHour: &core.UsageWindow{RemainingPercent: 38},
			}
			cfg := Config{
				Session: &core.Session{ID: "usage", Provider: provider, Model: "selected"},
				AccountUsage: func(context.Context, string) (*core.AccountUsage, error) {
					calls++
					return usage, nil
				},
			}
			m := testModel(t, cfg)
			m.resize(128, 40)
			cmd := m.refreshAccountUsage(false)
			loading := sidebarWords(m)
			if !strings.Contains(loading, "week loading…") || (strings.Contains(loading, "5 hour loading…") != (provider == "claude")) {
				t.Fatalf("unexpected loading state: %s", loading)
			}
			msg := cmd()
			usage.Weekly.RemainingPercent = 12
			usage.FiveHour.RemainingPercent = 13
			m.Update(msg)
			view := sidebarWords(m)
			if calls != 1 || !strings.Contains(view, "week 61%") {
				t.Fatalf("weekly data or single request missing: calls=%d view=%s", calls, view)
			}
			if got := strings.Contains(view, "5 hour 38%"); got != (provider == "claude") {
				t.Fatalf("unexpected five-hour visibility: %s", view)
			}
		})
	}
}

func TestClaudeAccountUsageIndependentWindows(t *testing.T) {
	var usage *core.AccountUsage
	var fetchErr error
	m := testModel(t, Config{
		Session:      &core.Session{ID: "usage", Provider: "claude", Model: "selected"},
		AccountUsage: func(context.Context, string) (*core.AccountUsage, error) { return usage, fetchErr },
	})
	check := func(wantWeekly, wantFiveHour string) {
		t.Helper()
		if got := weekly(m); got != wantWeekly {
			t.Errorf("weekly: got %q, want %q", got, wantWeekly)
		}
		if got := fiveHour(m); got != wantFiveHour {
			t.Errorf("five-hour: got %q, want %q", got, wantFiveHour)
		}
	}
	refresh := func() {
		t.Helper()
		cmd := m.refreshAccountUsage(false)
		if cmd == nil {
			t.Fatal("expected account usage request")
		}
		m.Update(cmd())
		m.now = m.now.Add(usageRefreshInterval)
	}

	usage = &core.AccountUsage{Weekly: &core.UsageWindow{RemainingPercent: 80}, FiveHour: &core.UsageWindow{RemainingPercent: 0}}
	refresh()
	check("week 80%", "5 hour 0%")

	// An invalid window retains only its own old value; the other still updates.
	usage = &core.AccountUsage{Weekly: &core.UsageWindow{RemainingPercent: 70}, FiveHour: &core.UsageWindow{RemainingPercent: math.NaN()}}
	refresh()
	check("week 70%", "5 hour 0% · stale")
	usage = &core.AccountUsage{Weekly: &core.UsageWindow{RemainingPercent: 101}, FiveHour: &core.UsageWindow{RemainingPercent: 50}}
	refresh()
	check("week 70% · stale", "5 hour 50%")

	// A failed lookup retains both old values, even if it returns partial data.
	fetchErr = errors.New("temporary failure")
	usage = &core.AccountUsage{Weekly: &core.UsageWindow{RemainingPercent: 99}}
	refresh()
	check("week 70% · stale", "5 hour 50% · stale")

	// Successful partial responses clear only the missing window.
	fetchErr = nil
	usage = &core.AccountUsage{FiveHour: &core.UsageWindow{RemainingPercent: 42, ResetsAt: m.now.Add(2 * usageRefreshInterval)}}
	refresh()
	check("week unavailable", "5 hour 42%")
	usage.Weekly = &core.UsageWindow{RemainingPercent: 60}
	refresh()
	check("week 60%", "5 hour 42% · stale")
	usage = &core.AccountUsage{Weekly: &core.UsageWindow{RemainingPercent: 33, ResetsAt: m.now.Add(usageRefreshInterval)}}
	refresh()
	check("week 33% · stale", "5 hour unavailable")
	usage = nil
	refresh()
	check("week unavailable", "5 hour unavailable")
}

func TestAccountUsageResetsOnAccountChange(t *testing.T) {
	for _, change := range []string{"provider", "profile"} {
		t.Run(change, func(t *testing.T) {
			cfg := settingsConfig()
			asked := ""
			cfg.AccountUsage = func(_ context.Context, provider string) (*core.AccountUsage, error) {
				asked = provider
				return &core.AccountUsage{Weekly: &core.UsageWindow{RemainingPercent: 61}}, nil
			}
			cfg.SwitchProfile = func(*core.Session, string) (ProfileSettings, error) {
				return ProfileSettings{}, nil
			}
			m := testModel(t, cfg)
			m.Update(m.refreshAccountUsage(false)())
			m.now = m.now.Add(usageRefreshInterval)
			oldResult := m.refreshAccountUsage(false)()
			var err error
			if change == "provider" {
				err = m.applySettings("claude", "model", "")
			} else {
				err = m.switchProfile("other")
			}
			if err != nil {
				t.Fatal(err)
			}
			m.Update(oldResult)
			if m.accountUsage.weekly.value != nil || m.accountUsage.fiveHour.value != nil {
				t.Fatal("old account usage survived the change")
			}
			refresh := m.refreshAccountUsage(false)
			if refresh == nil {
				t.Fatal("account change did not permit an immediate refresh")
			}

			// The session names the account: /model leaves the profile's provider behind.
			refresh()
			if asked != m.cfg.Session.Provider {
				t.Fatalf("asked for %q usage in a %q session", asked, m.cfg.Session.Provider)
			}
			if change == "provider" && asked != "claude" {
				t.Fatalf("asked for %q usage after the switch to claude", asked)
			}
		})
	}
}

// A preview reads nothing, so its readings must last the whole preview.
func TestDemoKeepsItsReadingsAcrossAProviderSwitch(t *testing.T) {
	m := testModel(t, Config{Session: &core.Session{ID: "usage", Provider: "claude", Model: "selected", Reasoning: "medium"}, Demo: true})
	for _, provider := range []string{"claude", "codex", "claude"} {
		if err := m.applySettings(provider, "selected", ""); err != nil {
			t.Fatal(err)
		}
		if got := weekly(m); got != "week 61%" {
			t.Fatalf("on %s: weekly = %q", provider, got)
		}
		if got := fiveHour(m); got != "5 hour 38%" {
			t.Fatalf("on %s: five-hour = %q", provider, got)
		}
	}
}

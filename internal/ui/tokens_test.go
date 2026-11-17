package ui

import (
	"context"
	"encoding/json"
	"math"
	"regexp"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"julia/internal/core"
)

func TestCompactWhole(t *testing.T) {
	for _, tc := range []struct {
		in   int
		want string
	}{
		{-1, "0"},
		{0, "0"},
		{999, "999"},
		{1000, "1k"},
		{1499, "1k"},
		{1500, "2k"},
		{118_600, "119k"},
		{138_412, "138k"},
		{256_000, "256k"},
		{999_499, "999k"},
		{999_500, "1M"},
		{1_000_000, "1M"},
		{1_250_000, "1.3M"},
		{2_000_000, "2M"},
	} {
		if got := compactWhole(tc.in); got != tc.want {
			t.Errorf("compactWhole(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if got := compactWhole(math.MaxInt); strings.HasPrefix(got, "-") {
		t.Errorf("compactWhole(MaxInt) = %q", got)
	}
}

func TestUsageBar(t *testing.T) {
	const width = 23
	for _, tc := range []struct {
		used, window, filled int
	}{
		{0, 256_000, 0},
		{-5, 256_000, 0},
		{1, 256_000, 1}, // any usage is visible
		{128_000, 256_000, 12},
		{255_999, 256_000, 23},
		{256_000, 256_000, 23},
		{900_000, 256_000, 23}, // the bar is clamped; the printed figure is not
		{5, 0, 23},
		{0, 0, 0},
		{0, -1, 0},
		{5, -1, 23},
	} {
		bar := ansi.Strip(usageBar(tc.used, tc.window, width, violetStyle, faintStyle))
		if got := strings.Count(bar, "━"); got != tc.filled || ansi.StringWidth(bar) != width {
			t.Errorf("usageBar(%d, %d) = %q, want %d filled cells of %d", tc.used, tc.window, bar, tc.filled, width)
		}
	}
}

var negativeNumber = regexp.MustCompile(`-\d`)

// sidebarText is the sidebar with styling removed, one entry per row, each run
// of spaces as one: a heading and its figure read "CONTEXT 53%".
func sidebarText(m *model) []string {
	var rows []string
	for _, row := range strings.Split(ansi.Strip(m.sidebar()), "\n") {
		rows = append(rows, strings.Join(strings.Fields(row), " "))
	}
	return rows
}

// hasContext reports whether the CONTEXT section reads percent over amount,
// as in "53%" and "138k of 256k".
func hasContext(rows []string, percent, amount string) bool {
	return hasRow(rows, "CONTEXT "+percent) && hasRow(rows, amount)
}

func hasRow(rows []string, want string) bool {
	for _, row := range rows {
		if row == want {
			return true
		}
	}
	return false
}

func TestSidebarShowsContextAndSessionTotals(t *testing.T) {
	// A resumed session: the saved totals must show before any run starts.
	saved := core.SessionUsage{Total: core.TokenUsage{Input: 20_000, CacheRead: 900_000, CacheWrite: 30_000, Output: 12_300}, Context: 138_000}
	m := testModel(t, Config{Session: &core.Session{ID: "resumed", Directory: "/tmp/julia", Provider: "fake", Usage: saved}})
	m.resize(128, 60)
	rows := sidebarText(m)
	for _, want := range []string{"CONTEXT 53%", "138k of 256k", "SESSION", "950k in · 12.3k out", "900k cached · 50k new", "0 tool calls"} {
		if !hasRow(rows, want) {
			t.Errorf("sidebar is missing %q:\n%s", want, strings.Join(rows, "\n"))
		}
	}
	m.applyEvent(core.UsageEvent(core.TokenUsage{Input: 1_200, CacheRead: 10_000, Output: 410}, 0))
	if rows = sidebarText(m); !hasRow(rows, "961.2k in · 12.7k out") {
		t.Errorf("a model call did not add to the totals:\n%s", strings.Join(rows, "\n"))
	}
}

func TestSubagentAggregateAddsToTotalsButNotToTheContext(t *testing.T) {
	m := testModel(t, Config{Session: &core.Session{ID: "s", Provider: "fake"}})
	m.resize(128, 60)
	m.applyEvent(core.UsageEvent(core.TokenUsage{Input: 1_200, CacheRead: 10_000, Output: 410}, 12_000))
	// A subagent's whole spend arrives as one event, far larger than any single call.
	m.applyEvent(core.AggregateUsageEvent(core.TokenUsage{Input: 200_000, CacheRead: 1_000_000, Output: 40_000}))
	rows := sidebarText(m)
	if !hasRow(rows, "1.2M in · 40.4k out") || !hasContext(rows, "4%", "12k of 256k") {
		t.Errorf("the aggregate must add to the totals and leave the context alone:\n%s", strings.Join(rows, "\n"))
	}
}

func TestSidebarZeroState(t *testing.T) {
	m := testModel(t, Config{})
	m.resize(128, 60)
	rows := sidebarText(m)
	for _, want := range []string{"CONTEXT 0%", "0 of 256k", "0 in · 0 out", "0 cached · 0 new", "0 tool calls"} {
		if !hasRow(rows, want) {
			t.Errorf("zero state is missing %q:\n%s", want, strings.Join(rows, "\n"))
		}
	}
	if negativeNumber.MatchString(strings.Join(rows, "\n")) {
		t.Error("zero state rendered a negative number")
	}
}

func TestContextWindowIsConfigurable(t *testing.T) {
	for _, tc := range []struct {
		window          int
		percent, amount string
	}{
		{0, "50%", "128k of 256k"},
		{-100, "50%", "128k of 256k"},
		{core.DefaultContextWindow, "50%", "128k of 256k"},
		{512_000, "25%", "128k of 512k"},
		{1_000_000, "12%", "128k of 1M"},
	} {
		session := &core.Session{ID: "w", Provider: "fake", Usage: core.SessionUsage{Context: 128_000}}
		m := testModel(t, Config{Session: session, ContextWindow: tc.window})
		m.resize(128, 40)
		if rows := sidebarText(m); !hasContext(rows, tc.percent, tc.amount) {
			t.Errorf("window %d: want %s, %s in\n%s", tc.window, tc.percent, tc.amount, strings.Join(rows, "\n"))
		}
	}
}

func TestProviderContextWindowReplacesTheFallback(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		t.Run(provider, func(t *testing.T) {
			session := &core.Session{ID: "window", Provider: provider, Model: "large"}
			m := testModel(t, Config{Session: session, ContextWindow: 256_000, ContextWindowFor: func(string, string) int { return 200_000 }})
			m.resize(128, 40)
			m.applyEvent(core.UsageEvent(core.TokenUsage{Input: 300_000, Output: 10_000}, 310_000))
			// Claude reports the window separately, at the end of the turn.
			m.applyEvent(core.Event{Kind: "usage", ContextWindow: 1_000_000})
			if rows := sidebarText(m); !hasContext(rows, "31%", "310k of 1M") {
				t.Fatalf("provider window was ignored:\n%s", strings.Join(rows, "\n"))
			}
		})
	}
}

// The gauge follows the session's model: a model with a window of its own is
// measured against it, and one without falls back to the configured window.
func TestContextWindowFollowsTheSessionModel(t *testing.T) {
	windows := map[string]int{"openrouter/vendor/large": 1_000_000, "codex/gpt-6-astra": 400_000}
	session := &core.Session{ID: "w", Provider: "openrouter", Model: "vendor/large", Usage: core.SessionUsage{Context: 100_000}}
	m := testModel(t, Config{Session: session, ContextWindow: 200_000, ContextWindowFor: func(provider, model string) int {
		return windows[provider+"/"+model]
	}})
	m.resize(128, 40)
	for _, tc := range []struct {
		provider, model string
		percent, amount string
	}{
		{"openrouter", "vendor/large", "10%", "100k of 1M"},
		{"codex", "gpt-6-astra", "25%", "100k of 400k"},
		{"openrouter", "vendor/plain", "50%", "100k of 200k"},
	} {
		session.Provider, session.Model = tc.provider, tc.model
		if rows := sidebarText(m); !hasContext(rows, tc.percent, tc.amount) {
			t.Errorf("%s/%s: want %s, %s in\n%s", tc.provider, tc.model, tc.percent, tc.amount, strings.Join(rows, "\n"))
		}
	}
	if got := m.contextWindow(); got != 200_000 {
		t.Fatalf("a model without a window of its own must use the configured one, got %d", got)
	}
	m.cfg.ContextWindow = 0
	if got := m.contextWindow(); got != core.DefaultContextWindow {
		t.Fatalf("without any configured window the default applies, got %d", got)
	}
}

// usageBackend records usage into the session before emitting it, as the real
// providers do, so the session it returns already holds every event of the run.
type usageBackend struct{ events []core.Event }

func (usageBackend) Name() string { return "fake" }
func (b usageBackend) Run(_ context.Context, s *core.Session, _ core.Prompt, _ core.ToolExecutor, emit core.Emit) error {
	for _, event := range b.events {
		s.RecordUsage(event)
		emit(event)
	}
	return nil
}

func nextStreamMsg(t *testing.T, m *model) streamMsg {
	t.Helper()
	select {
	case msg := <-m.stream:
		return msg
	case <-time.After(2 * time.Second):
		t.Fatal("no stream message")
		return streamMsg{}
	}
}

func TestRunUsageShowsLiveAndIsCountedOnce(t *testing.T) {
	prior := core.SessionUsage{Total: core.TokenUsage{Input: 1000, CacheRead: 4000, CacheWrite: 500, Output: 300}, Context: 5500}
	events := []core.Event{
		core.UsageEvent(core.TokenUsage{Input: 200, CacheRead: 5500, Output: 80}, 6000),
		core.UsageEvent(core.TokenUsage{Input: 50, CacheRead: 5700, CacheWrite: 400, Output: 120}, 6600),
		core.UsageEvent(core.TokenUsage{}, 7000), // a context update after tool results
	}
	session := &core.Session{ID: "live", Directory: "/tmp/julia", Provider: "fake", Usage: prior}
	m := testModel(t, Config{Session: session, Backend: usageBackend{events}})
	m.resize(128, 60)
	m.input.SetValue("go")
	m.submit()

	want := prior
	for i, event := range events {
		msg := nextStreamMsg(t, m)
		if msg.event == nil || msg.finished {
			t.Fatalf("event %d: got %+v", i, msg)
		}
		m.Update(msg)
		want.Total.Add(event.Usage())
		want.Context = event.ContextTokens
		if got := m.usage(); got != want {
			t.Fatalf("after event %d: displayed %+v, want %+v", i, got, want)
		}
		if m.cfg.Session.Usage != prior {
			t.Fatalf("after event %d: the run leaked into the session before it finished: %+v", i, m.cfg.Session.Usage)
		}
	}
	if rows := sidebarText(m); !hasContext(rows, "2%", "7k of 256k") || !hasRow(rows, "17.4k in · 500 out") {
		t.Errorf("live sidebar is stale:\n%s", strings.Join(rows, "\n"))
	}

	msg := nextStreamMsg(t, m)
	if !msg.finished {
		t.Fatalf("expected the run to finish, got %+v", msg)
	}
	m.Update(msg)
	if got := m.usage(); got != want {
		t.Errorf("finished run double counted or lost usage: displayed %+v, want %+v", got, want)
	}
	if m.cfg.Session.Usage != want || m.live != (core.SessionUsage{}) {
		t.Errorf("finished run left state behind: session %+v, live %+v", m.cfg.Session.Usage, m.live)
	}
	// A second run starts from the settled totals.
	m.input.SetValue("again")
	m.submit()
	m.Update(nextStreamMsg(t, m))
	want.Total.Add(events[0].Usage())
	want.Context = events[0].ContextTokens
	if got := m.usage(); got != want {
		t.Errorf("second run: displayed %+v, want %+v", got, want)
	}
	for m.busy {
		m.Update(nextStreamMsg(t, m))
	}
}

func TestReportedWindowSurvivesRunCompletionAndResume(t *testing.T) {
	events := []core.Event{
		core.UsageEvent(core.TokenUsage{Input: 300_000, Output: 10_000}, 310_000),
		{Kind: "usage", ContextWindow: 1_000_000},
	}
	session := &core.Session{ID: "window", Provider: "claude", Model: "large"}
	m := testModel(t, Config{Session: session, Backend: usageBackend{events}})
	m.input.SetValue("go")
	m.submit()
	for m.busy {
		m.Update(nextStreamMsg(t, m))
	}
	if m.contextWindow() != 1_000_000 || m.live != (core.SessionUsage{}) {
		t.Fatalf("run completion lost the window: %+v / %+v", session.Usage, m.live)
	}
	data, err := json.Marshal(session)
	if err != nil {
		t.Fatal(err)
	}
	var resumed core.Session
	if err := json.Unmarshal(data, &resumed); err != nil {
		t.Fatal(err)
	}
	next := testModel(t, Config{Session: &resumed})
	next.resize(128, 40)
	if rows := sidebarText(next); !hasContext(rows, "31%", "310k of 1M") {
		t.Fatalf("resumed session lost the provider window:\n%s", strings.Join(rows, "\n"))
	}
}

func TestUnrelatedUsageDoesNotChangeTheContextWindow(t *testing.T) {
	m := testModel(t, Config{Session: &core.Session{
		ID: "window", Provider: "claude", Model: "large",
		Usage: core.SessionUsage{Context: 310_000, ContextWindow: 1_000_000},
	}})
	for _, event := range []core.Event{
		{Kind: "usage"},
		{Kind: "usage", ContextWindow: -1},
		{Kind: "usage", Aggregate: true, ContextWindow: 200_000},
		{Kind: "usage", ContextWindow: 200_000, Subagent: &core.Subagent{ID: "child", Role: "scout"}},
	} {
		m.applyEvent(event)
		if m.contextWindow() != 1_000_000 {
			t.Fatalf("unrelated event replaced the reported window: %+v", event)
		}
	}
}

func TestFinishedRunWithoutSessionKeepsWhatWasShown(t *testing.T) {
	m := testModel(t, Config{Session: &core.Session{ID: "nil", Provider: "fake", Usage: core.SessionUsage{Total: core.TokenUsage{Input: 10}, Context: 10}}})
	m.busy = true
	m.applyEvent(core.UsageEvent(core.TokenUsage{Input: 5, CacheRead: 20, Output: 3}, 400))
	shown := m.usage()
	m.Update(streamMsg{finished: true})
	if m.usage() != shown || m.cfg.Session.Usage != shown || m.live != (core.SessionUsage{}) {
		t.Errorf("usage changed when a run finished without a session: shown %+v, now %+v / %+v", shown, m.usage(), m.live)
	}
}

func TestContextShrinksAfterCompaction(t *testing.T) {
	m := testModel(t, Config{})
	m.resize(128, 60)
	m.applyEvent(core.UsageEvent(core.TokenUsage{Input: 190_000, Output: 10_000}, 200_000))
	if rows := sidebarText(m); !hasContext(rows, "78%", "200k of 256k") {
		t.Fatalf("context did not grow:\n%s", strings.Join(rows, "\n"))
	}
	// The summarization call is billed in full, but the history it leaves is small.
	m.applyEvent(core.UsageEvent(core.TokenUsage{Input: 205_000, Output: 3_000}, 90_000))
	rows := sidebarText(m)
	if !hasContext(rows, "35%", "90k of 256k") {
		t.Errorf("context did not shrink after compaction:\n%s", strings.Join(rows, "\n"))
	}
	if !hasRow(rows, "395k in · 13k out") {
		t.Errorf("session totals must keep the compaction call:\n%s", strings.Join(rows, "\n"))
	}
}

func TestClearResetsUsage(t *testing.T) {
	session := &core.Session{ID: "old", Directory: "/tmp/julia", Provider: "fake", Usage: core.SessionUsage{Total: core.TokenUsage{Input: 1, CacheRead: 2, CacheWrite: 3, Output: 4}, Context: 50_000, ContextWindow: 1_000_000}}
	m := testModel(t, Config{Session: session})
	m.resize(128, 60)
	m.applyEvent(core.UsageEvent(core.TokenUsage{Input: 7, Output: 8}, 60_000))
	m.input.SetValue("/clear")
	m.submit()
	if m.usage() != (core.SessionUsage{}) {
		t.Fatalf("/clear kept usage: %+v", m.usage())
	}
	rows := sidebarText(m)
	for _, want := range []string{"CONTEXT 0%", "0 of 256k", "0 in · 0 out", "0 cached · 0 new"} {
		if !hasRow(rows, want) {
			t.Errorf("/clear sidebar is missing %q:\n%s", want, strings.Join(rows, "\n"))
		}
	}
}

func TestSettingsChangeInSameConversationKeepsUsage(t *testing.T) {
	m := testModel(t, Config{Demo: true, Session: &core.Session{ID: "same", Directory: "/tmp/julia", Provider: "codex", Model: "preview-balanced"}})
	m.cfg.Session.Usage.ContextWindow = 1_000_000
	before := m.usage()
	if err := m.applySettings("codex", "preview-balanced", "high"); err != nil {
		t.Fatal(err)
	}
	if m.usage() != before {
		t.Fatal("a reasoning change discarded the model's window")
	}
	if err := m.applySettings("codex", "preview-deep", ""); err != nil {
		t.Fatal(err)
	}
	want := before
	want.ContextWindow = 0
	if m.usage() != want || m.contextWindow() != core.DefaultContextWindow {
		t.Errorf("a model switch must clear only the reported window: %+v -> %+v", before, m.usage())
	}
}

func TestDemoPreviewsShowUsage(t *testing.T) {
	// Tall enough for the whole sidebar, so no section is scrolled out of view.
	demo := ansi.Strip(RenderDemo(128, 70))
	for _, want := range []string{"CONTEXT", "46%", "119k of 256k", "SESSION", "511.7k in · 6.8k out", "412k cached · 100k new"} {
		if !strings.Contains(demo, want) {
			t.Errorf("demo preview is missing %q", want)
		}
	}
	// The welcome is a poster: no instruments, which would only read zero, and
	// the model the first message will meet.
	if welcome := ansi.Strip(RenderWelcome(128, 45)); strings.Contains(welcome, "CONTEXT") || !strings.Contains(welcome, "your selected model") {
		t.Errorf("welcome preview:\n%s", welcome)
	}
	// Usage the caller supplied is never replaced by the sample figures.
	m := testModel(t, Config{Demo: true, Session: &core.Session{ID: "kept", Provider: "codex", Usage: core.SessionUsage{Context: 1000}}})
	if m.usage().Context != 1000 {
		t.Errorf("demo overwrote supplied usage: %+v", m.usage())
	}
}

// styleSequence is the escape sequence a style opens with.
func styleSequence(style lipgloss.Style) string {
	return strings.SplitN(style.Render("x"), "x", 2)[0]
}

// The gauge's own tip warms as the window fills. The figures beside it turn
// amber only at the compaction threshold, where the next turn summarizes.
func TestContextTurnsAmberAtCompactionThreshold(t *testing.T) {
	card := paletteOn(panel)
	amberOpen := styleSequence(card.amber)
	if amberOpen == "" || amberOpen == styleSequence(card.muted) {
		t.Fatalf("cannot distinguish amber output: %q", amberOpen)
	}
	for _, window := range []int{0, 100_000, 512_000} {
		effective := window
		if effective <= 0 {
			effective = core.DefaultContextWindow
		}
		threshold := core.CompactionThreshold(effective)
		for _, tc := range []struct {
			context int
			amber   bool
		}{{0, false}, {threshold - 1, false}, {threshold, true}, {threshold + 1, true}, {threshold * 2, true}} {
			session := &core.Session{ID: "c", Provider: "fake", Usage: core.SessionUsage{Context: tc.context}}
			m := testModel(t, Config{Session: session, ContextWindow: window})
			section := m.contextSection(card, sidebarCells)
			if got := strings.Contains(section[2], amberOpen); got != tc.amber {
				t.Errorf("window %d context %d: amber = %v, want %v", window, tc.context, got, tc.amber)
			}
		}
	}
	// Beyond the window the figure keeps counting while the gauge stays full.
	over := testModel(t, Config{Session: &core.Session{ID: "o", Provider: "fake", Usage: core.SessionUsage{Context: 300_000}}})
	over.resize(128, 60)
	if rows := sidebarText(over); !hasContext(rows, "117%", "300k of 256k") {
		t.Errorf("over-window context is wrong:\n%s", strings.Join(rows, "\n"))
	}
	if cells := gaugeCells(300_000, 256_000, sidebarCells); cells != sidebarCells {
		t.Errorf("over-window gauge fills %d of %d cells", cells, sidebarCells)
	}
}

// The context gauge is filled with the wave: a filled cell has the color of
// its own place, an empty one the track's.
func TestWaveGaugeFillsWithTheWave(t *testing.T) {
	track := styleSequence(ink(panel, ghost))
	for _, tc := range []struct{ used, window, filled int }{
		{0, 256_000, 0},
		{1, 256_000, 1}, // any usage is visible
		{128_000, 256_000, 12},
		{900_000, 256_000, sidebarCells},
	} {
		gauge := waveGauge(tc.used, tc.window, sidebarCells, panel)
		if got := sidebarCells - strings.Count(gauge, track); got != tc.filled || ansi.StringWidth(gauge) != sidebarCells {
			t.Errorf("waveGauge(%d, %d) fills %d cells of %d, want %d", tc.used, tc.window, got, ansi.StringWidth(gauge), tc.filled)
		}
	}
}

func TestSidebarNeverOverflowsItsColumn(t *testing.T) {
	long := strings.Repeat("界", 40)
	usages := []core.SessionUsage{
		{},
		{Total: core.TokenUsage{Input: 123_456_789, CacheRead: 987_654_321, CacheWrite: 55_555, Output: 999_999}, Context: 255_999},
		{Total: core.TokenUsage{Input: math.MaxInt / 4, CacheRead: math.MaxInt / 4, CacheWrite: math.MaxInt / 4, Output: math.MaxInt / 4}, Context: math.MaxInt},
		{Total: core.TokenUsage{Input: -5, CacheRead: -5, CacheWrite: -5, Output: -5}, Context: -10},
	}
	for _, provider := range []string{"fake", "codex", "openrouter", long} {
		for i, usage := range usages {
			for _, window := range []int{-1, 0, 1, 256_000, math.MaxInt} {
				session := &core.Session{ID: long, Directory: "/" + long, Provider: provider, Model: long, Reasoning: long, Usage: usage}
				m := testModel(t, Config{Session: session, Mode: long, ContextWindow: window, NamedProviders: map[string]bool{"openrouter": true, long: true}})
				for height := 8; height <= 70; height++ {
					m.height = height // the sidebar fills every row of the screen
					lines := strings.Split(m.sidebar(), "\n")
					if len(lines) != height {
						t.Fatalf("%.8s usage %d window %d: %d rows at height %d", provider, i, window, len(lines), height)
					}
					for _, line := range lines {
						if ansi.StringWidth(line) > sidebarWidth {
							t.Fatalf("%.8s usage %d window %d height %d: row exceeds the sidebar: %q", provider, i, window, height, ansi.Strip(line))
						}
					}
					if negativeNumber.MatchString(ansi.Strip(strings.Join(lines, "\n"))) {
						t.Fatalf("%.8s usage %d window %d height %d: negative number rendered", provider, i, window, height)
					}
				}
			}
		}
	}
	// The same figures must also fit the whole screen at awkward sizes.
	for _, size := range [][2]int{{20, 8}, {60, 20}, {107, 30}, {108, 9}, {108, 23}, {128, 40}, {160, 80}} {
		m := testModel(t, Config{Session: &core.Session{ID: "v", Provider: "codex", Usage: usages[2]}})
		m.resize(size[0], size[1])
		lines := strings.Split(m.View().Content, "\n")
		if len(lines) > size[1] {
			t.Fatalf("%v: %d rows", size, len(lines))
		}
		for _, line := range lines {
			if ansi.StringWidth(line) > size[0] {
				t.Fatalf("%v: row too wide: %q", size, ansi.Strip(line))
			}
		}
	}
}

func TestSidebarShowsEverySectionInOrder(t *testing.T) {
	m := testModel(t, Config{
		Version: "0.1.0",
		Session: &core.Session{ID: "all", Directory: "/tmp/julia", Provider: "codex", Model: "selected", Reasoning: "high", Usage: core.SessionUsage{Context: 119_000}},
	})
	m.resize(128, 24)
	m.height = 200 // room for the whole column, so nothing is scrolled out of view
	rows := sidebarText(m)
	text := strings.Join(rows, "\n")
	at := 0
	for _, want := range []string{
		"CONTEXT 46%", "119k of 256k", "MODEL", "selected", "codex · high", "QUOTA LEFT", "week",
		"SESSION", "0 cached · 0 new", "0 tool calls", "all · v0.1.0",
	} {
		i := strings.Index(text[at:], want)
		if i < 0 {
			t.Fatalf("missing %q after row offset %d:\n%s", want, at, text)
		}
		at += i + len(want)
	}
	for i, row := range rows {
		if row == "CONTEXT 46%" && !strings.HasPrefix(rows[i+1], "▄") {
			t.Errorf("the gauge lost its bar: %q", rows[i+1])
		}
	}
}

func TestSidebarScrollsIndependently(t *testing.T) {
	m := testModel(t, Config{Version: "0.1.0", Session: &core.Session{ID: "scroll", Directory: "/tmp/julia", Provider: "codex", Model: "selected"}})
	m.entries = []entry{{role: "assistant", text: "A conversation, so the sidebar is shown."}}
	m.resize(128, 16)
	if rows := sidebarText(m); len(rows) != m.height || !hasRow(rows, "CONTEXT 0%") || hasRow(rows, "scroll · v0.1.0") {
		t.Fatalf("a short sidebar should open on its head:\n%s", strings.Join(rows, "\n"))
	}
	x, y := m.margin()+m.mainWidth+4, m.height/2
	if !m.overSidebar(x, y) || m.overSidebar(m.margin()+1, y) {
		t.Fatal("sidebar hit test is wrong")
	}
	// The wheel over the conversation leaves the sidebar where it is.
	m.Update(tea.MouseWheelMsg{X: m.margin() + 1, Y: y, Button: tea.MouseWheelDown})
	if m.sidebarOffset != 0 {
		t.Fatal("the conversation's wheel scrolled the sidebar")
	}
	for range 200 {
		m.Update(tea.MouseWheelMsg{X: x, Y: y, Button: tea.MouseWheelDown})
	}
	rows := sidebarText(m)
	if len(rows) != m.height || !hasRow(rows, "scroll · v0.1.0") || hasRow(rows, "CONTEXT 0%") {
		t.Fatalf("scrolling down should end on the session line:\n%s", strings.Join(rows, "\n"))
	}
	bottom := m.sidebarOffset
	m.Update(tea.MouseWheelMsg{X: x, Y: y, Button: tea.MouseWheelUp})
	if m.sidebarOffset != bottom-1 {
		t.Fatalf("wheel up moved %d -> %d", bottom, m.sidebarOffset)
	}
	// Growing the terminal pulls the offset back so the column never shows a gap.
	m.resize(128, 200)
	if m.sidebar(); m.sidebarOffset != 0 {
		t.Fatalf("a sidebar that fits is still scrolled by %d", m.sidebarOffset)
	}
}

func TestSidebarReachesTheBottomEdge(t *testing.T) {
	for _, size := range [][2]int{{108, 20}, {128, 40}, {200, 60}} {
		m := testModel(t, Config{Session: &core.Session{ID: "edge", Provider: "codex"}})
		m.entries = []entry{{role: "assistant", text: "A conversation, so the sidebar is shown."}}
		m.resize(size[0], size[1])
		lines := strings.Split(ansi.Strip(m.View().Content), "\n")
		if len(lines) != size[1] {
			t.Fatalf("%v: %d rows", size, len(lines))
		}
		// The card's half-row edges are the screen's first and last rows, and
		// the column is laid out on every row between them.
		column := m.margin() + m.mainWidth + 2
		for y, edge := range map[int]rune{0: '▄', size[1] - 1: '▀'} {
			if cells := []rune(lines[y]); len(cells) <= column || cells[column] != edge {
				t.Fatalf("%v: row %d is not the sidebar card's edge: %q", size, y, lines[y])
			}
		}
		for y := range lines {
			if cells := []rune(lines[y]); len(cells) < column+sidebarWidth {
				t.Fatalf("%v: row %d stops short of the sidebar: %q", size, y, lines[y])
			}
		}
		if footer := ansi.StringWidth(m.footer(m.mainWidth)); footer > m.mainWidth {
			t.Fatalf("%v: footer is %d wide beside a %d-column main area", size, footer, m.mainWidth)
		}
	}
}

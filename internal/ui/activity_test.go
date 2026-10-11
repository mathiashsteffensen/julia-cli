package ui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

func TestActivityKeepsAStableFootprintAndPureFrames(t *testing.T) {
	for frame := range uint64(200) {
		a := activityModel{frame: frame}
		view := a.View()
		if view != a.View() {
			t.Fatalf("frame %d changed during a redraw", frame)
		}
		plain := ansi.Strip(view)
		if ansi.StringWidth(view) != activityWidth || len(plain) != activityWidth {
			t.Fatalf("frame %d changed the activity width: %q", frame, plain)
		}
		for _, char := range plain {
			if char != '.' && !strings.ContainsRune(activityGlyphs, char) {
				t.Fatalf("frame %d contains an unexpected glyph: %q", frame, char)
			}
		}
	}
	settled := uint64(time.Second / activityInterval)
	first := (activityModel{frame: settled}).View()
	next := (activityModel{frame: settled + 1}).View()
	if ansi.Strip(first) == ansi.Strip(next) {
		t.Fatal("the glyph stream did not advance")
	}
}

func TestActivityScramblesInsteadOfScrolling(t *testing.T) {
	a := activityModel{generation: 1, frame: uint64(time.Second / activityInterval)}
	previous := ansi.Strip(a.View())
	matched, compared := 0, 0
	for range 100 {
		a.frame++
		current := ansi.Strip(a.View())
		for column := 0; column < activityWidth-1; column++ {
			if current[column] == previous[column+1] {
				matched++
			}
			compared++
		}
		previous = current
	}
	// A moving tape preserves almost every neighboring glyph. Independent
	// scrambling can repeat a glyph by chance, but cannot preserve most of it.
	if matched*2 >= compared {
		t.Fatalf("activity still reads as a scrolling tape: %d/%d shifted glyphs preserved", matched, compared)
	}
}

func TestActivityRevealsItsGlyphsProgressively(t *testing.T) {
	var a activityModel
	a.start()
	settled := uint64(time.Second / activityInterval)
	previousDots := strings.Count(ansi.Strip(a.View()), ".")
	if previousDots == 0 {
		t.Fatal("activity appeared fully formed instead of revealing from dots")
	}
	partialFrames := 0
	for frame := uint64(1); frame <= settled; frame++ {
		a.frame = frame
		dots := strings.Count(ansi.Strip(a.View()), ".")
		if dots > previousDots {
			t.Fatalf("frame %d turned revealed glyphs back into dots", frame)
		}
		if dots > 0 && dots < activityWidth {
			partialFrames++
		}
		previousDots = dots
	}
	if partialFrames < 2 {
		t.Fatal("activity revealed its glyphs at once instead of staggering them")
	}
	if previousDots != 0 {
		t.Fatalf("activity still has %d unrevealed dots after one second", previousDots)
	}
}

func TestActivityNewRunsHaveFreshGlyphs(t *testing.T) {
	var a activityModel
	a.start()
	a.frame = uint64(time.Second / activityInterval)
	first := ansi.Strip(a.View())
	a.stop()
	a.start()
	a.frame = uint64(time.Second / activityInterval)
	if next := ansi.Strip(a.View()); next == first {
		t.Fatalf("a new task replayed the same settled glyphs: %q", next)
	}
}

func TestActivityStopsAndIgnoresTicksFromPreviousTasks(t *testing.T) {
	var a activityModel
	if cmd := a.update(activityTickMsg{frame: 1}); cmd != nil {
		t.Fatal("idle animation scheduled a timer")
	}
	firstTick := a.start()().(activityTickMsg)
	a.stop()
	if cmd := a.update(firstTick); cmd != nil {
		t.Fatal("completed task continued animating")
	}
	a.start()
	if cmd := a.update(firstTick); cmd != nil || a.frame != 0 {
		t.Fatal("a previous task's timer advanced the new animation")
	}
	currentTick := activityTickMsg{generation: a.generation, frame: 1}
	if cmd := a.update(currentTick); cmd == nil || a.frame != 1 {
		t.Fatal("the current animation did not advance")
	}
	if cmd := a.update(currentTick); cmd != nil || a.frame != 1 {
		t.Fatal("a duplicate tick started another timer loop")
	}
}

func TestTaskStartsAndStopsActivity(t *testing.T) {
	b := &cancellingBackend{started: make(chan struct{}), stopped: make(chan struct{})}
	m := testModel(t, Config{Backend: b})
	m.input.SetValue("Run a task")
	m.submit()
	if !m.spinner.active {
		t.Fatal("submitting a task did not start the activity stream")
	}
	m.cancel()
	deadline := time.After(time.Second)
	for m.busy {
		select {
		case msg := <-m.stream:
			m.Update(msg)
		case <-deadline:
			t.Fatal("task did not finish after cancellation")
		}
	}
	if m.spinner.active {
		t.Fatal("finishing a task left its activity stream running")
	}
}

func TestPickerActivityStopsAfterLoadingAndClosing(t *testing.T) {
	m := testModel(t, settingsConfig())
	load := m.openPicker("model")
	if !m.spinner.active {
		t.Fatal("catalog loading did not animate")
	}
	fetchPicker(t, m, load)
	if m.spinner.active {
		t.Fatal("loaded catalog continued animating")
	}
	m.loadPickerModels("codex", false)
	if !m.spinner.active {
		t.Fatal("catalog refresh did not restart the animation")
	}
	m.closePicker()
	if m.spinner.active {
		t.Fatal("closed picker continued animating")
	}
}

func TestBusyStatusKeepsElapsedTimeWhenModelDetailsDoNotFit(t *testing.T) {
	for _, width := range []int{22, 40, 60, 80, 107, 108} {
		m := testModel(t, Config{})
		m.cfg.Session.Model = strings.Repeat("long-model-name", 5)
		m.resize(width, 30)
		m.busy = true
		m.status = strings.Repeat("Running Subagent ", 10)
		m.started = time.Unix(100, 0)
		m.now = m.started.Add(85 * time.Second)
		status := ansi.Strip(m.statusLine())
		if !strings.Contains(status, "1m 25s") {
			t.Errorf("width %d clipped the elapsed time: %q", width, status)
		}
		if ansi.StringWidth(status) > m.mainWidth {
			t.Errorf("width %d overflowed the status line: %q", width, status)
		}
	}
}

func TestActivityFollowsTheLatestEntry(t *testing.T) {
	m := testModel(t, Config{})
	m.resize(100, 40)
	m.busy, m.status = true, "Running WebSearch"
	m.entries = []entry{{role: "user", text: "Look something up"}, {role: "tool", text: "WebSearch"}}
	m.refresh(true)
	lines := strings.Split(ansi.Strip(m.View().Content), "\n")
	tool, activity := -1, -1
	for i, line := range lines {
		if strings.Contains(line, "│ ◌ Tool") {
			tool = i
		}
		if strings.Contains(line, "Running WebSearch") {
			activity = i
		}
	}
	if tool < 0 || activity != tool+2 {
		t.Fatalf("activity on row %d, want two rows under the tool on row %d", activity, tool)
	}
	if strings.Contains(ansi.Strip(m.statusLine()), "Running") {
		t.Fatal("the status line repeated the activity")
	}

	for range 60 {
		m.entries = append(m.entries, entry{role: "note", text: "earlier"})
	}
	m.refresh(true)
	m.viewport.ScrollUp(20)
	body := strings.Split(ansi.Strip(m.withActivity(m.viewport.View())), "\n")
	if !strings.Contains(body[len(body)-1], "Running WebSearch") {
		t.Fatal("scrolled into history, the activity did not stick to the body's last row")
	}

	m.busy = false
	m.refresh(true)
	if strings.Contains(ansi.Strip(m.View().Content), "Running WebSearch") {
		t.Fatal("a finished task kept its activity row")
	}
}

package ui

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"julia/internal/core"
)

func TestTerminalSummary(t *testing.T) {
	for args, want := range map[string]string{
		`{"action":"start","command":"npm run dev"}`:                                "start · npm run dev",
		`{"action":"start","command":"npm run dev","network":true,"cols":120}`:      "start · npm run dev · network",
		`{"action":"start","command":"cd web &&\nnpm start"}`:                       "start · cd web && ↵ npm start",
		`{"action":"send","id":"t1","keys":["ctrl+c"]}`:                             "send t1 · ctrl+c",
		`{"action":"send","id":"t1","text":"npm test\n","wait_for":"passed"}`:       "send t1 · npm test ↵",
		`{"action":"send","id":"t2","text":"q","keys":["down","enter"]}`:            "send t2 · q · down enter",
		`{"action":"send","id":"t1","mouse":{"action":"click","row":3,"col":12}}`:   "send t1 · click 3,12",
		`{"action":"send","id":"t1","mouse":{"action":"click","text":"[Install]"}}`: `send t1 · click "[Install]"`,
		`{"action":"snapshot","id":"t1","image":true}`:                              "snapshot t1",
		`{"action":"resize","id":"t1","cols":120,"rows":40}`:                        "resize t1 · 120×40",
		`{"action":"stop","id":"t1"}`:                                               "stop t1",
		`{"action":"list"}`:                                                         "list",
		`{"action":`:                                                                `{"action":`,
	} {
		if got := toolSummary(core.ToolCall{Name: "Terminal", Arguments: json.RawMessage(args)}); got != want {
			t.Errorf("%s: summary %q, want %q", args, got, want)
		}
	}
}

const terminalResult = "t1 · bash\nThe screen was still changing after 10s.\nscreen 40×6 · cursor row 2, col 3 · running\n1\t$ echo 界 hi\n2\t$\n[rows 3–6 blank]"

func terminalEntry(content string) entry {
	return entry{role: "tool", done: true,
		call:   &core.ToolCall{Name: "Terminal", Arguments: json.RawMessage(`{"action":"send","id":"t1","text":"echo 界 hi\n"}`)},
		result: &core.ToolResult{Content: content}}
}

// Collapsed, a Terminal result is one row naming the terminal and its state;
// expanded, its screen is a card, with the cursor lit.
func TestTerminalScreenCard(t *testing.T) {
	m := testModel(t, Config{})
	m.resize(100, 30)
	item := terminalEntry(terminalResult)
	collapsed := strings.Split(ansi.Strip(m.renderEntryContent(item)), "\n")
	if len(collapsed) != 2 || !strings.Contains(collapsed[0], "Terminal  send t1 · echo 界 hi ↵") || !strings.Contains(collapsed[1], "t1 · bash · running · The screen was still changing after 10s.") {
		t.Fatalf("collapsed:\n%s", strings.Join(collapsed, "\n"))
	}
	m.expandTools = true
	layout := m.layoutEntry(item)
	plain := ansi.Strip(layout.text)
	for _, want := range []string{"│   The screen was still changing after 10s.", "t1 · bash", "running · 40×6", "  $ echo 界 hi", "  rows 3–6 blank"} {
		if !strings.Contains(plain, want) {
			t.Errorf("expanded card lacks %q:\n%s", want, plain)
		}
	}
	for _, row := range strings.Split(layout.text, "\n") {
		if w := ansi.StringWidth(row); w > m.mainWidth {
			t.Errorf("row %d cells wide in %d: %q", w, m.mainWidth, ansi.Strip(row))
		}
	}
	if lit := strings.Count(layout.text, "48;2;236;229;220"); lit != 1 {
		t.Errorf("cursor drawn %d times", lit)
	}
	if got := layout.targets[1]; got.label != "screen" || got.text != "$ echo 界 hi\n$" {
		t.Errorf("copy target %+v", got)
	}
	// A program that has exited has no cursor to draw.
	if layout = m.layoutEntry(terminalEntry(strings.Replace(terminalResult, "running", "exited with code 0", 1))); strings.Contains(layout.text, "48;2;236;229;220") {
		t.Error("cursor drawn for an exited program")
	}
}

// A long history shows only its end above the screen.
func TestTerminalCardScrollback(t *testing.T) {
	var result strings.Builder
	result.WriteString("t1 · seq 1 200\nscreen 40×3 · cursor hidden · exited with code 0\nscrollback, last 100 lines, numbered up from the screen:\n")
	for i := -100; i < 0; i++ {
		fmt.Fprintf(&result, "%d\tline %d\n", i, i)
	}
	result.WriteString("screen:\n1\tlast\n[rows 2–3 blank]")
	rows, _ := parseTerminalScreenOrFail(t, result.String()).card(60)
	plain := ansi.Strip(strings.Join(rows, "\n"))
	if !strings.Contains(plain, "60 earlier lines") || strings.Contains(plain, "line -41") || !strings.Contains(plain, "line -40") || !strings.Contains(plain, "line -1") || !strings.Contains(plain, "last") {
		t.Fatalf("card:\n%s", plain)
	}
}

func parseTerminalScreenOrFail(t *testing.T, content string) terminalScreen {
	t.Helper()
	screen, ok := parseTerminalScreen(content)
	if !ok {
		t.Fatalf("not a screen: %s", content)
	}
	return screen
}

// A resumed session keeps the result's text and an image marker; it draws the
// same card with the screenshot's caption under it.
func TestTerminalCardResumed(t *testing.T) {
	m := testModel(t, Config{})
	m.resize(100, 30)
	item := terminalEntry(terminalResult + "\n" + core.ImageMarker(core.ImageInfo{MediaType: "image/png", Width: 690, Height: 210, Bytes: 9000}))
	plain := ansi.Strip(m.renderEntryContent(item))
	if !strings.Contains(plain, "t1 · bash · running") || !strings.Contains(plain, "▣ PNG · 690×210 · 9 KB") {
		t.Fatalf("resumed:\n%s", plain)
	}
}

// Results that are not a screen, such as a list or an error, are ordinary output.
func TestTerminalNonScreenResults(t *testing.T) {
	m := testModel(t, Config{})
	m.resize(100, 30)
	for _, result := range []core.ToolResult{
		{Content: "t1 · npm run dev · running · 100×30\nt2 · bash · exited with code 0 · 100×30"},
		{Content: "there is no terminal \"t3\" (stopped ones are forgotten)", IsError: true},
	} {
		item := entry{role: "tool", done: true, call: &core.ToolCall{Name: "Terminal", Arguments: json.RawMessage(`{"action":"list"}`)}, result: &result}
		plain := ansi.Strip(m.renderEntryContent(item))
		if first, _, _ := strings.Cut(result.Content, "\n"); !strings.Contains(plain, first) {
			t.Errorf("result lost:\n%s", plain)
		}
	}
}

// The card frames the screen itself: as wide as the screen where there is
// room, and cut cleanly at the edge, with a note of what is hidden, where
// there is not.
func TestTerminalCardFitsTheScreen(t *testing.T) {
	screen := parseTerminalScreenOrFail(t, "t1 · bash\nscreen 80×3 · cursor hidden · running\n1\t"+strings.Repeat("x", 80)+"\n2\tshort\n[row 3 blank]")
	rows, _ := screen.card(200)
	for _, row := range rows {
		if w := ansi.StringWidth(row); w != 80+codeGutter+1 {
			t.Fatalf("row %d cells wide, want the screen's 80 and the card's gutter:\n%s", w, ansi.Strip(strings.Join(rows, "\n")))
		}
	}
	rows, _ = screen.card(50)
	plain := ansi.Strip(strings.Join(rows, "\n"))
	if !strings.Contains(plain, "t1 · bash") || !strings.Contains(plain, "running · 80×3 · 33 cols hidden \n") || strings.Contains(plain, "…") || !strings.Contains(plain, "  "+strings.Repeat("x", 47)+" \n") {
		t.Fatalf("narrow card:\n%s", plain)
	}
	// Narrower still, the header keeps the name and drops the detail.
	rows, _ = screen.card(30)
	if plain = ansi.Strip(rows[0]); !strings.HasPrefix(plain, "  t1 · bash") || !strings.HasSuffix(plain, "running · 80×3 ") || ansi.StringWidth(rows[0]) != 30 {
		t.Fatalf("narrowest header %q", plain)
	}
}

// A terminal that was stopped on purpose is not shown as a failure.
func TestTerminalStoppedIsNotAnAlarm(t *testing.T) {
	screen := parseTerminalScreenOrFail(t, "t1 · ./bin/julia --demo\nscreen 40×3 · cursor hidden · stopped\n1\tbye\n[rows 2–3 blank]")
	if screen.state != "stopped" || strings.Contains(screen.summary(), styleSequence(coralStyle)) || !strings.Contains(screen.summary(), styleSequence(mutedStyle)+"stopped") {
		t.Fatalf("state %q, summary %q", screen.state, screen.summary())
	}
	crashed := parseTerminalScreenOrFail(t, "t1 · ./bin/julia\nscreen 40×3 · cursor hidden · killed by SIGSEGV\n1\tbye")
	if !strings.Contains(crashed.summary(), styleSequence(coralStyle)+"killed by SIGSEGV") {
		t.Fatalf("crash summary %q", crashed.summary())
	}
}

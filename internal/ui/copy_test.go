package ui

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"julia/internal/core"
)

func TestSplitFences(t *testing.T) {
	got := splitFences("Intro\n\n```Bash title\necho hi\n\n```\nmiddle ```inline``` text\n1. step\n   ~~~go\n   x := 1\n     y\n   ~~~\nend\n```py\nprint(1)")
	want := []markdownSegment{
		{text: "Intro\n"},
		{code: true, language: "bash", text: "echo hi\n"},
		{text: "middle ```inline``` text\n1. step"},
		{code: true, language: "go", text: "x := 1\n  y"},
		{text: "end"},
		{code: true, language: "py", text: "print(1)"}, // still streaming: no closing fence yet
	}
	if len(got) != len(want) {
		t.Fatalf("segments = %#v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("segment %d = %#v, want %#v", i, got[i], want[i])
		}
	}
}

func TestCodeBlockCardWrapsAndPreservesCode(t *testing.T) {
	code := "git -C ~/code/quantrading worktree remove --force -- ~/Documents/a/very/long/path/that/wraps\n\tindented"
	rows, continued := renderCodeBlock(code, "bash", 40)
	for i, row := range rows {
		if w := ansi.StringWidth(row); w != 40 {
			t.Errorf("row %d is %d cells wide, want a filled 40: %q", i, w, ansi.Strip(row))
		}
	}
	if header := ansi.Strip(rows[0]); !strings.Contains(header, "bash") || !strings.Contains(header, "copy") {
		t.Errorf("header = %q", header)
	}
	var lines []string
	for i, row := range rows[2 : len(rows)-1] {
		plain := ansi.Strip(row)
		text := ansi.Cut(plain, codeGutter, 40-1)
		if continued[i+2] {
			if !strings.HasPrefix(plain, "↳") {
				t.Errorf("continuation row %q lacks its marker", plain)
			}
			lines[len(lines)-1] += text
			continue
		}
		lines = append(lines, text)
	}
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " ")
	}
	if got, want := strings.Join(lines, "\n"), strings.ReplaceAll(code, "\t", "    "); got != want {
		t.Errorf("card text = %q, want %q", got, want)
	}
}

func TestTrimTrailingDropsGlamourPadding(t *testing.T) {
	line := "\x1b[1mhi\x1b[m\x1b[38;5;1m \x1b[m\x1b[38;5;1m \x1b[m"
	if got := ansi.Strip(trimTrailing(line + "\n" + line)); got != "hi\nhi" {
		t.Errorf("trimTrailing = %q", got)
	}
}

const copyReply = "Run this:\n\n```bash\ngit -C ~/code/quantrading worktree remove --force -- ~/Documents/Codex/2026-10-01/we-recently-updated-quantrading-observability-stack/work/quantrading\nrm -rf ~/Documents/Codex/2026-10-01\n```\n\nThen check."

func copyModel(t *testing.T) (*model, *[]string) {
	t.Helper()
	m := newModel(Config{Session: nil})
	t.Cleanup(m.stop)
	var copied []string
	m.clipboard = func(text string) error { copied = append(copied, text); return nil }
	m.resize(100, 40)
	m.entries = []entry{{role: "user", text: "remove the worktree please"}, {role: "assistant", text: copyReply}}
	m.refresh(true)
	m.viewport.GotoTop()
	return m, &copied
}

// runCmd executes a command and everything it batches, feeding results back.
func runCmd(m *model, cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	switch msg := cmd().(type) {
	case tea.BatchMsg:
		for _, c := range msg {
			runCmd(m, c)
		}
	case copiedMsg:
		m.Update(msg)
	}
}

// screenRow finds the terminal row showing text.
func screenRow(t *testing.T, m *model, text string) int {
	t.Helper()
	for i, line := range m.lines {
		if strings.Contains(ansi.Strip(line), text) {
			return i - m.viewport.YOffset() + 3
		}
	}
	t.Fatalf("no row shows %q", text)
	return 0
}

func click(m *model, x, y int) tea.Cmd {
	m.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	_, cmd := m.Update(tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft})
	return cmd
}

func TestDragCopiesSelectionWithoutGuttersAndJoinsWrappedRows(t *testing.T) {
	m, copied := copyModel(t)
	start := screenRow(t, m, "git -C")
	end := screenRow(t, m, "rm -rf")
	m.Update(tea.MouseClickMsg{X: 0, Y: start, Button: tea.MouseLeft})
	m.Update(tea.MouseMotionMsg{X: 50, Y: end, Button: tea.MouseLeft})
	if !strings.Contains(ansi.Strip(m.highlightSelection(m.viewport.View())), "git -C") {
		t.Fatal("selection is not drawn")
	}
	_, cmd := m.Update(tea.MouseReleaseMsg{X: 50, Y: end, Button: tea.MouseLeft})
	runCmd(m, cmd)
	want := "git -C ~/code/quantrading worktree remove --force -- ~/Documents/Codex/2026-10-01/we-recently-updated-quantrading-observability-stack/work/quantrading\nrm -rf ~/Documents/Codex/2026-10-01"
	if len(*copied) != 1 || (*copied)[0] != want {
		t.Fatalf("copied %q, want %q", *copied, want)
	}
	if !strings.Contains(m.notice, "Copied selection") {
		t.Errorf("notice = %q", m.notice)
	}
}

func TestClickOnCodeBlockCopiesItsCode(t *testing.T) {
	m, copied := copyModel(t)
	runCmd(m, click(m, 30, screenRow(t, m, "rm -rf")))
	want := "git -C ~/code/quantrading worktree remove --force -- ~/Documents/Codex/2026-10-01/we-recently-updated-quantrading-observability-stack/work/quantrading\nrm -rf ~/Documents/Codex/2026-10-01"
	if len(*copied) != 1 || (*copied)[0] != want {
		t.Fatalf("copied %q, want the block's code", *copied)
	}
	if m.selection == nil || m.selection.flash == nil {
		t.Error("the copied block should light up")
	}
	if !strings.Contains(m.notice, "bash code") {
		t.Errorf("notice = %q", m.notice)
	}
}

func subagentCopyModel(t *testing.T, task string) (*model, *[]string) {
	t.Helper()
	arguments, err := json.Marshal(map[string]any{"tasks": []map[string]string{{"role": "reviewer", "task": task}}})
	if err != nil {
		t.Fatal(err)
	}
	m, copied := copyModel(t)
	m.entries = []entry{{
		role:   "tool",
		call:   &core.ToolCall{Name: "Subagent", Arguments: arguments},
		result: &core.ToolResult{Content: "child answer"},
	}}
	m.expandTools = true
	m.refresh(true)
	return m, copied
}

func TestDoubleClickCopiesTheSubagentTask(t *testing.T) {
	task := "Review this exact delegated prompt."
	m, copied := subagentCopyModel(t, task)
	row := screenRow(t, m, "Review this exact")
	runCmd(m, click(m, 10, row))
	runCmd(m, click(m, 10, row))
	if len(*copied) != 1 || (*copied)[0] != task {
		t.Fatalf("copied %q, want the subagent task %q", *copied, task)
	}
}

func TestDragCopyJoinsWrappedSubagentPromptRows(t *testing.T) {
	task := "Inspect the prompt rows and preserve the original text " +
		strings.Repeat(" including all visible words", 5) +
		"\nKeep this deliberate line break."
	m, copied := subagentCopyModel(t, task)
	start := screenRow(t, m, "Inspect the prompt")
	end := screenRow(t, m, "Keep this deliberate line break.")
	m.Update(tea.MouseClickMsg{X: 0, Y: start, Button: tea.MouseLeft})
	m.Update(tea.MouseMotionMsg{X: 97, Y: end, Button: tea.MouseLeft})
	_, cmd := m.Update(tea.MouseReleaseMsg{X: 97, Y: end, Button: tea.MouseLeft})
	runCmd(m, cmd)
	if len(*copied) != 1 || (*copied)[0] != task {
		t.Fatalf("copied %q, want the prompt with its original line break %q", *copied, task)
	}
}

func TestDoubleClickCopiesWholeMessages(t *testing.T) {
	m, copied := copyModel(t)
	row := screenRow(t, m, "remove the worktree please")
	runCmd(m, click(m, 10, row))
	if len(*copied) != 0 {
		t.Fatalf("a single click on prose copied %q", *copied)
	}
	runCmd(m, click(m, 10, row))
	if len(*copied) != 1 || (*copied)[0] != "remove the worktree please" {
		t.Fatalf("copied %q, want your message", *copied)
	}

	m.lastClick.at = time.Time{}
	row = screenRow(t, m, "Then check.")
	runCmd(m, click(m, 10, row))
	runCmd(m, click(m, 10, row))
	if len(*copied) != 2 || (*copied)[1] != copyReply {
		t.Fatalf("copied %q, want the reply's markdown", *copied)
	}
}

func TestCopyCommand(t *testing.T) {
	m, copied := copyModel(t)
	m.busy = true // copying never waits for the running task
	m.input.SetValue("/copy")
	runCmd(m, m.submit())
	m.input.SetValue("/copy code")
	runCmd(m, m.submit())
	if len(*copied) != 2 || (*copied)[0] != copyReply || !strings.HasPrefix((*copied)[1], "git -C") {
		t.Fatalf("copied %q", *copied)
	}
	if m.input.Value() != "" {
		t.Errorf("composer kept %q", m.input.Value())
	}
	m.input.SetValue("/copyright")
	m.busy = false
	if handled, _ := m.copyCommand("/copyright"); handled {
		t.Error("/copyright is not /copy")
	}
}

func TestCopySanitizesAndFallsBackToTerminalClipboard(t *testing.T) {
	m, _ := copyModel(t)
	m.clipboard = func(string) error { return errRemoteSession }
	msg := m.copyText("reply", "safe\x1b]52;c;ZXZpbA==\x07 ‮text")()
	copied := msg.(copiedMsg)
	if copied.text != "safe text" {
		t.Errorf("copied %q", copied.text)
	}
	if cmd := m.receiveCopied(copied); cmd == nil {
		t.Error("a failed system copy should fall back to OSC 52")
	}
}

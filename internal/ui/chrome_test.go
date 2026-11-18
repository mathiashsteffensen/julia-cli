package ui

import (
	"fmt"
	"strings"
	"testing"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"julia/internal/core"
)

// composerTop is the row of the composer's lit edge, -1 when no row has one.
func composerTop(lines []string) int {
	for row, line := range lines {
		if strings.Contains(line, "▁") {
			return row
		}
	}
	return -1
}

// The draft's rows are the panel from side to side: no blank cell lets the
// page show through, whatever the draft, the focus or the width.
func TestComposerBlankCellsKeepPanelBackground(t *testing.T) {
	wantR, wantG, wantB, _ := panel.RGBA()
	for _, text := range []string{"", "a draft", "first line\nsecond line"} {
		for _, focused := range []bool{true, false} {
			m := testModel(t, Config{})
			m.resize(100, 40)
			m.input.SetValue(text)
			if !focused {
				m.input.Blur()
			}
			for _, width := range []int{100, 40, 140} {
				m.resize(width, 40)
				view := m.View().Content
				buffer := uv.NewScreenBuffer(m.width, m.height)
				uv.NewStyledString(view).Draw(buffer, buffer.Bounds())

				top := composerTop(strings.Split(ansi.Strip(view), "\n"))
				if top < 0 {
					t.Fatalf("draft %q, focused %v, width %d: the composer has no lit edge", text, focused, width)
				}
				// Under the edge comes the row its light falls on, then the draft.
				for y := top + composerChrome; y < top+composerChrome+m.input.Height(); y++ {
					for x := m.margin(); x < m.margin()+m.mainWidth; x++ {
						cell := buffer.CellAt(x, y)
						if cell == nil || cell.Content != " " {
							continue
						}
						if cell.Style.Bg == nil {
							t.Fatalf("draft %q, focused %v: cell (%d,%d) has no background, want the panel", text, focused, x, y)
						}
						if r, g, b, _ := cell.Style.Bg.RGBA(); r != wantR || g != wantG || b != wantB {
							t.Fatalf("draft %q, focused %v: cell (%d,%d) has background #%02x%02x%02x, want the panel", text, focused, x, y, r>>8, g>>8, b>>8)
						}
					}
				}
			}
		}
	}
}

// The composer and the status row under it stay whole at every width and
// height the spacing changes at, whatever the composer is doing.
func TestBottomControlsSurviveSpacingBreakpoints(t *testing.T) {
	for _, width := range []int{22, 40, 59, 60, 80, 107, 108, 128} {
		for _, height := range []int{15, 27, 28, 35, 36} {
			for _, state := range []string{"message", "busy draft", "model search"} {
				t.Run(fmt.Sprintf("%s/%dx%d", state, width, height), func(t *testing.T) {
					m := testModel(t, Config{Demo: true})
					m.notice = ""
					m.entries = []entry{{role: "assistant", text: strings.Repeat("Transcript row.\n", 80)}}
					m.resize(width, height)
					m.input.SetValue("draft")
					caption, field := "", "draft"
					switch state {
					case "busy draft":
						// Activate after resize: a busy composer must not change
						// the reserved height.
						m.busy, m.status = true, "Working"
					case "model search":
						m.openPicker("model")
						m.picker.search.SetValue("probe")
						caption, field = "Search", "probe"
					}
					lines := strings.Split(ansi.Strip(m.View().Content), "\n")
					if len(lines) > height {
						t.Fatalf("view has %d rows for height %d", len(lines), height)
					}
					for row, line := range lines {
						if cells := ansi.StringWidth(line); cells > width {
							t.Fatalf("row %d has %d cells for width %d", row, cells, width)
						}
					}

					top := composerTop(lines)
					status := top + composerChrome + m.input.Height()
					if top < 0 || status >= len(lines) {
						t.Fatalf("composer or status row clipped (top %d, status %d of %d rows):\n%s", top, status, len(lines), strings.Join(lines, "\n"))
					}
					composer := strings.Join(lines[top:status], "\n")
					if !strings.Contains(lines[top+1], caption) || !strings.Contains(composer, field) {
						t.Fatalf("composer lost its caption or input:\n%s", composer)
					}
					if state != "busy draft" && strings.TrimSpace(lines[status]) == "" {
						t.Fatalf("the status row is empty:\n%s", strings.Join(lines, "\n"))
					}
				})
			}
		}
	}
}

// A tall screen parts the composer from the conversation by a clear row, and
// the status row from the bottom edge by another.
func TestTallBottomControlsHaveBreathingRoom(t *testing.T) {
	for _, height := range []int{28, 35, 36, 50} {
		t.Run(fmt.Sprintf("height%d", height), func(t *testing.T) {
			m := testModel(t, Config{Session: &core.Session{Provider: "fake"}})
			m.entries = []entry{{role: "assistant", text: strings.Repeat("Transcript row.\n", 80)}}
			m.resize(80, height)
			lines := strings.Split(ansi.Strip(m.View().Content), "\n")

			top := composerTop(lines)
			status := top + composerChrome + m.input.Height()
			if top < 1 || strings.TrimSpace(lines[top-1]) != "" {
				t.Fatalf("the composer needs a clear row above it:\n%s", strings.Join(lines, "\n"))
			}
			if status != len(lines)-2 || !strings.Contains(lines[status], "Ready") || strings.TrimSpace(lines[status+1]) != "" {
				t.Fatalf("the status row needs a clear row under it:\n%s", strings.Join(lines, "\n"))
			}
		})
	}
}

// The light is where the keys go. A question, an approval or the fleet takes
// the keyboard: the composer's edge goes dark, and its caption says why the
// draft waits.
func TestTheComposerIsLitOnlyWhileItHoldsTheKeyboard(t *testing.T) {
	m := testModel(t, Config{})
	m.entries = []entry{{role: "assistant", text: "A conversation."}}
	m.resize(100, 30)
	if rows := m.composer(); !strings.Contains(rows[0], "▁") || strings.Trim(ansi.Strip(rows[1]), "▀") != "" {
		t.Fatalf("a composer with the keyboard is not lit:\n%s", ansi.Strip(strings.Join(rows, "\n")))
	}

	m.fleetView = &fleetState{}
	rows := m.composer()
	if rows[0] != "" || !strings.Contains(ansi.Strip(rows[1]), "Draft · kept while you inspect the fleet") {
		t.Fatalf("a composer without the keyboard keeps its light:\n%s", ansi.Strip(strings.Join(rows, "\n")))
	}
}

// The masthead names the workspace and the mode, and JULIA's own name only
// when neither the sidebar nor the welcome carries the wordmark.
func TestMastheadNamesTheWorkspaceAndTheMode(t *testing.T) {
	m := testModel(t, Config{Mode: "auto", Session: &core.Session{ID: "s", Provider: "fake", Directory: "/srv/work/julia-cli"}})

	m.resize(100, 30)
	if row := ansi.Strip(m.masthead()); !strings.HasPrefix(row, "/srv/work/julia-cli  ─") || !strings.HasSuffix(row, "─  ● auto") || ansi.StringWidth(row) != m.mainWidth {
		t.Fatalf("welcome masthead: %q", row)
	}

	m.entries = []entry{{role: "assistant", text: "A conversation."}}
	m.resize(100, 30)
	if row := ansi.Strip(m.masthead()); !strings.HasPrefix(row, "JULIA  /srv/work/julia-cli  ─") {
		t.Fatalf("masthead without a sidebar: %q", row)
	}
	m.resize(128, 30)
	if row := ansi.Strip(m.masthead()); !strings.HasPrefix(row, "/srv/work/julia-cli  ─") {
		t.Fatalf("masthead beside the sidebar: %q", row)
	}

	// With less room the path goes, then the name.
	m.resize(60, 30)
	if row := ansi.Strip(m.masthead()); !strings.HasPrefix(row, "JULIA  julia-cli  ─") {
		t.Fatalf("narrow masthead: %q", row)
	}
	m.resize(30, 30)
	if row := ansi.Strip(m.masthead()); !strings.HasPrefix(row, "JULIA  ─") || !strings.HasSuffix(row, "● auto") {
		t.Fatalf("narrowest masthead: %q", row)
	}
}

// The status row gives its keys up to the status when both do not fit, and
// on a screen with neither sidebar nor welcome it carries the sidebar's two
// vitals in their place.
func TestStatusRowSharesItsRowWithTheKeys(t *testing.T) {
	m := testModel(t, Config{Session: &core.Session{ID: "s", Provider: "codex", Model: "gpt-6-astra", Reasoning: "high", Usage: core.SessionUsage{Context: 128_000}}})
	m.entries = []entry{{role: "assistant", text: "A conversation."}}

	m.resize(128, 30)
	if row := ansi.Strip(m.statusRow()); !strings.HasPrefix(row, "● Ready") || !strings.HasSuffix(row, "enter send   / commands   alt+enter newline   esc interrupt   ctrl+s sidebar") {
		t.Fatalf("beside the sidebar: %q", row)
	}
	m.busy = true
	if row := ansi.Strip(m.statusRow()); !strings.Contains(row, "enter queue") {
		t.Fatalf("while a task runs: %q", row)
	}
	m.busy = false

	m.resize(100, 30)
	if row := ansi.Strip(m.statusRow()); !strings.HasPrefix(row, "● Ready") || !strings.HasSuffix(row, "gpt-6-astra · high  ▄▄▄▄▄▄▄▄▄▄ 50%") {
		t.Fatalf("without a sidebar: %q", row)
	}
	m.notice = strings.Repeat("A long notice. ", 8)
	if row := ansi.Strip(m.statusRow()); strings.Contains(row, "gpt-6-astra") || ansi.StringWidth(row) > m.mainWidth {
		t.Fatalf("a long status must take the row: %q", row)
	}
}

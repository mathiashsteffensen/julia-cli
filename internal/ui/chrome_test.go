package ui

import (
	"fmt"
	"strings"
	"testing"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"julia/internal/core"
)

func TestComposerBlankCellsKeepPanelBackground(t *testing.T) {
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
				inside := false
				for y, line := range strings.Split(ansi.Strip(view), "\n") {
					main := ansi.Truncate(line, m.margin()+m.mainWidth, "") // the sidebar card has corners of its own
					top, bottom := strings.Contains(main, "╭"), strings.Contains(main, "╰")
					inside = inside || top
					if !inside {
						continue
					}
					// Border cells too: a terminal draws the stroke inside its cell,
					// so any other background shows as a gap before the edge.
					for x := m.margin(); x < m.margin()+m.mainWidth; x++ {
						cell := buffer.CellAt(x, y)
						edge := top || bottom || x == m.margin() || x == m.margin()+m.mainWidth-1
						if cell == nil || cell.Content != " " && !edge {
							continue
						}
						bg := cell.Style.Bg
						if bg == nil {
							bg = background
						}
						r, g, b, _ := bg.RGBA()
						wantR, wantG, wantB, _ := panel.RGBA()
						if r != wantR || g != wantG || b != wantB {
							t.Fatalf("draft %q, focused %v: cell (%d,%d) has background #%02x%02x%02x, want panel", text, focused, x, y, r>>8, g>>8, b>>8)
						}
					}
					if bottom {
						break
					}
				}
			}
		}
	}
}

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
					title, field := "MESSAGE", "draft"
					switch state {
					case "busy draft":
						// Activate after resize: the longer title must not change
						// the reserved height or wrap out of the narrow composer.
						m.busy, m.status = true, "Working"
						title = "MESSAGE"
					case "model search":
						m.openPicker("model")
						m.picker.search.SetValue("probe")
						title, field = "SEARCH", "probe"
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
					top, bottom, shortcuts := -1, -1, -1
					for row, line := range lines {
						switch {
						case strings.Contains(line, "╭") && strings.Contains(line, "╮"):
							top = row
						case strings.Contains(line, "╰") && strings.Contains(line, "╯"):
							bottom = row
						case strings.Contains(line, "enter"):
							shortcuts = row
						}
					}
					if top < 0 || bottom <= top || shortcuts <= bottom {
						t.Fatalf("composer or shortcuts clipped (top %d, bottom %d, shortcuts %d):\n%s", top, bottom, shortcuts, strings.Join(lines, "\n"))
					}
					composer := strings.Join(lines[top:bottom+1], "\n")
					if !strings.Contains(composer, title) || !strings.Contains(composer, field) {
						t.Fatalf("composer lost its title or input:\n%s", composer)
					}
				})
			}
		}
	}
}

func TestTallBottomControlsHaveBreathingRoom(t *testing.T) {
	for _, height := range []int{28, 35, 36, 50} {
		t.Run(fmt.Sprintf("height%d", height), func(t *testing.T) {
			m := testModel(t, Config{Session: &core.Session{Provider: "fake"}})
			m.entries = []entry{{role: "assistant", text: strings.Repeat("Transcript row.\n", 80)}}
			m.resize(80, height)
			lines := strings.Split(ansi.Strip(m.View().Content), "\n")
			status, top, bottom, shortcuts := -1, -1, -1, -1
			for row, line := range lines {
				switch {
				case strings.Contains(line, "Ready"):
					status = row
				case strings.Contains(line, "╭"):
					top = row
				case strings.Contains(line, "╰"):
					bottom = row
				case strings.Contains(line, "/help"):
					shortcuts = row
				}
			}
			if status < 1 || top <= status+1 || strings.TrimSpace(lines[status-1]) != "" || strings.TrimSpace(lines[status+1]) != "" {
				t.Fatalf("activity needs a blank row on each side:\n%s", strings.Join(lines, "\n"))
			}
			if height >= 36 && (bottom < 0 || shortcuts <= bottom+1 || strings.TrimSpace(lines[shortcuts-1]) != "") {
				t.Fatalf("tall composer needs separation from its shortcuts:\n%s", strings.Join(lines, "\n"))
			}
		})
	}
}

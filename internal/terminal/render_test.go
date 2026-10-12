package terminal

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"reflect"
	"strings"
	"testing"
)

// screen feeds output straight into an emulator, without a program or PTY.
func screen(t *testing.T, cols, rows int, output string) Snapshot {
	t.Helper()
	s := &Session{cols: cols, rows: rows, dec: map[int]bool{}, cursorOn: true, changed: make(chan struct{})}
	s.emu = newEmulator(s, cols, rows)
	s.emu.WriteString(output)
	return s.Snapshot(0)
}

func TestTextCollapsesTrailingBlankRows(t *testing.T) {
	for _, tc := range []struct {
		output string
		want   string
	}{
		{"", "screen 20×3 · cursor row 1, col 1 · running\n[all 3 rows blank]"},
		{"a\r\n\r\nb", "screen 20×3 · cursor row 3, col 2 · running\n1\ta\n2\t\n3\tb"},
		{"a\r\nb", "screen 20×3 · cursor row 2, col 2 · running\n1\ta\n2\tb\n[row 3 blank]"},
		{"\x1b[?1049h\x1b[?1000hx", "screen 20×3 · cursor row 1, col 2 · alt screen · mouse on · running\n1\tx\n[rows 2–3 blank]"},
	} {
		if got := screen(t, 20, 3, tc.output).Text(); got != tc.want {
			t.Errorf("output %q:\n%s\nwant\n%s", tc.output, got, tc.want)
		}
	}
}

func TestAltScreenKeepsNoScrollback(t *testing.T) {
	s := &Session{cols: 10, rows: 3, dec: map[int]bool{}, changed: make(chan struct{})}
	s.emu = newEmulator(s, 10, 3)
	scrs := reflect.ValueOf(s.emu).Elem().FieldByName("scrs")
	if !scrs.IsValid() || !scrs.Index(1).FieldByName("scrollback").IsNil() {
		t.Fatal("the alternate screen still keeps an unbounded scrollback; vt's layout changed")
	}
}

func pixel(img *image.RGBA, x, y int) color.RGBA { return img.RGBAAt(x, y) }

func TestRenderColoursAndGeometry(t *testing.T) {
	snap := screen(t, 10, 3, "\x1b[41m  \x1b[m\x1b[38;2;10;200;30m█\x1b[m\x1b[7m \x1b[m\r\n\x1b]11;#102030\x07")
	img := snap.Render(RenderOptions{})
	m := metricsFor()[25]
	if m.width != cellWidth || m.height != cellHeight {
		t.Fatalf("cell %dx%d, reported %dx%d", m.width, m.height, cellWidth, cellHeight)
	}
	if got, want := img.Bounds().Size(), image.Pt(12*cellWidth, 3*cellHeight+2*cellWidth); got != want {
		t.Fatalf("image %v, want %v", got, want)
	}
	at := func(col, row int) color.RGBA { // centre of a 0-based cell
		return pixel(img, cellWidth+col*cellWidth+cellWidth/2, cellWidth+row*cellHeight+cellHeight/2)
	}
	for _, tc := range []struct {
		name      string
		got, want color.RGBA
	}{
		{"margin uses the program's background", pixel(img, 1, 1), color.RGBA{0x10, 0x20, 0x30, 0xff}},
		{"palette red background", at(1, 0), themePalette[1].(color.RGBA)},
		{"truecolor full block", at(2, 0), color.RGBA{10, 200, 30, 0xff}},
		{"reverse video", at(3, 0), themeFg},
		{"block cursor", at(0, 1), themeCursor},
	} {
		if tc.got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, tc.got, tc.want)
		}
	}
}

func TestBoxDrawingJoinsAcrossCells(t *testing.T) {
	snap := screen(t, 6, 4, "╭──╮\r\n│  │\r\n╰──╯\r\n═╬═\x1b[?25l")
	img := snap.Render(RenderOptions{})
	bg := snap.bg
	m := metricsFor()[25]
	x0, y0 := m.width, m.width // padding
	lineY := y0 + lo(m.height/2, m.line)
	lineX := x0 + lo(m.width/2, m.line)
	// The top border is unbroken from the first arc's tangent to the last.
	for x := x0 + m.width; x < x0+3*m.width; x++ {
		if pixel(img, x, lineY) == bg {
			t.Fatalf("gap in ── at x=%d", x)
		}
	}
	// The left border runs from │ into ╰ until its arc turns.
	for y := y0 + m.height; y < y0+2*m.height+m.height/4; y++ {
		if pixel(img, lineX, y) == bg {
			t.Fatalf("gap in │ at y=%d", y)
		}
	}
	// ═ continues through ╬ on both of its lines.
	row := y0 + 3*m.height
	for _, y := range []int{row + lo(m.height/2-m.line, m.line), row + lo(m.height/2+m.line, m.line)} {
		for x := x0; x < x0+3*m.width; x++ {
			inside := x >= x0+m.width+m.width/2-m.line && x < x0+m.width+m.width/2+m.line
			if pixel(img, x, y) == bg && !inside {
				t.Fatalf("gap in ═╬═ at x=%d y=%d", x, y)
			}
		}
	}
	// Nothing but the arc is drawn in a rounded corner's outer quarter.
	if pixel(img, x0+1, y0+1) != bg {
		t.Fatal("╭ fills its outer corner")
	}
}

func TestRenderFitsLargeScreens(t *testing.T) {
	snap := screen(t, 200, 60, "wide")
	img := snap.Render(RenderOptions{})
	if b := img.Bounds(); b.Dx() > 2000 || b.Dy() > 2000 || b.Dx() < 1200 {
		t.Fatalf("200×60 rendered at %v", b.Size())
	}
	data, err := snap.PNG(RenderOptions{MaxWidth: 800})
	if err != nil {
		t.Fatal(err)
	}
	if decoded, err := png.Decode(bytes.NewReader(data)); err != nil || decoded.Bounds().Dx() > 1300 {
		t.Fatalf("PNG with MaxWidth 800: %v, %v", decoded.Bounds(), err)
	}
}

func TestSpecialGlyphCoverage(t *testing.T) {
	for r := rune(0x2500); r <= 0x28ff; r++ {
		if r > 0x259f && r < 0x2800 {
			continue
		}
		mask := specialGlyph(r, 15, 30, 2)
		if mask == nil {
			t.Fatalf("%U not drawn", r)
		}
		ink := 0
		for _, a := range mask.Pix {
			ink += int(a)
		}
		if (ink == 0) != (r == 0x2800) { // U+2800 is the blank braille pattern
			t.Errorf("%c %U has ink %d", r, r, ink)
		}
	}
	if specialGlyph('a', 15, 30, 2) != nil {
		t.Fatal("ordinary letters are drawn procedurally")
	}
}

// TestRenderSample writes a showcase screen to $TERMINAL_RENDER_SAMPLE for
// judging the renderer by eye.
func TestRenderSample(t *testing.T) {
	path := os.Getenv("TERMINAL_RENDER_SAMPLE")
	if path == "" {
		t.Skip("set TERMINAL_RENDER_SAMPLE to a PNG path")
	}
	snap := screen(t, 72, 12, "\x1b]2;sample\x07"+
		"\x1b[1mbold\x1b[m \x1b[2mfaint\x1b[m \x1b[3mitalic\x1b[m \x1b[4munder\x1b[m \x1b[4:3mcurly\x1b[m \x1b[4:2mdouble\x1b[m \x1b[9mstrike\x1b[m \x1b[7mreverse\x1b[m\r\n"+
		"\x1b[30m■\x1b[31m■\x1b[32m■\x1b[33m■\x1b[34m■\x1b[35m■\x1b[36m■\x1b[37m■ \x1b[90m■\x1b[91m■\x1b[92m■\x1b[93m■\x1b[94m■\x1b[95m■\x1b[96m■\x1b[97m■\x1b[m  \x1b[38;5;208m256\x1b[m \x1b[38;2;255;121;198mtruecolor\x1b[m\r\n"+
		"╭─────────╮ ┌──┬──┐ ╔══╦══╗ ┏━━┳━━┓ ╓──╥──╖\r\n"+
		"│ rounded │ ├──┼──┤ ╠══╬══╣ ┣━━╋━━┫ ╟──╫──╢\r\n"+
		"╰─────────╯ └──┴──┘ ╚══╩══╝ ┗━━┻━━┛ ╙──╨──╜\r\n"+
		"▁▂▃▄▅▆▇█ ░▒▓ ▖▗▘▙▚▛▜▝▞▟ ⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏ ⣿⡇ ╱╲╳ ┄┈╌\r\n"+
		"\x1b[30;44m NORMAL \x1b[34;42m\ue0b0\x1b[30m main \x1b[32;49m\ue0b0\x1b[m ✓ ✗ ★ ◆ ❯ → 界 🚀\r\n"+
		"$ \x1b[?12h")
	data, err := snap.PNG(RenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// Selection and focus that only colour shows are listed under the rows,
// measured against the screen's commonest background.
func TestTextListsHighlights(t *testing.T) {
	snap := screen(t, 30, 4, "  plain row\r\n\x1b[7m> selected   \x1b[m\r\n\x1b[44m  focused pane  \x1b[m x\x1b[41my\x1b[m")
	if got, want := snap.Text(), "screen 30×4 · cursor row 3, col 20 · running\n1\t  plain row\n2\t> selected\n3\t  focused pane   xy\n[row 4 blank]\n[highlighted: row 2, cols 1–13; row 3, cols 1–16]"; got != want {
		t.Fatalf("Text() =\n%s\nwant\n%s", got, want)
	}
	blue := screen(t, 10, 2, "\x1b[44mabcdefghij\r\nab\x1b[46mcdef\x1b[44mghij\x1b[m")
	if got := blue.Text(); !strings.HasSuffix(got, "\n[highlighted: row 2, cols 3–6]") {
		t.Fatalf("a blue screen:\n%s", got)
	}
	many := screen(t, 10, 20, strings.Repeat("\x1b[7mselected\x1b[m\r\n", 10))
	if got := many.Text(); !strings.HasSuffix(got, "row 8, cols 1–8; 2 more]") {
		t.Fatalf("many highlights:\n%s", got)
	}
}

// Characters no font here has, such as emoji, are drawn as tofu boxes;
// Missing names them so a screenshot is not mistaken for a broken screen.
func TestMissingGlyphs(t *testing.T) {
	if len(fallbackFonts()) == 0 {
		t.Skip("no system fallback fonts")
	}
	snap := screen(t, 30, 2, "✅ ok ✓ ─ ✅ 🚀")
	if got := snap.Missing(); !reflect.DeepEqual(got, []string{"✅", "🚀"}) {
		t.Fatalf("Missing() = %q", got)
	}
}

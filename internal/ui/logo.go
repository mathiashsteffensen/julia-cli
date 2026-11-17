package ui

import (
	"image/color"
	"strings"

	"charm.land/lipgloss/v2"
)

// Proportional, chamfered letters drawn with quadrant blocks in 18 columns.
// The narrow I keeps the mark compact between the open U and L.
// Vertical strokes are two pixels wide to balance the taller terminal cells.
var logoLetters = [][6]string{
	{"....##", "....##", "....##", "....##", "##..##", ".####."}, // J
	{"##..##", "##..##", "##..##", "##..##", "##..##", ".####."}, // U
	{"##....", "##....", "##....", "##....", "##....", "######"}, // L
	{"####", ".##.", ".##.", ".##.", ".##.", "####"},             // I
	{".####.", "##..##", "##..##", "######", "##..##", "##..##"}, // A
}

// quadrants maps a cell's pixels (top-left, top-right, bottom-left,
// bottom-right as bits 3 to 0) to the block element that draws them.
var quadrants = []string{" ", "▗", "▖", "▄", "▝", "▐", "▞", "▟", "▘", "▚", "▌", "▙", "▀", "▜", "▛", "█"}

// The brand's wave: the loader's violet and coral, warming into amber.
var logoGradient = []color.RGBA{{0xC6, 0xB6, 0xF2, 0xFF}, {0xF4, 0x9D, 0x89, 0xFF}, {0xF9, 0xD3, 0x99, 0xFF}}

func logoColor(column, width int) color.Color {
	t := float64(column) / float64(max(1, width-1)) * float64(len(logoGradient)-1)
	i := min(int(t), len(logoGradient)-2)
	a, b, f := logoGradient[i], logoGradient[i+1], t-float64(i)
	mix := func(x, y uint8) uint8 { return uint8(float64(x) + (float64(y)-float64(x))*f + 0.5) }
	return color.RGBA{mix(a.R, b.R), mix(a.G, b.G), mix(a.B, b.B), 0xFF}
}

// logoLettering shades the wordmark column by column without relying on a
// terminal's bold font or graphics protocol. On a surface every cell carries
// its background, the gaps too; with none (nil) the mark paints no background,
// for a scrollback whose color is not the interface's.
func logoLettering(surface color.Color) []string {
	style := lipgloss.NewStyle()
	if surface != nil {
		style = style.Background(surface)
	}
	width := len(logoLetters) - 1 // one column between letters
	for _, letter := range logoLetters {
		width += len(letter[0]) / 2
	}
	rows := make([]string, len(logoLetters[0])/2)
	for row := range rows {
		var b strings.Builder
		column := 0
		for i, letter := range logoLetters {
			if i > 0 {
				b.WriteString(style.Render(" "))
				column++
			}
			for x := range len(letter[0]) / 2 {
				bits := 0
				for _, pixel := range []byte{letter[2*row][2*x], letter[2*row][2*x+1], letter[2*row+1][2*x], letter[2*row+1][2*x+1]} {
					bits <<= 1
					if pixel == '#' {
						bits |= 1
					}
				}
				cell := quadrants[bits]
				b.WriteString(style.Foreground(logoColor(column, width)).Render(cell))
				column++
			}
		}
		rows[row] = b.String()
	}
	return rows
}

// heroLettering draws the wordmark for the welcome screen: each pixel a cell
// wide and half a cell tall, so the letters stand twice as wide as the sidebar's
// and their strokes read as blocks rather than quadrant stipple. The mark
// stands on its own reflection, mirrored at the baseline and fading as it
// sinks, which lifts it off the page as light would. gap is the number of
// cells between letters. The result is heroWidth(gap) cells wide and six rows
// tall: three of letters, three of reflection.
func heroLettering(gap int) []string {
	width := heroWidth(gap)
	lit := make([][]bool, len(logoLetters[0]))
	for y := range lit {
		lit[y] = make([]bool, width)
	}
	column := 0
	for i, l := range logoLetters {
		if i > 0 {
			column += gap
		}
		for y, row := range l {
			for x := range row {
				lit[y][column+x] = row[x] == '#'
			}
		}
		column += len(l[0])
	}

	// A half row of the mark: a letter's pixel, its reflection, or nothing (nil).
	half := func(y, x int) color.Color {
		letters := len(lit)
		if y < letters {
			if lit[y][x] {
				return logoColor(x, width)
			}
			return nil
		}
		depth := y - letters
		if depth >= len(reflectionFade) || !lit[letters-1-depth][x] {
			return nil
		}
		return sink(logoColor(x, width), background, reflectionFade[depth])
	}

	rows := make([]string, len(lit))
	for r := range rows {
		var b strings.Builder
		for x := range width {
			top, bottom := half(2*r, x), half(2*r+1, x)
			switch {
			case top == nil && bottom == nil:
				b.WriteString(" ")
			case top == bottom:
				b.WriteString(lipgloss.NewStyle().Foreground(top).Render("█"))
			case top == nil:
				b.WriteString(lipgloss.NewStyle().Foreground(bottom).Render("▄"))
			case bottom == nil:
				b.WriteString(lipgloss.NewStyle().Foreground(top).Render("▀"))
			default:
				b.WriteString(lipgloss.NewStyle().Foreground(top).Background(bottom).Render("▀"))
			}
		}
		rows[r] = b.String()
	}
	return rows
}

// reflectionFade is how much of the wordmark's color each half row of its
// reflection keeps, from the baseline down.
var reflectionFade = []float64{0.22, 0.14, 0.085, 0.045, 0.02}

// heroLetterRows is how many of heroLettering's rows hold the letters; the
// rest are the reflection.
const heroLetterRows = 3

// sink mixes a color into a surface until only keep of it shows: a rule that
// recedes, a reflection, the light a lit edge throws.
func sink(c, surface color.Color, keep float64) color.RGBA {
	from, to := rgbaOf(surface), rgbaOf(c)
	mix := func(x, y uint8) uint8 { return uint8(float64(x) + (float64(y)-float64(x))*keep + 0.5) }
	return color.RGBA{mix(from.R, to.R), mix(from.G, to.G), mix(from.B, to.B), 0xFF}
}

func rgbaOf(c color.Color) color.RGBA {
	r, g, b, _ := c.RGBA()
	return color.RGBA{uint8(r >> 8), uint8(g >> 8), uint8(b >> 8), 0xFF}
}

// heroWidth is the cell width of heroLettering(gap).
func heroWidth(gap int) int {
	width := gap * (len(logoLetters) - 1)
	for _, letter := range logoLetters {
		width += len(letter[0])
	}
	return width
}

// ribbon paints text cell by cell along the brand's gradient, as the name in
// the masthead is. ribbonOn does the same over a surface's background, for
// the composer's lit edge.
func ribbon(text string) string { return ribbonOn(nil, text) }

func ribbonOn(bg color.Color, text string) string {
	runes := []rune(text)
	var b strings.Builder
	for i, r := range runes {
		style := lipgloss.NewStyle().Foreground(logoColor(i, len(runes)))
		if bg != nil {
			style = style.Background(bg)
		}
		b.WriteString(style.Render(string(r)))
	}
	return b.String()
}

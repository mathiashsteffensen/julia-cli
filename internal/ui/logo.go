package ui

import (
	"image/color"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// Proportional, chamfered letters drawn with quadrant blocks in 23 columns.
// The narrow I gives the V room for two diagonal strokes and a centered point.
// Vertical strokes are two pixels wide to balance the taller terminal cells.
var logoLetters = [][6]string{
	{"....##", "....##", "....##", "....##", "##..##", ".####."},             // J
	{".####.", "##..##", "##..##", "######", "##..##", "##..##"},             // A
	{"#####.", "##..##", "#####.", "##.##.", "##..##", "##..##"},             // R
	{"##....##", "##....##", ".##..##.", ".##..##.", "..####..", "...##..."}, // V
	{"####", ".##.", ".##.", ".##.", ".##.", "####"},                         // I
	{".#####", "##....", ".####.", "....##", "....##", "#####."},             // S
}

// quadrants maps a cell's pixels (top-left, top-right, bottom-left,
// bottom-right as bits 3 to 0) to the block element that draws them.
var quadrants = []string{" ", "▗", "▖", "▄", "▝", "▐", "▞", "▟", "▘", "▚", "▌", "▙", "▀", "▜", "▛", "█"}

// Warm amber through the interface's coral to burnt orange.
var (
	logoGradient = []color.RGBA{{0xF9, 0xD3, 0x99, 0xFF}, {0xF4, 0x9D, 0x89, 0xFF}, {0xE9, 0x78, 0x4A, 0xFF}}
	logoRule     = lipgloss.Color("#A16C50")
)

func logoColor(column, width int) color.Color {
	t := float64(column) / float64(max(1, width-1)) * float64(len(logoGradient)-1)
	i := min(int(t), len(logoGradient)-2)
	a, b, f := logoGradient[i], logoGradient[i+1], t-float64(i)
	mix := func(x, y uint8) uint8 { return uint8(float64(x) + (float64(y)-float64(x))*f + 0.5) }
	return color.RGBA{mix(a.R, b.R), mix(a.G, b.G), mix(a.B, b.B), 0xFF}
}

// logoLettering shades the wordmark column by column without relying on a
// terminal's bold font or graphics protocol.
func logoLettering() []string {
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
				b.WriteString(" ")
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
				b.WriteString(lipgloss.NewStyle().Foreground(logoColor(column, width)).Render(cell))
				column++
			}
		}
		rows[row] = b.String()
	}
	return rows
}

// heroLettering draws the wordmark for the welcome screen: each pixel a cell
// wide and half a cell tall, so the letters stand twice as wide as the
// sidebar's and their strokes read as blocks rather than quadrant stipple.
// gap is the number of cells between letters.
func heroLettering(gap int) []string {
	width := 0
	for i, letter := range logoLetters {
		if i > 0 {
			width += gap
		}
		width += len(letter[0])
	}
	rows := make([]string, len(logoLetters[0])/2)
	for row := range rows {
		var b strings.Builder
		column := 0
		for i, letter := range logoLetters {
			if i > 0 {
				b.WriteString(strings.Repeat(" ", gap))
				column += gap
			}
			for x := range len(letter[0]) {
				top, bottom := letter[2*row][x] == '#', letter[2*row+1][x] == '#'
				cell := " "
				switch {
				case top && bottom:
					cell = "█"
				case top:
					cell = "▀"
				case bottom:
					cell = "▄"
				}
				b.WriteString(lipgloss.NewStyle().Foreground(logoColor(column, width)).Render(cell))
				column++
			}
		}
		rows[row] = b.String()
	}
	return rows
}

// heroWidth is the cell width of heroLettering(gap).
func heroWidth(gap int) int {
	width := gap * (len(logoLetters) - 1)
	for _, letter := range logoLetters {
		width += len(letter[0])
	}
	return width
}

// sidebarLogo keeps a blank row between each rail and the lettering at every
// terminal height. Collapsing those rows makes the diagonal rails touch the mark.
func sidebarLogo(version string, width int) []string {
	letters := logoLettering()
	rail := lipgloss.NewStyle().Foreground(logoRule)
	crest := strings.Repeat("╱", (width-5)/2)
	header := rail.Render(crest) + "  " + coralStyle.Render("◈") + "  " + rail.Render(crest)
	footer := rail.Render(strings.Repeat("╱", width))
	if version != "" {
		version = ansi.Truncate("v"+strings.Join(strings.Fields(sanitize(version)), " "), width, "")
		gap := min(2, width-ansi.StringWidth(version))
		rule := strings.Repeat("╱", width-ansi.StringWidth(version)-gap)
		footer = rail.Render(rule) + strings.Repeat(" ", gap) + mutedStyle.Render(version)
	}
	rows := append([]string{header, ""}, letters...)
	return append(rows, "", footer)
}

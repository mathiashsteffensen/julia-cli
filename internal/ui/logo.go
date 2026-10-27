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
// A drop shadow one pixel right and below, in the gradient dimmed almost to
// the background, lifts the mark off the screen. gap is the number of cells
// between letters. The result is heroWidth(gap) cells wide and four rows tall;
// the last row holds only the shadow's lower edge.
func heroLettering(gap int) []string {
	const (
		none = iota
		shadow
		letter
	)
	width := heroWidth(gap)
	pixels := make([][]int, len(logoLetters[0])+2) // the letters' six rows, the shadow's, and a blank to fill the cell
	for i := range pixels {
		pixels[i] = make([]int, width)
	}
	column := 0
	for i, l := range logoLetters {
		if i > 0 {
			column += gap
		}
		for y, row := range l {
			for x := range row {
				if row[x] != '#' {
					continue
				}
				pixels[y][column+x] = letter
				if pixels[y+1][column+x+1] == none {
					pixels[y+1][column+x+1] = shadow
				}
			}
		}
		column += len(l[0])
	}
	// The shadow sits on cells whose top or bottom pixel belongs to a letter,
	// so a cell may need two colors: the letter in the foreground and the
	// shadow painted as the background behind a half block.
	rows := make([]string, len(pixels)/2)
	for r := range rows {
		var b strings.Builder
		for c := range width {
			top, bottom := pixels[2*r][c], pixels[2*r+1][c]
			fg, bg := logoColor(c, width), shadowColor(c, width)
			var style lipgloss.Style
			var cell string
			switch {
			case top == none && bottom == none:
				cell = " "
			case top == bottom:
				cell, style = "█", lipgloss.NewStyle().Foreground(heroInk(top, fg, bg))
			case top == none:
				cell, style = "▄", lipgloss.NewStyle().Foreground(heroInk(bottom, fg, bg))
			case bottom == none:
				cell, style = "▀", lipgloss.NewStyle().Foreground(heroInk(top, fg, bg))
			case top == letter:
				cell, style = "▀", lipgloss.NewStyle().Foreground(fg).Background(bg)
			default:
				cell, style = "▄", lipgloss.NewStyle().Foreground(fg).Background(bg)
			}
			if cell == " " {
				b.WriteString(cell)
				continue
			}
			b.WriteString(style.Render(cell))
		}
		rows[r] = b.String()
	}
	return rows
}

// heroInk is the color of a hero pixel: the letter's, or its shadow's.
func heroInk(kind int, letter, shadow color.Color) color.Color {
	if kind == 2 {
		return letter
	}
	return shadow
}

// shadowColor is the wordmark's gradient sunk most of the way into the
// background, so the shadow reads as depth rather than as a second mark.
func shadowColor(column, width int) color.Color {
	c := logoColor(column, width).(color.RGBA)
	bg := color.RGBA{0x15, 0x13, 0x1A, 0xFF}
	mix := func(x, y uint8) uint8 { return uint8(float64(y) + (float64(x)-float64(y))*0.12 + 0.5) }
	return color.RGBA{mix(c.R, bg.R), mix(c.G, bg.G), mix(c.B, bg.B), 0xFF}
}

// heroWidth is the cell width of heroLettering(gap), one more than the
// letters for the shadow's right edge.
func heroWidth(gap int) int {
	width := gap*(len(logoLetters)-1) + 1
	for _, letter := range logoLetters {
		width += len(letter[0])
	}
	return width
}

// ribbon paints text cell by cell along the brand's gradient: the masthead's
// rule and name. ribbonOn does the same over a surface's background, for the
// composer's top edge.
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

package terminal

import (
	"image"
	"math"
	"strings"
	"sync"

	"golang.org/x/image/vector"
)

// specialGlyph draws box-drawing (U+2500–257F), block (U+2580–259F), braille
// (U+2800–28FF) and Powerline (U+E0B0–E0B3) characters as an alpha mask of
// w×h. Fonts leave gaps between cells and Go Mono lacks most of these, so
// terminals draw them per cell, letting borders join seamlessly. It returns
// nil for other runes.
func specialGlyph(r rune, w, h, line int) *image.Alpha {
	if !special(r) {
		return nil
	}
	key := specialKey{r, w, h, line}
	specials.Lock()
	defer specials.Unlock()
	if mask, ok := specials.cache[key]; ok {
		return mask
	}
	c := canvas{image.NewAlpha(image.Rect(0, 0, w, h))}
	switch {
	case r >= 0x2580:
		if r <= 0x259f {
			c.block(r)
		} else if r >= 0xe0b0 {
			c.powerline(r, line)
		} else {
			c.braille(r)
		}
	default:
		c.box(r, line)
	}
	if len(specials.cache) >= 1024 {
		clear(specials.cache)
	}
	specials.cache[key] = c.Alpha
	return c.Alpha
}

type specialKey struct {
	r          rune
	w, h, line int
}

var specials = struct {
	sync.Mutex
	cache map[specialKey]*image.Alpha
}{cache: map[specialKey]*image.Alpha{}}

type canvas struct{ *image.Alpha }

// special reports the runes specialGlyph draws.
func special(r rune) bool {
	return r >= 0x2500 && r <= 0x259f || r >= 0x2800 && r <= 0x28ff || r >= 0xe0b0 && r <= 0xe0b3
}

func (c canvas) fill(x0, y0, x1, y1 int) {
	b := c.Bounds()
	x0, y0, x1, y1 = max(x0, 0), max(y0, 0), min(x1, b.Max.X), min(y1, b.Max.Y)
	for y := y0; y < y1; y++ {
		row := c.Pix[y*c.Stride:]
		for x := x0; x < x1; x++ {
			row[x] = 0xff
		}
	}
}

// cover raises a pixel's coverage to a (0..1); strokes overlap by maximum.
func cover(m *image.Alpha, x, y int, a float64) {
	if a <= 0 || !image.Pt(x, y).In(m.Bounds()) {
		return
	}
	v := uint8(math.Round(min(a, 1) * 255))
	if i := m.PixOffset(x, y); m.Pix[i] < v {
		m.Pix[i] = v
	}
}

// Box drawing weights per arm: 0 none, 1 light, 2 heavy, 3 double.
const (
	none, light, heavy, double = 0, 1, 2, 3
)

// boxArms lists U+2500–257F as up, right, down, left weights. Dashed lines,
// arcs and diagonals are listed by their arms and drawn specially.
var boxArms = strings.Fields(`
0101 0202 1010 2020 0101 0202 1010 2020 0101 0202 1010 2020 0110 0210 0120 0220
0011 0012 0021 0022 1100 1200 2100 2200 1001 1002 2001 2002 1110 1210 2110 1120
2120 2210 1220 2220 1011 1012 2011 1021 2021 2012 1022 2022 0111 0112 0211 0212
0121 0122 0221 0222 1101 1102 1201 1202 2101 2102 2201 2202 1111 1112 1211 1212
2111 1121 2121 2112 2211 1122 1221 2212 1222 2122 2221 2222 0101 0202 1010 2020
0303 3030 0310 0130 0330 0013 0031 0033 1300 3100 3300 1003 3001 3003 1310 3130
3330 1013 3031 3033 0313 0131 0333 1303 3101 3303 1313 3131 3333 0110 0011 1001
1100 0000 0000 0000 0001 1000 0100 0010 0002 2000 0200 0020 0201 1020 0102 2010`)

func (c canvas) box(r rune, t int) {
	arms := boxArms[r-0x2500]
	var a [4]int
	for i := range a {
		a[i] = int(arms[i] - '0')
	}
	w, h := c.Bounds().Dx(), c.Bounds().Dy()
	switch {
	case r >= 0x2504 && r <= 0x250b, r >= 0x254c && r <= 0x254f:
		dashes := 4
		switch {
		case r <= 0x2507:
			dashes = 3
		case r >= 0x254c:
			dashes = 2
		}
		c.dashed(a, t, dashes)
		return
	case r >= 0x256d && r <= 0x2570:
		c.arc(r, t)
		return
	case r >= 0x2571 && r <= 0x2573:
		c.diagonals(r, t)
		return
	}
	horizontal := axis{c: c, along: w / 2, across: h / 2, length: w, t: t}
	vertical := axis{c: c, along: h / 2, across: w / 2, length: h, t: t, transposed: true}
	up, right, down, left := a[0], a[1], a[2], a[3]
	horizontal.arm(right, left, up, down, true)
	horizontal.arm(left, right, up, down, false)
	vertical.arm(down, up, left, right, true)
	vertical.arm(up, down, left, right, false)
}

// axis draws arms along one direction; a vertical axis is the horizontal one
// transposed, with "before" the side of smaller coordinates (up or left).
type axis struct {
	c                     canvas
	along, across, length int
	t                     int
	transposed            bool
}

func (x axis) fill(a0, b0, a1, b1 int) {
	if x.transposed {
		a0, b0, a1, b1 = b0, a0, b1, a1
	}
	x.c.fill(a0, b0, a1, b1)
}

func (x axis) thickness(weight int) int {
	switch weight {
	case none:
		return 0
	case heavy:
		return 2 * x.t
	}
	return x.t
}

func lo(centre, t int) int { return centre - t/2 }
func hi(centre, t int) int { return lo(centre, t) + t }

// arm draws an arm of the given weight from the centre forward (right or
// down) or backward (left or up). opposite is the arm on the other side;
// before and after are the perpendicular arms.
func (x axis) arm(weight, opposite, before, after int, forward bool) {
	if weight == none {
		return
	}
	t, o, ca := x.t, x.t, x.along
	single := func(w int) bool { return w == light || w == heavy }
	// line runs from the stop near the centre to the end of the cell, at
	// across-position centre.
	line := func(stop, centre, thick int) {
		if forward {
			x.fill(stop, lo(centre, thick), x.length, hi(centre, thick))
		} else {
			x.fill(0, lo(centre, thick), stop, hi(centre, thick))
		}
	}
	pick := func(start, end int) int {
		if forward {
			return start
		}
		return end
	}
	if single(weight) {
		var stop int
		switch {
		case before == double && after == double && opposite != none: // crosses ║ or ═
			stop = pick(lo(ca, t), hi(ca, t))
		case before == double && after == double: // meets its near line
			stop = pick(lo(ca+o, t), hi(ca-o, t))
		case before == double || after == double: // reaches the corner's far line
			stop = pick(lo(ca-o, t), hi(ca+o, t))
		default: // covers the perpendicular stroke, so corners are square
			ext := max(x.thickness(before), x.thickness(after))
			if ext == 0 {
				ext = x.thickness(weight)
			}
			stop = pick(lo(ca, ext), hi(ca, ext))
		}
		line(stop, x.across, x.thickness(weight))
		return
	}
	// A double arm is two lines; each stops where it meets the perpendicular
	// arm on its own side, or turns the corner into it.
	meet := func(own, other int) int {
		switch {
		case own == double:
			return pick(lo(ca+o, t), hi(ca-o, t))
		case single(own):
			return pick(lo(ca, x.thickness(own)), hi(ca, x.thickness(own)))
		case single(other):
			return pick(lo(ca, x.thickness(other)), hi(ca, x.thickness(other)))
		}
		return pick(lo(ca-o, t), hi(ca+o, t))
	}
	line(meet(before, after), x.across-o, t)
	line(meet(after, before), x.across+o, t)
}

func (c canvas) dashed(a [4]int, t, dashes int) {
	w, h := c.Bounds().Dx(), c.Bounds().Dy()
	thick := t
	if a[0] == heavy || a[1] == heavy {
		thick = 2 * t
	}
	vertical, length := a[0] != none, w
	if vertical {
		length = h
	}
	period := float64(length) / float64(dashes)
	gap := max(1, period*0.35)
	for i := range dashes {
		start := int(math.Round(float64(i)*period + gap/2))
		end := int(math.Round(float64(i+1)*period - gap/2))
		if vertical {
			c.fill(lo(w/2, thick), start, hi(w/2, thick), end)
		} else {
			c.fill(start, lo(h/2, thick), end, hi(h/2, thick))
		}
	}
}

// arc draws a rounded corner whose straight ends line up with ─ and │.
func (c canvas) arc(r rune, t int) {
	w, h := c.Bounds().Dx(), c.Bounds().Dy()
	dx, dy := 1, 1 // ╭ turns from down to right.
	switch r {
	case 0x256e:
		dx = -1
	case 0x256f:
		dx, dy = -1, -1
	case 0x2570:
		dy = -1
	}
	// Line centres as real coordinates, matching lo/hi rounding.
	cx := float64(lo(w/2, t)) + float64(t)/2
	cy := float64(lo(h/2, t)) + float64(t)/2
	radius := math.Min(math.Min(cx, float64(w)-cx), math.Min(cy, float64(h)-cy))
	ox, oy := cx+float64(dx)*radius, cy+float64(dy)*radius
	for py := range h {
		for px := range w {
			x, y := float64(px)+0.5, float64(py)+0.5
			if (x-ox)*float64(dx) > 0 || (y-oy)*float64(dy) > 0 {
				continue
			}
			d := math.Hypot(x-ox, y-oy)
			cover(c.Alpha, px, py, float64(t)/2+0.5-math.Abs(d-radius))
		}
	}
	if dy > 0 {
		c.fill(lo(w/2, t), int(math.Floor(oy)), hi(w/2, t), h)
	} else {
		c.fill(lo(w/2, t), 0, hi(w/2, t), int(math.Ceil(oy)))
	}
	if dx > 0 {
		c.fill(int(math.Floor(ox)), lo(h/2, t), w, hi(h/2, t))
	} else {
		c.fill(0, lo(h/2, t), int(math.Ceil(ox)), hi(h/2, t))
	}
}

func (c canvas) diagonals(r rune, t int) {
	w, h := float64(c.Bounds().Dx()), float64(c.Bounds().Dy())
	length := math.Hypot(w, h)
	for py := range c.Bounds().Dy() {
		for px := range c.Bounds().Dx() {
			x, y := float64(px)+0.5, float64(py)+0.5
			rising := math.Abs(h*x+w*y-w*h) / length // ╱ from bottom left to top right
			falling := math.Abs(h*x-w*y) / length    // ╲ from top left to bottom right
			d := rising
			switch r {
			case 0x2572:
				d = falling
			case 0x2573:
				d = math.Min(rising, falling)
			}
			cover(c.Alpha, px, py, float64(t)/2+0.5-d)
		}
	}
}

func (c canvas) block(r rune) {
	w, h := c.Bounds().Dx(), c.Bounds().Dy()
	eighthY := func(n int) int { return int(math.Round(float64(h*n) / 8)) }
	eighthX := func(n int) int { return int(math.Round(float64(w*n) / 8)) }
	switch {
	case r == 0x2580:
		c.fill(0, 0, w, eighthY(4))
	case r >= 0x2581 && r <= 0x2588:
		c.fill(0, h-eighthY(int(r-0x2580)), w, h)
	case r >= 0x2589 && r <= 0x258f:
		c.fill(0, 0, eighthX(int(0x2590-r)), h)
	case r == 0x2590:
		c.fill(eighthX(4), 0, w, h)
	case r >= 0x2591 && r <= 0x2593:
		alpha := uint8(0x40 * (r - 0x2590))
		for i := range c.Pix {
			c.Pix[i] = alpha
		}
	case r == 0x2594:
		c.fill(0, 0, w, eighthY(1))
	case r == 0x2595:
		c.fill(w-eighthX(1), 0, w, h)
	default: // Quadrants: bits are upper left, upper right, lower left, lower right.
		quads := map[rune]int{0x2596: 4, 0x2597: 8, 0x2598: 1, 0x2599: 1 | 4 | 8, 0x259a: 1 | 8, 0x259b: 1 | 2 | 4, 0x259c: 1 | 2 | 8, 0x259d: 2, 0x259e: 2 | 4, 0x259f: 2 | 4 | 8}[r]
		mx, my := eighthX(4), eighthY(4)
		for i, q := range [4][4]int{{0, 0, mx, my}, {mx, 0, w, my}, {0, my, mx, h}, {mx, my, w, h}} {
			if quads&(1<<i) != 0 {
				c.fill(q[0], q[1], q[2], q[3])
			}
		}
	}
}

// braille draws the dots of a 2×4 braille pattern as round, evenly spaced
// points that tile into a continuous grid across cells.
func (c canvas) braille(r rune) {
	w, h := float64(c.Bounds().Dx()), float64(c.Bounds().Dy())
	bits := int(r - 0x2800)
	// Dot numbering: 1 2 3 7 run down the left column, 4 5 6 8 the right.
	dots := [8][2]int{{0, 0}, {0, 1}, {0, 2}, {1, 0}, {1, 1}, {1, 2}, {0, 3}, {1, 3}}
	radius := math.Min(w/4, h/8) * 0.62
	for i, d := range dots {
		if bits&(1<<i) == 0 {
			continue
		}
		cx, cy := w*float64(2*d[0]+1)/4, h*float64(2*d[1]+1)/8
		for py := int(cy - radius - 1); py <= int(cy+radius+1); py++ {
			for px := int(cx - radius - 1); px <= int(cx+radius+1); px++ {
				dist := math.Hypot(float64(px)+0.5-cx, float64(py)+0.5-cy)
				cover(c.Alpha, px, py, radius+0.5-dist)
			}
		}
	}
}

// powerline draws the solid and outlined arrows status lines are built from.
func (c canvas) powerline(r rune, t int) {
	w, h := float32(c.Bounds().Dx()), float32(c.Bounds().Dy())
	z := vector.NewRasterizer(c.Bounds().Dx(), c.Bounds().Dy())
	flip := func(x float32) float32 {
		if r == 0xe0b2 || r == 0xe0b3 {
			return w - x
		}
		return x
	}
	switch r {
	case 0xe0b0, 0xe0b2:
		z.MoveTo(flip(0), 0)
		z.LineTo(flip(w), h/2)
		z.LineTo(flip(0), h)
		z.ClosePath()
	default: // A chevron: the triangle's two edges, t thick.
		s := float32(t)
		z.MoveTo(flip(0), 0)
		z.LineTo(flip(w), h/2)
		z.LineTo(flip(0), h)
		z.LineTo(flip(0), h-s*2)
		z.LineTo(flip(w-s*1.5), h/2)
		z.LineTo(flip(0), s*2)
		z.ClosePath()
	}
	z.Draw(c.Alpha, c.Bounds(), image.Opaque, image.Point{})
}

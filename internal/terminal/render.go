package terminal

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"unicode"
	"unicode/utf8"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
)

// The cell size at the largest font, reported to programs that ask (XTWINOPS,
// the PTY's pixel size) and used for SGR-pixel mouse reports.
const (
	cellWidth  = 15
	cellHeight = 30
)

// RenderOptions tune Snapshot.Render.
type RenderOptions struct {
	// MaxWidth and MaxHeight bound the image in pixels; the font steps down
	// from 25 px to fit. Zero means 2000, the size images are sent at.
	MaxWidth, MaxHeight int
	// HideCursor leaves the cursor out even when the program shows it.
	HideCursor bool
}

// PNG renders the snapshot and encodes it as PNG.
func (s Snapshot) PNG(opts RenderOptions) ([]byte, error) {
	var buf bytes.Buffer
	err := (&png.Encoder{CompressionLevel: png.BestSpeed}).Encode(&buf, s.Render(opts))
	return buf.Bytes(), err
}

// Render draws the screen as a terminal would show it: Go Mono text in its
// colours and attributes, box drawing, block and braille characters drawn per
// cell so lines join across cells, the cursor, and a margin of background.
func (s Snapshot) Render(opts RenderOptions) *image.RGBA {
	maxW, maxH := opts.MaxWidth, opts.MaxHeight
	if maxW <= 0 {
		maxW = 2000
	}
	if maxH <= 0 {
		maxH = 2000
	}
	all := metricsFor()
	m := all[fontSizes[len(fontSizes)-1]]
	for _, size := range fontSizes {
		candidate := all[size]
		if (s.Cols+2)*candidate.width <= maxW && s.Rows*candidate.height+2*candidate.width <= maxH {
			m = candidate
			break
		}
	}
	pad := m.width
	img := image.NewRGBA(image.Rect(0, 0, s.Cols*m.width+2*pad, s.Rows*m.height+2*pad))
	draw.Draw(img, img.Bounds(), image.NewUniform(s.bg), image.Point{}, draw.Src)
	r := renderer{img: img, m: m, pad: pad, snap: &s}

	for pass := range 2 {
		for row := range s.Rows {
			for col := range s.Cols {
				c := &s.cells[row*s.Cols+col]
				if c.width == 0 && c.text == "" {
					continue
				}
				fg, bg := s.colors(c)
				if pass == 0 {
					if bg != s.bg {
						r.fill(r.cellRect(row, col, max(c.width, 1)), bg)
					}
					continue
				}
				r.cell(row, col, c, fg)
			}
		}
	}
	if s.CursorVisible && !opts.HideCursor && !s.State.Exited {
		r.cursor()
	}
	return img
}

// Missing lists the characters on the screen that no font here has, mostly
// emoji, which Render draws as tofu boxes although the text has them right.
func (s Snapshot) Missing() []string {
	var missing []string
	seen := map[rune]bool{}
	for i := range s.cells {
		c := &s.cells[i]
		r, _ := utf8.DecodeRuneInString(c.text)
		if c.text == "" || r == ' ' || r == utf8.RuneError || c.attrs&uv.AttrConceal != 0 || special(r) || seen[r] {
			continue
		}
		seen[r] = true
		if !hasGlyph(r) {
			missing = append(missing, c.text)
		}
	}
	return missing
}

// colors resolves a cell's attributes into the colours it is drawn with.
func (s *Snapshot) colors(c *cell) (fg, bg color.RGBA) {
	fg, bg = c.fg, c.bg
	if c.attrs&uv.AttrReverse != 0 {
		fg, bg = bg, fg
	}
	if c.attrs&uv.AttrFaint != 0 {
		fg = mix(fg, bg, 0.5)
	}
	return fg, bg
}

type renderer struct {
	img  *image.RGBA
	m    metrics
	pad  int
	snap *Snapshot
}

func (r *renderer) cellRect(row, col, width int) image.Rectangle {
	x, y := r.pad+col*r.m.width, r.pad+row*r.m.height
	return image.Rect(x, y, x+width*r.m.width, y+r.m.height)
}

func (r *renderer) fill(rect image.Rectangle, c color.RGBA) {
	draw.Draw(r.img, rect, image.NewUniform(c), image.Point{}, draw.Src)
}

func (r *renderer) cell(row, col int, c *cell, fg color.RGBA) {
	rect := r.cellRect(row, col, max(c.width, 1))
	if c.attrs&uv.AttrConceal == 0 {
		r.glyph(rect, c, fg)
	}
	r.decorate(rect, c, fg)
}

// glyph draws a cell's text: procedurally for drawing characters, from a
// font otherwise, and as a tofu box when no font has it.
func (r *renderer) glyph(rect image.Rectangle, c *cell, fg color.RGBA) {
	first, size := utf8.DecodeRuneInString(c.text)
	if first == utf8.RuneError || first == ' ' || c.text == "" {
		return
	}
	if mask := specialGlyph(first, rect.Dx(), rect.Dy(), r.m.line); mask != nil {
		draw.DrawMask(r.img, rect, image.NewUniform(fg), image.Point{}, mask, image.Point{}, draw.Over)
		return
	}
	style := 0
	if c.attrs&uv.AttrBold != 0 {
		style |= 1
	}
	if c.attrs&uv.AttrItalic != 0 {
		style |= 2
	}
	origin := image.Pt(rect.Min.X, rect.Min.Y+r.m.ascent)
	g := r.lookup(first, style, rect.Dx())
	if g == nil {
		r.tofu(rect, fg)
		return
	}
	src := image.NewUniform(fg)
	at := origin.Add(image.Pt((rect.Dx()-g.advance)/2, 0))
	draw.DrawMask(r.img, g.mask.Bounds().Add(at), src, image.Point{}, g.mask, g.mask.Bounds().Min, draw.Over)
	for _, mark := range c.text[size:] { // Combining marks share the base's origin.
		if !unicode.In(mark, unicode.Mn, unicode.Me) {
			continue
		}
		if g := r.lookup(mark, style, rect.Dx()); g != nil {
			draw.DrawMask(r.img, g.mask.Bounds().Add(at), src, image.Point{}, g.mask, g.mask.Bounds().Min, draw.Over)
		}
	}
}

// lookup finds r in Go Mono's style, then in the fallback fonts, shrinking a
// fallback glyph that would spill out of its cells.
func (r *renderer) lookup(ch rune, style, span int) *glyph {
	if g := lookupGlyph(goMono()[style], r.m.size, ch); g != nil {
		return g
	}
	for _, f := range fallbackFonts() {
		g := lookupGlyph(f, r.m.size, ch)
		if g == nil {
			continue
		}
		if g.advance > span {
			if smaller := lookupGlyph(f, math.Floor(r.m.size*float64(span)/float64(g.advance)), ch); smaller != nil {
				return smaller
			}
		}
		return g
	}
	return nil
}

func (r *renderer) tofu(rect image.Rectangle, fg color.RGBA) {
	inset := rect.Inset(max(1, r.m.width/6))
	inset.Min.Y, inset.Max.Y = rect.Min.Y+r.m.height/5, rect.Max.Y-r.m.height/5
	c := mix(fg, r.snap.bg, 0.4)
	line := max(1, r.m.line/2)
	for _, edge := range []image.Rectangle{
		{inset.Min, image.Pt(inset.Max.X, inset.Min.Y+line)},
		{image.Pt(inset.Min.X, inset.Max.Y-line), inset.Max},
		{inset.Min, image.Pt(inset.Min.X+line, inset.Max.Y)},
		{image.Pt(inset.Max.X-line, inset.Min.Y), inset.Max},
	} {
		r.fill(edge, c)
	}
}

// decorate draws underlines and strikethrough.
func (r *renderer) decorate(rect image.Rectangle, c *cell, fg color.RGBA) {
	line := r.m.line
	if c.attrs&uv.AttrStrikethrough != 0 {
		y := rect.Min.Y + r.m.ascent - r.m.xHeight/2 - line/2
		r.fill(image.Rect(rect.Min.X, y, rect.Max.X, y+line), fg)
	}
	if c.underline == ansi.UnderlineNone {
		return
	}
	ul := fg
	if c.ul.A != 0 {
		ul = c.ul
	}
	y := rect.Min.Y + r.m.ascent + max(1, r.m.descent/3)
	switch c.underline {
	case ansi.UnderlineDouble:
		y -= line
		r.fill(image.Rect(rect.Min.X, y, rect.Max.X, y+line), ul)
		r.fill(image.Rect(rect.Min.X, y+2*line, rect.Max.X, y+3*line), ul)
	case ansi.UnderlineCurly:
		amplitude := float64(max(1, r.m.descent/3))
		mask := image.NewAlpha(image.Rect(0, 0, rect.Dx(), r.m.height))
		for x := range rect.Dx() {
			centre := float64(y-rect.Min.Y) + amplitude*math.Sin(2*math.Pi*(float64(x)+0.5)/float64(r.m.width))
			for py := range r.m.height {
				cover(mask, x, py, float64(line)/2+0.5-math.Abs(float64(py)+0.5-centre))
			}
		}
		draw.DrawMask(r.img, rect, image.NewUniform(ul), image.Point{}, mask, image.Point{}, draw.Over)
	case ansi.UnderlineDotted:
		for x := rect.Min.X; x < rect.Max.X; x += 2 * line {
			r.fill(image.Rect(x, y, x+line, y+line), ul)
		}
	case ansi.UnderlineDashed:
		for x := rect.Min.X; x < rect.Max.X; x += r.m.width {
			r.fill(image.Rect(x+r.m.width/4, y, x+r.m.width*3/4, y+line), ul)
		}
	default:
		r.fill(image.Rect(rect.Min.X, y, rect.Max.X, y+line), ul)
	}
}

func (r *renderer) cursor() {
	s := r.snap
	row, col := s.Cursor.Row-1, s.Cursor.Col-1
	if row < 0 || col < 0 || row >= s.Rows || col >= s.Cols {
		return
	}
	c := &s.cells[row*s.Cols+col]
	if c.width == 0 && c.text == "" && col > 0 { // On a wide character's second half.
		col--
		c = &s.cells[row*s.Cols+col]
	}
	rect := r.cellRect(row, col, max(c.width, 1))
	switch s.cursorKind {
	case vt.CursorBar:
		r.fill(image.Rect(rect.Min.X, rect.Min.Y, rect.Min.X+max(2, r.m.line), rect.Max.Y), s.cur)
	case vt.CursorUnderline:
		r.fill(image.Rect(rect.Min.X, rect.Max.Y-max(2, r.m.line), rect.Max.X, rect.Max.Y), s.cur)
	default:
		_, bg := s.colors(c)
		r.fill(rect, s.cur)
		r.cell(row, col, c, bg)
	}
}

func mix(a, b color.RGBA, t float64) color.RGBA {
	lerp := func(x, y uint8) uint8 { return uint8(math.Round(float64(x)*(1-t) + float64(y)*t)) }
	return color.RGBA{lerp(a.R, b.R), lerp(a.G, b.G), lerp(a.B, b.B), 0xff}
}

package terminal

import (
	"image"
	"image/draw"
	"os"
	"sync"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gomono"
	"golang.org/x/image/font/gofont/gomonobold"
	"golang.org/x/image/font/gofont/gomonobolditalic"
	"golang.org/x/image/font/gofont/gomonoitalic"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/font/sfnt"
	"golang.org/x/image/math/fixed"
)

// Go Mono draws text in four styles. Glyphs it lacks (✓, ❯, CJK) come from
// the first system font that has them; anything else becomes a tofu box.
var (
	goMono = sync.OnceValue(func() [4]*sfnt.Font {
		var faces [4]*sfnt.Font
		for i, ttf := range [][]byte{gomono.TTF, gomonobold.TTF, gomonoitalic.TTF, gomonobolditalic.TTF} {
			f, err := opentype.Parse(ttf)
			if err != nil {
				panic("terminal: embedded Go font: " + err.Error())
			}
			faces[i] = f
		}
		return faces
	})
	fallbackFonts = sync.OnceValue(func() []*sfnt.Font {
		var fonts []*sfnt.Font
		for _, path := range fallbackPaths {
			file, err := os.Open(path)
			if err != nil {
				continue
			}
			collection, err := sfnt.ParseCollectionReaderAt(file)
			if err != nil {
				file.Close()
				continue
			}
			f, err := collection.Font(0)
			if err != nil {
				file.Close()
				continue
			}
			fonts = append(fonts, f) // The file stays open: glyphs load lazily.
		}
		return fonts
	})
)

// fallbackPaths are monospace and symbol fonts first, then broad coverage.
var fallbackPaths = []string{
	"/System/Library/Fonts/Menlo.ttc",
	"/System/Library/Fonts/Apple Symbols.ttf",
	"/System/Library/Fonts/Supplemental/Arial Unicode.ttf",
	"/usr/share/fonts/truetype/dejavu/DejaVuSansMono.ttf",
	"/usr/share/fonts/TTF/DejaVuSansMono.ttf",
	"/usr/share/fonts/truetype/noto/NotoSansSymbols2-Regular.ttf",
	"/usr/share/fonts/opentype/noto/NotoSansCJK-Regular.ttc",
	"/usr/share/fonts/noto-cjk/NotoSansCJK-Regular.ttc",
}

// glyph is a rasterized glyph positioned relative to its baseline origin.
type glyph struct {
	mask    *image.Alpha
	advance int
}

type glyphKey struct {
	font *sfnt.Font
	size float64
	r    rune
}

// glyphs caches rasterized glyphs across renders. Faces are not safe for
// concurrent use, so all rasterizing happens under the lock.
var glyphs = struct {
	sync.Mutex
	faces  map[glyphKey]font.Face // keyed with r == 0
	cache  map[glyphKey]*glyph
	buffer sfnt.Buffer
}{faces: map[glyphKey]font.Face{}, cache: map[glyphKey]*glyph{}}

const maxCachedGlyphs = 8192

// lookupGlyph rasterizes r from f at size, or returns nil when f lacks it.
func lookupGlyph(f *sfnt.Font, size float64, r rune) *glyph {
	glyphs.Lock()
	defer glyphs.Unlock()
	key := glyphKey{f, size, r}
	if g, ok := glyphs.cache[key]; ok {
		return g
	}
	var g *glyph
	if index, err := f.GlyphIndex(&glyphs.buffer, r); err == nil && index != 0 {
		face := glyphs.faces[glyphKey{f, size, 0}]
		if face == nil {
			face, _ = opentype.NewFace(f, &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingFull})
			glyphs.faces[glyphKey{f, size, 0}] = face
		}
		if face != nil {
			if dr, mask, maskp, advance, ok := face.Glyph(fixed.Point26_6{}, r); ok {
				copied := image.NewAlpha(dr)
				draw.Draw(copied, dr, mask, maskp, draw.Src)
				g = &glyph{mask: copied, advance: advance.Round()}
			}
		}
	}
	if len(glyphs.cache) >= maxCachedGlyphs {
		clear(glyphs.cache)
	}
	glyphs.cache[key] = g
	return g
}

// hasGlyph reports whether Go Mono or a fallback font has r.
func hasGlyph(r rune) bool {
	glyphs.Lock()
	defer glyphs.Unlock()
	for _, f := range append([]*sfnt.Font{goMono()[0]}, fallbackFonts()...) {
		if index, err := f.GlyphIndex(&glyphs.buffer, r); err == nil && index != 0 {
			return true
		}
	}
	return false
}

// metrics are the cell geometry for a font size.
type metrics struct {
	size                  float64
	width, height, ascent int
	descent, xHeight      int
	line                  int // stroke width for rules and box drawing
}

var metricsFor = sync.OnceValue(func() map[float64]metrics {
	out := map[float64]metrics{}
	for _, size := range fontSizes {
		face, err := opentype.NewFace(goMono()[0], &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingFull})
		if err != nil {
			panic("terminal: Go Mono face: " + err.Error())
		}
		m := face.Metrics()
		advance, _ := face.GlyphAdvance('M')
		width := advance.Ceil()
		out[size] = metrics{
			size: size, width: width,
			ascent: m.Ascent.Ceil(), descent: m.Descent.Ceil(),
			height:  m.Ascent.Ceil() + m.Descent.Ceil(),
			xHeight: m.XHeight.Round(),
			line:    max(1, (width+4)/9),
		}
	}
	return out
})

// fontSizes are tried largest first until the image fits; each gives whole
// pixel cells (Go Mono's advance is 0.6 em).
var fontSizes = []float64{25, 20, 15, 10}

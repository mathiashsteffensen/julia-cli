// Package images turns what the user pastes or drops into the terminal into
// images every provider accepts.
package images

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	_ "image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"julia/internal/core"

	_ "golang.org/x/image/bmp"
	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"
)

const (
	// MaxEdge is the longest side, in pixels, an image is sent at. Providers
	// downscale anything larger themselves, so sending more only costs upload
	// time and session size.
	MaxEdge = 2000
	// maxBytes keeps an encoded image under the 5 MB base64 limit of the
	// strictest provider (Anthropic).
	maxBytes = 3_700_000
	// maxPixels refuses decompression bombs before they are decoded.
	maxPixels = 100_000_000
	// MaxFileBytes is the largest image file Load, or any tool, reads.
	MaxFileBytes = 64 << 20
)

// ErrNoImage reports a clipboard without an image on it.
var ErrNoImage = errors.New("no image on the clipboard")

// Prepare decodes PNG, JPEG, GIF, WebP, TIFF or BMP data and returns it ready to
// send: scaled down to MaxEdge on its long side, and re-encoded as PNG, or as
// JPEG when a PNG would be too large. Small PNG, JPEG and WebP images that
// already fit are sent unchanged. A GIF always becomes a PNG of its first
// frame: Gemini takes no GIF, and OpenAI no animated one.
func Prepare(data []byte) (core.Image, error) {
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return core.Image{}, fmt.Errorf("not a supported image (PNG, JPEG, GIF, WebP, TIFF or BMP): %w", err)
	}
	if config.Width <= 0 || config.Height <= 0 || config.Width*config.Height > maxPixels {
		return core.Image{}, fmt.Errorf("image of %d×%d pixels is too large", config.Width, config.Height)
	}
	fits := config.Width <= MaxEdge && config.Height <= MaxEdge && len(data) <= maxBytes
	if media := mediaTypes[format]; media != "" && fits {
		return core.Image{MediaType: media, Data: data, Width: config.Width, Height: config.Height}, nil
	}
	decoded, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return core.Image{}, fmt.Errorf("decode %s image: %w", format, err)
	}
	return encode(scale(decoded, MaxEdge))
}

// Encode prepares a picture already in memory, such as a rendered screenshot,
// as Prepare does a file: scaled down to MaxEdge on its long side and encoded
// as PNG, or as JPEG when a PNG would be too large. A tool returns the result
// in core.ToolResult.Images.
func Encode(img image.Image) (core.Image, error) {
	if bounds := img.Bounds(); bounds.Empty() {
		return core.Image{}, errors.New("image is empty")
	}
	return encode(scale(img, MaxEdge))
}

// mediaTypes are the formats every provider takes as they are.
var mediaTypes = map[string]string{"png": "image/png", "jpeg": "image/jpeg", "webp": "image/webp"}

// scale fits img within edge pixels on its long side, keeping its aspect.
func scale(img image.Image, edge int) image.Image {
	bounds := img.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	if w <= edge && h <= edge {
		return img
	}
	if w >= h {
		w, h = edge, max(1, h*edge/w)
	} else {
		w, h = max(1, w*edge/h), edge
	}
	scaled := image.NewNRGBA(image.Rect(0, 0, w, h))
	xdraw.CatmullRom.Scale(scaled, scaled.Bounds(), img, bounds, xdraw.Src, nil)
	return scaled
}

func encode(img image.Image) (core.Image, error) {
	bounds := img.Bounds()
	result := core.Image{Width: bounds.Dx(), Height: bounds.Dy()}
	var out bytes.Buffer
	if err := (&png.Encoder{CompressionLevel: png.BestCompression}).Encode(&out, img); err != nil {
		return core.Image{}, fmt.Errorf("encode PNG: %w", err)
	}
	if out.Len() <= maxBytes {
		result.MediaType, result.Data = "image/png", out.Bytes()
		return result, nil
	}
	// Photos and dense screenshots compress far better as JPEG. It has no alpha,
	// so transparent areas become white rather than black.
	flat := image.NewRGBA(bounds)
	draw.Draw(flat, bounds, image.NewUniform(color.White), image.Point{}, draw.Src)
	draw.Draw(flat, bounds, img, bounds.Min, draw.Over)
	for _, quality := range []int{90, 80, 65} {
		out.Reset()
		if err := jpeg.Encode(&out, flat, &jpeg.Options{Quality: quality}); err != nil {
			return core.Image{}, fmt.Errorf("encode JPEG: %w", err)
		}
		if out.Len() <= maxBytes {
			result.MediaType, result.Data = "image/jpeg", out.Bytes()
			return result, nil
		}
	}
	return core.Image{}, errors.New("image is too detailed to send even as a JPEG")
}

// Load reads and prepares the image file at path.
func Load(path string) (core.Image, error) {
	file, err := os.Open(path)
	if err != nil {
		return core.Image{}, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, MaxFileBytes+1))
	if err != nil {
		return core.Image{}, err
	}
	if len(data) > MaxFileBytes {
		return core.Image{}, fmt.Errorf("%s is larger than %d MB", filepath.Base(path), MaxFileBytes>>20)
	}
	img, err := Prepare(data)
	if err != nil {
		return core.Image{}, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return img, nil
}

// imageExtensions are the file types Paths recognizes.
var imageExtensions = map[string]bool{".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true, ".tif": true, ".tiff": true, ".bmp": true}

// IsImagePath reports whether path names an image file by its extension.
func IsImagePath(path string) bool {
	return imageExtensions[strings.ToLower(filepath.Ext(path))]
}

// Paths returns the image files a paste names when that is all it contains:
// what a terminal inserts when files are dragged onto it. Paths may be quoted,
// backslash-escaped, file:// URLs or start with ~/, and must exist. Any other
// text, or a path to something that is not an image file, returns nil, so an
// ordinary paste stays text.
func Paths(text string) []string {
	words, ok := shellWords(strings.TrimSpace(text))
	if !ok || len(words) == 0 {
		return nil
	}
	home, _ := os.UserHomeDir()
	paths := make([]string, 0, len(words))
	for _, word := range words {
		if strings.HasPrefix(word, "file://") {
			parsed, err := url.Parse(word)
			if err != nil || (parsed.Host != "" && parsed.Host != "localhost") {
				return nil
			}
			word = parsed.Path
		} else if strings.HasPrefix(word, "~/") && home != "" {
			word = filepath.Join(home, word[2:])
		}
		if !filepath.IsAbs(word) || !IsImagePath(word) {
			return nil
		}
		if info, err := os.Stat(word); err != nil || !info.Mode().IsRegular() {
			return nil
		}
		paths = append(paths, word)
	}
	return paths
}

// shellWords splits text the way a shell would read the paths a terminal
// pastes: on unquoted whitespace, honoring single and double quotes and
// backslash escapes. It fails on an unterminated quote.
func shellWords(text string) ([]string, bool) {
	var words []string
	var word strings.Builder
	inWord, quote, escaped := false, rune(0), false
	for _, r := range text {
		switch {
		case escaped:
			word.WriteRune(r)
			escaped = false
		case r == '\\' && quote != '\'':
			escaped, inWord = true, true
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				word.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote, inWord = r, true
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			if inWord {
				words = append(words, word.String())
				word.Reset()
				inWord = false
			}
		default:
			word.WriteRune(r)
			inWord = true
		}
	}
	if quote != 0 || escaped {
		return nil, false
	}
	if inWord {
		words = append(words, word.String())
	}
	return words, true
}

// Thumbnail decodes img and scales it to fit within width×height pixels, for
// drawing in the terminal. It never scales up.
func Thumbnail(img core.Image, width, height int) (image.Image, error) {
	decoded, _, err := image.Decode(bytes.NewReader(img.Data))
	if err != nil {
		return nil, err
	}
	w, h := decoded.Bounds().Dx(), decoded.Bounds().Dy()
	factor := min(float64(width)/float64(w), float64(height)/float64(h), 1)
	return scale(decoded, max(1, int(float64(max(w, h))*factor+0.5))), nil
}

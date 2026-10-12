package core

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ImageInfo describes an image without carrying it: what records keep of a tool
// result's image, so logs, journals and transcripts never hold its bytes.
type ImageInfo struct {
	MediaType string `json:"media_type"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	Bytes     int    `json:"bytes,omitempty"` // the encoded size; zero when unknown
}

// Info describes the image. An image decoded from a record has no data, so its
// weight is unknown.
func (i Image) Info() ImageInfo {
	return ImageInfo{MediaType: i.MediaType, Width: i.Width, Height: i.Height, Bytes: len(i.Data)}
}

// Format names the encoding the way captions do: PNG, JPG, GIF, WEBP.
func (i ImageInfo) Format() string {
	format := strings.ToUpper(strings.TrimPrefix(i.MediaType, "image/"))
	if format == "JPEG" {
		return "JPG"
	}
	return format
}

// String is a short caption: its format, size and weight, as in
// "PNG · 1440×900 · 312 KB". Whatever is unknown is left out.
func (i ImageInfo) String() string {
	var parts []string
	if format := i.Format(); format != "" {
		parts = append(parts, format)
	}
	if i.Width > 0 && i.Height > 0 {
		parts = append(parts, fmt.Sprintf("%d×%d", i.Width, i.Height))
	}
	if i.Bytes > 0 {
		parts = append(parts, byteSize(i.Bytes))
	}
	if len(parts) == 0 {
		return "image"
	}
	return strings.Join(parts, " · ")
}

func byteSize(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d KB", (n+512)>>10)
	}
	return fmt.Sprintf("%d B", n)
}

// ImageMarker is the line a transcript keeps in place of an image whose bytes it
// does not store, such as "[image: PNG · 1440×900 · 312 KB]".
func ImageMarker(info ImageInfo) string { return "[image: " + info.String() + "]" }

// ParseImageMarker returns the caption inside an ImageMarker line.
func ParseImageMarker(line string) (string, bool) {
	caption, ok := strings.CutPrefix(strings.TrimSpace(line), "[image: ")
	caption, closed := strings.CutSuffix(caption, "]")
	return caption, ok && closed && caption != ""
}

// MaxToolImages is the most images one tool result carries to the model.
// fantasy's ToolResponse, which every API provider goes through, holds one, so
// Codex, which could take several, is held to the same rule.
const MaxToolImages = 1

// Sendable returns r as every provider can send it: at most MaxToolImages
// images, each with its data, and none on an error, whose text alone is sent.
// Each image it leaves out becomes a line in Content, so neither the model nor
// the person watching loses track of it. Providers apply it before a result is
// emitted, journaled or sent, so every consumer sees the same result.
func (r ToolResult) Sendable() ToolResult {
	if len(r.Images) == 0 {
		return r
	}
	var kept []Image
	var notes []string
	for _, img := range r.Images {
		switch {
		case len(img.Data) == 0: // a record's image: its bytes were never kept
			notes = append(notes, ImageMarker(img.Info()))
		case r.IsError:
			notes = append(notes, "[image not sent: "+img.Info().String()+"; an error result carries text only]")
		case len(kept) == MaxToolImages:
			notes = append(notes, "[image not sent: "+img.Info().String()+"; a tool result carries one image]")
		default:
			kept = append(kept, img)
		}
	}
	r.Images = kept
	r.Content = joinLines(r.Content, notes...)
	return r
}

// Record returns r as a record keeps it: each image described, without its
// bytes. Sendable turns such an image back into a marker line.
func (r ToolResult) Record() ToolResult {
	r.Images = WithoutImageData(r.Images)
	return r
}

// WithoutImageData copies images keeping only what describes them.
func WithoutImageData(images []Image) []Image {
	if len(images) == 0 {
		return nil
	}
	described := make([]Image, len(images))
	for i, img := range images {
		described[i] = Image{MediaType: img.MediaType, Width: img.Width, Height: img.Height}
	}
	return described
}

// Transcript is the result as a session transcript keeps it: its text, then an
// ImageMarker line for each image instead of the image's bytes.
func (r ToolResult) Transcript() string {
	markers := make([]string, len(r.Images))
	for i, img := range r.Images {
		markers[i] = ImageMarker(img.Info())
	}
	return joinLines(r.Content, markers...)
}

// MarshalJSON writes images as their ImageInfo, never their bytes.
func (r ToolResult) MarshalJSON() ([]byte, error) {
	type record ToolResult // without this method
	var infos []ImageInfo
	for _, img := range r.Images {
		infos = append(infos, img.Info())
	}
	return json.Marshal(struct {
		record
		Images []ImageInfo `json:"images,omitempty"`
	}{record(r), infos})
}

// joinLines appends lines to text, each on its own line.
func joinLines(text string, lines ...string) string {
	for _, line := range lines {
		if text != "" {
			text += "\n"
		}
		text += line
	}
	return text
}

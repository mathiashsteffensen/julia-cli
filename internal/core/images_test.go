package core

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func pixels(n int) []byte { return bytes.Repeat([]byte{0x89}, n) }

func TestImageInfoCaption(t *testing.T) {
	for _, tc := range []struct {
		image Image
		want  string
	}{
		{Image{MediaType: "image/png", Width: 1440, Height: 900, Data: pixels(320_000)}, "PNG · 1440×900 · 313 KB"},
		{Image{MediaType: "image/jpeg", Width: 2000, Height: 1500, Data: pixels(2 << 20)}, "JPG · 2000×1500 · 2.0 MB"},
		{Image{MediaType: "image/gif", Width: 16, Height: 16, Data: pixels(300)}, "GIF · 16×16 · 300 B"},
		{Image{MediaType: "image/webp", Width: 640, Height: 480}, "WEBP · 640×480"}, // a record's image: weight unknown
		{Image{MediaType: "image/png", Data: pixels(2048)}, "PNG · 2 KB"},           // size unknown
	} {
		if got := tc.image.Info().String(); got != tc.want {
			t.Errorf("caption %q, want %q", got, tc.want)
		}
	}
}

func TestImageMarkersRoundTrip(t *testing.T) {
	info := Image{MediaType: "image/png", Width: 1440, Height: 900, Data: pixels(2048)}.Info()
	caption, ok := ParseImageMarker("  " + ImageMarker(info) + " ")
	if !ok || caption != "PNG · 1440×900 · 2 KB" {
		t.Fatalf("parsed %q, %v", caption, ok)
	}
	for _, line := range []string{"[image: ]", "image: PNG", "[image: PNG", "[Image #1]", "[image elided: PNG]"} {
		if _, ok := ParseImageMarker(line); ok {
			t.Errorf("%q parsed as a marker", line)
		}
	}
}

func TestSendableKeepsOneImageAndNamesTheRest(t *testing.T) {
	shot := Image{MediaType: "image/png", Width: 1440, Height: 900, Data: pixels(4096)}
	record := Image{MediaType: "image/png", Width: 800, Height: 600}
	for _, tc := range []struct {
		name   string
		result ToolResult
		images int
		lines  []string
	}{
		{"text only", ToolResult{Content: "ok"}, 0, []string{"ok"}},
		{"one image", ToolResult{Content: "shot.png", Images: []Image{shot}}, 1, []string{"shot.png"}},
		{"too many", ToolResult{Content: "two", Images: []Image{shot, shot}}, 1, []string{"two", "[image not sent: PNG · 1440×900 · 4 KB; a tool result carries one image]"}},
		{"error", ToolResult{Content: "failed", IsError: true, Images: []Image{shot}}, 0, []string{"failed", "[image not sent: PNG · 1440×900 · 4 KB; an error result carries text only]"}},
		{"record", ToolResult{Images: []Image{record, shot}}, 1, []string{"[image: PNG · 800×600]"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := len(tc.result.Images)
			got := tc.result.Sendable()
			if len(got.Images) != tc.images || got.Content != strings.Join(tc.lines, "\n") {
				t.Fatalf("%d images, content %q", len(got.Images), got.Content)
			}
			if len(tc.result.Images) != original {
				t.Fatal("Sendable altered its receiver")
			}
		})
	}
}

func TestToolResultRecordsDescribeImagesWithoutTheirBytes(t *testing.T) {
	shot := Image{MediaType: "image/png", Width: 1440, Height: 900, Data: pixels(64 << 10)}
	result := ToolResult{Content: "shot.png", Images: []Image{shot}}
	if got := result.Transcript(); got != "shot.png\n[image: PNG · 1440×900 · 64 KB]" {
		t.Fatalf("transcript %q", got)
	}
	data, err := json.Marshal(Event{Kind: "tool_end", Result: &result})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"kind":"tool_end","result":{"content":"shot.png","images":[{"media_type":"image/png","width":1440,"height":900,"bytes":65536}]}}`
	if string(data) != want {
		t.Fatalf("event JSON\n%s\nwant\n%s", data, want)
	}
	var decoded ToolResult
	if err := json.Unmarshal(data[len(`{"kind":"tool_end","result":`):len(data)-1], &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Images) != 1 || decoded.Images[0].Width != 1440 || decoded.Images[0].Data != nil {
		t.Fatalf("decoded %+v", decoded)
	}
	if data, _ := json.Marshal(ToolResult{Content: "plain"}); string(data) != `{"content":"plain"}` {
		t.Fatalf("plain result JSON %s", data)
	}
}

func TestRecordKeepsTheDescriptionWithoutTheBytes(t *testing.T) {
	shot := Image{MediaType: "image/png", Width: 1440, Height: 900, Data: pixels(4096)}
	result := ToolResult{Content: "shot.png", Images: []Image{shot}}
	record := result.Record()
	if len(record.Images) != 1 || record.Images[0].Data != nil || record.Images[0].Width != 1440 || len(result.Images[0].Data) != 4096 {
		t.Fatalf("record %+v", record)
	}
	if got := record.Sendable(); len(got.Images) != 0 || got.Content != "shot.png\n[image: PNG · 1440×900]" {
		t.Fatalf("replayed record %+v", got)
	}
	if WithoutImageData(nil) != nil || (ImageInfo{}).String() != "image" {
		t.Fatal("empty descriptions")
	}
}

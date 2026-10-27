package ui

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/ansi/kitty"
	"julia/internal/core"
)

func viewImageCall(path string) *core.ToolCall {
	return &core.ToolCall{ID: "call-1", Name: "ViewImage", Arguments: json.RawMessage(fmt.Sprintf(`{"path":%q}`, path))}
}

// viewImage runs a ViewImage call through the model's event handling and
// returns what tool_end asked to upload.
func viewImage(m *model, img core.Image) string {
	call := viewImageCall("$JULIA_SCRATCH/shot.png")
	m.applyEvent(core.Event{Kind: "tool_start", Call: call})
	result := core.ToolResult{Content: "$JULIA_SCRATCH/shot.png · 1440×900 PNG", Images: []core.Image{img}}
	return rawOutput(m.applyEvent(core.Event{Kind: "tool_end", Call: call, Result: &result}))
}

func TestToolImagesDrawAsThumbnailsInTheToolCard(t *testing.T) {
	m := graphicsModel(t, Config{})
	img := pngImage(t, 1440, 900)
	upload := viewImage(m, img)
	item := m.entries[len(m.entries)-1]
	if len(item.imageIDs) != 1 {
		t.Fatalf("tool image not registered: %+v", item.imageIDs)
	}
	id := item.imageIDs[0]
	cols, rows := m.thumbnailSize(img, m.thumbnailColumns())
	for _, want := range []string{fmt.Sprintf("i=%d", id), "U=1", fmt.Sprintf("c=%d", cols), fmt.Sprintf("r=%d", rows)} {
		if !strings.Contains(upload, want) {
			t.Fatalf("upload lacks %s: %.120q", want, upload)
		}
	}
	for _, expanded := range []bool{false, true} {
		m.expandTools = expanded
		rendered := m.renderEntryContent(item)
		if got := placeholderCells(t, rendered, 128, 30)[id]; got != cols*rows {
			t.Fatalf("expanded=%v: drew %d cells of a %dx%d thumbnail", expanded, got, cols, rows)
		}
		lines := strings.Split(ansi.Strip(rendered), "\n")
		// Collapsed, the one-line result joins the call on its row; expanded, it
		// is set under it. A blank row then parts the text from the thumbnail.
		want := []string{"     ViewImage  $JULIA_SCRATCH/shot.png  ·  $JULIA_SCRATCH/shot.png · 1440×900 PNG", "  "}
		if expanded {
			want = []string{"     ViewImage  $JULIA_SCRATCH/shot.png", "    $JULIA_SCRATCH/shot.png · 1440×900 PNG", "  "}
		}
		if strings.Join(lines[:len(want)], "\n") != strings.Join(want, "\n") {
			t.Fatalf("expanded=%v: card opens\n%s", expanded, strings.Join(lines[:3], "\n"))
		}
		if last := lines[len(lines)-1]; last != "    ▣ "+img.Info().String() {
			t.Fatalf("expanded=%v: caption %q", expanded, last)
		}
		for _, line := range strings.Split(rendered, "\n") {
			if width := ansi.StringWidth(line); width > m.mainWidth {
				t.Fatalf("row of %d cells in a %d-cell column", width, m.mainWidth)
			}
		}
	}
	if got := placeholderCells(t, m.View().Content, 128, 60)[id]; got != cols*rows {
		t.Fatalf("view shows %d of %d thumbnail cells", got, cols*rows)
	}
	m.input.SetValue("/clear")
	if out := rawOutput(m.submit()); !strings.Contains(out, fmt.Sprintf("a=d,d=I,i=%d", id)) {
		t.Fatal("/clear left the tool's image in the terminal")
	}
}

func TestToolImagesAreCaptionedWithoutGraphics(t *testing.T) {
	m := testModel(t, Config{})
	m.resize(100, 40)
	img := pngImage(t, 1440, 900)
	if upload := viewImage(m, img); upload != "" {
		t.Fatalf("uploaded to a terminal without graphics: %.80q", upload)
	}
	rendered := m.renderEntryContent(m.entries[len(m.entries)-1])
	if strings.ContainsRune(rendered, kitty.Placeholder) {
		t.Fatal("drew placeholders without graphics")
	}
	lines := strings.Split(ansi.Strip(rendered), "\n")
	if len(lines) != 2 || lines[1] != "    ▣ "+img.Info().String() {
		t.Fatalf("card\n%s", strings.Join(lines, "\n"))
	}
}

func TestResumedToolImagesShowTheirMarkerAsACaption(t *testing.T) {
	session := &core.Session{ID: "test", Directory: "/tmp/julia", Provider: "fake", Messages: []core.Message{
		{Role: "user", Content: "What is on screen?"},
		{Role: "assistant", ToolCalls: []core.ToolCall{*viewImageCall("/var/folders/x/T/julia-scratch/2026-10-11-abc/shots/term.png")}},
		{Role: "tool", ToolCallID: "call-1", Content: "$JULIA_SCRATCH/shots/term.png · 1440×900 PNG\n" + core.ImageMarker(core.ImageInfo{MediaType: "image/png", Width: 1440, Height: 900, Bytes: 312 << 10})},
	}}
	m := testModel(t, Config{Session: session})
	m.resize(100, 40)
	for _, expanded := range []bool{false, true} {
		m.expandTools = expanded
		lines := strings.Split(ansi.Strip(m.renderEntryContent(m.entries[1])), "\n")
		want := []string{"     ViewImage  $JULIA_SCRATCH/shots/term.png  ·  $JULIA_SCRATCH/shots/term.png · 1440×900 PNG", "    ▣ PNG · 1440×900 · 312 KB"}
		if expanded {
			want = []string{"     ViewImage  $JULIA_SCRATCH/shots/term.png", "    $JULIA_SCRATCH/shots/term.png · 1440×900 PNG", "    ▣ PNG · 1440×900 · 312 KB"}
		}
		if strings.Join(lines, "\n") != strings.Join(want, "\n") {
			t.Fatalf("expanded=%v: card\n%s", expanded, strings.Join(lines, "\n"))
		}
	}
}

func TestSplitImageMarkersTakesOnlyTrailingMarkers(t *testing.T) {
	for _, tc := range []struct {
		content, text string
		markers       []string
	}{
		{"plain", "plain", nil},
		{"shot.png\n[image: PNG · 8×8]", "shot.png", []string{"PNG · 8×8"}},
		{"[image: PNG]", "", []string{"PNG"}},
		{"[image: PNG]\nthen text", "[image: PNG]\nthen text", nil},
		{"out\n[image not sent: PNG; an error result carries text only]", "out\n[image not sent: PNG; an error result carries text only]", nil},
	} {
		text, markers := splitImageMarkers(tc.content)
		if text != tc.text || fmt.Sprint(markers) != fmt.Sprint(tc.markers) {
			t.Errorf("%q: %q %q", tc.content, text, markers)
		}
	}
}

func TestToolSummaryShowsScratchPathsTheWayTheAgentKnowsThem(t *testing.T) {
	for path, want := range map[string]string{
		"docs/welcome.png":        "docs/welcome.png",
		"$JULIA_SCRATCH/shot.png": "$JULIA_SCRATCH/shot.png",
		"/private/var/folders/x/T/julia-scratch/2026-10-11-abc/shot.png": "$JULIA_SCRATCH/shot.png",
		"/tmp/julia-scratch/2026-10-11-abc/a/b.png":                      "$JULIA_SCRATCH/a/b.png",
		"/tmp/julia-scratch/":        "/tmp/julia-scratch/",
		"/Users/me/Desktop/shot.png": "/Users/me/Desktop/shot.png",
	} {
		if got := toolSummary(*viewImageCall(path)); got != want {
			t.Errorf("%s: %q, want %q", path, got, want)
		}
	}
}

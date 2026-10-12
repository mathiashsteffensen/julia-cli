package ui

import (
	"bytes"
	"fmt"
	"image"
	"image/png"
	"reflect"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/ansi/kitty"
	"julia/internal/core"
	"julia/internal/images"
)

// graphicsModel is a model in a terminal that has accepted Kitty graphics.
func graphicsModel(t *testing.T, cfg Config) *model {
	t.Helper()
	m := testModel(t, cfg)
	m.resize(128, 60)
	m.Update(tea.ColorProfileMsg{Profile: colorprofile.TrueColor})
	m.Update(tea.EnvMsg{"TERM_PROGRAM=ghostty"})
	m.Update(tea.ModeReportMsg{Mode: ansi.ModeAltScreenSaveCursor, Value: ansi.ModeSet})
	m.Update(uv.KittyGraphicsEvent{Options: kitty.Options{ID: wordmarkID}, Payload: []byte("OK")})
	if !m.thumbnailsShown() {
		t.Fatal("graphics not confirmed")
	}
	return m
}

func pngImage(t *testing.T, w, h int) core.Image {
	t.Helper()
	var data bytes.Buffer
	if err := png.Encode(&data, image.NewGray(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	img, err := images.Prepare(data.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	return img
}

// placeholderCells draws text as Bubble Tea would and counts, for each image
// ID, the distinct cells of the image its placeholders address. A grid that
// repeats a row or column therefore counts short. Damaged cells fail the test.
func placeholderCells(t *testing.T, text string, width, height int) map[int]int {
	t.Helper()
	diacritics := make(map[rune]int)
	for i := 0; i < 64; i++ {
		diacritics[kitty.Diacritic(i)] = i
	}
	buffer := uv.NewScreenBuffer(width, height)
	uv.NewStyledString(text).Draw(buffer, buffer.Bounds())
	seen := make(map[[3]int]bool)
	counts := make(map[int]int)
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			cell := buffer.CellAt(x, y)
			if cell == nil || !strings.ContainsRune(cell.Content, kitty.Placeholder) {
				continue
			}
			if len([]rune(cell.Content)) != 3 || cell.Width != 1 {
				t.Fatalf("damaged placeholder: %#v", cell)
			}
			r, g, b, _ := cell.Style.Fg.RGBA()
			id, content := int(r>>8)<<16|int(g>>8)<<8|int(b>>8), []rune(cell.Content)
			key := [3]int{id, diacritics[content[1]], diacritics[content[2]]}
			if !seen[key] {
				seen[key] = true
				counts[id]++
			}
		}
	}
	return counts
}

func TestSentImagesDrawAsThumbnails(t *testing.T) {
	backend := promptBackend{prompts: make(chan core.Prompt, 1)}
	m := graphicsModel(t, Config{Backend: backend})
	img := pngImage(t, 1440, 900)
	m.receiveImages(imagesMsg{images: []core.Image{img}})
	m.input.InsertString("what is this?")
	upload := rawOutput(m.submit())
	<-backend.prompts
	id := m.entries[0].imageIDs[0]
	cols, rows := m.thumbnailSize(img, m.thumbnailColumns())
	for _, want := range []string{"i=" + fmt.Sprint(id), "U=1", fmt.Sprintf("c=%d", cols), fmt.Sprintf("r=%d", rows), "q=2", "f=100"} {
		if !strings.Contains(upload, want) {
			t.Fatalf("upload lacks %s: %.120q", want, upload)
		}
	}
	rendered := m.renderEntryContent(m.entries[0])
	if got := placeholderCells(t, rendered, 128, 30)[id]; got != cols*rows {
		t.Fatalf("drew %d cells of a %dx%d thumbnail", got, cols, rows)
	}
	if !strings.Contains(ansi.Strip(rendered), "▣ Image #1  1440×900") {
		t.Fatalf("no caption:\n%s", ansi.Strip(rendered))
	}
	// The whole view keeps the image ID intact through the sidebar and padding.
	if got := placeholderCells(t, m.View().Content, 128, 60)[id]; got != cols*rows {
		t.Fatalf("view shows %d of %d thumbnail cells", got, cols*rows)
	}
}

func TestHistoryThumbnailsUploadAfterTheFirstFrame(t *testing.T) {
	session := &core.Session{ID: "test", Directory: "/tmp/julia", Provider: "fake", Messages: []core.Message{{Role: "user", Content: "[Image #1]", Images: []core.Image{pngImage(t, 40, 30)}}}}
	m := testModel(t, Config{Session: session})
	m.Update(tea.ColorProfileMsg{Profile: colorprofile.TrueColor})
	m.Update(tea.EnvMsg{"TERM_PROGRAM=ghostty"})
	_, cmd := m.Update(tea.ModeReportMsg{Mode: ansi.ModeAltScreenSaveCursor, Value: ansi.ModeSet})
	if out := rawOutput(cmd); !strings.Contains(out, fmt.Sprintf("i=%d", m.entries[0].imageIDs[0])) {
		t.Fatal("saved message's image not uploaded")
	}
}

func TestThumbnailsKeepTheImageAspect(t *testing.T) {
	m := testModel(t, Config{})
	m.cellWidth, m.cellHeight = 9, 18
	for _, tc := range []struct {
		w, h, maxCols, cols, rows int
	}{
		{1440, 900, 100, 26, 8}, // landscape screenshot
		{900, 1950, 100, 7, 8},  // phone screenshot
		{4000, 500, 100, 48, 3}, // panorama, capped at thumbnailMaxCols
		{1440, 900, 13, 13, 4},  // a narrow window
		{1, 1000, 100, 2, 8},    // never thinner than two columns
	} {
		if cols, rows := m.thumbnailSize(core.Image{Width: tc.w, Height: tc.h}, tc.maxCols); cols != tc.cols || rows != tc.rows {
			t.Errorf("%dx%d in %d columns: %dx%d cells, want %dx%d", tc.w, tc.h, tc.maxCols, cols, rows, tc.cols, tc.rows)
		}
	}
}

func TestThumbnailsSitSideBySideAndWrap(t *testing.T) {
	m := graphicsModel(t, Config{})
	item := entry{role: "user", text: "compare", images: []core.Image{pngImage(t, 1440, 900), pngImage(t, 900, 1950), pngImage(t, 1440, 900)}}
	m.registerImages(&item)
	for _, width := range []int{120, 40, 20} {
		lines := m.thumbnailRowsFor(item, width)
		for _, line := range lines {
			if got := ansi.StringWidth(line); got > width {
				t.Fatalf("width %d: row of %d cells", width, got)
			}
		}
		counts := placeholderCells(t, strings.Join(lines, "\n"), width, len(lines))
		for i, img := range item.images {
			cols, rows := m.thumbnailSize(img, width)
			if counts[item.imageIDs[i]] != cols*rows {
				t.Fatalf("width %d: image %d drew %d of %d cells", width, i+1, counts[item.imageIDs[i]], cols*rows)
			}
		}
		captions := 0
		for _, line := range lines {
			captions += strings.Count(ansi.Strip(line), "▣ Image #")
		}
		if captions != 3 {
			t.Fatalf("width %d: %d captions", width, captions)
		}
		if width == 120 && len(lines) != thumbnailRows+1 {
			t.Fatalf("three thumbnails should share one band, got %d rows", len(lines))
		}
	}
}

func TestThumbnailsAreCaptionsUntilTheTerminalAcceptsGraphics(t *testing.T) {
	m := testModel(t, Config{})
	m.Update(tea.ColorProfileMsg{Profile: colorprofile.TrueColor})
	m.Update(tea.EnvMsg{"TERM_PROGRAM=ghostty"})
	item := entry{role: "user", text: "[Image #1]", images: []core.Image{fakeImage(640, 480)}}
	m.registerImages(&item)
	m.entries = append(m.entries, item)
	if rendered := m.renderEntry(&m.entries[0]).text; strings.ContainsRune(rendered, kitty.Placeholder) {
		t.Fatal("drew placeholders before the terminal accepted an image")
	}
	m.Update(tea.ModeReportMsg{Mode: ansi.ModeAltScreenSaveCursor, Value: ansi.ModeSet})
	m.Update(uv.KittyGraphicsEvent{Options: kitty.Options{ID: wordmarkID}, Payload: []byte("OK")})
	if rendered := m.renderEntry(&m.entries[0]).text; !strings.ContainsRune(rendered, kitty.Placeholder) {
		t.Fatal("cached caption layout survived the terminal accepting graphics")
	}
}

func TestCellSizeReportResizesThumbnails(t *testing.T) {
	m := graphicsModel(t, Config{})
	item := entry{role: "user", text: "[Image #1]", images: []core.Image{pngImage(t, 1440, 900)}}
	m.registerImages(&item)
	m.entries = append(m.entries, item)
	before := m.renderEntry(&m.entries[0]).text
	_, cmd := m.Update(uv.CellSizeEvent{Width: 9, Height: 30})
	if !strings.Contains(rawOutput(cmd), "U=1") {
		t.Fatal("thumbnails not uploaded again for the new cell size")
	}
	if m.renderEntry(&m.entries[0]).text == before {
		t.Fatal("layout kept the assumed cell size")
	}
	if _, cmd := m.Update(uv.CellSizeEvent{Width: 9, Height: 30}); cmd != nil {
		t.Fatal("an unchanged cell size uploaded again")
	}
}

func TestClearAndQuitDeleteThumbnails(t *testing.T) {
	m := graphicsModel(t, Config{})
	item := entry{role: "user", text: "[Image #1]", images: []core.Image{fakeImage(10, 10)}}
	m.registerImages(&item)
	m.entries = append(m.entries, item)
	deleted := fmt.Sprintf("a=d,d=I,i=%d", item.imageIDs[0])
	m.input.SetValue("/clear")
	if out := rawOutput(m.submit()); !strings.Contains(out, deleted) {
		t.Fatalf("/clear left the image in the terminal: %q", out)
	}
	if len(m.thumbnails) != 0 {
		t.Fatal("/clear kept the thumbnails")
	}
	next := entry{role: "user", text: "[Image #1]", images: []core.Image{fakeImage(10, 10)}}
	m.registerImages(&next)
	// quit is a sequence: the deletes, then the round trip that lets them land.
	steps := reflect.ValueOf(m.quit()())
	if steps.Kind() != reflect.Slice || steps.Len() == 0 || !strings.Contains(rawOutput(steps.Index(0).Interface().(tea.Cmd)), fmt.Sprintf("i=%d", next.imageIDs[0])) {
		t.Fatal("quitting left the image in the terminal")
	}
}

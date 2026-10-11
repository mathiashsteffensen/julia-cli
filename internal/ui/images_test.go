package ui

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"jarvis/internal/core"
	"jarvis/internal/images"
)

var ctrlV = tea.KeyPressMsg{Code: 'v', Mod: tea.ModCtrl}

func fakeImage(w, h int) core.Image {
	return core.Image{MediaType: "image/png", Data: make([]byte, 2048), Width: w, Height: h}
}

// paste presses Ctrl+V with img on the clipboard, or nothing when err is set,
// and delivers the result.
func paste(t *testing.T, m *model, img core.Image, err error) tea.Cmd {
	t.Helper()
	m.clipboardImage = func(context.Context) (core.Image, error) { return img, err }
	_, cmd := m.Update(ctrlV)
	if cmd == nil {
		t.Fatal("Ctrl+V did not read the clipboard")
	}
	_, next := m.Update(cmd())
	return next
}

type promptBackend struct{ prompts chan core.Prompt }

func (promptBackend) Name() string { return "fake" }
func (b promptBackend) Run(_ context.Context, _ *core.Session, prompt core.Prompt, _ core.ToolExecutor, _ core.Emit) error {
	b.prompts <- prompt
	return nil
}

func TestCtrlVAttachesClipboardImageAndSendsIt(t *testing.T) {
	backend := promptBackend{prompts: make(chan core.Prompt, 1)}
	m := testModel(t, Config{Backend: backend})
	m.resize(120, 40)
	m.input.InsertString("What is wrong with ")
	paste(t, m, fakeImage(1440, 900), nil)
	if got := m.input.Value(); got != "What is wrong with [Image #1] " {
		t.Fatalf("draft %q", got)
	}
	view := ansi.Strip(m.View().Content)
	if !strings.Contains(view, "▣ Image #1 1440×900") {
		t.Fatalf("composer does not show the attachment:\n%s", view)
	}
	m.submit()
	prompt := <-backend.prompts
	if prompt.Text != "What is wrong with [Image #1]" || len(prompt.Images) != 1 || prompt.Images[0].Width != 1440 {
		t.Fatalf("sent %q with %d images", prompt.Text, len(prompt.Images))
	}
	if len(m.attachments) != 0 || m.imageSeq != 0 {
		t.Fatal("sending kept the draft's attachments")
	}
	rendered := ansi.Strip(m.renderEntryContent(m.entries[0]))
	if !strings.Contains(rendered, "▣ Image #1  PNG · 1440×900 · 2 KB") {
		t.Fatalf("sent message does not show its image:\n%s", rendered)
	}
}

func TestCtrlVWithoutImageFallsBackToTextPaste(t *testing.T) {
	m := testModel(t, Config{})
	cmd := paste(t, m, core.Image{}, images.ErrNoImage)
	if cmd == nil || reflect.ValueOf(cmd).Pointer() != reflect.ValueOf(textarea.Paste).Pointer() {
		t.Fatal("no text paste after finding no image")
	}
	if m.input.Value() != "" || len(m.attachments) != 0 {
		t.Fatal("attached something from an imageless clipboard")
	}
}

func TestCtrlVReportsClipboardErrors(t *testing.T) {
	m := testModel(t, Config{})
	if cmd := paste(t, m, core.Image{}, os.ErrPermission); cmd != nil {
		t.Fatal("a failed read fell back to a text paste")
	}
	if !strings.Contains(m.notice, "Could not attach image") {
		t.Fatalf("notice %q", m.notice)
	}
}

func TestEmptyBracketedPasteReadsClipboardImage(t *testing.T) {
	m := testModel(t, Config{})
	m.clipboardImage = func(context.Context) (core.Image, error) { return fakeImage(10, 10), nil }
	_, cmd := m.Update(tea.PasteMsg{Content: ""})
	if cmd == nil {
		t.Fatal("empty paste ignored")
	}
	if _, next := m.Update(cmd()); next != nil {
		t.Fatal("an image paste also pasted text")
	}
	if m.input.Value() != "[Image #1] " {
		t.Fatalf("draft %q", m.input.Value())
	}
}

func TestDroppedImageFilesAttachAndOtherPastesStayText(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "Screen Shot.png")
	var data bytes.Buffer
	if err := png.Encode(&data, image.NewGray(image.Rect(0, 0, 30, 20))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	m := testModel(t, Config{})
	_, cmd := m.Update(tea.PasteMsg{Content: strings.ReplaceAll(path, " ", `\ `) + " "})
	if cmd == nil {
		t.Fatal("dropped file not loaded")
	}
	m.Update(cmd())
	if m.input.Value() != "[Image #1] " || len(m.attachments) != 1 || m.attachments[0].image.Width != 30 {
		t.Fatalf("draft %q, %d attachments", m.input.Value(), len(m.attachments))
	}
	m.Update(tea.PasteMsg{Content: "see " + path})
	if !strings.HasSuffix(m.input.Value(), "see "+path) {
		t.Fatalf("text paste lost: %q", m.input.Value())
	}
}

func TestDeletedTokensDropImagesAndTheRestAreRenumbered(t *testing.T) {
	m := testModel(t, Config{})
	for _, w := range []int{100, 200, 300} {
		paste(t, m, fakeImage(w, 50), nil)
	}
	// The user deletes the first token and names the third before the second.
	prompt := m.takeImages("compare [Image #3] with [Image #2], not [Image #9]; again [Image #3]")
	if prompt.Text != "compare [Image #1] with [Image #2], not [Image #9]; again [Image #1]" {
		t.Fatalf("text %q", prompt.Text)
	}
	if len(prompt.Images) != 2 || prompt.Images[0].Width != 300 || prompt.Images[1].Width != 200 {
		t.Fatalf("images %+v", prompt.Images)
	}
}

func TestPastesInsideDialogsDoNotAttach(t *testing.T) {
	m := testModel(t, settingsConfig())
	m.clipboardImage = func(context.Context) (core.Image, error) {
		t.Error("read the clipboard while the picker was open")
		return core.Image{}, nil
	}
	m.Update(tea.KeyPressMsg{Code: 'p', Mod: tea.ModCtrl})
	if m.picker == nil {
		t.Fatal("picker did not open")
	}
	if _, cmd := m.Update(ctrlV); cmd != nil {
		if msg, ok := cmd().(imagesMsg); ok {
			t.Fatalf("picker paste produced %+v", msg)
		}
	}
	if _, cmd := m.Update(tea.PasteMsg{Content: ""}); cmd != nil {
		if msg, ok := cmd().(imagesMsg); ok {
			t.Fatalf("picker paste produced %+v", msg)
		}
	}
}

func TestHistoryShowsImagesOfSavedMessages(t *testing.T) {
	session := &core.Session{ID: "test", Directory: "/tmp/jarvis", Provider: "fake", Messages: []core.Message{{Role: "user", Content: "Look at [Image #1]", Images: []core.Image{fakeImage(640, 480)}}}}
	m := testModel(t, Config{Session: session})
	if rendered := ansi.Strip(m.renderEntryContent(m.entries[0])); !strings.Contains(rendered, "▣ Image #1  PNG · 640×480") {
		t.Fatalf("history entry:\n%s", rendered)
	}
}

func TestAttachmentChipsFitTheirWidth(t *testing.T) {
	m := testModel(t, Config{})
	for range 4 {
		paste(t, m, fakeImage(1920, 1080), nil)
	}
	for _, width := range []int{0, 12, 30, 200} {
		chips := m.attachmentChips(width)
		if got := ansi.StringWidth(chips); got > max(width, len("  ▣ 4 images")) {
			t.Errorf("width %d: %q is %d cells", width, ansi.Strip(chips), got)
		}
		if !strings.Contains(ansi.Strip(chips), "▣") {
			t.Errorf("width %d: no chip in %q", width, ansi.Strip(chips))
		}
	}
	if got := ansi.Strip(m.attachmentChips(200)); strings.Count(got, "▣ Image #") != 4 {
		t.Fatalf("wide composer shows %q", got)
	}
}

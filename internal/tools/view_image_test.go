package tools

import (
	"bytes"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"jarvis/agents"
	"jarvis/internal/core"
)

func writePNG(t *testing.T, path string, w, h int) []byte {
	t.Helper()
	var data bytes.Buffer
	if err := png.Encode(&data, image.NewGray(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

// imageFixture is an executor in mode with a configured scratch directory,
// spelled through a symlink the way macOS spells /var as /private/var.
func imageFixture(t *testing.T, mode string) (e *Executor, workspace, scratch, alias string) {
	t.Helper()
	e, workspace = fixture(t, mode, nil)
	real := t.TempDir()
	alias = filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}
	e.config.ScratchDir = filepath.Join(alias, "session")
	scratch, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	return e, workspace, filepath.Join(scratch, "session"), alias
}

func TestViewImageReturnsTheImageInEveryMode(t *testing.T) {
	for _, mode := range []string{"auto", "ask", "read-only"} {
		t.Run(mode, func(t *testing.T) {
			e, dir, _, _ := imageFixture(t, mode)
			data := writePNG(t, filepath.Join(dir, "docs", "shot.png"), 1440, 900)
			r := invoke(e, "ViewImage", map[string]any{"path": "docs/shot.png"})
			if r.IsError || r.Content != "docs/shot.png · 1440×900 PNG" {
				t.Fatalf("result %+v", r.Record())
			}
			if len(r.Images) != 1 || !bytes.Equal(r.Images[0].Data, data) || r.Images[0].Width != 1440 || r.Images[0].MediaType != "image/png" {
				t.Fatalf("image %+v", r.Record().Images)
			}
		})
	}
}

func TestViewImageScalesLargeImagesAndSaysSo(t *testing.T) {
	e, dir := fixture(t, "auto", nil)
	writePNG(t, filepath.Join(dir, "wide.png"), 4000, 1000)
	r := invoke(e, "ViewImage", map[string]any{"path": "wide.png"})
	if r.IsError || r.Content != "wide.png · 4000×1000 PNG, sent as 2000×500 PNG" || r.Images[0].Width != 2000 {
		t.Fatalf("result %+v", r.Record())
	}
}

func TestViewImageReadsTheScratchDirectory(t *testing.T) {
	e, _, scratch, alias := imageFixture(t, "read-only")
	writePNG(t, filepath.Join(scratch, "shots", "term.png"), 64, 32)
	for _, path := range []string{
		"$JARVIS_SCRATCH/shots/term.png",
		"${JARVIS_SCRATCH}/shots/term.png",
		filepath.Join(scratch, "shots", "term.png"),          // as $JARVIS_SCRATCH expands
		filepath.Join(alias, "session", "shots", "term.png"), // as configured
	} {
		r := invoke(e, "ViewImage", map[string]any{"path": path})
		if r.IsError || r.Content != "$JARVIS_SCRATCH/shots/term.png · 64×32 PNG" || len(r.Images) != 1 {
			t.Fatalf("%s: %+v", path, r.Record())
		}
	}
}

func TestViewImageConfinement(t *testing.T) {
	e, dir, scratch, _ := imageFixture(t, "auto")
	outside := t.TempDir()
	writePNG(t, filepath.Join(outside, "secret.png"), 8, 8)
	writePNG(t, filepath.Join(dir, ".git", "shot.png"), 8, 8)
	writePNG(t, filepath.Join(scratch, ".ssh", "shot.png"), 8, 8)
	for _, link := range []struct{ target, name string }{
		{filepath.Join(outside, "secret.png"), filepath.Join(dir, "link.png")},
		{outside, filepath.Join(dir, "linked")},
		{filepath.Join(outside, "secret.png"), filepath.Join(scratch, "link.png")},
		{outside, filepath.Join(scratch, "linked")},
	} {
		if err := os.Symlink(link.target, link.name); err != nil {
			t.Fatal(err)
		}
	}
	for path, want := range map[string]string{
		filepath.Join(outside, "secret.png"):        "absolute paths must be inside $JARVIS_SCRATCH",
		"../secret.png":                             "parent traversal",
		"$JARVIS_SCRATCH/../secret.png":             "parent traversal",
		filepath.Join(scratch, "..", "secret.png"):  "absolute paths must be inside $JARVIS_SCRATCH",
		".git/shot.png":                             "protected",
		"$JARVIS_SCRATCH/.ssh/shot.png":             "protected",
		"link.png":                                  "symlink",
		"linked/secret.png":                         "symlink",
		"$JARVIS_SCRATCH/link.png":                  "symlink",
		"$JARVIS_SCRATCH/linked/secret.png":         "symlink",
		filepath.Join(scratch, "linked/secret.png"): "symlink",
		"$JARVIS_SCRATCH":                           "file path is required",
		"$JARVIS_SCRATCHY/secret.png":               "no such file",
	} {
		r := invoke(e, "ViewImage", map[string]any{"path": path})
		if !r.IsError || len(r.Images) != 0 || !strings.Contains(r.Content, want) {
			t.Errorf("%s: %+v, want an error with %q", path, r.Record(), want)
		}
	}
}

func TestViewImageExplainsWhatItCannotShow(t *testing.T) {
	e, dir := fixture(t, "auto", nil)
	os.WriteFile(filepath.Join(dir, "notes.md"), []byte("# Notes\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "blob.bin"), []byte{0, 1, 2, 3, 0xff}, 0o644)
	os.WriteFile(filepath.Join(dir, "empty.png"), nil, 0o644)
	os.Mkdir(filepath.Join(dir, "folder"), 0o755)
	if err := syscall.Mkfifo(filepath.Join(dir, "pipe"), 0o644); err != nil {
		t.Fatal(err)
	}
	huge, err := os.Create(filepath.Join(dir, "huge.png"))
	if err != nil {
		t.Fatal(err)
	}
	huge.Truncate(65<<20 + 1) // sparse: large without filling the disk
	huge.Close()
	for path, want := range map[string]string{
		"notes.md":  "notes.md is a text file, not an image; use Read for text",
		"blob.bin":  "blob.bin is not an image ViewImage can open (PNG, JPEG, GIF, WebP, TIFF or BMP)",
		"empty.png": "empty.png is empty",
		"folder":    "folder is not a regular file",
		"pipe":      "pipe is not a regular file",
		"huge.png":  "huge.png is larger than the 64 MiB an image may be",
		"absent":    "open absent",
	} {
		r := invoke(e, "ViewImage", map[string]any{"path": path})
		if !r.IsError || !strings.Contains(r.Content, want) {
			t.Errorf("%s: %q, want %q", path, r.Content, want)
		}
	}
}

func TestViewImageIsRegistered(t *testing.T) {
	for _, definitions := range [][]core.ToolDefinition{Definitions(nil), Definitions(agents.Builtin())} {
		found := false
		for _, definition := range definitions {
			if definition.Name == "ViewImage" {
				found = true
				if required, _ := definition.Parameters["required"].([]string); len(required) != 1 || required[0] != "path" {
					t.Fatalf("parameters %+v", definition.Parameters)
				}
			}
		}
		if !found {
			t.Fatal("ViewImage missing from the registry")
		}
	}
}

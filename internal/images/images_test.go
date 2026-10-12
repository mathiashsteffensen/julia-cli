package images

import (
	"bytes"
	"image"
	"image/gif"
	"image/png"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/image/bmp"
)

func encodePNG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func solid(w, h int) image.Image {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = 0x80
	}
	return img
}

func TestPrepareSendsSmallImagesUnchanged(t *testing.T) {
	data := encodePNG(t, solid(64, 32))
	img, err := Prepare(data)
	if err != nil {
		t.Fatal(err)
	}
	if img.MediaType != "image/png" || img.Width != 64 || img.Height != 32 || !bytes.Equal(img.Data, data) {
		t.Fatalf("got %s %dx%d, %d bytes", img.MediaType, img.Width, img.Height, len(img.Data))
	}
}

func TestPrepareScalesLargeImagesKeepingAspect(t *testing.T) {
	img, err := Prepare(encodePNG(t, solid(4000, 1000)))
	if err != nil {
		t.Fatal(err)
	}
	if img.Width != MaxEdge || img.Height != 500 || img.MediaType != "image/png" {
		t.Fatalf("got %s %dx%d", img.MediaType, img.Width, img.Height)
	}
	decoded, err := png.Decode(bytes.NewReader(img.Data))
	if err != nil || decoded.Bounds().Dx() != MaxEdge {
		t.Fatalf("encoded image does not match its size: %v", err)
	}
}

func TestPrepareConvertsFormatsProvidersReject(t *testing.T) {
	var out bytes.Buffer
	if err := bmp.Encode(&out, solid(10, 20)); err != nil {
		t.Fatal(err)
	}
	img, err := Prepare(out.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if img.MediaType != "image/png" || img.Width != 10 || img.Height != 20 {
		t.Fatalf("got %s %dx%d", img.MediaType, img.Width, img.Height)
	}
}

// A GIF, even a small one, goes as a PNG of its first frame, which every
// provider takes.
func TestPrepareConvertsGIFToPNG(t *testing.T) {
	var out bytes.Buffer
	if err := gif.Encode(&out, solid(16, 8), nil); err != nil {
		t.Fatal(err)
	}
	img, err := Prepare(out.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := png.Decode(bytes.NewReader(img.Data)); err != nil || img.MediaType != "image/png" || img.Width != 16 || img.Height != 8 {
		t.Fatalf("got %s %dx%d: %v", img.MediaType, img.Width, img.Height, err)
	}
}

func TestPrepareFallsBackToJPEGWhenPNGIsTooLarge(t *testing.T) {
	noise := image.NewNRGBA(image.Rect(0, 0, 1600, 1200))
	random := rand.New(rand.NewSource(1))
	random.Read(noise.Pix)
	for i := 3; i < len(noise.Pix); i += 4 {
		noise.Pix[i] = 0xff
	}
	img, err := Prepare(encodePNG(t, noise))
	if err != nil {
		t.Fatal(err)
	}
	if img.MediaType != "image/jpeg" || len(img.Data) > maxBytes || img.Width != 1600 {
		t.Fatalf("got %s %dx%d, %d bytes", img.MediaType, img.Width, img.Height, len(img.Data))
	}
}

func TestPrepareRejectsNonImages(t *testing.T) {
	if _, err := Prepare([]byte("hello, world")); err == nil {
		t.Fatal("text accepted as an image")
	}
}

func TestPathsRecognizesDroppedImageFiles(t *testing.T) {
	dir := t.TempDir()
	plain := filepath.Join(dir, "shot.png")
	spaced := filepath.Join(dir, "Screen Shot 1.jpg")
	notes := filepath.Join(dir, "notes.txt")
	for _, path := range []string{plain, spaced, notes} {
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	escaped := strings.ReplaceAll(spaced, " ", `\ `)
	for name, tc := range map[string]struct {
		paste string
		want  []string
	}{
		"plain":           {plain, []string{plain}},
		"trailing space":  {plain + " ", []string{plain}},
		"escaped":         {escaped, []string{spaced}},
		"single quoted":   {"'" + spaced + "'", []string{spaced}},
		"double quoted":   {`"` + spaced + `"`, []string{spaced}},
		"file URL":        {"file://" + strings.ReplaceAll(spaced, " ", "%20"), []string{spaced}},
		"several":         {plain + " " + escaped, []string{plain, spaced}},
		"not an image":    {notes, nil},
		"missing":         {filepath.Join(dir, "gone.png"), nil},
		"relative":        {"shot.png", nil},
		"prose":           {"look at " + plain, nil},
		"unterminated":    {"'" + plain, nil},
		"remote file URL": {"file://example.com" + plain, nil},
		"directory":       {dir, nil},
	} {
		if got := Paths(tc.paste); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: Paths(%q) = %q, want %q", name, tc.paste, got, tc.want)
		}
	}
}

func TestLoadNamesTheFileInErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broken.png")
	if err := os.WriteFile(path, []byte("not a png"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "broken.png") {
		t.Fatalf("got %v", err)
	}
}

func TestEncodePreparesInMemoryPictures(t *testing.T) {
	for _, tc := range []struct{ w, h, wantW, wantH int }{
		{1440, 900, 1440, 900},
		{4000, 2500, 2000, 1250},
	} {
		img, err := Encode(solid(tc.w, tc.h))
		if err != nil {
			t.Fatal(err)
		}
		if img.MediaType != "image/png" || img.Width != tc.wantW || img.Height != tc.wantH {
			t.Fatalf("%dx%d: %s %dx%d", tc.w, tc.h, img.MediaType, img.Width, img.Height)
		}
		if decoded, err := Prepare(img.Data); err != nil || decoded.Width != tc.wantW {
			t.Fatalf("%dx%d: encoded data does not decode: %v", tc.w, tc.h, err)
		}
	}
	if _, err := Encode(image.NewNRGBA(image.Rect(0, 0, 0, 0))); err == nil {
		t.Fatal("encoded an empty picture")
	}
}

// Clipboard data is read no further than the file limit.
func TestReadClipboardIsBounded(t *testing.T) {
	if _, err := readClipboard(io.LimitReader(zeros{}, MaxFileBytes+1)); err == nil {
		t.Fatal("an oversized clipboard image was read")
	}
	data, err := readClipboard(strings.NewReader("\x89PNG"))
	if err != nil || string(data) != "\x89PNG" {
		t.Fatalf("readClipboard = %q, %v", data, err)
	}
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

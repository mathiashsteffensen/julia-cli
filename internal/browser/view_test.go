package browser

import (
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// saveShot writes a screenshot where a person can look at it: the directory
// in JARVIS_BROWSER_SHOTS, if set.
func saveShot(t *testing.T, name string, img image.Image) {
	t.Helper()
	dir := os.Getenv("JARVIS_BROWSER_SHOTS")
	if dir == "" {
		return
	}
	f, err := os.Create(filepath.Join(dir, name+".png"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
}

// near reports whether a pixel is within tolerance of a colour.
func near(c color.Color, r, g, b uint8) bool {
	cr, cg, cb, _ := c.RGBA()
	d := func(x uint32, y uint8) int { return max(int(x>>8)-int(y), int(y)-int(x>>8)) }
	return d(cr, r) < 12 && d(cg, g) < 12 && d(cb, b) < 12
}

func TestScreenshots(t *testing.T) {
	f := open(t)
	f.goTo(t, "/app")
	img, note, err := f.p.Screenshot(f.ctx, ScreenshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() != 1280 || b.Dy() != 800 || note != "" {
		t.Fatalf("viewport shot %v, note %q", b, note)
	}
	if !near(img.At(400, 30), 0x1d, 0x4e, 0xd8) {
		t.Fatalf("the header is not blue: %v", img.At(400, 30))
	}
	saveShot(t, "app", img)

	el, _, err := f.p.Screenshot(f.ctx, ScreenshotOptions{Target: &Target{Selector: "table"}})
	if err != nil {
		t.Fatal(err)
	}
	if b := el.Bounds(); b.Dx() < 60 || b.Dx() > 400 || b.Dy() < 30 || b.Dy() > 200 {
		t.Fatalf("element shot of the table is %v", b)
	}
	saveShot(t, "app-table", el)

	clip, _, err := f.p.Screenshot(f.ctx, ScreenshotOptions{Clip: &Rect{0, 0, 200, 50}})
	if err != nil {
		t.Fatal(err)
	}
	if b := clip.Bounds(); b.Dx() != 200 || b.Dy() != 50 {
		t.Fatalf("clip %v", b)
	}
}

func TestFullPageScreenshot(t *testing.T) {
	f := open(t)
	f.goTo(t, "/long")
	img, _, err := f.p.Screenshot(f.ctx, ScreenshotOptions{FullPage: true})
	if err != nil {
		t.Fatal(err)
	}
	b := img.Bounds()
	if b.Dx() != 1280 || b.Dy() != 5000 {
		t.Fatalf("full page %v, want 1280×5000", b)
	}
	for _, band := range []struct {
		y       int
		r, g, b uint8
	}{{100, 0xe1, 0x1d, 0x48}, {1300, 0xf5, 0x9e, 0x0b}, {2600, 0x10, 0xb9, 0x81}, {4900, 0x25, 0x63, 0xeb}} {
		if c := img.At(640, band.y); !near(c, band.r, band.g, band.b) {
			t.Errorf("at y=%d the colour is %v", band.y, c)
		}
	}
	saveShot(t, "long-full", img)
	// A page too tall to read whole is captured from the top, as far as
	// stays legible, and the note says so.
	img, note, err := f.p.Screenshot(f.ctx, ScreenshotOptions{FullPage: true, MaxHeight: 2000})
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() != 1280 || b.Dy() != 2000 || !strings.Contains(note, "the page is 5000 px tall, too tall to read whole; captured the top 2000 px") {
		t.Fatalf("capped full page %v: %s", b, note)
	}
	// The capture leaves the page as it was: same viewport, same scroll.
	if got := f.eval(t, "innerWidth + 'x' + innerHeight + '@' + scrollY"); got != `"1280x800@0"` {
		t.Fatalf("after a full-page capture: %s", got)
	}
}

func TestDarkModeAndReducedMotion(t *testing.T) {
	f := open(t)
	f.goTo(t, "/theme")
	light, _, err := f.p.Screenshot(f.ctx, ScreenshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !near(light.At(600, 400), 0xff, 0xff, 0xff) {
		t.Fatalf("light background %v", light.At(600, 400))
	}
	if err := f.p.EmulateMedia(f.ctx, Media{ColorScheme: "dark", ReducedMotion: "reduce"}); err != nil {
		t.Fatal(err)
	}
	dark, _, err := f.p.Screenshot(f.ctx, ScreenshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !near(dark.At(600, 400), 0x0b, 0x10, 0x20) {
		t.Fatalf("dark background %v", dark.At(600, 400))
	}
	saveShot(t, "theme-dark", dark)
	if got := f.eval(t, "matchMedia('(prefers-reduced-motion: reduce)').matches && matchMedia('(prefers-color-scheme: dark)').matches"); got != "true" {
		t.Fatal("media features not emulated")
	}
	// It carries over a reload, and clears.
	if err := f.p.Reload(f.ctx, ""); err != nil {
		t.Fatal(err)
	}
	if got := f.eval(t, "matchMedia('(prefers-color-scheme: dark)').matches"); got != "true" {
		t.Fatal("dark mode lost on reload")
	}
	if err := f.p.EmulateMedia(f.ctx, Media{}); err != nil {
		t.Fatal(err)
	}
	if got := f.eval(t, "matchMedia('(prefers-color-scheme: dark)').matches"); got != "false" {
		t.Fatal("dark mode not cleared")
	}
	if err := f.p.EmulateMedia(f.ctx, Media{ColorScheme: "sepia"}); err == nil {
		t.Fatal("a bad color scheme was accepted")
	}
}

func TestMobileViewport(t *testing.T) {
	f := open(t)
	f.goTo(t, "/responsive")
	if snap := f.snapshot(t); !strings.Contains(snap, "Desktop layout") || strings.Contains(snap, "Mobile layout") {
		t.Fatalf("desktop:\n%s", snap)
	}
	if err := f.p.Resize(f.ctx, Viewport{Width: 390, Height: 844, Scale: 3, Mobile: true}); err != nil {
		t.Fatal(err)
	}
	if err := f.p.Reload(f.ctx, ""); err != nil {
		t.Fatal(err)
	}
	snap := f.snapshot(t)
	if !strings.Contains(snap, "Mobile layout") || strings.Contains(snap, "Desktop layout") || !strings.Contains(snap, "Viewport: 390×844 mobile @3x") {
		t.Fatalf("mobile:\n%s", snap)
	}
	if got := f.eval(t, "env.textContent"); got != `"mobile UA, touch, 390"` {
		t.Fatalf("mobile environment: %s", got)
	}
	img, _, err := f.p.Screenshot(f.ctx, ScreenshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() != 1170 || b.Dy() != 2532 {
		t.Fatalf("a 390×844 @3x screen captured as %v", b)
	}
	if !near(img.At(585, 60), 0x0f, 0x76, 0x6e) {
		t.Fatalf("the nav bar is not teal at the top: %v", img.At(585, 60))
	}
	saveShot(t, "responsive-mobile", img)
	// Clicks still land where they should on a scaled, mobile page.
	f.eval(t, "window.hit = ''; document.querySelector('nav').onclick = () => window.hit = 'nav'")
	if _, err := f.p.Click(f.ctx, Target{Text: "Acme"}); err != nil {
		t.Fatal(err)
	}
	if got := f.eval(t, "hit"); got != `"nav"` {
		t.Fatalf("click on mobile: %s", got)
	}
	if err := f.p.Resize(f.ctx, Viewport{Width: 50, Height: 50}); err == nil {
		t.Fatal("a tiny viewport was accepted")
	}
	if err := f.p.Resize(f.ctx, DefaultViewport); err != nil {
		t.Fatal(err)
	}
	if err := f.p.Reload(f.ctx, ""); err != nil {
		t.Fatal(err)
	}
	if got := f.eval(t, "env.textContent"); got != `"desktop UA, no touch, 1280"` {
		t.Fatalf("back on the desktop: %s", got)
	}
}

func TestConsoleAndErrors(t *testing.T) {
	f := open(t)
	f.goTo(t, "/console")
	time.Sleep(500 * time.Millisecond)
	logs := f.p.Logs()
	var all []string
	for _, e := range logs.Entries {
		all = append(all, e.Kind+" "+e.String())
	}
	text := strings.Join(all, "\n")
	t.Log("\n" + text)
	for _, want := range []string{
		"console log hello Ada, you are 36 (/console:",
		`console warning careful {a: 1, b: "x"}`,
		"console error Error: boom",
		"console info status 404",
		"network error 404 Not Found GET " + f.site.url("/missing") + " (fetch)",
		"exception error Uncaught ReferenceError: undefinedFunction is not defined",
		"exception error Uncaught Error: nobody caught me",
		"network error failed GET http://" + f.site.refused + "/refused (fetch): net::ERR_CONNECTION_REFUSED",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("logs lack %q", want)
		}
	}
	if again := f.p.Logs(); len(again.Entries) != 0 {
		t.Fatalf("logs were not drained: %v", again.Entries)
	}
	// Bounded: a flood keeps the newest and counts the rest.
	f.eval(t, "for (let i = 0; i < 1000; i++) console.log('line', i)")
	time.Sleep(300 * time.Millisecond)
	logs = f.p.Logs()
	if len(logs.Entries) != maxLogEntries || logs.Dropped != 1000-maxLogEntries || !strings.Contains(logs.Entries[len(logs.Entries)-1].Text, "line 999") {
		t.Fatalf("flood: %d kept, %d dropped", len(logs.Entries), logs.Dropped)
	}
}

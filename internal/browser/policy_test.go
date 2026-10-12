package browser

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPolicySeesEveryRequest(t *testing.T) {
	// LoopbackOnly (on in every test) keeps what the policy cannot stop,
	// the WebSocket, off the network.
	f := open(t)
	f.goTo(t, "/requests")
	if err := f.p.WaitFor(f.ctx, Wait{Text: "sw registered"}, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	// Give the service worker's install and the cross-site frame a moment.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !(f.rec.saw("from-sw") && f.rec.saw("from-frame") && f.rec.saw("from-worker") && f.rec.saw("from-shared")) {
		time.Sleep(100 * time.Millisecond)
	}
	for _, want := range []string{
		"/requests",                           // the document
		"/style.css", "blocked.example/x.css", // stylesheets
		"/img.svg", "blocked.example/pixel.gif", // images
		"/script.js",                                        // a script
		"localhost", "/frame", "blocked.example/from-frame", // a cross-site frame and its fetch
		"/api/data", "blocked.example/api", "/api/xhr", // fetch and XHR
		"/worker.js", "/api/worker", "blocked.example/from-worker", // a worker
		"/sw.js", "/api/sw", "blocked.example/from-sw", // a service worker
		"/shared.js", "/api/shared", "blocked.example/from-shared", // a shared worker
		"blocked.example/beacon", // a beacon
	} {
		if !f.rec.saw(want) {
			t.Errorf("the policy never saw %s; it saw:\n%s", want, strings.Join(f.rec.urls(), "\n"))
		}
	}
	// What it refused never reached a server and failed in the page.
	if got := f.eval(t, "[...document.querySelectorAll('#log li')].map(l => l.textContent).sort().join(', ')"); !strings.Contains(got, "remote fetch failed") || !strings.Contains(got, "fetch true") || !strings.Contains(got, "xhr 200") {
		t.Fatalf("page log: %s", got)
	}
	notes := noteText(f.c.Notes())
	// Repeated blocks of one origin come as one note with a count.
	for _, want := range []string{
		"requests to http://blocked.example (GET /x.css, GET /pixel.gif, GET /api, …): not a loopback address",
		"blocked GET ws://blocked.example/socket (websocket): not a loopback address",
	} {
		if !strings.Contains(notes, want) {
			t.Errorf("notes lack %q:\n%s", want, notes)
		}
	}
	logs := f.p.Logs()
	var blocked int
	for _, e := range logs.Entries {
		if e.Kind == "blocked" {
			blocked++
		}
	}
	if blocked < 4 {
		t.Errorf("logs record %d blocked requests: %v", blocked, logs.Entries)
	}
}

func TestWebSocketsTheBrowserCannotStopAreReported(t *testing.T) {
	f := open(t, func(o *ContextOptions) {
		o.LoopbackOnly = false
		o.Policy = func(r Request) Decision {
			if r.Type == "WebSocket" {
				return Decision{Reason: "no sockets"}
			}
			return Decision{Allow: true}
		}
	})
	f.goTo(t, "/")
	f.eval(t, `await new Promise(done => { const ws = new WebSocket('ws://'+location.host+'/socket'); ws.onerror = ws.onopen = () => done(1); setTimeout(done, 3000); })`)
	if notes := noteText(f.c.Notes()); !strings.Contains(notes, "/socket opened although the policy refuses it (no sockets): WebSockets cannot be intercepted") {
		t.Fatalf("notes:\n%s", notes)
	}
}

func TestLoopbackOnlyStopsWebSockets(t *testing.T) {
	f := open(t)
	f.goTo(t, "/")
	got := f.eval(t, `await new Promise(done => {
  const ws = new WebSocket('ws://blocked.example/socket');
  ws.onopen = () => done('open');
  ws.onerror = () => done('error');
  setTimeout(() => done('pending'), 5000);
})`)
	if got != `"error"` {
		t.Fatalf("WebSocket to a remote host: %s", got)
	}
	if notes := noteText(f.c.Notes()); !strings.Contains(notes, "blocked GET ws://blocked.example/socket (websocket): not a loopback address") {
		t.Fatalf("notes:\n%s", notes)
	}
	// Loopback still connects directly: the handshake reaches the server.
	before := f.site.hit("/socket")
	f.eval(t, `await new Promise(done => { const ws = new WebSocket('ws://'+location.host+'/socket'); ws.onerror = ws.onopen = () => done(1); setTimeout(done, 3000); })`)
	if f.site.hit("/socket") == before {
		t.Fatal("a loopback WebSocket never reached the server")
	}
	// So does an allowed page, and an IP literal off loopback cannot.
	if err := f.p.Navigate(f.ctx, f.site.url("/app"), WaitLoad); err != nil {
		t.Fatal(err)
	}
}

func TestRedirectsArePoliced(t *testing.T) {
	f := open(t)
	err := f.p.Navigate(f.ctx, f.site.url("/redirect-out"), WaitLoad)
	if err == nil || !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("redirect to a refused origin: %v", err)
	}
	if !f.rec.saw("blocked.example/landing") {
		t.Fatal("the policy never saw the redirect's target")
	}
	if err := f.p.Navigate(f.ctx, f.site.url("/redirect-in"), WaitLoad); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(f.p.URL(), "/app") {
		t.Fatalf("URL after an allowed redirect: %s", f.p.URL())
	}
	err = f.p.Navigate(f.ctx, f.site.url("/loop"), WaitLoad)
	if err == nil || !strings.Contains(err.Error(), "redirect loop") {
		t.Fatalf("redirect loop: %v", err)
	}
}

func TestNavigationRefusals(t *testing.T) {
	f := open(t)
	for _, tc := range []struct{ url, want string }{
		{"chrome://version", "chrome: URLs are not available"},
		{"devtools://devtools/bundled/inspector.html", "devtools: URLs are not available"},
		{"view-source:" + f.site.url("/app"), "view-source: URLs are not available"},
		{"chrome-extension://abc/x.html", "not available"},
		{"javascript:alert(1)", "evaluate script instead"},
		{"about:settings", "only about:blank"},
		{"http://blocked.example/", "not a loopback address"},
		{"localhost:3000", "not available"},
		{"/relative", "not an absolute URL"},
	} {
		err := f.p.Navigate(f.ctx, tc.url, WaitLoad)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Navigate(%s) = %v, want %q", tc.url, err, tc.want)
		}
	}
	if err := f.p.Navigate(f.ctx, "about:blank", WaitLoad); err != nil {
		t.Fatal(err)
	}
	if err := f.p.Navigate(f.ctx, "data:text/html,<h1>Inline</h1>", WaitLoad); err != nil {
		t.Fatal(err)
	}
	if snap := f.snapshot(t); !strings.Contains(snap, `heading "Inline"`) {
		t.Fatalf("data: page:\n%s", snap)
	}
}

func TestFileURLsAreConfinedByThePolicy(t *testing.T) {
	f := open(t)
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	f.rec.mu.Lock()
	f.rec.fileRoot = root
	f.rec.mu.Unlock()
	os.WriteFile(filepath.Join(root, "logo.svg"), []byte(`<svg xmlns="http://www.w3.org/2000/svg" width="40" height="40"><circle cx="20" cy="20" r="20" fill="teal"/></svg>`), 0o644)
	os.WriteFile(filepath.Join(root, "index.html"), []byte(`<!doctype html><title>Local</title><h1>Local file</h1>
<img src="logo.svg" alt="Logo" id=ok><img src="file:///etc/hosts" alt="Hosts"><iframe src="file:///etc/passwd"></iframe>`), 0o644)
	if err := f.p.Navigate(f.ctx, "file://"+filepath.Join(root, "index.html"), WaitLoad); err != nil {
		t.Fatal(err)
	}
	if got := f.eval(t, "ok.naturalWidth"); got != "40" {
		t.Fatalf("an image inside the root did not load: %s", got)
	}
	notes := noteText(f.c.Notes())
	for _, want := range []string{"file:///etc/hosts", "file:///etc/passwd", "outside the workspace"} {
		if !strings.Contains(notes, want) {
			t.Errorf("notes lack %q:\n%s", want, notes)
		}
	}
	if err := f.p.Navigate(f.ctx, "file:///etc/hosts", WaitLoad); err == nil || !strings.Contains(err.Error(), "outside the workspace") {
		t.Fatalf("navigating outside the root: %v", err)
	}
}

func TestPopupsAreTrackedAndPoliced(t *testing.T) {
	f := open(t)
	f.goTo(t, "/popups")
	if _, err := f.p.Click(f.ctx, Target{Text: "Open app"}); err != nil {
		t.Fatal(err)
	}
	popup := waitPages(t, f, 2)[1]
	if popup.Opener() != f.p.ID() {
		t.Fatalf("popup opener %q", popup.Opener())
	}
	if err := popup.WaitFor(f.ctx, Wait{Text: "Checkout"}, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	// The opener stays usable while the popup is in front.
	if _, err := f.p.Click(f.ctx, Target{Text: "New window"}); err != nil {
		t.Fatal(err)
	}
	third := waitPages(t, f, 3)[2]
	if err := third.WaitFor(f.ctx, Wait{Text: "Alert"}, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := f.p.Click(f.ctx, Target{Text: "Bad popup"}); err != nil {
		t.Fatal(err)
	}
	waitPages(t, f, 4)
	time.Sleep(500 * time.Millisecond)
	notes := noteText(f.c.Notes())
	for _, want := range []string{f.p.ID() + " opened a new page " + popup.ID(), f.p.ID() + " opened a new page " + third.ID(), "blocked GET http://blocked.example/ (document)"} {
		if !strings.Contains(notes, want) {
			t.Errorf("notes lack %q:\n%s", want, notes)
		}
	}
	if err := popup.Close(f.ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := popup.Snapshot(f.ctx, SnapshotOptions{}); err == nil {
		t.Fatal("a closed page took a snapshot")
	}
	if len(f.c.Pages()) != 3 {
		t.Fatalf("pages after closing one: %d", len(f.c.Pages()))
	}
	// A page closing itself is noticed.
	if _, err := third.Eval(f.ctx, "setTimeout(() => window.close(), 50); 1"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !third.isClosed() {
		time.Sleep(50 * time.Millisecond)
	}
	if !third.isClosed() {
		t.Fatal("window.close() went unnoticed")
	}
}

func waitPages(t *testing.T, f *fixture, n int) []*Page {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if pages := f.c.Pages(); len(pages) >= n {
			return pages
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("expected %d pages, have %d", n, len(f.c.Pages()))
	return nil
}

func TestDownloadsAreRefused(t *testing.T) {
	f := open(t)
	f.goTo(t, "/downloads")
	if _, err := f.p.Click(f.ctx, Target{Text: "Get report"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	var notes string
	for time.Now().Before(deadline) && !strings.Contains(notes, "report.csv") {
		notes += noteText(f.c.Notes())
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(notes, `refused a download of "report.csv"`) {
		t.Fatalf("notes:\n%s", notes)
	}

	dir := t.TempDir()
	g := open(t, func(o *ContextOptions) { o.Downloads = dir })
	g.goTo(t, "/downloads")
	if _, err := g.p.Click(g.ctx, Target{Text: "Get report"}); err != nil {
		t.Fatal(err)
	}
	notes = ""
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(notes, "downloaded") {
		notes += noteText(g.c.Notes())
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(notes, `downloaded "report.csv" (8 bytes)`) {
		t.Fatalf("notes:\n%s", notes)
	}

	// Keep moves a finished download where the caller wants it, by the name
	// the page suggested, and the note says where.
	kept := filepath.Join(t.TempDir(), "kept.csv")
	h := open(t, func(o *ContextOptions) {
		o.Downloads = t.TempDir()
		o.Keep = func(path, name string) (string, error) {
			if name != "report.csv" {
				return "", fmt.Errorf("named %q", name)
			}
			return kept, os.Rename(path, kept)
		}
	})
	h.goTo(t, "/downloads")
	if _, err := h.p.Click(h.ctx, Target{Text: "Get report"}); err != nil {
		t.Fatal(err)
	}
	notes = ""
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(notes, "downloaded") {
		notes += noteText(h.c.Notes())
		time.Sleep(50 * time.Millisecond)
	}
	if data, err := os.ReadFile(kept); err != nil || string(data) != "a,b\n1,2\n" || !strings.Contains(notes, `downloaded "report.csv" (8 bytes) to `+kept) {
		t.Fatalf("kept %q, %v; notes:\n%s", data, err, notes)
	}

	// A download past MaxDownload is canceled and nothing is kept.
	keeps := 0
	k := open(t, func(o *ContextOptions) {
		o.Downloads = t.TempDir()
		o.MaxDownload = 4
		o.Keep = func(path, name string) (string, error) { keeps++; return path, nil }
	})
	k.goTo(t, "/downloads")
	if _, err := k.p.Click(k.ctx, Target{Text: "Get report"}); err != nil {
		t.Fatal(err)
	}
	notes = ""
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(notes, "canceled") {
		notes += noteText(k.c.Notes())
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	notes += noteText(k.c.Notes())
	if !strings.Contains(notes, `canceled the download of "report.csv": it is larger than`) || strings.Contains(notes, "downloaded") || keeps != 0 {
		t.Fatalf("kept %d; notes:\n%s", keeps, notes)
	}
}

func TestBasicAuthFailsFast(t *testing.T) {
	f := open(t)
	start := time.Now()
	err := f.p.Navigate(f.ctx, f.site.url("/auth"), WaitLoad)
	if time.Since(start) > 5*time.Second {
		t.Fatalf("an auth challenge took %v", time.Since(start))
	}
	notes := noteText(f.c.Notes())
	if !strings.Contains(notes, `refused an HTTP authentication challenge from `+f.site.url("/auth")+` (realm "Admin")`) {
		t.Fatalf("navigate: %v\nnotes:\n%s", err, notes)
	}
}

func TestPolicyPanicBlocks(t *testing.T) {
	f := open(t, func(o *ContextOptions) {
		o.Policy = func(r Request) Decision {
			if strings.Contains(r.URL.Path, "boom") {
				panic("policy bug")
			}
			return Decision{Allow: true}
		}
	})
	f.goTo(t, "/")
	got := f.eval(t, "await fetch('/boom').then(() => 'loaded', () => 'failed')")
	if got != `"failed"` {
		t.Fatalf("a request the policy panicked on: %s", got)
	}
	if f.site.hit("/boom") != 0 {
		t.Fatal("a request the policy panicked on reached the server")
	}

}

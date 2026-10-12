package browser

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// The tests drive the real Chrome that Find discovers, shared by the whole
// test binary; each test gets a context of its own. TestMain closes it and
// checks that no Chrome process outlives it.

var shared struct {
	once sync.Once
	b    *Browser
	err  error
	skip string
	site *site
}

func TestMain(m *testing.M) {
	code := m.Run()
	if shared.b != nil {
		pid, profile := shared.b.PID(), shared.b.ProfileDir()
		shared.b.Close()
		if left := leftovers(pid, profile); len(left) > 0 {
			fmt.Fprintf(os.Stderr, "browser processes left after Close: %v\n", left)
			code = 1
		}
	}
	if shared.site != nil {
		shared.site.close()
	}
	os.Exit(code)
}

// testBrowser returns the shared browser, skipping the test when this
// machine has no Chrome.
func testBrowser(t *testing.T) *Browser {
	t.Helper()
	shared.once.Do(func() {
		bin, err := Find(os.Getenv("JULIA_BROWSER"))
		if err != nil {
			shared.skip = err.Error()
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		shared.b, shared.err = Launch(ctx, Options{Binary: bin})
		shared.site = newSite()
	})
	if shared.skip != "" {
		t.Skip("no Chrome to test with: " + shared.skip)
	}
	if shared.err != nil {
		t.Fatal(shared.err)
	}
	return shared.b
}

// recorder is a policy that allows the test servers and file:// under one
// root, refuses everything else, and remembers every request it saw.
type recorder struct {
	mu       sync.Mutex
	requests []Request
	fileRoot string
}

func (r *recorder) policy(req Request) Decision {
	r.mu.Lock()
	r.requests = append(r.requests, req)
	root := r.fileRoot
	r.mu.Unlock()
	switch req.URL.Scheme {
	case "data", "blob":
		return Decision{Allow: true}
	case "file":
		if root != "" && strings.HasPrefix(req.URL.Path, root+"/") {
			return Decision{Allow: true}
		}
		return Decision{Reason: "outside the workspace"}
	}
	if isLoopback(req.URL.Hostname()) {
		return Decision{Allow: true}
	}
	return Decision{Reason: "not a loopback address"}
}

// saw reports whether a request whose URL contains part was asked about.
func (r *recorder) saw(part string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, req := range r.requests {
		if strings.Contains(req.URL.String(), part) {
			return true
		}
	}
	return false
}

func (r *recorder) urls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, req := range r.requests {
		out = append(out, req.Type+" "+req.URL.String())
	}
	return out
}

type fixture struct {
	b    *Browser
	c    *Context
	p    *Page
	rec  *recorder
	site *site
	ctx  context.Context
}

// open gives a test a fresh context and page, with the recorder as policy.
func open(t *testing.T, opts ...func(*ContextOptions)) *fixture {
	t.Helper()
	return openOn(t, testBrowser(t), opts...)
}

// launch starts a browser of the test's own, closed as the test ends.
func launch(t *testing.T, opts Options) *Browser {
	t.Helper()
	testBrowser(t) // skips the test without a Chrome
	bin, _ := Find(os.Getenv("JULIA_BROWSER"))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	opts.Binary = bin
	b, err := Launch(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	return b
}

// openOn is open, in browser b.
func openOn(t *testing.T, b *Browser, opts ...func(*ContextOptions)) *fixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	rec := &recorder{}
	// As JULIA browses: loopback only, so no test reaches the network.
	o := ContextOptions{Policy: rec.policy, LoopbackOnly: true}
	for _, f := range opts {
		f(&o)
	}
	c, err := b.NewContext(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	p, err := c.NewPage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{b: b, c: c, p: p, rec: rec, site: shared.site, ctx: ctx}
}

func (f *fixture) goTo(t *testing.T, path string) {
	t.Helper()
	if err := f.p.Navigate(f.ctx, f.site.url(path), WaitLoad); err != nil {
		t.Fatalf("navigate %s: %v", path, err)
	}
}

func (f *fixture) eval(t *testing.T, js string) string {
	t.Helper()
	out, err := f.p.Eval(f.ctx, js)
	if err != nil {
		t.Fatalf("eval %s: %v", js, err)
	}
	return out
}

func (f *fixture) snapshot(t *testing.T) string {
	t.Helper()
	snap, err := f.p.Snapshot(f.ctx, SnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

// waitText polls an expression until it yields want.
func (f *fixture) waitEval(t *testing.T, js, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var got string
	for time.Now().Before(deadline) {
		got = f.eval(t, js)
		if got == want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s = %s, want %s", js, got, want)
}

func noteText(notes []Note) string {
	var b strings.Builder
	for _, n := range notes {
		fmt.Fprintf(&b, "[%s %s] %s\n", n.Page, n.Kind, n.Text)
	}
	return b.String()
}

// site serves the pages the tests drive: realistic markup and scripts, on
// two origins (127.0.0.1 and localhost) so iframes can be cross-site.
type site struct {
	main, other *httptest.Server
	otherURL    string
	sameSiteURL string // the other server on 127.0.0.1: same site, another origin
	refused     string // a loopback address nothing listens on
	hang        chan struct{}
	mu          sync.Mutex
	hits        map[string]int
}

func newSite() *site {
	s := &site{hang: make(chan struct{}), hits: map[string]int{}}
	s.main = httptest.NewServer(http.HandlerFunc(s.serveMain))
	s.other = httptest.NewUnstartedServer(http.HandlerFunc(s.serveOther))
	l, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		panic(err)
	}
	s.other.Listener = l
	s.other.Start()
	s.otherURL = strings.Replace(s.other.URL, "127.0.0.1", "localhost", 1)
	s.sameSiteURL = s.other.URL
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	s.refused = closed.Addr().String()
	closed.Close()
	return s
}

func (s *site) close() {
	close(s.hang)
	s.main.Close()
	s.other.Close()
}

func (s *site) url(path string) string { return s.main.URL + path }

func (s *site) hit(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits[path]
}

func (s *site) serveOther(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.hits["other"+r.URL.Path]++
	s.mu.Unlock()
	switch r.URL.Path {
	case "/pay":
		html(w, "Payments", `<h3>Card</h3><input aria-label="Card number"><button onclick="this.textContent='Paid'">Pay now</button>`)
	case "/reviews":
		html(w, "Reviews", `<button onclick="this.textContent='Thanks!'">Helpful</button>`)
	case "/frame":
		html(w, "Frame", `<p>cross-site frame</p><script>fetch('http://blocked.example/from-frame').catch(() => {})</script>`)
	default:
		http.NotFound(w, r)
	}
}

func html(w http.ResponseWriter, title, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!doctype html><html lang=en><head><meta charset=utf-8><title>%s</title>
<style>body{font:16px system-ui,sans-serif;margin:16px} button{font:inherit}</style></head><body>%s</body></html>`, title, body)
}

func script(w http.ResponseWriter, js string) {
	w.Header().Set("Content-Type", "text/javascript")
	fmt.Fprint(w, js)
}

func (s *site) serveMain(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.hits[r.URL.Path]++
	s.mu.Unlock()
	switch r.URL.Path {
	case "/app":
		html(w, "Acme Checkout", `
<header style="display:flex;gap:16px;padding:12px;background:#1d4ed8;color:white">
  <a href="/" aria-label="Acme home" style="color:white">Acme</a>
  <nav aria-label="Main"><a href="/shop" style="color:white">Shop</a> <a href="/about" style="color:white">About</a></nav>
</header>
<main>
  <h1>Checkout</h1>
  <form id=f>
    <label>Email <input name=email type=email required placeholder="you@example.com"></label>
    <label><input type=checkbox name=news checked> Subscribe to news</label>
    <fieldset><legend>Shipping</legend>
      <label><input type=radio name=ship value=std checked> Standard</label>
      <label><input type=radio name=ship value=exp> Express</label>
    </fieldset>
    <select name=country aria-label=Country><option value=dk>Denmark</option><option value=se selected>Sweden</option></select>
    <button type=submit disabled>Pay $12.00</button>
  </form>
  <details><summary>Order details</summary><ul><li>Apples ×3</li><li>Pears ×1</li></ul></details>
  <p style="display:none">Secret hidden text</p>
  <p aria-hidden=true>Decorative flourish</p>
  <table><caption>Totals</caption><tr><th>Item</th><th>Price</th></tr><tr><td>Apples</td><td>$9.00</td></tr></table>
  <my-widget></my-widget>
  <iframe title="Help" srcdoc="<button>Chat with us</button>" style="height:60px"></iframe>
  <iframe title="Payments" src="`+s.otherURL+`/pay" style="height:120px"></iframe>
  <iframe title="Reviews" src="`+s.sameSiteURL+`/reviews" style="height:80px"></iframe>
  <div role=dialog aria-modal=true aria-labelledby=dt><h2 id=dt>Cookies</h2><button>Accept all</button></div>
</main>
<script>
customElements.define('my-widget', class extends HTMLElement {
  constructor() { super(); this.attachShadow({mode: 'open'}).innerHTML = '<button onclick="this.textContent=\'Shadow done\'">Shadow action</button><input aria-label="Shadow field">'; }
});
</script>`)
	case "/controlled":
		html(w, "Controlled", `
<label>Name <input id=fullname></label>
<p id=out></p><p id=keys>0</p>
<form id=form><input id=search aria-label=Search value="old query"><button>Go</button></form><p id=submitted></p>
<textarea id=notes aria-label=Notes></textarea>
<div id=rich contenteditable aria-label="Rich text">Draft</div>
<label>Due <input id=due type=date></label>
<script>
// Like React: the field shows state, and only input events change state.
let state = '', keydowns = 0;
fullname.addEventListener('keydown', () => keys.textContent = ++keydowns);
fullname.addEventListener('input', e => { state = e.target.value.toUpperCase(); fullname.value = state; out.textContent = state; });
form.addEventListener('submit', e => { e.preventDefault(); submitted.textContent = 'searched ' + search.value; });
due.addEventListener('change', () => out.textContent = 'due ' + due.value);
</script>`)
	case "/covered":
		html(w, "Covered", `
<button id=buy onclick="this.textContent='Bought'">Buy now</button>
<div class="cookie-banner" style="position:fixed;inset:0;background:rgba(0,0,0,.4)">We use cookies</div>`)
	case "/dialogs":
		html(w, "Dialogs", `
<button onclick="alert('Saved!'); r.textContent='alerted'">Alert</button>
<button onclick="r.textContent = confirm('Delete 3 items?') ? 'deleted' : 'kept'">Delete</button>
<button onclick="r.textContent = 'name=' + prompt('Your name?', 'anon')">Name</button>
<p id=r></p>`)
	case "/unload":
		html(w, "Unsaved", `<input id=draft aria-label=Draft><script>addEventListener('beforeunload', e => { e.preventDefault(); e.returnValue = ''; });</script>`)
	case "/popups":
		html(w, "Popups", `
<a href="/app" target=_blank>Open app</a>
<button onclick="window.open('/dialogs')">New window</button>
<button onclick="window.open('http://blocked.example/')">Bad popup</button>`)
	case "/requests":
		html(w, "Requests", `
<link rel=stylesheet href="/style.css"><link rel=stylesheet href="http://blocked.example/x.css">
<img src="/img.svg" alt=local><img src="http://blocked.example/pixel.gif" alt=remote>
<script src="/script.js"></script>
<iframe src="`+s.otherURL+`/frame"></iframe>
<ul id=log></ul>
<script>
const log = m => { const li = document.createElement('li'); li.textContent = m; document.getElementById('log').append(li); };
fetch('/api/data').then(r => r.json()).then(d => log('fetch ' + d.ok));
fetch('http://blocked.example/api').catch(e => log('remote fetch failed'));
const x = new XMLHttpRequest(); x.open('GET', '/api/xhr'); x.onload = () => log('xhr ' + x.status); x.send();
new Worker('/worker.js');
new SharedWorker('/shared.js');
navigator.sendBeacon('http://blocked.example/beacon', 'x');
const ws = new WebSocket('ws://blocked.example/socket'); ws.onerror = () => log('ws failed');
navigator.serviceWorker.register('/sw.js').then(() => log('sw registered'));
</script>`)
	case "/style.css":
		w.Header().Set("Content-Type", "text/css")
		fmt.Fprint(w, `body { background: #fafafa }`)
	case "/img.svg":
		w.Header().Set("Content-Type", "image/svg+xml")
		fmt.Fprint(w, `<svg xmlns="http://www.w3.org/2000/svg" width="10" height="10"><rect width="10" height="10" fill="green"/></svg>`)
	case "/script.js":
		script(w, `window.scriptRan = true;`)
	case "/api/data":
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok": true}`)
	case "/api/xhr", "/api/worker", "/api/sw", "/api/shared":
		fmt.Fprint(w, "ok")
	case "/worker.js":
		script(w, `fetch('/api/worker'); fetch('http://blocked.example/from-worker').catch(() => {});`)
	case "/shared.js":
		script(w, `fetch('/api/shared'); fetch('http://blocked.example/from-shared').catch(() => {});`)
	case "/sw.js":
		script(w, `self.addEventListener('install', e => e.waitUntil(Promise.all([fetch('/api/sw'), fetch('http://blocked.example/from-sw').catch(() => {})])));`)
	case "/redirect-out":
		http.Redirect(w, r, "http://blocked.example/landing", http.StatusFound)
	case "/redirect-in":
		http.Redirect(w, r, "/app", http.StatusFound)
	case "/loop":
		http.Redirect(w, r, "/loop", http.StatusFound)
	case "/slow":
		select {
		case <-s.hang:
		case <-r.Context().Done():
		}
	case "/stuck":
		html(w, "Stuck", `<h1>Almost there</h1><img src="/slow?img" alt=never>`)
	case "/poll":
		html(w, "Polling", `<h1>Live</h1><script>fetch('/slow?poll');</script>`)
	case "/auth":
		w.Header().Set("WWW-Authenticate", `Basic realm="Admin"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	case "/download":
		w.Header().Set("Content-Disposition", `attachment; filename="report.csv"`)
		w.Header().Set("Content-Type", "text/csv")
		fmt.Fprint(w, "a,b\n1,2\n")
	case "/downloads":
		html(w, "Downloads", `<a href="/download">Get report</a>`)
	case "/console":
		html(w, "Console", `<h1>Noisy</h1><script>
console.log('hello %s, you are %d', 'Ada', 36);
console.warn('careful', {a: 1, b: 'x'});
console.error(new Error('boom'));
fetch('/missing').then(r => console.info('status', r.status));
setTimeout(() => { undefinedFunction(); }, 0);
Promise.reject(new Error('nobody caught me'));
fetch('http://`+s.refused+`/refused').catch(() => {});
</script>`)
	case "/files":
		html(w, "Files", `<label>Avatar <input type=file id=one></label><label>Photos <input type=file id=many multiple></label><p id=names></p>
<script>for (const i of document.querySelectorAll('input')) i.onchange = () => names.textContent = [...i.files].map(f => f.name + ':' + f.size).join(',');</script>`)
	case "/select":
		html(w, "Select", `<label for=c>Country</label><select id=c><option value=dk>Denmark</option><option value=se>Sweden</option><option value=no disabled>Norway</option></select>
<select id=m multiple aria-label=Toppings><option>Cheese</option><option>Ham</option><option>Olives</option></select><p id=r></p>
<script>c.onchange = () => r.textContent = 'country ' + c.value; m.onchange = () => r.textContent = [...m.selectedOptions].map(o => o.value).join('+');</script>`)
	case "/long":
		html(w, "Long", `<style>body{margin:0} div{height:1250px} p{margin:0}</style><div style="background:#e11d48"></div><div style="background:#f59e0b"></div><div style="background:#10b981"></div><div style="background:#2563eb"><p id=end>The end</p></div>`)
	case "/theme":
		html(w, "Theme", `<style>:root{color-scheme:light dark} body{background:#ffffff;color:#111} @media (prefers-color-scheme: dark){body{background:#0b1020;color:#e5e7eb}}</style><h1>Theme</h1><p>Light or dark, as you prefer.</p>`)
	case "/responsive":
		html(w, "Responsive", `<meta name=viewport content="width=device-width,initial-scale=1">
<style>.mobile{display:none} @media (max-width:600px){.mobile{display:block}.desktop{display:none}} nav{display:flex;gap:8px;background:#0f766e;color:#fff;padding:12px}</style>
<nav><strong>Acme</strong></nav><p class=desktop>Desktop layout</p><p class=mobile>Mobile layout</p><p id=env></p>
<script>env.textContent = [navigator.userAgent.includes('Mobile') ? 'mobile UA' : 'desktop UA', 'ontouchstart' in window ? 'touch' : 'no touch', innerWidth].join(', ');</script>`)
	case "/scroll":
		html(w, "Scroll", `<div id=box style="height:200px;overflow:auto;border:1px solid #999"><div style="height:2000px">Inner content</div></div><div style="height:3000px">Tall page</div>`)
	case "/drag":
		html(w, "Drag", `
<div id=card draggable=true style="width:100px;height:40px;background:#fde68a">Card</div>
<div id=zone style="width:200px;height:100px;margin-top:40px;background:#bfdbfe">Drop here</div>
<div id=track style="width:300px;height:24px;margin-top:40px;background:#e5e7eb;position:relative"><div id=thumb style="width:24px;height:24px;background:#374151;position:absolute;left:0" aria-label=Volume></div></div>
<p id=value>0</p>
<script>
card.ondragstart = e => e.dataTransfer.setData('text/plain', 'card-1');
zone.ondragover = e => e.preventDefault();
zone.ondrop = e => { e.preventDefault(); zone.textContent = 'dropped ' + e.dataTransfer.getData('text/plain'); };
let dragging = false;
thumb.onpointerdown = e => { dragging = true; thumb.setPointerCapture(e.pointerId); };
thumb.onpointermove = e => { if (!dragging) return; const x = Math.max(0, Math.min(276, e.clientX - track.getBoundingClientRect().left - 12)); thumb.style.left = x + 'px'; value.textContent = Math.round(x / 276 * 100); };
thumb.onpointerup = () => dragging = false;
</script>`)
	case "/later":
		html(w, "Later", `<p id=progress>Loading…</p><script>setTimeout(() => { progress.textContent = 'Ready to go'; history.pushState({}, '', '/later/done'); }, 300);</script>`)
	case "/rerender":
		html(w, "Rerender", `<div id=root><button onclick="root.innerHTML = '<button>Again</button>'">Replace me</button></div>`)
	case "/hover":
		html(w, "Hover", `<button id=h onmouseenter="tip.hidden = false">Info</button><p id=tip hidden>Tooltip!</p>
<div id=menu oncontextmenu="event.preventDefault(); this.textContent = 'menu opened'">Right-click here</div>
<div id=dbl ondblclick="this.textContent = 'double clicked'">Double-click me</div>`)
	case "/settings", "/settings-backdrop":
		// About 1.3 viewports tall: Save is below the fold.
		cards := ""
		for _, name := range []string{"Profile", "Notifications", "Billing", "Security", "Team"} {
			cards += `<section class=card><h2>` + name + `</h2></section>`
		}
		backdrop := ""
		if r.URL.Path == "/settings-backdrop" {
			backdrop = `<div id=backdrop style="position:fixed;inset:0;background:rgba(0,0,0,.3)" onclick="this.dataset.clicked = 'yes'"></div>`
		}
		html(w, "Settings", `<style>.card{height:200px;margin:8px 0;background:#eef2ff}</style><h1>Settings</h1>`+cards+
			`<footer><button onclick="saved.textContent = 'Saved'">Save settings</button><input aria-label=Note id=note><p id=saved></p></footer>`+backdrop)
	case "/frame-banner":
		// A cross-site frame under the page's own cookie banner.
		html(w, "Checkout", `<iframe title=Payments src="`+s.otherURL+`/pay" style="height:120px"></iframe>
<div id=banner style="position:fixed;inset:0;background:rgba(0,0,0,.3)" onclick="this.dataset.clicked = 'yes'">We use cookies</div>`)
	case "/clip":
		gap := `<span style="display:inline-block;width:220px"></span>`
		html(w, "Clip", `<style>.bar{width:200px;white-space:nowrap} button{width:60px;padding:0}</style>
<div id=bar class=bar style="overflow:hidden"><button>One</button><button>Two</button><button>Three</button>`+gap+`<button onclick="out.textContent = 'hidden clicked'">Hidden action</button></div>
<div id=bar2 class=bar style="overflow:clip"><button>Uno</button><button>Dos</button><button>Tres</button>`+gap+`<button onclick="out.textContent = 'clipped clicked'">Clipped action</button></div>
<div id=bar3 class=bar style="overflow:hidden"><span style="display:inline-block;width:170px"></span><button onclick="out.textContent = 'half clicked'">Half action</button></div>
<p id=out></p>`)
	case "/guard":
		html(w, "App", `<h1>Dashboard</h1><script type=module>if (!localStorage.token) location.assign('/login')</script>`)
	case "/replace":
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<!doctype html><head><script>location.replace('/login')</script></head><body><h1>Old</h1></body>`)
	case "/login":
		html(w, "Sign in", `<h1>Sign in</h1>`)
	case "/modal-history":
		html(w, "Modal", `<button onclick="history.pushState({modal: true}, ''); m.hidden = false">Open modal</button><div id=m hidden>A modal</div>
<script>addEventListener('popstate', () => m.hidden = true)</script>`)
	case "/spa":
		html(w, "Orders", `<p id=msg>Loading…</p><script>fetch('/api/orders').then(r => r.json()).then(d => { msg.textContent = 'Orders: ' + d.join(', ') })</script>`)
	case "/api/orders", "/api/save":
		time.Sleep(150 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `["A-1", "B-2"]`)
	case "/save":
		html(w, "Save", `<button onclick="fetch('/api/save').then(() => out.textContent = 'Saved!')">Save</button>
<button onclick="fetch('/slow?save')">Sync</button><p id=out role=status></p>
<script>fetch('/missing-items').then(r => { if (!r.ok) console.error('api failed') })</script>`)
	case "/settings-modal":
		html(w, "Shop", `<header><a href="/">Home</a></header><main><h1>Groceries</h1><ul><li>Milk</li><li>Bread</li></ul>
<button onclick="d.showModal()">Settings</button><button>Checkout</button></main>
<dialog id=d aria-labelledby=dt><h2 id=dt>Settings</h2><button onclick="d.close()">Close</button></dialog>`)
	case "/big":
		var rows strings.Builder
		for i := range 3000 {
			fmt.Fprintf(&rows, `<li id=row-%d><a href="#%d">Item %d</a> <button>Act %d</button></li>`, i, i, i, i)
		}
		html(w, "Big", `<h1>Inventory</h1><ul>`+rows.String()+`</ul>`)
	case "/missing":
		http.NotFound(w, r)
	case "/":
		html(w, "Home", `<h1>Home</h1>`)
	default:
		http.NotFound(w, r)
	}
}

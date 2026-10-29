package tools

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"julia/internal/browser"
	"julia/internal/core"
)

// A value is what bash makes of it as a double-quoted string, and a command
// it runs failing fails it rather than sending a header with a hole in it.
func TestExpandHeader(t *testing.T) {
	t.Setenv("JULIA_AUTH_TEST", "ada")
	for _, tc := range []struct{ value, want, err string }{
		{"Bearer $(printf %s glsa_tok)", "Bearer glsa_tok", ""},
		{"$JULIA_AUTH_TEST", "ada", ""},
		{"Basic ${JULIA_AUTH_TEST}:$(echo pw)", "Basic ada:pw", ""},
		{"plain value, 'quoted'", "plain value, 'quoted'", ""},
		{`say \"hi\"`, `say "hi"`, ""},
		{"Bearer $(echo denied >&2; exit 3)", "", "exit status 3: denied"},
		{"$JULIA_AUTH_UNSET", "", "expanded to nothing"},
		{"$(printf 'a\\nb')", "", "more than one line"},
		{`unbalanced "`, "", "exit status"},
	} {
		got, err := expandHeader(context.Background(), tc.value)
		if tc.err == "" && (err != nil || got != tc.want) || tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)) {
			t.Errorf("expandHeader(%q) = %q, %v; want %q, %q", tc.value, got, err, tc.want, tc.err)
		}
	}
}

// Headers go with the origin's own requests and top-level GET navigations to
// it, never with another site's requests or to another origin.
func TestBrowserAuthPolicy(t *testing.T) {
	signed := map[string]string{"Authorization": "Bearer glsa_tok"}
	st := &browserState{
		origins: map[string]bool{"https://grafana.example.com": true, "https://docs.example.com": true},
		auth:    map[string]browserAuth{"https://grafana.example.com": {headers: signed}},
		ports:   []int{5173},
	}
	const grafana, docs = "https://grafana.example.com/d/abc", "https://docs.example.com/guide"
	for _, tc := range []struct {
		url, from string
		main      bool
		method    string
		signed    bool
	}{
		{"https://grafana.example.com/d/abc", "about:blank", true, "GET", true},
		{"https://grafana.example.com/d/abc", docs, true, "GET", true},
		{"https://grafana.example.com/api/ds/query", grafana, false, "POST", true},
		{"https://grafana.example.com/public/app.js", grafana, false, "GET", true},
		{"https://grafana.example.com/api/live/ws", "blob:https://grafana.example.com/5f0c", false, "GET", true},
		{"https://grafana.example.com/d/abc", grafana, true, "POST", true},
		// Another site's page, a frame included, does not get to use the sign-in.
		{"https://grafana.example.com/api/user", docs, false, "GET", false},
		{"https://grafana.example.com/api/dashboards", docs, true, "POST", false},
		{"https://grafana.example.com/d/abc", "http://localhost:5173/app", false, "GET", false},
		// Nor does any other origin, Grafana's own requests of it included.
		{"https://docs.example.com/guide", grafana, true, "GET", false},
		{"http://grafana.example.com/d/abc", grafana, true, "GET", false},
	} {
		u, _ := url.Parse(tc.url)
		d := st.decide(browser.Request{URL: u, Method: tc.method, From: tc.from, MainFrame: tc.main, Navigation: tc.main})
		if !d.Allow && !strings.Contains(tc.url, "http://grafana") {
			t.Errorf("%s from %s refused: %s", tc.url, tc.from, d.Reason)
		}
		if got := d.Headers != nil; got != tc.signed {
			t.Errorf("%s from %q (main %v, %s): headers %v, want signed %v", tc.url, tc.from, tc.main, tc.method, d.Headers, tc.signed)
		}
	}
}

// Only an origin the browser may reach is signed in to, once a turn, and a
// failure is reported, not retried, and sends nothing.
func TestBrowserSignIn(t *testing.T) {
	e, _ := fixture(t, "auto", nil)
	e.config.BrowserAuth = map[string]map[string]string{
		"https://grafana.example.com": {"Authorization": "Bearer $(printf %s glsa_tok)"},
		"https://broken.example.com":  {"Authorization": "Bearer $(exit 1)", "X-Org": "1"},
	}
	if note := e.signIn(context.Background()); note != "" {
		t.Fatalf("signed in to origins not approved: %s", note)
	}

	st := e.browserState()
	st.approve("https://grafana.example.com", "https://broken.example.com")
	note := e.signIn(context.Background())
	for _, want := range []string{
		"browser_auth for https://broken.example.com failed, so its requests go without Authorization, X-Org: Authorization: exit status 1",
		"Signed in to https://grafana.example.com: its requests carry Authorization from browser_auth.",
	} {
		if !strings.Contains(note, want) {
			t.Errorf("sign-in note lacks %q:\n%s", want, note)
		}
	}
	if strings.Contains(note, "glsa_tok") {
		t.Fatalf("the note shows the token: %s", note)
	}
	if again := e.signIn(context.Background()); again != "" {
		t.Fatalf("signed in twice: %s", again)
	}

	nav := func(raw string) browser.Decision {
		u, _ := url.Parse(raw)
		return st.decide(browser.Request{URL: u, Method: "GET", From: "about:blank", MainFrame: true, Navigation: true})
	}
	if d := nav("https://grafana.example.com/"); d.Headers["Authorization"] != "Bearer glsa_tok" {
		t.Fatalf("grafana: %+v", d)
	}
	if d := nav("https://broken.example.com/"); !d.Allow || d.Headers != nil {
		t.Fatalf("broken: %+v", d)
	}

	if notice := e.signInNotice("https://broken.example.com", "https://docs.example.com"); notice != " The user's browser_auth configuration signs the browser in to https://broken.example.com (Authorization, X-Org) with those headers." {
		t.Fatalf("notice: %q", notice)
	}
}

// In Chrome: the signed-in origin's navigation and own requests carry the
// headers, replacing the page's own of the same name and keeping the rest;
// another site's request of it and a redirect away from it do not.
func TestBrowserAuthInChrome(t *testing.T) {
	t.Setenv("JULIA_AUTH_TEST", "ada")
	site := &browserSite{hits: map[string]int{}}
	record := func(side string, r *http.Request) {
		site.hit(fmt.Sprintf("%s%s?%s auth=%q page=%q who=%q", side, r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization"), r.Header.Get("X-Page"), r.Header.Get("X-Who")))
	}
	page := func(w io.Writer, title, body string) {
		io.WriteString(w, "<!doctype html><meta charset=utf-8><title>"+title+"</title>"+body)
	}
	site.local = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		record("local", r)
		switch r.URL.Path {
		case "/cross":
			page(w, "Cross", `<p id=r>…</p><script>fetch('http://external.test/api/me?cross', {mode: 'no-cors'}).finally(() => r.textContent = 'cross done')</script>`)
		case "/seen":
			page(w, "Seen", `<h1>Seen</h1>`)
		}
	}))
	site.external = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		record("external", r)
		switch r.URL.Path {
		case "/whoami":
			page(w, "Who", `<p id=r>…</p><script>fetch('/api/me?own', {headers: {'X-Page': 'kept', 'Authorization': 'page-own'}}).then(r => r.text()).then(t => r.textContent = 'me ' + t)</script>`)
		case "/api/me":
			io.WriteString(w, "ok")
		case "/bounce":
			http.Redirect(w, r, site.local.URL+"/seen", http.StatusFound)
		}
	}))
	t.Cleanup(func() {
		site.local.Close()
		site.external.Close()
	})

	e := browserFixture(t, "auto", nil, site)
	e.config.BrowserAuth = map[string]map[string]string{
		"http://external.test": {"Authorization": "Bearer $(printf %s glsa_tok)", "X-Who": "$JULIA_AUTH_TEST"},
	}
	var reasons []string
	e.config.Review = func(_ context.Context, a core.Approval) (core.Decision, error) {
		reasons = append(reasons, a.Reason)
		return core.Decision{Allow: true}, nil
	}

	r := mustBrowse(t, e, map[string]any{"action": "navigate", "url": "http://external.test/whoami", "network": true})
	if !strings.Contains(r.Content, "Signed in to http://external.test: its requests carry Authorization, X-Who from browser_auth.") || strings.Contains(r.Content, "glsa_tok") {
		t.Fatalf("navigate:\n%s", r.Content)
	}
	if len(reasons) != 1 || !strings.Contains(reasons[0], "signs the browser in to http://external.test (Authorization, X-Who)") {
		t.Fatalf("approval reasons: %q", reasons)
	}
	mustBrowse(t, e, map[string]any{"action": "wait", "text": "me ok"})
	mustBrowse(t, e, map[string]any{"action": "navigate", "url": site.local.URL + "/cross"})
	mustBrowse(t, e, map[string]any{"action": "wait", "text": "cross done"})
	r = mustBrowse(t, e, map[string]any{"action": "navigate", "url": "http://external.test/bounce"})
	if strings.Contains(r.Content, "Signed in") {
		t.Fatalf("signed in twice in one turn:\n%s", r.Content)
	}

	for _, want := range []string{
		`external/whoami? auth="Bearer glsa_tok" page="" who="ada"`,
		`external/api/me?own auth="Bearer glsa_tok" page="kept" who="ada"`,
		`external/api/me?cross auth="" page="" who=""`,
		`external/bounce? auth="Bearer glsa_tok" page="" who="ada"`,
		`local/seen? auth="" page="" who=""`,
	} {
		if site.count(want) == 0 {
			site.mu.Lock()
			t.Errorf("no request %s; requests:\n%v", want, site.hits)
			site.mu.Unlock()
		}
	}
}

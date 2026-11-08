package tools

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"julia/agents"
	"julia/internal/core"
)

type webRoundTrip func(*http.Request) (*http.Response, error)

func (f webRoundTrip) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func webResponse(status int, contentType, body string) *http.Response {
	return &http.Response{
		StatusCode: status, Header: http.Header{"Content-Type": {contentType}},
		Body: io.NopCloser(strings.NewReader(body)),
	}
}

type webItem struct{ title, url, description string }

func webResultJSON(items ...webItem) string {
	web := make([]map[string]any, len(items))
	for i, item := range items {
		web[i] = map[string]any{"title": item.title, "url": item.url, "description": item.description, "position": i + 1}
	}
	b, _ := json.Marshal(map[string]any{"success": true, "data": map[string]any{"web": web}, "creditsUsed": 2})
	return string(b)
}

const testSearchKey = "fc-test-key"

func webFixture(t *testing.T, transport webRoundTrip) *Executor {
	t.Helper()
	e, _ := fixture(t, "auto", nil)
	e.config.Review = func(context.Context, core.Approval) (core.Decision, error) {
		return core.Decision{Allow: true, Reason: "public search"}, nil
	}
	e.config.SearchKey = testSearchKey
	e.webClient = newWebSearchClient()
	e.webClient.Transport = transport
	return e
}

func TestWebSearchRegisteredWithoutConfiguration(t *testing.T) {
	e, _ := fixture(t, "auto", nil)
	for _, definitions := range [][]core.ToolDefinition{Definitions(nil, false, false, false), Definitions(agents.Builtin(), false, false, false), e.Definitions(), WithSubagents(e, agents.Builtin(), nil).Definitions()} {
		found := 0
		for _, definition := range definitions {
			if definition.Name == "WebSearch" {
				found++
				required, _ := definition.Parameters["required"].([]string)
				if len(required) != 1 || required[0] != "query" {
					t.Fatalf("unexpected required arguments: %v", required)
				}
			}
		}
		if found != 1 {
			t.Fatalf("WebSearch registered %d times", found)
		}
	}
}

func TestWebSearchRequestAndCompactResults(t *testing.T) {
	calls := 0
	e := webFixture(t, func(req *http.Request) (*http.Response, error) {
		calls++
		if req.Method != http.MethodPost || req.URL.String() != webSearchEndpoint {
			t.Fatalf("unexpected search destination: %s %s", req.Method, req.URL)
		}
		if req.Header.Get("Authorization") != "Bearer "+testSearchKey || req.Header.Get("Content-Type") != "application/json" {
			t.Fatal("missing Firecrawl authentication or content type")
		}
		if req.Header.Get("Cookie") != "" || req.Header.Get("Referer") != "" || !strings.HasPrefix(req.Header.Get("User-Agent"), "JULIA/") {
			t.Fatal("search sent unexpected headers")
		}
		var body map[string]any
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["query"] != "Go net/http & TLS" || body["scrapeOptions"] != nil {
			t.Fatalf("unexpected request body: %v", body)
		}
		var items []webItem
		for i := 1; i <= 10; i++ {
			items = append(items, webItem{fmt.Sprintf("Result %d", i), fmt.Sprintf("https://example.com/%d", i), "A short\nsnippet."})
		}
		return webResponse(200, "application/json; charset=utf-8", webResultJSON(items...)), nil
	})
	var approval core.Approval
	e.config.Task = "Find Go's public HTTP documentation."
	e.config.Review = func(_ context.Context, a core.Approval) (core.Decision, error) {
		approval = a
		return core.Decision{Allow: true}, nil
	}
	r := invoke(e, "WebSearch", map[string]any{"query": "Go net/http & TLS"})
	if r.IsError {
		t.Fatal(r.Content)
	}
	if calls != 1 || !strings.Contains(r.Content, "3. Result 3") || strings.Contains(r.Content, "Result 4") {
		t.Fatalf("unexpected result: calls=%d %s", calls, r.Content)
	}
	if len(r.Content) > maxWebSearchText || !strings.Contains(r.Content, "https://example.com/1") || !strings.Contains(r.Content, "A short snippet.") {
		t.Fatal(r.Content)
	}
	if strings.Contains(r.Content, testSearchKey) {
		t.Fatal("API key leaked into the tool result")
	}
	if approval.Call.Name != "WebSearch" || approval.ExecutionMode != "web-search" || !approval.Network || approval.Task != e.config.Task || approval.Directory != e.directory {
		t.Fatalf("incorrect approval boundary: %+v", approval)
	}
	r = invoke(e, "WebSearch", map[string]any{"query": "Go net/http & TLS", "limit": 5})
	if r.IsError || !strings.Contains(r.Content, "5. Result 5") || strings.Contains(r.Content, "Result 6") {
		t.Fatal(r.Content)
	}
}

func TestWebSearchRequiresAPIKey(t *testing.T) {
	e := webFixture(t, func(*http.Request) (*http.Response, error) {
		t.Fatal("unconfigured search connected")
		return nil, nil
	})
	e.config.SearchKey = ""
	e.config.Review = func(context.Context, core.Approval) (core.Decision, error) {
		t.Fatal("unconfigured search prompted")
		return core.Decision{}, nil
	}
	r := invoke(e, "WebSearch", map[string]any{"query": "Go docs"})
	if !r.IsError || !strings.Contains(r.Content, WebSearchKeyEnv) {
		t.Fatal(r)
	}
}

func TestWebSearchInvalidArgumentsDoNotReviewOrConnect(t *testing.T) {
	e := webFixture(t, func(*http.Request) (*http.Response, error) {
		t.Fatal("invalid request connected")
		return nil, nil
	})
	e.config.Review = func(context.Context, core.Approval) (core.Decision, error) {
		t.Fatal("invalid request prompted")
		return core.Decision{}, nil
	}
	for _, args := range []string{
		`{}`, `{"query":null}`, `{"query":""}`, `{"query":"  "}`, `{"query":42}`,
		`{"query":"q","limit":0}`, `{"query":"q","limit":6}`, `{"query":"q","limit":-1}`,
		`{"query":"q","limit":1.5}`, `{"query":"q","limit":"3"}`, `{"query":"q","url":"https://example.com"}`,
		`{"query":"q\nsecret"}`, `{"query":"q\u001b"}`, `{"query":"q\u202e"}`,
		`{"query":"` + strings.Repeat("x", 513) + `"}`,
		`{"query":"` + strings.Repeat("界", 171) + `"}`,
	} {
		t.Run(args[:min(len(args), 60)], func(t *testing.T) {
			r := e.Execute(context.Background(), core.ToolCall{Name: "WebSearch", Arguments: json.RawMessage(args)})
			if !r.IsError {
				t.Fatalf("accepted %s", args)
			}
		})
	}
}

func TestWebSearchApprovalModes(t *testing.T) {
	for _, tc := range []struct {
		name, mode            string
		review, human         *bool
		wantReview, wantHuman int
		wantNetwork           int
	}{
		{"auto approved", "auto", ptrWebBool(true), nil, 1, 0, 1},
		{"auto denied headless", "auto", ptrWebBool(false), nil, 1, 0, 0},
		{"auto denied returns to the model", "auto", ptrWebBool(false), ptrWebBool(true), 1, 0, 0},
		{"auto missing reviewer human denied", "auto", nil, ptrWebBool(false), 0, 1, 0},
		{"auto missing reviewer headless", "auto", nil, nil, 0, 0, 0},
		{"ask approved", "ask", ptrWebBool(true), ptrWebBool(true), 0, 1, 1},
		{"ask denied", "ask", ptrWebBool(true), ptrWebBool(false), 0, 1, 0},
		{"read only", "read-only", ptrWebBool(true), ptrWebBool(true), 0, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reviews, humans, network := 0, 0, 0
			e := webFixture(t, func(*http.Request) (*http.Response, error) {
				network++
				return webResponse(200, "application/json", webResultJSON()), nil
			})
			e.config.Mode, e.config.Review, e.config.Approve = tc.mode, nil, nil
			if tc.review != nil {
				e.config.Review = func(context.Context, core.Approval) (core.Decision, error) {
					reviews++
					return core.Decision{Allow: *tc.review, Reason: "test"}, nil
				}
			}
			if tc.human != nil {
				e.config.Approve = func(context.Context, core.Approval) (core.Decision, error) {
					humans++
					return core.Decision{Allow: *tc.human, Reason: "test"}, nil
				}
			}
			r := invoke(e, "WebSearch", map[string]any{"query": "Go docs"})
			if reviews != tc.wantReview || humans != tc.wantHuman || network != tc.wantNetwork || r.IsError != (tc.wantNetwork == 0) {
				t.Fatalf("reviews=%d humans=%d network=%d result=%+v", reviews, humans, network, r)
			}
		})
	}
}

func ptrWebBool(b bool) *bool { return &b }

func TestParseWebSearch(t *testing.T) {
	target := "https://pkg.go.dev/net/http?tab=doc&x=1#Client"
	body := webResultJSON(
		webItem{"Go <b>net/http</b> & TLS", target, "  Read the docs.\n APIs."},
		webItem{"duplicate", target, "duplicate snippet"},
		webItem{"", "https://go.dev/", "a\tb"},
		webItem{"unsafe", "javascript:alert(1)", "bad"},
		webItem{"credentials", "https://user:pass@example.com/", "bad"},
	)
	results, err := parseWebSearch([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || results[0].url != target || results[0].title != "Go <b>net/http</b> & TLS" || results[0].snippet != "Read the docs. APIs." {
		t.Fatalf("%+v", results)
	}
	if results[1].title != "https://go.dev/" || results[1].snippet != "a b" {
		t.Fatalf("untitled result: %+v", results[1])
	}
}

func TestWebSearchResultURLs(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"https://example.com/docs#section", "https://example.com/docs#section"},
		{"http://example.com", "http://example.com"},
		{"", ""}, {"/relative", ""}, {"//example.com/docs", ""}, {"javascript:alert(1)", ""}, {"file:///etc/passwd", ""},
		{"https://user:secret@example.com", ""}, {"https:///empty", ""}, {"https://example.com/\n", ""},
		{"https://example.com/\u202e", ""},
		{"https://example.com/" + strings.Repeat("x", 1024), ""},
	} {
		if got := webResultURL(tc.input); got != tc.want {
			t.Errorf("%q: got %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestWebSearchFailuresAreNotEmptyResults(t *testing.T) {
	for _, tc := range []struct {
		name, contentType, body, want string
		status                        int
	}{
		{"unauthorized", "application/json", `{"error":"private details"}`, "rejected the API key", 401},
		{"forbidden", "application/json", `{"error":"private details"}`, "HTTP 403", 403},
		{"out of credits", "application/json", `{"error":"private details"}`, "out of credits", 402},
		{"rate limit", "application/json", `{"error":"private details"}`, "rate limit", 429},
		{"timeout", "application/json", `{"error":"private details"}`, "timed out", 408},
		{"server error", "application/json", `{"error":"private details"}`, "HTTP 500", 500},
		{"html", "text/html", "<html>private page</html>", "content type", 200},
		{"oversized", "application/json", strings.Repeat("x", maxWebSearchBody+1), "exceeded 1 MiB", 200},
		{"malformed", "application/json", `{"success":true,"data":`, "parse", 200},
		{"unsuccessful", "application/json", `{"success":false,"error":"private details"}`, "unsuccessful", 200},
		{"missing data", "application/json", `{"success":true}`, "format may have changed", 200},
		{"only invalid results", "application/json", webResultJSON(webItem{"bad", "javascript:void(0)", "private"}), "no usable result URLs", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			e := webFixture(t, func(*http.Request) (*http.Response, error) {
				calls++
				return webResponse(tc.status, tc.contentType, tc.body), nil
			})
			r := invoke(e, "WebSearch", map[string]any{"query": "Go docs"})
			if !r.IsError || !strings.Contains(r.Content, tc.want) || strings.Contains(r.Content, "private") || strings.Contains(r.Content, "found no results") || calls != 1 {
				t.Fatalf("calls=%d result=%+v", calls, r)
			}
		})
	}
	results, err := parseWebSearch([]byte(webResultJSON()))
	if err != nil || len(results) != 0 || !strings.Contains(formatWebSearch(results, 3), "found no results") {
		t.Fatalf("valid empty response: %+v %v", results, err)
	}
}

func TestWebSearchDoesNotFollowRedirects(t *testing.T) {
	calls := 0
	e := webFixture(t, func(*http.Request) (*http.Response, error) {
		calls++
		response := webResponse(302, "application/json", "")
		response.Header.Set("Location", "http://127.0.0.1/private")
		return response, nil
	})
	r := invoke(e, "WebSearch", map[string]any{"query": "Go docs"})
	if !r.IsError || calls != 1 || !strings.Contains(r.Content, "redirects are not followed") {
		t.Fatalf("calls=%d result=%+v", calls, r)
	}
	client := newWebSearchClient()
	defer client.CloseIdleConnections()
	if client.Jar != nil || client.Timeout <= 0 || client.Timeout > 30*time.Second || client.Transport.(*http.Transport).Proxy != nil {
		t.Fatal("client permits ambient cookies/proxies or unbounded requests")
	}
}

func TestWebSearchCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	e := webFixture(t, func(req *http.Request) (*http.Response, error) {
		cancel()
		<-req.Context().Done()
		return nil, req.Context().Err()
	})
	args := json.RawMessage(`{"query":"not echoed in errors"}`)
	r := e.Execute(ctx, core.ToolCall{Name: "WebSearch", Arguments: args})
	if !r.IsError || !strings.Contains(r.Content, "context canceled") || strings.Contains(r.Content, "not echoed") {
		t.Fatal(r)
	}
	e.config.Review = func(context.Context, core.Approval) (core.Decision, error) {
		t.Fatal("already canceled request was reviewed")
		return core.Decision{}, nil
	}
	r = e.Execute(ctx, core.ToolCall{Name: "WebSearch", Arguments: args})
	if !r.IsError {
		t.Fatal("ignored cancellation")
	}
}

type webBrokenBody struct{}

func (webBrokenBody) Read([]byte) (int, error) { return 0, errors.New("broken body") }
func (webBrokenBody) Close() error             { return nil }

func TestWebSearchBodyReadFailure(t *testing.T) {
	e := webFixture(t, func(*http.Request) (*http.Response, error) {
		response := webResponse(200, "application/json", "")
		response.Body = webBrokenBody{}
		return response, nil
	})
	r := invoke(e, "WebSearch", map[string]any{"query": "Go docs"})
	if !r.IsError || !strings.Contains(r.Content, "broken body") {
		t.Fatal(r)
	}
}

func TestWebSearchOutputBoundAndSanitization(t *testing.T) {
	var items []webItem
	for i := 0; i < 10; i++ {
		items = append(items, webItem{
			strings.Repeat("界", 100) + "\x1b\u202e",
			fmt.Sprintf("https://example.com/%d/%s", i, strings.Repeat("x", 980)),
			strings.Repeat("文", 200) + "\x00\u2066",
		})
	}
	results, err := parseWebSearch([]byte(webResultJSON(items...)))
	if err != nil {
		t.Fatal(err)
	}
	out := formatWebSearch(results, 5)
	if len(out) > maxWebSearchText || !utf8.ValidString(out) || !strings.Contains(out, "omitted") {
		t.Fatalf("invalid compact output (%d bytes): %q", len(out), out)
	}
	for _, result := range results {
		if len(result.title) > 160 || len(result.snippet) > 240 || strings.IndexFunc(result.title+result.snippet, webControl) >= 0 {
			t.Fatalf("unsanitized or unbounded text: %+v", result)
		}
	}
	// Output truncates only between results, never in the middle of a source URL.
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "https://") {
			if _, err := url.ParseRequestURI(line); err != nil || !strings.HasSuffix(line, strings.Repeat("x", 980)) {
				t.Fatalf("truncated source URL: %s", line)
			}
		}
	}
}

// setupFixture has no API key, so WebSearch runs first-use setup against the
// stubbed Firecrawl authorization and search endpoints.
func setupFixture(t *testing.T, poll func(n int, body map[string]string) *http.Response) (*Executor, *[]string, *int) {
	t.Helper()
	interval, opener := webSearchAuthInterval, openBrowser
	t.Cleanup(func() { webSearchAuthInterval, openBrowser = interval, opener })
	webSearchAuthInterval = time.Millisecond
	var opened []string
	openBrowser = func(link string) error { opened = append(opened, link); return nil }
	polls := 0
	e := webFixture(t, func(req *http.Request) (*http.Response, error) {
		switch req.URL.String() {
		case webSearchAuthPollURL:
			polls++
			var body map[string]string
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil || req.Header.Get("Authorization") != "" {
				t.Fatalf("bad poll: %v %v", body, err)
			}
			return poll(polls, body), nil
		case webSearchEndpoint:
			if req.Header.Get("Authorization") != "Bearer fc-authorized123" {
				t.Fatalf("search used key %q", req.Header.Get("Authorization"))
			}
			return webResponse(200, "application/json", webResultJSON(webItem{"Go", "https://go.dev/", "docs"})), nil
		}
		t.Fatalf("unexpected request to %s", req.URL)
		return nil, nil
	})
	e.config.SearchKey = ""
	e.config.SearchKeyFile = filepath.Join(t.TempDir(), "julia", "firecrawl.json")
	e.config.Approve = func(context.Context, core.Approval) (core.Decision, error) {
		return core.Decision{Allow: true}, nil
	}
	return e, &opened, &polls
}

func TestWebSearchFirstUseSetup(t *testing.T) {
	var verifier string
	e, opened, polls := setupFixture(t, func(n int, body map[string]string) *http.Response {
		verifier = body["code_verifier"]
		if n < 3 {
			return webResponse(200, "application/json", `{"status":"pending"}`)
		}
		return webResponse(200, "application/json", `{"status":"complete","apiKey":"fc-authorized123","teamName":"t"}`)
	})
	var setup core.Approval
	e.config.Approve = func(_ context.Context, a core.Approval) (core.Decision, error) {
		if a.ExecutionMode == "firecrawl-setup" {
			setup = a
		}
		return core.Decision{Allow: true}, nil
	}
	e.config.Review = func(_ context.Context, a core.Approval) (core.Decision, error) {
		if a.ExecutionMode != "web-search" {
			t.Fatalf("setup went to the automatic reviewer: %+v", a)
		}
		return core.Decision{Allow: true}, nil
	}
	r := invoke(e, "WebSearch", map[string]any{"query": "Go docs"})
	if r.IsError || !strings.Contains(r.Content, "https://go.dev/") || strings.Contains(r.Content, "fc-authorized123") {
		t.Fatal(r)
	}
	if setup.Call.Name != "WebSearch" || !strings.Contains(setup.Reason, e.config.SearchKeyFile) || *polls != 3 || len(*opened) != 1 {
		t.Fatalf("setup=%+v polls=%d opened=%v", setup, *polls, *opened)
	}
	link, err := url.Parse((*opened)[0])
	if err != nil || link.Host != "www.firecrawl.dev" || link.Path != "/cli-auth" || !strings.HasPrefix(link.Fragment, "session_id=") {
		t.Fatalf("bad authorization link %q", (*opened)[0])
	}
	sum := sha256.Sum256([]byte(verifier))
	if link.Query().Get("code_challenge") != base64.RawURLEncoding.EncodeToString(sum[:]) || strings.Contains((*opened)[0], verifier) {
		t.Fatal("authorization link does not carry only the PKCE challenge")
	}
	info, err := os.Stat(e.config.SearchKeyFile)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("saved key file: %v %v", info, err)
	}
	if key, err := LoadSearchKey(e.config.SearchKeyFile); err != nil || key != "fc-authorized123" {
		t.Fatalf("saved key %q %v", key, err)
	}
	// Later calls, and fresh executors reading the same file, skip setup.
	if r = invoke(e, "WebSearch", map[string]any{"query": "Go docs"}); r.IsError || *polls != 3 {
		t.Fatal(r)
	}
	e.config.SearchKey = ""
	if r = invoke(e, "WebSearch", map[string]any{"query": "Go docs"}); r.IsError || *polls != 3 || len(*opened) != 1 {
		t.Fatal(r)
	}
}

func TestWebSearchSetupDeniedHeadlessOrYolo(t *testing.T) {
	allow := func(context.Context, core.Approval) (core.Decision, error) { return core.Decision{Allow: true}, nil }
	for _, tc := range []struct {
		name, mode string
		approve    core.Approver
	}{
		{"denied", "auto", func(context.Context, core.Approval) (core.Decision, error) { return core.Decision{Reason: "no"}, nil }},
		{"headless", "auto", nil},
		// Yolo grants every action, but nobody is at a browser to authorize.
		{"yolo", "yolo", allow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, opened, polls := setupFixture(t, func(int, map[string]string) *http.Response {
				t.Fatal("polled without consent")
				return nil
			})
			e.config.Mode, e.config.Approve = tc.mode, tc.approve
			r := invoke(e, "WebSearch", map[string]any{"query": "Go docs"})
			if !r.IsError || !strings.Contains(r.Content, WebSearchKeyEnv) || *polls != 0 || len(*opened) != 0 {
				t.Fatal(r)
			}
			if _, err := os.Stat(e.config.SearchKeyFile); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("key file written without setup")
			}
		})
	}
}

func TestWebSearchSetupPollFailures(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		status     int
		body       string
		polls      int
	}{
		{"rejected", "HTTP 400", 400, `{"error":"private"}`, 1},
		{"busy", "busy", 503, ``, 5},
		{"rate limited", "busy", 429, ``, 5},
		{"malformed key", "malformed API key", 200, `{"status":"complete","apiKey":"sk-wrong"}`, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, _, polls := setupFixture(t, func(int, map[string]string) *http.Response {
				return webResponse(tc.status, "application/json", tc.body)
			})
			r := invoke(e, "WebSearch", map[string]any{"query": "Go docs"})
			if !r.IsError || !strings.Contains(r.Content, tc.want) || strings.Contains(r.Content, "private") || *polls != tc.polls {
				t.Fatalf("polls=%d result=%+v", *polls, r)
			}
		})
	}
}

func TestWebSearchSetupCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	e, _, _ := setupFixture(t, func(n int, _ map[string]string) *http.Response {
		if n == 2 {
			cancel()
		}
		return webResponse(200, "application/json", `{"status":"pending"}`)
	})
	r := e.Execute(ctx, core.ToolCall{Name: "WebSearch", Arguments: json.RawMessage(`{"query":"Go docs"}`)})
	if !r.IsError || !strings.Contains(r.Content, "context canceled") {
		t.Fatal(r)
	}
}

func TestWebSearchRevokedSavedKeyIsRemoved(t *testing.T) {
	e := webFixture(t, func(*http.Request) (*http.Response, error) {
		return webResponse(401, "application/json", `{"error":"private"}`), nil
	})
	e.config.SearchKey = ""
	e.config.SearchKeyFile = filepath.Join(t.TempDir(), "firecrawl.json")
	if err := saveSearchKey(e.config.SearchKeyFile, "fc-revoked123"); err != nil {
		t.Fatal(err)
	}
	r := invoke(e, "WebSearch", map[string]any{"query": "Go docs"})
	if !r.IsError || !strings.Contains(r.Content, "set up Firecrawl again") || e.config.SearchKey != "" {
		t.Fatal(r)
	}
	if _, err := os.Stat(e.config.SearchKeyFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("revoked key file kept")
	}
	// An environment key is the user's to fix; it is never discarded.
	e.config.SearchKey, e.envKey = "fc-envkey123", "fc-envkey123"
	if r = invoke(e, "WebSearch", map[string]any{"query": "Go docs"}); !r.IsError || !strings.Contains(r.Content, WebSearchKeyEnv) || e.config.SearchKey == "" {
		t.Fatal(r)
	}
}

func TestLoadSearchKey(t *testing.T) {
	dir := t.TempDir()
	if key, err := LoadSearchKey(filepath.Join(dir, "missing.json")); key != "" || err != nil {
		t.Fatal(key, err)
	}
	bad := filepath.Join(dir, "bad.json")
	os.WriteFile(bad, []byte(`{"api_key":"fc-has spaces"}`), 0600)
	if _, err := LoadSearchKey(bad); err == nil {
		t.Fatal("accepted malformed key")
	}
}

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"jarvis/internal/core"
)

type webRoundTrip func(*http.Request) (*http.Response, error)

func (f webRoundTrip) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func webResponse(status int, contentType, body string) *http.Response {
	return &http.Response{
		StatusCode: status, Header: http.Header{"Content-Type": {contentType}},
		Body: io.NopCloser(strings.NewReader(body)),
	}
}

func webResultHTML(title, href, snippet string) string {
	return fmt.Sprintf(`<div class="result results_links web-result"><h2><a class="result__a" href="%s">%s</a></h2><a class="result__snippet">%s</a></div>`, href, title, snippet)
}

func webFixture(t *testing.T, transport webRoundTrip) *Executor {
	t.Helper()
	e, _ := fixture(t, "auto", nil)
	e.config.Review = func(context.Context, core.Approval) (core.Decision, error) {
		return core.Decision{Allow: true, Reason: "public search"}, nil
	}
	e.webClient = newWebSearchClient()
	e.webClient.Transport = transport
	return e
}

func TestWebSearchRegisteredWithoutConfiguration(t *testing.T) {
	e, _ := fixture(t, "auto", nil)
	for _, definitions := range [][]core.ToolDefinition{Definitions(false), Definitions(true), e.Definitions(), WithSubagents(e, nil).Definitions()} {
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
		if req.Method != http.MethodGet || req.URL.Scheme != "https" || req.URL.Host != "html.duckduckgo.com" || req.URL.Path != "/html/" {
			t.Fatalf("unexpected search destination: %s %s", req.Method, req.URL)
		}
		if req.URL.Query().Get("q") != "Go net/http & TLS" || len(req.URL.Query()) != 1 {
			t.Fatalf("unexpected query: %s", req.URL.RawQuery)
		}
		if req.Body != nil || req.Header.Get("Authorization") != "" || req.Header.Get("Cookie") != "" || req.Header.Get("Referer") != "" {
			t.Fatal("search sent state other than its query")
		}
		if req.Header.Get("Accept") != "text/html" || !strings.HasPrefix(req.Header.Get("User-Agent"), "JARVIS/") {
			t.Fatal("missing request headers")
		}
		var body strings.Builder
		for i := 1; i <= 10; i++ {
			body.WriteString(webResultHTML(fmt.Sprintf("Result %d", i), fmt.Sprintf("https://example.com/%d", i), "A short <b>snippet</b>."))
		}
		return webResponse(200, "text/html; charset=UTF-8", body.String()), nil
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
	if calls != 1 || !strings.Contains(r.Content, "3. Result 3") || strings.Contains(r.Content, "Result 4") || strings.Contains(r.Content, "<b>") {
		t.Fatalf("unexpected result: calls=%d %s", calls, r.Content)
	}
	if len(r.Content) > maxWebSearchText || !strings.Contains(r.Content, "https://example.com/1") || !strings.Contains(r.Content, "A short snippet.") {
		t.Fatal(r.Content)
	}
	if approval.Call.Name != "WebSearch" || approval.ExecutionMode != "web-search" || !approval.Network || approval.Task != e.config.Task || approval.Directory != e.directory {
		t.Fatalf("incorrect approval boundary: %+v", approval)
	}
	r = invoke(e, "WebSearch", map[string]any{"query": "Go net/http & TLS", "limit": 5})
	if r.IsError || !strings.Contains(r.Content, "5. Result 5") || strings.Contains(r.Content, "Result 6") {
		t.Fatal(r.Content)
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
		{"auto denied human approved", "auto", ptrWebBool(false), ptrWebBool(true), 1, 1, 1},
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
				return webResponse(200, "text/html", `<div class="no-results">No results</div>`), nil
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
	wrapped := "//duckduckgo.com/l/?uddg=" + url.QueryEscape(target) + "&amp;rut=tracking"
	body := `<html><body><nav>Navigation must stay out of results</nav>` +
		webResultHTML("Go <b>net/http</b> &amp; TLS", wrapped, "  Read <b>the docs</b>. <script>hidden()</script>\n APIs.") +
		webResultHTML("duplicate", target, "duplicate snippet") +
		`<div class="result result--ad"><a class="result__a" href="https://ads.example/">Advertisement</a></div>` +
		webResultHTML("second", "https://go.dev/", "a<br>b") +
		webResultHTML("unsafe", "javascript:alert(1)", "bad") +
		webResultHTML("credentials", "https://user:pass@example.com/", "bad") +
		`</body></html>`
	results, err := parseWebSearch([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || results[0].url != target || results[0].title != "Go net/http & TLS" || results[0].snippet != "Read the docs. APIs." || results[1].snippet != "a b" {
		t.Fatalf("%+v", results)
	}
}

func TestWebSearchResultURLs(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"https://example.com/docs#section", "https://example.com/docs#section"},
		{"//example.com/docs", "https://example.com/docs"},
		{"/l/?uddg=https%3A%2F%2Fexample.com%2Fdocs", "https://example.com/docs"},
		{"https://duckduckgo.com/l/?uddg=http%3A%2F%2Fexample.com", "http://example.com"},
		{"https://evil.example/l/?uddg=https%3A%2F%2Fexample.com", "https://evil.example/l/?uddg=https%3A%2F%2Fexample.com"},
		{"", ""}, {"/relative", ""}, {"javascript:alert(1)", ""}, {"file:///etc/passwd", ""},
		{"https://user:secret@example.com", ""}, {"https:///empty", ""}, {"https://example.com/\n", ""},
		{"https://example.com/\u202e", ""}, {"/l/?uddg=javascript%3Aalert%281%29", ""},
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
		{"challenge status", "text/html", "private challenge page", "bot challenge", 202},
		{"forbidden", "text/html", "private challenge page", "bot challenge", 403},
		{"rate limit", "text/html", "private rate limit page", "rate limit", 429},
		{"server error", "text/html", "private server details", "HTTP 503", 503},
		{"json", "application/json", `{"error":"private details"}`, "content type", 200},
		{"oversized", "text/html", strings.Repeat("x", maxWebSearchBody+1), "exceeded 1 MiB", 200},
		{"challenge html", "text/html", `<form id="challenge-form">private challenge</form>`, "bot challenge", 200},
		{"challenge despite results", "text/html", `<div class="anomaly-modal"></div>` + webResultHTML("result", "https://example.com", "snippet"), "bot challenge", 200},
		{"unknown markup", "text/html", "<html>private unknown page</html>", "page format", 200},
		{"only invalid results", "text/html", webResultHTML("bad", "javascript:void(0)", "private"), "page format", 200},
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
	results, err := parseWebSearch([]byte(`<div class="result result--no-result"><div class="no-results"><div class="no-results__message">No results found</div></div></div>`))
	if err != nil || len(results) != 0 || !strings.Contains(formatWebSearch(results, 3), "found no results") {
		t.Fatalf("valid empty page: %+v %v", results, err)
	}
}

func TestWebSearchDoesNotFollowRedirects(t *testing.T) {
	calls := 0
	e := webFixture(t, func(*http.Request) (*http.Response, error) {
		calls++
		response := webResponse(302, "text/html", "")
		response.Header.Set("Location", "http://127.0.0.1/private")
		return response, nil
	})
	r := invoke(e, "WebSearch", map[string]any{"query": "Go docs"})
	if !r.IsError || calls != 1 || !strings.Contains(r.Content, "redirects are not followed") {
		t.Fatalf("calls=%d result=%+v", calls, r)
	}
	client := newWebSearchClient()
	defer client.CloseIdleConnections()
	if client.Jar != nil || client.Timeout <= 0 || client.Timeout > 15*time.Second || client.Transport.(*http.Transport).Proxy != nil {
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
		response := webResponse(200, "text/html", "")
		response.Body = webBrokenBody{}
		return response, nil
	})
	r := invoke(e, "WebSearch", map[string]any{"query": "Go docs"})
	if !r.IsError || !strings.Contains(r.Content, "broken body") {
		t.Fatal(r)
	}
}

func TestWebSearchOutputBoundAndSanitization(t *testing.T) {
	var body strings.Builder
	for i := 0; i < 10; i++ {
		body.WriteString(webResultHTML(
			strings.Repeat("界", 100)+"\x1b\u202e",
			fmt.Sprintf("https://example.com/%d/%s", i, strings.Repeat("x", 980)),
			strings.Repeat("文", 200)+"\x00\u2066",
		))
	}
	results, err := parseWebSearch([]byte(body.String()))
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

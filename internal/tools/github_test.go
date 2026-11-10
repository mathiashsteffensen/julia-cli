package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

	"julia/internal/core"
)

const githubFixtureToken = "gho_fixture_0123456789"

func githubFixture(t *testing.T, mode string, handler func(*http.Request) (*http.Response, error)) *Executor {
	t.Helper()
	e, _ := fixture(t, mode, nil)
	e.config.GitHub = GitHubAuth{token: githubFixtureToken}
	e.config.Task = "fixture task"
	e.webClient = newWebSearchClient()
	e.webClient.Transport = webRoundTrip(handler)
	return e
}

func githubCall(e *Executor, args map[string]any) (string, error) {
	data, _ := json.Marshal(args)
	return e.github(context.Background(), core.ToolCall{ID: "call-1", Name: "GitHub", Arguments: data})
}

func TestGitHubMethodsAndApprovalModes(t *testing.T) {
	for _, mode := range []string{"auto", "ask", "yolo", "read-only"} {
		for _, method := range []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE"} {
			t.Run(mode+"/"+method, func(t *testing.T) {
				requests, reviews, humans := 0, 0, 0
				e := githubFixture(t, mode, func(req *http.Request) (*http.Response, error) {
					requests++
					if req.Method != method || req.URL.String() != githubOrigin+"/repos/a/b?state=all" || req.Header.Get("Authorization") != "Bearer "+githubFixtureToken || req.Header.Get("X-GitHub-Api-Version") != githubAPIVersion {
						t.Errorf("unexpected request: %s %s headers %v", req.Method, req.URL, req.Header)
					}
					if method != "GET" && method != "HEAD" {
						body, _ := io.ReadAll(req.Body)
						if string(body) != `{"title":"fixture"}` || req.Header.Get("Content-Type") != "application/json" {
							t.Errorf("body %s headers %v", body, req.Header)
						}
					}
					return webResponse(200, "application/json", `{"merged":false}`), nil
				})
				checkApproval := func(a core.Approval) {
					if a.ExecutionMode != "github" || !a.Network || a.Task != "fixture task" || a.Call.ID != "call-1" || !strings.Contains(a.Reason, "authenticated account") {
						t.Errorf("approval %+v", a)
					}
					var args githubArgs
					if json.Unmarshal(a.Call.Arguments, &args) != nil || args.Method != method || args.Path != "/repos/a/b?state=all" || args.Accept != "json" || args.Body == nil || *args.Body != `{"title":"fixture"}` {
						t.Errorf("approval args %s", a.Call.Arguments)
					}
				}
				e.config.Review = func(_ context.Context, a core.Approval) (core.Decision, error) {
					reviews++
					checkApproval(a)
					return core.Decision{Allow: true}, nil
				}
				e.config.Approve = func(_ context.Context, a core.Approval) (core.Decision, error) {
					humans++
					checkApproval(a)
					return core.Decision{Allow: true}, nil
				}
				args := map[string]any{"method": method, "path": "/repos/a/b?state=all"}
				read := method == "GET" || method == "HEAD"
				if !read {
					args["body"] = `{"title":"fixture"}`
				}
				out, err := githubCall(e, args)
				if mode == "read-only" && !read {
					if err == nil || !strings.Contains(err.Error(), "read-only") || requests != 0 || reviews != 0 || humans != 0 {
						t.Fatalf("read-only write: %q %v requests=%d reviews=%d humans=%d", out, err, requests, reviews, humans)
					}
					return
				}
				if err != nil || !strings.Contains(out, `"merged":false`) || requests != 1 {
					t.Fatalf("request: %q %v requests=%d", out, err, requests)
				}
				wantReview := !read && mode == "auto"
				wantHuman := !read && mode == "ask"
				if (reviews == 1) != wantReview || (humans == 1) != wantHuman {
					t.Fatalf("reviews=%d humans=%d", reviews, humans)
				}
			})
		}
	}
}

func TestGitHubDenialsBeforeCredentialAndNetwork(t *testing.T) {
	for _, mode := range []string{"auto", "ask", "read-only"} {
		t.Run(mode, func(t *testing.T) {
			requests := 0
			e := githubFixture(t, mode, func(*http.Request) (*http.Response, error) {
				requests++
				return nil, errors.New("unexpected request")
			})
			// An unconfigured credential proves the policy is checked before resolution.
			e.config.GitHub = GitHubAuth{}
			e.config.Review = func(context.Context, core.Approval) (core.Decision, error) {
				return core.Decision{Allow: false, Reason: "fixture denial"}, nil
			}
			e.config.Approve = func(context.Context, core.Approval) (core.Decision, error) {
				return core.Decision{Allow: false, Reason: "fixture denial"}, nil
			}
			_, err := githubCall(e, map[string]any{"method": "POST", "path": "/repos/a/b/issues", "body": `{"title":"x"}`})
			if err == nil || requests != 0 || strings.Contains(err.Error(), "credential") {
				t.Fatalf("denial: %v requests=%d", err, requests)
			}
		})
	}
}

func TestGitHubInvalidArgumentsAndGraphQL(t *testing.T) {
	requests, reviews := 0, 0
	e := githubFixture(t, "auto", func(req *http.Request) (*http.Response, error) {
		requests++
		return webResponse(200, "application/json", "{}"), nil
	})
	e.config.Review = func(context.Context, core.Approval) (core.Decision, error) {
		reviews++
		return core.Decision{Allow: true}, nil
	}
	for _, args := range []map[string]any{
		{"path": "/x", "offset": -1}, {"path": "/x", "offset": githubMaxResponse},
		{"path": "/x", "method": "POST", "offset": 1}, {"path": "/x", "offset": nil},
		{}, {"method": "get", "path": "/x"}, {"method": "OPTIONS", "path": "/x"},
		{"path": "https://api.github.com/x"}, {"path": "//evil.test/x"}, {"path": "/x#fragment"},
		{"path": "/a/../x"}, {"path": "/a/%2e%2e/x"}, {"path": "/a/%252e%252e/x"},
		{"path": "/a/%2fx"}, {"path": "/a/%5cx"}, {"path": "/a\\x"},
		{"path": "/a/%00"}, {"path": "/a/%ff"}, {"path": "/a?q=%0a"}, {"path": "/a?q=%ff"}, {"path": "/a//b"},
		{"path": "/x", "body": "{}"}, {"method": "HEAD", "path": "/x", "body": ""},
		{"method": "POST", "path": "/x", "body": "null"}, {"method": "POST", "path": "/x", "body": "\"x\""},
		{"method": "POST", "path": "/x", "body": "{} {}"}, {"method": "POST", "path": "/x", "body": nil}, {"path": "/x", "method": nil}, {"path": "/x", "accept": "html"},
		{"path": "/x", "headers": "foo"}, {"path": "/%67raphql"}, {"method": "HEAD", "path": "/GraphQL/"},
	} {
		if _, err := githubCall(e, args); err == nil {
			t.Errorf("accepted %v", args)
		}
	}
	if requests != 0 || reviews != 0 {
		t.Fatalf("invalid calls reached network or review: %d %d", requests, reviews)
	}
	if _, err := githubCall(e, map[string]any{"method": "POST", "path": "/%67raphql", "body": `{"query":"mutation { x }"}`}); err != nil || requests != 1 || reviews != 1 {
		t.Fatalf("GraphQL not reviewed: %v requests=%d reviews=%d", err, requests, reviews)
	}
}

func TestGitHubGetFollowsSignedRedirectWithoutSensitiveHeaders(t *testing.T) {
	requests := 0
	e := githubFixture(t, "read-only", func(req *http.Request) (*http.Response, error) {
		requests++
		switch requests {
		case 1:
			if req.URL.String() != githubOrigin+"/repos/a/b/actions/jobs/123/logs" {
				t.Errorf("unexpected API request: %s", req.URL)
			}
			resp := webResponse(http.StatusFound, "text/plain", "")
			resp.Header.Set("Location", "https://blob.example.test/signed/workflow.log?signature=fixture")
			return resp, nil
		case 2:
			if req.Method != http.MethodGet || req.URL.Host != "blob.example.test" {
				t.Errorf("unexpected redirect request: %s %s", req.Method, req.URL)
			}
			for _, name := range []string{"Authorization", "Referer"} {
				if value := req.Header.Get(name); value != "" {
					t.Errorf("%s forwarded to signed URL: %q", name, value)
				}
			}
			return webResponse(http.StatusOK, "text/plain", "workflow log"), nil
		default:
			return nil, errors.New("unexpected extra request")
		}
	})

	out, err := githubCall(e, map[string]any{"path": "/repos/a/b/actions/jobs/123/logs", "accept": "raw"})
	if err != nil || requests != 2 || !strings.Contains(out, "workflow log") {
		t.Fatalf("out=%q err=%v requests=%d", out, err, requests)
	}
}

func TestGitHubSignedRedirectErrorIdentifiesDownloadHost(t *testing.T) {
	requests := 0
	e := githubFixture(t, "read-only", func(req *http.Request) (*http.Response, error) {
		requests++
		if requests == 1 {
			resp := webResponse(http.StatusFound, "text/plain", "")
			resp.Header.Set("Location", "https://blob.example.test/signed/workflow.log?signature=expired")
			return resp, nil
		}
		return webResponse(http.StatusForbidden, "text/plain", "signature expired"), nil
	})

	_, err := githubCall(e, map[string]any{"path": "/repos/a/b/actions/jobs/123/logs", "accept": "raw"})
	if err == nil || !strings.Contains(err.Error(), "Download host blob.example.test HTTP 403") || !strings.Contains(err.Error(), "fresh signed URL") || strings.Contains(err.Error(), "token permissions") {
		t.Fatalf("err=%v requests=%d", err, requests)
	}
}

func TestGitHubRedirectConfinement(t *testing.T) {
	for _, tc := range []struct {
		name, method, location string
		wantRequests           int
	}{
		{"same origin", "GET", "https://api.github.com/repos/new", 2},
		{"relative", "HEAD", "/repos/new", 2},
		{"cross origin GET", "GET", "https://download.test/repos/new", 2},
		{"cross origin HEAD", "HEAD", "https://download.test/repos/new", 1},
		{"cross scheme", "GET", "http://api.github.com/repos/new", 1},
		{"userinfo", "GET", "https://user@api.github.com/repos/new", 1},
		{"graphql", "GET", "/%67raphql", 1},
		{"traversal", "GET", "/repos/%2e%2e/new", 1},
		{"mutation", "POST", "/repos/new", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			e := githubFixture(t, "yolo", func(req *http.Request) (*http.Response, error) {
				requests++
				authorization := req.Header.Get("Authorization")
				switch {
				case req.URL.Host == "api.github.com" && authorization != "Bearer "+githubFixtureToken:
					t.Errorf("missing API credential: %s auth=%q", req.URL, authorization)
				case req.URL.Host != "api.github.com" && (tc.method != http.MethodGet || authorization != ""):
					t.Errorf("unsafe redirect: %s auth=%q", req.URL, authorization)
				}
				if requests == 1 {
					resp := webResponse(302, "text/plain", "moved")
					resp.Header.Set("Location", tc.location)
					return resp, nil
				}
				return webResponse(200, "application/json", "{}"), nil
			})
			_, err := githubCall(e, map[string]any{"method": tc.method, "path": "/repos/old"})
			if requests != tc.wantRequests || (err == nil) != (tc.wantRequests == 2) {
				t.Fatalf("requests=%d err=%v", requests, err)
			}
			if err == nil {
				return
			}
			// The model learns why it stopped, but not a target that can be a signed URL.
			if !strings.Contains(err.Error(), "redirect not followed") || strings.Contains(err.Error(), tc.location) {
				t.Fatalf("redirect result: %v", err)
			}
		})
	}
	t.Run("hop limit", func(t *testing.T) {
		requests := 0
		e := githubFixture(t, "yolo", func(*http.Request) (*http.Response, error) {
			requests++
			resp := webResponse(302, "text/plain", "moved")
			resp.Header.Set("Location", "/repos/new")
			return resp, nil
		})
		_, err := githubCall(e, map[string]any{"path": "/repos/old"})
		if err == nil || requests != 5 {
			t.Fatalf("requests=%d err=%v", requests, err)
		}
	})
}

func TestGitHubResponseHandling(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		body    string
		headers http.Header
		accept  string
		want    []string
		fail    bool
	}{
		{"unauthorized", 401, githubFixtureToken, nil, "", []string{"authentication failed", "[redacted]"}, true},
		{"escaped token", 200, `{"token":"\u0067ho_fixture_0123456789"}`, nil, "", []string{"[redacted]"}, false},
		{"header boundary", 200, "ok", http.Header{"Retry-After": []string{strings.Repeat("x", 190) + githubFixtureToken}}, "", []string{"[redacted]"}, false},
		{"forbidden", 403, "denied", nil, "", []string{"forbidden", "permissions"}, true},
		{"not found", 404, "missing", nil, "", []string{"not found or not accessible"}, true},
		{"rate", 403, "slow down", http.Header{"X-Ratelimit-Remaining": []string{"0"}, "X-Ratelimit-Reset": []string{"123"}}, "", []string{"rate limited", "123"}, true},
		{"too many", 429, "slow down", http.Header{"Retry-After": []string{"10"}}, "", []string{"rate limited", "Retry-After: 10"}, true},
		{"secondary limit", 403, `{"message":"secondary rate limit"}`, nil, "", []string{"rate limited"}, true},
		{"empty", 204, "", nil, "", []string{"204", "No content"}, false},
		{"raw", 200, "hello", nil, "raw", []string{"hello"}, false},
		{"diff", 200, "+change", nil, "diff", []string{"+change"}, false},
		{"binary", 200, "\x00\xff", nil, "raw", []string{"binary"}, true},
		{"terminal escape", 200, "\x1b[31m", nil, "raw", []string{"�[31m", "replaced with U+FFFD"}, false},
		{"control in JSON", 200, "\x1b[31m", nil, "", []string{"binary"}, true},
		{"large", 200, strings.Repeat("x", githubMaxResponse+1), nil, "raw", []string{"4 MiB", "output truncated"}, false},
		{"pagination", 200, "[]", http.Header{"Link": []string{`<https://evil.test/?page=2>; rel="next", <https://api.github.com/repos/a/issues?page=2>; rel="next"`}}, "", []string{"Next page path (not fetched): /repos/a/issues?page=2"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := githubFixture(t, "read-only", func(req *http.Request) (*http.Response, error) {
				media := map[string]string{"": "application/vnd.github+json", "raw": "application/vnd.github.raw+json", "diff": "application/vnd.github.diff"}
				if req.Header.Get("Accept") != media[tc.accept] || req.Header.Get("User-Agent") != "JULIA" {
					t.Errorf("headers %v", req.Header)
				}
				resp := webResponse(tc.status, "application/json", tc.body)
				for k, v := range tc.headers {
					resp.Header[k] = v
				}
				return resp, nil
			})
			args := map[string]any{"path": "/repos/a/issues"}
			if tc.accept != "" {
				args["accept"] = tc.accept
			}
			out, err := githubCall(e, args)
			if (err != nil) != tc.fail {
				t.Fatalf("out=%q err=%v", out, err)
			}
			if err != nil {
				out = err.Error()
			}
			for _, s := range tc.want {
				if !strings.Contains(out, s) {
					t.Errorf("missing %q in %q", s, out)
				}
			}
			if len(out) > githubMaxOutput || strings.Contains(out, githubFixtureToken) {
				t.Fatal("output too large or leaked token")
			}
		})
	}
}

// githubBody calls the tool and returns the response body it shows, without the
// status and notes above it.
func githubBody(t *testing.T, body string, args map[string]any) string {
	t.Helper()
	e := githubFixture(t, "read-only", func(*http.Request) (*http.Response, error) {
		return webResponse(http.StatusOK, "application/json", body), nil
	})
	out, err := githubCall(e, args)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	_, content, _ := strings.Cut(out, "\n\n")
	return content
}

func TestGitHubJSONStaysAsGitHubSentIt(t *testing.T) {
	file := "{\n  \"zeta\": \"a<b&c\",\n  \"alpha\": 1.0\n}\n"
	if got := githubBody(t, file, map[string]any{"path": "/repos/a/b/contents/package.json", "accept": "raw"}); got != file {
		t.Errorf("raw file changed:\n got %q\nwant %q", got, file)
	}

	issue := `{"body":"<details>a & b</details>","html_url":"https://github.com/x?a=1&b=2"}`
	if got := githubBody(t, issue, map[string]any{"path": "/repos/a/b/issues/1"}); got != issue {
		t.Errorf("JSON response changed:\n got %q\nwant %q", got, issue)
	}
}

func TestGitHubRedactsATokenWithoutEscapingTheRest(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"plain token keeps the bytes", `{ "z": "` + githubFixtureToken + `", "a": "<b>" }`, `{ "z": "[redacted]", "a": "<b>" }`},
		{"escaped token re-encodes", `{"z":"\u0067ho_fixture_0123456789","a":"<b>&"}`, `{"a":"<b>&","z":"[redacted]"}`},
		{"escaped token in a key", `{"\u0067ho_fixture_0123456789":1}`, `{"[redacted]":1}`},
		{"escaped token in an array", `["ok",["\u0067ho_fixture_0123456789"]]`, `["ok",["[redacted]"]]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, accept := range []string{"json", "raw"} {
				if got := githubBody(t, tc.body, map[string]any{"path": "/x", "accept": accept}); got != tc.want {
					t.Errorf("accept=%s:\n got %q\nwant %q", accept, got, tc.want)
				}
			}
		})
	}
}

func TestGitHubOversizedBodies(t *testing.T) {
	// A Markdown file can start with a badge link and still not be JSON.
	markdown := "[![Build](https://x)](y)\n" + strings.Repeat("plain text line\n", githubMaxResponse/16+10)

	e := githubFixture(t, "read-only", func(*http.Request) (*http.Response, error) {
		return webResponse(http.StatusOK, "application/json", markdown), nil
	})
	out, err := githubCall(e, map[string]any{"path": "/repos/a/b/contents/README.md", "accept": "raw"})
	if err != nil || !strings.Contains(out, "exceeds 4 MiB") || !strings.Contains(out, "[![Build]") || strings.Contains(out, "omitted") {
		t.Fatalf("raw text was not truncated: %.200q, %v", out, err)
	}

	out, err = githubCall(e, map[string]any{"path": "/repos/a/b/issues"})
	if err != nil || !strings.Contains(out, "[oversized JSON body omitted]") || strings.Contains(out, "[![Build]") {
		t.Fatalf("partial JSON was shown: %.200q, %v", out, err)
	}
}

// Text that grows past the limit must not lose its tail without saying so.
func TestGitHubReportsTextThatGrowsPastTheLimit(t *testing.T) {
	for _, tc := range []struct{ name, accept, body string }{
		{"replaced characters", "raw", strings.Repeat("\a", 2<<20)},
		{"re-encoded token", "json", `{"a":"\u0067ho_fixture_0123456789","b":"` + strings.Repeat("\u2028", 1200<<10) + `"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if len(tc.body) > githubMaxResponse {
				t.Fatalf("fixture must start under the limit: %d bytes", len(tc.body))
			}
			e := githubFixture(t, "read-only", func(*http.Request) (*http.Response, error) {
				return webResponse(http.StatusOK, "application/json", tc.body), nil
			})
			out, err := githubCall(e, map[string]any{"path": "/x", "accept": tc.accept})
			if err != nil || !strings.Contains(out, "exceeds 4 MiB") || strings.Contains(out, githubFixtureToken) {
				t.Fatalf("tail lost without notice: %.200q, %v", out, err)
			}
		})
	}
}

func TestGitHubTextKeepsContentAroundUnsafeBytes(t *testing.T) {
	for _, tc := range []struct{ name, accept, body, want string }{
		{"form feed in source", "raw", "package a\n\f\nfunc A() {}\n", "package a\n\ufffd\nfunc A() {}\n"},
		{"form feed in diff", "diff", "+\f\n-x\n", "+\ufffd\n-x\n"},
		{"Latin-1 in diff", "diff", "+caf\xe9\n", "+caf\ufffd\n"},
		{"terminal escape", "raw", "a\x1b]52;c;evil\x07b", "a\ufffd]52;c;evil\ufffdb"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := githubFixture(t, "read-only", func(*http.Request) (*http.Response, error) {
				return webResponse(http.StatusOK, "text/plain", tc.body), nil
			})
			out, err := githubCall(e, map[string]any{"path": "/x", "accept": tc.accept})
			if err != nil || !strings.Contains(out, "replaced with U+FFFD") {
				t.Fatalf("out=%q err=%v", out, err)
			}
			if _, content, _ := strings.Cut(out, "\n\n"); content != tc.want {
				t.Fatalf("content:\n got %q\nwant %q", content, tc.want)
			}
		})
	}

	// Clean text carries no note.
	e := githubFixture(t, "read-only", func(*http.Request) (*http.Response, error) {
		return webResponse(http.StatusOK, "text/plain", "line\r\n\tcaf\u00e9 \ufffd\n"), nil
	})
	if out, err := githubCall(e, map[string]any{"path": "/x", "accept": "raw"}); err != nil || strings.Contains(out, "replaced") {
		t.Fatalf("clean text: %q, %v", out, err)
	}

	// NUL marks a binary file in every representation.
	e = githubFixture(t, "read-only", func(*http.Request) (*http.Response, error) {
		return webResponse(http.StatusOK, "text/plain", "text\x00more"), nil
	})
	for _, accept := range []string{"json", "raw", "diff"} {
		if _, err := githubCall(e, map[string]any{"path": "/x", "accept": accept}); err == nil || !strings.Contains(err.Error(), "binary") {
			t.Errorf("accept=%s accepted NUL: %v", accept, err)
		}
	}
}

func TestGitHubQueryIsEncodedOnTheRequestLine(t *testing.T) {
	for path, want := range map[string]string{
		"/search/issues?q=repo:a/b is:open":        "/search/issues?q=repo:a/b%20is:open",
		"/search/issues?q=s\u00f8ren":              "/search/issues?q=s%C3%B8ren",
		`/search/issues?q=label:"good first"`:      "/search/issues?q=label:%22good%20first%22",
		"/repos/a/b/issues?labels=bug,help&page=2": "/repos/a/b/issues?labels=bug,help&page=2",
		"/search/issues?q=a%20b+c%3Ad":             "/search/issues?q=a%20b+c%3Ad",
		"/repos/a/b/contents/my file.txt?ref=x/y":  "/repos/a/b/contents/my%20file.txt?ref=x/y",
	} {
		t.Run(path, func(t *testing.T) {
			var requestLine, reviewedPath string
			e := githubFixture(t, "auto", func(req *http.Request) (*http.Response, error) {
				var wire strings.Builder
				if err := req.Write(&wire); err != nil {
					t.Fatal(err)
				}
				requestLine, _, _ = strings.Cut(wire.String(), "\r\n")
				return webResponse(http.StatusOK, "application/json", "{}"), nil
			})
			e.config.Review = func(_ context.Context, a core.Approval) (core.Decision, error) {
				var args githubArgs
				if err := json.Unmarshal(a.Call.Arguments, &args); err != nil {
					t.Fatal(err)
				}
				reviewedPath = args.Path
				return core.Decision{Allow: true}, nil
			}

			if _, err := githubCall(e, map[string]any{"path": path}); err != nil {
				t.Fatal(err)
			}
			if wantLine := "GET " + want + " HTTP/1.1"; requestLine != wantLine {
				t.Fatalf("request line:\n got %q\nwant %q", requestLine, wantLine)
			}

			// The reviewer judges the path that is sent.
			if _, err := githubCall(e, map[string]any{"method": "POST", "path": path}); err != nil {
				t.Fatal(err)
			}
			if reviewedPath != want {
				t.Fatalf("reviewed path %q, want %q", reviewedPath, want)
			}
		})
	}
}

func TestGitHubCredentialErrorDoesNotReachNetwork(t *testing.T) {
	requests := 0
	e := githubFixture(t, "read-only", func(*http.Request) (*http.Response, error) {
		requests++
		return nil, errors.New("unexpected request")
	})
	e.config.GitHub = GitHubAuth{}
	_, err := githubCall(e, map[string]any{"path": "/user"})
	if err == nil || !strings.Contains(err.Error(), "credential") || requests != 0 {
		t.Fatalf("credential: %v requests=%d", err, requests)
	}
}

func TestGitHubTransportErrorDoesNotLeakTokenOrRetry(t *testing.T) {
	requests := 0
	e := githubFixture(t, "yolo", func(*http.Request) (*http.Response, error) {
		requests++
		return nil, errors.New("credential " + githubFixtureToken)
	})
	_, err := githubCall(e, map[string]any{"method": "POST", "path": "/repos/a/b"})
	if err == nil || strings.Contains(err.Error(), githubFixtureToken) || requests != 1 {
		t.Fatalf("transport: %v requests=%d", err, requests)
	}
}

func TestGitHubReadContinuation(t *testing.T) {
	body := strings.Repeat("😀x", 15000)
	e := githubFixture(t, "read-only", func(*http.Request) (*http.Response, error) {
		return webResponse(http.StatusOK, "text/plain", body), nil
	})
	var reconstructed strings.Builder
	offset := 0
	for page := 0; page < 4; page++ {
		out, err := githubCall(e, map[string]any{"path": "/repos/a/b/contents/file", "accept": "raw", "offset": offset})
		if err != nil || len(out) > githubMaxOutput || !utf8.ValidString(out) {
			t.Fatalf("page %d: bytes=%d, error=%v", page, len(out), err)
		}
		_, content, _ := strings.Cut(out, "\n\n")
		chunk, continuation, more := strings.Cut(content, "\n[output truncated; repeat this GET with offset=")
		reconstructed.WriteString(chunk)
		if !more {
			break
		}
		var next int
		if _, err := fmt.Sscanf(continuation, "%d", &next); err != nil || next != offset+len(chunk) {
			t.Fatalf("invalid continuation %q after offset %d", continuation, offset)
		}
		offset = next
	}
	if reconstructed.String() != body {
		t.Fatalf("continuation lost content: got %d bytes, want %d", reconstructed.Len(), len(body))
	}
	if _, err := githubCall(e, map[string]any{"path": "/x", "offset": 1}); err == nil || !strings.Contains(err.Error(), "UTF-8") {
		t.Fatalf("accepted an offset inside a rune: %v", err)
	}
	out, err := githubCall(e, map[string]any{"path": "/x", "offset": len(body) + 1})
	if err != nil || !strings.Contains(out, "offset beyond") {
		t.Fatalf("offset beyond body: %q, %v", out, err)
	}
}

func TestGitHubTruncatedMutationDoesNotSuggestRepeating(t *testing.T) {
	e := githubFixture(t, "yolo", func(*http.Request) (*http.Response, error) {
		return webResponse(http.StatusOK, "text/plain", strings.Repeat("x", githubMaxOutput)), nil
	})
	out, err := githubCall(e, map[string]any{"method": "POST", "path": "/graphql"})
	if err != nil || !strings.Contains(out, "use GET") || strings.Contains(out, "offset=") || len(out) > githubMaxOutput {
		t.Fatalf("truncated mutation: bytes=%d, error=%v", len(out), err)
	}
}

func TestGitHubRedactsAcrossResponseLimit(t *testing.T) {
	prefix := strings.Repeat("x", githubMaxResponse-len(githubFixtureToken)/2)
	e := githubFixture(t, "read-only", func(*http.Request) (*http.Response, error) {
		return webResponse(http.StatusOK, "text/plain", prefix+githubFixtureToken+"tail"), nil
	})
	out, err := githubCall(e, map[string]any{"path": "/x", "offset": len(prefix) - 10})
	if err != nil || !strings.Contains(out, "[redacted]") || strings.Contains(out, "gho_fixture") {
		t.Fatalf("credential at response boundary: %q, %v", out, err)
	}
}

func TestGitHubRegistryAndExecutorAgree(t *testing.T) {
	t.Setenv(SlackTokenEnv, "")
	e := githubFixture(t, "read-only", func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/user" {
			t.Fatalf("unexpected API path: %s", req.URL)
		}
		return webResponse(http.StatusOK, "application/json", "{\"login\":\"fixture\"}"), nil
	})
	actual, _ := json.Marshal(e.Definitions())
	registered, _ := json.Marshal(Definitions(nil, false, false, true, false))
	if string(actual) != string(registered) {
		t.Fatal("GitHub display registry differs from executor")
	}
	if result := invoke(e, "GitHub", map[string]any{"path": "/user"}); result.IsError || !strings.Contains(result.Content, "fixture") {
		t.Fatalf("registered execution: %+v", result)
	}
	e.config.GitHub = GitHubAuth{}
	for _, definition := range e.Definitions() {
		if definition.Name == "GitHub" {
			t.Fatal("unconfigured executor advertised GitHub")
		}
	}
}

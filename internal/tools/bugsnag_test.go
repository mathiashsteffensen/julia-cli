package tools

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"julia/internal/core"
	"julia/internal/memory"
)

func bugsnagFixture(t *testing.T, mode string, handler func(*http.Request) (*http.Response, error)) *Executor {
	t.Helper()
	e, _ := fixture(t, mode, nil)
	e.config.Bugsnag = BugsnagAuth{token: bugsnagFixtureToken, endpoint: bugsnagOrigin}
	e.webClient = newWebSearchClient()
	e.webClient.Transport = webRoundTrip(handler)
	return e
}

func TestBugsnagReadsInEveryModeWithoutApproval(t *testing.T) {
	for _, mode := range []string{"auto", "ask", "read-only", "yolo"} {
		t.Run(mode, func(t *testing.T) {
			requests := 0
			e := bugsnagFixture(t, mode, func(req *http.Request) (*http.Response, error) {
				requests++
				if req.Method != "GET" || req.URL.String() != bugsnagOrigin+"/projects/project-1/errors" || req.Body != nil {
					t.Fatalf("unexpected request: %s %s", req.Method, req.URL)
				}
				if req.Header.Get("Authorization") != "token "+bugsnagFixtureToken || req.Header.Get("X-Version") != "2" || req.Header.Get("Accept") != "application/json" {
					t.Fatal("missing authentication or v2 headers")
				}
				return webResponse(200, "application/json", `[{"id":"error-1","message":"failed"}]`), nil
			})
			deny := func(context.Context, core.Approval) (core.Decision, error) {
				t.Fatal("read requested approval")
				return core.Decision{}, nil
			}
			e.config.Approve, e.config.Review = deny, deny
			result := invoke(e, "Bugsnag", map[string]any{"path": "/projects/project-1/errors"})
			if result.IsError || requests != 1 || !strings.Contains(result.Content, `"id":"error-1"`) {
				t.Fatalf("result=%+v requests=%d", result, requests)
			}
		})
	}
}

func TestBugsnagProjectDiscoveryOmitsAPIKeys(t *testing.T) {
	e := bugsnagFixture(t, "read-only", func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/user/organizations":
			return webResponse(200, "application/json", `[{"id":"org-1","name":"Company","projects_url":"https://api.bugsnag.com/organizations/org-1/projects","api_key":"organization-secret","extra":{"api_key":"nested-secret"}}]`), nil
		case "/organizations/org-1/projects":
			resp := webResponse(200, "application/json", `[{"id":"project-1","name":"Backend","release_stages":["production"],"api_key":"notifier-secret","upload_api_key":"upload-secret"}]`)
			resp.Header.Add("Link", `<https://api.bugsnag.com/organizations/org-1/projects?offset=30&per_page=30>; rel="next"`)
			return resp, nil
		default:
			t.Fatalf("unexpected path %s", req.URL.Path)
			return nil, nil
		}
	})
	organizations := invoke(e, "Bugsnag", map[string]any{"path": "/user/organizations"})
	if organizations.IsError || !strings.Contains(organizations.Content, "Company") || strings.Contains(organizations.Content, "secret") {
		t.Fatalf("organizations: %+v", organizations)
	}
	projects := invoke(e, "Bugsnag", map[string]any{"path": "/organizations/org-1/projects"})
	if projects.IsError || !strings.Contains(projects.Content, "Backend") || !strings.Contains(projects.Content, "production") || strings.Contains(projects.Content, "secret") {
		t.Fatalf("projects: %+v", projects)
	}
	if !strings.Contains(projects.Content, "Next page path (use offset 0): /organizations/org-1/projects?offset=30&per_page=30") {
		t.Fatalf("missing project pagination: %s", projects.Content)
	}
}

func TestBugsnagReadsIssueAndCompleteDiagnosticEvent(t *testing.T) {
	event := `{"id":"event-1","exceptions":[{"errorClass":"TypeError","stacktrace":[{"file":"app.rb","line_number":42,"code":{"42":"raise 'failed'"}}]}],"threads":[{"stacktrace":[{"lineNumber":17}]}],"breadcrumbs":[{"metaData":{"route":"/trips"}}],"metaData":{"customCase":{"count":9007199254740993}},"request":{"url":"https://app.example/trips"},"app":{"version":"1.2.3"},"device":{"osVersion":"19"},"user":{"id":"user-1"}}`
	paths := []string{
		"/projects/project-1/errors/error-1",
		"/projects/project-1/errors/error-1/events",
		"/projects/project-1/events/event-1",
		"/errors/error-1/latest_event",
		"/projects/project-1/events",
		"/projects/project-1/event_fields",
	}
	e := bugsnagFixture(t, "read-only", func(req *http.Request) (*http.Response, error) {
		if req.URL.Host != "api.bugsnag.smartbear.com" {
			t.Fatalf("wrong configured host: %s", req.URL.Host)
		}
		switch req.URL.Path {
		case "/projects/project-1/errors/error-1":
			return webResponse(200, "application/json", `{"id":"error-1","status":"open","events":12,"users":3}`), nil
		case "/projects/project-1/errors/error-1/events", "/projects/project-1/events":
			return webResponse(200, "application/json", `[{"id":"event-1","is_full_report":false}]`), nil
		case "/projects/project-1/event_fields":
			return webResponse(200, "application/json", `[{"name":"app.release_stage"}]`), nil
		default:
			return webResponse(200, "application/json", event), nil
		}
	})
	e.config.Bugsnag.endpoint = "https://api.bugsnag.smartbear.com"
	for _, path := range paths {
		result := invoke(e, "Bugsnag", map[string]any{"path": path})
		if result.IsError {
			t.Fatalf("%s: %s", path, result.Content)
		}
		if strings.HasSuffix(path, "/event-1") || strings.HasSuffix(path, "/latest_event") {
			for _, detail := range []string{"TypeError", "app.rb", "line_number", "lineNumber", "breadcrumbs", "metaData", "9007199254740993", "request", "app", "device", "user"} {
				if !strings.Contains(result.Content, detail) {
					t.Errorf("%s lost %s: %s", path, detail, result.Content)
				}
			}
		}
	}
}

func TestBugsnagQueryPreservesFiltersAndPagination(t *testing.T) {
	e := bugsnagFixture(t, "auto", func(req *http.Request) (*http.Response, error) {
		query := req.URL.Query()
		if query.Get("filters[error.status][][type]") != "eq" || query.Get("filters[error.status][][value]") != "open" || query.Get("filters[event.message][][value]") != "Something went wrong" || query.Get("offset") != "next-cursor" {
			t.Fatalf("query changed: %s", req.URL.RawQuery)
		}
		if strings.Contains(req.URL.RequestURI(), " ") {
			t.Fatal("raw space in request URI")
		}
		return webResponse(200, "application/json", "[]"), nil
	})
	result := invoke(e, "Bugsnag", map[string]any{"path": "/projects/p1/errors?filters[error.status][][type]=eq&filters[error.status][][value]=open&filters[event.message][][value]=Something went wrong&offset=next-cursor"})
	if result.IsError {
		t.Fatal(result.Content)
	}
}

func TestBugsnagRepeatedFiltersKeepTheirOrder(t *testing.T) {
	query := "filters[user.id][][type]=eq&filters[user.id][][value]=user1&filters[user.id][][type]=eq&filters[user.id][][value]=user2"
	e := bugsnagFixture(t, "auto", func(req *http.Request) (*http.Response, error) {
		decoded, err := url.QueryUnescape(req.URL.RawQuery)
		if err != nil || decoded != query {
			t.Errorf("filter objects regrouped on request:\ngot  %s\nwant %s", decoded, query)
		}
		resp := webResponse(200, "application/json", "[]")
		resp.Header.Set("Link", "<"+bugsnagOrigin+"/projects/p1/errors?"+query+`&offset=next>; rel="next"`)
		return resp, nil
	})
	result := invoke(e, "Bugsnag", map[string]any{"path": "/projects/p1/errors?" + query})
	if result.IsError {
		t.Fatal(result.Content)
	}
	_, after, ok := strings.Cut(result.Content, "Next page path (use offset 0): ")
	if !ok {
		t.Fatal("pagination is missing")
	}
	next, _, _ := strings.Cut(after, "\n")
	decoded, err := url.QueryUnescape(next)
	if err != nil || decoded != "/projects/p1/errors?"+query+"&offset=next" {
		t.Fatalf("filter objects regrouped in pagination: %s", decoded)
	}
}

func TestBugsnagRejectsWritesAndUnsafePathsBeforeNetwork(t *testing.T) {
	requests := 0
	e := bugsnagFixture(t, "yolo", func(*http.Request) (*http.Response, error) {
		requests++
		return webResponse(200, "application/json", "{}"), nil
	})
	for _, args := range []map[string]any{
		{"path": "/projects/p1/errors", "method": "POST"},
		{"path": "/projects/p1/errors/e1", "body": "{}"},
		{"path": "/user/organizations", "headers": map[string]string{"X-Test": "x"}},
		{"path": "https://api.bugsnag.com/user/organizations"},
		{"path": "//other.example/user/organizations"},
		{"path": "/projects/p1/errors/e1/comments"},
		{"path": "/projects/p1"},
		{"path": "/organizations/org1/projects/../users"},
		{"path": "/projects/p1/errors/%2e%2e"},
		{"path": "/projects/p1%2fother/errors"},
		{"path": "/%75ser/organizations"},
		{"path": "/projects//errors"},
		{"path": "/user/organizations#fragment"},
		{"path": "/user/organizations?auth_token=secret"},
		{"path": "/user/organizations?%5fmethod=DELETE"},
		{"path": "/user/organizations?callback=run"},
		{"path": "/user/organizations?bad=%0a"},
		{"path": "/user/organizations?bad=%ff"},
		{"path": "/user/organizations?bad=%"},
		{"path": "/user/organizations", "offset": -1},
		{"path": "/user/organizations", "offset": 0.5},
		{"path": strings.Repeat("/", 8193)},
		{},
	} {
		if result := invoke(e, "Bugsnag", args); !result.IsError {
			t.Fatalf("accepted unsafe arguments: %+v", args)
		}
	}
	if requests != 0 {
		t.Fatalf("unsafe inputs reached network %d times", requests)
	}
}

func TestBugsnagRefusesEveryRedirect(t *testing.T) {
	for _, target := range []string{bugsnagOrigin + "/user/organizations", "https://other.example/collect", "http://api.bugsnag.com/user/organizations"} {
		requests := 0
		e := bugsnagFixture(t, "auto", func(*http.Request) (*http.Response, error) {
			requests++
			response := webResponse(302, "text/plain", bugsnagFixtureToken)
			response.Header.Set("Location", target)
			return response, nil
		})
		result := invoke(e, "Bugsnag", map[string]any{"path": "/user/organizations"})
		if !result.IsError || !strings.Contains(result.Content, "redirect refused") || requests != 1 || strings.Contains(result.Content, bugsnagFixtureToken) {
			t.Fatalf("redirect %s: %+v requests=%d", target, result, requests)
		}
	}
}

func TestBugsnagPaginationStaysOnTheSameEndpoint(t *testing.T) {
	current, _ := url.Parse(bugsnagOrigin + "/projects/p1/errors")
	for _, target := range []string{
		"https://other.example/projects/p1/errors?offset=30",
		"http://api.bugsnag.com/projects/p1/errors?offset=30",
		"https://api.bugsnag.com@other.example/projects/p1/errors?offset=30",
		bugsnagOrigin + "/projects/p2/errors?offset=30",
		bugsnagOrigin + "/projects/p1/errors?auth_token=bad",
		bugsnagOrigin + "/projects/p1/errors?offset=" + bugsnagFixtureToken,
		bugsnagOrigin + "/projects/p1/errors?offset=30#fragment",
	} {
		if got := bugsnagNextPage([]string{"<" + target + `>; rel="next"`}, current, bugsnagFixtureToken); got != "" {
			t.Fatalf("unsafe continuation %q became %q", target, got)
		}
	}
	if got := bugsnagNextPage([]string{`<?offset=30>; rel="next"`}, current, bugsnagFixtureToken); got != "/projects/p1/errors?offset=30" {
		t.Fatalf("relative continuation: %q", got)
	}
}

func TestBugsnagHTTPAndTransportErrorsDoNotLeakOrRetry(t *testing.T) {
	for _, status := range []int{401, 403, 404, 429, 500} {
		requests := 0
		e := bugsnagFixture(t, "auto", func(*http.Request) (*http.Response, error) {
			requests++
			resp := webResponse(status, "application/json", `{"message":"`+bugsnagFixtureToken+`"}`)
			resp.Header.Set("Retry-After", "7")
			return resp, nil
		})
		result := invoke(e, "Bugsnag", map[string]any{"path": "/user/organizations"})
		if !result.IsError || !strings.Contains(result.Content, strconv.Itoa(status)) || strings.Contains(result.Content, bugsnagFixtureToken) || requests != 1 {
			t.Fatalf("status %d: %+v requests=%d", status, result, requests)
		}
		if status == 429 && !strings.Contains(result.Content, "7 seconds") {
			t.Fatal("missing retry delay")
		}
	}
	requests := 0
	e := bugsnagFixture(t, "auto", func(*http.Request) (*http.Response, error) {
		requests++
		return nil, errors.New("transport exposed " + bugsnagFixtureToken)
	})
	result := invoke(e, "Bugsnag", map[string]any{"path": "/user/organizations"})
	if !result.IsError || strings.Contains(result.Content, bugsnagFixtureToken) || requests != 1 {
		t.Fatalf("transport: %+v requests=%d", result, requests)
	}
}

func TestBugsnagLongTraceCanBeReadWithoutLoss(t *testing.T) {
	body := `{"exceptions":[{"stacktrace":[{"code":"` + strings.Repeat("æ🙂", 12000) + bugsnagFixtureToken + `"}]}],"metaData":{"token":"\u0062ugsnag_fixture_0123456789abcdef"}}`
	e := bugsnagFixture(t, "auto", func(*http.Request) (*http.Response, error) {
		return webResponse(200, "application/json", body), nil
	})
	offsetPattern := regexp.MustCompile(`Continue this path with offset ([0-9]+)`)
	var joined strings.Builder
	offset, calls := 0, 0
	for {
		result := invoke(e, "Bugsnag", map[string]any{"path": "/projects/p1/events/event-1", "offset": offset})
		if result.IsError || len(result.Content) > maxOutput || !utf8.ValidString(result.Content) || strings.Contains(result.Content, bugsnagFixtureToken) {
			t.Fatalf("chunk: %+v", result)
		}
		header, chunk, ok := strings.Cut(result.Content, "\n\n")
		if !ok {
			t.Fatal("missing body separator")
		}
		joined.WriteString(chunk)
		calls++
		matches := offsetPattern.FindStringSubmatch(header)
		if matches == nil {
			break
		}
		next, _ := strconv.Atoi(matches[1])
		if next <= offset || calls > 10 {
			t.Fatalf("continuation does not advance: %d to %d", offset, next)
		}
		offset = next
	}
	if calls < 2 || !json.Valid([]byte(joined.String())) || strings.Contains(joined.String(), "bugsnag_fixture") || !strings.Contains(joined.String(), "[redacted]") {
		t.Fatal("trace or token redaction lost across chunks")
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(joined.String()), &decoded); err != nil {
		t.Fatal(err)
	}
	code := decoded["exceptions"].([]any)[0].(map[string]any)["stacktrace"].([]any)[0].(map[string]any)["code"]
	if code != strings.Repeat("æ🙂", 12000)+"[redacted]" {
		t.Fatal("trace content changed")
	}
}

func TestBugsnagRejectsOversizedOrInvalidResponses(t *testing.T) {
	for _, body := range []string{
		`{"message":"` + strings.Repeat("x", bugsnagMaxResponse) + `"}`,
		`{"message":`,
		`{} {}`,
		"{\"message\":\"\xff\"}",
	} {
		e := bugsnagFixture(t, "auto", func(*http.Request) (*http.Response, error) {
			return webResponse(200, "application/json", body), nil
		})
		if result := invoke(e, "Bugsnag", map[string]any{"path": "/projects/p1/events/event-1"}); !result.IsError || len(result.Content) > maxOutput {
			t.Fatalf("invalid response was accepted: %+v", result)
		}
	}
}

func TestBugsnagRegistryAndUnconfiguredExecution(t *testing.T) {
	t.Setenv(SlackTokenEnv, "")
	e := bugsnagFixture(t, "read-only", func(*http.Request) (*http.Response, error) {
		t.Fatal("unconfigured request reached network")
		return nil, nil
	})
	actual, _ := json.Marshal(e.Definitions())
	registered, _ := json.Marshal(Definitions(nil, false, false, false, true))
	if string(actual) != string(registered) {
		t.Fatal("Bugsnag registry differs from executor")
	}
	e.config.Bugsnag = BugsnagAuth{}
	if strings.Contains(strings.Join(toolNames(e.Definitions()), ","), "Bugsnag") {
		t.Fatal("unconfigured executor advertises Bugsnag")
	}
	result := invoke(e, "Bugsnag", map[string]any{"path": "/user/organizations"})
	if !result.IsError || !strings.Contains(result.Content, BugsnagTokenEnv) {
		t.Fatalf("unconfigured request: %+v", result)
	}
}

func TestBugsnagCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	e := bugsnagFixture(t, "auto", func(req *http.Request) (*http.Response, error) {
		cancel()
		return nil, req.Context().Err()
	})
	result := e.Execute(ctx, core.ToolCall{Name: "Bugsnag", Arguments: json.RawMessage(`{"path":"/user/organizations"}`)})
	if !result.IsError || !strings.Contains(result.Content, context.Canceled.Error()) {
		t.Fatalf("cancellation: %+v", result)
	}
}

func TestBugsnagOffsetsRejectInvalidPositions(t *testing.T) {
	for _, offset := range []int{2, 100} {
		if _, err := bugsnagOutput([]byte("a🙂b"), offset, ""); err == nil {
			t.Fatalf("accepted offset %d", offset)
		}
	}
	out, err := bugsnagOutput([]byte("hello"), 5, "")
	if err != nil || !strings.HasSuffix(out, "\n\n") {
		t.Fatalf("end of body: %q %v", out, err)
	}
}

func TestBugsnagCredentialPathsAreProtected(t *testing.T) {
	e, _ := fixture(t, "yolo", nil)
	for _, name := range []string{"bugsnag.json", "settings/Bugsnag.JSON"} {
		result := invoke(e, "Read", map[string]any{"path": name})
		if !result.IsError || !strings.Contains(result.Content, "protected") {
			t.Fatalf("%s: %+v", name, result)
		}
	}
	// The generated Seatbelt pattern must also deny case aliases.
	profile := seatbeltProfile("/tmp/project", "/tmp/run", "/tmp/scratch", offline, nil)
	if !strings.Contains(profile, "[bB][uU][gG][sS][nN][aA][gG][.][jJ][sS][oO][nN]") {
		t.Fatal("Bugsnag credential file is absent from the sandbox deny pattern")
	}
}

func TestBugsnagResponsesTaintMemory(t *testing.T) {
	wrapped := WithMemory(stubTools{"private event data"}, MemoryOptions{Store: &memory.Store{Directory: t.TempDir()}, Workspace: t.TempDir(), Mode: "auto"})
	wrapped.Execute(context.Background(), core.ToolCall{Name: "Bugsnag", Arguments: json.RawMessage(`{"path":"/projects/p1/events/e1"}`)})
	if got := wrapped.Tainted(); len(got) != 1 || got[0] != "Bugsnag" {
		t.Fatalf("Bugsnag did not taint memory: %v", got)
	}
}

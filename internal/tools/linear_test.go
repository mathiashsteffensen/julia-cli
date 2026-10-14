package tools

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"julia/internal/core"
)

type linearCallRecord struct {
	query     string
	variables map[string]any
}

// linearFixture answers Linear GraphQL calls from handlers keyed by a
// substring of the query, in order of registration, and records every call.
type linearHandler struct {
	match string
	reply func(call linearCallRecord, attempt int) *http.Response
}

func linearFixture(t *testing.T, mode string, handlers []linearHandler) (*Executor, *[]linearCallRecord) {
	t.Helper()
	e, _ := fixture(t, mode, nil)
	e.config.LinearToken = "lin_oauth_test"
	var calls []linearCallRecord
	attempts := map[string]int{}
	e.webClient = newWebSearchClient()
	e.webClient.Transport = webRoundTrip(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodPost || req.URL.String() != linearEndpoint || req.Header.Get("Authorization") != "Bearer lin_oauth_test" || req.Header.Get("Content-Type") != "application/json" {
			t.Errorf("unexpected request %s %s auth %q type %q", req.Method, req.URL, req.Header.Get("Authorization"), req.Header.Get("Content-Type"))
		}
		body, _ := io.ReadAll(req.Body)
		var payload struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Errorf("request body is not JSON: %s", body)
		}
		call := linearCallRecord{payload.Query, payload.Variables}
		calls = append(calls, call)
		for _, handler := range handlers {
			if strings.Contains(payload.Query, handler.match) {
				attempts[handler.match]++
				return handler.reply(call, attempts[handler.match]), nil
			}
		}
		t.Errorf("unexpected Linear query %s", payload.Query)
		return webResponse(http.StatusBadRequest, "application/json", `{"errors":[{"message":"unexpected query"}]}`), nil
	})
	return e, &calls
}

func linearOK(data string) func(linearCallRecord, int) *http.Response {
	return func(linearCallRecord, int) *http.Response {
		return webResponse(http.StatusOK, "application/json", `{"data":`+data+`}`)
	}
}

const (
	linearTeamJSON  = `{"id":"T1","key":"QUA","name":"Quantrading","states":{"nodes":[{"id":"S1","name":"Todo","type":"unstarted","position":1},{"id":"S2","name":"In Progress","type":"started","position":2},{"id":"S3","name":"Done","type":"completed","position":3}]}}`
	linearIssueJSON = `{"id":"I14","identifier":"QUA-14","title":"Implement alpha decay monitoring","priorityLabel":"High","url":"https://linear.app/q/issue/QUA-14","updatedAt":"2026-05-21T19:31:50.238Z","createdAt":"2026-05-20T10:00:00.000Z","state":{"name":"Todo"},"assignee":{"name":"Mathias","displayName":"mathias"},"delegate":{"name":"JULIA","displayName":"julia"},"creator":{"name":"Mathias"},"project":{"name":"Live trading"},"team":{"id":"T1","key":"QUA"},"description":"Use **measurable** triggers.","labels":{"nodes":[{"name":"research"}]},"comments":{"nodes":[{"body":"Second","createdAt":"2026-05-21T12:00:00.000Z","user":{"name":"JULIA"}},{"body":"First","createdAt":"2026-05-20T12:00:00.000Z","user":{"name":"Mathias"}}]}}`
)

func TestLinearTokenLoading(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "linear.json")
	t.Setenv(LinearTokenEnv, "")
	if token, err := LoadLinearToken(path); err != nil || token != "" {
		t.Fatalf("missing file: %q %v", token, err)
	}
	if token, err := LoadLinearToken(""); err != nil || token != "" {
		t.Fatalf("no path: %q %v", token, err)
	}
	os.WriteFile(path, []byte(`{"api_key":"lin_oauth_1234567890abcdef"}`), 0600)
	if token, err := LoadLinearToken(path); err != nil || token != "lin_oauth_1234567890abcdef" {
		t.Fatalf("file: %q %v", token, err)
	}
	os.WriteFile(path, []byte(`{"api_key":"short"}`), 0600)
	if _, err := LoadLinearToken(path); err == nil || !strings.Contains(err.Error(), "valid Linear access token") {
		t.Fatalf("short token accepted: %v", err)
	}
	os.WriteFile(path, []byte(`not json`), 0600)
	if _, err := LoadLinearToken(path); err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("bad json: %v", err)
	}
	t.Setenv(LinearTokenEnv, "lin_oauth_fromenv_0123456789")
	if token, err := LoadLinearToken(path); err != nil || token != "lin_oauth_fromenv_0123456789" {
		t.Fatalf("env should win over the file: %q %v", token, err)
	}
	t.Setenv(LinearTokenEnv, "has space in it which is wrong")
	if _, err := LoadLinearToken(path); err == nil || !strings.Contains(err.Error(), LinearTokenEnv) {
		t.Fatalf("env token with spaces accepted: %v", err)
	}
}

func TestLinearOfferedOnlyWithToken(t *testing.T) {
	if names := toolNames(Definitions(nil, false)); strings.Contains(strings.Join(names, " "), "Linear") {
		t.Fatalf("Linear offered without a token: %v", names)
	}
	definitions := Definitions(nil, true)
	last := definitions[len(definitions)-1]
	if last.Name != "Linear" || !strings.Contains(last.Description, "untrusted context") {
		t.Fatalf("Linear definition %+v", last)
	}
	e, _ := fixture(t, "auto", nil)
	if names := toolNames(e.Definitions()); strings.Contains(strings.Join(names, " "), "Linear") {
		t.Fatalf("executor without a token offers Linear: %v", names)
	}
	if r := invoke(e, "Linear", map[string]any{"action": "list_teams"}); !r.IsError || !strings.Contains(r.Content, LinearTokenEnv) {
		t.Fatalf("unconfigured Linear: %+v", r)
	}
	e.config.LinearToken = "lin_oauth_test"
	if names := toolNames(e.Definitions()); !strings.Contains(strings.Join(names, " "), "Linear") {
		t.Fatalf("executor with a token offers no Linear: %v", names)
	}
}

func TestLinearReadsNeedNoReviewAndFormat(t *testing.T) {
	e, calls := linearFixture(t, "read-only", []linearHandler{
		{"viewer { name displayName } teams", linearOK(`{"viewer":{"name":"JULIA","displayName":"julia"},"teams":{"nodes":[` + linearTeamJSON + `]}}`)},
		{"teams(filter: { key: { eq: $key } }) { nodes { projects", linearOK(`{"teams":{"nodes":[{"projects":{"nodes":[{"id":"P1","name":"Live trading","description":"Ship the engine","targetDate":"2026-12-01","url":"https://linear.app/q/project/live","status":{"name":"In Progress"},"lead":{"name":"Mathias"},"teams":{"nodes":[{"key":"QUA"}]}}]}}]}}`)},
		{"{ projects(first: 100)", linearOK(`{"projects":{"nodes":[]}}`)},
		{"users(filter:", linearOK(`{"users":{"nodes":[{"id":"U1","name":"Mathias","displayName":"mathias"}]}}`)},
		{"issues(first: $first", func(call linearCallRecord, _ int) *http.Response {
			return webResponse(http.StatusOK, "application/json", `{"data":{"issues":{"nodes":[`+linearIssueJSON+`]}}}`)
		}},
		{"issue(id: $id)", linearOK(`{"issue":` + linearIssueJSON + `}`)},
	})
	e.config.Review = func(context.Context, core.Approval) (core.Decision, error) {
		t.Fatal("a read was reviewed")
		return core.Decision{}, nil
	}
	r := invoke(e, "Linear", map[string]any{"action": "list_teams"})
	if r.IsError || r.Content != "Acting as JULIA.\n\nQUA Quantrading\n  states: Todo (unstarted), In Progress (started), Done (completed)" {
		t.Fatalf("teams: %+v", r)
	}
	r = invoke(e, "Linear", map[string]any{"action": "list_projects", "team": "qua"})
	if r.IsError || r.Content != "Live trading (In Progress; lead Mathias; target 2026-12-01; teams QUA)\nhttps://linear.app/q/project/live\nShip the engine" || (*calls)[1].variables["key"] != "QUA" {
		t.Fatalf("projects: %+v %+v", r, (*calls)[1])
	}
	if r := invoke(e, "Linear", map[string]any{"action": "list_projects"}); r.IsError || r.Content != "No projects." {
		t.Fatalf("no projects: %+v", r)
	}
	r = invoke(e, "Linear", map[string]any{"action": "list_issues", "team": "QUA", "assignee": "mathias", "project": "Live trading", "limit": 1})
	if r.IsError || r.Content != "QUA-14 Implement alpha decay monitoring (Todo; assignee Mathias; delegate JULIA; high; project Live trading; updated 2026-05-21 19:31)\n\n[more may exist; raise limit or narrow the filter]" {
		t.Fatalf("issues: %+v", r)
	}
	filter, _ := json.Marshal((*calls)[4].variables["filter"])
	if want := `{"assignee":{"id":{"eq":"U1"}},"project":{"name":{"eqIgnoreCase":"Live trading"}},"state":{"type":{"nin":["completed","canceled"]}},"team":{"key":{"eq":"QUA"}}}`; string(filter) != want || (*calls)[4].variables["first"] != float64(1) {
		t.Fatalf("filter %s", filter)
	}
	r = invoke(e, "Linear", map[string]any{"action": "list_issues", "state": "done", "limit": 50})
	filter, _ = json.Marshal((*calls)[5].variables["filter"])
	if r.IsError || strings.Contains(r.Content, "more may exist") || string(filter) != `{"state":{"name":{"eqIgnoreCase":"done"}}}` {
		t.Fatalf("closed issues: %+v filter %s", r, filter)
	}
	r = invoke(e, "Linear", map[string]any{"action": "read_issue", "identifier": "qua-14"})
	want := "QUA-14 Implement alpha decay monitoring (Todo; assignee Mathias; delegate JULIA; high; project Live trading; updated 2026-05-21 19:31)\nhttps://linear.app/q/issue/QUA-14\ncreated by Mathias 2026-05-20 10:00; labels: research\n\nUse **measurable** triggers.\n\n[2026-05-20 12:00] Mathias:\nFirst\n\n[2026-05-21 12:00] JULIA:\nSecond"
	if r.IsError || r.Content != want {
		t.Fatalf("issue:\n%s\nwant:\n%s", r.Content, want)
	}
	if (*calls)[6].variables["id"] != "QUA-14" {
		t.Fatalf("issue lookup %+v", (*calls)[6])
	}
	for _, args := range []map[string]any{
		{"action": "create_issue", "team": "QUA", "title": "x"},
		{"action": "update_issue", "identifier": "QUA-14", "state": "Done"},
		{"action": "add_comment", "identifier": "QUA-14", "body": "hi"},
	} {
		if r := invoke(e, "Linear", args); !r.IsError || !strings.Contains(r.Content, "read-only") {
			t.Fatalf("read-only %v: %+v", args, r)
		}
	}
	if len(*calls) != 7 {
		t.Fatalf("read-only writes reached Linear: %d calls", len(*calls))
	}
}

func TestLinearWritesResolveNamesAndAreReviewed(t *testing.T) {
	e, calls := linearFixture(t, "auto", []linearHandler{
		{"teams(filter: { key: { eq: $key } }) { nodes { id key name states", linearOK(`{"teams":{"nodes":[` + linearTeamJSON + `]}}`)},
		{"projects(filter: { name: { eqIgnoreCase: $name } })", linearOK(`{"projects":{"nodes":[{"id":"P1","name":"Live trading"}]}}`)},
		{"users(filter:", linearOK(`{"users":{"nodes":[{"id":"U2","name":"Mathias Steffensen","displayName":"msteff"},{"id":"U1","name":"Mathias","displayName":"m"}]}}`)},
		{"issueCreate", linearOK(`{"issueCreate":{"success":true,"issue":{"identifier":"QUA-15","url":"https://linear.app/q/issue/QUA-15"}}}`)},
		{"issue(id: $id)", linearOK(`{"issue":{"id":"I14","identifier":"QUA-14","team":{"id":"T1","key":"QUA"}}}`)},
		{"issueUpdate", linearOK(`{"issueUpdate":{"success":true,"issue":` + linearIssueJSON + `}}`)},
		{"commentCreate", linearOK(`{"commentCreate":{"success":true,"comment":{"url":"https://linear.app/q/issue/QUA-14#comment-1"}}}`)},
	})
	var reviewed []core.Approval
	e.config.Review = func(_ context.Context, a core.Approval) (core.Decision, error) {
		reviewed = append(reviewed, a)
		return core.Decision{Allow: true, Reason: "owner asked"}, nil
	}
	r := invoke(e, "Linear", map[string]any{"action": "create_issue", "team": "qua", "title": "Backtest harness", "description": "Details", "project": "Live trading", "priority": "High", "assignee": "Mathias", "state": "in progress"})
	if r.IsError || r.Content != "Created QUA-15: https://linear.app/q/issue/QUA-15" {
		t.Fatalf("create: %+v", r)
	}
	if len(reviewed) != 1 || reviewed[0].ExecutionMode != "linear" || !reviewed[0].Network || !strings.Contains(string(reviewed[0].Call.Arguments), "Backtest harness") {
		t.Fatalf("review %+v", reviewed)
	}
	// The review comes first; the lookups and the mutation follow it.
	if len(*calls) != 4 || !strings.Contains((*calls)[3].query, "issueCreate") {
		t.Fatalf("calls %+v", *calls)
	}
	input, _ := json.Marshal((*calls)[3].variables["input"])
	if want := `{"assigneeId":"U1","description":"Details","priority":2,"projectId":"P1","stateId":"S2","teamId":"T1","title":"Backtest harness"}`; string(input) != want {
		t.Fatalf("create input %s", input)
	}
	r = invoke(e, "Linear", map[string]any{"action": "update_issue", "identifier": "QUA-14", "state": "done", "assignee": "none", "priority": "low"})
	if r.IsError || !strings.HasPrefix(r.Content, "Updated QUA-14 Implement alpha decay monitoring (Todo;") {
		t.Fatalf("update: %+v", r)
	}
	update := (*calls)[len(*calls)-1]
	input, _ = json.Marshal(update.variables["input"])
	if !strings.Contains(update.query, "issueUpdate") || update.variables["id"] != "I14" || string(input) != `{"assigneeId":null,"priority":4,"stateId":"S3"}` {
		t.Fatalf("update call %+v input %s", update, input)
	}
	r = invoke(e, "Linear", map[string]any{"action": "add_comment", "identifier": "QUA-14", "body": "Done, sir."})
	if r.IsError || r.Content != "Commented on QUA-14: https://linear.app/q/issue/QUA-14#comment-1" {
		t.Fatalf("comment: %+v", r)
	}
	input, _ = json.Marshal((*calls)[len(*calls)-1].variables["input"])
	if string(input) != `{"body":"Done, sir.","issueId":"I14"}` {
		t.Fatalf("comment input %s", input)
	}
	if len(reviewed) != 3 {
		t.Fatalf("reviews %d", len(reviewed))
	}
	// A denied write never reaches Linear.
	before := len(*calls)
	e.config.Review = func(context.Context, core.Approval) (core.Decision, error) {
		return core.Decision{Allow: false, Reason: "not asked for"}, nil
	}
	if r := invoke(e, "Linear", map[string]any{"action": "add_comment", "identifier": "QUA-14", "body": "hello"}); !r.IsError || !strings.Contains(r.Content, "denied") || len(*calls) != before {
		t.Fatalf("denied comment: %+v calls %d", r, len(*calls)-before)
	}
}

func TestLinearValidatesArgumentsAndReportsErrors(t *testing.T) {
	e, calls := linearFixture(t, "yolo", []linearHandler{
		{"users(filter:", func(call linearCallRecord, _ int) *http.Response {
			if call.variables["name"] == "nobody" {
				return webResponse(http.StatusOK, "application/json", `{"data":{"users":{"nodes":[]}}}`)
			}
			return webResponse(http.StatusOK, "application/json", `{"data":{"users":{"nodes":[{"id":"U1","name":"Mat Ias"},{"id":"U2","name":"Mathias"}]}}}`)
		}},
		{"issue(id: $id)", func(call linearCallRecord, attempt int) *http.Response {
			switch attempt {
			case 1:
				r := webResponse(http.StatusTooManyRequests, "application/json", `{"errors":[{"message":"Rate limited"}]}`)
				r.Header.Set("Retry-After", "0")
				return r
			case 2:
				return webResponse(http.StatusOK, "application/json", `{"errors":[{"message":"Entity not found: Issue - Could not find referenced Issue."}],"data":null}`)
			case 3:
				return webResponse(http.StatusUnauthorized, "application/json", `{"errors":[{"message":"Authentication required"}]}`)
			case 4:
				return webResponse(http.StatusInternalServerError, "text/html", "<html>")
			}
			return webResponse(http.StatusOK, "application/json", `{"data":{"issue":{"id":"I1","identifier":"QUA-1"}}}`)
		}},
		{"commentCreate", linearOK(`{"commentCreate":{"success":false}}`)},
	})
	for _, args := range []map[string]any{
		{"action": "dance"},
		{"action": "list_projects", "team": "not a key"},
		{"action": "list_issues", "limit": 500},
		{"action": "list_issues", "state": "bad\x01state"},
		{"action": "read_issue", "identifier": "QUA14"},
		{"action": "create_issue", "team": "QUA"},
		{"action": "create_issue", "team": "QUA", "title": "x", "priority": "asap"},
		{"action": "update_issue", "identifier": "QUA-14"},
		{"action": "add_comment", "identifier": "QUA-14", "body": "  "},
	} {
		if r := invoke(e, "Linear", args); !r.IsError || len(*calls) != 0 {
			t.Fatalf("accepted %v: %+v", args, r)
		}
	}
	if r := invoke(e, "Linear", map[string]any{"action": "list_issues", "assignee": "nobody"}); !r.IsError || r.Content != "nobody in the workspace is named nobody" {
		t.Fatalf("unknown assignee: %+v", r)
	}
	if r := invoke(e, "Linear", map[string]any{"action": "list_issues", "assignee": "mat"}); !r.IsError || r.Content != "mat matches several people: Mat Ias, Mathias" {
		t.Fatalf("ambiguous assignee: %+v", r)
	}
	// A rate limit is waited out once; Linear's errors come back shortened.
	if r := invoke(e, "Linear", map[string]any{"action": "read_issue", "identifier": "QUA-1"}); !r.IsError || r.Content != "no issue QUA-1 is visible to the app" || len(*calls) != 4 {
		t.Fatalf("missing issue: %+v calls %d", r, len(*calls))
	}
	if r := invoke(e, "Linear", map[string]any{"action": "read_issue", "identifier": "QUA-1"}); !r.IsError || !strings.Contains(r.Content, "HTTP 401") || !strings.Contains(r.Content, "new developer token") {
		t.Fatalf("expired token: %+v", r)
	}
	if r := invoke(e, "Linear", map[string]any{"action": "read_issue", "identifier": "QUA-1"}); !r.IsError || r.Content != "linear: HTTP 500 with unexpected content type, not JSON" {
		t.Fatalf("http error: %+v", r)
	}
	if r := invoke(e, "Linear", map[string]any{"action": "add_comment", "identifier": "QUA-1", "body": "hi"}); !r.IsError || r.Content != "linear commentCreate did not succeed" {
		t.Fatalf("failed mutation: %+v", r)
	}
}

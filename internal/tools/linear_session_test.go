package tools

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"julia/internal/core"
)

const (
	testSessionID  = "aa98a2ee-8363-4e18-8567-cbd50397d0ba"
	testActivityID = "f31cebbb-36ae-49aa-8deb-54744e689200"
)

func TestLinearReadSession(t *testing.T) {
	page := `{"agentSession":{
		"id":"` + testSessionID + `", "status":"active", "url":"https://linear.app/session",
		"appUser":{"id":"app-id","name":"JULIA"}, "creator":{"id":"creator-id","name":"Creator"},
		"issue":{"identifier":"ENG-1","title":"Issue title","description":"issue context","url":"https://linear.app/issue"},
		"sourceComment":{"id":"source-id","body":"source directive","user":{"id":"owner-id","name":"Owner"}},
		"comment":{"id":"root-id","body":"root context","user":{"id":"root-author-id","name":"Root author"}},
		"activities":{"nodes":[
			{"id":"f31cebbb-36ae-49aa-8deb-54744e689200","createdAt":"2026-10-31T12:00:00Z","user":{"id":"later-user","name":"Later"},"signal":"stop","content":{"__typename":"AgentActivityResponseContent","body":"later response"}},
			{"id":"e31cebbb-36ae-49aa-8deb-54744e689200","createdAt":"2026-10-31T11:00:00Z","user":{"id":"earlier-user","name":"Earlier"},"signal":null,"content":{"__typename":"AgentActivityPromptContent","body":"earlier prompt"}}
		],"pageInfo":{"hasNextPage":true,"endCursor":"next+/="}}
	}}`
	var requests []string
	e, _ := linearFixture(t, "auto", []linearHandler{{match: "agentSession", reply: func(call linearCallRecord, index int) *http.Response {
		requests = append(requests, fmt.Sprintf("%+v", call))
		return linearOK(page)(call, index)
	}}})
	first := invoke(e, "Linear", map[string]any{"action": "read_session", "session_id": testSessionID})
	if first.IsError {
		t.Fatalf("read session: %v", first.Content)
	}
	output := fmt.Sprint(first.Content)
	for _, want := range []string{"has_next_page=true", "next+/=", "untrusted", "ENG-1", "issue context", "source directive", "owner-id", "root-author-id", "creator-id", "app-id", "earlier-user", "later-user", "AgentActivityPromptContent", "stop"} {
		if !strings.Contains(output, want) {
			t.Errorf("output missing %q: %s", want, output)
		}
	}
	if strings.Index(output, "earlier prompt") >= strings.Index(output, "later response") {
		t.Errorf("activities not chronological: %s", output)
	}
	second := invoke(e, "Linear", map[string]any{"action": "read_session", "session_id": testSessionID, "cursor": "next+/="})
	if second.IsError || len(requests) != 2 || !strings.Contains(requests[1], "next+/=") {
		t.Fatalf("next page cursor not forwarded: result=%v requests=%v", second.Content, requests)
	}
}

func TestLinearReadSessionValidation(t *testing.T) {
	for _, tc := range []struct{ name, id, cursor string }{
		{"missing id", "", ""},
		{"malformed id", "not-a-uuid", ""},
		{"control cursor", testSessionID, "bad\nvalue"},
		{"long cursor", testSessionID, strings.Repeat("x", 1001)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, calls := linearFixture(t, "auto", nil)
			result := invoke(e, "Linear", map[string]any{"action": "read_session", "session_id": tc.id, "cursor": tc.cursor})
			if !result.IsError || len(*calls) != 0 {
				t.Fatalf("invalid input should fail before API: result=%v calls=%v", result.Content, *calls)
			}
		})
	}
}

func TestLinearReadSessionNotVisible(t *testing.T) {
	e, _ := linearFixture(t, "auto", []linearHandler{{match: "agentSession", reply: linearOK(`{"agentSession":null}`)}})
	result := invoke(e, "Linear", map[string]any{"action": "read_session", "session_id": testSessionID})
	if !result.IsError || !strings.Contains(fmt.Sprint(result.Content), "not visible") {
		t.Fatalf("expected not-visible error: %v", result.Content)
	}
}

func TestLinearSessionActivity(t *testing.T) {
	for _, activityType := range []string{"thought", "elicitation", "response", "error"} {
		t.Run(activityType, func(t *testing.T) {
			var request string
			e, _ := linearFixture(t, "yolo", []linearHandler{{match: "agentActivityCreate", reply: func(call linearCallRecord, index int) *http.Response {
				request = fmt.Sprintf("%+v", call)
				return linearOK(`{"agentActivityCreate":{"success":true,"agentActivity":{"id":"`+testActivityID+`"}}}`)(call, index)
			}}})
			result := invoke(e, "Linear", map[string]any{"action": "session_activity", "session_id": testSessionID, "activity_type": activityType, "activity_id": testActivityID, "body": "public message"})
			if result.IsError {
				t.Fatalf("publish: %v", result.Content)
			}
			for _, want := range []string{"agentSessionId", testSessionID, "content", "type", activityType, "body", "public message", testActivityID} {
				if !strings.Contains(request, want) {
					t.Errorf("request missing %q: %s", want, request)
				}
			}
			output := fmt.Sprint(result.Content)
			if !strings.Contains(output, "visible to issue readers") {
				t.Errorf("visibility warning missing: %s", output)
			}
			if activityType == "elicitation" && (!strings.Contains(output, "End this invocation") || !strings.Contains(output, "future run")) {
				t.Errorf("elicitation must end without blocking: %s", output)
			}
		})
	}
}

func TestLinearSessionActivityValidation(t *testing.T) {
	for _, tc := range []struct{ name, id, activityType, activityID, body string }{
		{"invalid session", "bad", "response", "", "hello"},
		{"invalid type", testSessionID, "prompt", "", "hello"},
		{"empty body", testSessionID, "response", "", "  \n"},
		{"overlong body", testSessionID, "response", "", strings.Repeat("x", maxLinearText+1)},
		{"invalid id", testSessionID, "response", "not-a-uuid", "hello"},
		{"not v4", testSessionID, "response", "f31cebbb-36ae-19aa-8deb-54744e689200", "hello"},
		{"invalid variant", testSessionID, "response", "f31cebbb-36ae-49aa-0deb-54744e689200", "hello"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, calls := linearFixture(t, "yolo", nil)
			result := invoke(e, "Linear", map[string]any{"action": "session_activity", "session_id": tc.id, "activity_type": tc.activityType, "activity_id": tc.activityID, "body": tc.body})
			if !result.IsError || len(*calls) != 0 {
				t.Fatalf("invalid input should fail before API: result=%v calls=%v", result.Content, *calls)
			}
		})
	}
}

func TestLinearSessionActivityFailures(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reply func(linearCallRecord, int) *http.Response
	}{
		{"unsuccessful mutation", linearOK(`{"agentActivityCreate":{"success":false,"agentActivity":null}}`)},
		{"graphql error", func(linearCallRecord, int) *http.Response {
			return webResponse(http.StatusOK, "application/json", `{"errors":[{"message":"mutation failed"}]}`)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, calls := linearFixture(t, "yolo", []linearHandler{{match: "agentActivityCreate", reply: tc.reply}})
			result := invoke(e, "Linear", map[string]any{"action": "session_activity", "session_id": testSessionID, "activity_type": "response", "body": "hello"})
			if !result.IsError || len(*calls) != 1 {
				t.Fatalf("expected mutation failure without retry: result=%v calls=%v", result.Content, *calls)
			}
		})
	}
}

func TestLinearSessionActivityAuthorization(t *testing.T) {
	args := map[string]any{"action": "session_activity", "session_id": testSessionID, "activity_type": "response", "body": "hello"}
	t.Run("read-only", func(t *testing.T) {
		e, calls := linearFixture(t, "read-only", nil)
		result := invoke(e, "Linear", args)
		if !result.IsError || len(*calls) != 0 {
			t.Fatalf("read-only should reject without API: result=%v calls=%v", result.Content, *calls)
		}
	})
	t.Run("review denied", func(t *testing.T) {
		e, calls := linearFixture(t, "auto", nil)
		reviewed := false
		e.config.Review = func(context.Context, core.Approval) (core.Decision, error) {
			reviewed = true
			var denied core.Decision
			return denied, nil
		}
		result := invoke(e, "Linear", args)
		if !reviewed || !result.IsError || len(*calls) != 0 {
			t.Fatalf("denied review should reject without API: reviewed=%t result=%v calls=%v", reviewed, result.Content, *calls)
		}
	})
}

func TestLinearSessionActivityWithoutID(t *testing.T) {
	var request string
	e, _ := linearFixture(t, "yolo", []linearHandler{{match: "agentActivityCreate", reply: func(call linearCallRecord, index int) *http.Response {
		request = fmt.Sprintf("%+v", call)
		return linearOK(`{"agentActivityCreate":{"success":true,"agentActivity":{"id":"created-id"}}}`)(call, index)
	}}})
	result := invoke(e, "Linear", map[string]any{"action": "session_activity", "session_id": testSessionID, "activity_type": "response", "body": "hello"})
	if result.IsError {
		t.Fatalf("publish without activity id: %v", result.Content)
	}
	if strings.Contains(request, testActivityID) {
		t.Fatalf("unexpected activity id: %s", request)
	}
}

func TestLinearSessionTextTruncation(t *testing.T) {
	got := linearSessionText(strings.Repeat("é", 1000), 11)
	if !strings.HasSuffix(got, " [truncated]") || len(got) > 11+len(" [truncated]") || !strings.HasPrefix(got, strings.Repeat("é", 5)) {
		t.Fatalf("unexpected UTF-8 truncation: %q", got)
	}
}

func TestLinearSessionActivityReadback(t *testing.T) {
	const body = "public message"
	matching := func(id, sessionID, activityType, activityBody string) string {
		return fmt.Sprintf(`{"agentActivity":{"id":%q,"agentSession":{"id":%q},"content":{"__typename":%q,"body":%q}}}`, id, sessionID, activityType, activityBody)
	}
	graphqlError := func(linearCallRecord, int) *http.Response {
		return webResponse(http.StatusOK, "application/json", `{"errors":[{"message":"mutation failed"}]}`)
	}
	for _, tc := range []struct {
		name          string
		activityType  string
		mutationReply func(linearCallRecord, int) *http.Response
		lookupReply   func(linearCallRecord, int) *http.Response
		wantError     bool
		wantMessage   string
	}{
		{"duplicate thought", "thought", graphqlError, linearOK(matching(testActivityID, testSessionID, "AgentActivityThoughtContent", body)), false, ""},
		{"uncertain elicitation", "elicitation", linearOK(`{"agentActivityCreate":{"success":false}}`), linearOK(matching(testActivityID, testSessionID, "AgentActivityElicitationContent", body)), false, ""},
		{"missing response ID", "response", linearOK(`{"agentActivityCreate":{"success":true,"agentActivity":{}}}`), linearOK(matching(testActivityID, testSessionID, "AgentActivityResponseContent", body)), false, ""},
		{"duplicate error", "error", graphqlError, linearOK(matching(testActivityID, testSessionID, "AgentActivityErrorContent", body)), false, ""},
		{"different ID", "response", graphqlError, linearOK(matching("other-id", testSessionID, "AgentActivityResponseContent", body)), true, "mutation failed"},
		{"different session", "response", graphqlError, linearOK(matching(testActivityID, "other-session", "AgentActivityResponseContent", body)), true, "mutation failed"},
		{"different type", "response", graphqlError, linearOK(matching(testActivityID, testSessionID, "AgentActivityThoughtContent", body)), true, "mutation failed"},
		{"different body", "response", graphqlError, linearOK(matching(testActivityID, testSessionID, "AgentActivityResponseContent", "private reasoning")), true, "mutation failed"},
		{"not found", "response", graphqlError, linearOK(`{"agentActivity":null}`), true, "mutation failed"},
		{"lookup failure", "response", graphqlError, func(linearCallRecord, int) *http.Response {
			return webResponse(http.StatusOK, "application/json", `{"errors":[{"message":"lookup failed"}]}`)
		}, true, "mutation failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var lookupRequest string
			e, calls := linearFixture(t, "yolo", []linearHandler{
				{match: "agentActivityCreate", reply: tc.mutationReply},
				{match: "agentActivity(id:", reply: func(call linearCallRecord, index int) *http.Response {
					lookupRequest = fmt.Sprintf("%+v", call)
					return tc.lookupReply(call, index)
				}},
			})
			result := invoke(e, "Linear", map[string]any{"action": "session_activity", "session_id": testSessionID, "activity_type": tc.activityType, "activity_id": testActivityID, "body": body})
			output := fmt.Sprint(result.Content)
			if result.IsError != tc.wantError || !strings.Contains(output, tc.wantMessage) {
				t.Fatalf("unexpected result: %v", result.Content)
			}
			if len(*calls) != 2 || !strings.Contains(lookupRequest, testActivityID) {
				t.Fatalf("expected one mutation and lookup by explicit ID: calls=%v", *calls)
			}
			if strings.Contains(output, "private reasoning") || strings.Contains(output, "lookup failed") {
				t.Errorf("lookup details exposed: %s", output)
			}
			if !tc.wantError && !strings.Contains(output, testActivityID) {
				t.Errorf("successful readback did not return activity ID: %s", output)
			}
			if !tc.wantError && tc.activityType == "elicitation" && (!strings.Contains(output, "End this invocation") || !strings.Contains(output, "future run")) {
				t.Errorf("readback elicitation must end invocation: %s", output)
			}
		})
	}
}

func TestLinearSessionActivityRejectsInvalidSuccessID(t *testing.T) {
	for _, tc := range []struct {
		name, activityID, reply string
		lookup                  bool
	}{
		{"missing ID without explicit ID", "", `{"agentActivityCreate":{"success":true,"agentActivity":{}}}`, false},
		{"missing ID with explicit ID", testActivityID, `{"agentActivityCreate":{"success":true,"agentActivity":{}}}`, true},
		{"different ID with explicit ID", testActivityID, `{"agentActivityCreate":{"success":true,"agentActivity":{"id":"other-id"}}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, calls := linearFixture(t, "yolo", []linearHandler{
				{match: "agentActivityCreate", reply: linearOK(tc.reply)},
				{match: "agentActivity(id:", reply: linearOK(`{"agentActivity":null}`)},
			})
			result := invoke(e, "Linear", map[string]any{"action": "session_activity", "session_id": testSessionID, "activity_type": "response", "activity_id": tc.activityID, "body": "hello"})
			wantCalls := 1
			if tc.lookup {
				wantCalls++
			}
			if !result.IsError || len(*calls) != wantCalls {
				t.Fatalf("expected invalid success ID to fail: result=%v calls=%v", result.Content, *calls)
			}
		})
	}
}

func TestLinearSessionCursorRejectsInvalidUTF8(t *testing.T) {
	// JSON encoding replaces invalid UTF-8 before the tool receives it, so
	// exercise this validator directly rather than through invoke.
	if validLinearSessionCursor("\xff") {
		t.Fatal("accepted invalid UTF-8 cursor")
	}
}

func TestLinearSessionActivityApproval(t *testing.T) {
	e, calls := linearFixture(t, "auto", []linearHandler{{
		match: "agentActivityCreate",
		reply: linearOK(`{"agentActivityCreate":{"success":true,"agentActivity":{"id":"` + testActivityID + `"}}}`),
	}})
	reviewed := false
	e.config.Review = func(_ context.Context, approval core.Approval) (core.Decision, error) {
		reviewed = true
		if approval.ExecutionMode != "linear" || !approval.Network || !strings.Contains(string(approval.Call.Arguments), testSessionID) {
			t.Errorf("unexpected approval: %+v", approval)
		}
		return core.Decision{Allow: true, Reason: "owner requested this reply"}, nil
	}
	result := invoke(e, "Linear", map[string]any{
		"action": "session_activity", "session_id": testSessionID,
		"activity_type": "response", "activity_id": testActivityID, "body": "hello",
	})
	if result.IsError || !reviewed || len(*calls) != 1 {
		t.Fatalf("approved activity: result=%v reviewed=%t calls=%v", result.Content, reviewed, *calls)
	}
}

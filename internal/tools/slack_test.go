package tools

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"jarvis/internal/core"
)

type slackCallRecord struct {
	method string
	form   url.Values
}

// slackFixture answers Slack Web API calls from handlers keyed by method and
// records every call made.
func slackFixture(t *testing.T, mode string, handlers map[string]func(form url.Values, attempt int) *http.Response) (*Executor, *[]slackCallRecord) {
	t.Helper()
	e, _ := fixture(t, mode, nil)
	e.config.SlackToken, e.config.SlackOwner = "xoxb-test", "U0OWNER"
	var calls []slackCallRecord
	attempts := map[string]int{}
	e.webClient = newWebSearchClient()
	e.webClient.Transport = webRoundTrip(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodPost || req.URL.Host != "slack.com" || req.Header.Get("Authorization") != "Bearer xoxb-test" {
			t.Errorf("unexpected request %s %s auth %q", req.Method, req.URL, req.Header.Get("Authorization"))
		}
		method := strings.TrimPrefix(req.URL.Path, "/api/")
		body, _ := io.ReadAll(req.Body)
		form, _ := url.ParseQuery(string(body))
		calls = append(calls, slackCallRecord{method, form})
		handler, ok := handlers[method]
		if !ok {
			t.Errorf("unexpected Slack method %s", method)
			return webResponse(http.StatusNotFound, "application/json", `{"ok":false,"error":"unknown_method"}`), nil
		}
		attempts[method]++
		return handler(form, attempts[method]), nil
	})
	return e, &calls
}

func slackOK(body string) func(url.Values, int) *http.Response {
	return func(url.Values, int) *http.Response { return webResponse(http.StatusOK, "application/json", body) }
}

func TestSlackOfferedOnlyWithToken(t *testing.T) {
	t.Setenv(SlackTokenEnv, "")
	if names := toolNames(Definitions(nil)); strings.Contains(strings.Join(names, " "), "Slack") {
		t.Fatalf("Slack offered without a token: %v", names)
	}
	t.Setenv(SlackTokenEnv, "xoxb-test")
	t.Setenv(SlackOwnerEnv, "U0OWNER")
	definitions := Definitions(nil)
	last := definitions[len(definitions)-1]
	if last.Name != "Slack" || !strings.Contains(last.Parameters["properties"].(map[string]any)["channel"].(map[string]any)["description"].(string), "U0OWNER") {
		t.Fatalf("Slack definition %+v", last)
	}
	e, _ := fixture(t, "auto", nil)
	if names := toolNames(e.Definitions()); strings.Contains(strings.Join(names, " "), "Slack") {
		t.Fatalf("executor without a token offers Slack: %v", names)
	}
	if r := invoke(e, "Slack", map[string]any{"action": "list_channels"}); !r.IsError || !strings.Contains(r.Content, SlackTokenEnv) {
		t.Fatalf("unconfigured Slack: %+v", r)
	}
}

func TestSlackPostResolvesUsersAndIsReviewed(t *testing.T) {
	e, calls := slackFixture(t, "auto", map[string]func(url.Values, int) *http.Response{
		"conversations.open": slackOK(`{"ok":true,"channel":{"id":"D0DM"}}`),
		"chat.postMessage":   slackOK(`{"ok":true,"channel":"D0DM","ts":"1700000000.000100"}`),
	})
	var reviewed []core.Approval
	e.config.Review = func(_ context.Context, a core.Approval) (core.Decision, error) {
		reviewed = append(reviewed, a)
		return core.Decision{Allow: true, Reason: "owner asked"}, nil
	}
	r := invoke(e, "Slack", map[string]any{"action": "post_message", "channel": "U0OWNER", "text": "Done, sir.", "thread_ts": "1699999999.000001"})
	if r.IsError || r.Content != "Posted to channel D0DM with ts 1700000000.000100." {
		t.Fatalf("post: %+v", r)
	}
	if len(reviewed) != 1 || reviewed[0].ExecutionMode != "slack" || !reviewed[0].Network || !strings.Contains(string(reviewed[0].Call.Arguments), "Done, sir.") {
		t.Fatalf("review %+v", reviewed)
	}
	if len(*calls) != 2 || (*calls)[0].method != "conversations.open" || (*calls)[0].form.Get("users") != "U0OWNER" {
		t.Fatalf("calls %+v", *calls)
	}
	if post := (*calls)[1]; post.method != "chat.postMessage" || post.form.Get("channel") != "D0DM" || post.form.Get("thread_ts") != "1699999999.000001" || post.form.Get("text") != "Done, sir." {
		t.Fatalf("post call %+v", post)
	}
	// The review happens before any call: a denied post never opens a DM.
	e.config.Review = func(context.Context, core.Approval) (core.Decision, error) {
		return core.Decision{Allow: false, Reason: "not asked for"}, nil
	}
	if r := invoke(e, "Slack", map[string]any{"action": "post_message", "channel": "C0GENERAL", "text": "hello"}); !r.IsError || !strings.Contains(r.Content, "denied") || len(*calls) != 2 {
		t.Fatalf("denied post: %+v calls %d", r, len(*calls))
	}
}

func TestSlackReadsNeedNoReviewAndFormatTranscripts(t *testing.T) {
	e, calls := slackFixture(t, "read-only", map[string]func(url.Values, int) *http.Response{
		"conversations.replies": slackOK(`{"ok":true,"messages":[{"ts":"1.000","user":"U0OWNER","text":"Look into this","reply_count":2},{"ts":"1.001","thread_ts":"1.000","bot_id":"B1","username":"jarvis","text":"On it"},{"ts":"1.002","thread_ts":"1.000","user":"U9","text":"see attached","files":[{"name":"log.txt"},{}]}],"has_more":true}`),
		"conversations.history": slackOK(`{"ok":true,"messages":[{"ts":"3.000","user":"U9","text":"newest"},{"ts":"2.000","user":"U9","text":"older"}]}`),
		"users.conversations": func(form url.Values, _ int) *http.Response {
			if form.Get("cursor") == "" {
				return webResponse(http.StatusOK, "application/json", `{"ok":true,"channels":[{"id":"C1","name":"general","is_private":false}],"response_metadata":{"next_cursor":"page2"}}`)
			}
			return webResponse(http.StatusOK, "application/json", `{"ok":true,"channels":[{"id":"G2","name":"ops","is_private":true}]}`)
		},
	})
	e.config.Review = func(context.Context, core.Approval) (core.Decision, error) {
		t.Fatal("a read was reviewed")
		return core.Decision{}, nil
	}
	r := invoke(e, "Slack", map[string]any{"action": "read_thread", "channel": "C0GENERAL", "thread_ts": "1.000", "limit": 3})
	want := "[ts 1.000] owner (U0OWNER) (2 replies):\nLook into this\n\n[ts 1.001] bot:jarvis (in thread 1.000):\nOn it\n\n[ts 1.002] U9 (in thread 1.000; files: log.txt, unnamed):\nsee attached\n\n[more replies exist; raise limit to read them]"
	if r.IsError || r.Content != want {
		t.Fatalf("thread:\n%s\nwant:\n%s", r.Content, want)
	}
	if (*calls)[0].form.Get("limit") != "3" || (*calls)[0].form.Get("ts") != "1.000" {
		t.Fatalf("replies call %+v", (*calls)[0])
	}
	if r := invoke(e, "Slack", map[string]any{"action": "read_channel", "channel": "C0GENERAL"}); r.IsError || !strings.HasPrefix(r.Content, "Channel C0GENERAL:\n\n[ts 2.000] U9:\nolder\n\n[ts 3.000]") || (*calls)[1].form.Get("limit") != "20" {
		t.Fatalf("channel: %+v", r)
	}
	if r := invoke(e, "Slack", map[string]any{"action": "list_channels"}); r.IsError || r.Content != "C1 #general\nG2 #ops (private)" {
		t.Fatalf("channels: %+v", r)
	}
	for _, args := range []map[string]any{
		{"action": "post_message", "channel": "C0GENERAL", "text": "hi"},
		{"action": "add_reaction", "channel": "C0GENERAL", "timestamp": "1.000", "name": "eyes"},
	} {
		if r := invoke(e, "Slack", args); !r.IsError || !strings.Contains(r.Content, "read-only") {
			t.Fatalf("read-only %v: %+v", args, r)
		}
	}
}

func TestSlackValidatesArgumentsAndReportsErrors(t *testing.T) {
	e, calls := slackFixture(t, "yolo", map[string]func(url.Values, int) *http.Response{
		"reactions.add": func(form url.Values, attempt int) *http.Response {
			switch {
			case form.Get("name") == "eyes" && attempt == 1:
				r := webResponse(http.StatusTooManyRequests, "application/json", `{"ok":false,"error":"ratelimited"}`)
				r.Header.Set("Retry-After", "0")
				return r
			case form.Get("name") == "eyes":
				return webResponse(http.StatusOK, "application/json", `{"ok":false,"error":"already_reacted"}`)
			case form.Get("name") == "tada":
				return webResponse(http.StatusOK, "application/json", `{"ok":false,"error":"missing_scope","needed":"reactions:write"}`)
			}
			return webResponse(http.StatusInternalServerError, "text/html", "<html>")
		},
	})
	for _, args := range []map[string]any{
		{"action": "dance"},
		{"action": "post_message", "channel": "general", "text": "hi"},
		{"action": "post_message", "channel": "C0GENERAL", "text": "  "},
		{"action": "post_message", "channel": "C0GENERAL", "text": "hi", "thread_ts": "yesterday"},
		{"action": "add_reaction", "channel": "C0GENERAL", "timestamp": "1.000", "name": "Eyes!"},
		{"action": "read_thread", "channel": "C0GENERAL", "thread_ts": "1.000", "limit": 500},
		{"action": "read_channel", "channel": "C0GENERAL", "oldest": "now"},
	} {
		if r := invoke(e, "Slack", args); !r.IsError || len(*calls) != 0 {
			t.Fatalf("accepted %v: %+v", args, r)
		}
	}
	// A rate limit is waited out once; an existing reaction is no failure.
	if r := invoke(e, "Slack", map[string]any{"action": "add_reaction", "channel": "C0GENERAL", "timestamp": "1.000", "name": ":eyes:"}); r.IsError || r.Content != "Reacted with :eyes:." || len(*calls) != 2 {
		t.Fatalf("reaction: %+v calls %d", r, len(*calls))
	}
	if r := invoke(e, "Slack", map[string]any{"action": "add_reaction", "channel": "C0GENERAL", "timestamp": "1.000", "name": "tada"}); !r.IsError || r.Content != "slack reactions.add: missing_scope (needs scope reactions:write)" {
		t.Fatalf("scope error: %+v", r)
	}
	if r := invoke(e, "Slack", map[string]any{"action": "add_reaction", "channel": "C0GENERAL", "timestamp": "1.000", "name": "x"}); !r.IsError || r.Content != "slack reactions.add: HTTP 500" {
		t.Fatalf("http error: %+v", r)
	}
}

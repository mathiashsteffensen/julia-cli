package ui

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"julia/internal/core"
)

func TestSlackSummary(t *testing.T) {
	for args, want := range map[string]string{
		`{"action":"post_message","channel":"general","text":"Deploy is out.\nThanks all."}`:            `post_message · #general · "Deploy is out. ↵ Thanks all."`,
		`{"action":"post_message","channel":"C0123456789","text":"hi","thread_ts":"1700000000.000100"}`: `post_message · C0123456789 · "hi" · in thread 1700000000.000100`,
		`{"action":"add_reaction","channel":"#ops","timestamp":"1.2","name":":eyes:"}`:                  `add_reaction · #ops · :eyes: · on 1.2`,
		`{"action":"read_thread","channel":"ops","thread_ts":"1.2","limit":20}`:                         `read_thread · #ops · thread 1.2 · limit 20`,
		`{"action":"read_channel","channel":"ops","oldest":"1.0","limit":5}`:                            `read_channel · #ops · since 1.0 · limit 5`,
		`{"action":"list_channels"}`: `list_channels`,
	} {
		if got := toolSummary(core.ToolCall{Name: "Slack", Arguments: json.RawMessage(args)}); got != want {
			t.Errorf("%s\n got %q\nwant %q", args, got, want)
		}
	}
	if got := toolSummary(core.ToolCall{Name: "Slack", Arguments: json.RawMessage(`{"channel":"ops"}`)}); got != "" {
		t.Errorf("no action: %q", got)
	}
}

func TestLinearSummary(t *testing.T) {
	for args, want := range map[string]string{
		`{"action":"list_teams"}`:                 `list_teams`,
		`{"action":"list_projects","team":"ENG"}`: `list_projects · ENG`,
		`{"action":"list_issues","team":"ENG","state":"In Progress","assignee":"me","limit":10}`:               `list_issues · ENG · state In Progress · assignee me · limit 10`,
		`{"action":"read_issue","identifier":"ENG-123"}`:                                                       `read_issue · ENG-123`,
		`{"action":"create_issue","team":"ENG","title":"Login\nfails","description":"long","labels":"Bug"}`:    `create_issue · ENG · "Login ↵ fails" · labels Bug`,
		`{"action":"update_issue","identifier":"ENG-123","state":"Done","labels":"Bug","replace_labels":true}`: `update_issue · ENG-123 · state Done · labels = Bug`,
		`{"action":"add_comment","identifier":"ENG-123","body":"Fixed in #42"}`:                                `add_comment · ENG-123 · "Fixed in #42"`,
		`{"action":"create_label","name":"Flaky","color":"#ff0000"}`:                                           `create_label · "Flaky" · workspace · #ff0000`,
		`{"action":"create_label","name":"Flaky","team":"ENG"}`:                                                `create_label · "Flaky" · ENG`,
	} {
		if got := toolSummary(core.ToolCall{Name: "Linear", Arguments: json.RawMessage(args)}); got != want {
			t.Errorf("%s\n got %q\nwant %q", args, got, want)
		}
	}
}

func TestActionFallbackSummary(t *testing.T) {
	if got := toolSummary(core.ToolCall{Name: "Future", Arguments: json.RawMessage(`{"action":"frobnicate","count":3}`)}); got != "frobnicate" {
		t.Errorf("fallback = %q", got)
	}
}

func TestCallArguments(t *testing.T) {
	got := callArguments(json.RawMessage(`{"action":"create_issue","title":"A","priority":2,"urgent":true,"labels":["a","b"],"none":null}`))
	want := []argument{{"action", "create_issue"}, {"title", "A"}, {"priority", "2"}, {"urgent", "true"}, {"labels", `["a","b"]`}, {"none", "null"}}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("argument %d = %v, want %v", i, got[i], want[i])
		}
	}
	if got := callArguments(json.RawMessage(`{"action":`)); len(got) != 1 || got[0].key != "arguments" {
		t.Errorf("malformed: %v", got)
	}
	if got := callArguments(nil); got != nil {
		t.Errorf("empty: %v", got)
	}
}

func TestExpandedServiceArguments(t *testing.T) {
	call := core.ToolCall{Name: "Linear", Arguments: json.RawMessage(`{"action":"create_issue","team":"ENG","title":"Login fails","description":"Steps:\n1. open the app\n2. sign in with SSO and watch it bounce back to the login page every single time"}`)}
	for _, expanded := range []bool{false, true} {
		m := testModel(t, Config{})
		m.resize(60, 30)
		m.expandTools = expanded
		m.entries = append(m.entries, entry{role: "tool", call: &call, done: true, result: &core.ToolResult{Content: "ENG-124 created"}})
		m.refresh(false)
		view := ansi.Strip(m.View().Content)
		if !strings.Contains(view, `create_issue · ENG · "Login fails"`) {
			t.Fatalf("expanded=%v: title row missing:\n%s", expanded, view)
		}
		if strings.Contains(view, "description") != expanded || strings.Contains(view, "2. sign in with SSO") != expanded {
			t.Fatalf("expanded=%v: arguments shown wrongly:\n%s", expanded, view)
		}
	}
}

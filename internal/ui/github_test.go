package ui

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"julia/internal/core"
)

func TestGitHubSummary(t *testing.T) {
	for args, want := range map[string]string{
		`{"path":"/repos/owner/repo/issues"}`:                                                            `GET · /repos/owner/repo/issues`,
		`{"method":"GET","path":"/repos/owner/repo/issues?state=all","accept":"raw"}`:                    `GET · /repos/owner/repo/issues?state=all`,
		`{"method":"POST","path":"/repos/owner/repo/issues","body":"{\"title\":\"A\"}","accept":"json"}`: `POST · /repos/owner/repo/issues`,
	} {
		got := toolSummary(core.ToolCall{Name: "GitHub", Arguments: json.RawMessage(args)})
		if got != want {
			t.Errorf("%s: got %q, want %q", args, got, want)
		}
	}
	for _, args := range []string{`{}`, `{"method":"GET"}`, `{"path":`} {
		if summary, ok := githubSummary(json.RawMessage(args)); ok {
			t.Errorf("%s: unexpected summary %q", args, summary)
		}
	}
}

func TestGitHubExpandedArguments(t *testing.T) {
	body := "{\n  \"title\": \"An issue\",\n  \"body\": \"First line\\nSecond line with a complete ending\"\n}"
	args, err := json.Marshal(map[string]string{
		"method": "POST", "path": "/repos/owner/repo/issues", "body": body, "accept": "json",
	})
	if err != nil {
		t.Fatal(err)
	}
	call := core.ToolCall{Name: "GitHub", Arguments: args}
	for _, expanded := range []bool{false, true} {
		m := testModel(t, Config{})
		m.resize(80, 30)
		m.expandTools = expanded
		view := ansi.Strip(m.renderEntryContent(entry{role: "tool", call: &call, done: true}))
		if !strings.Contains(view, "POST · /repos/owner/repo/issues") {
			t.Fatalf("expanded=%v: summary missing:\n%s", expanded, view)
		}
		for _, value := range []string{"method", "path", "body", "accept", "An issue", "First line", "complete ending", "json"} {
			if strings.Contains(view, value) != expanded {
				t.Errorf("expanded=%v: %q shown wrongly:\n%s", expanded, value, view)
			}
		}
		if expanded {
			for _, line := range strings.Split(body, "\n") {
				if !strings.Contains(view, strings.TrimSpace(line)) {
					t.Errorf("body line %q missing:\n%s", line, view)
				}
			}
		}
	}
}

func TestGitHubUnsafeInputAndLongPath(t *testing.T) {
	path := "/repos/" + strings.Repeat("long-segment/", 12) + "issues?state=all"
	args, err := json.Marshal(map[string]string{
		"method": "GET\x1b[2J", "path": path + "\x1b]52;c;evil\x07\u202e", "body": "safe\x1b[2J\x07\u202eevil",
	})
	if err != nil {
		t.Fatal(err)
	}
	call := core.ToolCall{Name: "GitHub", Arguments: args}
	m := testModel(t, Config{})
	m.resize(50, 40)
	m.expandTools = true
	raw := m.renderEntryContent(entry{role: "tool", call: &call, done: true})
	for _, unsafe := range []string{"\x1b[2J", "\x1b]52", "\x07", "\u202e"} {
		if strings.Contains(raw, unsafe) {
			t.Errorf("unsafe control %q in rendered output", unsafe)
		}
	}
	view := ansi.Strip(raw)
	if !strings.Contains(view, "GET · /repos/") || !strings.Contains(view, "…") {
		t.Errorf("long title not truncated:\n%s", view)
	}
	if !strings.Contains(strings.Join(strings.Fields(view), ""), path) {
		t.Errorf("expanded path incomplete:\n%s", view)
	}
	if !strings.Contains(view, "safeevil") {
		t.Errorf("sanitized body missing:\n%s", view)
	}
}

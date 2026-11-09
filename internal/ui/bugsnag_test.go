package ui

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"julia/internal/core"
)

func TestBugsnagCallShowsGETPathAndExpandedOffset(t *testing.T) {
	call := core.ToolCall{Name: "Bugsnag", Arguments: json.RawMessage(`{"path":"/projects/project-id/errors?per_page=10","offset":32768}`)}
	want := "GET · /projects/project-id/errors?per_page=10"
	if got := toolSummary(call); got != want {
		t.Fatalf("summary %q, want %q", got, want)
	}

	m := testModel(t, Config{})
	m.resize(100, 30)
	item := entry{role: "tool", call: &call, done: true}
	collapsed := ansi.Strip(m.renderEntryContent(item))
	if !strings.Contains(collapsed, "Bugsnag") || !strings.Contains(collapsed, want) || strings.Contains(collapsed, "32768") {
		t.Fatalf("collapsed call:\n%s", collapsed)
	}

	m.expandTools = true
	expanded := ansi.Strip(m.renderEntryContent(item))
	for _, value := range []string{want, "path", "offset", "32768"} {
		if !strings.Contains(expanded, value) {
			t.Errorf("expanded call lacks %q:\n%s", value, expanded)
		}
	}
}

func TestBugsnagLongPathIsSanitizedAndFullyExpanded(t *testing.T) {
	path := "/projects/project-id/errors?filters[error.status][]=" + strings.Repeat("open", 25)
	arguments, err := json.Marshal(map[string]any{"path": path + "\x1b]52;c;evil\x07\u202e", "offset": 32768})
	if err != nil {
		t.Fatal(err)
	}
	call := core.ToolCall{Name: "Bugsnag", Arguments: arguments}
	m := testModel(t, Config{})
	m.resize(50, 40)
	m.expandTools = true
	rendered := m.renderEntryContent(entry{role: "tool", call: &call, done: true})
	for _, unsafe := range []string{"\x1b]52", "\x07", "\u202e"} {
		if strings.Contains(rendered, unsafe) {
			t.Errorf("unsafe control %q in rendered output", unsafe)
		}
	}
	view := ansi.Strip(rendered)
	if !strings.Contains(view, "GET · /projects/") || !strings.Contains(view, "…") {
		t.Fatalf("long path lacks truncated summary:\n%s", view)
	}
	if !strings.Contains(strings.Join(strings.Fields(view), ""), path) {
		t.Fatalf("expanded path incomplete:\n%s", view)
	}
}

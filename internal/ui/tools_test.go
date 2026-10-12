package ui

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"jarvis/agents"
	"jarvis/internal/core"
	"jarvis/internal/tools"
)

func TestWebSearchToolSummary(t *testing.T) {
	for _, tc := range []struct {
		name string
		args string
		want string
	}{
		{name: "query", args: `{"query":"Go release notes"}`, want: "Go release notes"},
		{name: "limit", args: `{"query":"Go release notes","limit":3}`, want: "Go release notes"},
		{name: "multiline", args: `{"query":"Go\nreleases"}`, want: "Go ↵ releases"},
		{name: "empty", args: `{"query":""}`},
		{name: "missing", args: `{"limit":3}`},
		{name: "non-string", args: `{"query":42}`},
		{name: "malformed", args: `{"query":`, want: `{"query":`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			call := core.ToolCall{Name: "WebSearch", Arguments: json.RawMessage(tc.args)}
			if got := toolSummary(call); got != tc.want {
				t.Fatalf("summary = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestWebSearchCollapsedRow(t *testing.T) {
	for _, tc := range []struct {
		name      string
		query     string
		want      string
		truncated bool
	}{
		{name: "query", query: "Go release notes", want: "Go release notes"},
		{name: "multiline", query: "Go\nreleases", want: "Go ↵ releases"},
		{
			name:  "untrusted",
			query: "Go\x1b]52;c;Y2xpcGJvYXJk\a\x1b[2J\u202e\u2066\r\b\x00 release notes",
			want:  "Go release notes",
		},
		{name: "long", query: "Go " + strings.Repeat("界", 100), want: "Go 界", truncated: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args, err := json.Marshal(map[string]any{"query": tc.query, "limit": 3})
			if err != nil {
				t.Fatal(err)
			}
			for _, width := range []int{40, 80} {
				m := testModel(t, Config{})
				m.resize(width, 20)
				item := entry{
					role:   "tool",
					done:   true,
					call:   &core.ToolCall{Name: "WebSearch", Arguments: args},
					result: &core.ToolResult{Content: "Found 3 results\nHidden result detail"},
				}
				rendered := m.renderEntryContent(item)
				for _, unsafe := range []string{"\x1b]52", "\x1b[2J", "\u202e", "\u2066", "\r", "\b", "\x00", "\a"} {
					if strings.Contains(rendered, unsafe) {
						t.Fatalf("width %d: unsafe sequence %q in %q", width, unsafe, rendered)
					}
				}
				rows := strings.Split(ansi.Strip(rendered), "\n")
				if len(rows) != 2 {
					t.Fatalf("width %d: collapsed tool has %d rows: %q", width, len(rows), rows)
				}
				if !strings.Contains(rows[0], "WebSearch") || !strings.Contains(rows[0], tc.want) {
					t.Fatalf("width %d: missing tool name or query in %q", width, rows[0])
				}
				if tc.truncated && !strings.HasSuffix(rows[0], "…") {
					t.Fatalf("width %d: long query lacks truncation marker: %q", width, rows[0])
				}
				if !strings.Contains(rows[1], "Found 3 results") || strings.Contains(rendered, "Hidden result detail") {
					t.Fatalf("width %d: unexpected collapsed result: %q", width, rendered)
				}
				for _, row := range rows {
					if ansi.StringWidth(row) > m.mainWidth {
						t.Fatalf("width %d: row exceeds content width %d: %q", width, m.mainWidth, row)
					}
				}
			}
		})
	}
}

// A session from before roles delegated bare prompts.
func TestSubagentCallShowsTaskCountAndFullPromptsWhenExpanded(t *testing.T) {
	tasks := []string{
		"Inspect the caller and report the exact regression.",
		"Review concurrency.\nPreserve ordering and include test names.",
	}
	arguments, err := json.Marshal(map[string]any{"tasks": tasks})
	if err != nil {
		t.Fatal(err)
	}
	call := core.ToolCall{Name: "Subagent", Arguments: arguments}
	if got := toolSummary(call); got != "2 tasks" {
		t.Fatalf("summary = %q, want task count", got)
	}
	singleTask, err := json.Marshal(map[string]any{"tasks": []string{tasks[0]}})
	if err != nil {
		t.Fatal(err)
	}
	if got := toolSummary(core.ToolCall{Name: "Subagent", Arguments: singleTask}); got != "1 task" {
		t.Fatalf("single-task summary = %q, want 1 task", got)
	}

	m := testModel(t, Config{})
	m.resize(80, 20)
	item := entry{role: "tool", call: &call}
	collapsed := ansi.Strip(m.renderEntryContent(item))
	if !strings.Contains(collapsed, "2 tasks") || strings.Contains(collapsed, "Inspect the caller") {
		t.Fatalf("collapsed subagent card should show count, not prompt bodies: %q", collapsed)
	}

	m.expandTools = true
	expanded := ansi.Strip(m.renderEntryContent(item))
	for _, want := range []string{
		"Task 1", tasks[0], "Task 2", "Review concurrency.",
		"Preserve ordering and include test names.",
	} {
		if !strings.Contains(expanded, want) {
			t.Errorf("expanded subagent card is missing %q: %q", want, expanded)
		}
	}
}

func TestSubagentCallNamesEachTasksRole(t *testing.T) {
	tasks := []map[string]string{
		{"role": "scout", "task": "Map the config loader."},
		{"role": "reviewer", "task": "Review the diff."},
		{"role": "scout", "task": "Map the UI."},
	}
	arguments, err := json.Marshal(map[string]any{"tasks": tasks})
	if err != nil {
		t.Fatal(err)
	}
	call := core.ToolCall{Name: "Subagent", Arguments: arguments}
	if got := toolSummary(call); got != "3 tasks · scout ×2, reviewer" {
		t.Fatalf("summary = %q, want the count and the roles", got)
	}
	one, _ := json.Marshal(map[string]any{"tasks": tasks[1:2]})
	if got := toolSummary(core.ToolCall{Name: "Subagent", Arguments: one}); got != "1 task · reviewer" {
		t.Fatalf("single-task summary = %q", got)
	}
	m := testModel(t, Config{})
	m.resize(80, 20)
	m.expandTools = true
	expanded := ansi.Strip(m.renderEntryContent(entry{role: "tool", call: &call}))
	for _, want := range []string{"Task 1 · scout", "Map the config loader.", "Task 2 · reviewer", "Task 3 · scout", "Map the UI."} {
		if !strings.Contains(expanded, want) {
			t.Errorf("expanded subagent card is missing %q: %q", want, expanded)
		}
	}
}

// A role the model invented, newlines and all, stays on its card's one row.
func TestSubagentRoleIsOneLine(t *testing.T) {
	call := core.ToolCall{Name: "Subagent", Arguments: json.RawMessage(`{"tasks":[{"role":"scout\nINJECTED\tx","task":"Map it."}]}`)}
	if got := toolSummary(call); got != "1 task · scout INJECTED x" {
		t.Fatalf("summary = %q", got)
	}
	m := testModel(t, Config{})
	m.resize(80, 20)
	for _, expand := range []bool{false, true} {
		m.expandTools = expand
		layout := m.layoutEntry(entry{role: "tool", call: &call})
		if lines := strings.Split(strings.TrimSuffix(layout.text, "\n"), "\n"); len(lines) != len(layout.rows) {
			t.Fatalf("expand=%v: %d lines for %d rows: %q", expand, len(lines), len(layout.rows), layout.text)
		}
	}
}

func TestSidebarFitsRegisteredTools(t *testing.T) {
	for _, provider := range []string{"codex", "anthropic"} {
		for _, height := range []int{24, 56} {
			definitions := tools.Definitions(agents.Builtin())
			m := testModel(t, Config{
				Session:         &core.Session{Provider: provider},
				ToolDefinitions: definitions,
			})
			m.resize(128, height)
			m.scrollSidebar(1000) // the essentials close the sidebar
			view := ansi.Strip(m.sidebar())
			for _, definition := range definitions {
				if !strings.Contains(view, definition.Name) {
					t.Errorf("%s at height %d: missing %s in %s", provider, height, definition.Name, view)
				}
			}
		}
	}
}

func TestSidebarUsesSuppliedRegistry(t *testing.T) {
	m := testModel(t, Config{ToolDefinitions: []core.ToolDefinition{{Name: "CustomSearch"}, {Name: "Delegate"}}})
	m.resize(128, 52)
	m.height = 200 // the essentials close the sidebar; show the whole column
	view := ansi.Strip(m.sidebar())
	for _, name := range []string{"CustomSearch", "Delegate"} {
		if !strings.Contains(view, name) {
			t.Fatalf("missing registered tool %s: %s", name, view)
		}
	}
	if strings.Contains(view, "MultiEdit") || strings.Contains(view, "Subagent") {
		t.Fatal("sidebar contains unregistered tools")
	}
	m.cfg.ToolDefinitions = []core.ToolDefinition{{Name: "Replacement"}}
	view = ansi.Strip(m.sidebar())
	if !strings.Contains(view, "Replacement") || strings.Contains(view, "Delegate") {
		t.Fatal("sidebar retained stale tools")
	}
	m.cfg.ToolDefinitions = nil
	if strings.Contains(ansi.Strip(m.sidebar()), "THE ESSENTIALS") {
		t.Fatal("empty registry shows tools section")
	}
}

func TestToolRowsFitAndPreserveNames(t *testing.T) {
	definitions := []core.ToolDefinition{{Name: "CustomSearch"}, {Name: "Delegate"}, {Name: "Review"}, {Name: "Inspect"}}
	for _, width := range []int{14, 23, 40} {
		rows := toolRows(definitions, width)
		if strings.Join(strings.Fields(strings.Join(rows, " ")), " ") != "CustomSearch Delegate Review Inspect" {
			t.Fatalf("lost names: %v", rows)
		}
		for _, row := range rows {
			if ansi.StringWidth(row) > width {
				t.Fatalf("row exceeds %d: %s", width, row)
			}
		}
	}
	for _, row := range toolRows([]core.ToolDefinition{{Name: strings.Repeat("界", 30)}}, 23) {
		if ansi.StringWidth(row) > 23 {
			t.Fatal("long tool name overflowed")
		}
	}
}

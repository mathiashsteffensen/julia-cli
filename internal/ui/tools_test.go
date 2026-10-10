package ui

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
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

func TestSidebarFitsRegisteredTools(t *testing.T) {
	for _, provider := range []string{"codex", "anthropic"} {
		// The essentials rank below every other sidebar section, so they need a
		// terminal tall enough for the context, token and activity sections too.
		for _, height := range []int{48, 49, 50} {
			definitions := tools.Definitions(true)
			m := testModel(t, Config{
				Session:         &core.Session{Provider: provider},
				ToolDefinitions: definitions,
			})
			m.resize(128, height)
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
	m.resize(128, 52) // the essentials rank below every other section, so they need a tall terminal
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

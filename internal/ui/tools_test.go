package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"jarvis/internal/core"
)

func TestSidebarUsesSuppliedRegistry(t *testing.T) {
	m := testModel(t, Config{ToolDefinitions: []core.ToolDefinition{{Name: "CustomSearch"}, {Name: "Delegate"}}})
	m.resize(128, 40)
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

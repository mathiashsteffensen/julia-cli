package ui

import (
	"regexp"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"julia/internal/core"
)

// Opening the fleet closes any picker, and the picker's loading animation
// shares the spinner with the task's activity stream. A running agent's stream
// keeps moving across the open window, and the conversation's does after it
// closes.
func TestFleetKeepsTheActivityStreamMoving(t *testing.T) {
	m := testModel(t, Config{})
	m.resize(120, 40)
	m.entries = []entry{{role: "user", text: "Delegate."}}
	m.busy = true
	m.status = "Working"
	m.spinner.start()
	m.refresh(true)
	ref := &core.Subagent{ID: "w", Role: "worker", Model: "fake/model"}
	m.applyEvent(core.Event{Kind: "subagent_start", Text: "Edit many files.", Subagent: ref})
	m.applyEvent(core.Event{Kind: "status", Text: "Working · step 5", Subagent: ref})
	// The activity line is the stream, two spaces, then what the task is doing;
	// the agent list's own "Working · step 5 · 0s" row has no stream before it.
	activity := regexp.MustCompile(`[^ ]{3,}  Working`)
	stream := func() string {
		for _, line := range strings.Split(ansi.Strip(m.View().Content), "\n") {
			if activity.MatchString(line) {
				return line
			}
		}
		t.Fatal("no activity line on screen")
		return ""
	}
	tick := func() { m.Update(activityTickMsg{generation: m.spinner.generation, frame: m.spinner.frame + 1}) }
	m.Update(ctrlF)
	if !m.spinner.active {
		t.Fatal("opening the fleet stopped the task's spinner")
	}
	before := stream()
	tick()
	tick()
	if after := stream(); after == before {
		t.Fatalf("the fleet's stream is static:\n%s", after)
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	before = stream()
	tick()
	tick()
	if after := stream(); after == before {
		t.Fatalf("the conversation's stream is static after the fleet closes:\n%s", after)
	}
}

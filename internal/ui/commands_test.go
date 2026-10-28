package ui

import (
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"julia/internal/core"
	"julia/internal/skills"
)

func testSkills() []skills.Skill {
	return []skills.Skill{
		{Name: "code-review", Description: "Review the changes since a fixed point."},
		{Name: "implement", Description: "Implement a spec.", UserOnly: true},
		{Name: "review", Description: "A skill named like a built-in."},
	}
}

func commandNames(list []command) []string {
	var names []string
	for _, c := range list {
		names = append(names, c.name)
	}
	return names
}

func TestEverySkillIsACommandAndABuiltinKeepsItsName(t *testing.T) {
	m := testModel(t, Config{Skills: testSkills})
	m.loadSkills()
	names := commandNames(m.commands())
	for _, want := range []string{"help", "review", "code-review", "implement", "skill:review"} {
		if !slices.Contains(names, want) {
			t.Errorf("no /%s in %v", want, names)
		}
	}
	if slices.Contains(names, "ask") {
		t.Error("/ask is the preview's alone")
	}
	for prompt, want := range map[string]string{"/implement docs/plan.md": "implement", "/skill:review": "review", "/code-review\nsince main": "code-review"} {
		if got, ok := m.skillCommand(prompt); !ok || got != want {
			t.Errorf("%q runs %q, %v; want %q", prompt, got, ok, want)
		}
	}
	for _, prompt := range []string{"/review", "/implementation", "implement", "/help"} {
		if got, ok := m.skillCommand(prompt); ok {
			t.Errorf("%q ran skill %q", prompt, got)
		}
	}
}

func TestTheMenuCompletesAsYouType(t *testing.T) {
	m := testModel(t, Config{Skills: testSkills})
	m.resize(120, 40)

	typeText(m, "/")
	if !m.menuOpen() || len(m.menu.matches) != len(builtinCommands)+3 {
		t.Fatalf("a slash must list every command: %v", commandNames(m.menu.matches))
	}
	view := ansi.Strip(m.View().Content)
	for _, want := range []string{"COMMANDS", "/help", "Keys and commands"} {
		if !strings.Contains(view, want) {
			t.Fatalf("menu lacks %q:\n%s", want, view)
		}
	}

	typeText(m, "re")
	if got := commandNames(m.menu.matches); !slices.Equal(got, []string{"reasoning", "review", "skill:review", "code-review"}) {
		t.Fatalf("/re matched %v: names that start with it first, then names that contain it", got)
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	if m.menu.selected != 2 {
		t.Fatalf("selected %d", m.menu.selected)
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	if m.input.Value() != "/review " || m.menuOpen() {
		t.Fatalf("tab must complete the chosen command and close the menu: %q", m.input.Value())
	}

	m.input.Reset()
	typeText(m, "/zz")
	if m.menuOpen() {
		t.Fatal("no match, no menu")
	}
	m.input.Reset()
	typeText(m, "/he")
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.menuOpen() {
		t.Fatal("esc must close the menu")
	}
	typeText(m, "l")
	if !m.menuOpen() {
		t.Fatal("typing on must open it again")
	}
}

func TestEnterRunsTheChosenSkillAsTyped(t *testing.T) {
	backend := promptBackend{prompts: make(chan core.Prompt, 1)}
	m := testModel(t, Config{Backend: backend, Skills: testSkills})
	m.resize(120, 40)

	typeText(m, "/impl")
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	select {
	case prompt := <-backend.prompts:
		if prompt.Text != "/implement" || prompt.Skill != "implement" {
			t.Fatalf("sent %+v", prompt)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("enter did not run the chosen command")
	}
	if m.menuOpen() || m.input.Value() != "" {
		t.Fatalf("draft %q, menu open %v", m.input.Value(), m.menuOpen())
	}
	if last := m.entries[len(m.entries)-1]; last.role != "user" || last.text != "/implement" {
		t.Fatalf("the conversation shows %+v", last)
	}
}

func TestEnterOnACommandTypedInFullRunsItWithItsInput(t *testing.T) {
	backend := promptBackend{prompts: make(chan core.Prompt, 1)}
	m := testModel(t, Config{Backend: backend, Skills: testSkills})
	m.resize(120, 40)

	typeText(m, "/skill:review")
	if !m.menuOpen() {
		t.Fatal("menu closed on a full name")
	}
	typeText(m, " the auth change")
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	select {
	case prompt := <-backend.prompts:
		if prompt.Text != "/skill:review the auth change" || prompt.Skill != "review" {
			t.Fatalf("sent %+v", prompt)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the skill did not run")
	}
}

func TestAResumedSkillShowsAsItsCommand(t *testing.T) {
	session := &core.Session{ID: "s", Directory: "/tmp/julia", Provider: "fake", Messages: []core.Message{
		{Role: "user", Content: "/implement docs/plan.md\n\n<skill name=\"implement\" source=\"/x\">\nWrite the tests first.\n</skill>"},
	}}
	m := testModel(t, Config{Session: session})
	if len(m.entries) != 1 || m.entries[0].text != "/implement docs/plan.md" {
		t.Fatalf("entries %+v", m.entries)
	}
}

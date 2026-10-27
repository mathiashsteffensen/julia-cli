package ui

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"julia/internal/config"
	"julia/internal/core"
)

// The fleet is every subagent the session has run: each child's task, the
// tools it called and its report, kept as the child's own small conversation.
// Ctrl+F opens it as a window over the chat, the agents listed down the left
// and the chosen one's transcript on the right, drawn by the same renderer as
// the conversation so a tool call looks the same wherever it ran.

// agent is one subagent, running or finished.
type agent struct {
	id, role, model string
	task            string
	started, ended  time.Time
	state           string // running, done or failed
	activity        string // what a running agent is doing, from its status and tool events
	entries         []entry
	calls           int
	usage           core.TokenUsage
}

// fleetState is the open window: which agent is shown and how far its
// transcript is scrolled. A following pane keeps the newest activity in view.
type fleetState struct {
	selected int
	offset   int
	follow   bool
	below    int // rows of the transcript under the pane, set by the last render
}

const (
	agentRunning = "running"
	agentDone    = "done"
	agentFailed  = "failed"
)

// agentFor finds the agent an event belongs to, opening it if its start was
// missed, so a child is never silently dropped.
func (m *model) agentFor(ref *core.Subagent) *agent {
	for _, a := range m.fleet {
		if a.id == ref.ID {
			return a
		}
	}
	a := &agent{id: ref.ID, role: ref.Role, model: ref.Model, started: time.Now(), state: agentRunning, activity: "Starting"}
	m.fleet = append(m.fleet, a)
	return a
}

// applyAgentEvent records a subagent's event against its agent. Nothing a
// child does appears in the conversation; its usage is not counted either,
// since the parent's aggregate event carries it when the child finishes.
func (m *model) applyAgentEvent(event core.Event) {
	a := m.agentFor(event.Subagent)
	switch event.Kind {
	case "subagent_start":
		a.task = event.Text
		a.started = time.Now()
		a.state, a.activity = agentRunning, "Starting"
		a.entries = []entry{{role: "user", label: "TASK", text: event.Text}}
	case "subagent_end":
		a.ended = time.Now()
		a.state, a.activity = agentDone, ""
		if event.Text != "" {
			a.state = agentFailed
			a.entries = append(a.entries, entry{role: "warning", text: event.Text})
		}
	case "text":
		if len(a.entries) == 0 || a.entries[len(a.entries)-1].role != "assistant" {
			a.entries = append(a.entries, entry{role: "assistant", label: "✦ " + a.role})
		}
		a.entries[len(a.entries)-1].text += event.Text
		a.entries[len(a.entries)-1].revision++
		a.activity = "Working"
	case "status":
		a.activity = event.Text
	case "warning":
		a.entries = append(a.entries, entry{role: "warning", text: event.Text})
	case "tool_start":
		a.calls++
		a.entries = append(a.entries, entry{role: "tool", call: event.Call, text: event.Text})
		if event.Call != nil {
			a.activity = "Running " + event.Call.Name
		}
	case "tool_end":
		for i := len(a.entries) - 1; i >= 0; i-- {
			if a.entries[i].role == "tool" && !a.entries[i].done && (event.Call == nil || a.entries[i].call != nil && a.entries[i].call.ID == event.Call.ID) {
				a.entries[i].done = true
				a.entries[i].result = event.Result
				a.entries[i].revision++
				break
			}
		}
		a.activity = "Thinking"
	case "usage":
		a.usage.Add(event.Usage())
	}
}

// loadFleet rebuilds the fleet of a resumed session from its children's saved
// transcripts, oldest first.
func (m *model) loadFleet() {
	if m.cfg.Children == nil || m.cfg.Session == nil || m.cfg.Session.ID == "" {
		return
	}
	children, err := m.cfg.Children(m.cfg.Session.ID)
	if err != nil {
		m.entries = append(m.entries, entry{role: "warning", text: "Could not load this session's subagents: " + err.Error()})
		return
	}
	slices.SortFunc(children, func(a, b *core.Session) int { return a.CreatedAt.Compare(b.CreatedAt) })
	for _, child := range children {
		a := &agent{id: child.ID, role: child.Role, model: config.JoinModel(child.Provider, child.Model), started: child.CreatedAt, ended: child.UpdatedAt, state: agentDone, usage: child.Usage.Total}
		if a.role == "" {
			// Before roles were saved on their own, the title carried them.
			if rest, ok := strings.CutPrefix(child.Title, "Subagent "); ok {
				a.role, _, _ = strings.Cut(rest, ":")
			}
		}
		for _, message := range child.Messages {
			switch message.Role {
			case "user":
				if a.task == "" {
					a.task = message.Content
					a.entries = append(a.entries, entry{role: "user", label: "TASK", text: message.Content})
				}
			case "assistant":
				if message.Content != "" {
					a.entries = append(a.entries, entry{role: "assistant", label: "✦ " + a.role, text: message.Content})
				}
				for _, call := range message.ToolCalls {
					call := call
					a.entries = append(a.entries, entry{role: "tool", call: &call, done: true})
					a.calls++
				}
			case "tool":
				for i := len(a.entries) - 1; i >= 0; i-- {
					if a.entries[i].call != nil && a.entries[i].call.ID == message.ToolCallID {
						a.entries[i].result = &core.ToolResult{Content: message.Content}
						break
					}
				}
			}
		}
		m.fleet = append(m.fleet, a)
	}
}

// fleetCounts is how many agents are running, done and failed.
func (m *model) fleetCounts() (running, done, failed int) {
	for _, a := range m.fleet {
		switch a.state {
		case agentRunning:
			running++
		case agentFailed:
			failed++
		default:
			done++
		}
	}
	return running, done, failed
}

// fleetSummary reads "1 running · 2 done · 1 failed", naming only the states
// present.
func (m *model) fleetSummary() string {
	running, done, failed := m.fleetCounts()
	var parts []string
	for _, part := range []struct {
		n    int
		name string
	}{{running, "running"}, {done, "done"}, {failed, "failed"}} {
		if part.n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", part.n, part.name))
		}
	}
	return strings.Join(parts, " · ")
}

// openFleet shows the window on the agent that matters most: the first one
// still running, else the latest.
func (m *model) openFleet() tea.Cmd {
	m.closePicker()
	selected := max(0, len(m.fleet)-1)
	for i, a := range m.fleet {
		if a.state == agentRunning {
			selected = i
			break
		}
	}
	m.fleetView = &fleetState{}
	m.selectAgent(selected)
	m.input.Blur()
	m.notice = ""
	return nil
}

func (m *model) closeFleet() tea.Cmd {
	m.fleetView = nil
	return m.input.Focus()
}

// selectAgent shows agent i from the top when it has finished, or following
// its newest activity while it runs.
func (m *model) selectAgent(i int) {
	if m.fleetView == nil {
		return
	}
	m.fleetView.selected = max(0, min(i, len(m.fleet)-1))
	m.fleetView.offset = 0
	m.fleetView.follow = len(m.fleet) > 0 && m.fleet[m.fleetView.selected].state == agentRunning
}

func (m *model) updateFleet(key tea.KeyPressMsg) tea.Cmd {
	v := m.fleetView
	switch key.String() {
	case "esc", "ctrl+f":
		return m.closeFleet()
	case "up", "shift+tab":
		m.selectAgent(v.selected - 1)
	case "down", "tab":
		m.selectAgent(v.selected + 1)
	case "pgup":
		m.scrollFleet(-max(1, m.fleetLayout().rows/2))
	case "pgdown":
		m.scrollFleet(max(1, m.fleetLayout().rows/2))
	case "ctrl+home", "home":
		v.offset, v.follow = 0, false
	case "ctrl+end", "end":
		v.follow = true
	case "ctrl+o":
		m.expandTools = !m.expandTools
		m.refresh(false)
	}
	return nil
}

// scrollFleet moves the transcript by delta rows. Scrolling up stops following
// the newest activity; reaching the bottom again resumes it.
func (m *model) scrollFleet(delta int) {
	v := m.fleetView
	if v == nil {
		return
	}
	if v.follow {
		v.offset = v.offset + v.below // the following pane sits at the bottom
	}
	v.offset = max(0, v.offset+delta)
	v.follow = delta > 0 && v.below <= delta
}

// clickFleet selects the agent under a click on the list.
func (m *model) clickFleet(msg tea.MouseClickMsg) tea.Cmd {
	if msg.Button != tea.MouseLeft || m.fleetView == nil {
		return nil
	}
	l := m.fleetLayout()
	x, y := msg.X-m.margin()-2, msg.Y-3-l.top
	if l.list == 0 || x < 0 || x >= l.list || y < 0 || y >= l.rows {
		return nil
	}
	if i := l.start + y/l.rowHeight; i < len(m.fleet) {
		m.selectAgent(i)
	}
	return nil
}

// fleetLayout is the window's geometry: what fits inside the frame, how the
// columns share it, and which list rows are on screen.
type fleetLayout struct {
	inner, rows      int  // columns and rows inside the frame below the title
	top              int  // rows of the frame above the first list row
	list, pane       int  // columns of the agent list (0 when hidden) and the transcript
	rowHeight, start int  // rows per list item and the first item shown
	title            bool // the window has room for its title
}

func (m *model) fleetLayout() fleetLayout {
	l := fleetLayout{inner: max(10, m.mainWidth-4), title: m.bodyHeight >= 9}
	l.rows, l.top = m.bodyHeight-2, 1
	if l.title {
		l.rows, l.top = l.rows-2, 3
	}
	l.rows = max(1, l.rows)
	if l.inner >= 76 {
		l.list = 26 // "done · 1m 02s · 12 calls" beside the lead
	}
	l.pane = l.inner - l.list
	if l.list > 0 {
		l.pane -= 3
	}
	l.rowHeight = 2
	if l.rows >= 14 {
		l.rowHeight = 3
	} else if l.rows < 6 {
		l.rowHeight = 1
	}
	if m.fleetView != nil {
		slots := max(1, (l.rows+1)/l.rowHeight)
		l.start = max(0, m.fleetView.selected-slots+1)
	}
	return l
}

// fleetBody is the window: the frame, its title, the list and the pane.
func (m *model) fleetBody() string {
	l := m.fleetLayout()
	var rows []string
	if l.title {
		heading := violetStyle.Bold(true).Render("FLEET")
		summary := "no subagents yet"
		if len(m.fleet) > 0 {
			summary = fmt.Sprintf("%d subagents · %s", len(m.fleet), m.fleetSummary())
			if len(m.fleet) == 1 {
				summary = "1 subagent · " + m.fleetSummary()
			}
		}
		heading += "  " + mutedStyle.Render(summary)
		if m.fleetView != nil && len(m.fleet) > 0 {
			position := faintStyle.Render(fmt.Sprintf("%d / %d", m.fleetView.selected+1, len(m.fleet)))
			if fill := l.inner - ansi.StringWidth(heading) - ansi.StringWidth(position); fill >= 2 {
				heading += strings.Repeat(" ", fill) + position
			}
		}
		rows = append(rows, ansi.Truncate(heading, l.inner, "…"), "")
	}
	if len(m.fleet) == 0 {
		empty := violetStyle.Render("◇ No subagents yet") + "\n\n" + mutedStyle.Render(ansi.Wrap("When JULIA delegates, each child's task, tool calls and report appear here.", max(10, min(60, l.inner)), ""))
		rows = append(rows, strings.Split(lipgloss.Place(l.inner, l.rows, lipgloss.Center, lipgloss.Center, empty), "\n")...)
	} else {
		list, pane := m.fleetList(l), m.fleetPane(l)
		for i := range l.rows {
			row := ""
			if l.list > 0 {
				row = paint(background, l.list, list[i]) + faintStyle.Render(" │ ")
			}
			row += pane[i]
			rows = append(rows, row)
		}
	}
	// The frame's height is a minimum: what does not fit is cut here.
	if limit := max(0, m.bodyHeight-2); len(rows) > limit {
		rows = rows[:limit]
	}
	frame := lipgloss.NewStyle().Width(m.mainWidth).Height(m.bodyHeight).Border(lipgloss.RoundedBorder()).BorderForeground(border).Padding(0, 1)
	return frame.Render(strings.Join(rows, "\n"))
}

// agentIcon is an agent's state glyph: the tool call's own marks, since an
// agent is a tool call seen from inside.
func agentIcon(state string) string {
	switch state {
	case agentRunning:
		return violetStyle.Render("◌")
	case agentFailed:
		return coralStyle.Render("!")
	default:
		return lipgloss.NewStyle().Foreground(green).Render("✓")
	}
}

func agentStateStyle(state string) lipgloss.Style {
	switch state {
	case agentRunning:
		return violetStyle
	case agentFailed:
		return coralStyle
	default:
		return lipgloss.NewStyle().Foreground(green)
	}
}

// elapsedAgent is how long an agent has run, or ran.
func (m *model) elapsedAgent(a *agent) string {
	end := a.ended
	if a.state == agentRunning || end.IsZero() {
		end = m.now
	}
	return elapsed(end.Sub(a.started))
}

// fleetList is the agent list, exactly l.rows rows: each agent lit like a
// picker choice, with a coral bar and the panel behind the chosen one.
func (m *model) fleetList(l fleetLayout) []string {
	if l.list == 0 {
		return make([]string, l.rows)
	}
	var rows []string
	width := l.list - 5 // the lead, then the icon and a space, or the detail's indent
	for i := l.start; i < len(m.fleet) && len(rows) < l.rows; i++ {
		a := m.fleet[i]
		if l.rowHeight == 3 && i > l.start {
			rows = append(rows, "")
		}
		bg := background
		bar, name, detail := " ", creamStyle, faintStyle
		if m.fleetView != nil && i == m.fleetView.selected {
			bg = panel
			bar, name, detail = ink(panel, coral).Render("▌"), ink(panel, cream).Bold(true), ink(panel, muted)
		}
		lead := ink(bg, cream).Render(" ") + bar + ink(bg, cream).Render(" ")
		rows = append(rows, paint(bg, l.list, lead+agentIcon(a.state)+ink(bg, cream).Render(" ")+name.Render(ansi.Truncate(oneLine(a.role), max(1, width), "…"))))
		if l.rowHeight >= 2 {
			var line string
			switch a.state {
			case agentRunning:
				line = oneLine(a.activity) + " · " + m.elapsedAgent(a)
			case agentFailed:
				line = "failed · " + m.elapsedAgent(a)
			default:
				line = fmt.Sprintf("done · %s · %d calls", m.elapsedAgent(a), a.calls)
				if a.calls == 1 {
					line = "done · " + m.elapsedAgent(a) + " · 1 call"
				}
			}
			rows = append(rows, paint(bg, l.list, lead+ink(bg, cream).Render("  ")+detail.Render(ansi.Truncate(line, max(1, width), "…"))))
		}
	}
	for len(rows) < l.rows {
		rows = append(rows, "")
	}
	return rows[:l.rows]
}

// fleetPane is the chosen agent's transcript, exactly l.rows rows: a masthead
// naming the agent, its model, state and spend, then its conversation, the
// task as the message it was given, each tool call as in the chat, and its
// report under its own name. A running agent's activity streams at the end.
func (m *model) fleetPane(l fleetLayout) []string {
	v := m.fleetView
	a := m.fleet[max(0, min(v.selected, len(m.fleet)-1))]
	width := l.pane
	if m.fleetMarkdownWidth != width {
		m.fleetMarkdown, m.fleetMarkdownWidth = markdownRenderer(width), width
	}
	state := agentStateStyle(a.state).Render(a.state) + mutedStyle.Render(fmt.Sprintf(" · %s · %d calls", m.elapsedAgent(a), a.calls))
	if a.calls == 1 {
		state = agentStateStyle(a.state).Render(a.state) + mutedStyle.Render(" · "+m.elapsedAgent(a)+" · 1 call")
	}
	name := agentIcon(a.state) + " " + creamStyle.Bold(true).Render(oneLine(a.role)) + "  " + mutedStyle.Render(oneLine(a.model))
	masthead := []string{fit(name, state, width)}
	spend := mutedStyle.Render(fmt.Sprintf("%s in · %s out", core.CompactTokens(a.usage.Prompt()), core.CompactTokens(a.usage.Output)))
	if a.usage.IsZero() {
		spend = faintStyle.Render("no model calls yet")
	}
	masthead = append(masthead, fit(spend, faintStyle.Render(oneLine(a.id)), width), faintStyle.Render(strings.Repeat("─", width)))
	var lines []string
	in := column{width: width, markdown: m.fleetMarkdown, wordWrap: true}
	for i := range a.entries {
		layout := m.renderEntryIn(&a.entries[i], in)
		if i > 0 && (a.entries[i].role != "tool" || a.entries[i-1].role != "tool") {
			lines = append(lines, "")
		}
		lines = append(lines, strings.Split(layout.text, "\n")...)
	}
	if a.state == agentRunning {
		stream := ansi.Truncate(m.spinner.View(), min(activityWidth, max(3, width/5)), "")
		duration := mutedStyle.Render(m.elapsedAgent(a))
		activity := mutedStyle.Render(ansi.Truncate(oneLine(a.activity), max(0, width-ansi.StringWidth(stream)-ansi.StringWidth(duration)-5), "…"))
		lines = append(lines, "", " "+stream+"  "+activity+"  "+duration)
	}
	visible := max(0, l.rows-len(masthead))
	most := max(0, len(lines)-visible)
	if v.follow {
		v.offset = most
	}
	v.offset = min(v.offset, most)
	v.below = most - v.offset
	rows := append([]string(nil), masthead...)
	for _, line := range lines[v.offset:min(len(lines), v.offset+visible)] {
		rows = append(rows, ansi.Truncate(line, width, "…"))
	}
	for len(rows) < l.rows {
		rows = append(rows, "")
	}
	return rows[:l.rows]
}

// fit sets left and right at the ends of a row of width, the right dropped
// when both cannot fit.
func fit(left, right string, width int) string {
	if gap := width - ansi.StringWidth(left) - ansi.StringWidth(right); gap >= 2 {
		return left + strings.Repeat(" ", gap) + right
	}
	return ansi.Truncate(left, width, "…")
}

func (m *model) fleetFooter() string {
	if m.width < 44 {
		return mutedStyle.Render("↑↓ agent · esc close")
	}
	if m.width < 70 {
		return mutedStyle.Render("↑↓ agent · pgup/pgdn scroll · esc close")
	}
	keys := []string{"↑↓ agent", "pgup/pgdn scroll", "ctrl+o expand", "esc close"}
	var out string
	for _, item := range keys {
		key, name, _ := strings.Cut(item, " ")
		out += violetStyle.Render(key+" ") + mutedStyle.Render(name) + "  "
	}
	return strings.TrimRight(out, " ")
}

// demoFleet is the preview's fleet: one agent in each state, mid-task.
func (m *model) demoFleet() []*agent {
	call := func(name, arguments, output string) entry {
		return entry{role: "tool", call: &core.ToolCall{Name: name, Arguments: json.RawMessage(arguments)}, done: true, result: &core.ToolResult{Content: output}}
	}
	scout := &agent{id: "scout-9f31", role: "scout", model: "codex/gpt-6-luna", started: m.now.Add(-74 * time.Second), ended: m.now.Add(-36 * time.Second), state: agentDone, calls: 4, usage: core.TokenUsage{Input: 4_100, CacheRead: 38_200, Output: 920}}
	scout.task = "Find where approvals are decided and where a cancelled task signals its process group. Return paths and line ranges only."
	scout.entries = []entry{
		{role: "user", label: "TASK", text: scout.task},
		call("Grep", `{"pattern":"func .*Approve","path":"internal/"}`, "3 matches across 2 files"),
		call("Read", `{"path":"internal/tools/tools.go"}`, "Read 212 lines · permission engine and reviewer hand-off"),
		call("Read", `{"path":"internal/tools/sandbox.go"}`, "Read 96 lines · process group and Seatbelt profile"),
		call("Grep", `{"pattern":"Signal\\(|Kill\\(","path":"internal/tools/"}`, "2 matches in sandbox.go"),
		{role: "assistant", label: "✦ scout", text: "**Approvals** are decided in `internal/tools/tools.go:88-141`; the reviewer is consulted at line 120.\n\n**Cancellation** signals the process group in `internal/tools/sandbox.go:61-74`, after the context is done."},
	}
	worker := &agent{id: "worker-2c8d", role: "worker", model: "codex/gpt-6-astra", started: m.now.Add(-31 * time.Second), state: agentRunning, activity: "Running Bash", calls: 3, usage: core.TokenUsage{Input: 9_800, CacheRead: 61_000, Output: 2_140}}
	worker.task = "Add a regression test for cancellation of the ordinary process group, then run the sandbox package's tests with the race detector."
	worker.entries = []entry{
		{role: "user", label: "TASK", text: worker.task},
		call("Read", `{"path":"internal/tools/sandbox_test.go"}`, "Read 318 lines"),
		call("Edit", `{"path":"internal/tools/sandbox_test.go"}`, "Edited internal/tools/sandbox_test.go: inserted 41 lines"),
		{role: "tool", call: &core.ToolCall{Name: "Bash", Arguments: json.RawMessage(`{"command":"go test -race ./internal/tools/ -run Cancel"}`)}},
	}
	oracle := &agent{id: "oracle-7b02", role: "oracle", model: "codex/gpt-6-astra", started: m.now.Add(-58 * time.Second), ended: m.now.Add(-49 * time.Second), state: agentFailed, calls: 1, usage: core.TokenUsage{Input: 3_300, Output: 85}}
	oracle.task = "Second opinion: does keeping approvals outside the sandbox boundary leave any path where a denied command still runs?"
	oracle.entries = []entry{
		{role: "user", label: "TASK", text: oracle.task},
		call("Read", `{"path":"internal/tools/tools.go"}`, "Read 212 lines"),
		{role: "warning", text: "codex/gpt-6-astra: the provider closed the stream (rate limited); the task was not completed"},
	}
	return []*agent{scout, oracle, worker}
}

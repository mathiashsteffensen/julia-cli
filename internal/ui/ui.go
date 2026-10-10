// Package ui is the provider-independent JARVIS terminal interface.
package ui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/glamour/v2"
	"github.com/charmbracelet/colorprofile"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"jarvis/internal/core"
	sessions "jarvis/internal/session"
)

// ApprovalRequest bridges the permission engine to the terminal. Reply should
// have capacity one so cancellation cannot leave a responder blocked.
type ApprovalRequest struct {
	Approval core.Approval
	Reply    chan core.Decision
}

// NewApprover creates a cancellable permission bridge with no worker goroutines.
func NewApprover() (core.Approver, <-chan ApprovalRequest) {
	requests := make(chan ApprovalRequest)
	return func(ctx context.Context, approval core.Approval) (core.Decision, error) {
		request := ApprovalRequest{Approval: approval, Reply: make(chan core.Decision, 1)}
		select {
		case requests <- request:
		case <-ctx.Done():
			return core.Decision{}, ctx.Err()
		}
		select {
		case decision := <-request.Reply:
			return decision, nil
		case <-ctx.Done():
			return core.Decision{}, ctx.Err()
		}
	}, requests
}

type Config struct {
	Session             *core.Session
	Backend             core.Backend
	Tools               core.ToolExecutor
	ToolDefinitions     []core.ToolDefinition
	Mode                string
	Save                func(*core.Session) error
	Approvals           <-chan ApprovalRequest
	Questions           <-chan QuestionRequest
	Demo                bool
	CompatibleReasoning bool
	Models              func(context.Context) ([]core.ModelOption, error)
	CompatibleModels    []core.ModelOption
	ApplySettings       func(*core.Session, string, string, string) error
	WeeklyUsage         func(context.Context) (*core.WeeklyUsage, error)
	ContextWindow       int // tokens the context gauge is measured against; <= 0 means core.DefaultContextWindow
}

type entry struct {
	role           string
	text           string
	call           *core.ToolCall
	result         *core.ToolResult
	done           bool
	revision       uint64
	cachedRevision uint64
	cachedWidth    int
	cachedExpanded bool
	cached         string
}

type streamMsg struct {
	event    *core.Event
	finished bool
	session  *core.Session
	err      error
}

type approvalMsg struct{ request ApprovalRequest }
type tickMsg time.Time

type model struct {
	cfg                    Config
	input                  textarea.Model
	viewport               viewport.Model
	spinner                spinner.Model
	width                  int
	height                 int
	mainWidth              int
	bodyHeight             int
	entries                []entry
	markdown               *glamour.TermRenderer
	renderWidth            int
	busy                   bool
	quitting               bool
	status                 string
	notice                 string
	started                time.Time
	lastDuration           time.Duration
	now                    time.Time
	inputTokens            int               // last model call: the whole prompt, cached tokens included
	outputTokens           int               // last model call
	live                   core.SessionUsage // usage reported by the running task, added to the session's until the stream finishes
	toolCount              int
	expandTools            bool
	pending                *ApprovalRequest
	approvalListening      bool
	question               *questionState
	questionListening      bool
	deferredApproval       *ApprovalRequest
	deferredQuestion       *QuestionRequest
	stream                 chan streamMsg
	cancel                 context.CancelFunc
	lifetime               context.Context
	stop                   context.CancelFunc
	wg                     sync.WaitGroup
	wordmarkEnabled        bool
	wordmarkTrueColor      bool
	wordmarkPending        bool
	wordmarkReady          bool
	wordmarkStopping       bool
	picker                 *settingsPicker
	pickerSequence         uint64
	models                 []core.ModelOption
	weekly                 weeklyUsageState
	reasoningCycleSequence uint64
	reasoningCyclePending  int
	reasoningCycleCancel   context.CancelFunc
	// compatibleModels is the switchable catalog for the compatible provider.
	// It starts from the saved compatible_models and remembers any
	// compatible model the session has used, so the provider stays reachable
	// after the user switches to Codex and back.
	compatibleModels []core.ModelOption
}

// Run owns terminal lifetime and waits for a cancelled backend before returning.
func Run(cfg Config) error {
	if cfg.Session == nil {
		return errors.New("UI requires a session")
	}
	if cfg.Backend == nil && !cfg.Demo {
		return errors.New("UI requires a backend")
	}
	m := newModel(cfg)
	defer func() {
		m.stop()
		if m.cancel != nil {
			m.cancel()
		}
		m.wg.Wait()
	}()
	_, err := tea.NewProgram(m).Run()
	if err != nil {
		return fmt.Errorf("run terminal interface: %w", err)
	}
	return nil
}

func newModel(cfg Config) *model {
	ctx, stop := context.WithCancel(context.Background())
	input := textarea.New()
	input.Placeholder = "What shall we work on, sir?"
	input.Prompt = "› "
	input.SetPromptFunc(2, func(info textarea.PromptInfo) string {
		if info.LineNumber == 0 {
			return "› "
		}
		return "  "
	})
	input.ShowLineNumbers = false
	input.CharLimit = 65536
	input.SetHeight(2)
	input.SetVirtualCursor(true)
	input.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("alt+enter", "shift+enter", "ctrl+j"))
	input.SetStyles(composerStyles())
	input.Focus()
	spin := spinner.New()
	spin.Spinner = spinner.Dot
	spin.Style = coralStyle
	m := &model{
		cfg: cfg, input: input, viewport: viewport.New(), spinner: spin,
		status: "Ready", lifetime: ctx, stop: stop, now: time.Now(),
		compatibleModels: append([]core.ModelOption(nil), cfg.CompatibleModels...),
	}
	m.viewport.FillHeight = true
	m.loadHistory()
	if cfg.Demo {
		m.entries = demoEntries()
		m.toolCount = 3
		m.lastDuration = 28 * time.Second
		m.inputTokens, m.outputTokens = 117_300, 1_284
		if cfg.Session.Usage == (core.SessionUsage{}) {
			cfg.Session.Usage = demoUsage()
		}
		m.notice = "Preview · offline · no tools or provider calls · try /ask"
		if cfg.Session.Provider == "codex" {
			m.weekly.value = &core.WeeklyUsage{RemainingPercent: 61, ResetsAt: m.now.Add(3 * 24 * time.Hour)}
		}
	}
	m.resize(100, 30)
	return m
}

func (m *model) Init() tea.Cmd {
	return tea.Batch(textarea.Blink, m.spinner.Tick, m.waitApproval(), m.waitQuestion(), nextTick(), m.refreshWeeklyUsage(false))
}

func nextTick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m *model) waitApproval() tea.Cmd {
	if m.cfg.Approvals == nil || m.approvalListening {
		return nil
	}
	m.approvalListening = true
	return func() tea.Msg {
		select {
		case request, ok := <-m.cfg.Approvals:
			if !ok {
				return nil
			}
			return approvalMsg{request}
		case <-m.lifetime.Done():
			return nil
		}
	}
}

func waitStream(stream <-chan streamMsg, ctx context.Context) tea.Cmd {
	return func() tea.Msg {
		select {
		case msg := <-stream:
			return msg
		case <-ctx.Done():
			return nil
		}
	}
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case weeklyUsageMsg:
		m.receiveWeeklyUsage(msg)
		return m, nil
	case modelsMsg:
		return m, m.receiveModels(msg)
	case reasoningCycleMsg:
		return m, m.receiveReasoningCycle(msg)
	case tea.EnvMsg:
		m.wordmarkEnabled = supportsWordmark(msg)
		return m, m.requestWordmark()
	case tea.ColorProfileMsg:
		m.wordmarkTrueColor = msg.Profile == colorprofile.TrueColor
		return m, m.requestWordmark()
	case tea.ModeReportMsg:
		if msg.Mode == ansi.ModeTextCursorEnable && m.wordmarkStopping {
			return m, tea.Quit
		}
		if msg.Mode == ansi.ModeAltScreenSaveCursor && m.wordmarkPending && !m.quitting {
			m.wordmarkPending = false
			return m, uploadWordmark()
		}
		return m, nil
	case wordmarkExitMsg:
		return m, tea.Quit
	case uv.KittyGraphicsEvent:
		if msg.Options.ID == wordmarkID && m.wordmarkEnabled && m.wordmarkTrueColor && !m.wordmarkPending && !m.quitting {
			m.wordmarkReady = string(msg.Payload) == "OK"
		}
		return m, nil
	case tea.WindowSizeMsg:
		m.resize(msg.Width, msg.Height)
		return m, m.requestWordmark()
	case tickMsg:
		m.now = time.Time(msg)
		return m, tea.Batch(nextTick(), m.refreshWeeklyUsage(false))
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	case approvalMsg:
		m.approvalListening = false
		if !m.busy || m.quitting {
			reply(msg.request, false, "The active task has ended or was cancelled")
			return m, m.waitApproval()
		}
		if m.question != nil {
			// One request on screen at a time; this one waits behind the question.
			m.deferredApproval = &msg.request
			return m, nil
		}
		m.pending = &msg.request
		m.refresh(true)
		return m, nil
	case questionMsg:
		m.questionListening = false
		if !m.busy || m.quitting {
			replyQuestion(msg.request, core.Answer{Skipped: true})
			return m, m.waitQuestion()
		}
		if m.pending != nil || m.question != nil {
			m.deferredQuestion = &msg.request
			return m, nil
		}
		return m, m.openQuestion(msg.request, false)
	case streamMsg:
		if msg.finished {
			m.busy = false
			m.cancel = nil
			m.lastDuration = time.Since(m.started)
			m.status = "Ready"
			m.notice = ""
			if m.pending != nil {
				reply(*m.pending, false, "The active task has ended")
				m.pending = nil
			}
			m.dismissInteractions("The active task has ended")
			if msg.session != nil {
				*m.cfg.Session = *msg.session
				if m.cfg.Save != nil {
					if err := m.cfg.Save(m.cfg.Session); err != nil {
						m.entries = append(m.entries, entry{role: "warning", text: "Could not save session: " + err.Error()})
					}
				}
			} else {
				// Nothing recorded the run in the session; keep what was shown.
				m.cfg.Session.Usage = m.usage()
			}
			// The session now holds every event of the run. Clearing the live
			// figures exactly here is what keeps them from being counted twice.
			m.live = core.SessionUsage{}
			if msg.err != nil {
				if errors.Is(msg.err, context.Canceled) {
					m.entries = append(m.entries, entry{role: "note", text: "Task interrupted. Ready when you are."})
				} else {
					m.entries = append(m.entries, entry{role: "warning", text: msg.err.Error()})
				}
			}
			m.refresh(false)
			if m.quitting {
				return m, m.quit()
			}
			return m, tea.Batch(m.waitApproval(), m.waitQuestion(), m.refreshWeeklyUsage(true))
		}
		if msg.event != nil {
			m.applyEvent(*msg.event)
		}
		return m, waitStream(m.stream, m.lifetime)
	case tea.KeyPressMsg:
		if m.pending != nil {
			if msg.String() != "ctrl+c" {
				return m, m.decide(msg.String() == "y" || msg.String() == "enter")
			}
			reply(*m.pending, false, "Task cancelled by user")
			m.pending = nil
			m.refresh(false)
		}
		if m.question != nil && msg.String() != "ctrl+c" {
			// Ctrl+C falls through to cancelTask, which cancels the task before the
			// question is released, so the tool sees a cancellation, not a skip.
			return m, m.updateQuestion(msg)
		}
		if m.picker != nil && m.pending == nil && msg.String() != "ctrl+c" {
			return m, m.updatePicker(msg)
		}
		switch msg.String() {
		case "ctrl+c":
			m.cancelWeeklyUsage()
			if m.busy {
				m.quitting = true
				m.cancelTask()
				return m, nil
			}
			return m, m.quit()
		case "esc":
			if m.busy {
				m.cancelTask()
			} else {
				m.notice = ""
			}
			return m, nil
		case "pgup", "pgdown", "ctrl+home", "ctrl+end":
			switch msg.String() {
			case "pgup":
				m.viewport.ScrollUp(max(1, m.bodyHeight-2))
			case "pgdown":
				m.viewport.ScrollDown(max(1, m.bodyHeight-2))
			case "ctrl+home":
				m.viewport.GotoTop()
			case "ctrl+end":
				m.viewport.GotoBottom()
			}
			return m, nil
		case "ctrl+o":
			m.expandTools = !m.expandTools
			m.refresh(false)
			return m, nil
		}
		if msg.String() == "ctrl+p" {
			return m, m.openPicker("model")
		}
		if msg.String() == "shift+tab" {
			return m, m.cycleReasoning()
		}
		if msg.String() == "enter" {
			return m, m.submit()
		}
	case tea.MouseClickMsg:
		if m.question != nil && m.pending == nil {
			return m, m.clickQuestion(msg)
		}
	case tea.MouseWheelMsg:
		if m.picker != nil && m.pending == nil {
			return m, nil
		}
		var cmd tea.Cmd
		m.viewport, cmd = m.viewport.Update(msg)
		return m, cmd
	}
	if m.question != nil {
		return m, m.updateQuestion(msg)
	}
	if m.picker != nil {
		return m, m.updatePicker(msg)
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func (m *model) decide(allow bool) tea.Cmd {
	if m.pending == nil {
		return nil
	}
	reason := "Declined by user"
	if allow {
		reason = "Approved once by user"
	}
	reply(*m.pending, allow, reason)
	m.entries = append(m.entries, entry{role: "note", text: reason + ": " + m.pending.Approval.Call.Name})
	m.pending = nil
	m.refresh(true)
	return tea.Batch(m.waitApproval(), m.promoteInteraction())
}

func reply(request ApprovalRequest, allow bool, reason string) {
	select {
	case request.Reply <- core.Decision{Allow: allow, Reason: reason}:
	default:
	}
}

func (m *model) cancelTask() {
	if m.cancel != nil {
		m.cancel()
	}
	m.status = "Stopping safely…"
	m.notice = "Waiting for the current process to stop"
	if m.pending != nil {
		reply(*m.pending, false, "Task cancelled by user")
		m.pending = nil
		m.refresh(false)
	}
	m.dismissInteractions("Task cancelled by user")
}

func (m *model) submit() tea.Cmd {
	prompt := strings.TrimSpace(m.input.Value())
	if prompt == "" {
		return nil
	}
	if m.busy {
		m.notice = "Draft kept. Send when the task finishes, or Esc to interrupt."
		return nil
	}
	if strings.HasPrefix(prompt, "/") {
		if handled, cmd := m.settingsCommand(prompt); handled {
			return cmd
		}
		switch prompt {
		case "/quit", "/exit":
			return m.quit()
		case "/help":
			m.entries = append(m.entries, entry{role: "assistant", text: "**At your service.**\n\n- **Enter** sends · **Alt+Enter** adds a line\n- **Esc** interrupts a running task\n- **PgUp / PgDown** scroll · **Ctrl+End** returns to live output\n- **Ctrl+O** expands tool results\n- **Ctrl+P** or `/model` opens model search · **Tab/Shift+Tab** switch catalogs in the picker\n- **Shift+Tab** outside the picker cycles reasoning · `/reasoning` opens its picker\n- `/model ID` and `/reasoning LEVEL|default` set them directly\n- **Ctrl+C** stops the task and exits safely\n\n`/clear` starts a fresh conversation · `/quit` exits\n\nSettings apply to the next turn. While a task runs, you can prepare your next message. Your draft stays in the composer until you send it."})
		case "/ask", "/ask text":
			if !m.cfg.Demo {
				m.notice = "Unknown command. /help lists the essentials."
				return nil
			}
			m.input.Reset()
			return m.openQuestion(QuestionRequest{Question: demoQuestion(prompt == "/ask text")}, true)
		case "/clear":
			next := sessions.New(m.cfg.Session.Directory, m.cfg.Session.Provider, m.cfg.Session.Model)
			next.Reasoning = m.cfg.Session.Reasoning
			next.ReasoningSet = m.cfg.Session.ReasoningSet
			if m.cfg.Save != nil {
				if err := m.cfg.Save(m.cfg.Session); err != nil {
					m.notice = "Could not save current conversation: " + err.Error()
					return nil
				}
				if err := m.cfg.Save(next); err != nil {
					m.notice = "Could not create conversation: " + err.Error()
					return nil
				}
			}
			*m.cfg.Session = *next
			m.entries = nil
			m.resetCounters()
			m.notice = "New conversation."
			if m.cfg.Save != nil {
				m.notice += " The previous session is saved."
			}
		default:
			m.notice = "Unknown command. /help lists the essentials."
			return nil
		}
		m.input.Reset()
		m.refresh(true)
		return nil
	}
	if m.cfg.Demo {
		m.notice = "Offline preview. Restart without --demo to begin a real session."
		return nil
	}
	m.invalidateReasoningCycle()
	m.input.Reset()
	m.notice = ""
	m.busy = true
	m.status = "Thinking"
	m.started, m.now = time.Now(), time.Now()
	m.entries = append(m.entries, entry{role: "user", text: prompt})
	m.refresh(true)
	ctx, cancel := context.WithCancel(m.lifetime)
	m.cancel = cancel
	m.stream = make(chan streamMsg, 128)
	stream := m.stream
	session := cloneSession(m.cfg.Session)
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		err := m.cfg.Backend.Run(ctx, session, prompt, m.cfg.Tools, func(event core.Event) {
			// Providers may reuse their event's backing data after Emit returns.
			event = cloneEvent(event)
			select {
			case stream <- streamMsg{event: &event}:
			case <-ctx.Done():
			}
		})
		select {
		case stream <- streamMsg{finished: true, session: session, err: err}:
		case <-m.lifetime.Done():
		}
	}()
	return waitStream(stream, m.lifetime)
}

func cloneSession(original *core.Session) *core.Session {
	copy := *original
	copy.State = append(json.RawMessage(nil), original.State...)
	copy.Audit = make([]core.AuditEntry, len(original.Audit))
	for i, event := range original.Audit {
		copy.Audit[i] = event
		copy.Audit[i].Event = cloneEvent(event.Event)
	}
	if original.PendingTool != nil {
		pending := *original.PendingTool
		pending.Arguments = append(json.RawMessage(nil), pending.Arguments...)
		copy.PendingTool = &pending
	}
	copy.Messages = make([]core.Message, len(original.Messages))
	for i, message := range original.Messages {
		copy.Messages[i] = message
		copy.Messages[i].ToolCalls = make([]core.ToolCall, len(message.ToolCalls))
		for j, call := range message.ToolCalls {
			copy.Messages[i].ToolCalls[j] = call
			copy.Messages[i].ToolCalls[j].Arguments = append(json.RawMessage(nil), call.Arguments...)
		}
	}
	return &copy
}

func cloneEvent(event core.Event) core.Event {
	if event.Call != nil {
		call := *event.Call
		call.Arguments = append(json.RawMessage(nil), call.Arguments...)
		event.Call = &call
	}
	if event.Result != nil {
		result := *event.Result
		event.Result = &result
	}
	return event
}

func (m *model) applyEvent(event core.Event) {
	switch event.Kind {
	case "text":
		if len(m.entries) == 0 || m.entries[len(m.entries)-1].role != "assistant" {
			m.entries = append(m.entries, entry{role: "assistant"})
		}
		m.entries[len(m.entries)-1].text += event.Text
		m.entries[len(m.entries)-1].revision++
		m.status = "Working"
	case "status":
		m.status = event.Text
	case "warning":
		m.entries = append(m.entries, entry{role: "warning", text: event.Text})
	case "tool_start":
		m.toolCount++
		m.entries = append(m.entries, entry{role: "tool", call: event.Call, text: event.Text})
		if event.Call != nil {
			m.status = "Running " + event.Call.Name
		}
	case "tool_end":
		for i := len(m.entries) - 1; i >= 0; i-- {
			if m.entries[i].role == "tool" && !m.entries[i].done && (event.Call == nil || m.entries[i].call != nil && m.entries[i].call.ID == event.Call.ID) {
				m.entries[i].done = true
				m.entries[i].result = event.Result
				m.entries[i].revision++
				break
			}
		}
		m.status = "Thinking"
	case "usage":
		m.recordUsage(event)
	}
	m.refresh(false)
}

func (m *model) loadHistory() {
	if m.cfg.Session == nil {
		return
	}
	for _, message := range m.cfg.Session.Messages {
		switch message.Role {
		case "user", "assistant":
			if message.Content != "" {
				m.entries = append(m.entries, entry{role: message.Role, text: message.Content})
			}
			for _, call := range message.ToolCalls {
				call := call
				m.entries = append(m.entries, entry{role: "tool", call: &call, done: true})
				m.toolCount++
			}
		case "tool":
			for i := len(m.entries) - 1; i >= 0; i-- {
				if m.entries[i].call != nil && m.entries[i].call.ID == message.ToolCallID {
					m.entries[i].result = &core.ToolResult{Content: message.Content}
					break
				}
			}
		}
	}
}

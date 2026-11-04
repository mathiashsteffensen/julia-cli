// Package ui is the provider-independent JULIA terminal interface.
package ui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"julia/internal/config"
	"julia/internal/core"
	"julia/internal/images"
	sessions "julia/internal/session"
	"julia/internal/skills"
	"julia/internal/throughput"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
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
	Session         *core.Session
	Backend         core.Backend
	Tools           core.ToolExecutor
	ToolDefinitions []core.ToolDefinition
	Mode            string
	Save            func(*core.Session) error
	Approvals       <-chan ApprovalRequest
	Questions       <-chan QuestionRequest
	Demo            bool
	// NamedProviders are the configured OpenAI-compatible providers by id,
	// each true when it takes a reasoning effort.
	NamedProviders map[string]bool
	Models         func(context.Context) ([]core.ModelOption, error)
	// EnabledModels are the configured enabled_models, which the picker lists
	// first and ctrl+n cycles.
	EnabledModels []core.ModelOption
	ApplySettings func(*core.Session, string, string, string) error
	// Profile is the active configuration profile. Profiles lists the
	// profiles /profile offers, and SwitchProfile makes one active: it moves
	// the session to the profile's model, as ApplySettings does, and returns
	// what else the profile changes in the interface.
	Profile       string
	Profiles      func() ([]Profile, error)
	SwitchProfile func(*core.Session, string) (ProfileSettings, error)
	// AccountUsage reads the allowance of the account behind a session's provider.
	AccountUsage func(ctx context.Context, provider string) (*core.AccountUsage, error)
	// Throughput reports each model's recorded speed, keyed by provider/model,
	// read when the picker opens.
	Throughput func() map[string]throughput.Stats
	// Children lists the subagent sessions a session ran, so a resumed
	// session's fleet shows what its children did.
	Children func(parent string) ([]*core.Session, error)
	// Memory answers the /memory command: an empty action lists the notes this workspace sees, "show" with a name shows one, and "forget" with a name deletes it; the Markdown returned is shown in the conversation.
	Memory func(action, name string) (string, error)
	// Skills lists the user's skills, each a slash command; it is read again
	// whenever the command menu opens or a command runs.
	Skills        func() []skills.Skill
	Version       string // shown in the sidebar masthead
	ContextWindow int    // fallback when no provider/model window is known; <= 0 means core.DefaultContextWindow
	// ContextWindowFor is the window of the session's model, by provider and
	// model ID, when the provider has not reported one. Nil, or a result of
	// zero or less, measures against ContextWindow.
	ContextWindowFor func(provider, model string) int
	// Hangup ends the interface when the terminal is gone, as a closed
	// window's SIGHUP says, after the running task has stopped and cleaned
	// up: its terminals and browser killed, not orphaned.
	Hangup context.Context
	// Followups announces work the backend finishes after a turn has
	// ended, such as the reflection that reviews it; see Followup.
	Followups <-chan Followup
}

type entry struct {
	role           string
	text           string
	label          string       // the heading shown instead of the role's own, such as a fleet agent's task or name
	noSpark        bool         // a reply's later block in a transcript, which goes on from the first
	images         []core.Image // a user message's attached images, or the image a tool returned
	imageIDs       []int        // the Kitty image ID that draws each of images
	call           *core.ToolCall
	result         *core.ToolResult
	done           bool
	revision       uint64
	cachedRevision uint64
	cachedWidth    int
	cachedExpanded bool
	cachedGraphics uint64
	layout         entryLayout
}

// heading is the label an entry is shown under: its own, or fallback.
func (e entry) heading(fallback string) string {
	if e.label != "" {
		return e.label
	}
	return fallback
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
	cfg          Config
	input        textarea.Model
	composerRows int // input rows the current layout was measured with
	// menu completes slash commands; skills are the user's, as last read.
	menu          commandMenu
	skills        []skills.Skill
	viewport      viewport.Model
	spinner       activityModel
	width         int
	height        int
	mainWidth     int
	bodyHeight    int
	sidebarOffset int // rows of the sidebar scrolled out of view
	entries       []entry
	markdown      markdownCache
	lines         []string // the conversation as rendered, one per row
	rows          []rowRef // what each of those rows shows, for copying
	selection     *selection
	lastClick     struct {
		at  time.Time
		row int
	}
	flashSeq               uint64
	clipboard              func(string) error
	openURL                func(string) error
	clipboardImage         func(context.Context) (core.Image, error)
	attachments            []attachment // images pasted into the draft
	imageSeq               int          // the last [Image #N] number handed out in this draft
	busy                   bool
	activityRow            bool // the conversation ends with a row reserved for the activity
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
	followup               *Followup // work under way after the last turn ended
	blurred                bool      // the terminal is in the background, as its last focus report said
	tmux                   bool      // JULIA runs inside tmux, so notifications are passed through to the terminal
	followupListening      bool
	turnStart              int             // where the turn under way begins in entries; -1 when none
	queued                 []queuedMessage // messages sent while the turn runs, oldest first
	inbox                  *core.Inbox     // the turn under way's steering; nil when none runs
	queueRows              int             // rows of the queue strip the current layout was measured with
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
	graphicsConfirmed      bool               // the terminal accepted a Kitty graphics upload
	graphicsEpoch          uint64             // bumps when thumbnails must be laid out again
	cellWidth, cellHeight  int                // the terminal's cell size in pixels; zero until reported
	thumbnails             map[int]core.Image // sent images, by the Kitty image ID that draws them
	thumbnailOrder         []int              // those IDs, in the order they were registered
	picker                 *settingsPicker
	profiles               []Profile // what the profile picker lists, read when it opens
	pickerSequence         uint64
	fleet                  []*agent    // every subagent of the session, oldest first
	fleetView              *fleetState // the fleet window, while open
	fleetMarkdown          markdownCache
	models                 []core.ModelOption
	accountUsage           accountUsageState
	throughput             map[string]throughput.Stats // each model's recorded speed, loaded when the picker opens
	reasoningCycleSequence uint64
	reasoningCyclePending  int
	reasoningCycleCancel   context.CancelFunc
	// knownModels are the picker's models besides Codex's catalog: the enabled
	// models, then any other model the session has used, so it stays
	// reachable after switching away from it.
	knownModels []core.ModelOption
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
	var options []tea.ProgramOption
	if cfg.Hangup != nil {
		options = append(options, tea.WithContext(cfg.Hangup))
	}
	_, err := tea.NewProgram(m, options...).Run()
	if err != nil && !(cfg.Hangup != nil && cfg.Hangup.Err() != nil && errors.Is(err, tea.ErrProgramKilled)) {
		return fmt.Errorf("run terminal interface: %w", err)
	}
	return nil
}

func newModel(cfg Config) *model {
	ctx, stop := context.WithCancel(context.Background())
	input := textarea.New()
	input.Placeholder = "What shall we work on?"
	input.Prompt = "› "
	input.SetPromptFunc(2, func(info textarea.PromptInfo) string {
		if info.LineNumber == 0 {
			return "› "
		}
		return "  "
	})
	input.ShowLineNumbers = false
	input.CharLimit = 65536
	// The composer grows with its draft; resize caps it at half the screen.
	input.DynamicHeight = true
	input.MinHeight = 2
	// Without this the textarea treats MaxHeight as a line limit and stops
	// accepting newlines once the draft fills the visible rows.
	input.MaxContentHeight = 65536
	input.SetHeight(2)
	input.SetVirtualCursor(true)
	input.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("alt+enter", "shift+enter", "ctrl+j"))
	input.SetStyles(composerStyles())
	input.Focus()
	m := &model{
		cfg: cfg, input: input, viewport: viewport.New(),
		status: "Ready", lifetime: ctx, stop: stop, now: time.Now(), clipboard: systemClipboard, openURL: openBrowser, clipboardImage: images.Clipboard,
		knownModels: append([]core.ModelOption(nil), cfg.EnabledModels...),
		turnStart:   -1,
	}
	m.viewport.FillHeight = true
	m.loadHistory()
	m.loadFleet()
	if cfg.Demo {
		m.entries = demoEntries()
		m.fleet = m.demoFleet()
		m.toolCount = 4
		m.lastDuration = 28 * time.Second
		m.inputTokens, m.outputTokens = 117_300, 1_284
		if cfg.Session.Usage == (core.SessionUsage{}) {
			cfg.Session.Usage = demoUsage()
		}
		m.notice = "Preview · offline · no tools or provider calls · try /ask or /review"
		if config.IsCLIProvider(cfg.Session.Provider) {
			m.accountUsage.weekly.value = &core.UsageWindow{RemainingPercent: 61, ResetsAt: m.now.Add(3 * 24 * time.Hour)}
			m.accountUsage.fiveHour.value = &core.UsageWindow{RemainingPercent: 38, ResetsAt: m.now.Add(2 * time.Hour)}
		}
	}
	m.resize(100, 30)
	return m
}

func (m *model) Init() tea.Cmd {
	return tea.Batch(textarea.Blink, m.waitApproval(), m.waitQuestion(), m.waitFollowup(), nextTick(), m.refreshAccountUsage(false))
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
	next, cmd := m.update(msg)
	m.syncMenu()
	if m.input.Height() != m.composerRows || len(m.queueStrip()) != m.queueRows {
		// The draft or the queue grew or shrank a row: give the transcript the difference.
		m.resize(m.width, m.height)
	}
	return next, cmd
}

func (m *model) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case accountUsageMsg:
		m.receiveAccountUsage(msg)
		return m, nil
	case modelsMsg:
		return m, m.receiveModels(msg)
	case reasoningCycleMsg:
		return m, m.receiveReasoningCycle(msg)
	case tea.EnvMsg:
		m.wordmarkEnabled = supportsWordmark(msg)
		m.tmux = msg.Getenv("TMUX") != ""
		return m, tea.Batch(m.requestWordmark(), m.requestCellSize())
	case tea.ColorProfileMsg:
		m.wordmarkTrueColor = msg.Profile == colorprofile.TrueColor
		return m, tea.Batch(m.requestWordmark(), m.requestCellSize())
	case tea.ModeReportMsg:
		if msg.Mode == ansi.ModeTextCursorEnable && m.wordmarkStopping {
			return m, tea.Quit
		}
		if msg.Mode == ansi.ModeAltScreenSaveCursor && m.wordmarkPending && !m.quitting {
			m.wordmarkPending = false
			return m, tea.Batch(uploadWordmark(), m.uploadThumbnails())
		}
		return m, nil
	case wordmarkExitMsg:
		return m, tea.Quit
	case uv.KittyGraphicsEvent:
		if msg.Options.ID == wordmarkID && m.wordmarkEnabled && m.wordmarkTrueColor && !m.wordmarkPending && !m.quitting {
			m.wordmarkReady = string(msg.Payload) == "OK"
			if m.wordmarkReady && !m.graphicsConfirmed {
				m.graphicsConfirmed = true
				m.graphicsEpoch++
				m.refresh(false)
			}
		}
		return m, nil
	case uv.CellSizeEvent:
		return m, m.receiveCellSize(msg)
	case tea.WindowSizeMsg:
		m.resize(msg.Width, msg.Height)
		// A font size change resizes the window too, so ask for the cell size again.
		return m, tea.Batch(m.requestWordmark(), m.requestCellSize())
	case copiedMsg:
		return m, m.receiveCopied(msg)
	case linkOpenedMsg:
		m.receiveLinkOpened(msg)
		return m, nil
	case imagesMsg:
		return m, m.receiveImages(msg)
	case tea.PasteMsg:
		if m.pending == nil && m.question == nil && m.picker == nil && m.fleetView == nil {
			if cmd, handled := m.pasteText(msg.Content); handled {
				return m, cmd
			}
		}
	case copyNoticeDoneMsg:
		if m.notice == msg.notice {
			m.notice = ""
		}
		return m, nil
	case flashDoneMsg:
		if m.selection != nil && m.selection.flash != nil && m.selection.flashSeq == msg.seq {
			m.selection = nil
		}
		return m, nil
	case tea.FocusMsg:
		m.blurred = false
		return m, nil
	case tea.BlurMsg:
		m.blurred = true
		return m, nil
	case tickMsg:
		m.now = time.Time(msg)
		return m, tea.Batch(nextTick(), m.refreshAccountUsage(false))
	case activityTickMsg:
		return m, m.spinner.update(msg)
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
		return m, m.notifyApproval(msg.request.Approval)
	case followupMsg:
		m.followupListening = false
		m.receiveFollowup(msg.followup)
		return m, m.waitFollowup()
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
			m.spinner.stop()
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
			settled := m.notifySettled(msg.err)
			return m, tea.Batch(m.waitApproval(), m.waitQuestion(), m.refreshAccountUsage(true), m.settleQueue(msg.err), settled)
		}
		if msg.event != nil {
			return m, tea.Batch(m.applyEvent(*msg.event), waitStream(m.stream, m.lifetime))
		}
		return m, waitStream(m.stream, m.lifetime)
	case tea.KeyPressMsg:
		if m.selection != nil && !m.selection.dragging {
			m.selection = nil
		}
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
		if m.fleetView != nil && m.pending == nil && msg.String() != "ctrl+c" {
			return m, m.updateFleet(msg)
		}
		if msg.String() == "ctrl+f" {
			return m, m.openFleet()
		}
		if m.picker != nil && m.pending == nil && msg.String() != "ctrl+c" {
			return m, m.updatePicker(msg)
		}
		if m.menuOpen() {
			if handled, cmd := m.updateMenu(msg); handled {
				return m, cmd
			}
		}
		switch msg.String() {
		case "ctrl+c":
			m.cancelAccountUsage()
			if m.busy {
				m.quitting = true
				m.cancelTask()
				return m, nil
			}
			return m, m.quit()
		case "esc":
			if m.busy {
				m.cancelTask()
			} else if !m.skipFollowup() {
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
		if msg.String() == "ctrl+n" {
			return m, m.cycleModel()
		}
		if msg.String() == "shift+tab" {
			return m, m.cycleReasoning()
		}
		if msg.String() == "ctrl+v" {
			return m, m.pasteImage(true)
		}
		if msg.String() == "ctrl+s" && m.busy {
			// A draft in the composer steers along with what is queued.
			cmd := m.submit()
			m.steer()
			return m, cmd
		}
		if msg.String() == "up" && m.editQueued() {
			return m, nil
		}
		if msg.String() == "enter" {
			return m, m.submit()
		}
	case tea.MouseClickMsg:
		if m.pending == nil && msg.Button == tea.MouseLeft && m.clickSteer(msg.X, msg.Y) {
			return m, nil
		}
		if m.question != nil && m.pending == nil {
			return m, m.clickQuestion(msg)
		}
		if m.fleetView != nil && m.pending == nil {
			return m, m.clickFleet(msg)
		}
		return m, m.mouseDown(msg)
	case tea.MouseMotionMsg:
		return m, m.mouseDrag(msg)
	case tea.MouseReleaseMsg:
		return m, m.mouseUp(msg)
	case tea.MouseWheelMsg:
		if m.overSidebar(msg.X, msg.Y) {
			switch msg.Button {
			case tea.MouseWheelUp:
				m.scrollSidebar(-1)
			case tea.MouseWheelDown:
				m.scrollSidebar(1)
			}
			return m, nil
		}
		if m.fleetView != nil && m.pending == nil {
			switch msg.Button {
			case tea.MouseWheelUp:
				m.scrollFleet(-3)
			case tea.MouseWheelDown:
				m.scrollFleet(3)
			}
			return m, nil
		}
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
	if handled, cmd := m.copyCommand(prompt); handled {
		return cmd // copying never waits for the task
	}
	if m.busy {
		if strings.HasPrefix(prompt, "/") {
			m.notice = "Commands wait until the task finishes. Draft kept."
			return nil
		}
		m.queue(m.takeImages(prompt))
		m.input.Reset()
		return nil
	}
	if text, ok := reviewRequest(prompt); ok {
		if m.cfg.Demo {
			review := demoReview()
			m.input.Reset()
			return m.openQuestion(QuestionRequest{Review: &review}, true)
		}
		// From here it is an ordinary message, the same one a user could type.
		prompt = text
	}
	if strings.HasPrefix(prompt, "/") {
		if handled, cmd := m.settingsCommand(prompt); handled {
			return cmd
		}
		if handled, cmd := m.profileCommand(prompt); handled {
			return cmd
		}
		if m.memoryCommand(prompt) {
			return nil
		}
		if skill, ok := m.skillCommand(prompt); ok {
			if m.cfg.Demo {
				m.notice = "Offline preview. Restart without --demo to run a skill."
				return nil
			}
			// The command is sent as typed; the runtime adds the skill to it.
			request := m.takeImages(prompt)
			request.Skill = skill
			m.input.Reset()
			return m.send(request)
		}
		var cmd tea.Cmd
		switch prompt {
		case "/quit", "/exit":
			return m.quit()
		case "/help":
			m.entries = append(m.entries, entry{role: "assistant", text: "**At your service.**\n\n- **Enter** sends · **Alt+Enter** adds a line\n- **Esc** interrupts a running task, or skips the reflection after it\n- **PgUp / PgDown** scroll · **Ctrl+End** returns to live output\n- **Ctrl+O** expands tool results and reveals full subagent tasks\n- **Ctrl+F** opens the fleet: every subagent's task, tool calls and report\n- **Ctrl+V** pastes an image from the clipboard · dropping image files attaches them · delete an `[Image #N]` token to remove it\n- **Drag** to copy a selection · **click** a code block to copy it · **click** a link to open it · **double-click** copies a whole message\n- **/** opens the command menu · **↑↓** choose · **Tab** completes · **Enter** runs\n- `/SKILL` runs one of your skills from `~/.agents/skills`, with what follows as its input · `/skill:SKILL` when a command already has its name\n- `/copy` copies the last reply · `/copy code` its last code block\n- `/review` reviews your uncommitted changes · `/review WHAT` reviews what you name · you decide each finding in a card\n- `/memory` lists what JULIA remembers here · `/memory NAME` shows a note · `/memory forget NAME` deletes it\n- `/memory pin NAME` shows a note in full every turn · `/memory unpin NAME` undoes it\n- **Ctrl+P** or `/model` opens model search · **Ctrl+N** cycles your enabled models\n- **Shift+Tab** outside the picker cycles reasoning · `/reasoning` opens its picker\n- `/model provider/ID` and `/reasoning LEVEL|default` set them directly\n- `/profile` switches configuration profile · `/profile NAME` switches to one directly\n- **Ctrl+C** stops the task and exits safely\n\n`/clear` starts a fresh conversation · `/quit` exits\n\nSettings apply to the next turn. While a task runs, **Enter** queues your message: it is sent when JULIA settles, or at once with **Ctrl+S** or the steer button, which hands it to the task under way. **↑** in an empty composer takes a queued message back to edit."})
		case "/ask", "/ask text":
			if !m.cfg.Demo {
				m.notice = "Unknown command. /help lists the essentials."
				return nil
			}
			m.input.Reset()
			return m.openQuestion(QuestionRequest{Question: demoQuestion(prompt == "/ask text")}, true)
		case "/clear":
			next := sessions.New(m.cfg.Session.Directory, m.cfg.Session.Provider, m.cfg.Session.Model)
			next.ConfigPath = m.cfg.Session.ConfigPath
			next.ConfigProfile = m.cfg.Session.ConfigProfile
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
			cmd = m.deleteThumbnails()
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
		return cmd
	}
	if m.cfg.Demo {
		m.notice = "Offline preview. Restart without --demo to begin a real session."
		return nil
	}
	request := m.takeImages(prompt)
	m.input.Reset()
	return m.send(request)
}

// send starts a turn with request.
func (m *model) send(request core.Prompt) tea.Cmd {
	m.invalidateReasoningCycle()
	m.inbox = core.NewInbox()
	request.Steering = m.inbox
	m.notice = ""
	m.busy = true
	m.status = "Thinking"
	m.started, m.now = time.Now(), time.Now()
	sent := entry{role: "user", text: request.Text, images: request.Images}
	m.registerImages(&sent)
	m.turnStart = len(m.entries)
	m.entries = append(m.entries, sent)
	m.refresh(true)
	ctx, cancel := context.WithCancel(m.lifetime)
	m.cancel = cancel
	m.stream = make(chan streamMsg, 128)
	stream := m.stream
	session := cloneSession(m.cfg.Session)
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		err := m.cfg.Backend.Run(ctx, session, request, m.cfg.Tools, func(event core.Event) {
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
	return tea.Batch(waitStream(stream, m.lifetime), m.spinner.start(), m.uploadThumbnails(sent.imageIDs...))
}

// memoryCommand answers /memory: alone it lists the notes this workspace
// sees, with a name it shows that note, "forget NAME" deletes it, and "pin
// NAME" or "unpin NAME" pins it for every task or unpins it. The
// Markdown the hook returns joins the conversation under its own heading, so
// what JULIA remembers reads like any other reply; an error is a notice.
// memoryActions are the words /memory takes before a note's name.
var memoryActions = []string{"forget", "pin", "unpin"}

func (m *model) memoryCommand(prompt string) bool {
	parts := strings.Fields(prompt)
	if parts[0] != "/memory" {
		return false
	}
	var action, name string
	switch {
	case len(parts) == 1:
	case len(parts) == 2 && !slices.Contains(memoryActions, parts[1]):
		action, name = "show", parts[1]
	case len(parts) == 3 && slices.Contains(memoryActions, parts[1]):
		action, name = parts[1], parts[2]
	default:
		m.notice = "Use /memory, /memory NAME, /memory forget NAME or /memory pin|unpin NAME."
		return true
	}
	if m.cfg.Memory == nil {
		m.notice = "Memory is not configured."
		if m.cfg.Demo {
			m.notice = "Memory is unavailable in this preview."
		}
		return true
	}
	text, err := m.cfg.Memory(action, name)
	if err != nil {
		m.notice = "Memory: " + oneLine(err.Error())
		return true
	}
	m.input.Reset()
	m.entries = append(m.entries, entry{role: "assistant", label: "✦ MEMORY", text: text})
	m.refresh(true)
	return true
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
	if event.Subagent != nil {
		ref := *event.Subagent
		event.Subagent = &ref
	}
	return event
}

// applyEvent shows a provider event. It returns the upload of a thumbnail when
// a tool's result brought an image.
func (m *model) applyEvent(event core.Event) tea.Cmd {
	if event.Subagent != nil {
		m.applyAgentEvent(event)
		return nil
	}
	var cmd tea.Cmd
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
	case "steer":
		cmd = m.receiveSteer()
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
			if m.entries[i].role == "tool" && !m.entries[i].done && m.entries[i].isCall(event.Call) {
				m.entries[i].done = true
				m.entries[i].result = event.Result
				m.entries[i].revision++
				if images := resultImages(event.Result); images != nil {
					m.entries[i].images = images
					m.registerImages(&m.entries[i])
					cmd = m.uploadThumbnails(m.entries[i].imageIDs...)
				}
				break
			}
		}
		m.status = "Thinking"
	case "usage":
		m.recordUsage(event)
	}
	m.refresh(false)
	return cmd
}

// isCall reports whether a tool entry is the call an event names. An event that
// names no call means the latest one, so every entry is a candidate for it.
func (item entry) isCall(call *core.ToolCall) bool {
	return call == nil || item.call != nil && item.call.ID == call.ID
}

// resultImages is what a tool entry draws of its result: a result carries one
// image (see core.MaxToolImages), and a replayed one has no pixels to draw.
func resultImages(result *core.ToolResult) []core.Image {
	if result == nil || len(result.Images) == 0 || len(result.Images[0].Data) == 0 {
		return nil
	}
	return result.Images[:1]
}

func (m *model) loadHistory() {
	if m.cfg.Session == nil {
		return
	}
	for _, message := range m.cfg.Session.Messages {
		switch message.Role {
		case "user", "assistant":
			if message.Content != "" {
				text := message.Content
				if message.Role == "user" {
					// A skill the user ran shows as the command they typed.
					text = skills.Typed(text)
				}
				item := entry{role: message.Role, text: text, images: message.Images}
				m.registerImages(&item)
				m.entries = append(m.entries, item)
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

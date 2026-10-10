package ui

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"jarvis/internal/core"
)

func testModel(t *testing.T, cfg Config) *model {
	t.Helper()
	if cfg.Session == nil {
		cfg.Session = &core.Session{ID: "test", Directory: "/tmp/jarvis", Provider: "fake"}
	}
	if cfg.Mode == "" {
		cfg.Mode = "ask"
	}
	m := newModel(cfg)
	t.Cleanup(func() {
		m.stop()
		if m.cancel != nil {
			m.cancel()
		}
		m.wg.Wait()
	})
	return m
}

func TestUntrustedTextCannotControlTerminal(t *testing.T) {
	for _, input := range []string{
		"normal\x1b]52;c;Y2xpcGJvYXJk\a text",
		"normal\x1b[2J\x1b[H text",
		"normal\u202e text\u2066",
		"normal\r\b\x00 text",
	} {
		got := sanitize(input)
		if strings.ContainsAny(got, "\x1b\x00\r\b\u202e\u2066\a") {
			t.Errorf("unsafe output %q", got)
		}
		if !strings.Contains(got, "normal") {
			t.Errorf("lost normal text: %q", got)
		}
	}
	if got := sanitize("line 1\n\tline 2"); got != "line 1\n\tline 2" {
		t.Fatalf("lost formatting: %q", got)
	}
}

func TestViewsFitTerminalAndKeepComposerVisible(t *testing.T) {
	for _, size := range [][2]int{{40, 15}, {60, 20}, {100, 30}, {120, 40}, {160, 50}} {
		for _, demo := range []bool{false, true} {
			m := testModel(t, Config{Demo: demo})
			m.resize(size[0], size[1])
			view := m.View().Content
			lines := strings.Split(view, "\n")
			if len(lines) > size[1] {
				t.Errorf("%v demo %v: %d rows", size, demo, len(lines))
			}
			for _, line := range lines {
				if ansi.StringWidth(line) > size[0] {
					t.Errorf("%v: overflow row %q", size, line)
				}
			}
			if !strings.Contains(view, "/help") {
				t.Errorf("%v demo %v: footer hidden", size, demo)
			}
			if !strings.Contains(view, "MESSAGE") {
				t.Errorf("%v demo %v: composer hidden", size, demo)
			}
		}
	}
}

func TestStreamingDoesNotYankScrolledHistory(t *testing.T) {
	m := testModel(t, Config{})
	m.resize(70, 20)
	m.entries = []entry{{role: "assistant", text: strings.Repeat("A line in the transcript.\n\n", 60)}}
	m.refresh(true)
	m.viewport.ScrollUp(12)
	before := m.viewport.YOffset()
	m.applyEvent(core.Event{Kind: "text", Text: "Another line.\n"})
	if after := m.viewport.YOffset(); after != before {
		t.Errorf("scrolled reader moved from %d to %d", before, after)
	}
	m.viewport.GotoBottom()
	m.applyEvent(core.Event{Kind: "text", Text: strings.Repeat("New live output.\n", 12)})
	if !m.viewport.AtBottom() {
		t.Fatal("live reader did not follow output")
	}
}

func TestResizeKeepsLiveReaderAtTheLatestOutput(t *testing.T) {
	m := testModel(t, Config{Demo: true})
	m.resize(128, 40)
	if !m.viewport.AtBottom() {
		t.Fatal("expected live follow before resize")
	}
	m.resize(50, 20)
	if !m.viewport.AtBottom() {
		t.Fatal("narrow resize lost live follow")
	}
	if !strings.Contains(strings.Join(strings.Fields(ansi.Strip(m.viewport.View())), " "), "No files changed") {
		t.Fatal("latest output hidden after resize")
	}
	m.viewport.ScrollUp(3)
	before := m.viewport.YOffset()
	m.resize(52, 20)
	if m.viewport.YOffset() != before {
		t.Fatal("resize yanked a reader scrolled into history")
	}
}

func TestBusySubmissionRetainsDraft(t *testing.T) {
	m := testModel(t, Config{})
	m.busy = true
	m.input.SetValue("Please check one more thing")
	if cmd := m.submit(); cmd != nil {
		t.Fatal("started a second task")
	}
	if m.input.Value() != "Please check one more thing" {
		t.Fatal("draft was lost")
	}
	if !strings.Contains(m.notice, "Draft kept") {
		t.Fatal("missing busy feedback")
	}
}

type cancellingBackend struct {
	started chan struct{}
	stopped chan struct{}
}

func (b *cancellingBackend) Name() string { return "fake" }
func (b *cancellingBackend) Run(ctx context.Context, session *core.Session, prompt string, _ core.ToolExecutor, emit core.Emit) error {
	session.Messages = append(session.Messages, core.Message{Role: "user", Content: prompt})
	close(b.started)
	emit(core.Event{Kind: "text", Text: "Thinking"})
	<-ctx.Done()
	close(b.stopped)
	return ctx.Err()
}

func TestQuitCancelsBackendAndWaitsForItsCompletion(t *testing.T) {
	b := &cancellingBackend{started: make(chan struct{}), stopped: make(chan struct{})}
	saved := false
	m := testModel(t, Config{Backend: b, Save: func(s *core.Session) error { saved = true; return nil }})
	m.input.SetValue("Research cancellation")
	m.submit()
	select {
	case <-b.started:
	case <-time.After(time.Second):
		t.Fatal("backend did not start")
	}
	if len(m.cfg.Session.Messages) != 0 {
		t.Fatal("backend mutated live UI session before handoff")
	}
	_, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if cmd != nil {
		t.Fatal("quit returned before backend completion")
	}
	if !m.quitting || !m.busy {
		t.Fatal("missing stopping state")
	}
	select {
	case <-b.stopped:
	case <-time.After(time.Second):
		t.Fatal("backend did not cancel")
	}
	deadline := time.After(time.Second)
	for m.busy {
		select {
		case event := <-m.stream:
			_, cmd = m.Update(event)
		case <-deadline:
			t.Fatal("completion was not delivered")
		}
	}
	if cmd == nil {
		t.Fatal("did not quit after completion")
	}
	if !saved || len(m.cfg.Session.Messages) != 1 {
		t.Fatal("cancelled session was not saved")
	}
}

func TestApprovalBridgeDenialAndCancellation(t *testing.T) {
	approve, requests := NewApprover()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan core.Decision, 1)
	go func() {
		decision, _ := approve(ctx, core.Approval{Call: core.ToolCall{Name: "Write"}})
		result <- decision
	}()
	request := <-requests
	reply(request, false, "user declined")
	select {
	case decision := <-result:
		if decision.Allow || decision.Reason != "user declined" {
			t.Fatal("wrong decision", decision)
		}
	case <-time.After(time.Second):
		t.Fatal("decision blocked")
	}
	cancel()
	if _, err := approve(ctx, core.Approval{}); err != context.Canceled {
		t.Fatalf("expected cancellation, got %v", err)
	}
}

func TestEnterApprovesPendingRequestInsteadOfSendingDraft(t *testing.T) {
	m := testModel(t, Config{})
	m.busy = true
	m.pending = &ApprovalRequest{Approval: core.Approval{Call: core.ToolCall{Name: "Bash", Arguments: json.RawMessage(`{"command":"example"}`)}}, Reply: make(chan core.Decision, 1)}
	replies := m.pending.Reply
	m.input.SetValue("draft")
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	decision := <-replies
	if !decision.Allow {
		t.Fatal("Enter must approve the pending action")
	}
	if m.input.Value() != "draft" {
		t.Fatal("approval consumed draft")
	}
}

func TestSessionCloneIsolatedToolArguments(t *testing.T) {
	original := &core.Session{Messages: []core.Message{{ToolCalls: []core.ToolCall{{Arguments: json.RawMessage(`{"path":"a"}`)}}}}}
	copy := cloneSession(original)
	copy.Messages[0].ToolCalls[0].Arguments[9] = 'b'
	if string(original.Messages[0].ToolCalls[0].Arguments) != `{"path":"a"}` {
		t.Fatal("tool argument storage was shared")
	}
}

func TestApprovalPanelPreservesCompleteActionAndNetworkRequest(t *testing.T) {
	m := testModel(t, Config{})
	m.resize(40, 15)
	command := strings.Repeat("long-command-argument ", 70) + "IMPORTANT-COMMAND-TAIL"
	arguments, err := json.Marshal(map[string]any{"command": command, "network": true})
	if err != nil {
		t.Fatal(err)
	}
	m.pending = &ApprovalRequest{Approval: core.Approval{Call: core.ToolCall{Name: "Bash", Arguments: arguments}, Reason: "Review this exact action", Network: true}, Reply: make(chan core.Decision, 1)}
	m.refresh(true)
	panel := ansi.Strip(m.approvalView())
	if !strings.Contains(panel, "Network access requested") {
		t.Fatal("network grant is not disclosed")
	}
	// Wrapping is allowed; truncating the action is not.
	panel = strings.Map(func(r rune) rune {
		if r >= 0x2500 && r <= 0x257f {
			return -1
		}
		return r
	}, panel)
	flattened := strings.Join(strings.Fields(panel), "")
	if !strings.Contains(flattened, "IMPORTANT-COMMAND-TAIL") {
		t.Fatal("approval panel truncated the command tail")
	}
	if m.viewport.AtBottom() {
		t.Fatal("long approval has no scrollable overflow")
	}
	m.viewport.GotoBottom()
	if !strings.Contains(ansi.Strip(m.View().Content), "y/enter allow") {
		t.Fatal("fixed approval controls unavailable while scrolling")
	}
}

func TestUnsandboxedApprovalDisclosesExactScopeAndDirectory(t *testing.T) {
	for _, size := range [][2]int{{40, 15}, {100, 30}} {
		m := testModel(t, Config{})
		m.resize(size[0], size[1])
		command := "printf '%s' " + strings.Repeat("argument ", 40) + "IMPORTANT-COMMAND-TAIL"
		arguments, err := json.Marshal(map[string]any{"command": command, "sandbox_permissions": "require_escalated"})
		if err != nil {
			t.Fatal(err)
		}
		m.pending = &ApprovalRequest{Approval: core.Approval{
			Call:          core.ToolCall{Name: "Bash", Arguments: arguments},
			ExecutionMode: "unsandboxed", Directory: "/actual/working/directory/outside/workspace",
			Justification: "Need host access\x1b]52;c;c2VjcmV0\a to run this exact command.",
			Reason:        "Review the requested execution scope", Network: true,
		}, Reply: make(chan core.Decision, 1)}
		m.refresh(true)
		panel := strings.Map(func(r rune) rune {
			if r >= 0x2500 && r <= 0x257f {
				return -1
			}
			return r
		}, ansi.Strip(m.approvalView()))
		flat := strings.Join(strings.Fields(panel), "")
		for _, want := range []string{"OUTSIDE SANDBOX", "Host-user filesystem access", "Unrestricted network access", "no sudo granted", "Working directory", "/actual/working/directory/outside/workspace", "Justification", "Need host access to run this exact command.", "IMPORTANT-COMMAND-TAIL"} {
			if !strings.Contains(flat, strings.Join(strings.Fields(want), "")) {
				t.Errorf("%v: approval omitted %q", size, want)
			}
		}
		if strings.Contains(m.approvalView(), "52;c;") || strings.Contains(m.approvalView(), "c2VjcmV0") {
			t.Fatal("justification leaked a terminal-control payload")
		}
		if strings.Contains(panel, "Network access requested") {
			t.Fatal("duplicate network disclosure")
		}
	}
}

func TestSandboxedApprovalKeepsExistingNetworkDisclosure(t *testing.T) {
	m := testModel(t, Config{})
	m.pending = &ApprovalRequest{Approval: core.Approval{ExecutionMode: "sandboxed", Directory: "/workspace", Justification: "inside workspace", Call: core.ToolCall{Name: "Bash", Arguments: json.RawMessage(`{"command":"curl https://example.com"}`)}, Reason: "Network needs approval", Network: true}, Reply: make(chan core.Decision, 1)}
	panel := ansi.Strip(m.approvalView())
	if !strings.Contains(panel, "Network access requested") || !strings.Contains(panel, "curl https://example.com") {
		t.Fatal("normal approval lost its network or command disclosure")
	}
	for _, unexpected := range []string{"OUTSIDE SANDBOX", "Unrestricted network access", "Working directory", "Justification"} {
		if strings.Contains(panel, unexpected) {
			t.Fatalf("normal approval gained escalation-only disclosure %q", unexpected)
		}
	}
}

func TestClearArchivesOldSessionAndResetsAllProviderState(t *testing.T) {
	old := &core.Session{ID: "old-id", Directory: "/workspace", Provider: "codex", Model: "selected", RemoteID: "remote-thread", State: json.RawMessage(`{"history":"old"}`), PendingTool: &core.ToolCall{Name: "Bash", Arguments: json.RawMessage(`{"command":"old"}`)}, Messages: []core.Message{{Role: "user", Content: "old task"}}}
	old.Reasoning = "medium"
	var saved []*core.Session
	m := testModel(t, Config{Session: old, Save: func(s *core.Session) error { saved = append(saved, cloneSession(s)); return nil }})
	m.input.SetValue("/clear")
	m.submit()
	if len(saved) != 2 || saved[0].ID != "old-id" || len(saved[0].Messages) != 1 {
		t.Fatal("previous conversation was not archived")
	}
	current := m.cfg.Session
	if current.ID == "old-id" || current.ID == "" {
		t.Fatal("new conversation reused old session ID")
	}
	if current.RemoteID != "" || len(current.State) != 0 || current.PendingTool != nil || len(current.Messages) != 0 {
		t.Fatal("old provider state survived clear")
	}
	if current.Directory != "/workspace" || current.Provider != "codex" || current.Model != "selected" || current.Reasoning != "medium" {
		t.Fatal("session settings lost")
	}
	if saved[1].ID != current.ID {
		t.Fatal("new session was not persisted")
	}
}

func TestModelAndReasoningVisibleAcrossLayouts(t *testing.T) {
	for _, size := range [][2]int{{100, 30}, {128, 40}} {
		m := testModel(t, Config{Session: &core.Session{ID: "test", Provider: "codex", Model: "gpt-6-astra", Reasoning: "medium"}})
		m.resize(size[0], size[1])
		view := ansi.Strip(m.View().Content)
		if !strings.Contains(view, "gpt-6-astra") || !strings.Contains(view, "medium") {
			t.Fatalf("model/effort hidden at %v", size)
		}
		m.cfg.Session.Reasoning = ""
		if !strings.Contains(ansi.Strip(m.View().Content), "inherited") {
			t.Fatal("default reasoning not labeled as inherited")
		}
	}
}

func TestCompatibleReasoningVisibleAcrossLayouts(t *testing.T) {
	for _, size := range [][2]int{{100, 30}, {128, 40}} {
		m := testModel(t, Config{
			Session:             &core.Session{ID: "test", Provider: "compatible", Model: "vendor/model", Reasoning: "high"},
			CompatibleReasoning: true,
		})
		m.resize(size[0], size[1])
		view := ansi.Strip(m.View().Content)
		if !strings.Contains(view, "high") {
			t.Fatalf("compatible effort hidden at %v", size)
		}

		m.cfg.Session.Reasoning = ""
		view = ansi.Strip(m.View().Content)
		if !strings.Contains(view, "provider default") || strings.Contains(view, "inherited") {
			t.Fatalf("compatible default label is wrong at %v: %s", size, view)
		}
	}
}

func TestClearKeepsConversationWhenSavingFails(t *testing.T) {
	m := testModel(t, Config{Save: func(*core.Session) error { return errors.New("disk full") }})
	m.cfg.Session.Messages = []core.Message{{Role: "user", Content: "keep this"}}
	before := m.cfg.Session.ID
	m.input.SetValue("/clear")
	m.submit()
	if m.cfg.Session.ID != before || len(m.cfg.Session.Messages) != 1 {
		t.Fatal("clear discarded unsaved conversation")
	}
}

func TestSessionCloneIsolatesProviderState(t *testing.T) {
	original := &core.Session{State: json.RawMessage(`{"state":1}`), PendingTool: &core.ToolCall{Name: "Read", Arguments: json.RawMessage(`{"path":"a"}`)}}
	original.Audit = []core.AuditEntry{{Event: core.Event{Kind: "tool_end", Call: &core.ToolCall{Name: "Read", Arguments: json.RawMessage(`{"path":"a"}`)}, Result: &core.ToolResult{Content: "original"}}}}
	copy := cloneSession(original)
	copy.State[9] = '2'
	copy.PendingTool.Name = "Write"
	copy.PendingTool.Arguments[9] = 'b'
	copy.Audit[0].Event.Call.Arguments[9] = 'b'
	copy.Audit[0].Event.Result.Content = "changed"
	if string(original.State) != `{"state":1}` || original.PendingTool.Name != "Read" || string(original.PendingTool.Arguments) != `{"path":"a"}` {
		t.Fatal("provider state was shared")
	}
	if string(original.Audit[0].Event.Call.Arguments) != `{"path":"a"}` || original.Audit[0].Event.Result.Content != "original" {
		t.Fatal("audit event storage was shared")
	}
}

package ui

import (
	"encoding/json"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"julia/internal/core"
)

func TestPendingApprovalConsumesEveryKeyAndOnlyYOrEnterAllows(t *testing.T) {
	for _, tc := range []struct {
		name  string
		key   tea.KeyPressMsg
		allow bool
	}{
		{"y", tea.KeyPressMsg{Code: 'y'}, true},
		{"enter", tea.KeyPressMsg{Code: tea.KeyEnter}, true},
		{"n", tea.KeyPressMsg{Code: 'n'}, false},
		{"escape", tea.KeyPressMsg{Code: tea.KeyEscape}, false},
		{"other letter", tea.KeyPressMsg{Code: 'x'}, false},
		{"uppercase Y", tea.KeyPressMsg{Code: 'Y'}, false},
		{"space", tea.KeyPressMsg{Code: tea.KeySpace}, false},
		{"tab", tea.KeyPressMsg{Code: tea.KeyTab}, false},
		{"shift tab", tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift}, false},
		{"page up", tea.KeyPressMsg{Code: tea.KeyPgUp}, false},
		{"page down", tea.KeyPressMsg{Code: tea.KeyPgDown}, false},
		{"history end", tea.KeyPressMsg{Code: tea.KeyEnd, Mod: tea.ModCtrl}, false},
		{"tool expansion", tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl}, false},
		{"model picker", tea.KeyPressMsg{Code: 'p', Mod: tea.ModCtrl}, false},
		{"alt enter", tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModAlt}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := testModel(t, Config{})
			m.busy = true
			cancelled := false
			m.cancel = func() { cancelled = true }
			m.input.SetValue("retained draft")
			replies := make(chan core.Decision, 2)
			m.pending = &ApprovalRequest{Approval: core.Approval{Call: core.ToolCall{Name: "Bash"}}, Reply: replies}
			m.Update(tc.key)
			select {
			case decision := <-replies:
				if decision.Allow != tc.allow {
					t.Fatalf("allow=%v, want %v", decision.Allow, tc.allow)
				}
			default:
				t.Fatal("key did not resolve the approval")
			}
			if m.pending != nil || m.input.Value() != "retained draft" || m.expandTools || m.picker != nil || cancelled {
				t.Fatal("approval key leaked into another action")
			}
			m.decide(!tc.allow)
			select {
			case <-replies:
				t.Fatal("approval replied more than once")
			default:
			}
		})
	}
}

func TestApprovalCtrlCDeniesAndCancelsBeforeExit(t *testing.T) {
	for _, busy := range []bool{true, false} {
		m := testModel(t, Config{})
		m.busy = busy
		cancelled := false
		m.cancel = func() { cancelled = true }
		replies := make(chan core.Decision, 2)
		m.pending = &ApprovalRequest{Reply: replies}
		_, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
		if decision := <-replies; decision.Allow {
			t.Fatal("Ctrl+C allowed pending request")
		}
		if !m.quitting || m.pending != nil || cancelled != busy {
			t.Fatal("Ctrl+C lost existing cancellation/exit behavior")
		}
		if busy && cmd != nil {
			t.Fatal("busy quit stopped waiting for backend cancellation")
		}
		if !busy && cmd == nil {
			t.Fatal("idle quit did not exit")
		}
		m.cancelTask()
		select {
		case <-replies:
			t.Fatal("cancellation replied twice")
		default:
		}
	}
}

func TestApprovalMouseWheelInspectsWithoutDeciding(t *testing.T) {
	m := testModel(t, Config{})
	m.busy = true
	m.resize(40, 15)
	arguments, _ := json.Marshal(map[string]string{"command": strings.Repeat("inspect this complete command ", 100)})
	replies := make(chan core.Decision, 1)
	m.pending = &ApprovalRequest{Approval: core.Approval{Call: core.ToolCall{Name: "Bash", Arguments: arguments}}, Reply: replies}
	m.refresh(true)
	before := m.viewport.YOffset()
	m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	if m.pending == nil || m.viewport.YOffset() <= before {
		t.Fatal("mouse wheel did not inspect pending request")
	}
	select {
	case <-replies:
		t.Fatal("mouse scrolling decided the request")
	default:
	}
	view := strings.ToLower(ansi.Strip(m.approvalView() + "\n" + m.footer()))
	if !strings.Contains(view, "mouse wheel") || strings.Contains(view, "pgup") || strings.Contains(view, "pgdown") {
		t.Fatal("approval advertises keys that now deny")
	}
}

func TestEndedTaskCannotLeaveAnApprovableRequest(t *testing.T) {
	m := testModel(t, Config{})
	replies := make(chan core.Decision, 2)
	m.Update(approvalMsg{request: ApprovalRequest{Reply: replies}})
	if decision := <-replies; decision.Allow {
		t.Fatal("ended task accepted late approval")
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.pending != nil {
		t.Fatal("late request remained approvable")
	}
	select {
	case <-replies:
		t.Fatal("Enter replied to an ended request")
	default:
	}
}

func TestApprovalHintsFitAndExplainNewKeys(t *testing.T) {
	for _, size := range [][2]int{{30, 15}, {40, 15}, {100, 30}} {
		m := testModel(t, Config{})
		m.resize(size[0], size[1])
		m.pending = &ApprovalRequest{Reply: make(chan core.Decision, 1)}
		m.refresh(true)
		view := strings.ToLower(ansi.Strip(m.View().Content))
		if !strings.Contains(view, "enter") || !strings.Contains(view, "allow") || !strings.Contains(view, "den") {
			t.Fatalf("%v: approval hints hidden", size)
		}
		if len(strings.Split(view, "\n")) > size[1] {
			t.Fatal("approval exceeds terminal height")
		}
		for _, line := range strings.Split(view, "\n") {
			if ansi.StringWidth(line) > size[0] {
				t.Fatal("approval exceeds terminal width")
			}
		}
	}
}

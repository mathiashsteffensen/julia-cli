package ui

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"julia/internal/core"

	tea "charm.land/bubbletea/v2"
)

// finishTurn ends the turn under way with err and returns what its end writes
// to the terminal.
func finishTurn(t *testing.T, m *model, b *turnBackend, err error) string {
	t.Helper()
	stream := m.stream
	b.end <- err
	deadline := time.After(2 * time.Second)
	for {
		select {
		case msg := <-stream:
			_, cmd := m.Update(msg)
			if msg.finished {
				return rawOutput(cmd)
			}
		case <-deadline:
			t.Fatal("the turn's end was not delivered")
		}
	}
}

func TestFocusedTerminalIsNotNotified(t *testing.T) {
	m, b := busyModel(t, false)
	if !m.View().ReportFocus {
		t.Fatal("the view must ask for focus reports")
	}
	m.Update(tea.BlurMsg{})
	m.Update(tea.FocusMsg{})
	if out := finishTurn(t, m, b, nil); out != "" {
		t.Fatalf("a focused terminal was notified: %q", out)
	}
}

func TestTurnThatSettlesInTheBackgroundNotifies(t *testing.T) {
	m, b := busyModel(t, false)
	m.Update(tea.BlurMsg{})
	m.entries = append(m.entries, entry{role: "assistant", text: "\n**The build is fixed.**\n\nThe linker flag was stale."})
	out := finishTurn(t, m, b, nil)
	if want := "\x1b]9;JULIA is done · The build is fixed.\x07\x07"; out != want {
		t.Fatalf("notification = %q, want %q", out, want)
	}
}

func TestTurnWithoutAReplyNotifiesPlainly(t *testing.T) {
	m, b := busyModel(t, false)
	m.Update(tea.BlurMsg{})
	if out := finishTurn(t, m, b, nil); !strings.Contains(out, "]9;JULIA is done\x07") {
		t.Fatalf("notification = %q", out)
	}
}

func TestFailedTurnNotifiesWithTheError(t *testing.T) {
	m, b := busyModel(t, false)
	m.Update(tea.BlurMsg{})
	if out := finishTurn(t, m, b, errors.New("provider unreachable")); !strings.Contains(out, "]9;JULIA stopped · provider unreachable\x07") {
		t.Fatalf("notification = %q", out)
	}
}

func TestQueuedMessageKeepsTheTurnFromNotifying(t *testing.T) {
	m, _ := busyModel(t, false)
	m.Update(tea.BlurMsg{})
	sendText(m, "then update the changelog")
	// The queued message is sent as the next turn, so JULIA does not need the user yet.
	if cmd := m.notifySettled(nil); cmd != nil {
		t.Fatalf("notified though a queued message follows: %q", rawOutput(cmd))
	}
}

func TestApprovalInTheBackgroundNotifies(t *testing.T) {
	m, _ := busyModel(t, false)
	m.Update(tea.BlurMsg{})
	arguments, _ := json.Marshal(map[string]string{"command": "git push origin master"})
	request := ApprovalRequest{Approval: core.Approval{Call: core.ToolCall{Name: "Bash", Arguments: arguments}}, Reply: make(chan core.Decision, 1)}
	_, cmd := m.Update(approvalMsg{request})
	if out := rawOutput(cmd); !strings.Contains(out, "]9;JULIA needs your approval · Bash · git push origin master\x07") {
		t.Fatalf("notification = %q", out)
	}
}

func TestQuestionAndReviewInTheBackgroundNotify(t *testing.T) {
	m, _ := busyModel(t, false)
	m.Update(tea.BlurMsg{})
	_, cmd := m.Update(questionMsg{QuestionRequest{Question: sampleChoices, Reply: make(chan core.Answer, 1)}})
	if out := rawOutput(cmd); !strings.Contains(out, "]9;JULIA asks · "+sampleChoices.Text+"\x07") {
		t.Fatalf("question notification = %q", out)
	}
	review := &core.Review{Findings: []core.Finding{{}, {}, {}}}
	if out := rawOutput(m.notifyQuestion(QuestionRequest{Review: review})); !strings.Contains(out, "]9;JULIA has 3 review findings for you to triage\x07") {
		t.Fatalf("review notification = %q", out)
	}
}

func TestNotificationCannotCarryTerminalControl(t *testing.T) {
	m := testModel(t, Config{})
	m.Update(tea.BlurMsg{})
	out := rawOutput(m.notify("JULIA asks · ok\x1b]52;c;Y2xpcA==\a\x1b[2J\nnext‮line " + strings.Repeat("x", 400)))
	body, ok := strings.CutPrefix(out, "\x1b]9;")
	if !ok || !strings.HasSuffix(body, "\x07\x07") {
		t.Fatalf("not one OSC 9 and a bell: %q", out)
	}
	body = strings.TrimSuffix(body, "\x07\x07")
	if strings.ContainsAny(body, "\x1b\x07\n‮") || len([]rune(body)) > notificationWidth {
		t.Fatalf("notification body carries control or runs long: %q", body)
	}
}

func TestNotificationPassesThroughTmux(t *testing.T) {
	m := testModel(t, Config{})
	m.Update(tea.EnvMsg{"TMUX=/tmp/tmux-501/default,1,0"})
	m.Update(tea.BlurMsg{})
	if out, want := rawOutput(m.notify("JULIA is done")), "\x1bPtmux;\x1b\x1b]9;JULIA is done\x07\x1b\\\x07"; out != want {
		t.Fatalf("notification = %q, want %q", out, want)
	}
}

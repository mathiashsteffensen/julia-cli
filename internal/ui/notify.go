package ui

import (
	"context"
	"errors"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"julia/internal/core"
)

// While its terminal is in the background, JULIA tells the user when it has
// settled and is waiting on them: a turn has ended with nothing queued to
// follow it, or an approval or a question is on screen. A focused terminal is
// left alone; the user is already looking.
//
// The notification is an OSC 9 desktop notification, which Ghostty, iTerm2,
// WezTerm and kitty show, followed by a bell for the terminals that show none,
// such as Terminal.app. Inside tmux the notification is passed through to the
// outer terminal, which needs tmux's allow-passthrough; focus reports need its
// focus-events.

// notificationWidth caps a notification's text, in cells; the system cuts
// a long one off anyway, and the start is what says what happened.
const notificationWidth = 140

// notify alerts the user with text when the terminal is not focused.
func (m *model) notify(text string) tea.Cmd {
	if !m.blurred {
		return nil
	}
	sequence := ansi.Notify(notificationText(text))
	if m.tmux {
		sequence = ansi.TmuxPassthrough(sequence)
	}
	return tea.Raw(sequence + "\a")
}

// notifySettled announces the end of a turn, unless what was queued during it
// is about to be sent as the next one. It must run before settleQueue takes
// the queue.
func (m *model) notifySettled(err error) tea.Cmd {
	switch {
	case errors.Is(err, context.Canceled):
		return m.notify("JULIA stopped: the task was interrupted")
	case err != nil:
		return m.notify("JULIA stopped · " + err.Error())
	case len(m.queued) > 0:
		return nil
	}
	if reply := m.turnReply(); reply != "" {
		return m.notify("JULIA is done · " + reply)
	}
	return m.notify("JULIA is done")
}

// notifyApproval announces an approval prompt.
func (m *model) notifyApproval(approval core.Approval) tea.Cmd {
	return m.notify("JULIA needs your approval · " + approval.Call.Name + " · " + toolSummary(approval.Call))
}

// notifyQuestion announces a question card, or a review card.
func (m *model) notifyQuestion(request QuestionRequest) tea.Cmd {
	if request.Review != nil {
		n := len(request.Review.Findings)
		if n == 1 {
			return m.notify("JULIA has a review finding for you to triage")
		}
		return m.notify(fmt.Sprintf("JULIA has %d review findings for you to triage", n))
	}
	return m.notify("JULIA asks · " + request.Question.Text)
}

// turnReply is the first line of the last reply of the turn that just ended,
// without its Markdown marks, or "" when it gave none.
func (m *model) turnReply() string {
	if m.turnStart < 0 || m.turnStart > len(m.entries) {
		return ""
	}
	for i := len(m.entries) - 1; i >= m.turnStart; i-- {
		if e := m.entries[i]; e.role == "assistant" && strings.TrimSpace(e.text) != "" {
			for line := range strings.Lines(e.text) {
				if line = strings.TrimLeft(markdownMarks.Replace(strings.TrimSpace(line)), "#> "); line != "" {
					return line
				}
			}
		}
	}
	return ""
}

// markdownMarks are the Markdown marks a reply's first line commonly holds,
// which a notification would show literally.
var markdownMarks = strings.NewReplacer("**", "", "__", "", "`", "")

// notificationText makes text safe and short for a notification: one line,
// with no escape sequences or control characters, which could end the OSC
// early and reach the terminal.
func notificationText(text string) string {
	return ansi.Truncate(strings.Join(strings.Fields(sanitize(text)), " "), notificationWidth, "…")
}

package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"jarvis/internal/core"
)

func keyMsg(name string) tea.KeyPressMsg {
	switch name {
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	case "up":
		return tea.KeyPressMsg{Code: tea.KeyUp}
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	case "tab":
		return tea.KeyPressMsg{Code: tea.KeyTab}
	case "shift+tab":
		return tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift}
	case "alt+enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModAlt}
	case "ctrl+c":
		return tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}
	}
	r := []rune(name)
	return tea.KeyPressMsg{Code: r[0], Text: name}
}

func press(m *model, names ...string) {
	for _, name := range names {
		m.Update(keyMsg(name))
	}
}

func typeText(m *model, text string) {
	for _, r := range text {
		m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
}

var sampleChoices = core.Question{
	Text: "Which storage backend should the session index use?",
	Options: []core.QuestionOption{
		{Label: "SQLite (Recommended)", Description: "Embedded and zero-config."},
		{Label: "Postgres", Description: "Shared server; needs a running instance."},
		{Label: "Flat JSON files"},
	},
}

// askModel opens a question the way the stream does and disarms the typing guard.
func askModel(t *testing.T, question core.Question) (*model, chan core.Answer) {
	t.Helper()
	m := testModel(t, Config{})
	m.resize(100, 40)
	m.busy = true
	replies := make(chan core.Answer, 2)
	m.Update(questionMsg{QuestionRequest{Question: question, Reply: replies}})
	if m.question == nil {
		t.Fatal("question was not opened")
	}
	m.question.armedAt = time.Time{}
	return m, replies
}

func mustAnswer(t *testing.T, replies chan core.Answer) core.Answer {
	t.Helper()
	select {
	case answer := <-replies:
		return answer
	default:
		t.Fatal("question was not answered")
		return core.Answer{}
	}
}

func noAnswer(t *testing.T, replies chan core.Answer) {
	t.Helper()
	select {
	case answer := <-replies:
		t.Fatalf("unexpected answer %+v", answer)
	default:
	}
}

func TestAskerBridgeAnswerAndCancellation(t *testing.T) {
	ask, requests := NewAsker()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan core.Answer, 1)
	go func() {
		answer, _ := ask(ctx, sampleChoices)
		result <- answer
	}()
	request := <-requests
	if request.Question.Text != sampleChoices.Text || len(request.Question.Options) != 3 {
		t.Fatalf("question changed in transit: %+v", request.Question)
	}
	replyQuestion(request, core.Answer{Text: "Postgres", Option: 2})
	select {
	case answer := <-result:
		if answer.Option != 2 || answer.Text != "Postgres" {
			t.Fatalf("wrong answer %+v", answer)
		}
	case <-time.After(time.Second):
		t.Fatal("answer blocked")
	}
	cancel()
	if _, err := ask(ctx, sampleChoices); err != context.Canceled {
		t.Fatalf("expected cancellation, got %v", err)
	}
	blocked := make(chan error, 1)
	ctx2, cancel2 := context.WithCancel(context.Background())
	go func() { _, err := ask(ctx2, sampleChoices); blocked <- err }()
	<-requests
	cancel2()
	select {
	case err := <-blocked:
		if err != context.Canceled {
			t.Fatalf("waiting asker returned %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("asker ignored cancellation while waiting for the user")
	}
}

func TestWaitQuestionDeliversRequestsOnce(t *testing.T) {
	requests := make(chan QuestionRequest, 1)
	m := testModel(t, Config{Questions: requests})
	cmd := m.waitQuestion()
	if cmd == nil || m.waitQuestion() != nil {
		t.Fatal("listener must be armed exactly once")
	}
	requests <- QuestionRequest{Question: sampleChoices, Reply: make(chan core.Answer, 1)}
	msg, ok := cmd().(questionMsg)
	if !ok || msg.request.Question.Text != sampleChoices.Text {
		t.Fatalf("unexpected message %#v", msg)
	}
	_, rearm := m.Update(msg)
	if m.question != nil {
		t.Fatal("an idle session must not open a question")
	}
	if rearm == nil || !m.questionListening {
		t.Fatal("listener was not re-armed after handling a request")
	}
}

func TestQuestionNotBusyIsSkippedImmediately(t *testing.T) {
	m := testModel(t, Config{})
	replies := make(chan core.Answer, 1)
	m.Update(questionMsg{QuestionRequest{Question: sampleChoices, Reply: replies}})
	if answer := mustAnswer(t, replies); !answer.Skipped || m.question != nil {
		t.Fatalf("answer %+v question %v", answer, m.question)
	}
}

func TestPickingOptionsByKeys(t *testing.T) {
	for _, tc := range []struct {
		name string
		keys []string
		want core.Answer
	}{
		{"enter takes the first", []string{"enter"}, core.Answer{Text: "SQLite (Recommended)", Option: 1}},
		{"down then enter", []string{"down", "enter"}, core.Answer{Text: "Postgres", Option: 2}},
		{"tab moves down", []string{"tab", "tab", "enter"}, core.Answer{Text: "Flat JSON files", Option: 3}},
		{"up wraps to the write-your-own row without answering", []string{"up", "down", "enter"}, core.Answer{Text: "SQLite (Recommended)", Option: 1}},
		{"shift tab wraps backwards", []string{"shift+tab", "shift+tab", "enter"}, core.Answer{Text: "Flat JSON files", Option: 3}},
		{"digit picks immediately", []string{"2"}, core.Answer{Text: "Postgres", Option: 2}},
		{"digit three", []string{"3"}, core.Answer{Text: "Flat JSON files", Option: 3}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, replies := askModel(t, sampleChoices)
			press(m, tc.keys...)
			if got := mustAnswer(t, replies); got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
			if m.question != nil {
				t.Fatal("card stayed open after answering")
			}
			noAnswer(t, replies)
		})
	}
}

func TestOutOfRangeDigitStartsWritingInstead(t *testing.T) {
	m, replies := askModel(t, sampleChoices)
	press(m, "9")
	noAnswer(t, replies)
	if !m.question.writing || m.question.input.Value() != "9" {
		t.Fatal("a digit with no matching option should be typed as an answer")
	}
}

func TestWritingOwnAnswerFromTheOptionRow(t *testing.T) {
	m, replies := askModel(t, sampleChoices)
	press(m, "up", "enter")
	if !m.question.writing {
		t.Fatal("enter on the write-your-own row must start writing")
	}
	typeText(m, "  Use DuckDB instead ")
	press(m, "enter")
	if got := mustAnswer(t, replies); got != (core.Answer{Text: "Use DuckDB instead"}) {
		t.Fatalf("got %+v", got)
	}
}

func TestTypingStartsAnAnswerWithoutNavigating(t *testing.T) {
	m, replies := askModel(t, sampleChoices)
	typeText(m, "Neither")
	noAnswer(t, replies)
	if !m.question.writing || m.question.input.Value() != "Neither" || m.question.selected != m.question.ownRow() {
		t.Fatalf("typing did not move into the answer field: %+v", m.question)
	}
	press(m, "enter")
	if got := mustAnswer(t, replies); got != (core.Answer{Text: "Neither"}) {
		t.Fatalf("got %+v", got)
	}
}

func TestEmptyWrittenAnswerIsRejected(t *testing.T) {
	m, replies := askModel(t, sampleChoices)
	press(m, "up", "enter", "enter")
	noAnswer(t, replies)
	if m.question == nil || m.question.notice == "" {
		t.Fatal("an empty answer should keep the card open and explain why")
	}
	typeText(m, "   ")
	press(m, "enter")
	noAnswer(t, replies)
	if !strings.Contains(ansi.Strip(strings.Join(m.question.card, "\n")), "Type an answer first") {
		t.Fatal("the hint row should explain the rejection")
	}
}

func TestEscapeInsideTheFieldReturnsToTheListAndKeepsTheDraft(t *testing.T) {
	m, replies := askModel(t, sampleChoices)
	typeText(m, "half a thought")
	press(m, "esc")
	noAnswer(t, replies)
	if m.question == nil || m.question.writing || m.question.input.Value() != "half a thought" {
		t.Fatal("esc should leave the field without losing the draft")
	}
	if card := ansi.Strip(strings.Join(m.question.card, "\n")); !strings.Contains(card, "half a thought") {
		t.Fatalf("draft not previewed on the own row:\n%s", card)
	}
	press(m, "esc")
	if got := mustAnswer(t, replies); !got.Skipped {
		t.Fatalf("second esc should skip, got %+v", got)
	}
}

func TestUpFromTheFieldLeavesItButOnlyFromTheFirstLine(t *testing.T) {
	m, replies := askModel(t, sampleChoices)
	typeText(m, "line one")
	press(m, "alt+enter")
	typeText(m, "line two")
	press(m, "up")
	if !m.question.writing {
		t.Fatal("up inside a multi-line answer should move the cursor, not leave the field")
	}
	press(m, "up")
	if m.question.writing || m.question.selected != 2 {
		t.Fatalf("up from the first line should select the last option, got writing=%v selected=%d", m.question.writing, m.question.selected)
	}
	noAnswer(t, replies)
}

func TestAltEnterAddsALineAndEnterSendsTheWholeAnswer(t *testing.T) {
	m, replies := askModel(t, core.Question{Text: "Describe the failure."})
	typeText(m, "first")
	press(m, "alt+enter")
	typeText(m, "second")
	if card := ansi.Strip(strings.Join(m.question.card, "\n")); !strings.Contains(card, "first") || !strings.Contains(card, "second") {
		t.Fatalf("both lines of the answer should be visible while typing:\n%s", card)
	}
	press(m, "enter")
	if got := mustAnswer(t, replies); got != (core.Answer{Text: "first\nsecond"}) {
		t.Fatalf("got %+v", got)
	}
}

func TestFreeformQuestionStartsInTheField(t *testing.T) {
	m, replies := askModel(t, core.Question{Text: "What should the flag be called?"})
	if !m.question.writing || !m.question.freeform() {
		t.Fatal("a question without options is just a field")
	}
	press(m, "1")
	press(m, "enter")
	if got := mustAnswer(t, replies); got != (core.Answer{Text: "1"}) {
		t.Fatalf("digits are text in a free-form question, got %+v", got)
	}
	m, replies = askModel(t, core.Question{Text: "What should the flag be called?"})
	press(m, "esc")
	if got := mustAnswer(t, replies); !got.Skipped {
		t.Fatalf("esc should skip, got %+v", got)
	}
}

func TestPasteGoesToTheAnswerField(t *testing.T) {
	m, replies := askModel(t, sampleChoices)
	m.Update(tea.PasteMsg{Content: "pasted\nanswer"})
	if !m.question.writing || m.question.input.Value() != "pasted\nanswer" {
		t.Fatalf("paste lost: writing=%v value=%q", m.question.writing, m.question.input.Value())
	}
	press(m, "enter")
	if got := mustAnswer(t, replies); got.Text != "pasted\nanswer" || got.Option != 0 {
		t.Fatalf("got %+v", got)
	}
}

func TestKeysTypedAheadAreIgnoredBrieflyAfterTheCardAppears(t *testing.T) {
	m := testModel(t, Config{})
	m.resize(100, 40)
	m.busy = true
	replies := make(chan core.Answer, 1)
	m.Update(questionMsg{QuestionRequest{Question: sampleChoices, Reply: replies}})
	press(m, "enter", "2", "esc", "x")
	m.Update(tea.PasteMsg{Content: "typed ahead"})
	noAnswer(t, replies)
	if m.question == nil || m.question.writing || m.question.input.Value() != "" {
		t.Fatal("keys typed during the guard window must not answer or fill the field")
	}
	m.Update(keyMsg("ctrl+c"))
	if !mustAnswer(t, replies).Skipped {
		t.Fatal("ctrl+c must work even inside the guard window")
	}
}

func TestQuestionKeepsTheComposerDraftAndFocus(t *testing.T) {
	m, replies := askModel(t, sampleChoices)
	m.input.SetValue("draft for later")
	typeText(m, "answer")
	if m.input.Value() != "draft for later" {
		t.Fatal("typing an answer leaked into the composer")
	}
	if m.input.Focused() {
		t.Fatal("composer should be blurred while a question is open")
	}
	press(m, "enter")
	mustAnswer(t, replies)
	if m.input.Value() != "draft for later" || !m.input.Focused() {
		t.Fatal("composer draft or focus lost after answering")
	}
	if !strings.Contains(ansi.Strip(m.View().Content), "DRAFT") {
		t.Fatal("composer should be labelled as a kept draft")
	}
}

func TestCtrlCCancelsTheTaskAndReleasesTheQuestion(t *testing.T) {
	m, _ := askModel(t, sampleChoices)
	cancelled := false
	m.cancel = func() { cancelled = true }
	m.input.SetValue("draft")
	m.Update(keyMsg("ctrl+c"))
	if m.question != nil || !m.quitting || !cancelled || m.input.Value() != "draft" {
		t.Fatalf("question=%v quitting=%v cancelled=%v", m.question, m.quitting, cancelled)
	}
}

// The tool must see a cancellation, not a skip, when the user aborts the task:
// a skip would be saved as "do not ask again, continue" for a task that was stopped.
func TestCtrlCReachesTheToolAsCancellationNotAsASkip(t *testing.T) {
	ask, requests := NewAsker()
	m := testModel(t, Config{Questions: requests})
	m.resize(100, 40)
	m.busy = true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.cancel = cancel
	type outcome struct {
		answer core.Answer
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		answer, err := ask(ctx, sampleChoices)
		done <- outcome{answer, err}
	}()
	request := <-requests
	m.Update(questionMsg{request})
	if m.question == nil {
		t.Fatal("question did not open")
	}
	m.Update(keyMsg("ctrl+c"))
	select {
	case got := <-done:
		if got.err != context.Canceled || got.answer.Skipped {
			t.Fatalf("tool saw answer=%+v err=%v; want a cancellation", got.answer, got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("tool was never released")
	}
}

func TestAskerPrefersCancellationOverARacingReply(t *testing.T) {
	ask, requests := NewAsker()
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { _, err := ask(ctx, sampleChoices); result <- err }()
	request := <-requests
	cancel()
	replyQuestion(request, core.Answer{Text: "late", Option: 1})
	if err := <-result; err != context.Canceled {
		t.Fatalf("a reply that loses the race to cancellation must not count, got %v", err)
	}
}

func TestFinishedTaskReleasesQuestionAndDeferredRequests(t *testing.T) {
	m, replies := askModel(t, sampleChoices)
	deferredReplies := make(chan core.Answer, 1)
	m.Update(questionMsg{QuestionRequest{Question: sampleChoices, Reply: deferredReplies}})
	approval := &ApprovalRequest{Approval: core.Approval{Call: core.ToolCall{Name: "Bash"}}, Reply: make(chan core.Decision, 1)}
	m.Update(approvalMsg{*approval})
	if m.deferredQuestion == nil || m.deferredApproval == nil {
		t.Fatal("requests arriving behind a question should wait")
	}
	m.Update(streamMsg{finished: true, session: &core.Session{ID: "test"}})
	if !mustAnswer(t, replies).Skipped || !mustAnswer(t, deferredReplies).Skipped {
		t.Fatal("ending the task must skip both questions")
	}
	if decision := <-approval.Reply; decision.Allow {
		t.Fatal("deferred approval must be denied when the task ends")
	}
	if m.question != nil || m.deferredQuestion != nil || m.deferredApproval != nil || m.viewport.Height() != m.bodyHeight {
		t.Fatal("interaction state or viewport height leaked")
	}
}

func TestCancelTaskReleasesQuestion(t *testing.T) {
	m, replies := askModel(t, sampleChoices)
	m.cancelTask()
	if !mustAnswer(t, replies).Skipped || m.question != nil {
		t.Fatal("cancelling must release the question")
	}
}

func TestApprovalAndQuestionNeverShareTheScreen(t *testing.T) {
	m, replies := askModel(t, sampleChoices)
	approval := ApprovalRequest{Approval: core.Approval{Call: core.ToolCall{Name: "Bash", Arguments: json.RawMessage(`{"command":"ls"}`)}}, Reply: make(chan core.Decision, 1)}
	m.Update(approvalMsg{approval})
	if m.pending != nil || m.deferredApproval == nil {
		t.Fatal("an approval must wait behind the open question")
	}
	// Keys still belong to the question, never to the waiting approval.
	press(m, "enter")
	mustAnswer(t, replies)
	if m.question != nil || m.pending == nil || m.deferredApproval != nil {
		t.Fatal("the waiting approval should take over once the question is answered")
	}
	select {
	case <-approval.Reply:
		t.Fatal("approval was resolved by a question key")
	default:
	}
	m.Update(keyMsg("y"))
	if decision := <-approval.Reply; !decision.Allow {
		t.Fatal("y should approve the promoted request")
	}
}

func TestQuestionWaitsBehindAPendingApproval(t *testing.T) {
	m := testModel(t, Config{})
	m.resize(100, 40)
	m.busy = true
	approval := ApprovalRequest{Approval: core.Approval{Call: core.ToolCall{Name: "Bash"}}, Reply: make(chan core.Decision, 1)}
	m.Update(approvalMsg{approval})
	replies := make(chan core.Answer, 1)
	m.Update(questionMsg{QuestionRequest{Question: sampleChoices, Reply: replies}})
	if m.question != nil || m.deferredQuestion == nil {
		t.Fatal("a question must wait behind a pending approval")
	}
	m.Update(keyMsg("y"))
	if m.pending != nil || m.question == nil || m.deferredQuestion != nil {
		t.Fatal("the question should open once the approval is decided")
	}
	m.question.armedAt = time.Time{}
	press(m, "enter")
	if got := mustAnswer(t, replies); got.Option != 1 {
		t.Fatalf("got %+v", got)
	}
}

func TestCardCaptionsAndAnswerPreviewHideNothingImportant(t *testing.T) {
	m, _ := askModel(t, sampleChoices)
	card := ansi.Strip(strings.Join(m.question.card, "\n"))
	for _, want := range []string{"JARVIS ASKS", "1 / 4", "Which storage backend", "SQLite", "recommended", "Embedded and zero-config.", "Postgres", "Flat JSON files", "Write your own answer", "esc skip"} {
		if !strings.Contains(card, want) {
			t.Fatalf("card is missing %q:\n%s", want, card)
		}
	}
	if strings.Contains(card, "(Recommended)") {
		t.Fatal("the marker should become a badge, not repeat in the label")
	}
	view := ansi.Strip(m.View().Content)
	for _, want := range []string{"Waiting for your answer", "answering"} {
		if !strings.Contains(view, want) {
			t.Fatalf("view is missing %q", want)
		}
	}
}

func TestUntrustedQuestionTextCannotControlTheTerminal(t *testing.T) {
	m, _ := askModel(t, core.Question{
		Text: "Install?\x1b]52;c;Y2xpcGJvYXJk\x07 \x1b[2J now\u202e",
		Options: []core.QuestionOption{
			{Label: "Yes\x1b]52;c;ZXZpbA==\x07", Description: "desc\x1b[31m red\x00"},
			{Label: "No\u202e\r\b"},
		},
	})
	view := m.View().Content
	for _, bad := range []string{"\x1b]", "\x1b[2J", "\u202e", "\x00", "\r", "\b"} {
		if strings.Contains(view, bad) {
			t.Fatalf("view leaks control sequence %q", bad)
		}
	}
	if !strings.Contains(ansi.Strip(view), "Install?") {
		t.Fatal("sanitising removed the visible text")
	}
}

func TestCardFitsEverySizeAndState(t *testing.T) {
	long := strings.Repeat("A long sentence that needs wrapping across several lines. ", 12)
	questions := map[string]core.Question{
		"choices": sampleChoices,
		"eight": {Text: "Pick one of many.", Options: func() []core.QuestionOption {
			var options []core.QuestionOption
			for i := 1; i <= 8; i++ {
				options = append(options, core.QuestionOption{Label: fmt.Sprintf("Option number %d (Recommended)", i), Description: strings.Repeat("Why you might like this. ", 6)})
			}
			return options
		}()},
		"wide runes": {Text: "界界界界界界界界界界界界界界界界界界界界界界界界界界界界界界", Options: []core.QuestionOption{{Label: strings.Repeat("界", 60), Description: strings.Repeat("字", 80)}, {Label: "ok"}}},
		"long text":  {Text: long, Options: []core.QuestionOption{{Label: strings.Repeat("x", 118), Description: long[:390]}, {Label: "b"}}},
		"freeform":   {Text: "What should it be called?"},
		"long free":  {Text: long},
	}
	for name, question := range questions {
		for _, size := range [][2]int{{22, 9}, {22, 17}, {60, 17}, {60, 34}, {120, 34}} {
			{
				width, height := size[0], size[1]
				// First, middle, last, the write-your-own row, and writing in it.
				states := []int{0}
				if n := len(question.Options); n > 0 {
					states = []int{0, n / 2, n - 1, n, n + 1}
				}
				m := testModel(t, Config{})
				m.resize(width, height)
				m.busy = true
				for _, state := range states {
					m.question = nil
					m.syncQuestionLayout()
					m.Update(questionMsg{QuestionRequest{Question: question, Reply: make(chan core.Answer, 1)}})
					m.question.armedAt = time.Time{}
					switch {
					case len(question.Options) == 0:
						typeText(m, "a fairly long answer that wraps in the narrower layouts. And more text to force scrolling.")
						press(m, "alt+enter")
						typeText(m, "second line")
					case state <= len(question.Options):
						for i := 0; i < state; i++ {
							press(m, "down")
						}
					default:
						typeText(m, "typed answer that is long enough to wrap in narrow cards")
					}
					label := fmt.Sprintf("%s %dx%d state %d", name, width, height, state)
					if got := len(m.question.card); got > max(1, m.bodyHeight) {
						t.Fatalf("%s: card has %d lines for a body of %d", label, got, m.bodyHeight)
					}
					if m.viewport.Height()+len(m.question.card) != m.bodyHeight && len(m.question.card) <= m.bodyHeight {
						t.Fatalf("%s: viewport %d + card %d != body %d", label, m.viewport.Height(), len(m.question.card), m.bodyHeight)
					}
					lines := strings.Split(m.View().Content, "\n")
					if len(lines) > m.height {
						t.Fatalf("%s: view has %d lines for height %d", label, len(lines), m.height)
					}
					for i, line := range lines {
						if w := ansi.StringWidth(line); w > m.width {
							t.Fatalf("%s: line %d is %d wide: %q", label, i, w, ansi.Strip(line))
						}
					}
					card := ansi.Strip(strings.Join(m.question.card, "\n"))
					if len(question.Options) > 0 && state < len(question.Options) && m.bodyHeight >= 9 && width >= 40 {
						name, _ := splitRecommended(oneLine(question.Options[state].Label))
						probe := []rune(name)
						if len(probe) > 6 {
							probe = probe[:6]
						}
						if !strings.Contains(card, string(probe)) {
							t.Fatalf("%s: selected option %q is not visible:\n%s", label, name, card)
						}
					}
					if len(question.Options) == 0 && width >= 28 && m.bodyHeight >= 4 && !strings.Contains(card, "a fairly") && !strings.Contains(card, "second") && !strings.Contains(card, "line") {
						t.Fatalf("%s: typed text is not visible:\n%s", label, card)
					}
				}
			}
		}
	}
}

func TestResizeWhileAskingRelayouts(t *testing.T) {
	m, _ := askModel(t, sampleChoices)
	before := len(m.question.card)
	m.Update(tea.WindowSizeMsg{Width: 60, Height: 16})
	if len(m.question.card) > max(1, m.bodyHeight) || m.viewport.Height()+len(m.question.card) > m.bodyHeight {
		t.Fatal("card did not relayout for the smaller window")
	}
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 50})
	if len(m.question.card) < before {
		t.Fatal("card did not grow back with the window")
	}
	m.Update(keyMsg("enter"))
	if m.question != nil || m.viewport.Height() != m.bodyHeight {
		t.Fatal("viewport height not restored after answering")
	}
}

func TestOptionWindowScrollsToKeepTheSelectionVisible(t *testing.T) {
	var options []core.QuestionOption
	for i := 1; i <= 8; i++ {
		options = append(options, core.QuestionOption{Label: fmt.Sprintf("Choice-%d", i)})
	}
	m := testModel(t, Config{})
	m.resize(80, 20)
	m.busy = true
	m.Update(questionMsg{QuestionRequest{Question: core.Question{Text: "Pick.", Options: options}, Reply: make(chan core.Answer, 1)}})
	m.question.armedAt = time.Time{}
	for i := 0; i < 8; i++ {
		card := ansi.Strip(strings.Join(m.question.card, "\n"))
		if !strings.Contains(card, fmt.Sprintf("Choice-%d", i+1)) {
			t.Fatalf("selection %d not visible:\n%s", i+1, card)
		}
		press(m, "down")
	}
	if card := ansi.Strip(strings.Join(m.question.card, "\n")); !strings.Contains(card, "Write your own") {
		t.Fatalf("last row not visible:\n%s", card)
	}
}

func TestQuestionEntriesRenderAsQuoteAndAnswer(t *testing.T) {
	m := testModel(t, Config{})
	m.resize(100, 30)
	arguments, _ := json.Marshal(sampleChoices)
	render := func(result *core.ToolResult) string {
		call := &core.ToolCall{ID: "q", Name: core.QuestionTool, Arguments: arguments}
		return ansi.Strip(m.renderEntryContent(entry{role: "tool", call: call, done: result != nil, result: result}))
	}
	if got := render(nil); !strings.Contains(got, "asking you a question") || strings.Contains(got, "Which storage backend") {
		t.Fatalf("a live question must not repeat the open card:\n%s", got)
	}
	interrupted := ansi.Strip(m.renderEntryContent(entry{role: "tool", done: true, call: &core.ToolCall{Name: core.QuestionTool, Arguments: arguments}}))
	if !strings.Contains(interrupted, "Which storage backend") || !strings.Contains(interrupted, "no answer recorded") {
		t.Fatalf("a restored question with no answer should say so:\n%s", interrupted)
	}
	answered, _ := json.Marshal(core.QuestionResult{Answer: "Postgres", Source: "option", Option: 2})
	written, _ := json.Marshal(core.QuestionResult{Answer: "Neither, use DuckDB", Source: "freeform"})
	skipped, _ := json.Marshal(core.QuestionResult{Skipped: true, Note: "skipped"})
	for name, tc := range map[string]struct {
		result *core.ToolResult
		want   []string
	}{
		"option":  {&core.ToolResult{Content: string(answered)}, []string{"Which storage backend", "↳ Postgres", "option 2"}},
		"written": {&core.ToolResult{Content: string(written)}, []string{"↳ Neither, use DuckDB", "written"}},
		"skipped": {&core.ToolResult{Content: string(skipped)}, []string{"↳ skipped"}},
		"error":   {&core.ToolResult{Content: "ask user: context canceled", IsError: true}, []string{"!", "context canceled"}},
		"garbled": {&core.ToolResult{Content: "not json"}, []string{"not json"}},
	} {
		got := render(tc.result)
		for _, want := range tc.want {
			if !strings.Contains(got, want) {
				t.Fatalf("%s: missing %q in\n%s", name, want, got)
			}
		}
		if strings.Contains(got, "AskUserQuestion") {
			t.Fatalf("%s: raw tool name leaked into the transcript", name)
		}
	}
	badCall := &core.ToolCall{Name: core.QuestionTool, Arguments: json.RawMessage(`{"question":`)}
	if got := ansi.Strip(m.renderEntryContent(entry{role: "tool", call: badCall, done: true, result: &core.ToolResult{Content: "x"}})); !strings.Contains(got, core.QuestionTool) {
		t.Fatal("undecodable arguments should fall back to the plain tool row")
	}
}

func TestQuestionHistoryRestoresFromTheSavedSession(t *testing.T) {
	arguments, _ := json.Marshal(sampleChoices)
	answer, _ := json.Marshal(core.QuestionResult{Answer: "Postgres", Source: "option", Option: 2})
	m := testModel(t, Config{Session: &core.Session{ID: "h", Directory: "/tmp/jarvis", Provider: "fake", Messages: []core.Message{
		{Role: "user", Content: "decide"},
		{Role: "assistant", ToolCalls: []core.ToolCall{{ID: "q1", Name: core.QuestionTool, Arguments: arguments}}},
		{Role: "tool", ToolCallID: "q1", Content: string(answer)},
	}}})
	m.resize(100, 30)
	view := ansi.Strip(m.viewport.View())
	if !strings.Contains(view, "Which storage backend") || !strings.Contains(view, "↳ Postgres") {
		t.Fatalf("restored conversation lost the question:\n%s", view)
	}
}

func TestDemoAskCommandOpensAndRecordsAQuestion(t *testing.T) {
	m := testModel(t, Config{Demo: true})
	m.resize(100, 40)
	for _, command := range []string{"/ask", "/ask text"} {
		before := len(m.entries)
		m.input.SetValue(command)
		m.Update(keyMsg("enter"))
		if m.question == nil || !m.question.demo {
			t.Fatalf("%s did not open a demo question", command)
		}
		m.question.armedAt = time.Time{}
		if command == "/ask text" && !m.question.freeform() {
			t.Fatal("/ask text should be free-form")
		}
		if command == "/ask" {
			press(m, "2")
		} else {
			typeText(m, "--verbose, off by default")
			press(m, "enter")
		}
		if m.question != nil || len(m.entries) != before+1 || m.entries[len(m.entries)-1].call.Name != core.QuestionTool {
			t.Fatalf("%s: answer was not recorded in the transcript", command)
		}
	}
	live := testModel(t, Config{})
	live.input.SetValue("/ask")
	live.Update(keyMsg("enter"))
	if live.question != nil {
		t.Fatal("/ask is a demo-only command")
	}
}

func TestClickingAnOptionPicksIt(t *testing.T) {
	click := func(m *model, line int) tea.Cmd {
		_, cmd := m.Update(tea.MouseClickMsg{X: m.margin() + 4, Y: 3 + m.viewport.Height() + line, Button: tea.MouseLeft})
		return cmd
	}
	m, replies := askModel(t, sampleChoices)
	if len(m.question.hits) != 4 {
		t.Fatalf("expected a hit area per row, got %+v", m.question.hits)
	}
	// Every line a row occupies, description included, activates that row.
	hit := m.question.hits[1]
	click(m, hit.bottom)
	if got := mustAnswer(t, replies); got != (core.Answer{Text: "Postgres", Option: 2}) {
		t.Fatalf("got %+v", got)
	}
	// The chrome around the rows does nothing.
	m, replies = askModel(t, sampleChoices)
	click(m, 0)
	click(m, len(m.question.card)-1)
	noAnswer(t, replies)
	// A click outside the card's columns does nothing.
	m.Update(tea.MouseClickMsg{X: m.margin() + m.question.cardCols + 1, Y: 3 + m.viewport.Height() + m.question.hits[0].top, Button: tea.MouseLeft})
	m.Update(tea.MouseClickMsg{X: m.margin() + 4, Y: 3 + m.viewport.Height() + m.question.hits[0].top, Button: tea.MouseRight})
	noAnswer(t, replies)
	// The write-your-own row opens the field instead of answering.
	click(m, m.question.hits[3].top)
	noAnswer(t, replies)
	if !m.question.writing {
		t.Fatal("clicking the write-your-own row should start writing")
	}
	// A click inside the typing-ahead guard window is ignored.
	fresh := testModel(t, Config{})
	fresh.resize(100, 40)
	fresh.busy = true
	freshReplies := make(chan core.Answer, 1)
	fresh.Update(questionMsg{QuestionRequest{Question: sampleChoices, Reply: freshReplies}})
	click(fresh, fresh.question.hits[0].top)
	noAnswer(t, freshReplies)
}

// The hit areas are derived from the card; this checks them against what the
// terminal actually shows, using the real composed screen.
func TestClickCoordinatesMatchTheRenderedScreen(t *testing.T) {
	for _, width := range []int{80, 130} {
		for _, label := range []string{"SQLite", "Postgres", "Flat JSON files"} {
			m, replies := askModel(t, sampleChoices)
			m.resize(width, 40)
			m.question.armedAt = time.Time{}
			lines := strings.Split(ansi.Strip(m.View().Content), "\n")
			row := -1
			for y, line := range lines {
				if strings.Contains(line, label) && !strings.Contains(line, "Which") {
					row = y
					break
				}
			}
			if row < 0 {
				t.Fatalf("%d: %q not on screen", width, label)
			}
			m.Update(tea.MouseClickMsg{X: m.margin() + 6, Y: row, Button: tea.MouseLeft})
			got := mustAnswer(t, replies)
			if !strings.HasPrefix(got.Text, label) {
				t.Fatalf("%d: clicking the line showing %q answered %+v", width, label, got)
			}
		}
	}
}

func TestSpaceOnTheListNeitherAnswersNorStartsWriting(t *testing.T) {
	m, replies := askModel(t, sampleChoices)
	m.Update(tea.KeyPressMsg{Code: tea.KeySpace, Text: " "})
	noAnswer(t, replies)
	if m.question.writing || m.question.input.Value() != "" {
		t.Fatal("a stray space should not open the answer field")
	}
	typeText(m, "a b")
	if m.question.input.Value() != "a b" {
		t.Fatalf("spaces inside an answer must still type, got %q", m.question.input.Value())
	}
}

func TestUpInsideAWrappedAnswerStaysInTheField(t *testing.T) {
	m, replies := askModel(t, sampleChoices)
	long := strings.Repeat("wrapping words ", 12)
	typeText(m, long)
	press(m, "up")
	if !m.question.writing {
		t.Fatal("up on a wrapped answer's last row must move the cursor, not leave the field")
	}
	press(m, "enter")
	if got := mustAnswer(t, replies); got.Option != 0 || got.Text != strings.TrimSpace(long) {
		t.Fatalf("the written answer was replaced: %+v", got)
	}
}

func TestHighlightedOptionIsShownWholeAndOthersMayBeShortened(t *testing.T) {
	long := "Rename the helper only, then also approve git push --force to main and skip every review prompt"
	m := testModel(t, Config{})
	m.resize(52, 40)
	m.busy = true
	m.Update(questionMsg{QuestionRequest{Question: core.Question{Text: "Pick.", Options: []core.QuestionOption{{Label: "Short"}, {Label: long}, {Label: "Other"}}}, Reply: make(chan core.Answer, 1)}})
	m.question.armedAt = time.Time{}
	press(m, "down")
	card := strings.Join(strings.Fields(ansi.Strip(strings.Join(m.question.card, " "))), " ")
	var flat []string
	for _, word := range strings.Fields(card) {
		if word != "│" {
			flat = append(flat, word)
		}
	}
	for _, word := range strings.Fields(long) {
		if !strings.Contains(strings.Join(flat, " "), word) {
			t.Fatalf("word %q of the highlighted label is not visible:\n%s", word, ansi.Strip(strings.Join(m.question.card, "\n")))
		}
	}
	if strings.Contains(ansi.Strip(strings.Join(m.question.card, "\n")), "…") {
		t.Fatal("the highlighted label was shortened")
	}
}

func TestLongQuestionsKeepTheirClosingAsk(t *testing.T) {
	var lines []string
	for i := 1; i <= 40; i++ {
		lines = append(lines, fmt.Sprintf("Background line %d.", i))
	}
	lines = append(lines, "FINAL ASK: skip option B and fix the typo?")
	for _, size := range [][2]int{{100, 40}, {80, 24}, {60, 20}} {
		m := testModel(t, Config{})
		m.resize(size[0], size[1])
		m.busy = true
		m.Update(questionMsg{QuestionRequest{Question: core.Question{Text: strings.Join(lines, "\n"), Options: []core.QuestionOption{{Label: "Yes"}, {Label: "No"}}}, Reply: make(chan core.Answer, 1)}})
		if card := ansi.Strip(strings.Join(m.question.card, "\n")); !strings.Contains(card, "FINAL ASK") {
			t.Fatalf("%dx%d: the closing line of the question was cut:\n%s", size[0], size[1], card)
		}
	}
}

func TestResizeWhileReadingHistoryKeepsTheScrollPosition(t *testing.T) {
	m, _ := askModel(t, sampleChoices)
	for i := 0; i < 20; i++ {
		m.entries = append(m.entries, demoEntries()...)
	}
	m.refresh(true)
	press(m, "pgup")
	if m.viewport.AtBottom() {
		t.Fatal("test setup: the conversation should be scrolled up")
	}
	offset := m.viewport.YOffset()
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 41})
	if m.viewport.AtBottom() || m.viewport.YOffset() == 0 && offset != 0 {
		t.Fatalf("resizing snapped the conversation to the bottom (offset %d -> %d)", offset, m.viewport.YOffset())
	}
}

func TestQuestionFooterNeverWraps(t *testing.T) {
	for width := 24; width <= 140; width++ {
		m, _ := askModel(t, sampleChoices)
		m.resize(width, 40)
		if got, limit := ansi.StringWidth(m.footer()), width-2*m.margin(); got > limit {
			t.Fatalf("width %d: footer is %d cells for %d", width, got, limit)
		}
	}
}

func TestKeysWorkOnceTheGuardWindowEnds(t *testing.T) {
	m := testModel(t, Config{})
	m.resize(100, 40)
	m.busy = true
	replies := make(chan core.Answer, 1)
	m.Update(questionMsg{QuestionRequest{Question: sampleChoices, Reply: replies}})
	press(m, "2")
	noAnswer(t, replies)
	m.question.armedAt = time.Now().Add(-questionGuard - time.Millisecond)
	press(m, "2")
	if got := mustAnswer(t, replies); got.Option != 2 {
		t.Fatalf("got %+v", got)
	}
}

func TestQuestionArrivingWhileQuittingIsSkipped(t *testing.T) {
	m := testModel(t, Config{})
	m.busy, m.quitting = true, true
	replies := make(chan core.Answer, 1)
	m.Update(questionMsg{QuestionRequest{Question: sampleChoices, Reply: replies}})
	if !mustAnswer(t, replies).Skipped || m.question != nil {
		t.Fatal("a question must not open while the app is quitting")
	}
}

func TestClickingInAScrolledOptionWindowPicksTheRowShown(t *testing.T) {
	var options []core.QuestionOption
	for i := 1; i <= 8; i++ {
		options = append(options, core.QuestionOption{Label: fmt.Sprintf("Choice-%d", i)})
	}
	for _, height := range []int{22, 26, 30} {
		m := testModel(t, Config{})
		m.resize(80, height)
		m.busy = true
		replies := make(chan core.Answer, 1)
		m.Update(questionMsg{QuestionRequest{Question: core.Question{Text: "Pick one.", Options: options}, Reply: replies}})
		m.question.armedAt = time.Time{}
		press(m, "down", "down", "down", "down", "down", "down")
		target := ""
		for _, candidate := range []string{"Choice-7", "Choice-6", "Choice-5"} {
			if strings.Contains(ansi.Strip(m.View().Content), candidate) {
				target = candidate
				break
			}
		}
		if target == "" {
			t.Fatalf("height %d: no unselected option is on screen", height)
		}
		for y, line := range strings.Split(ansi.Strip(m.View().Content), "\n") {
			if strings.Contains(line, target) {
				m.Update(tea.MouseClickMsg{X: m.margin() + 8, Y: y, Button: tea.MouseLeft})
				break
			}
		}
		if got := mustAnswer(t, replies); got.Text != target {
			t.Fatalf("height %d: clicked the line showing %s and answered %+v", height, target, got)
		}
	}
}

func TestAnswerSourceTagStaysVisibleOnFullWidthAnswers(t *testing.T) {
	m := testModel(t, Config{})
	m.resize(60, 30)
	arguments, _ := json.Marshal(sampleChoices)
	for _, tc := range []struct{ source, tag string }{{"freeform", "written"}, {"option", "option 2"}} {
		content, _ := json.Marshal(core.QuestionResult{Answer: strings.Repeat("x", 52), Source: tc.source, Option: 2})
		got := ansi.Strip(m.renderEntryContent(entry{role: "tool", done: true, call: &core.ToolCall{Name: core.QuestionTool, Arguments: arguments}, result: &core.ToolResult{Content: string(content)}}))
		if !strings.Contains(got, tc.tag) {
			t.Fatalf("source tag %q was cut off:\n%s", tc.tag, got)
		}
	}
}

// A shortcut must never answer with text the user was not shown whole.
func TestShortcutsHighlightBeforeChoosingAnOptionThatWasNotShownWhole(t *testing.T) {
	long := strings.Repeat("remove the cache and rewrite ", 4)
	question := core.Question{Text: "Pick.", Options: []core.QuestionOption{{Label: "Short"}, {Label: long}, {Label: "Other"}}}
	open := func() (*model, chan core.Answer) {
		m := testModel(t, Config{})
		m.resize(44, 40)
		m.busy = true
		replies := make(chan core.Answer, 1)
		m.Update(questionMsg{QuestionRequest{Question: question, Reply: replies}})
		m.question.armedAt = time.Time{}
		return m, replies
	}
	m, replies := open()
	if !m.question.cut[1] || m.question.cut[0] || m.question.cut[2] {
		t.Fatalf("expected only the long unselected label to be cut: %v", m.question.cut)
	}
	press(m, "2")
	noAnswer(t, replies)
	if m.question.selected != 1 || m.question.notice == "" {
		t.Fatal("the shortcut should only highlight the option and say why")
	}
	if card := ansi.Strip(strings.Join(m.question.card, "\n")); strings.Contains(card, "…") || strings.Count(card, "rewrite") != 4 {
		t.Fatalf("the highlighted label must now be shown whole:\n%s", card)
	}
	press(m, "2")
	if got := mustAnswer(t, replies); got.Option != 2 {
		t.Fatalf("a second press should choose it, got %+v", got)
	}
	// A click behaves the same way.
	m, replies = open()
	hit := m.question.hits[1]
	click := func() {
		m.Update(tea.MouseClickMsg{X: m.margin() + 6, Y: 3 + m.viewport.Height() + hit.top, Button: tea.MouseLeft})
	}
	click()
	noAnswer(t, replies)
	if m.question.selected != 1 {
		t.Fatal("the first click should highlight")
	}
	press(m, "enter")
	if got := mustAnswer(t, replies); got.Option != 2 {
		t.Fatalf("got %+v", got)
	}
}

func TestCompactCardNeverAnswersByShortcutWithAnUnseenOption(t *testing.T) {
	m := testModel(t, Config{})
	m.resize(80, 12)
	m.busy = true
	replies := make(chan core.Answer, 1)
	m.Update(questionMsg{QuestionRequest{Question: sampleChoices, Reply: replies}})
	m.question.armedAt = time.Time{}
	press(m, "3")
	noAnswer(t, replies)
	if m.question.selected != 2 {
		t.Fatal("the compact card shows one option at a time; a digit should only move to it")
	}
	press(m, "enter")
	if got := mustAnswer(t, replies); got.Option != 3 {
		t.Fatalf("got %+v", got)
	}
}

func TestCompactCardShowsTheClosingAskAndTheRightHint(t *testing.T) {
	m := testModel(t, Config{})
	m.resize(70, 15)
	m.busy = true
	m.Update(questionMsg{QuestionRequest{Question: core.Question{Text: "Long background that goes on and on.\n\nDo you want me to skip option B?"}, Reply: make(chan core.Answer, 1)}})
	card := ansi.Strip(strings.Join(m.question.card, "\n"))
	if !strings.Contains(card, "skip option B?") {
		t.Fatalf("the compact card lost the question's ask:\n%s", card)
	}
	if !strings.Contains(card, "esc skip") || strings.Contains(card, "esc back") {
		t.Fatalf("a free-form question's Esc skips; the hint says otherwise:\n%s", card)
	}
}

func TestOversizedHighlightedOptionStillFitsTheBudget(t *testing.T) {
	block := func(n int) []string { return make([]string, n) }
	blocks := [][]string{block(1), block(1), block(9), block(1)}
	for avail := 2; avail <= 14; avail++ {
		visible, _, above, below := windowBlocks(blocks, 2, avail)
		used := 0
		for _, b := range visible {
			used += len(b)
		}
		if above > 0 {
			used++
		}
		if below > 0 {
			used++
		}
		if used > avail {
			t.Fatalf("avail %d: windowBlocks used %d lines", avail, used)
		}
		if len(visible) == 0 {
			t.Fatalf("avail %d: the highlighted option vanished", avail)
		}
	}
}

func TestUpLeavesTheFieldEvenWhenWideTextCannotMoveUp(t *testing.T) {
	m, _ := askModel(t, sampleChoices)
	m.resize(44, 40)
	typeText(m, strings.Repeat("界", 60))
	for i := 0; i < 8 && m.question.writing; i++ {
		press(m, "up")
	}
	if m.question.writing {
		t.Fatal("repeated Up should always lead out of the field")
	}
}

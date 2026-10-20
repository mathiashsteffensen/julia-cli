package ui

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"julia/internal/core"
)

// reviewModel opens a review the way the stream does and disarms the typing guard.
func reviewModel(t *testing.T, width, height int) (*model, chan core.Answer) {
	t.Helper()
	m := testModel(t, Config{})
	m.resize(width, height)
	m.busy = true
	replies := make(chan core.Answer, 2)
	review := demoReview()
	m.Update(questionMsg{QuestionRequest{Review: &review, Reply: replies}})
	if m.question == nil || m.question.review == nil {
		t.Fatal("review was not opened")
	}
	m.question.armedAt = time.Time{}
	return m, replies
}

func decisions(triage []core.Triage) []string {
	var out []string
	for _, t := range triage {
		out = append(out, t.Decision)
	}
	return out
}

func TestReviewWalksEveryFindingAndSends(t *testing.T) {
	m, replies := reviewModel(t, 142, 44)
	press(m, "f", "d", "r")
	if !m.question.writing {
		t.Fatal("r did not open the reply field")
	}
	typeText(m, "use the config value")
	press(m, "enter", "i")
	r := m.question.review
	if r.selected != 4 {
		t.Fatalf("after four decisions the open finding is %d, want 4", r.selected)
	}
	press(m, "enter") // Fix is lit on a fresh finding
	if r.selected != r.sendRow() {
		t.Fatalf("with every finding decided the selection is %d, want the send row", r.selected)
	}
	noAnswer(t, replies)
	press(m, "enter")
	answer := mustAnswer(t, replies)
	want := []string{core.TriageFix, core.TriageFixed, core.TriageReply, core.TriageIgnore, core.TriageFix}
	if got := decisions(answer.Triage); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("triage = %v, want %v", got, want)
	}
	if answer.Triage[2].Reply != "use the config value" || answer.Skipped {
		t.Fatalf("reply lost: %+v", answer)
	}
	if m.question != nil {
		t.Fatal("card still open after sending")
	}
}

func TestReviewArrowsAndEnterChoose(t *testing.T) {
	m, replies := reviewModel(t, 142, 44)
	press(m, "down")
	m.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	m.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	press(m, "enter")
	r := m.question.review
	if r.triage[1].Decision != core.TriageFixed {
		t.Fatalf("→→ enter on finding 2 gave %q", r.triage[1].Decision)
	}
	// Advancing wraps to the first open finding, not past the end.
	if r.selected != 2 {
		t.Fatalf("selection after deciding finding 2 is %d", r.selected)
	}
	press(m, "up")
	if r.choice != choiceIndex(core.TriageFixed) {
		t.Fatalf("returning to a decided finding lights chip %d, want its decision", r.choice)
	}
	m.Update(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	answer := mustAnswer(t, replies)
	if got := decisions(answer.Triage); got[1] != core.TriageFixed || got[0] != "" || got[4] != "" {
		t.Fatalf("ctrl+s sent %v", got)
	}
}

func TestReviewEscProtectsDecisions(t *testing.T) {
	m, replies := reviewModel(t, 142, 44)
	press(m, "f", "esc")
	noAnswer(t, replies)
	if !strings.Contains(m.question.notice, "Esc again") {
		t.Fatalf("first esc notice = %q", m.question.notice)
	}
	// Any other key stands the warning down.
	press(m, "down", "esc")
	noAnswer(t, replies)
	press(m, "esc")
	if answer := mustAnswer(t, replies); !answer.Skipped {
		t.Fatalf("second esc sent %+v", answer)
	}

	m, replies = reviewModel(t, 142, 44)
	press(m, "esc")
	if answer := mustAnswer(t, replies); !answer.Skipped {
		t.Fatalf("esc with nothing decided sent %+v", answer)
	}
}

func TestReviewFixAllOpenKeepsDecided(t *testing.T) {
	m, _ := reviewModel(t, 142, 44)
	press(m, "i", "d", "F")
	r := m.question.review
	want := []string{core.TriageIgnore, core.TriageFixed, core.TriageFix, core.TriageFix, core.TriageFix}
	if got := decisions(r.triage); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("triage after F = %v", got)
	}
	if r.selected != r.sendRow() {
		t.Fatal("F should leave the selection on send")
	}
}

func TestReviewClearAndReplyEditing(t *testing.T) {
	m, _ := reviewModel(t, 142, 44)
	r := m.question.review
	press(m, "f", "up")
	m.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
	if decided(r.triage[0]) {
		t.Fatal("backspace did not clear the decision")
	}
	press(m, "r", "enter")
	if !m.question.writing || decided(r.triage[0]) || m.question.notice == "" {
		t.Fatal("an empty reply must not settle the finding")
	}
	typeText(m, "first")
	press(m, "esc")
	if m.question.writing || decided(r.triage[0]) {
		t.Fatal("esc in the field must leave the finding undecided")
	}
	press(m, "r")
	typeText(m, "second")
	press(m, "enter", "up", "r")
	if got := m.question.input.Value(); got != "second" {
		t.Fatalf("reopening a reply shows %q, want the saved reply", got)
	}
}

func TestReviewPasteStartsAReply(t *testing.T) {
	m, _ := reviewModel(t, 142, 44)
	m.Update(tea.PasteMsg{Content: "pasted direction"})
	if !m.question.writing || m.question.input.Value() != "pasted direction" {
		t.Fatalf("paste gave writing=%v value=%q", m.question.writing, m.question.input.Value())
	}
}

func TestReviewClickChipsAndRows(t *testing.T) {
	m, replies := reviewModel(t, 142, 44)
	q := m.question
	click := func(line, x int) {
		m.Update(tea.MouseClickMsg{X: q.cardLeft + x, Y: 3 + m.viewport.Height() + line, Button: tea.MouseLeft})
	}
	chip := func(index int) reviewHit {
		for _, hit := range q.review.hits {
			if hit.chip == index {
				return hit
			}
		}
		t.Fatalf("no hit for chip %d in %+v", index, q.review.hits)
		return reviewHit{}
	}
	ignore := chip(choiceIndex(core.TriageIgnore))
	click(ignore.top, ignore.x0)
	if q.review.triage[0].Decision != core.TriageIgnore {
		t.Fatalf("clicking Ignore gave %+v", q.review.triage[0])
	}
	// A click on a collapsed row opens that finding without deciding it.
	for _, hit := range q.review.hits {
		if hit.row == 3 && hit.chip < 0 {
			click(hit.top, 6)
		}
	}
	if q.review.selected != 3 || decided(q.review.triage[3]) {
		t.Fatalf("row click: selected %d, triage %+v", q.review.selected, q.review.triage[3])
	}
	// The send row needs a first click to select it and a second to send.
	var send reviewHit
	for _, hit := range q.review.hits {
		if hit.row == q.review.sendRow() {
			send = hit
		}
	}
	click(send.top, 6)
	noAnswer(t, replies)
	for _, hit := range q.review.hits {
		if hit.row == q.review.sendRow() {
			send = hit
		}
	}
	click(send.top, 6)
	if answer := mustAnswer(t, replies); answer.Triage[0].Decision != core.TriageIgnore {
		t.Fatalf("sent %+v", answer)
	}
}

// At every size the card fits its budget, and outside the compact fallback the
// open finding's choices are on screen.
func TestReviewCardFitsAndKeepsTheChoices(t *testing.T) {
	for _, size := range [][2]int{{142, 44}, {120, 34}, {100, 24}, {90, 30}, {80, 20}, {60, 18}, {44, 14}} {
		for _, writing := range []bool{false, true} {
			m, _ := reviewModel(t, size[0], size[1])
			press(m, "down", "down")
			if writing {
				press(m, "r")
				typeText(m, strings.Repeat("a long reply that wraps ", 8))
			}
			card := m.question.card
			budget := m.bodyHeight
			if budget >= 14 {
				budget -= 3
			}
			if len(card) > max(1, budget) {
				t.Errorf("%dx%d: card is %d lines, budget %d", size[0], size[1], len(card), budget)
			}
			text := ansi.Strip(strings.Join(card, "\n"))
			for i, line := range card {
				if w := ansi.StringWidth(line); w > m.mainWidth {
					t.Errorf("%dx%d: line %d is %d wide, main %d", size[0], size[1], i, w, m.mainWidth)
				}
			}
			if !strings.Contains(text, "--timeout reaches") {
				t.Errorf("%dx%d: open finding missing:\n%s", size[0], size[1], text)
			}
			if m.question.review.hits != nil && !strings.Contains(text, "Ignore") {
				t.Errorf("%dx%d writing=%v: choices missing:\n%s", size[0], size[1], writing, text)
			}
		}
	}
}

func TestReviewEntryShowsDecisions(t *testing.T) {
	m := testModel(t, Config{})
	review := demoReview()
	arguments, _ := json.Marshal(review)
	result, _ := json.Marshal(core.ReviewResult{Decisions: []core.FindingDecision{
		{Finding: 1, Decision: core.TriageFix},
		{Finding: 2, Decision: core.TriageFixed},
		{Finding: 3, Decision: core.TriageReply, Reply: "Use the config value"},
		{Finding: 4, Decision: core.TriageIgnore},
		{Finding: 5, Decision: core.TriageOpen},
	}})
	call := &core.ToolCall{ID: "r", Name: core.ReviewTool, Arguments: arguments}
	rendered, ok := m.renderReviewEntry(entry{role: "tool", call: call, done: true, result: &core.ToolResult{Content: string(result)}}, 120)
	text := ansi.Strip(rendered)
	for _, want := range []string{"Review", "5 findings", "● Fix", "✓ Fixed", "✎ Reply", "○ Ignore", "Use the config value", "1 open"} {
		if !ok || !strings.Contains(text, want) {
			t.Fatalf("entry missing %q:\n%s", want, text)
		}
	}
	skipped, _ := json.Marshal(core.ReviewResult{Skipped: true})
	rendered, _ = m.renderReviewEntry(entry{role: "tool", call: call, done: true, result: &core.ToolResult{Content: string(skipped)}}, 120)
	if !strings.Contains(ansi.Strip(rendered), "skipped") {
		t.Fatalf("skipped entry:\n%s", ansi.Strip(rendered))
	}
	pending, _ := m.renderReviewEntry(entry{role: "tool", call: call}, 120)
	if !strings.Contains(ansi.Strip(pending), "waiting for your decisions") {
		t.Fatalf("pending entry:\n%s", ansi.Strip(pending))
	}
}

func TestTriageBridgeCarriesTheReview(t *testing.T) {
	_, triage, requests := NewBridge()
	result := make(chan core.Answer, 1)
	go func() {
		answer, _ := triage(context.Background(), demoReview())
		result <- answer
	}()
	request := <-requests
	if request.Review == nil || len(request.Review.Findings) != 5 {
		t.Fatalf("review changed in transit: %+v", request.Review)
	}
	replyQuestion(request, core.Answer{Triage: make([]core.Triage, 5)})
	select {
	case answer := <-result:
		if len(answer.Triage) != 5 {
			t.Fatalf("wrong answer %+v", answer)
		}
	case <-time.After(time.Second):
		t.Fatal("answer blocked")
	}
}

func TestReviewSkippedWhenTheTaskEnds(t *testing.T) {
	m, replies := reviewModel(t, 142, 44)
	press(m, "f")
	m.dismissInteractions("The active task has ended")
	if answer := mustAnswer(t, replies); !answer.Skipped {
		t.Fatalf("ending the task sent %+v", answer)
	}
}

func TestReviewCommandSendsAReviewRequest(t *testing.T) {
	for _, tc := range []struct{ typed, want string }{
		{"/review", "Review the uncommitted changes in this workspace"},
		{"/review   the last commit", "Review the last commit for correctness"},
	} {
		backend := promptBackend{prompts: make(chan core.Prompt, 1)}
		m := testModel(t, Config{Backend: backend})
		m.resize(120, 40)
		m.input.SetValue(tc.typed)
		m.submit()
		prompt := <-backend.prompts
		if !strings.HasPrefix(prompt.Text, tc.want) || !strings.Contains(prompt.Text, "ReviewFindings") {
			t.Fatalf("%q sent %q", tc.typed, prompt.Text)
		}
		if m.entries[len(m.entries)-1].text != prompt.Text {
			t.Fatalf("the conversation shows %q, not what was sent", m.entries[len(m.entries)-1].text)
		}
	}
	if _, ok := reviewRequest("/reviewer"); ok {
		t.Fatal("/reviewer is not /review")
	}
}

func TestReviewCommandOpensTheSampleInTheDemo(t *testing.T) {
	m := testModel(t, Config{Demo: true})
	m.resize(120, 40)
	m.input.SetValue("/review")
	m.submit()
	if m.question == nil || m.question.review == nil || !m.question.demo {
		t.Fatal("/review in the demo should open the sample card")
	}
}

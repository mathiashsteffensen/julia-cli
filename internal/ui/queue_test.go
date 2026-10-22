package ui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"julia/internal/core"
)

// turnBackend runs each turn until the test ends it. With steers set it takes
// steered messages as they arrive, as a backend does between its steps.
type turnBackend struct {
	prompts chan core.Prompt // each turn's prompt as the turn starts
	end     chan error       // ends the turn under way
	steers  bool
}

func newTurnBackend(steers bool) *turnBackend {
	return &turnBackend{prompts: make(chan core.Prompt, 8), end: make(chan error), steers: steers}
}

func (b *turnBackend) Name() string { return "fake" }
func (b *turnBackend) Run(ctx context.Context, s *core.Session, prompt core.Prompt, _ core.ToolExecutor, emit core.Emit) error {
	s.Messages = append(s.Messages, core.Message{Role: "user", Content: prompt.Text})
	b.prompts <- prompt
	var ready <-chan struct{}
	if b.steers && prompt.Steering != nil {
		ready = prompt.Steering.Ready()
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ready:
			for _, p := range prompt.Steering.Take() {
				s.Messages = append(s.Messages, core.Message{Role: "user", Content: p.Text})
				emit(core.Event{Kind: "steer", Text: p.Text})
			}
		case err := <-b.end:
			return err
		}
	}
}

func (b *turnBackend) started(t *testing.T) core.Prompt {
	t.Helper()
	select {
	case p := <-b.prompts:
		return p
	case <-time.After(2 * time.Second):
		t.Fatal("no turn started")
		return core.Prompt{}
	}
}

func (b *turnBackend) noTurn(t *testing.T) {
	t.Helper()
	select {
	case p := <-b.prompts:
		t.Fatalf("unexpected turn %q", p.Text)
	case <-time.After(50 * time.Millisecond):
	}
}

// pump feeds the running turn's stream to the model until done holds.
func pump(t *testing.T, m *model, done func() bool) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for !done() {
		select {
		case msg := <-m.stream:
			m.Update(msg)
		case <-deadline:
			t.Fatal("stream did not reach the expected state")
		}
	}
}

// endTurn ends the turn under way with err and feeds its end to the model.
func endTurn(t *testing.T, m *model, b *turnBackend, err error) {
	t.Helper()
	stream := m.stream
	b.end <- err
	deadline := time.After(2 * time.Second)
	for {
		select {
		case msg := <-stream:
			m.Update(msg)
			if msg.finished {
				return
			}
		case <-deadline:
			t.Fatal("the turn's end was not delivered")
		}
	}
}

func sendText(m *model, text string) {
	m.input.SetValue(text)
	m.Update(keyMsg("enter"))
}

func ctrlS(m *model) { m.Update(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl}) }

func userTexts(m *model) []string {
	var texts []string
	for _, e := range m.entries {
		if e.role == "user" {
			texts = append(texts, e.text)
		}
	}
	return texts
}

func busyModel(t *testing.T, steers bool) (*model, *turnBackend) {
	t.Helper()
	b := newTurnBackend(steers)
	m := testModel(t, Config{Backend: b})
	m.resize(120, 40)
	sendText(m, "Fix the build")
	b.started(t)
	return m, b
}

func TestEnterWhileBusyQueuesTheMessageUntilTheTurnSettles(t *testing.T) {
	m, b := busyModel(t, true)
	sendText(m, "then update the changelog")
	if m.input.Value() != "" || len(m.queued) != 1 {
		t.Fatalf("message not queued: draft %q, queue %+v", m.input.Value(), m.queued)
	}
	view := ansi.Strip(m.View().Content)
	if !strings.Contains(view, "QUEUED") || !strings.Contains(view, "then update the changelog") || !strings.Contains(view, "steer now") {
		t.Fatalf("queue strip missing:\n%s", view)
	}
	b.noTurn(t)
	endTurn(t, m, b, nil)
	next := b.started(t)
	if next.Text != "then update the changelog" {
		t.Fatalf("next turn %q", next.Text)
	}
	if !m.busy || len(m.queued) != 0 {
		t.Fatal("queued message did not start the next turn")
	}
	if got := userTexts(m); len(got) != 2 || got[1] != "then update the changelog" {
		t.Fatalf("conversation %q", got)
	}
}

func TestSeveralQueuedMessagesSendAsOne(t *testing.T) {
	m, b := busyModel(t, true)
	sendText(m, "first")
	sendText(m, "second")
	endTurn(t, m, b, nil)
	if next := b.started(t); next.Text != "first\n\nsecond" {
		t.Fatalf("next turn %q", next.Text)
	}
}

func TestSteerHandsTheQueueToTheTurnUnderWay(t *testing.T) {
	m, b := busyModel(t, true)
	sendText(m, "use the staging config")
	ctrlS(m)
	pump(t, m, func() bool { return len(m.queued) == 0 })
	if got := userTexts(m); len(got) != 2 || got[1] != "use the staging config" {
		t.Fatalf("steered message not shown where delivered: %q", got)
	}
	endTurn(t, m, b, nil)
	b.noTurn(t)
}

func TestCtrlSSteersTheDraftDirectly(t *testing.T) {
	m, b := busyModel(t, true)
	m.input.SetValue("stop touching the tests")
	ctrlS(m)
	if m.input.Value() != "" {
		t.Fatal("draft not sent")
	}
	pump(t, m, func() bool { return len(m.queued) == 0 })
	if got := userTexts(m); got[len(got)-1] != "stop touching the tests" {
		t.Fatalf("conversation %q", got)
	}
	b.end <- nil
}

func TestClickingTheSteerButtonSteers(t *testing.T) {
	m, b := busyModel(t, true)
	sendText(m, "also rename it")
	rows, at := m.bottomLayout()
	if at < 0 || !strings.Contains(ansi.Strip(rows[at]), "steer now") {
		t.Fatalf("no steer button in %q", rows)
	}
	y := 3 + m.bodyHeight + at
	right := m.margin() + m.mainWidth - 1
	// The button's row in the full view is the one the click lands on.
	if line := ansi.Strip(strings.Split(m.View().Content, "\n")[y]); !strings.Contains(line, "steer now") {
		t.Fatalf("row %d is %q", y, line)
	}
	m.Update(tea.MouseClickMsg{X: right - 30, Y: y, Button: tea.MouseLeft})
	if m.queued[0].steering {
		t.Fatal("a click beside the button steered")
	}
	m.Update(tea.MouseClickMsg{X: right - 2, Y: y, Button: tea.MouseLeft})
	if len(m.queued) == 1 && !m.queued[0].steering {
		t.Fatal("click on the button did not steer")
	}
	pump(t, m, func() bool { return len(m.queued) == 0 })
	b.end <- nil
}

func TestSteeredMessageTheBackendNeverTookSendsNext(t *testing.T) {
	m, b := busyModel(t, false)
	sendText(m, "and the docs")
	ctrlS(m)
	view := ansi.Strip(m.View().Content)
	if !strings.Contains(view, "STEERING") || strings.Contains(view, "steer now") {
		t.Fatalf("strip should say the message is steering:\n%s", view)
	}
	endTurn(t, m, b, nil)
	if next := b.started(t); next.Text != "and the docs" {
		t.Fatalf("next turn %q", next.Text)
	}
}

func TestInterruptReturnsTheQueueToTheComposer(t *testing.T) {
	for _, end := range []string{"esc", "error"} {
		t.Run(end, func(t *testing.T) {
			m, b := busyModel(t, true)
			sendText(m, "and deploy it")
			m.input.SetValue("draft in progress")
			if end == "esc" {
				m.Update(keyMsg("esc"))
				pump(t, m, func() bool { return !m.busy })
			} else {
				endTurn(t, m, b, errors.New("provider stopped"))
			}
			b.noTurn(t)
			if got := m.input.Value(); got != "and deploy it\n\ndraft in progress" {
				t.Fatalf("composer %q", got)
			}
			if len(m.queued) != 0 || !strings.Contains(m.notice, "back in the composer") {
				t.Fatalf("queue %+v notice %q", m.queued, m.notice)
			}
		})
	}
}

func TestUpTakesTheQueuedMessageBackToEdit(t *testing.T) {
	m, b := busyModel(t, true)
	sendText(m, "typo here")
	m.Update(keyMsg("up"))
	if m.input.Value() != "typo here" || len(m.queued) != 0 {
		t.Fatalf("draft %q queue %+v", m.input.Value(), m.queued)
	}
	endTurn(t, m, b, nil)
	b.noTurn(t)
}

func TestQueuedImagesKeepTheirNumbers(t *testing.T) {
	m, b := busyModel(t, true)
	red, blue := core.Image{MediaType: "image/png", Data: []byte("red")}, core.Image{MediaType: "image/png", Data: []byte("blue")}
	m.receiveImages(imagesMsg{images: []core.Image{red}})
	m.input.InsertString("this one")
	m.Update(keyMsg("enter"))
	m.receiveImages(imagesMsg{images: []core.Image{blue}})
	m.input.InsertString("and this one")
	m.Update(keyMsg("enter"))
	endTurn(t, m, b, nil)
	next := b.started(t)
	if next.Text != "[Image #1] this one\n\n[Image #2] and this one" || len(next.Images) != 2 || string(next.Images[1].Data) != "blue" {
		t.Fatalf("next turn %q with %d images", next.Text, len(next.Images))
	}
}

func TestCommandsWhileBusyKeepTheDraft(t *testing.T) {
	m := testModel(t, Config{})
	m.busy = true
	m.input.SetValue("/clear")
	if cmd := m.submit(); cmd != nil {
		t.Fatal("ran a command mid-turn")
	}
	if m.input.Value() != "/clear" || len(m.queued) != 0 || !strings.Contains(m.notice, "Draft kept") {
		t.Fatalf("draft %q queue %+v notice %q", m.input.Value(), m.queued, m.notice)
	}
}

func TestQueueStripFitsEverySize(t *testing.T) {
	for _, size := range [][2]int{{22, 15}, {40, 18}, {60, 24}, {90, 30}, {142, 44}} {
		m := testModel(t, Config{Demo: true})
		m.resize(size[0], size[1])
		m.busy, m.inbox = true, core.NewInbox()
		m.queue(core.Prompt{Text: strings.Repeat("a long queued message ", 10)})
		m.queue(core.Prompt{Text: "second"})
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		lines := strings.Split(m.View().Content, "\n")
		if len(lines) > size[1] {
			t.Fatalf("%v: view has %d rows", size, len(lines))
		}
		for _, line := range lines {
			if ansi.StringWidth(line) > size[0] {
				t.Fatalf("%v: row too wide: %q", size, ansi.Strip(line))
			}
		}
		view := ansi.Strip(m.View().Content)
		if !strings.Contains(view, "QUEUED") || !strings.Contains(view, "steer") {
			t.Fatalf("%v: strip missing:\n%s", size, view)
		}
	}
}

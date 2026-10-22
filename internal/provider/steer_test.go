package provider

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"charm.land/fantasy"
	"julia/internal/core"
)

func steerTexts(events []core.Event) []string {
	var texts []string
	for _, e := range events {
		if e.Kind == "steer" {
			texts = append(texts, e.Text)
		}
	}
	return texts
}

func userContents(messages []core.Message) []string {
	var texts []string
	for _, m := range messages {
		if m.Role == "user" {
			texts = append(texts, m.Content)
		}
	}
	return texts
}

func TestAPISteeringJoinsTheTurnAfterTheToolStep(t *testing.T) {
	model := &scriptedAPIModel{steps: [][]fantasy.StreamPart{toolStep(fantasy.Usage{}), textStep("Done, with the tests too.", fantasy.Usage{})}}
	backend := &API{Model: model, Provider: "anthropic"}
	inbox := core.NewInbox()
	inbox.Send(core.Prompt{Text: "Run the tests as well"})
	session := &core.Session{}
	var events []core.Event
	if err := backend.Run(context.Background(), session, core.Prompt{Text: "List files", Steering: inbox}, &testTools{}, func(e core.Event) { events = append(events, e) }); err != nil {
		t.Fatal(err)
	}
	if model.streams != 2 {
		t.Fatalf("model called %d times", model.streams)
	}
	// The second request carries the tool's result and then the user's message.
	sent := model.calls[1].Prompt
	last := sent[len(sent)-1]
	if last.Role != fantasy.MessageRoleUser || !strings.Contains(textOf(last), "Run the tests as well") || sent[len(sent)-2].Role != fantasy.MessageRoleTool {
		t.Fatalf("steered message not after the tool result: %+v", sent)
	}
	if got := steerTexts(events); len(got) != 1 || got[0] != "Run the tests as well" {
		t.Fatalf("steer events %q", got)
	}
	if got := userContents(session.Messages); len(got) != 2 || got[1] != "Run the tests as well" {
		t.Fatalf("session user messages %q", got)
	}
	if left := inbox.Take(); len(left) != 0 {
		t.Fatalf("inbox kept %+v", left)
	}
}

func TestAPISteeringAfterTheFinalAnswerContinuesTheTurn(t *testing.T) {
	model := &scriptedAPIModel{steps: [][]fantasy.StreamPart{textStep("Here it is.", fantasy.Usage{}), textStep("And in French.", fantasy.Usage{})}}
	backend := &API{Model: model, Provider: "anthropic"}
	inbox := core.NewInbox()
	inbox.Send(core.Prompt{Text: "also in French"})
	session := &core.Session{}
	var events []core.Event
	if err := backend.Run(context.Background(), session, core.Prompt{Text: "Summarize", Steering: inbox}, &testTools{}, func(e core.Event) { events = append(events, e) }); err != nil {
		t.Fatal(err)
	}
	if model.streams != 2 {
		t.Fatalf("model called %d times, want the steered message answered", model.streams)
	}
	for _, e := range events {
		if e.Kind == "text" && e.Text == "\n\n" {
			t.Fatal("a steered step must not pad the previous answer")
		}
	}
	if got := userContents(session.Messages); len(got) != 2 || got[1] != "also in French" {
		t.Fatalf("session user messages %q", got)
	}
	history := decodeHistory(t, session)
	if len(history) != 4 || history[2].Role != fantasy.MessageRoleUser {
		t.Fatalf("history shape %+v", history)
	}
}

func TestAPIWithoutSteeringEndsAtTheAnswer(t *testing.T) {
	model := &scriptedAPIModel{steps: [][]fantasy.StreamPart{textStep("Here it is.", fantasy.Usage{})}}
	backend := &API{Model: model, Provider: "anthropic"}
	if err := backend.Run(context.Background(), &core.Session{}, core.Prompt{Text: "Summarize", Steering: core.NewInbox()}, &testTools{}, func(core.Event) {}); err != nil {
		t.Fatal(err)
	}
	if model.streams != 1 {
		t.Fatalf("model called %d times", model.streams)
	}
}

func textOf(m fantasy.Message) string {
	var b strings.Builder
	for _, part := range m.Content {
		if text, ok := part.(fantasy.TextPart); ok {
			b.WriteString(text.Text)
		}
	}
	return b.String()
}

func TestCodexSteersTheTurnUnderWay(t *testing.T) {
	f := newCodexFixture(t)
	f.turn = func(f *codexFixture) {
		f.push(map[string]any{"method": "item/agentMessage/delta", "params": map[string]any{"threadId": "thread-1", "turnId": "turn-1", "itemId": "a", "delta": "Working. "}})
	}
	// The turn ends only once the steered message has arrived.
	f.steer = func(f *codexFixture, id json.RawMessage) {
		f.respond(id, map[string]any{"turnId": "turn-1"})
		f.push(map[string]any{"method": "item/agentMessage/delta", "params": map[string]any{"threadId": "thread-1", "turnId": "turn-1", "itemId": "b", "delta": "Also done."}})
		f.finish("completed")
	}
	inbox := core.NewInbox()
	session := &core.Session{Directory: t.TempDir()}
	var events []core.Event
	emit := func(e core.Event) {
		events = append(events, e)
		// The user steers once the reply is under way.
		if e.Kind == "text" && e.Text == "Working. " {
			inbox.Send(core.Prompt{Text: "check the docs too", Images: []core.Image{{MediaType: "image/png", Data: []byte("png")}}})
		}
	}
	if err := (&Codex{start: f.start}).Run(context.Background(), session, core.Prompt{Text: "Fix it", Steering: inbox}, &codexTestExecutor{}, emit); err != nil {
		t.Fatal(err)
	}
	var params struct {
		ThreadID       string           `json:"threadId"`
		ExpectedTurnID string           `json:"expectedTurnId"`
		Input          []map[string]any `json:"input"`
	}
	for _, r := range f.requests {
		if string(r["method"]) == `"turn/steer"` {
			if err := json.Unmarshal(r["params"], &params); err != nil {
				t.Fatal(err)
			}
		}
	}
	if params.ThreadID != "thread-1" || params.ExpectedTurnID != "turn-1" || len(params.Input) != 2 || params.Input[0]["text"] != "check the docs too" || params.Input[1]["type"] != "image" {
		t.Fatalf("turn/steer params %+v", params)
	}
	if got := steerTexts(events); len(got) != 1 || got[0] != "check the docs too" {
		t.Fatalf("steer events %q", got)
	}
	// The text before the steer stays its own reply, ahead of the message.
	var roles []string
	for _, m := range session.Messages {
		roles = append(roles, m.Role+":"+m.Content)
	}
	if strings.Join(roles, "|") != "user:Fix it|assistant:Working. |user:check the docs too|assistant:Also done." {
		t.Fatalf("transcript %q", roles)
	}
}

func TestCodexRefusedSteerLeavesTheMessageForTheNextTurn(t *testing.T) {
	f := newCodexFixture(t)
	f.steer = func(f *codexFixture, id json.RawMessage) {
		f.push(map[string]any{"id": id, "error": map[string]any{"code": -32600, "message": "no active turn"}})
		f.finish("completed")
	}
	inbox := core.NewInbox()
	inbox.Send(core.Prompt{Text: "one more thing"})
	session := &core.Session{Directory: t.TempDir()}
	var events []core.Event
	if err := (&Codex{start: f.start}).Run(context.Background(), session, core.Prompt{Text: "Fix it", Steering: inbox}, &codexTestExecutor{}, func(e core.Event) { events = append(events, e) }); err != nil {
		t.Fatal(err)
	}
	if got := steerTexts(events); len(got) != 0 {
		t.Fatalf("reported an undelivered message: %q", got)
	}
	if got := userContents(session.Messages); len(got) != 1 {
		t.Fatalf("recorded an undelivered message: %q", got)
	}
	if left := inbox.Take(); len(left) != 1 || left[0].Text != "one more thing" {
		t.Fatalf("message not left for the next turn: %+v", left)
	}
	steers := 0
	for _, r := range f.requests {
		if string(r["method"]) == `"turn/steer"` {
			steers++
		}
	}
	if steers != 1 {
		t.Fatalf("asked Codex %d times after a refusal", steers)
	}
}

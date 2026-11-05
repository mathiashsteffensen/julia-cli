package provider

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"charm.land/fantasy"
	"julia/internal/core"
)

func TestAPIStreamsAndPersistsReasoningSeparately(t *testing.T) {
	model := &scriptedAPIModel{parts: []fantasy.StreamPart{
		{Type: fantasy.StreamPartTypeReasoningStart, ID: "r"},
		{Type: fantasy.StreamPartTypeReasoningDelta, ID: "r", Delta: "First "},
		{Type: fantasy.StreamPartTypeReasoningDelta, ID: "r", Delta: "thought."},
		{Type: fantasy.StreamPartTypeReasoningEnd, ID: "r"},
		{Type: fantasy.StreamPartTypeTextStart, ID: "t"},
		{Type: fantasy.StreamPartTypeTextDelta, ID: "t", Delta: "The answer."},
		{Type: fantasy.StreamPartTypeTextEnd, ID: "t"},
		{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop},
	}}
	session := &core.Session{}
	var reasoning, answer []string
	backend := &API{Model: model}
	if err := backend.Run(context.Background(), session, core.Prompt{Text: "Question"}, &testTools{}, func(event core.Event) {
		switch event.Kind {
		case "reasoning":
			reasoning = append(reasoning, event.Text)
		case "text":
			answer = append(answer, event.Text)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(reasoning, []string{"First ", "thought."}) || !reflect.DeepEqual(answer, []string{"The answer."}) {
		t.Fatalf("streamed reasoning=%q answer=%q", reasoning, answer)
	}
	if len(session.Messages) != 2 || session.Messages[1].Reasoning != "First thought." || session.Messages[1].Content != "The answer." {
		t.Fatalf("transcript lost or duplicated reasoning: %+v", session.Messages)
	}

	data, err := json.Marshal(session)
	if err != nil {
		t.Fatal(err)
	}
	var restored core.Session
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.Messages[1].Reasoning != "First thought." || restored.Messages[1].Content != "The answer." {
		t.Fatalf("round trip: %+v", restored.Messages)
	}

	// A provider switch rebuilds from the transcript, not the old model state.
	restored.State = nil
	if err := backend.Run(context.Background(), &restored, core.Prompt{Text: "Follow up"}, &testTools{}, func(core.Event) {}); err != nil {
		t.Fatal(err)
	}
	request, err := json.Marshal(model.calls[1].Prompt)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(request), "First thought.") || !strings.Contains(string(request), "The answer.") {
		t.Fatalf("rebuilt history leaked reasoning or lost answer: %s", request)
	}
}

func TestAPITranscriptKeepsReasoningOnlyAssistantMessages(t *testing.T) {
	session := &core.Session{}
	appendTranscript(session, []fantasy.Message{
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{
			fantasy.ReasoningPart{Text: "first"},
			fantasy.ReasoningPart{Text: " second"},
		}},
		{Role: fantasy.MessageRoleUser, Content: []fantasy.MessagePart{fantasy.ReasoningPart{Text: "not assistant reasoning"}}},
	})
	if len(session.Messages) != 1 || session.Messages[0].Reasoning != "first second" || session.Messages[0].Content != "" {
		t.Fatalf("reasoning-only transcript: %+v", session.Messages)
	}
}

func TestAPIInterruptedReasoningRetained(t *testing.T) {
	for _, tc := range []struct {
		name     string
		withText bool
		cancel   bool
	}{
		{name: "reasoning only"},
		{name: "reasoning and answer", withText: true},
		{name: "cancel", cancel: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parts := []fantasy.StreamPart{
				{Type: fantasy.StreamPartTypeReasoningStart, ID: "r"},
				{Type: fantasy.StreamPartTypeReasoningDelta, ID: "r", Delta: "Partial thought"},
			}
			if tc.withText {
				parts = append(parts,
					fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: "t"},
					fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "t", Delta: "Partial answer"},
				)
			}
			parts = append(parts, fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: errors.New("connection interrupted")})
			model := &scriptedAPIModel{parts: parts}
			session := &core.Session{}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var got string
			err := (&API{Model: model}).Run(ctx, session, core.Prompt{Text: "Question"}, &testTools{}, func(event core.Event) {
				if event.Kind == "reasoning" {
					got += event.Text
					if tc.cancel {
						cancel()
					}
				}
			})
			if err == nil || got != "Partial thought" || len(session.Messages) != 2 {
				t.Fatalf("err=%v reasoning=%q messages=%+v", err, got, session.Messages)
			}
			answer := "[Response interrupted]"
			if tc.withText {
				answer = "Partial answer\n\n" + answer
			}
			if message := session.Messages[1]; message.Reasoning != "Partial thought" || message.Content != answer {
				t.Fatalf("interrupted transcript: %+v", message)
			}
		})
	}
}

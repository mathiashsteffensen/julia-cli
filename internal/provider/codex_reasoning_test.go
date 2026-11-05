package provider

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"julia/internal/core"
)

func TestCodexReasoningSummaries(t *testing.T) {
	f := newCodexFixture(t)
	f.turn = func(f *codexFixture) {
		delta := func(thread, turn, item string, index int, text string) {
			f.push(map[string]any{"method": "item/reasoning/summaryTextDelta", "params": map[string]any{
				"threadId": thread, "turnId": turn, "itemId": item, "summaryIndex": index, "delta": text,
			}})
		}
		completed := func(thread, turn, item string, summaries []string) {
			f.push(map[string]any{"method": "item/completed", "params": map[string]any{
				"threadId": thread, "turnId": turn,
				"item": map[string]any{"type": "reasoning", "id": item, "summary": summaries, "content": []string{"not a summary"}, "encrypted_content": "secret"},
			}})
		}

		delta("other", "turn-1", "bad", 0, "wrong thread")
		delta("thread-1", "other", "bad", 0, "wrong turn")
		delta("", "turn-1", "bad", 0, "missing thread")
		delta("thread-1", "", "bad", 0, "missing turn")
		delta("thread-1", "turn-1", "bad", -1, "invalid index")
		f.push(map[string]any{"method": "item/reasoning/summaryTextDelta", "params": map[string]any{
			"threadId": "thread-1", "turnId": "turn-1", "itemId": "bad", "delta": "missing index",
		}})
		completed("thread-1", "other", "bad", []string{"wrong completion"})
		completed("", "turn-1", "bad", []string{"missing thread"})
		delta("thread-1", "turn-1", "one", 0, "First")
		delta("thread-1", "turn-1", "one", 0, " part")
		delta("thread-1", "turn-1", "one", 1, "Second")
		completed("thread-1", "turn-1", "one", []string{"First part", "Second part", "Third"})
		completed("thread-1", "turn-1", "one", []string{"First part", "Second part", "Third"})
		delta("thread-1", "turn-1", "one", 1, " duplicate")
		completed("thread-1", "turn-1", "two", []string{"Fourth", "Fifth"})
		f.push(map[string]any{"method": "item/reasoning/textDelta", "params": map[string]any{
			"threadId": "thread-1", "turnId": "turn-1", "itemId": "two", "delta": "not a summary", "contentIndex": 0,
		}})
		f.push(map[string]any{"method": "item/agentMessage/delta", "params": map[string]any{
			"threadId": "thread-1", "turnId": "turn-1", "itemId": "answer", "delta": "Answer",
		}})
		f.finish("completed")
	}

	var checkpoints [][]byte
	backend := &Codex{start: f.start, Checkpoint: func(s *core.Session) error {
		data, err := json.Marshal(s.Messages)
		if err == nil {
			checkpoints = append(checkpoints, data)
		}
		return err
	}}
	session := &core.Session{Directory: t.TempDir()}
	var reasoning, answer strings.Builder
	err := backend.Run(context.Background(), session, core.Prompt{Text: "Question"}, nil, func(e core.Event) {
		switch e.Kind {
		case "reasoning":
			reasoning.WriteString(e.Text)
		case "text":
			answer.WriteString(e.Text)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "First part\n\nSecond part\n\nThird\n\nFourth\n\nFifth"
	if reasoning.String() != want || answer.String() != "Answer" {
		t.Fatalf("reasoning %q; answer %q", reasoning.String(), answer.String())
	}
	if len(session.Messages) != 2 || session.Messages[1].Reasoning != want || session.Messages[1].Content != "Answer" {
		t.Fatalf("messages: %+v", session.Messages)
	}
	if len(checkpoints) == 0 || !strings.Contains(string(checkpoints[len(checkpoints)-1]), `"reasoning":"First part`) {
		t.Fatalf("reasoning not checkpointed: %q", checkpoints)
	}
	var restored []core.Message
	if err := json.Unmarshal(checkpoints[len(checkpoints)-1], &restored); err != nil || len(restored) != 2 || restored[1].Reasoning != want {
		t.Fatalf("reasoning did not survive serialization: %+v, %v", restored, err)
	}
	roundTrip := withoutImageData(session.Messages)
	if roundTrip[1].Reasoning != "" || session.Messages[1].Reasoning != want {
		t.Fatalf("migration included display-only reasoning or mutated session: %+v", roundTrip)
	}
}

func TestCodexReasoningBeforeToolIsSaved(t *testing.T) {
	f := newCodexFixture(t)
	f.turn = func(f *codexFixture) {
		f.push(map[string]any{"method": "item/reasoning/summaryTextDelta", "params": map[string]any{
			"threadId": "thread-1", "turnId": "turn-1", "itemId": "thought", "summaryIndex": 0, "delta": "Before tool",
		}})
		f.push(map[string]any{"id": "rpc", "method": "item/tool/call", "params": map[string]any{
			"threadId": "thread-1", "turnId": "turn-1", "callId": "call", "tool": "Read", "arguments": map[string]any{"path": "README.md"},
		}})
		f.push(map[string]any{"method": "item/completed", "params": map[string]any{
			"threadId": "thread-1", "turnId": "turn-1",
			"item": map[string]any{"type": "reasoning", "id": "thought", "summary": []string{"Before tool"}},
		}})
		f.finish("completed")
	}
	session := &core.Session{Directory: t.TempDir()}
	var reasoning strings.Builder
	err := (&Codex{start: f.start}).Run(context.Background(), session, core.Prompt{Text: "Question"}, &codexTestExecutor{}, func(e core.Event) {
		if e.Kind == "reasoning" {
			reasoning.WriteString(e.Text)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if reasoning.String() != "Before tool" || len(session.Messages) != 4 || session.Messages[1].Reasoning != "Before tool" || session.Messages[1].Content != "" {
		t.Fatalf("reasoning %q; messages %+v", reasoning.String(), session.Messages)
	}
}

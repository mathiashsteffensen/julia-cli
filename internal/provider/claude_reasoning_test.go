package provider

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"julia/internal/core"
)

func TestClaudeReasoningStreamAndFallback(t *testing.T) {
	f := newClaudeFixture(t)
	f.turn = func(f *claudeFixture) {
		stream := func(event map[string]any) {
			f.push(map[string]any{"type": "stream_event", "event": event})
		}
		assistant := func(blocks ...any) {
			f.push(map[string]any{"type": "assistant", "message": map[string]any{"id": "m", "content": blocks}})
		}
		stream(map[string]any{"type": "message_start", "message": map[string]any{"id": "m"}})
		stream(map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "thinking"}})
		stream(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "thinking_delta", "thinking": "Think "}})
		stream(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "thinking_delta", "thinking": "first."}})
		stream(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "signature_delta", "signature": "secret-signature"}})
		assistant(map[string]any{"type": "thinking", "thinking": "Think first.", "signature": "secret-signature"})
		stream(map[string]any{"type": "content_block_stop", "index": 0})

		// This single-block message has local index zero but stream index one.
		stream(map[string]any{"type": "content_block_start", "index": 1, "content_block": map[string]any{"type": "text"}})
		stream(map[string]any{"type": "content_block_delta", "index": 1, "delta": map[string]any{"type": "text_delta", "text": "Answer."}})
		assistant(map[string]any{"type": "text", "text": "Answer."})
		stream(map[string]any{"type": "content_block_stop", "index": 1})

		// A later unstreamed thinking block must not be hidden by the first.
		stream(map[string]any{"type": "content_block_start", "index": 2, "content_block": map[string]any{"type": "thinking"}})
		assistant(map[string]any{"type": "thinking", "thinking": " More thinking."})
		stream(map[string]any{"type": "content_block_stop", "index": 2})
		assistant(
			map[string]any{"type": "thinking", "thinking": "Think first."},
			map[string]any{"type": "text", "text": "Answer."},
			map[string]any{"type": "thinking", "thinking": " More thinking."},
		)
		assistant(map[string]any{"type": "redacted_thinking", "data": "secret-redacted"})
		f.finish(false, "Answer.", nil)
	}
	session := &core.Session{Directory: t.TempDir()}
	var events []string
	var saved []core.Message
	backend := &Claude{start: f.start, Checkpoint: func(s *core.Session) error {
		saved = append([]core.Message(nil), s.Messages...)
		return nil
	}}
	err := backend.Run(context.Background(), session, core.Prompt{Text: "hi"}, nil, func(e core.Event) {
		if e.Kind == "reasoning" || e.Kind == "text" {
			events = append(events, e.Kind+":"+e.Text)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"reasoning:Think ", "reasoning:first.", "text:Answer.", "reasoning: More thinking."}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events: %#v; want %#v", events, want)
	}
	if len(session.Messages) != 2 || session.Messages[1].Content != "Answer." || session.Messages[1].Reasoning != "Think first. More thinking." {
		t.Fatalf("transcript: %+v", session.Messages)
	}
	if !reflect.DeepEqual(saved, session.Messages) {
		t.Fatalf("saved transcript: %+v", saved)
	}
}

func TestClaudeReasoningCompletedMessageFallback(t *testing.T) {
	f := newClaudeFixture(t)
	f.turn = func(f *claudeFixture) {
		f.push(map[string]any{"type": "assistant", "message": map[string]any{"id": "m", "content": []any{
			map[string]any{"type": "thinking", "thinking": "First."},
			map[string]any{"type": "redacted_thinking", "data": "hidden"},
			map[string]any{"type": "thinking", "thinking": " Second."},
			map[string]any{"type": "text", "text": "Done."},
		}}})
		// Subagent output must not enter the top-level transcript.
		f.push(map[string]any{"type": "assistant", "parent_tool_use_id": "child", "message": map[string]any{"id": "child", "content": []any{map[string]any{"type": "thinking", "thinking": "child"}}}})
		f.finish(false, "Done.", nil)
	}
	session := &core.Session{Directory: t.TempDir()}
	var events []string
	if err := (&Claude{start: f.start}).Run(context.Background(), session, core.Prompt{Text: "hi"}, nil, func(e core.Event) {
		if e.Kind == "reasoning" || e.Kind == "text" {
			events = append(events, e.Kind+":"+e.Text)
		}
	}); err != nil {
		t.Fatal(err)
	}
	want := []string{"reasoning:First.", "reasoning: Second.", "text:Done."}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events: %#v; want %#v", events, want)
	}
	if len(session.Messages) != 2 || session.Messages[1].Reasoning != "First. Second." || session.Messages[1].Content != "Done." {
		t.Fatalf("transcript: %+v", session.Messages)
	}
}

func TestClaudeReasoningOnlyAndInterruptedAreCheckpointed(t *testing.T) {
	for _, interrupted := range []bool{false, true} {
		name := "completed"
		if interrupted {
			name = "interrupted"
		}
		t.Run(name, func(t *testing.T) {
			f := newClaudeFixture(t)
			f.turn = func(f *claudeFixture) {
				f.push(map[string]any{"type": "stream_event", "event": map[string]any{"type": "message_start", "message": map[string]any{"id": "m"}}})
				f.push(map[string]any{"type": "stream_event", "event": map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "thinking_delta", "thinking": "Partial"}}})
				if interrupted {
					f.incoming <- claudeRead{Err: io.EOF}
				} else {
					f.finish(false, "", nil)
				}
			}
			session := &core.Session{Directory: t.TempDir()}
			var saved []core.Message
			backend := &Claude{start: f.start, Checkpoint: func(s *core.Session) error {
				saved = append([]core.Message(nil), s.Messages...)
				return nil
			}}
			err := backend.Run(context.Background(), session, core.Prompt{Text: "hi"}, nil, nil)
			if interrupted && !errors.Is(err, io.EOF) || !interrupted && err != nil {
				t.Fatalf("run error: %v", err)
			}
			if len(session.Messages) != 2 || session.Messages[1].Content != "" || session.Messages[1].Reasoning != "Partial" {
				t.Fatalf("transcript: %+v", session.Messages)
			}
			if !reflect.DeepEqual(saved, session.Messages) {
				t.Fatalf("saved transcript: %+v", saved)
			}
		})
	}
}

func TestClaudeInterruptedReasoningCheckpointError(t *testing.T) {
	f := newClaudeFixture(t)
	f.turn = func(f *claudeFixture) {
		f.push(map[string]any{"type": "stream_event", "event": map[string]any{"type": "message_start", "message": map[string]any{"id": "m"}}})
		f.push(map[string]any{"type": "stream_event", "event": map[string]any{"type": "content_block_delta", "delta": map[string]any{"type": "thinking_delta", "thinking": "Partial"}}})
		f.incoming <- claudeRead{Err: io.EOF}
	}
	backend := &Claude{start: f.start, Checkpoint: func(s *core.Session) error {
		if len(s.Messages) > 1 {
			return errors.New("disk full")
		}
		return nil
	}}
	err := backend.Run(context.Background(), &core.Session{Directory: t.TempDir()}, core.Prompt{Text: "hi"}, nil, nil)
	if !errors.Is(err, io.EOF) || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("error: %v", err)
	}
}

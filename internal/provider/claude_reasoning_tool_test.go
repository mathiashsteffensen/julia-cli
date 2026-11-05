package provider

import (
	"context"
	"testing"

	"julia/internal/core"
)

func TestClaudeReasoningFlushesBeforeTool(t *testing.T) {
	f := newClaudeFixture(t)
	f.turn = func(f *claudeFixture) {
		f.push(map[string]any{"type": "stream_event", "event": map[string]any{"type": "message_start", "message": map[string]any{"id": "m"}}})
		f.push(map[string]any{"type": "stream_event", "event": map[string]any{"type": "content_block_delta", "delta": map[string]any{"type": "thinking_delta", "thinking": "Before tool."}}})
		f.request("mcp-call", map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{"name": "Read", "arguments": map[string]any{"path": "README.md"}, "_meta": map[string]any{"claudecode/toolUseId": "toolu_01"}}})
		f.text("next", "Done.")
		f.finish(false, "Done.", nil)
	}
	var savedBeforeTool bool
	backend := &Claude{start: f.start, Checkpoint: func(s *core.Session) error {
		if len(s.Messages) == 2 && s.Messages[1].Reasoning == "Before tool." {
			savedBeforeTool = true
		}
		return nil
	}}
	session := &core.Session{Directory: t.TempDir()}
	if err := backend.Run(context.Background(), session, core.Prompt{Text: "hi"}, &claudeTestExecutor{}, nil); err != nil {
		t.Fatal(err)
	}
	if !savedBeforeTool || len(session.Messages) != 5 || session.Messages[1].Reasoning != "Before tool." || session.Messages[1].Content != "" || len(session.Messages[2].ToolCalls) != 1 || session.Messages[2].ToolCalls[0].ID != "toolu_01" || session.Messages[4].Content != "Done." {
		t.Fatalf("savedBeforeTool=%v transcript=%+v", savedBeforeTool, session.Messages)
	}
}

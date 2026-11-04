package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"julia/internal/core"
	"julia/internal/tools"
	"julia/internal/ui"
)

// The app-server finishes while the real interaction bridge is waiting for the
// human, so its final message cannot have consumed the tool response.
func TestCodexContinuesAfterEarlyInteractionCompletion(t *testing.T) {
	cases := []struct {
		name     string
		args     string
		answer   core.Answer
		contains string
	}{
		{core.QuestionTool, `{"question":"Choose?","options":[{"label":"Yes"},{"label":"No"}]}`, core.Answer{Option: 1}, `"answer":"Yes"`},
		{core.ReviewTool, `{"findings":[{"severity":"P1","title":"Bug"}]}`, core.Answer{Triage: []core.Triage{{Decision: core.TriageFix}}}, `"decision":"fix"`},
		{core.QuestionTool + "/skipped", `{"question":"Choose?"}`, core.Answer{Skipped: true}, `"skipped":true`},
		{core.ReviewTool + "/skipped", `{"findings":[{"severity":"P1","title":"Bug"}]}`, core.Answer{Skipped: true}, `"skipped":true`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newCodexFixture(t)
			toolName := strings.Split(tc.name, "/")[0]
			turns := 0
			f.turn = func(f *codexFixture) {
				turns++
				if turns > 1 {
					f.push(map[string]any{"method": "item/agentMessage/delta", "params": map[string]any{"threadId": "thread-1", "turnId": "turn-1", "itemId": "b", "delta": "Acting on your answer."}})
					f.finish("completed")
					return
				}
				f.push(map[string]any{"id": "rpc-tool", "method": "item/tool/call", "params": map[string]any{"threadId": "thread-1", "turnId": "turn-1", "callId": "call", "tool": toolName, "arguments": json.RawMessage(tc.args)}})
				f.push(map[string]any{"method": "item/agentMessage/delta", "params": map[string]any{"threadId": "thread-1", "turnId": "turn-1", "itemId": "a", "delta": "Still waiting for your decision."}})
				f.finish("completed")
			}
			delivered := make(chan string, 1)
			start := func(ctx context.Context, binary, dir string) (*codexClient, error) {
				c, err := f.start(ctx, binary, dir)
				if err != nil {
					return nil, err
				}
				underlying := c.input
				c.input = codexTestWriter(func(p []byte) (int, error) {
					var m map[string]json.RawMessage
					_ = json.Unmarshal(p, &m)
					n, e := underlying.Write(p)
					if string(m["id"]) == `"rpc-tool"` && len(m["result"]) > 0 {
						var result struct {
							ContentItems []struct {
								Text string `json:"text"`
							} `json:"contentItems"`
						}
						_ = json.Unmarshal(m["result"], &result)
						if len(result.ContentItems) > 0 {
							delivered <- result.ContentItems[0].Text
						}
					}
					return n, e
				})
				return c, nil
			}
			ask, triage, requests := ui.NewBridge()
			executor := tools.WithInteractions(&codexTestExecutor{}, ask, triage)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			finished := make(chan error, 1)
			var text strings.Builder
			go func() {
				finished <- (&Codex{start: start}).Run(ctx, &core.Session{Directory: t.TempDir()}, core.Prompt{Text: "Task"}, executor, func(e core.Event) {
					if e.Kind == "text" {
						text.WriteString(e.Text)
					}
				})
			}()
			var request ui.QuestionRequest
			select {
			case request = <-requests:
			case <-ctx.Done():
				t.Fatal("no card")
			}
			select {
			case err := <-finished:
				t.Fatalf("completed before answer: %v", err)
			default:
			}
			request.Reply <- tc.answer
			select {
			case result := <-delivered:
				if !strings.Contains(result, tc.contains) {
					t.Fatalf("wrong result: %s", result)
				}
			case <-ctx.Done():
				t.Fatal("answer not delivered")
			}
			select {
			case err := <-finished:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal("provider did not finish")
			}
			if turns != 2 {
				t.Fatalf("want one continuation, got %d turns", turns)
			}
			var starts []json.RawMessage
			for _, request := range f.requests {
				if string(request["method"]) == `"turn/start"` {
					starts = append(starts, request["params"])
				}
			}
			var continuation struct {
				ThreadID string `json:"threadId"`
				Input    []struct {
					Text string `json:"text"`
				} `json:"input"`
			}
			if len(starts) != 2 || json.Unmarshal(starts[1], &continuation) != nil || continuation.ThreadID != "thread-1" || len(continuation.Input) != 1 {
				t.Fatalf("invalid continuation: %s", starts)
			}
			prefix, payload, ok := strings.Cut(continuation.Input[0].Text, "\n\n")
			var actual codexLateInteraction
			if !ok || !strings.Contains(prefix, "JULIA system continuation") || json.Unmarshal([]byte(payload), &actual) != nil || actual.Call.Name != toolName || !strings.Contains(actual.Result.Content, tc.contains) {
				t.Fatalf("continuation did not carry the actual interaction: %s", continuation.Input[0].Text)
			}
			if text.String() != "Acting on your answer." {
				t.Fatalf("unexpected final text %q", text.String())
			}
		})
	}
}

func TestCodexInteractionNormalCompletionDoesNotContinue(t *testing.T) {
	for _, name := range []string{core.QuestionTool, core.ReviewTool} {
		t.Run(name, func(t *testing.T) {
			args := `{"question":"Choose?","options":[{"label":"Yes"},{"label":"No"}]}`
			answer := core.Answer{Option: 1}
			if name == core.ReviewTool {
				args = `{"findings":[{"severity":"P1","title":"Bug"}]}`
				answer = core.Answer{Triage: []core.Triage{{Decision: core.TriageFix}}}
			}
			f := newCodexFixture(t)
			turns := 0
			f.turn = func(f *codexFixture) {
				turns++
				f.push(map[string]any{"id": "rpc-tool", "method": "item/tool/call", "params": map[string]any{"threadId": "thread-1", "turnId": "turn-1", "callId": "call", "tool": name, "arguments": json.RawMessage(args)}})
			}
			start := func(ctx context.Context, binary, dir string) (*codexClient, error) {
				client, err := f.start(ctx, binary, dir)
				if err != nil {
					return nil, err
				}
				underlying := client.input
				client.input = codexTestWriter(func(p []byte) (int, error) {
					var message map[string]json.RawMessage
					_ = json.Unmarshal(p, &message)
					n, err := underlying.Write(p)
					if string(message["id"]) == `"rpc-tool"` && len(message["result"]) > 0 {
						f.push(map[string]any{"method": "item/agentMessage/delta", "params": map[string]any{"threadId": "thread-1", "turnId": "turn-1", "itemId": "a", "delta": "Acting on your answer."}})
					}
					return n, err
				})
				return client, nil
			}
			ask, triage, requests := ui.NewBridge()
			executor := tools.WithInteractions(&codexTestExecutor{}, ask, triage)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			finished := make(chan error, 1)
			textEvents := make(chan string, 1)
			var text strings.Builder
			go func() {
				finished <- (&Codex{start: start}).Run(ctx, &core.Session{Directory: t.TempDir()}, core.Prompt{Text: "Task"}, executor, func(e core.Event) {
					if e.Kind == "text" {
						text.WriteString(e.Text)
						textEvents <- e.Text
					}
				})
			}()
			select {
			case request := <-requests:
				request.Reply <- answer
			case <-ctx.Done():
				t.Fatal("no card")
			}
			select {
			case <-textEvents:
				f.finish("completed")
			case <-ctx.Done():
				t.Fatal("post-response text did not stream before completion")
			}
			select {
			case err := <-finished:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal("provider did not finish")
			}
			if turns != 1 || text.String() != "Acting on your answer." {
				t.Fatalf("turns=%d text=%q", turns, text.String())
			}
		})
	}
}

func TestCodexCancelledInteractionLeavesNoUncertainIntent(t *testing.T) {
	for _, name := range []string{core.QuestionTool, core.ReviewTool} {
		t.Run(name, func(t *testing.T) {
			args := `{"question":"Choose?"}`
			if name == core.ReviewTool {
				args = `{"findings":[{"severity":"P1","title":"Bug"}]}`
			}
			f := newCodexFixture(t)
			f.turn = func(f *codexFixture) {
				f.push(map[string]any{"id": "rpc-tool", "method": "item/tool/call", "params": map[string]any{"threadId": "thread-1", "turnId": "turn-1", "callId": "call", "tool": name, "arguments": json.RawMessage(args)}})
			}
			ask, triage, requests := ui.NewBridge()
			session := &core.Session{Directory: t.TempDir()}
			ctx, cancel := context.WithCancel(context.Background())
			finished := make(chan error, 1)
			go func() {
				finished <- (&Codex{start: f.start}).Run(ctx, session, core.Prompt{Text: "Task"}, tools.WithInteractions(&codexTestExecutor{}, ask, triage), nil)
			}()
			select {
			case <-requests:
				cancel()
			case <-time.After(5 * time.Second):
				cancel()
				t.Fatal("no card")
			}
			select {
			case err := <-finished:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("got %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("cancellation blocked")
			}
			journal, err := loadCodexJournal(session.State)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := journal.Calls["call"]; ok || journal.PendingContinuation != "" {
				t.Fatalf("uncertain journal after cancellation: %+v", journal)
			}
		})
	}
}

// Completion can also arrive while the answered result is being checkpointed,
// after interaction returned but before JULIA writes its RPC response.
func TestCodexInteractionCompletionDuringCheckpoint(t *testing.T) {
	f := newCodexFixture(t)
	turns := 0
	f.turn = func(f *codexFixture) {
		turns++
		if turns == 1 {
			f.push(map[string]any{"id": "rpc-tool", "method": "item/tool/call", "params": map[string]any{"threadId": "thread-1", "turnId": "turn-1", "callId": "call", "tool": core.QuestionTool, "arguments": map[string]any{"question": "Choose?"}}})
		} else {
			f.finish("completed")
		}
	}
	ask, triage, requests := ui.NewBridge()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	checkpointed := false
	backend := &Codex{start: f.start, Checkpoint: func(session *core.Session) error {
		if !checkpointed && len(session.Messages) > 0 && session.Messages[len(session.Messages)-1].Role == "tool" {
			checkpointed = true
			f.finish("completed")
		}
		return nil
	}}
	finished := make(chan error, 1)
	go func() {
		finished <- backend.Run(ctx, &core.Session{Directory: t.TempDir()}, core.Prompt{Text: "Task"}, tools.WithInteractions(&codexTestExecutor{}, ask, triage), nil)
	}()
	select {
	case request := <-requests:
		request.Reply <- core.Answer{Text: "yes"}
	case <-ctx.Done():
		t.Fatal("no card")
	}
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("provider did not finish")
	}
	if !checkpointed || turns != 2 {
		t.Fatalf("checkpointed=%t turns=%d", checkpointed, turns)
	}
}

func TestCodexUncertainContinuationIsNotReplayed(t *testing.T) {
	f := newCodexFixture(t)
	f.turn = func(f *codexFixture) {
		f.push(map[string]any{"id": "rpc-tool", "method": "item/tool/call", "params": map[string]any{"threadId": "thread-1", "turnId": "turn-1", "callId": "call", "tool": core.QuestionTool, "arguments": map[string]any{"question": "Choose?"}}})
		f.finish("completed")
	}
	starts := 0
	start := func(ctx context.Context, binary, dir string) (*codexClient, error) {
		client, err := f.start(ctx, binary, dir)
		if err != nil {
			return nil, err
		}
		underlying := client.input
		client.input = codexTestWriter(func(p []byte) (int, error) {
			var message map[string]json.RawMessage
			_ = json.Unmarshal(p, &message)
			if string(message["method"]) == `"turn/start"` {
				starts++
				if starts == 2 {
					return 0, io.ErrClosedPipe
				}
			}
			return underlying.Write(p)
		})
		return client, nil
	}
	ask, triage, requests := ui.NewBridge()
	session := &core.Session{Directory: t.TempDir()}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		finished <- (&Codex{start: start}).Run(ctx, session, core.Prompt{Text: "Task"}, tools.WithInteractions(&codexTestExecutor{}, ask, triage), nil)
	}()
	select {
	case request := <-requests:
		request.Reply <- core.Answer{Text: "yes"}
	case <-ctx.Done():
		t.Fatal("no card")
	}
	select {
	case err := <-finished:
		if err == nil || !strings.Contains(err.Error(), "outcome uncertain") {
			t.Fatalf("got %v", err)
		}
	case <-ctx.Done():
		t.Fatal("provider did not finish")
	}
	journal, err := loadCodexJournal(session.State)
	if err != nil || journal.PendingContinuation != "thread-1" || journal.Calls["call"].Result == nil {
		t.Fatalf("journal=%+v err=%v", journal, err)
	}
	if err := (&Codex{start: start}).Run(context.Background(), session, core.Prompt{Text: "Continue"}, nil, nil); err == nil || !strings.Contains(err.Error(), "uncertain outcome") {
		t.Fatalf("unsafe resume: %v", err)
	}
	if starts != 2 {
		t.Fatalf("continuation replayed: %d starts", starts)
	}
}

func TestCodexDoesNotRepeatContinuationForReplayedInteraction(t *testing.T) {
	f := newCodexFixture(t)
	turns := 0
	f.turn = func(f *codexFixture) {
		turns++
		f.push(map[string]any{"id": "rpc-tool", "method": "item/tool/call", "params": map[string]any{"threadId": "thread-1", "turnId": "turn-1", "callId": "call", "tool": core.QuestionTool, "arguments": map[string]any{"question": "Choose?"}}})
		f.finish("completed")
	}
	ask, triage, requests := ui.NewBridge()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		finished <- (&Codex{start: f.start}).Run(ctx, &core.Session{Directory: t.TempDir()}, core.Prompt{Text: "Task"}, tools.WithInteractions(&codexTestExecutor{}, ask, triage), nil)
	}()
	select {
	case request := <-requests:
		request.Reply <- core.Answer{Text: "yes"}
	case <-ctx.Done():
		t.Fatal("no card")
	}
	select {
	case err := <-finished:
		if err == nil || !strings.Contains(err.Error(), "without repeating") {
			t.Fatalf("got %v", err)
		}
	case <-requests:
		t.Fatal("replayed question opened a second card")
	case <-ctx.Done():
		t.Fatal("provider did not finish")
	}
	if turns != 2 {
		t.Fatalf("got %d turns", turns)
	}
}

// A final answer may arrive while the card is open or during its checkpoint,
// even though turn/completed waits until after the tool response is sent.
func TestCodexInteractionFinalBeforeResponse(t *testing.T) {
	for _, name := range []string{core.QuestionTool, core.ReviewTool} {
		for _, timing := range []string{"open", "open-skipped", "open-commentary", "checkpoint", "commentary", "after"} {
			t.Run(name+"/"+timing, func(t *testing.T) {
				args := json.RawMessage(`{"question":"Choose?"}`)
				answer := core.Answer{Text: "yes"}
				if name == core.ReviewTool {
					args = json.RawMessage(`{"findings":[{"severity":"P1","title":"Bug"}]}`)
					answer = core.Answer{Triage: []core.Triage{{Decision: core.TriageFix}}}
				}
				if timing == "open-skipped" {
					answer = core.Answer{Skipped: true}
				}
				f := newCodexFixture(t)
				turns := 0
				pushMessage := func(phase, text string) {
					f.push(map[string]any{"method": "item/completed", "params": map[string]any{"threadId": "thread-1", "turnId": "turn-1", "item": map[string]any{"id": "a", "type": "agentMessage", "phase": phase, "text": text}}})
				}
				pushDelta := func(text string) {
					f.push(map[string]any{"method": "item/agentMessage/delta", "params": map[string]any{"threadId": "thread-1", "turnId": "turn-1", "itemId": "a", "delta": text}})
				}
				f.turn = func(f *codexFixture) {
					turns++
					if turns > 1 {
						pushMessage("final_answer", "Acting on your answer.")
						f.finish("completed")
						return
					}
					f.push(map[string]any{"id": "rpc-tool", "method": "item/tool/call", "params": map[string]any{"threadId": "thread-1", "turnId": "turn-1", "callId": "call", "tool": name, "arguments": args}})
					if timing == "open-commentary" {
						f.push(map[string]any{"method": "item/agentMessage/delta", "params": map[string]any{"threadId": "thread-1", "turnId": "turn-1", "itemId": "commentary", "delta": "Considering."}})
						f.push(map[string]any{"method": "item/completed", "params": map[string]any{"threadId": "thread-1", "turnId": "turn-1", "item": map[string]any{"id": "commentary", "type": "agentMessage", "phase": "commentary", "text": "Considering."}}})
					}
					if timing == "open" || timing == "open-skipped" || timing == "open-commentary" {
						pushMessage("final_answer", "Still waiting.")
					} else if timing == "commentary" {
						pushMessage("commentary", "Considering.")
					}
				}
				start := func(ctx context.Context, binary, dir string) (*codexClient, error) {
					client, err := f.start(ctx, binary, dir)
					if err != nil {
						return nil, err
					}
					underlying := client.input
					client.input = codexTestWriter(func(p []byte) (int, error) {
						var message map[string]json.RawMessage
						_ = json.Unmarshal(p, &message)
						n, err := underlying.Write(p)
						if string(message["id"]) == `"rpc-tool"` && len(message["result"]) > 0 {
							if timing == "after" || timing == "commentary" {
								pushMessage("final_answer", "Acting on your answer.")
							}
							f.finish("completed")
						}
						return n, err
					})
					return client, nil
				}
				checkpointed := false
				backend := &Codex{start: start, Checkpoint: func(session *core.Session) error {
					if timing == "checkpoint" && !checkpointed && len(session.Messages) > 0 && session.Messages[len(session.Messages)-1].Role == "tool" {
						checkpointed = true
						f.push(map[string]any{"method": "item/agentMessage/delta", "params": map[string]any{"threadId": "thread-1", "turnId": "turn-1", "itemId": "commentary", "delta": "Considering."}})
						f.push(map[string]any{"method": "item/completed", "params": map[string]any{"threadId": "thread-1", "turnId": "turn-1", "item": map[string]any{"id": "commentary", "type": "agentMessage", "phase": "commentary", "text": "Considering."}}})
						pushDelta("Still ")
						pushDelta("waiting.")
						pushMessage("final_answer", "Still waiting.")
					}
					return nil
				}}
				ask, triage, requests := ui.NewBridge()
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				finished := make(chan error, 1)
				var output strings.Builder
				session := &core.Session{Directory: t.TempDir()}
				go func() {
					finished <- backend.Run(ctx, session, core.Prompt{Text: "Task"}, tools.WithInteractions(&codexTestExecutor{}, ask, triage), func(e core.Event) {
						if e.Kind == "text" {
							output.WriteString(e.Text)
						}
					})
				}()
				select {
				case request := <-requests:
					request.Reply <- answer
				case <-ctx.Done():
					t.Fatal("no card")
				}
				select {
				case err := <-finished:
					if err != nil {
						t.Fatal(err)
					}
				case <-ctx.Done():
					t.Fatal("provider did not finish")
				}
				wantTurns, wantText := 1, "Acting on your answer."
				if timing == "open" || timing == "open-skipped" || timing == "open-commentary" || timing == "checkpoint" {
					wantTurns = 2
				}
				if timing == "commentary" || timing == "checkpoint" || timing == "open-commentary" {
					wantText = "Considering.Acting on your answer."
				}
				if turns != wantTurns || output.String() != wantText {
					t.Fatalf("turns=%d output=%q, want %d and %q", turns, output.String(), wantTurns, wantText)
				}
				for _, message := range session.Messages {
					if message.Role == "assistant" && strings.Contains(message.Content, "Still waiting.") {
						t.Fatalf("stale final answer in transcript: %+v", message)
					}
				}
			})
		}
	}
}

// A pre-response delta whose item completes after the response is not stale.
// Text from other items must not overtake it while its phase is unknown.
func TestCodexInteractionBufferedTextOrder(t *testing.T) {
	for _, name := range []string{core.QuestionTool, core.ReviewTool} {
		t.Run(name, func(t *testing.T) {
			args := json.RawMessage(`{"question":"Choose?"}`)
			answer := core.Answer{Text: "yes"}
			if name == core.ReviewTool {
				args = json.RawMessage(`{"findings":[{"severity":"P1","title":"Bug"}]}`)
				answer = core.Answer{Triage: []core.Triage{{Decision: core.TriageFix}}}
			}
			f := newCodexFixture(t)
			f.turn = func(f *codexFixture) {
				f.push(map[string]any{"id": "rpc-tool", "method": "item/tool/call", "params": map[string]any{"threadId": "thread-1", "turnId": "turn-1", "callId": "call", "tool": name, "arguments": args}})
			}
			delta := func(id, text string) {
				f.push(map[string]any{"method": "item/agentMessage/delta", "params": map[string]any{"threadId": "thread-1", "turnId": "turn-1", "itemId": id, "delta": text}})
			}
			complete := func(id, phase, text string) {
				f.push(map[string]any{"method": "item/completed", "params": map[string]any{"threadId": "thread-1", "turnId": "turn-1", "item": map[string]any{"id": id, "type": "agentMessage", "phase": phase, "text": text}}})
			}
			start := func(ctx context.Context, binary, dir string) (*codexClient, error) {
				client, err := f.start(ctx, binary, dir)
				if err != nil {
					return nil, err
				}
				underlying := client.input
				client.input = codexTestWriter(func(p []byte) (int, error) {
					var message map[string]json.RawMessage
					_ = json.Unmarshal(p, &message)
					n, err := underlying.Write(p)
					if string(message["id"]) == `"rpc-tool"` && len(message["result"]) > 0 {
						delta("first", "B")
						delta("second", "C")
						complete("second", "commentary", "C")
						complete("first", "final_answer", "AB")
						f.finish("completed")
					}
					return n, err
				})
				return client, nil
			}
			checkpointed := false
			backend := &Codex{start: start, Checkpoint: func(session *core.Session) error {
				if !checkpointed && len(session.Messages) > 0 && session.Messages[len(session.Messages)-1].Role == "tool" {
					checkpointed = true
					delta("first", "A")
				}
				return nil
			}}
			ask, triage, requests := ui.NewBridge()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			session := &core.Session{Directory: t.TempDir()}
			finished := make(chan error, 1)
			var output strings.Builder
			go func() {
				finished <- backend.Run(ctx, session, core.Prompt{Text: "Task"}, tools.WithInteractions(&codexTestExecutor{}, ask, triage), func(e core.Event) {
					if e.Kind == "text" {
						output.WriteString(e.Text)
					}
				})
			}()
			select {
			case request := <-requests:
				request.Reply <- answer
			case <-ctx.Done():
				t.Fatal("no card")
			}
			select {
			case err := <-finished:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal("provider did not finish")
			}
			if !checkpointed || output.String() != "ABC" {
				t.Fatalf("checkpointed=%t output=%q", checkpointed, output.String())
			}
			last := session.Messages[len(session.Messages)-1]
			if last.Role != "assistant" || last.Content != "ABC" {
				t.Fatalf("wrong transcript: %+v", last)
			}
		})
	}
}

type codexFailedInteractionExecutor struct{ name string }

func (e codexFailedInteractionExecutor) Definitions() []core.ToolDefinition {
	return []core.ToolDefinition{{Name: e.name}}
}

func (e codexFailedInteractionExecutor) Execute(context.Context, core.ToolCall) core.ToolResult {
	return core.ToolResult{Content: "interaction failed", IsError: true}
}

func TestCodexFailedInteractionFinalDoesNotContinue(t *testing.T) {
	for _, name := range []string{core.QuestionTool, core.ReviewTool} {
		t.Run(name, func(t *testing.T) {
			f := newCodexFixture(t)
			turns := 0
			f.turn = func(f *codexFixture) {
				turns++
				f.push(map[string]any{"id": "rpc-tool", "method": "item/tool/call", "params": map[string]any{"threadId": "thread-1", "turnId": "turn-1", "callId": "call", "tool": name, "arguments": map[string]any{}}})
				f.push(map[string]any{"method": "item/completed", "params": map[string]any{"threadId": "thread-1", "turnId": "turn-1", "item": map[string]any{"id": "a", "type": "agentMessage", "phase": "final_answer", "text": "Waiting."}}})
			}
			start := func(ctx context.Context, binary, dir string) (*codexClient, error) {
				client, err := f.start(ctx, binary, dir)
				if err != nil {
					return nil, err
				}
				underlying := client.input
				client.input = codexTestWriter(func(p []byte) (int, error) {
					var message map[string]json.RawMessage
					_ = json.Unmarshal(p, &message)
					n, err := underlying.Write(p)
					if string(message["id"]) == `"rpc-tool"` && len(message["result"]) > 0 {
						f.finish("completed")
					}
					return n, err
				})
				return client, nil
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var output strings.Builder
			err := (&Codex{start: start}).Run(ctx, &core.Session{Directory: t.TempDir()}, core.Prompt{Text: "Task"}, codexFailedInteractionExecutor{name}, func(e core.Event) {
				if e.Kind == "text" {
					output.WriteString(e.Text)
				}
			})
			if err != nil || turns != 1 || output.String() != "Waiting." {
				t.Fatalf("err=%v turns=%d output=%q", err, turns, output.String())
			}
		})
	}
}

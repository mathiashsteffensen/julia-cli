package provider

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"julia/internal/core"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openaicompat"
)

func TestCodexCompleteIsIsolatedAndReturnsUsage(t *testing.T) {
	f := newCodexFixture(t)
	f.turn = func(f *codexFixture) {
		f.pushUsage(codexCounts{input: 300, cached: 100, output: 40}, codexCounts{input: 300, cached: 100, output: 40})
		f.push(map[string]any{"method": "item/completed", "params": map[string]any{"threadId": "thread-1", "turnId": "turn-1", "item": map[string]any{"id": "final", "type": "agentMessage", "text": `{"operations":[],"helped":[],"misled":[]}`}}})
		f.finish("completed")
	}
	// No effort configured: the isolated call runs low.
	backend := &Codex{start: f.start, Model: "gpt-6-astra", Checkpoint: func(*core.Session) error { t.Fatal("isolated call persisted a session"); return nil }}
	text, spent, err := backend.Complete(context.Background(), "REFLECTION_INSTRUCTIONS", "the turn as JSON", map[string]any{"type": "object"})
	if err != nil {
		t.Fatal(err)
	}
	if text != `{"operations":[],"helped":[],"misled":[]}` || spent.Output != 40 || spent.Prompt() != 300 || spent.CacheRead != 100 {
		t.Fatalf("text %q usage %+v", text, spent)
	}
	var started, turned bool
	for _, request := range f.requests {
		var params map[string]any
		if err := json.Unmarshal(request["params"], &params); err != nil {
			continue
		}
		switch string(request["method"]) {
		case `"thread/start"`:
			started = true
			if params["ephemeral"] != true || params["baseInstructions"] != "REFLECTION_INSTRUCTIONS" || params["developerInstructions"] != codexCompleteDeveloperInstructions || params["model"] != "gpt-6-astra" || params["sandbox"] != "read-only" || len(params["dynamicTools"].([]any)) != 0 {
				t.Fatalf("thread not isolated: %+v", params)
			}
		case `"turn/start"`:
			turned = true
			if params["effort"] != "low" || params["model"] != "gpt-6-astra" || params["outputSchema"] == nil {
				t.Fatalf("turn settings: %+v", params)
			}
		case `"thread/resume"`:
			t.Fatal("isolated call resumed a thread")
		}
	}
	if !started || !turned {
		t.Fatal("no thread or turn started")
	}
}

func TestCodexCompleteFailsClosedOnToolAttempt(t *testing.T) {
	f := newCodexFixture(t)
	f.turn = func(f *codexFixture) {
		f.push(map[string]any{"id": 9, "method": "item/tool/call", "params": map[string]any{"threadId": "thread-1", "turnId": "turn-1", "callId": "call", "tool": "Bash", "arguments": map[string]any{}}})
		f.finish("completed")
	}
	if _, _, err := (&Codex{start: f.start}).Complete(context.Background(), "i", "p", nil); err == nil || !strings.Contains(err.Error(), "tool call") {
		t.Fatalf("tool attempt not refused: %v", err)
	}
	f = newCodexFixture(t)
	f.turn = func(f *codexFixture) { f.finish("completed") }
	if _, _, err := (&Codex{start: f.start}).Complete(context.Background(), "i", "p", nil); err == nil || !strings.Contains(err.Error(), "no answer text") {
		t.Fatalf("empty answer accepted: %v", err)
	}
}

func TestCodexReviewStillPinsItsModelAndInstructions(t *testing.T) {
	f := newCodexFixture(t)
	f.turn = func(f *codexFixture) {
		f.push(map[string]any{"method": "item/completed", "params": map[string]any{"threadId": "thread-1", "turnId": "turn-1", "item": map[string]any{"id": "final", "type": "agentMessage", "text": `{"allow":true,"reason":"ok"}`}}})
		f.finish("completed")
	}
	backend := &Codex{start: f.start, Model: "gpt-6-astra", Reasoning: "xhigh"}
	if _, err := backend.Review(context.Background(), core.Approval{Call: core.ToolCall{Name: "Bash", Arguments: json.RawMessage(`{"command":"ls"}`)}, Task: "[]"}); err != nil {
		t.Fatal(err)
	}
	for _, request := range f.requests {
		var params map[string]any
		json.Unmarshal(request["params"], &params)
		switch string(request["method"]) {
		case `"thread/start"`:
			if params["model"] != CodexReviewerModel || params["baseInstructions"] != codexReviewInstructions || params["developerInstructions"] != codexReviewDeveloperInstructions {
				t.Fatalf("review settings drifted: %+v", params)
			}
		case `"turn/start"`:
			if params["effort"] != CodexReviewerReasoning || params["model"] != CodexReviewerModel {
				t.Fatalf("review turn drifted: %+v", params)
			}
		}
	}
}

func TestAPICompleteCarriesSchemaInInstructionsAndReturnsUsage(t *testing.T) {
	model := &scriptedAPIModel{generated: &fantasy.Response{FinishReason: fantasy.FinishReasonStop, Content: fantasy.ResponseContent{fantasy.TextContent{Text: `{"operations":[]}`}}, Usage: fantasy.Usage{InputTokens: 120, OutputTokens: 12}}}
	backend := &API{Model: model, Provider: "openai"}
	text, spent, err := backend.Complete(context.Background(), "REFLECTION_INSTRUCTIONS", "the turn as JSON", map[string]any{"type": "object", "required": []string{"operations"}})
	if err != nil {
		t.Fatal(err)
	}
	if text != `{"operations":[]}` || spent.Output != 12 || spent.Prompt() != 120 {
		t.Fatalf("text %q usage %+v", text, spent)
	}
	call := model.generateCalls[0]
	if len(call.Prompt) != 2 || call.Prompt[0].Role != fantasy.MessageRoleSystem || call.Prompt[1].Role != fantasy.MessageRoleUser {
		t.Fatalf("prompt shape %+v", call.Prompt)
	}
	system := call.Prompt[0].Content[0].(fantasy.TextPart).Text
	if !strings.HasPrefix(system, "REFLECTION_INSTRUCTIONS") || !strings.Contains(system, `"required":["operations"]`) || call.Prompt[1].Content[0].(fantasy.TextPart).Text != "the turn as JSON" {
		t.Fatalf("instructions %q", system)
	}
	if call.ToolChoice == nil || *call.ToolChoice != fantasy.ToolChoiceNone || call.MaxOutputTokens == nil || *call.MaxOutputTokens != completionOutputTokens {
		t.Fatal("isolated call gained tools or an unbounded answer")
	}
	// Without a schema the instructions go as they are.
	if _, _, err := backend.Complete(context.Background(), "PLAIN", "p", nil); err != nil {
		t.Fatal(err)
	}
	if plain := model.generateCalls[1].Prompt[0].Content[0].(fantasy.TextPart).Text; plain != "PLAIN" {
		t.Fatalf("instructions changed without a schema: %q", plain)
	}
	// An answer cut short is no answer; its tokens are still reported.
	model.generated = &fantasy.Response{FinishReason: fantasy.FinishReasonLength, Content: fantasy.ResponseContent{fantasy.TextContent{Text: `{"oper`}}, Usage: fantasy.Usage{InputTokens: 50, OutputTokens: 4096}}
	if _, spent, err := backend.Complete(context.Background(), "i", "p", nil); err == nil || spent.Output != 4096 {
		t.Fatalf("truncated answer: %v %+v", err, spent)
	}
}

func TestAPICompleteSendsItsEffortAndExplainsLength(t *testing.T) {
	model := &scriptedAPIModel{generated: &fantasy.Response{FinishReason: fantasy.FinishReasonStop, Content: fantasy.ResponseContent{fantasy.TextContent{Text: "{}"}}}}
	backend := &API{Model: model, Provider: "compatible", OpenRouter: true, Reasoning: "low"}
	if _, _, err := backend.Complete(context.Background(), "i", "p", nil); err != nil {
		t.Fatal(err)
	}
	options, ok := model.generateCalls[0].ProviderOptions[openaicompat.Name].(*openaicompat.ProviderOptions)
	if !ok || options.ReasoningEffort == nil || string(*options.ReasoningEffort) != "low" || options.ExtraBody["tool_choice"] != "none" {
		t.Fatalf("options %+v", model.generateCalls[0].ProviderOptions)
	}
	model.generated = &fantasy.Response{FinishReason: fantasy.FinishReasonLength, Content: fantasy.ResponseContent{fantasy.TextContent{Text: "{"}}}
	if _, _, err := backend.Complete(context.Background(), "i", "p", nil); err == nil || !strings.Contains(err.Error(), "ran past 8192 tokens") {
		t.Fatalf("length: %v", err)
	}
}

func TestCodexCompleteUsesItsConfiguredEffort(t *testing.T) {
	f := newCodexFixture(t)
	f.turn = func(f *codexFixture) {
		f.push(map[string]any{"method": "item/completed", "params": map[string]any{"threadId": "thread-1", "turnId": "turn-1", "item": map[string]any{"id": "final", "type": "agentMessage", "text": "{}"}}})
		f.finish("completed")
	}
	if _, _, err := (&Codex{start: f.start, Reasoning: "medium"}).Complete(context.Background(), "i", "p", nil); err != nil {
		t.Fatal(err)
	}
	for _, request := range f.requests {
		if string(request["method"]) == `"turn/start"` {
			var params map[string]any
			json.Unmarshal(request["params"], &params)
			if params["effort"] != "medium" {
				t.Fatalf("effort %v", params["effort"])
			}
		}
	}
}

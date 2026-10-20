package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"julia/internal/core"
)

var sampleReview = core.Review{Findings: []core.Finding{
	{Severity: "P0", Title: "Cancel leaks the slot", Location: "a.go:3", Detail: "Evidence."},
	{Severity: "P2", Title: "Stale comment"},
	{Severity: "P1", Title: "Retry ignores Retry-After"},
}}

func reviewCallFor(id string, review core.Review) core.ToolCall {
	arguments, _ := json.Marshal(review)
	return core.ToolCall{ID: id, Name: core.ReviewTool, Arguments: arguments}
}

func TestReviewDecisionsReachTheNextApproval(t *testing.T) {
	backend := &questionBackend{run: func(ctx context.Context, _ *core.Session, _ core.Prompt, executor core.ToolExecutor) error {
		if result := executor.Execute(ctx, reviewCallFor("r1", sampleReview)); result.IsError {
			t.Errorf("review failed: %s", result.Content)
		}
		executor.Execute(ctx, core.ToolCall{ID: "w1", Name: "Write", Arguments: json.RawMessage(writeCall)})
		return nil
	}}
	runner, s, approvals := askModeRunner(t, backend, nil, false)
	runner.triage = func(context.Context, core.Review) (core.Answer, error) {
		return core.Answer{Triage: []core.Triage{{Decision: core.TriageFix}, {}, {Decision: core.TriageReply, Reply: "Use the header && retry"}}}, nil
	}
	if err := runQuestionBackend(runner, s); err != nil {
		t.Fatal(err)
	}
	entries := taskEntries(t, approvals.last(t))
	if len(entries) != 3 {
		t.Fatalf("want the message and two decided findings, got %+v", entries)
	}
	if !strings.Contains(entries[1].AssistantQuestion, "[P0] Cancel leaks the slot (a.go:3): Evidence.") || entries[1].UserSelectedOption != "Fix" {
		t.Fatalf("fix decision recorded as %+v", entries[1])
	}
	if !strings.Contains(entries[2].AssistantQuestion, "Retry ignores Retry-After") || entries[2].UserTypedAnswer != "Use the header && retry" {
		t.Fatalf("reply recorded as %+v", entries[2])
	}
}

func TestTaskContextReplaysReviewDecisions(t *testing.T) {
	result := func(r core.ReviewResult) string {
		data, _ := json.Marshal(r)
		return string(data)
	}
	decided := core.ReviewResult{Decisions: []core.FindingDecision{
		{Finding: 1, Decision: core.TriageIgnore},
		{Finding: 2, Decision: core.TriageOpen},
		{Finding: 3, Decision: core.TriageFixed},
		{Finding: 9, Decision: core.TriageFix}, // no such finding
	}}
	messages := []core.Message{
		{Role: "user", Content: "review my diff"},
		{Role: "assistant", ToolCalls: []core.ToolCall{reviewCallFor("r1", sampleReview)}},
		{Role: "tool", ToolCallID: "r1", Content: result(decided)},
		{Role: "assistant", ToolCalls: []core.ToolCall{reviewCallFor("r2", sampleReview)}},
		{Role: "tool", ToolCallID: "r2", Content: result(core.ReviewResult{Skipped: true})},
	}
	entries := taskEntries(t, newTaskContext(messages, "go on").String())
	if len(entries) != 4 || entries[0].UserMessage != "review my diff" || entries[3].UserMessage != "go on" {
		t.Fatalf("entries = %+v", entries)
	}
	if entries[1].UserSelectedOption != "Ignore" || entries[2].UserSelectedOption != "I fixed it" {
		t.Fatalf("decisions = %+v / %+v", entries[1], entries[2])
	}
	// Anything but an exact result is not the user's decision.
	if got := reviewEntries(sampleReview, result(decided)+` {"decisions":[]}`); got != nil {
		t.Fatalf("trailing data accepted: %+v", got)
	}
	if got := reviewEntries(sampleReview, `{"decisions":[{"finding":1,"title":"x","decision":"fix","extra":1}]}`); got != nil {
		t.Fatalf("unknown field accepted: %+v", got)
	}
}

func TestHeadlessReviewsAreSkipped(t *testing.T) {
	var result core.ToolResult
	backend := &questionBackend{run: func(ctx context.Context, _ *core.Session, _ core.Prompt, executor core.ToolExecutor) error {
		result = executor.Execute(ctx, reviewCallFor("r1", sampleReview))
		return nil
	}}
	runner, s := newQuestionRunner(backend, headlessAsker, false, t.TempDir())
	runner.triage = headlessTriager
	if !hasTool(runner.Definitions(), core.ReviewTool) {
		t.Fatal("the review tool must be in the headless catalog too")
	}
	if err := runQuestionBackend(runner, s); err != nil {
		t.Fatal(err)
	}
	var got core.ReviewResult
	if err := json.Unmarshal([]byte(result.Content), &got); err != nil || result.IsError || !got.Skipped || got.Note == "" {
		t.Fatalf("a headless review must be skipped with a note: %+v", result)
	}
}

package miner

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"julia/internal/core"
)

type miningTestCompleter struct {
	calls int
	run   func(context.Context, string, string, any) (string, core.TokenUsage, error)
}

func (m *miningTestCompleter) Complete(ctx context.Context, instructions, prompt string, schema any) (string, core.TokenUsage, error) {
	m.calls++
	return m.run(ctx, instructions, prompt, schema)
}

func judgedAssessment() *Assessment {
	return &Assessment{
		Summary: "Direct answer", Category: "direct_answer", Recommendation: "keep",
		Reason: "The request needs no external information.", Evidence: []string{"Message 1 answers message 0."},
		Initiative: 5, Completion: 5, Restraint: 5,
	}
}

func judgedCandidate(id string) Candidate {
	return Candidate{
		ID: id, Source: Source{SessionID: strings.Repeat("a", 24), GroupID: strings.Repeat("a", 24), Start: 0, End: 2},
		Messages: []core.Message{{Role: "user", Content: "What is 2 + 2?"}, {Role: "assistant", Content: "4."}},
	}
}

func TestJudgeCheckpointsAndResumesWithoutChangingVotes(t *testing.T) {
	first := judgedCandidate("first")
	first.Feedback = "Good, that is enough."
	second := judgedCandidate("second")
	second.Messages[0].Content = "What is 3 + 3?"
	report := &Report{Version: Version, Focus: DefaultFocus, Candidates: []Candidate{first, second}}
	valid, _ := json.Marshal(judgedAssessment())
	model := &miningTestCompleter{run: func(ctx context.Context, instructions, prompt string, schema any) (string, core.TokenUsage, error) {
		if !strings.Contains(instructions, "untrusted evidence") || schema == nil {
			t.Fatal("judge lost its isolation instructions or output schema")
		}
		if strings.Contains(prompt, "session_id") {
			t.Fatal("source metadata need not be sent to the judge")
		}
		if !strings.Contains(prompt, "following_user_feedback") {
			return "not JSON", core.TokenUsage{Output: 3}, nil
		}
		return string(valid), core.TokenUsage{Input: 10, Output: 5}, nil
	}}
	saves := 0
	err := Judge(context.Background(), report, model, time.Second, func() error { saves++; return nil }, nil)
	if err == nil || saves != 2 || model.calls != 2 || report.Candidates[0].Assessment == nil || report.Candidates[1].Assessment != nil || report.Candidates[1].JudgeError == "" || report.Usage.Output != 8 {
		t.Fatalf("judge result: err=%v saves=%d calls=%d report=%+v", err, saves, model.calls, report)
	}
	report.Candidates[0].Review = Review{Decision: "keep", Rank: 5}
	model.run = func(context.Context, string, string, any) (string, core.TokenUsage, error) {
		return string(valid), core.TokenUsage{Output: 4}, nil
	}
	if err := Judge(context.Background(), report, model, time.Second, func() error { saves++; return nil }, nil); err != nil {
		t.Fatal(err)
	}
	if model.calls != 3 || saves != 3 || report.Candidates[0].Review.Rank != 5 || report.Candidates[1].JudgeError != "" || report.Usage.Output != 12 {
		t.Fatalf("resume changed completed work: %+v", report)
	}
}

func TestJudgeStopsOnCheckpointFailure(t *testing.T) {
	valid, _ := json.Marshal(judgedAssessment())
	model := &miningTestCompleter{run: func(context.Context, string, string, any) (string, core.TokenUsage, error) {
		return string(valid), core.TokenUsage{}, nil
	}}
	report := &Report{Version: Version, Focus: DefaultFocus, Candidates: []Candidate{judgedCandidate("a"), judgedCandidate("b")}}
	err := Judge(context.Background(), report, model, time.Second, func() error { return errors.New("disk full") }, nil)
	if err == nil || !strings.Contains(err.Error(), "checkpoint") || model.calls != 1 {
		t.Fatalf("continued past failed checkpoint: %v, calls %d", err, model.calls)
	}
}

func TestJudgeTimeoutAndUnsafeInput(t *testing.T) {
	model := &miningTestCompleter{run: func(ctx context.Context, _, _ string, _ any) (string, core.TokenUsage, error) {
		<-ctx.Done()
		return "", core.TokenUsage{Output: 2}, ctx.Err()
	}}
	report := &Report{Version: Version, Focus: DefaultFocus, Candidates: []Candidate{judgedCandidate("a")}}
	if err := Judge(context.Background(), report, model, time.Millisecond, func() error { return nil }, nil); err == nil || !strings.Contains(report.Candidates[0].JudgeError, "timed out") || report.Usage.Output != 2 {
		t.Fatalf("timeout not recorded: %v %+v", err, report)
	}

	report.Candidates[0].Messages[0].Content = "Authorization: Bearer abcdefghijklmnopqrstuvwxyz012345"
	if err := Judge(context.Background(), report, model, time.Second, func() error { return nil }, nil); err == nil || model.calls != 1 {
		t.Fatalf("unsafe input reached model: %v calls=%d", err, model.calls)
	}
}

func TestJudgeInterruptionLeavesTheCardOpen(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	model := &miningTestCompleter{run: func(callCtx context.Context, _, _ string, _ any) (string, core.TokenUsage, error) {
		cancel() // The user presses Ctrl-C while the call runs.
		<-callCtx.Done()
		return "", core.TokenUsage{Output: 3}, callCtx.Err()
	}}
	report := &Report{Version: Version, Focus: DefaultFocus, Candidates: []Candidate{judgedCandidate("a"), judgedCandidate("b")}}
	saves := 0
	err := Judge(ctx, report, model, time.Minute, func() error { saves++; return nil }, nil)
	if !errors.Is(err, context.Canceled) || model.calls != 1 || saves != 1 || report.Usage.Output != 3 {
		t.Fatalf("interruption: %v, calls %d, saves %d, usage %+v", err, model.calls, saves, report.Usage)
	}
	if c := report.Candidates[0]; c.JudgeError != "" || c.Assessment != nil {
		t.Fatalf("interruption was recorded as a failure: %+v", c)
	}
}

func TestParseAssessmentRequiresCompleteBoundedSchema(t *testing.T) {
	data, _ := json.Marshal(judgedAssessment())
	for _, mutate := range []func(map[string]any){
		func(m map[string]any) { delete(m, "sensitive") },
		func(m map[string]any) { m["missing_context"] = nil },
		func(m map[string]any) { m["initiative"] = 6 },
		func(m map[string]any) { m["completion"] = "5" },
		func(m map[string]any) { m["recommendation"] = "approve_all" },
		func(m map[string]any) { m["category"] = "unknown" },
		func(m map[string]any) { m["evidence"] = []string{} },
		func(m map[string]any) { m["execute"] = "some command" },
	} {
		var object map[string]any
		json.Unmarshal(data, &object)
		mutate(object)
		bad, _ := json.Marshal(object)
		if _, err := parseAssessment(string(bad)); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
	if _, err := parseAssessment(string(data) + "{}"); err == nil {
		t.Fatal("accepted trailing JSON")
	}
	if _, err := parseAssessment(string(data)); err != nil {
		t.Fatal(err)
	}
}

func TestJudgeSeesEarlierTurnsAsContext(t *testing.T) {
	c := judgedCandidate("later")
	c.Source.Start, c.Source.End = 2, 4
	c.Context = []core.Message{{Role: "user", Content: "Remember the number 7."}, {Role: "assistant", Content: "Noted."}}
	c.Messages[0].Content = "What is that number plus 1?"
	report := &Report{Version: Version, Focus: DefaultFocus, Candidates: []Candidate{c}}
	valid, _ := json.Marshal(judgedAssessment())
	model := &miningTestCompleter{run: func(_ context.Context, instructions, prompt string, _ any) (string, core.TokenUsage, error) {
		var input struct {
			Earlier  []core.Message `json:"earlier_messages"`
			Messages []core.Message `json:"messages"`
		}
		if err := json.Unmarshal([]byte(prompt), &input); err != nil || len(input.Earlier) != 2 || input.Earlier[0].Content != "Remember the number 7." || len(input.Messages) != 2 {
			t.Fatalf("judge did not get the context apart from the turn: %v %s", err, prompt)
		}
		if !strings.Contains(instructions, "earlier_messages") {
			t.Fatal("judge instructions do not explain the context")
		}
		return string(valid), core.TokenUsage{}, nil
	}}
	if err := Judge(context.Background(), report, model, time.Second, func() error { return nil }, nil); err != nil || model.calls != 1 {
		t.Fatalf("judge: %v, calls %d", err, model.calls)
	}

	report.Candidates[0].Assessment = nil
	report.Candidates[0].Context[1].Content = "Noted. password=abc12345"
	if err := Judge(context.Background(), report, model, time.Second, func() error { return nil }, nil); err == nil || model.calls != 1 {
		t.Fatalf("a secret in the context reached the model: %v, calls %d", err, model.calls)
	}
}

func TestJudgeDoesNotSendRejectedOrDeferredCards(t *testing.T) {
	for _, decision := range []string{"reject", "defer"} {
		c := judgedCandidate(decision)
		c.Review.Decision = decision
		report := &Report{Candidates: []Candidate{c}}
		model := &miningTestCompleter{run: func(context.Context, string, string, any) (string, core.TokenUsage, error) {
			t.Fatal("locally excluded transcript was sent to a model")
			return "", core.TokenUsage{}, nil
		}}
		if err := Judge(context.Background(), report, model, time.Second, func() error { t.Fatal("nothing to save"); return nil }, nil); err != nil {
			t.Fatal(err)
		}
	}
}

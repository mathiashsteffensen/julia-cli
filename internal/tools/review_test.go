package tools

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"

	"julia/internal/core"
)

// triageRecorder is a Triager that records the reviews it receives and replies with a fixed answer.
type triageRecorder struct {
	mu      sync.Mutex
	reviews []core.Review
	answer  core.Answer
}

func (r *triageRecorder) triage(_ context.Context, review core.Review) (core.Answer, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reviews = append(r.reviews, review)
	return r.answer, nil
}

func reviewCall(raw string) core.ToolCall {
	return core.ToolCall{ID: "call-r", Name: core.ReviewTool, Arguments: json.RawMessage(raw)}
}

const twoFindings = `{"summary":"Two problems.","findings":[` +
	`{"severity":"P0","title":"Cancel leaks the slot","location":"a.go:3","detail":"Evidence."},` +
	`{"severity":"p2","title":"  Stale\tcomment  ","location":"b.go:9"}]}`

func decodeReviewResult(t *testing.T, result core.ToolResult) core.ReviewResult {
	t.Helper()
	if result.IsError {
		t.Fatalf("error result: %s", result.Content)
	}
	var got core.ReviewResult
	d := json.NewDecoder(strings.NewReader(result.Content))
	d.DisallowUnknownFields()
	if err := d.Decode(&got); err != nil || d.Decode(new(any)) != io.EOF {
		t.Fatalf("content is not one ReviewResult: %q (%v)", result.Content, err)
	}
	return got
}

func TestReviewDefinitionRegisteredOnlyWithTriager(t *testing.T) {
	ask := (&askRecorder{}).ask
	if hasDefinition(WithQuestions(&stubExecutor{}, ask), core.ReviewTool) {
		t.Fatal("review tool registered without a triager")
	}
	both := WithInteractions(&stubExecutor{}, ask, (&triageRecorder{}).triage)
	if !hasDefinition(both, core.ReviewTool) || !hasDefinition(both, core.QuestionTool) {
		t.Fatalf("missing tools: %+v", both.Definitions())
	}
	reviewOnly := WithInteractions(&stubExecutor{}, nil, (&triageRecorder{}).triage)
	if hasDefinition(reviewOnly, core.QuestionTool) || !hasDefinition(reviewOnly, core.ReviewTool) {
		t.Fatalf("wrong tools: %+v", reviewOnly.Definitions())
	}
	stub := &stubExecutor{}
	if got := WithInteractions(stub, nil, nil); got != core.ToolExecutor(stub) {
		t.Fatal("no asker and no triager should leave the executor unchanged")
	}
	// A review call without a triager is the base executor's business.
	WithQuestions(stub, ask).Execute(context.Background(), reviewCall(twoFindings))
	if len(stub.calls) != 1 || stub.calls[0].Name != core.ReviewTool {
		t.Fatalf("review call was not delegated: %+v", stub.calls)
	}
}

func hasDefinition(executor core.ToolExecutor, name string) bool {
	for _, definition := range executor.Definitions() {
		if definition.Name == name {
			return true
		}
	}
	return false
}

func TestReviewMapsDecisionsWithTitlesFromTheReview(t *testing.T) {
	rec := &triageRecorder{answer: core.Answer{Triage: []core.Triage{
		{Decision: core.TriageReply, Reply: "  use the config value  "},
		{},
	}}}
	result := WithInteractions(&stubExecutor{}, nil, rec.triage).Execute(context.Background(), reviewCall(twoFindings))
	got := decodeReviewResult(t, result)
	if len(rec.reviews) != 1 {
		t.Fatalf("triager called %d times", len(rec.reviews))
	}
	review := rec.reviews[0]
	if review.Findings[1].Severity != "P2" || review.Findings[1].Title != "Stale comment" {
		t.Fatalf("finding not normalized: %+v", review.Findings[1])
	}
	want := []core.FindingDecision{
		{Finding: 1, Title: "Cancel leaks the slot", Decision: core.TriageReply, Reply: "use the config value"},
		{Finding: 2, Title: "Stale comment", Decision: core.TriageOpen},
	}
	if len(got.Decisions) != 2 || got.Decisions[0] != want[0] || got.Decisions[1] != want[1] {
		t.Fatalf("decisions = %+v, want %+v", got.Decisions, want)
	}
	if got.Skipped || !strings.Contains(got.Note, "fix: make the smallest fix") {
		t.Fatalf("missing guidance: %+v", got)
	}
}

func TestReviewSkippedTellsTheModelToChangeNothing(t *testing.T) {
	rec := &triageRecorder{answer: core.Answer{Skipped: true}}
	got := decodeReviewResult(t, WithInteractions(&stubExecutor{}, nil, rec.triage).Execute(context.Background(), reviewCall(twoFindings)))
	if !got.Skipped || len(got.Decisions) != 0 || !strings.Contains(got.Note, "Change no code") {
		t.Fatalf("skipped result = %+v", got)
	}
}

func TestReviewRejectsBadTriage(t *testing.T) {
	for name, answer := range map[string]core.Answer{
		"too few":     {Triage: []core.Triage{{Decision: core.TriageFix}}},
		"unknown":     {Triage: []core.Triage{{Decision: "merge"}, {}}},
		"empty reply": {Triage: []core.Triage{{Decision: core.TriageReply, Reply: "  "}, {}}},
	} {
		t.Run(name, func(t *testing.T) {
			rec := &triageRecorder{answer: answer}
			if result := WithInteractions(&stubExecutor{}, nil, rec.triage).Execute(context.Background(), reviewCall(twoFindings)); !result.IsError {
				t.Fatalf("bad triage accepted: %s", result.Content)
			}
		})
	}
}

func TestReviewRejectsBadArguments(t *testing.T) {
	many := make([]map[string]string, maxFindings+1)
	for i := range many {
		many[i] = map[string]string{"severity": "P1", "title": "t"}
	}
	for name, raw := range map[string]string{
		"no findings":    `{"summary":"s"}`,
		"empty list":     `{"findings":[]}`,
		"too many":       questionJSON(map[string]any{"findings": many}),
		"bad severity":   `{"findings":[{"severity":"P4","title":"t"}]}`,
		"no severity":    `{"findings":[{"title":"t"}]}`,
		"blank title":    `{"findings":[{"severity":"P1","title":" ​ "}]}`,
		"long title":     questionJSON(map[string]any{"findings": []map[string]string{{"severity": "P1", "title": strings.Repeat("a", maxFindingTitle+1)}}}),
		"long detail":    questionJSON(map[string]any{"findings": []map[string]string{{"severity": "P1", "title": "t", "detail": strings.Repeat("a", maxFindingDetail+1)}}}),
		"NUL":            `{"findings":[{"severity":"P1","title":"a\u0000b"}]}`,
		"unknown field":  `{"findings":[{"severity":"P1","title":"t","fix":"x"}]}`,
		"invalid UTF-8":  "{\"findings\":[{\"severity\":\"P1\",\"title\":\"\xff\"}]}",
		"not an object":  `[]`,
		"long summary":   questionJSON(map[string]any{"summary": strings.Repeat("s", maxSummaryBytes+1), "findings": []map[string]string{{"severity": "P1", "title": "t"}}}),
		"summary is NUL": `{"summary":"\u0000","findings":[{"severity":"P1","title":"t"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			rec := &triageRecorder{}
			if result := WithInteractions(&stubExecutor{}, nil, rec.triage).Execute(context.Background(), reviewCall(raw)); !result.IsError || len(rec.reviews) != 0 {
				t.Fatalf("bad arguments reached the user: %+v", result)
			}
		})
	}
}

// Long replies are cut down until the result fits the tool output limit, the
// longest first, so a short reply survives whole beside them.
func TestReviewResultStaysWithinOutputLimit(t *testing.T) {
	findings := make([]map[string]string, 12)
	triage := make([]core.Triage, 12)
	for i := range findings {
		findings[i] = map[string]string{"severity": "P1", "title": "t"}
		triage[i] = core.Triage{Decision: core.TriageReply, Reply: strings.Repeat("\x01", maxReplyBytes)}
	}
	triage[0].Reply = "keep me"
	rec := &triageRecorder{answer: core.Answer{Triage: triage}}
	result := WithInteractions(&stubExecutor{}, nil, rec.triage).Execute(context.Background(), reviewCall(questionJSON(map[string]any{"findings": findings})))
	if len(result.Content) > maxOutput {
		t.Fatalf("result is %d bytes, limit %d", len(result.Content), maxOutput)
	}
	got := decodeReviewResult(t, result)
	if got.Decisions[0].Reply != "keep me" || !strings.HasSuffix(got.Decisions[1].Reply, "[reply truncated]") {
		t.Fatalf("replies not cut fairly: %q / %q", got.Decisions[0].Reply, got.Decisions[1].Reply[len(got.Decisions[1].Reply)-20:])
	}
}

// A review waits while a question is on screen: one card at a time.
func TestReviewSharesTheQuestionSlot(t *testing.T) {
	release := make(chan struct{})
	asking := make(chan struct{})
	reviewed := make(chan struct{}, 1)
	executor := WithInteractions(&stubExecutor{},
		func(context.Context, core.Question) (core.Answer, error) {
			close(asking)
			<-release
			return core.Answer{Text: "ok"}, nil
		},
		func(context.Context, core.Review) (core.Answer, error) {
			reviewed <- struct{}{}
			return core.Answer{Skipped: true}, nil
		})
	go executor.Execute(context.Background(), questionCall(`{"question":"q"}`))
	within(t, asking, "question to open")
	done := make(chan core.ToolResult, 1)
	go func() { done <- executor.Execute(context.Background(), reviewCall(twoFindings)) }()
	select {
	case <-reviewed:
		t.Fatal("review opened while a question was on screen")
	case <-done:
		t.Fatal("review returned while a question was on screen")
	default:
	}
	close(release)
	within(t, reviewed, "review to open after the question")
	within(t, done, "review to return")
}

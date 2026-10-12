package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openaicompat"
	"julia/internal/core"
)

func TestCompatibleCompactionSendsTranscriptAsDataWithoutTools(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var body struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
			ToolChoice      string `json:"tool_choice"`
			ReasoningEffort string `json:"reasoning_effort"`
			MaxTokens       int    `json:"max_tokens"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		valid := len(body.Messages) == 2 && body.Messages[0].Role == "system" && body.Messages[1].Role == "user" && body.ToolChoice == "none" && body.ReasoningEffort == "low"
		w.Header().Set("Content-Type", "application/json")
		if !valid {
			fmt.Fprint(w, `{"choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","content":null,"tool_calls":[{"id":"unexpected","type":"function","function":{"name":"Read","arguments":"{}"}}]}}]}`)
			return
		}
		if !strings.Contains(body.Messages[1].Content, "fixture.go") || !strings.Contains(body.Messages[1].Content, "Review uncommitted changes") {
			t.Error("summary request lost task or tool evidence")
		}
		if strings.Contains(body.Messages[1].Content, "PRIVATE_REASONING_SENTINEL") {
			t.Error("summary request retained model reasoning")
		}
		fmt.Fprint(w, `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"Inspected fixture.go; the review is incomplete."}}]}`)
	}))
	defer server.Close()
	backend, err := NewAPI(context.Background(), APIConfig{Provider: "compatible", Model: "z-ai/glm-5.3", BaseURL: server.URL + "/v1", Reasoning: "max"})
	if err != nil {
		t.Fatal(err)
	}
	backend.OpenRouter = true
	history := []fantasy.Message{
		fantasy.NewUserMessage("Review uncommitted changes"),
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{
			fantasy.ReasoningPart{Text: "PRIVATE_REASONING_SENTINEL"},
			fantasy.ToolCallPart{ToolCallID: "read-1", ToolName: "Read", Input: `{"path":"fixture.go"}`},
		}},
		{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{fantasy.ToolResultPart{ToolCallID: "read-1", Output: fantasy.ToolResultOutputContentText{Text: "package fixture"}}}},
	}
	before, _ := json.Marshal(history)
	summary, _, err := backend.compact(context.Background(), history, "Review uncommitted changes")
	if err != nil {
		t.Fatal(err)
	}
	if requests != 1 || len(summary) != 2 || !strings.Contains(summary[0].Content[0].(fantasy.TextPart).Text, "review is incomplete") {
		t.Fatalf("unexpected compaction result: requests=%d summary=%v", requests, summary)
	}
	after, _ := json.Marshal(history)
	if string(before) != string(after) || backend.Reasoning != "max" {
		t.Fatal("compaction changed original history or executing-model reasoning")
	}
}

type compactionModel struct {
	scriptedAPIModel
	responses []*fantasy.Response
}

func (m *compactionModel) Generate(_ context.Context, call fantasy.Call) (*fantasy.Response, error) {
	m.generateCalls = append(m.generateCalls, call)
	if len(m.responses) == 0 {
		return nil, fmt.Errorf("unexpected summary retry")
	}
	response := m.responses[0]
	m.responses = m.responses[1:]
	return response, nil
}

func completeSummary(text string) *fantasy.Response {
	return &fantasy.Response{FinishReason: fantasy.FinishReasonStop, Content: fantasy.ResponseContent{fantasy.TextContent{Text: text}}}
}

func TestCompactionRetriesUnusableResponsesWithoutReducingBudget(t *testing.T) {
	for _, tt := range []struct {
		name     string
		response *fantasy.Response
	}{
		{"nil", nil},
		{"empty", completeSummary(" ")},
		{"reasoning only", &fantasy.Response{FinishReason: fantasy.FinishReasonStop, Content: fantasy.ResponseContent{fantasy.ReasoningContent{Text: "Still thinking"}}}},
		{"tool call with text", &fantasy.Response{FinishReason: fantasy.FinishReasonToolCalls, Content: fantasy.ResponseContent{fantasy.TextContent{Text: "Let me read another file."}, fantasy.ToolCallContent{ToolName: "Read"}}}},
		{"tool call reported as stop", &fantasy.Response{FinishReason: fantasy.FinishReasonStop, Content: fantasy.ResponseContent{fantasy.TextContent{Text: "Let me read another file."}, fantasy.ToolCallContent{ToolName: "Read"}}}},
		{"tool markup", completeSummary("<tool_call>Bash<arg_key>command</arg_key><arg_value>go test</arg_value></tool_call>")},
		{"truncated text", &fantasy.Response{FinishReason: fantasy.FinishReasonLength, Content: fantasy.ResponseContent{fantasy.TextContent{Text: "Incomplete summary"}}}},
		{"unknown completion", &fantasy.Response{Content: fantasy.ResponseContent{fantasy.TextContent{Text: "Unconfirmed summary"}}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			model := &compactionModel{responses: []*fantasy.Response{tt.response, completeSummary("Continue the unfinished review.")}}
			backend := &API{Model: model, Provider: "compatible", OpenRouter: true, Reasoning: "high"}
			_, _, err := backend.compact(context.Background(), []fantasy.Message{fantasy.NewUserMessage("Review")}, "Review")
			if err != nil {
				t.Fatal(err)
			}
			if len(model.generateCalls) != 2 {
				t.Fatalf("invalid response accepted without retry: calls=%d", len(model.generateCalls))
			}
			first, retry := model.generateCalls[0], model.generateCalls[1]
			if *retry.MaxOutputTokens < *first.MaxOutputTokens {
				t.Fatal("retry reduced the reasoning/output budget")
			}
			options := first.ProviderOptions[openaicompat.Name].(*openaicompat.ProviderOptions)
			if options.ReasoningEffort != nil {
				t.Fatal("unknown model received an unverified reasoning effort")
			}
		})
	}
}

func TestCompactionFailurePreservesHistoryAndReportsBothAttempts(t *testing.T) {
	model := &compactionModel{responses: []*fantasy.Response{completeSummary(""), {FinishReason: fantasy.FinishReasonLength, Content: fantasy.ResponseContent{fantasy.TextContent{Text: "Partial"}}}}}
	history := []fantasy.Message{fantasy.NewUserMessage("Preserve this task verbatim.")}
	before, _ := json.Marshal(history)
	model.responses[0].Usage = fantasy.Usage{InputTokens: 1000, CacheReadTokens: 100, OutputTokens: 380}
	model.responses[1].Usage = fantasy.Usage{InputTokens: 2000, CacheReadTokens: 500, OutputTokens: 220}
	summary, spent, err := (&API{Model: model}).compact(context.Background(), history, "Preserve this task verbatim.")
	if err == nil || summary != nil || !strings.Contains(err.Error(), "first attempt") || !strings.Contains(err.Error(), "length") {
		t.Fatalf("unusable summary accepted or diagnostics lost: summary=%v err=%v", summary, err)
	}
	after, _ := json.Marshal(history)
	if string(before) != string(after) {
		t.Fatal("failed compaction changed the original history")
	}
	if want := (core.TokenUsage{Input: 3000, CacheRead: 600, Output: 600}); spent != want {
		t.Fatalf("failed attempt usage lost: got %+v want %+v", spent, want)
	}
}

func TestCompactionDoesNotRetryTerminalProviderFailure(t *testing.T) {
	for _, reason := range []fantasy.FinishReason{fantasy.FinishReasonContentFilter, fantasy.FinishReasonError} {
		model := &compactionModel{responses: []*fantasy.Response{{FinishReason: reason}}}
		if _, _, err := (&API{Model: model}).compact(context.Background(), nil, "Review"); err == nil || len(model.generateCalls) != 1 {
			t.Fatalf("terminal provider failure %q was accepted or retried: %v", reason, err)
		}
	}
}

func TestCompactionPreservesAllAnswerTextAndVerbatimTask(t *testing.T) {
	const task = "Review this file.\nDo not deploy or send messages."
	model := &compactionModel{responses: []*fantasy.Response{{FinishReason: fantasy.FinishReasonStop, Content: fantasy.ResponseContent{
		fantasy.TextContent{Text: "Files inspected."},
		fantasy.ReasoningContent{Text: "Hidden thought"},
		fantasy.TextContent{Text: "Tests remain unverified."},
	}}}}
	summary, _, err := (&API{Model: model}).compact(context.Background(), []fantasy.Message{fantasy.NewUserMessage(task)}, task)
	if err != nil {
		t.Fatal(err)
	}
	text := summary[0].Content[0].(fantasy.TextPart).Text
	if !strings.Contains(text, "Files inspected.\nTests remain unverified.") || strings.Contains(text, "Hidden thought") {
		t.Fatalf("answer text was lost or reasoning leaked: %s", text)
	}
	if got := summary[1].Content[0].(fantasy.TextPart).Text; got != "Current user task, retained verbatim:\n"+task {
		t.Fatalf("current task changed: %q", got)
	}
}

func TestCompactionCancelledBeforeRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	model := &compactionModel{}
	if _, _, err := (&API{Model: model}).compact(ctx, nil, "Review"); err != context.Canceled || len(model.generateCalls) != 0 {
		t.Fatalf("cancelled compaction generated a request: calls=%d err=%v", len(model.generateCalls), err)
	}
}

func withUsage(response *fantasy.Response, usage fantasy.Usage) *fantasy.Response {
	response.Usage = usage
	return response
}

func TestCompactionReportsEveryAttemptsTokens(t *testing.T) {
	first := fantasy.Usage{InputTokens: 700, CacheReadTokens: 300, OutputTokens: 8192}
	retry := fantasy.Usage{InputTokens: 650, CacheReadTokens: 350, CacheCreationTokens: 10, OutputTokens: 120}
	for _, tt := range []struct {
		name      string
		provider  string
		responses []*fantasy.Response
		wantErr   bool
		want      core.TokenUsage
	}{
		{"first attempt succeeds", "anthropic", []*fantasy.Response{withUsage(completeSummary("Summary."), retry)}, false, core.TokenUsage{Input: 650, CacheRead: 350, CacheWrite: 10, Output: 120}},
		{"unusable first attempt is still paid for", "anthropic", []*fantasy.Response{withUsage(completeSummary(" "), first), withUsage(completeSummary("Summary."), retry)}, false, core.TokenUsage{Input: 1350, CacheRead: 650, CacheWrite: 10, Output: 8312}},
		{"both attempts unusable", "openai", []*fantasy.Response{withUsage(completeSummary(" "), first), withUsage(completeSummary(""), first)}, true, core.TokenUsage{Input: 1400, CacheRead: 600, Output: 16384}},
		{"retry request fails after a paid first attempt", "openai", []*fantasy.Response{withUsage(completeSummary(" "), first)}, true, core.TokenUsage{Input: 700, CacheRead: 300, Output: 8192}},
		{"terminal provider failure is not retried but is counted", "openai", []*fantasy.Response{withUsage(&fantasy.Response{FinishReason: fantasy.FinishReasonContentFilter}, first)}, true, core.TokenUsage{Input: 700, CacheRead: 300, Output: 8192}},
		{"google prompt count includes cached content", "google", []*fantasy.Response{withUsage(completeSummary("Summary."), fantasy.Usage{InputTokens: 1000, CacheReadTokens: 600, OutputTokens: 50})}, false, core.TokenUsage{Input: 400, CacheRead: 600, Output: 50}},
		{"provider reports nothing", "openai", []*fantasy.Response{completeSummary("Summary.")}, false, core.TokenUsage{}},
		{"no response at all", "openai", []*fantasy.Response{nil, nil}, true, core.TokenUsage{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			model := &compactionModel{responses: tt.responses}
			_, spent, err := (&API{Model: model, Provider: tt.provider}).compact(context.Background(), []fantasy.Message{fantasy.NewUserMessage("Review")}, "Review")
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, want failure=%t", err, tt.wantErr)
			}
			if spent != tt.want {
				t.Fatalf("spent %+v, want %+v", spent, tt.want)
			}
		})
	}
}

func TestCompactionReportsTokensSpentBeforeCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	model := &cancelAfterGenerate{compactionModel: compactionModel{responses: []*fantasy.Response{withUsage(completeSummary("Summary."), fantasy.Usage{InputTokens: 90, OutputTokens: 10})}}, cancel: cancel}
	_, spent, err := (&API{Model: model}).compact(ctx, []fantasy.Message{fantasy.NewUserMessage("Review")}, "Review")
	if err == nil || spent != (core.TokenUsage{Input: 90, Output: 10}) {
		t.Fatalf("a response received just before cancellation was not counted: spent=%+v err=%v", spent, err)
	}
}

type cancelAfterGenerate struct {
	compactionModel
	cancel context.CancelFunc
}

func (m *cancelAfterGenerate) Generate(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	response, err := m.compactionModel.Generate(ctx, call)
	m.cancel()
	return response, err
}

// sizedSummaryModel rejects a summarization request over limit bytes the way a
// provider rejects one that does not fit its window.
type sizedSummaryModel struct {
	scriptedAPIModel
	limit    int
	sizes    []int
	requests []string // each request's text, since the caller reuses its prompt slice
}

func (m *sizedSummaryModel) Generate(_ context.Context, call fantasy.Call) (*fantasy.Response, error) {
	m.generateCalls = append(m.generateCalls, call)
	request := call.Prompt[1].Content[0].(fantasy.TextPart).Text
	m.sizes, m.requests = append(m.sizes, len(request)), append(m.requests, request)
	if len(request) > m.limit {
		return nil, &fantasy.ProviderError{Message: "context_length_exceeded", ContextTooLargeErr: true}
	}
	return completeSummary("Continue the review."), nil
}

// readHistory is n Read calls whose results are size bytes each: a recognizable
// head and tail around source-like filler.
func readHistory(n, size int) []fantasy.Message {
	history := []fantasy.Message{fantasy.NewUserMessage("Review the repository")}
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("read-%d", i)
		body := fmt.Sprintf("HEAD-%d\n", i) + strings.Repeat("    12\tfunc f() error { return nil }\n", size/32)
		body = body[:size-len(fmt.Sprintf("TAIL-%d", i))] + fmt.Sprintf("TAIL-%d", i)
		history = append(history,
			fantasy.Message{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.ToolCallPart{ToolCallID: id, ToolName: "Read", Input: fmt.Sprintf(`{"path":"file%d.go"}`, i)}}},
			fantasy.Message{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{fantasy.ToolResultPart{ToolCallID: id, Output: fantasy.ToolResultOutputContentText{Text: body}}}})
	}
	return history
}

func TestCompactionRequestFitsTheWindow(t *testing.T) {
	// 640 KB of tool results would be well over this window once re-encoded.
	const window = 32_000
	history := readHistory(80, 8000)
	before, _ := json.Marshal(history)
	budget := (&API{ContextTokens: window}).compactionBudget()
	model := &sizedSummaryModel{limit: budget + 1000}
	summary, _, err := (&API{Model: model, ContextTokens: window}).compact(context.Background(), history, "Review the repository")
	if err != nil || len(summary) != 2 || len(model.sizes) != 1 {
		t.Fatalf("an oversized history must still be summarized in one request: err=%v requests=%v", err, model.sizes)
	}
	for _, want := range []string{"HEAD-79", "TAIL-79", "bytes omitted", "Review the repository"} {
		if !strings.Contains(model.requests[0], want) {
			t.Errorf("request lost %q", want)
		}
	}
	if after, _ := json.Marshal(history); string(before) != string(after) {
		t.Fatal("shrinking the request changed the live history")
	}
}

func TestCompactionRetriesARequestThatWasTooLargeWithASmallerTranscript(t *testing.T) {
	const window = 32_000
	history := readHistory(80, 8000)
	budget := (&API{ContextTokens: window}).compactionBudget()
	// The estimate of the first request was optimistic: the provider's tokenizer rejects it.
	model := &sizedSummaryModel{limit: budget * 3 / 4}
	summary, _, err := (&API{Model: model, ContextTokens: window}).compact(context.Background(), history, "Review the repository")
	if err != nil || len(summary) != 2 {
		t.Fatalf("a rejected request must be retried smaller: err=%v requests=%v", err, model.sizes)
	}
	if len(model.sizes) != 2 || model.sizes[1] >= model.sizes[0]/2+1000 {
		t.Fatalf("the retry must carry roughly half the transcript: %v", model.sizes)
	}
	if !strings.Contains(model.requests[0], "under 2000 words") || !strings.Contains(model.requests[1], "under 500 words") {
		t.Fatal("the retry must also ask for a shorter summary")
	}
}

func TestCompactionOnlyRetriesWhenTheRequestWasTooLarge(t *testing.T) {
	for _, tt := range []struct {
		name     string
		limit    int
		failure  error
		requests int
		wantErr  string
	}{
		{"unrelated failure", 1 << 30, errors.New("connection reset"), 1, "attempt 1"},
		{"rejected credentials", 1 << 30, &fantasy.ProviderError{Message: "invalid api key", StatusCode: 401}, 1, "attempt 1"},
		{"too large even when halved", 0, nil, 2, "attempt 2"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			model := &failingSummaryModel{sizedSummaryModel: sizedSummaryModel{limit: tt.limit}, failure: tt.failure}
			_, _, err := (&API{Model: model}).compact(context.Background(), readHistory(2, 2000), "Review")
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) || len(model.sizes) != tt.requests {
				t.Fatalf("requests=%d err=%v, want %d requests and %q", len(model.sizes), err, tt.requests, tt.wantErr)
			}
		})
	}
}

type failingSummaryModel struct {
	sizedSummaryModel
	failure error
}

func (m *failingSummaryModel) Generate(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	response, err := m.sizedSummaryModel.Generate(ctx, call)
	if m.failure != nil {
		return nil, m.failure
	}
	return response, err
}

func TestCompactionPayloadShrinksOnlyAsFarAsNeeded(t *testing.T) {
	history := readHistory(10, 6000)
	const task = "Review the repository"
	whole, err := compactionPayload(history, task, 1<<30)
	if err != nil || strings.Contains(string(whole), "bytes omitted") {
		t.Fatalf("a payload within budget must be sent whole (err=%v)", err)
	}
	// Between the whole size and the smallest capped size, bodies are cut but every message stays.
	data, err := compactionPayload(history, task, len(whole)/2)
	if err != nil || len(data) > len(whole)/2 {
		t.Fatalf("payload of %d bytes exceeds the budget %d (err=%v)", len(data), len(whole)/2, err)
	}
	for i := 0; i < 10; i++ {
		if !strings.Contains(string(data), fmt.Sprintf("HEAD-%d", i)) || !strings.Contains(string(data), fmt.Sprintf("TAIL-%d", i)) {
			t.Fatalf("result %d lost its head or tail", i)
		}
	}
	// A budget no cap can meet drops the oldest messages and keeps the newest.
	var tiny []fantasy.Message
	for i := 0; i < 400; i++ {
		tiny = append(tiny, fantasy.NewUserMessage(fmt.Sprintf("MSG-%03d %s", i, strings.Repeat("x", 100))))
	}
	data, err = compactionPayload(tiny, task, 8<<10)
	if err != nil || len(data) > 8<<10 || !strings.Contains(string(data), "MSG-399") || strings.Contains(string(data), "MSG-000") {
		t.Fatalf("oldest messages must go first: len=%d err=%v", len(data), err)
	}
	// The task is retained verbatim in the new history, so the summarizer's copy may be cut too.
	long := strings.Repeat("t", 100_000)
	data, err = compactionPayload([]fantasy.Message{fantasy.NewUserMessage("short")}, long, 8<<10)
	if err != nil || len(data) > 8<<10 {
		t.Fatalf("an enormous task must not defeat the budget: len=%d err=%v", len(data), err)
	}
}

func TestCapText(t *testing.T) {
	if got := capText("short", 64); got != "short" {
		t.Fatalf("text within the limit changed: %q", got)
	}
	long := "HEAD" + strings.Repeat("-", 1000) + "TAIL"
	got := capText(long, 100)
	if !strings.HasPrefix(got, "HEAD") || !strings.HasSuffix(got, "TAIL") || !strings.Contains(got, "bytes omitted") || len(got) >= len(long) {
		t.Fatalf("capped text %q", got)
	}
	// A cut must never split a multi-byte character.
	for limit := 100; limit < 106; limit++ {
		if got = capText(strings.Repeat("é", 1000), limit); !utf8.ValidString(got) || got == strings.Repeat("é", 1000) {
			t.Fatalf("limit %d: cut inside a rune or not cut: %q", limit, got)
		}
	}
	// Omitting a few bytes would cost more than it saves, so the text stays whole.
	if just := strings.Repeat("a", 70); capText(just, 64) != just {
		t.Fatal("a cap that would lengthen the text must leave it alone")
	}
}

func TestAPICompactsAnOversizedHistoryAndStaysUsable(t *testing.T) {
	// A saved conversation of ~480 KB, already past the threshold: the summarization
	// request must be cut to fit, or the session could never run again.
	const window = 32_000
	var messages []core.Message
	for i := 0; i < 60; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		messages = append(messages, core.Message{Role: role, Content: fmt.Sprintf("MSG-%d ", i) + strings.Repeat("review notes ", 8000/13)})
	}
	budget := (&API{ContextTokens: window}).compactionBudget()
	model := &sizedSummaryModel{scriptedAPIModel: scriptedAPIModel{parts: textStep("Done.", fantasy.Usage{InputTokens: 900, OutputTokens: 5})}, limit: budget + 1000}
	session := &core.Session{Messages: messages, Usage: core.SessionUsage{Context: 30_000}}
	backend := &API{Model: model, ContextTokens: window}
	for run := 1; run <= 2; run++ {
		if err := backend.Run(context.Background(), session, core.Prompt{Text: "Verify the review"}, &testTools{}, func(core.Event) {}); err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
	}
	if len(model.sizes) != 1 || model.streams != 2 {
		t.Fatalf("summaries=%d streams=%d, want the first run to compact once and both runs to answer", len(model.sizes), model.streams)
	}
	if session.Usage.Context >= core.CompactionThreshold(window) {
		t.Fatalf("context %d is still over the threshold after compaction", session.Usage.Context)
	}
}

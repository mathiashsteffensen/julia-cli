package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"julia/internal/core"
)

func TestCompatibleRateLimitThenSuccess(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) <= 4 {
			w.Header().Set("Retry-After", "0.001")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error":"Rate limit exceeded"}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"a\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Done.\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	backend, err := NewAPI(context.Background(), APIConfig{Provider: "compatible", Model: "fixture", BaseURL: server.URL + "/v1"})
	if err != nil {
		t.Fatal(err)
	}
	err = backend.Run(context.Background(), &core.Session{}, core.Prompt{Text: "hello"}, &testTools{}, func(core.Event) {})
	if err != nil {
		t.Fatalf("four 429s should recover on the fifth request: calls=%d error=%v", calls.Load(), err)
	}
	if calls.Load() != 5 {
		t.Fatalf("calls=%d, want 5", calls.Load())
	}
}

func TestRateLimitDelay(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		headers  map[string]string
		captured retryHeaders
		attempt  int
		want     time.Duration
	}{
		{"exponential", nil, retryHeaders{}, 4, 40 * time.Second},
		{"case insensitive seconds", map[string]string{"Retry-After": "3"}, retryHeaders{}, 1, 3 * time.Second},
		{"milliseconds", map[string]string{"Retry-After-Ms": "250", "Retry-After": "8"}, retryHeaders{}, 1, 250 * time.Millisecond},
		{"date", map[string]string{"retry-after": now.Add(90 * time.Second).Format(http.TimeFormat)}, retryHeaders{}, 1, 90 * time.Second},
		{"past date", map[string]string{"retry-after": now.Add(-time.Minute).Format(http.TimeFormat)}, retryHeaders{}, 1, 0},
		{"zero", map[string]string{"retry-after": "0"}, retryHeaders{}, 1, 0},
		{"malformed", map[string]string{"retry-after": "NaN"}, retryHeaders{}, 2, 10 * time.Second},
		{"negative", map[string]string{"retry-after": "-1"}, retryHeaders{}, 1, 5 * time.Second},
		{"large", map[string]string{"retry-after": "999999999999999999999"}, retryHeaders{}, 1, time.Duration(1<<63 - 1)},
		{"overflow", map[string]string{"retry-after": "1e999"}, retryHeaders{}, 1, time.Duration(1<<63 - 1)},
		{"captured", nil, retryHeaders{after: "20"}, 1, 20 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := retryDelay(tc.headers, tc.captured, tc.attempt, now); got != tc.want {
				t.Fatalf("delay=%s, want %s", got, tc.want)
			}
		})
	}
}

func TestRateLimitHTTPBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, header string
		status       int
		wantCalls    int32
	}{
		{"exhausted", "0", 429, 5}, {"server delay exceeds budget", "121", 429, 1}, {"non-429", "0", 400, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.Header().Set("Retry-After", tc.header)
				w.WriteHeader(tc.status)
				fmt.Fprint(w, `{"error":"fixture"}`)
			}))
			defer server.Close()
			backend, err := NewAPI(context.Background(), APIConfig{Provider: "compatible", Model: "fixture", BaseURL: server.URL + "/v1"})
			if err != nil {
				t.Fatal(err)
			}
			err = backend.Run(context.Background(), &core.Session{}, core.Prompt{Text: "hello"}, &testTools{}, func(core.Event) {})
			if err == nil || calls.Load() != tc.wantCalls {
				t.Fatalf("calls=%d error=%v", calls.Load(), err)
			}
		})
	}
}

func TestRateLimitCancellationDuringWait(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(429)
	}))
	defer server.Close()
	backend, err := NewAPI(context.Background(), APIConfig{Provider: "compatible", Model: "fixture", BaseURL: server.URL + "/v1"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err = backend.Run(ctx, &core.Session{}, core.Prompt{Text: "hello"}, &testTools{}, func(event core.Event) {
		if strings.Contains(event.Text, "Provider rate limit") {
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) || calls.Load() != 1 {
		t.Fatalf("calls=%d error=%v", calls.Load(), err)
	}
}

func TestRateLimitFifthAttemptPreservesContextErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want error
	}{
		{"canceled", context.Canceled, context.Canceled},
		{"wrapped canceled", fmt.Errorf("request failed: %w", context.Canceled), context.Canceled},
		{"deadline", context.DeadlineExceeded, context.DeadlineExceeded},
		{"wrapped deadline", fmt.Errorf("request failed: %w", context.DeadlineExceeded), context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			limited := fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: &fantasy.ProviderError{
				StatusCode: http.StatusTooManyRequests, Message: "limited", ResponseHeaders: map[string]string{"retry-after": "0"},
			}}
			model := &scriptedAPIModel{steps: [][]fantasy.StreamPart{
				{limited}, {limited}, {limited}, {limited},
				{{Type: fantasy.StreamPartTypeError, Error: tc.err}},
			}}
			err := (&API{Model: model}).Run(context.Background(), &core.Session{}, core.Prompt{Text: "hello"}, &testTools{}, func(core.Event) {})
			if !errors.Is(err, tc.want) || model.streams != 5 {
				t.Fatalf("streams=%d error=%v, want %v", model.streams, err, tc.want)
			}
		})
	}
}

func TestRateLimitAfterToolDoesNotReplayIt(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n >= 2 && n <= 5 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(429)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if n == 1 {
			fmt.Fprint(w, "data: {\"id\":\"a\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"List\",\"arguments\":\"{\\\"path\\\":\\\".\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		encoded, _ := json.Marshal(body["messages"])
		if !strings.Contains(string(encoded), "README.md") {
			t.Error("completed tool result missing")
		}
		fmt.Fprint(w, "data: {\"id\":\"b\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Done\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	backend, err := NewAPI(context.Background(), APIConfig{Provider: "compatible", Model: "fixture", BaseURL: server.URL + "/v1"})
	if err != nil {
		t.Fatal(err)
	}
	executor := &testTools{}
	var events []core.Event
	err = backend.Run(context.Background(), &core.Session{}, core.Prompt{Text: "hello"}, executor, func(event core.Event) { events = append(events, event) })
	if err != nil || calls.Load() != 6 || executor.calls != 1 {
		t.Fatalf("requests=%d tool calls=%d error=%v", calls.Load(), executor.calls, err)
	}
	var retries int
	for _, event := range events {
		if strings.Contains(event.Text, "Provider rate limit") {
			retries++
			if strings.Contains(event.Text, "fixture") {
				t.Fatal("retry progress exposed response data")
			}
		}
	}
	if retries != 4 {
		t.Fatalf("retry events=%d", retries)
	}
}

func TestRateLimitAfterPartialStreamIsNotRetried(t *testing.T) {
	model := &scriptedAPIModel{parts: []fantasy.StreamPart{
		{Type: fantasy.StreamPartTypeTextStart, ID: "text"}, {Type: fantasy.StreamPartTypeTextDelta, ID: "text", Delta: "Partial"},
		{Type: fantasy.StreamPartTypeError, Error: &fantasy.ProviderError{StatusCode: 429, Message: "limited", ResponseHeaders: map[string]string{"retry-after": "0"}}},
	}}
	session := &core.Session{}
	err := (&API{Model: model}).Run(context.Background(), session, core.Prompt{Text: "hello"}, &testTools{}, func(core.Event) {})
	if err == nil || model.streams != 1 || !strings.Contains(session.Messages[len(session.Messages)-1].Content, "Response interrupted") {
		t.Fatalf("streams=%d error=%v messages=%+v", model.streams, err, session.Messages)
	}
}

func TestGoogleRateLimitHonorsHTTPHeader(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "121")
		w.WriteHeader(429)
		fmt.Fprint(w, `{"error":{"code":429,"message":"fixture limit","status":"RESOURCE_EXHAUSTED"}}`)
	}))
	defer server.Close()
	backend, err := NewAPI(context.Background(), APIConfig{Provider: "google", Model: "fixture", APIKey: "fixture", BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	err = backend.Run(context.Background(), &core.Session{}, core.Prompt{Text: "hello"}, &testTools{}, func(core.Event) {})
	if err == nil || calls.Load() != 1 || !strings.Contains(err.Error(), "rate limit retry stopped") {
		t.Fatalf("calls=%d error=%v", calls.Load(), err)
	}
}

func TestAnthropicRateLimitDoesNotMultiplyRetries(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(429)
		fmt.Fprint(w, `{"type":"error","error":{"type":"rate_limit_error","message":"fixture limit"}}`)
	}))
	defer server.Close()
	backend, err := NewAPI(context.Background(), APIConfig{Provider: "anthropic", Model: "fixture", APIKey: "fixture", BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	err = backend.Run(context.Background(), &core.Session{}, core.Prompt{Text: "hello"}, &testTools{}, func(core.Event) {})
	if err == nil || calls.Load() != 5 {
		t.Fatalf("calls=%d error=%v", calls.Load(), err)
	}
}

func TestOpenAIAndGoogleRateLimitsDoNotMultiplyRetries(t *testing.T) {
	for _, tc := range []struct{ provider, body string }{
		{"openai", `{"error":{"message":"fixture limit","type":"rate_limit_error"}}`},
		{"google", `{"error":{"code":429,"message":"fixture limit","status":"RESOURCE_EXHAUSTED"}}`},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(429)
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			backend, err := NewAPI(context.Background(), APIConfig{Provider: tc.provider, Model: "fixture", APIKey: "fixture", BaseURL: server.URL})
			if err != nil {
				t.Fatal(err)
			}
			err = backend.Run(context.Background(), &core.Session{}, core.Prompt{Text: "hello"}, &testTools{}, func(core.Event) {})
			if err == nil || calls.Load() != 5 {
				t.Fatalf("calls=%d error=%v", calls.Load(), err)
			}
		})
	}
}

func TestRateLimitBudgetIncludesOtherFantasyRetries(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "0")
		if n <= 4 {
			w.WriteHeader(429)
		} else {
			w.WriteHeader(500)
		}
		fmt.Fprint(w, `{"error":{"message":"fixture"}}`)
	}))
	defer server.Close()
	backend, err := NewAPI(context.Background(), APIConfig{Provider: "compatible", Model: "fixture", BaseURL: server.URL + "/v1"})
	if err != nil {
		t.Fatal(err)
	}
	err = backend.Run(context.Background(), &core.Session{}, core.Prompt{Text: "hello"}, &testTools{}, func(core.Event) {})
	if err == nil || calls.Load() != 5 {
		t.Fatalf("calls=%d error=%v", calls.Load(), err)
	}
}

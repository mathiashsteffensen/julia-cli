package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"charm.land/fantasy"
	"julia/internal/core"
)

func TestOpenRouterRejectionReportsTheUpstreamReason(t *testing.T) {
	// OpenRouter's own message is generic; the provider's reply rides in metadata.raw.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"message":"Provider returned error","code":400,"metadata":{"raw":"{\"object\":\"error\",\"message\":\"Invalid tool schema: None is not of type 'array'\",\"type\":\"invalid_request_tool_schema\"}","provider_name":"Mistral","is_byok":false}}}`)
	}))
	defer server.Close()
	backend, err := NewAPI(context.Background(), APIConfig{Provider: "compatible", Model: "fixture", BaseURL: server.URL + "/v1"})
	if err != nil {
		t.Fatal(err)
	}
	err = backend.Run(context.Background(), &core.Session{}, core.Prompt{Text: "Review"}, &testTools{}, func(core.Event) {})
	if err == nil || !strings.Contains(err.Error(), "Provider returned error (Mistral: Invalid tool schema: None is not of type 'array')") {
		t.Fatalf("error = %v, want the upstream reason", err)
	}
	var providerErr *fantasy.ProviderError
	if !errors.As(err, &providerErr) || providerErr.StatusCode != http.StatusBadRequest {
		t.Fatalf("provider error no longer reachable through %v", err)
	}
}

func TestUpstreamDetailShapes(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"mid-stream envelope", `{"error":{"message":"Provider returned error","metadata":{"raw":"{\"detail\":\"model overloaded\"}","provider_name":"Groq"}}}`, "Groq: model overloaded"},
		{"nested error object", `{"error":{"metadata":{"raw":"{\"error\":{\"message\":\"bad role\"}}"}}}`, "bad role"},
		{"plain text reply", `{"error":{"metadata":{"raw":"upstream timed out","provider_name":"Fireworks"}}}`, "Fireworks: upstream timed out"},
		{"object reply", `{"error":{"metadata":{"raw":{"message":"quota"}}}}`, "quota"},
		{"no metadata", `{"error":{"message":"invalid model"}}`, ""},
		{"not json", `bad gateway`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := &fantasy.ProviderError{Message: "stream error", ResponseBody: []byte(tc.body)}
			if got := upstreamDetail(err); got != tc.want {
				t.Fatalf("detail = %q, want %q", got, tc.want)
			}
		})
	}
	long := &fantasy.ProviderError{ResponseBody: []byte(`{"error":{"metadata":{"raw":"` + strings.Repeat("x", 2*maxUpstreamDetail) + `"}}}`)}
	if got := upstreamDetail(long); len(got) > maxUpstreamDetail+len("…") {
		t.Fatalf("detail is %d bytes, want at most %d", len(got), maxUpstreamDetail)
	}
	plain := errors.New("connection reset")
	if explainUpstream(plain) != plain {
		t.Fatal("an error without upstream detail must pass through unchanged")
	}
}

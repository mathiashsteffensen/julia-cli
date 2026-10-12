package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"julia/internal/core"
)

func TestReviewDecisionAPICombinesIndependentChecks(t *testing.T) {
	for _, tc := range []struct {
		name                          string
		relevance, violation, effects float64
		allow                         bool
	}{
		{"routine development", .85, .15, .78, true},
		{"uncertain relevance passes", .61, .17, .91, true},
		{"relevance at its floor", .50, .01, .99, true},
		{"relevance below its floor", .4999, .01, .99, false},
		{"user limits at threshold", .99, .30, .99, false},
		{"effects at threshold", .99, .01, .70, false},
		{"unrelated action", .05, .01, .99, false},
		{"explicit prohibition", .99, .98, .99, false},
		{"prohibited effect", .99, .01, .05, false},
		{"strictly above threshold", .50, .2999, .7001, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					State struct {
						Policy  string `json:"policy"`
						Request struct {
							Task string `json:"task"`
						} `json:"request"`
					} `json:"state"`
					Questions map[string]decisionsQuestion `json:"questions"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if len(body.Questions) != 3 || body.State.Policy != reviewPolicy || body.State.Request.Task != "Add tests for the worker changes" {
					t.Error("review omitted independent checks or trusted task/policy")
				}
				for _, key := range []string{"task_relevant", "violates_user_limits", "prohibited_effects"} {
					if q := body.Questions[key]; q.Type != "noul" || q.Instructions == "" {
						t.Errorf("missing typed check %s", key)
					}
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"answers":{"task_relevant":{"type":"noul","noul":%g},"violates_user_limits":{"type":"noul","noul":%g},"prohibited_effects":{"type":"noul","noul":%g}}}`, tc.relevance, tc.violation, 1-tc.effects)
			}))
			defer server.Close()
			decision, err := ReviewDecisionAPI(context.Background(), APIConfig{Provider: "compatible", Model: "typesafe/jev-1.13", BaseURL: server.URL + "/api/v1", APIKey: "fixture-key"}, core.Approval{
				Task: "Add tests for the worker changes", ExecutionMode: "unsandboxed", Network: true,
				Call: core.ToolCall{Name: "Bash", Arguments: json.RawMessage(`{"command":"go test ./internal/workers"}`)},
			})
			if err != nil || decision.Allow != tc.allow {
				t.Fatalf("allow=%v, want %v; %v", decision.Allow, tc.allow, err)
			}
			for _, check := range []string{"task relevance", "user limits", "permitted effects"} {
				if !strings.Contains(decision.Reason, check) {
					t.Errorf("reason omitted %s: %s", check, decision.Reason)
				}
			}
			// A denial reason goes back to the agent, so it must say which check failed.
			for _, check := range []struct {
				failed bool
				phrase string
			}{
				{tc.relevance < MinimumRelevanceProbability, "does not look relevant"},
				{1-tc.violation <= MinimumApprovalProbability, "conflict with a restriction"},
				{tc.effects <= MinimumApprovalProbability, "not explicitly authorized"},
			} {
				if strings.Contains(decision.Reason, check.phrase) != check.failed {
					t.Errorf("reason mentions %q = %v, want %v: %s", check.phrase, !check.failed, check.failed, decision.Reason)
				}
			}
		})
	}
}

func TestReviewDecisionAPIRejectsIncompleteOrMalformedChecks(t *testing.T) {
	for _, response := range []string{
		`{"answers":{"authorized":{"type":"noul","noul":0.99}}}`,
		`{"answers":{"task_relevant":{"type":"noul","noul":0.99},"violates_user_limits":{"type":"noul","noul":0.01}}}`,
		`{"answers":{"task_relevant":{"type":"noul","noul":0.99},"violates_user_limits":{"type":"noul","noul":0.01},"prohibited_effects":{"type":"choice","noul":0.01}}}`,
		`{"answers":{"task_relevant":{"type":"noul","noul":0.99},"violates_user_limits":{"type":"noul","noul":null},"prohibited_effects":{"type":"noul","noul":0.01}}}`,
		`{"answers":{"task_relevant":{"type":"noul","noul":0.99},"violates_user_limits":{"type":"noul","noul":-0.01},"prohibited_effects":{"type":"noul","noul":0.01}}}`,
		`{"answers":{"task_relevant":{"type":"noul","noul":0.99},"violates_user_limits":{"type":"noul","noul":0.01},"prohibited_effects":{"type":"noul","noul":1.01}}}`,
		`{"answers":{"task_relevant":{"type":"noul","noul":0.99},"violates_user_limits":{"type":"noul","noul":0.01},"prohibited_effects":{"type":"noul","noul":0.01}}} {}`,
	} {
		t.Run(response, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, response) }))
			defer server.Close()
			decision, err := ReviewDecisionAPI(context.Background(), APIConfig{Provider: "compatible", Model: "typesafe/jev-1.13", BaseURL: server.URL, APIKey: "fixture-key"}, core.Approval{Call: core.ToolCall{Name: "Bash", Arguments: json.RawMessage(`{"command":"go test ./..."}`)}})
			if err == nil || decision.Allow {
				t.Fatalf("malformed checks granted approval: %+v %v", decision, err)
			}
		})
	}
}

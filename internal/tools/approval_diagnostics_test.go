package tools

import (
	"context"
	"errors"
	"strings"
	"testing"

	"jarvis/internal/core"
)

func TestAutomaticReviewIsVisibleBeforeReviewerAndHumanFallback(t *testing.T) {
	for _, mode := range []string{"sandboxed", "unsandboxed"} {
		for _, outcome := range []string{"approved", "denied", "failed"} {
			t.Run(mode+"/"+outcome, func(t *testing.T) {
				var events []core.Event
				reviews, humans := 0, 0
				e, _ := fixture(t, "auto", func(_ context.Context, a core.Approval) (core.Decision, error) {
					humans++
					if reviews != 1 || len(events) < 2 || events[len(events)-1].Kind != "warning" || !strings.Contains(a.Reason, "review result fixture") {
						t.Fatal("human fallback did not disclose completed review", reviews, events, a.Reason)
					}
					return core.Decision{Allow: false, Reason: "human denied fixture"}, nil
				})
				e.config.Emit = func(event core.Event) { events = append(events, event) }
				e.config.Review = func(context.Context, core.Approval) (core.Decision, error) {
					reviews++
					if len(events) != 1 || events[0].Kind != "status" || !strings.Contains(events[0].Text, "Automatic review in progress") {
						t.Fatal("review began without visible start status", events)
					}
					if mode == "unsandboxed" && !strings.Contains(events[0].Text, "outside the OS sandbox") {
						t.Fatal("review status omitted escalation boundary", events[0])
					}
					if outcome == "failed" {
						return core.Decision{}, errors.New("review result fixture")
					}
					return core.Decision{Allow: outcome == "approved", Reason: "review result fixture"}, nil
				}
				err := e.authorizeRequest(context.Background(), core.Approval{Call: core.ToolCall{Name: "Bash"}, ExecutionMode: mode, Reason: "Run this command?"}, true)
				if reviews != 1 {
					t.Fatal("expected exactly one review", reviews)
				}
				if outcome == "approved" {
					if err != nil || humans != 0 || len(events) != 2 || !strings.Contains(events[1].Text, "Automatic review approved") {
						t.Fatal("approved review reached human fallback", err, humans, events)
					}
				} else if err == nil || humans != 1 {
					t.Fatal("failed/denied review bypassed human decision", err, humans)
				}
			})
		}
	}
}

func TestAutomaticReviewUnavailableIsDisclosedBeforeHumanFallback(t *testing.T) {
	var events []core.Event
	humans := 0
	e, _ := fixture(t, "auto", func(_ context.Context, a core.Approval) (core.Decision, error) {
		humans++
		if !strings.Contains(a.Reason, "Automatic review unavailable") || !strings.Contains(a.Reason, "no reviewer is configured") {
			t.Fatal("missing reviewer looked like a direct approval request", a.Reason)
		}
		if len(events) != 1 || events[0].Kind != "warning" || events[0].Text != a.Reason {
			t.Fatal("unavailable review was not visible before prompting", events)
		}
		return core.Decision{Allow: false, Reason: "declined"}, nil
	})
	e.config.Emit = func(event core.Event) { events = append(events, event) }
	if err := e.authorizeRequest(context.Background(), core.Approval{Reason: "Run command?"}, true); err == nil || humans != 1 {
		t.Fatal("missing reviewer bypassed approval", err, humans)
	}
}

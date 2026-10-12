package tools

import (
	"context"
	"errors"
	"strings"
	"testing"

	"julia/internal/core"
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
				switch outcome {
				case "approved":
					if err != nil || humans != 0 || len(events) != 2 || !strings.Contains(events[1].Text, "Automatic review approved") {
						t.Fatal("approved review reached human fallback", err, humans, events)
					}
				case "denied":
					// A denial is the reviewer's judgment: the model gets it, visibly, without a prompt.
					if err == nil || humans != 0 || !strings.Contains(err.Error(), "review result fixture") || len(events) != 2 || events[1].Kind != "warning" || !strings.Contains(events[1].Text, "Automatic review denied") {
						t.Fatal("denied review was not returned to the model", err, humans, events)
					}
				default:
					if err == nil || humans != 1 {
						t.Fatal("failed review bypassed human decision", err, humans)
					}
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

func TestRepeatedReviewDenialsReturnToAHuman(t *testing.T) {
	humans := 0
	e, _ := fixture(t, "auto", func(_ context.Context, a core.Approval) (core.Decision, error) {
		humans++
		if !strings.Contains(a.Reason, "Repeated automatic denials") || !strings.Contains(a.Reason, "not authorized") {
			t.Fatal("human was not told why the denial reached them", a.Reason)
		}
		return core.Decision{Allow: false, Reason: "human denied"}, nil
	})
	allow := false
	e.config.Review = func(context.Context, core.Approval) (core.Decision, error) {
		return core.Decision{Allow: allow, Reason: "not authorized"}, nil
	}
	request := func() error {
		return e.authorizeRequest(context.Background(), core.Approval{Call: core.ToolCall{Name: "Bash"}, Reason: "Run this command?"}, true)
	}
	// The third consecutive denial goes to a human, who resets the streak.
	for i, wantHumans := range []int{0, 0, 1, 1, 1, 2} {
		if err := request(); err == nil || humans != wantHumans {
			t.Fatalf("denial %d: humans=%d, want %d (%v)", i+1, humans, wantHumans, err)
		}
	}
	// An approval also resets the streak.
	allow = true
	if err := request(); err != nil {
		t.Fatal(err)
	}
	allow = false
	for range maxConsecutiveDenials - 1 {
		if err := request(); err == nil || humans != 2 {
			t.Fatal("approval did not reset the denial streak", humans, err)
		}
	}
	// Past the turn limit, every denial goes to a human.
	e.denials, e.consecutiveDenials = maxTurnDenials-1, 0
	for i := range 2 {
		if err := request(); err == nil || humans != 3+i {
			t.Fatal("denial past the turn limit did not reach a human", humans, err)
		}
	}
}

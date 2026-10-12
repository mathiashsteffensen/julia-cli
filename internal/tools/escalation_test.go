package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"jarvis/internal/core"
)

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
func escalationArgs(command string) map[string]any {
	return map[string]any{"command": command, "sandbox_permissions": "require_escalated", "justification": "Create the user-requested temporary fixture outside the workspace."}
}

func TestEscalatedCommandRequiresExactApprovalEveryTime(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("requires POSIX Bash")
	}
	e, dir := fixture(t, "auto", nil)
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "approved")
	command := "printf approved > " + shellQuote(outside)
	reviews := 0
	e.config.Review = func(_ context.Context, a core.Approval) (core.Decision, error) {
		reviews++
		if a.ExecutionMode != "unsandboxed" || a.Directory != dir || !a.Network || strings.TrimSpace(a.Justification) == "" {
			t.Errorf("incorrect effective capabilities: %+v", a)
		}
		return core.Decision{Allow: true, Reason: "explicit temporary fixture request"}, nil
	}
	for range 2 {
		result := invoke(e, "Bash", escalationArgs(command))
		if result.IsError {
			t.Fatal(result.Content)
		}
	}
	if reviews != 2 {
		t.Fatalf("got %d reviews for two executions", reviews)
	}
	data, err := os.ReadFile(outside)
	if err != nil || string(data) != "approved" {
		t.Fatal("approved host write failed", string(data), err)
	}
}

func TestEscalationFailureNeverExecutes(t *testing.T) {
	cases := []struct {
		name, mode string
		review     core.Approver
		approve    core.Approver
		mutate     func(map[string]any)
	}{
		{name: "missing handler", mode: "auto"},
		{name: "review denial", mode: "auto", review: func(context.Context, core.Approval) (core.Decision, error) {
			return core.Decision{Allow: false, Reason: "not authorized"}, nil
		}},
		{name: "review failure", mode: "auto", review: func(context.Context, core.Approval) (core.Decision, error) {
			return core.Decision{}, errors.New("malformed reviewer answer")
		}},
		{name: "human denial", mode: "ask", approve: func(context.Context, core.Approval) (core.Decision, error) { return core.Decision{Allow: false}, nil }},
		{name: "human failure", mode: "ask", approve: func(context.Context, core.Approval) (core.Decision, error) {
			return core.Decision{}, errors.New("approval channel unavailable")
		}},
		{name: "missing justification", mode: "auto", mutate: func(a map[string]any) { delete(a, "justification") }},
		{name: "blank justification", mode: "auto", mutate: func(a map[string]any) { a["justification"] = "  " }},
		{name: "unknown permission", mode: "auto", mutate: func(a map[string]any) { a["sandbox_permissions"] = "unrestricted" }},
		{name: "empty permission", mode: "auto", mutate: func(a map[string]any) { a["sandbox_permissions"] = "" }},
		{name: "null permission", mode: "auto", mutate: func(a map[string]any) { a["sandbox_permissions"] = nil }},
		{name: "read only", mode: "read-only"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			e, _ := fixture(t, tt.mode, tt.approve)
			e.config.Review = tt.review
			if tt.mutate != nil || tt.mode == "read-only" {
				e.config.Review = func(context.Context, core.Approval) (core.Decision, error) {
					t.Error("invalid or forbidden request reached reviewer")
					return core.Decision{Allow: true}, nil
				}
			}
			outside := filepath.Join(t.TempDir(), "must-not-exist")
			a := escalationArgs("printf forbidden > " + shellQuote(outside))
			if tt.mutate != nil {
				tt.mutate(a)
			}
			result := invoke(e, "Bash", a)
			if !result.IsError {
				t.Fatal("unexpected approval", result.Content)
			}
			if _, err := os.Stat(outside); !os.IsNotExist(err) {
				t.Fatal("denied command executed", err)
			}
		})
	}
}

func TestSandboxFailureDoesNotRetryOutsideSandbox(t *testing.T) {
	if os.Getenv("JARVIS_SANDBOX_TEST") != "1" || runtime.GOOS != "darwin" {
		t.Skip("requires host macOS sandbox integration")
	}
	// Ask mode, because auto mode runs offline sandboxed Bash unreviewed.
	reviews := 0
	e, _ := fixture(t, "ask", func(_ context.Context, a core.Approval) (core.Decision, error) {
		reviews++
		if a.ExecutionMode != "sandboxed" || a.Network {
			t.Error("changed default capabilities", a)
		}
		return core.Decision{Allow: true}, nil
	})
	outside := filepath.Join(t.TempDir(), "must-not-exist")
	result := invoke(e, "Bash", map[string]any{"command": "printf forbidden > " + shellQuote(outside)})
	if !result.IsError || reviews != 1 {
		t.Fatal("sandbox failure triggered retry", result, reviews)
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatal("default Bash escaped workspace", err)
	}
}

func TestEscalationAskSkipsReviewerAndRepeatedReviewDenialAsks(t *testing.T) {
	for _, mode := range []string{"ask", "auto"} {
		t.Run(mode, func(t *testing.T) {
			reviews, humans := 0, 0
			e, _ := fixture(t, mode, func(_ context.Context, a core.Approval) (core.Decision, error) {
				humans++
				if a.ExecutionMode != "unsandboxed" || !a.Network {
					t.Fatal("human saw false containment", a)
				}
				return core.Decision{Allow: true, Reason: "explicit test approval"}, nil
			})
			e.config.Review = func(context.Context, core.Approval) (core.Decision, error) {
				reviews++
				return core.Decision{Allow: false, Reason: "need human"}, nil
			}
			wantReviews := 0
			if mode == "auto" {
				// Denials go back to the model until the streak reaches the limit.
				wantReviews = maxConsecutiveDenials
				for range maxConsecutiveDenials - 1 {
					if result := invoke(e, "Bash", escalationArgs("printf approved")); !result.IsError || !strings.Contains(result.Content, "automatic review denied") || humans != 0 {
						t.Fatal("review denial was not returned to the model", result, humans)
					}
				}
			}
			result := invoke(e, "Bash", escalationArgs("printf approved"))
			if result.IsError || result.Content != "approved" {
				t.Fatal(result)
			}
			if reviews != wantReviews || humans != 1 {
				t.Fatal(reviews, humans)
			}
		})
	}
}

func TestEscalationCanceledAfterReviewNeverExecutes(t *testing.T) {
	e, _ := fixture(t, "auto", nil)
	outside := filepath.Join(t.TempDir(), "must-not-exist")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.config.Review = func(context.Context, core.Approval) (core.Decision, error) {
		cancel()
		return core.Decision{Allow: true}, nil
	}
	result := invokeContext(ctx, e, "Bash", escalationArgs("printf forbidden > "+shellQuote(outside)))
	if !result.IsError {
		t.Fatal("canceled command succeeded")
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatal("canceled command executed", err)
	}
}

func TestEscalationPreservesSanitizedEnvironmentAndLimits(t *testing.T) {
	e, _ := fixture(t, "auto", nil)
	e.config.Review = func(context.Context, core.Approval) (core.Decision, error) { return core.Decision{Allow: true}, nil }
	t.Setenv("JARVIS_ESCALATION_SECRET", "must-not-leak")
	result := invoke(e, "Bash", escalationArgs("printf '%s' \"${JARVIS_ESCALATION_SECRET-unset}\""))
	if result.IsError || result.Content != "unset" {
		t.Fatal("inherited credential leaked", result)
	}
	result = invoke(e, "Bash", escalationArgs("head -c 100000 /dev/zero | tr '\\0' x"))
	if result.IsError || len(result.Content) > maxOutput || !strings.Contains(result.Content, "truncated") {
		t.Fatal("output was not bounded", len(result.Content), result.IsError)
	}
	args := escalationArgs("sleep 30 & wait")
	args["timeout"] = 1
	start := time.Now()
	result = invoke(e, "Bash", args)
	if !result.IsError || !strings.Contains(result.Content, "deadline exceeded") || time.Since(start) > 4*time.Second {
		t.Fatal("timeout did not stop process group", result, time.Since(start))
	}
}

func invokeContext(ctx context.Context, e *Executor, name string, args any) core.ToolResult {
	b, _ := json.Marshal(args)
	return e.Execute(ctx, core.ToolCall{Name: name, Arguments: b})
}

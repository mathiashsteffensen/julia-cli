package ui

import (
	"strings"
	"testing"

	"julia/internal/core"

	"github.com/charmbracelet/x/ansi"
)

func TestGoodbyeReportsUsageAndResume(t *testing.T) {
	s := &core.Session{ID: "9f3c2a7be41d08c5e6a1f47d", Title: "Tidy the store", Provider: "codex", Model: "gpt-5.5", Reasoning: "high",
		Usage:    core.SessionUsage{Total: core.TokenUsage{Input: 200_000, CacheRead: 800_000, Output: 48_210}},
		Messages: []core.Message{{Role: "user"}, {Role: "assistant"}, {Role: "user"}}}
	resume := "julia --resume " + s.ID + " --dir /a/rather/long/workspace/path"
	out := ansi.Strip(RenderGoodbye(s, resume, 60))
	for _, want := range []string{"Until next time.", "Tidy the store", "1M in · 48.2k out", "80% from cache", "codex · gpt-5.5 · high", s.ID + " · 2 turns", "› " + resume} {
		if !strings.Contains(out, want) {
			t.Errorf("goodbye lacks %q:\n%s", want, out)
		}
	}
	if !strings.HasPrefix(out, "\n") || !strings.HasSuffix(out, "\n\n") {
		t.Errorf("goodbye should be parted from the prompts by blank rows: %q", out)
	}
}

func TestGoodbyeWithoutTurnsOffersNoResume(t *testing.T) {
	s := &core.Session{ID: "9f3c2a7be41d08c5e6a1f47d", Title: "New conversation", Provider: "codex"}
	out := ansi.Strip(RenderGoodbye(s, "julia --resume "+s.ID, 80))
	if strings.Contains(out, "RESUME") || strings.Contains(out, s.ID) {
		t.Errorf("an empty session cannot be resumed:\n%s", out)
	}
	if !strings.Contains(out, "nothing to resume") {
		t.Errorf("goodbye should say why there is no resume:\n%s", out)
	}
}

func TestGoodbyeSanitizesTitle(t *testing.T) {
	s := &core.Session{ID: "abc", Title: "evil\x1b]52;c;aGk=\x07 title", Messages: []core.Message{{Role: "user"}}}
	if out := RenderGoodbye(s, "julia --resume abc", 80); strings.Contains(out, "\x1b]52") {
		t.Errorf("title escaped into the terminal: %q", out)
	}
}

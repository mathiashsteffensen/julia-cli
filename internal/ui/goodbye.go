package ui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/x/ansi"
	"julia/internal/core"
)

// RenderGoodbye is what stays in the scrollback once the interface closes: the
// quadrant wordmark, a sign-off with the conversation's title, a hairline, and
// a ledger of the session's tokens, model and the command that resumes it.
// It is set flush left, two cells in, like the shell output around it, and
// paints no backgrounds, since the terminal's own may not be the interface's.
// The session ID and the resume command are never cut short, so they can be
// copied whole; a conversation with nothing in it was never saved, so it
// offers no resume.
func RenderGoodbye(s *core.Session, resume string, width int) string {
	const indent, keyWidth = 2, 9
	width = max(width-indent, 24)
	value := width - keyWidth
	pad := strings.Repeat(" ", indent)
	label := func(s string) string {
		return violetStyle.Bold(true).Render(s) + strings.Repeat(" ", keyWidth-ansi.StringWidth(s))
	}
	fit := func(s string, w int) string { return ansi.Truncate(sanitize(s), w, "…") }

	rows := append(logoLettering(), "", creamStyle.Bold(true).Render("Until next time, sir."))
	turns := 0
	for _, m := range s.Messages {
		if m.Role == "user" {
			turns++
		}
	}
	if turns == 0 {
		rows = append(rows, mutedStyle.Italic(true).Render(fit("Nothing was said, so there is nothing to resume.", width)))
		return indented(pad, rows)
	}
	rows = append(rows, mutedStyle.Italic(true).Render(fit(s.Title, width)), "", hairline(min(width, 41), "◆"), "")

	total := s.Usage.Total
	rows = append(rows, label("TOKENS")+creamStyle.Render(fit(core.CompactTokens(total.Prompt())+" in · "+core.CompactTokens(total.Output)+" out", value)))
	if prompt := total.Prompt(); prompt > 0 {
		bar := min(value, 20)
		hit := float64(total.CacheRead) * 100 / float64(prompt)
		rows = append(rows, label("")+usageBar(total.CacheRead, prompt, bar, cyanStyle)+mutedStyle.Render(fit(fmt.Sprintf(" %.0f%% from cache", hit), value-bar)))
	}
	model := s.Model
	if model == "" {
		model = "provider default"
	}
	if s.Reasoning != "" {
		model += " · " + s.Reasoning
	}
	count := fmt.Sprintf(" · %d turns", turns)
	if turns == 1 {
		count = " · 1 turn"
	}
	rows = append(rows,
		label("MODEL")+cyanStyle.Render(fit(s.Provider, value))+mutedStyle.Render(fit(" · "+model, max(0, value-ansi.StringWidth(s.Provider)))),
		label("SESSION")+creamStyle.Render(s.ID)+mutedStyle.Render(fit(count, max(0, value-ansi.StringWidth(s.ID)))),
		"",
		label("RESUME")+coralStyle.Render("› ")+creamStyle.Bold(true).Render(sanitize(resume)),
	)
	return indented(pad, rows)
}

// indented sets rows two cells in, with a blank row above and below to part
// them from the shell prompts around them.
func indented(pad string, rows []string) string {
	var b strings.Builder
	b.WriteString("\n")
	for _, row := range rows {
		if row != "" {
			b.WriteString(pad + row)
		}
		b.WriteString("\n")
	}
	b.WriteString("\n")
	return b.String()
}

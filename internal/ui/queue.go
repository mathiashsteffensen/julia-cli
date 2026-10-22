package ui

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"julia/internal/core"
)

// queuedMessage is a message the user sent while a turn ran. It waits for
// the turn to settle and is then sent as the next one, unless the user steers
// it into the turn under way, which the backend takes at its next step.
type queuedMessage struct {
	prompt   core.Prompt
	steering bool // handed to the turn's inbox; shown in the conversation once delivered
}

// queue holds a message until the turn under way settles.
func (m *model) queue(prompt core.Prompt) {
	m.queued = append(m.queued, queuedMessage{prompt: prompt})
	m.notice = ""
}

// steer hands every queued message to the turn under way, oldest first. A
// message the backend has not taken when the turn ends is sent as the next
// turn, as a queued one is.
func (m *model) steer() {
	if !m.busy || m.inbox == nil {
		return
	}
	for i := range m.queued {
		if !m.queued[i].steering {
			m.queued[i].steering = true
			m.inbox.Send(m.queued[i].prompt)
		}
	}
}

// receiveSteer shows a message the backend delivered mid-turn where the
// conversation now has it: the oldest steering one, since the backend takes
// them in order.
func (m *model) receiveSteer() tea.Cmd {
	i := slices.IndexFunc(m.queued, func(q queuedMessage) bool { return q.steering })
	if i < 0 {
		return nil
	}
	q := m.queued[i]
	m.queued = slices.Delete(m.queued, i, i+1)
	sent := entry{role: "user", text: q.prompt.Text, images: q.prompt.Images}
	m.registerImages(&sent)
	m.entries = append(m.entries, sent)
	m.status = "Thinking"
	return m.uploadThumbnails(sent.imageIDs...)
}

// settleQueue takes what is still queued when a turn ends. After a turn that
// finished, it is sent as the next turn, as one message; after one that
// failed or was interrupted, it goes back to the composer, for the user to
// send again or not.
func (m *model) settleQueue(err error) tea.Cmd {
	waiting := m.queued
	m.queued, m.inbox = nil, nil
	if len(waiting) == 0 || m.quitting {
		return nil
	}
	prompts := make([]core.Prompt, len(waiting))
	for i, q := range waiting {
		prompts[i] = q.prompt
	}
	prompt := core.JoinPrompts(prompts)
	if err != nil {
		m.restoreDraft(prompt)
		m.notice = "Your queued message is back in the composer · Enter sends it"
		return nil
	}
	return m.send(prompt)
}

// editQueued takes the newest message not yet steered back into the empty
// composer, and reports whether there was one.
func (m *model) editQueued() bool {
	if m.input.Value() != "" {
		return false
	}
	for i := len(m.queued) - 1; i >= 0; i-- {
		if !m.queued[i].steering {
			prompt := m.queued[i].prompt
			m.queued = slices.Delete(m.queued, i, i+1)
			m.restoreDraft(prompt)
			return true
		}
	}
	return false
}

// restoreDraft puts a message back in the composer ahead of the draft, its
// images attached again under the draft's next numbers.
func (m *model) restoreDraft(p core.Prompt) {
	renumber := make(map[int]int, len(p.Images))
	for i, image := range p.Images {
		m.imageSeq++
		m.attachments = append(m.attachments, attachment{number: m.imageSeq, image: image})
		renumber[i+1] = m.imageSeq
	}
	text := imageTokenPattern.ReplaceAllStringFunc(p.Text, func(token string) string {
		number, _ := strconv.Atoi(imageTokenPattern.FindStringSubmatch(token)[1])
		if next, ok := renumber[number]; ok {
			return imageToken(next)
		}
		return token
	})
	if draft := m.input.Value(); strings.TrimSpace(draft) != "" {
		text += "\n\n" + draft
	}
	m.input.SetValue(text)
}

// canSteer reports whether a queued message is waiting that steering would send.
func (m *model) canSteer() bool {
	return m.busy && m.inbox != nil && slices.ContainsFunc(m.queued, func(q queuedMessage) bool { return !q.steering })
}

// steerButton is the queue strip's button, drawn at the right of its first
// row; a narrow strip gets a shorter one, and the narrowest drops the key.
func (m *model) steerButton() string {
	space := ink(chipBG, cream).Render(" ")
	label := ink(chipBG, cream).Bold(true)
	switch {
	case m.mainWidth < 36:
		return space + label.Render("steer") + space
	case m.mainWidth < 64:
		return space + ink(chipBG, violet).Render("ctrl+s") + space + space + label.Render("steer") + space
	}
	return space + ink(chipBG, violet).Render("ctrl+s") + space + space + label.Render("steer now") + space
}

// queueStrip is the queue as shown above the composer: a heading with the
// steer button, then each message on a row of its own, as many as the
// screen spares.
func (m *model) queueStrip() []string {
	if len(m.queued) == 0 {
		return nil
	}
	width := m.mainWidth
	heading := violetStyle.Render("◇ QUEUED")
	why := " · sends when JULIA settles"
	if !m.canSteer() {
		heading, why = violetStyle.Render("↳ STEERING"), " · JULIA reads it after the current step"
	}
	shown := 3
	switch {
	case m.height < 20:
		shown = 0
	case m.height < 30:
		shown = 1
	}
	var button string
	if m.canSteer() {
		button = m.steerButton()
	}
	// The button ends a cell short of the edge, in line with the composer's border.
	room := width - 2 - ansi.StringWidth(heading) - ansi.StringWidth(button) - 1
	var detail string
	if shown == 0 {
		// No room for the messages' rows: the heading names the first.
		why = " · " + oneLine(m.queued[0].prompt.Text)
		if len(m.queued) > 1 {
			detail = fmt.Sprintf(" +%d", len(m.queued)-1)
		}
		why = ansi.Truncate(why, max(0, room-ansi.StringWidth(detail)), "…")
	} else if ansi.StringWidth(why) > room {
		why = ""
	}
	head := " " + heading + mutedStyle.Render(why) + faintStyle.Render(detail)
	if button != "" {
		head += strings.Repeat(" ", max(1, width-1-ansi.StringWidth(head)-ansi.StringWidth(button))) + button
	}
	rows := []string{ansi.Truncate(head, width, "")}
	for i, q := range m.queued {
		if i == shown {
			break
		}
		marker, text := faintStyle.Render("   › "), creamStyle
		if q.steering {
			marker, text = violetStyle.Render("   ↳ "), mutedStyle
		}
		var more string
		if n := len(q.prompt.Images); n > 0 {
			more = faintStyle.Render(fmt.Sprintf("  ▣ %d", n))
		}
		if i == shown-1 && len(m.queued) > shown {
			more += faintStyle.Render(fmt.Sprintf("  +%d more", len(m.queued)-shown))
		}
		room := max(1, width-ansi.StringWidth(marker)-ansi.StringWidth(more))
		rows = append(rows, marker+text.Render(ansi.Truncate(oneLine(q.prompt.Text), room, "…"))+more)
	}
	return rows
}

// clickSteer steers the queue when a click lands on the steer button, and
// reports whether it did.
func (m *model) clickSteer(x, y int) bool {
	if !m.canSteer() {
		return false
	}
	rows, at := m.bottomLayout()
	if at < 0 || at >= len(rows) {
		return false
	}
	right := m.margin() + m.mainWidth - 1
	left := right - ansi.StringWidth(m.steerButton())
	if y != 3+m.bodyHeight+at || x < left || x >= right {
		return false
	}
	m.steer()
	return true
}

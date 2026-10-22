package core

import (
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// Steering carries the messages the user sends while a turn runs, for the
// backend to put before the model at its next step: after the tool calls
// under way, before the model answers again. Ready is signalled whenever a
// message may be waiting. Take removes and returns every waiting message,
// oldest first. Return puts back, ahead of any others, messages a backend
// took but could not deliver; whatever is still waiting when the turn ends
// is the next turn's to send.
//
// A backend reports each message it delivered with a steer event, whose Text
// is the message's, and records it in the session as a user message.
type Steering interface {
	Ready() <-chan struct{}
	Take() []Prompt
	Return([]Prompt)
}

// Inbox is the Steering an interface fills with Send.
type Inbox struct {
	mu      sync.Mutex
	waiting []Prompt
	ready   chan struct{}
}

func NewInbox() *Inbox { return &Inbox{ready: make(chan struct{}, 1)} }

// Send queues a message for the turn under way.
func (b *Inbox) Send(p Prompt) {
	b.mu.Lock()
	b.waiting = append(b.waiting, p)
	b.mu.Unlock()
	b.signal()
}

func (b *Inbox) Ready() <-chan struct{} { return b.ready }

func (b *Inbox) Take() []Prompt {
	b.mu.Lock()
	defer b.mu.Unlock()
	taken := b.waiting
	b.waiting = nil
	return taken
}

func (b *Inbox) Return(prompts []Prompt) {
	if len(prompts) == 0 {
		return
	}
	b.mu.Lock()
	b.waiting = append(append([]Prompt(nil), prompts...), b.waiting...)
	b.mu.Unlock()
	b.signal()
}

func (b *Inbox) signal() {
	select {
	case b.ready <- struct{}{}:
	default:
	}
}

var imageTokenPattern = regexp.MustCompile(`\[Image #(\d+)\]`)

// JoinPrompts makes one message of several sent in a row: their texts apart
// by a blank line, their images in order, and each one's [Image #N] tokens
// renumbered to name its images in the joined list. A joined prompt steers
// nothing.
func JoinPrompts(prompts []Prompt) Prompt {
	if len(prompts) == 1 {
		return Prompt{Text: prompts[0].Text, Images: prompts[0].Images}
	}
	var joined Prompt
	texts := make([]string, 0, len(prompts))
	for _, p := range prompts {
		offset, count := len(joined.Images), len(p.Images)
		text := p.Text
		if offset > 0 && count > 0 {
			text = imageTokenPattern.ReplaceAllStringFunc(text, func(token string) string {
				number, _ := strconv.Atoi(imageTokenPattern.FindStringSubmatch(token)[1])
				if number < 1 || number > count {
					return token
				}
				return "[Image #" + strconv.Itoa(offset+number) + "]"
			})
		}
		texts = append(texts, text)
		joined.Images = append(joined.Images, p.Images...)
	}
	joined.Text = strings.Join(texts, "\n\n")
	return joined
}

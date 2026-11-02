package ui

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"julia/internal/core"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/term"
	"golang.org/x/sys/unix"
)

// logWidth is the width laid out for an output that is not a terminal.
const logWidth = 120

// Transcript prints a headless run line by line, with the same components the
// conversation uses: a reply as Markdown under the spark, tool calls as pills,
// and notes and warnings as their usual rows.
//
// The two outputs keep their jobs. stdout carries the reply and stderr the work
// around it. A reply that does not go to a terminal stays the model's own text,
// so a pipe or a file gets what the model wrote. An output that is not a terminal
// gets no colors, and work that goes to one keeps what ran but not what it printed.
type Transcript struct {
	reply transcriptOutput
	work  transcriptOutput

	layout *model // lays out entries; holds no state of the run

	markdown markdownCache

	pending string   // reply text that does not yet make a complete block
	replied bool     // some of a verbatim reply has been written
	midLine bool     // a verbatim reply has not ended its line
	last    lastRows // what the last row printed belongs to

	running []*entry // calls that have started and not finished, oldest first
	titled  *entry   // the call whose title is the last row printed
}

// lastRows says what a Transcript printed last.
type lastRows int

const (
	noRows lastRows = iota
	replyRows
	workRows
)

// transcriptOutput is one of the run's two outputs.
type transcriptOutput struct {
	writer   io.Writer
	terminal bool
	width    func() int
}

// NewTranscript returns a Transcript that prints to the given outputs.
func NewTranscript(stdout, stderr *os.File) *Transcript {
	return newTranscript(outputFor(stdout), outputFor(stderr))
}

func newTranscript(reply, work transcriptOutput) *Transcript {
	return &Transcript{reply: reply, work: work, layout: &model{}}
}

func outputFor(file *os.File) transcriptOutput {
	writer := colorprofile.NewWriter(file, os.Environ())

	// The palette suits the interface's dark canvas, and a transcript paints no
	// canvas. On a light terminal, its colors would hide the text.
	if writer.Profile > colorprofile.ASCII && inForeground(file) && !lipgloss.HasDarkBackground(file, file) {
		writer.Profile = colorprofile.ASCII
	}

	return transcriptOutput{
		writer:   writer,
		terminal: term.IsTerminal(file.Fd()),
		width: func() int {
			width, _, err := term.GetSize(file.Fd())
			if err != nil || width <= 0 {
				return logWidth
			}
			return width
		},
	}
}

// inForeground reports whether this process owns the terminal. A run in the
// background must not ask it anything: changing its mode would stop the process,
// and the answer would go to the program in the foreground as typed input.
func inForeground(file *os.File) bool {
	owner, err := unix.IoctlGetInt(int(file.Fd()), unix.TIOCGPGRP)
	return err == nil && owner == unix.Getpgrp()
}

// Show prints what an event adds to the run. The caller serializes events.
func (t *Transcript) Show(event core.Event) {
	if event.Subagent != nil {
		t.showAgentEvent(event)
		return
	}

	switch event.Kind {
	case "text":
		t.showReplyText(event.Text)
	case "tool_start":
		t.startCall(event)
	case "tool_end":
		t.finishCall(event)
	case "status":
		t.printWork(entry{role: "note", text: event.Text})
	case "warning":
		t.printWork(entry{role: "warning", text: event.Text})
	}
}

// Finish ends the reply and prints the session's ID and token total.
func (t *Transcript) Finish(sessionID, tokens string) {
	t.closeReply()
	if t.last != noRows {
		fmt.Fprintln(t.work.writer)
	}
	label := violetStyle.Bold(true)
	fmt.Fprintln(t.work.writer, label.Render("Session:")+" "+creamStyle.Render(sanitize(sessionID)))
	fmt.Fprintln(t.work.writer, label.Render("Tokens:")+" "+creamStyle.Render(sanitize(tokens)))
}

// showAgentEvent shows a child's tool calls as progress under the role's name.
// A child's text is its report to the parent, not output.
func (t *Transcript) showAgentEvent(event core.Event) {
	if event.Kind != "tool_start" || event.Call == nil || isCardTool(event.Call) {
		return
	}
	t.printWork(entry{role: "tool", call: event.Call, done: true, label: event.Subagent.Role})
}

// startCall prints a call's title row at once, so a long command shows what is
// running. The row has no running mark: it stays in the scrollback after the
// call ends, where the mark would be wrong.
func (t *Transcript) startCall(event core.Event) {
	item := &entry{role: "tool", call: event.Call, text: event.Text, done: true}
	t.running = append(t.running, item)

	// A question or a review draws its own card when it ends; headless, that is
	// at once, and its open card would wait for an answer that cannot come.
	if isCardTool(event.Call) {
		return
	}

	t.printWork(*item)
	t.titled = item
}

// finishCall prints a call's result. The result goes straight under the title
// when that is still the last row. If other rows came between, or the call
// failed, the whole entry prints again, as the conversation sets it.
//
// Work that does not go to a terminal may end up in a log that outlives the run,
// and a result can hold a secret or private mail. There, a call that worked adds
// nothing to its title, and a call that failed adds only its mark.
func (t *Transcript) finishCall(event core.Event) {
	t.closeReply()

	item := t.takeRunning(event.Call)
	if item == nil {
		return
	}
	result := event.Result
	failed := result != nil && result.IsError
	if !t.work.terminal && !isCardTool(item.call) {
		if !failed {
			return
		}
		result = &core.ToolResult{IsError: true}
	}
	item.result, item.images = result, resultImages(result)

	underTitle := t.titled == item && !failed
	rows := t.layoutWork(*item, underTitle)
	if underTitle {
		_, rows, _ = strings.Cut(rows, "\n")
	}
	t.writeWork(rows)
}

// takeRunning removes and returns the running call an event names, or the latest
// one when it names none.
func (t *Transcript) takeRunning(call *core.ToolCall) *entry {
	for i := len(t.running) - 1; i >= 0; i-- {
		item := t.running[i]
		if item.isCall(call) {
			t.running = append(t.running[:i], t.running[i+1:]...)
			return item
		}
	}
	return nil
}

// isCardTool reports whether a call draws a card of its own instead of a pill.
func isCardTool(call *core.ToolCall) bool {
	return call != nil && (call.Name == core.QuestionTool || call.Name == core.ReviewTool)
}

func (t *Transcript) printWork(item entry) {
	t.closeReply()
	t.writeWork(t.layoutWork(item, false))
}

func (t *Transcript) layoutWork(item entry, resultBelow bool) string {
	in := column{width: t.work.width(), wordWrap: true, printed: true, resultBelow: resultBelow}
	return t.layout.layoutEntryIn(item, in).text
}

// writeWork prints rows to the work output, a blank row apart from a reply.
func (t *Transcript) writeWork(rows string) {
	t.titled = nil
	if rows == "" {
		return
	}
	if t.reply.terminal && t.last == replyRows {
		fmt.Fprintln(t.work.writer)
	}
	fmt.Fprintln(t.work.writer, rows)
	t.last = workRows
}

// showReplyText prints a reply as it streams. A terminal gets each Markdown block
// when it is complete; anything else gets the text as it arrives.
func (t *Transcript) showReplyText(text string) {
	if !t.reply.terminal {
		t.writeVerbatim(text)
		return
	}

	t.pending += text
	var complete string
	complete, t.pending = completeBlocks(t.pending)
	t.printReply(complete)
}

// writeVerbatim writes reply text as it is. Text that follows work is one blank
// line after the reply before it, whatever separator the provider sent.
func (t *Transcript) writeVerbatim(text string) {
	text = sanitize(text)
	if t.last == workRows {
		text = strings.TrimLeft(text, "\n")
		if text != "" && t.replied {
			fmt.Fprintln(t.reply.writer)
		}
	}
	if text == "" {
		return
	}

	fmt.Fprint(t.reply.writer, text)
	t.midLine = !strings.HasSuffix(text, "\n")
	t.replied, t.last = true, replyRows
	t.titled = nil
}

// closeReply prints what remains of a reply, so that the next row starts on a
// line of its own.
func (t *Transcript) closeReply() {
	if t.midLine {
		fmt.Fprintln(t.reply.writer)
		t.midLine = false
	}
	t.printReply(t.pending)
	t.pending = ""
}

// printReply sets finished blocks of a reply. Only the reply's first block has
// the spark.
func (t *Transcript) printReply(text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	if t.last != noRows {
		fmt.Fprintln(t.reply.writer)
	}

	item := entry{role: "assistant", text: text, noSpark: t.last == replyRows}
	fmt.Fprintln(t.reply.writer, t.layout.layoutEntryIn(item, t.replyColumn()).text)

	t.titled = nil
	t.last = replyRows
}

func (t *Transcript) replyColumn() column {
	width := t.reply.width()
	return column{width: width, markdown: t.markdown.at(width), wordWrap: true, printed: true}
}

// listItem matches the first line of a Markdown list item.
var listItem = regexp.MustCompile(`^(?:[-*+]|\d{1,9}[.)])(?:\s|$)`)

// completeBlocks splits text after the last Markdown block that cannot change
// any more: a blank line ended it, outside any code fence, and the line after
// that starts something new. A list stays whole, so that it renders as one list
// even when blank lines part its items.
func completeBlocks(text string) (complete, rest string) {
	var fence string
	afterBlank := false
	split, start := 0, 0

	for {
		length := strings.IndexByte(text[start:], '\n')
		if length < 0 {
			return text[:split], text[split:] // an unfinished line decides nothing
		}
		line := text[start : start+length]

		switch {
		case fence != "":
			if closesFence(line, fence) {
				fence = ""
			}
		case strings.TrimSpace(line) == "":
			afterBlank = true
		default:
			if afterBlank && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") && !listItem.MatchString(line) {
				split = start
			}
			afterBlank = false
			if _, marker, _, opens := openingFence(line); opens {
				fence = marker
			}
		}

		start += length + 1
	}
}

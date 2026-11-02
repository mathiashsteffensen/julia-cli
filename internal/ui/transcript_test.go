package ui

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"julia/internal/core"

	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
)

// transcriptFixture holds a Transcript and what it printed to each output.
type transcriptFixture struct {
	*Transcript
	reply, work *bytes.Buffer
}

// piped is a run whose two outputs are files or pipes.
func piped() transcriptFixture {
	return fixtureWith(false, colorprofile.NoTTY)
}

// onTerminal is a run whose two outputs are a color terminal.
func onTerminal() transcriptFixture {
	return fixtureWith(true, colorprofile.TrueColor)
}

func fixtureWith(terminal bool, profile colorprofile.Profile) transcriptFixture {
	reply, work := &bytes.Buffer{}, &bytes.Buffer{}
	output := func(buffer *bytes.Buffer) transcriptOutput {
		return transcriptOutput{
			writer:   &colorprofile.Writer{Forward: buffer, Profile: profile},
			terminal: terminal,
			width:    func() int { return 80 },
		}
	}
	return transcriptFixture{newTranscript(output(reply), output(work)), reply, work}
}

// sharing sends both outputs to one buffer, as one terminal or 2>&1 does.
func (f transcriptFixture) sharing() *bytes.Buffer {
	shared := &bytes.Buffer{}
	profile := f.Transcript.work.writer.(*colorprofile.Writer).Profile
	writer := &colorprofile.Writer{Forward: shared, Profile: profile}
	f.Transcript.reply.writer, f.Transcript.work.writer = writer, writer
	return shared
}

func (f transcriptFixture) plainReply() string { return ansi.Strip(f.reply.String()) }

// rows is an output's rows with runs of spaces closed up, so a test pins what
// each row says and their order, not the padding of a pill.
func rows(output string) []string {
	var out []string
	for _, line := range strings.Split(strings.TrimSuffix(ansi.Strip(output), "\n"), "\n") {
		out = append(out, strings.Join(strings.Fields(line), " "))
	}
	return out
}

func requireRows(t *testing.T, output *bytes.Buffer, want ...string) {
	t.Helper()
	if got := rows(output.String()); !slices.Equal(got, want) {
		t.Fatalf("rows:\n%q\nwant:\n%q", got, want)
	}
}

func bashCall(id, command string) *core.ToolCall {
	arguments, _ := json.Marshal(map[string]string{"command": command})
	return &core.ToolCall{ID: id, Name: "Bash", Arguments: arguments}
}

func started(call *core.ToolCall) core.Event {
	return core.Event{Kind: "tool_start", Call: call}
}

func ended(call *core.ToolCall, output string) core.Event {
	return core.Event{Kind: "tool_end", Call: call, Result: &core.ToolResult{Content: output}}
}

func TestPipedReplyStaysTheModelsTextWithoutColor(t *testing.T) {
	f := piped()
	call := bashCall("1", "git status")

	f.Show(core.Event{Kind: "status", Text: "Working"})
	f.Show(core.Event{Kind: "text", Text: "# Plan\n\n"})
	f.Show(core.Event{Kind: "text", Text: "Check **this**"})
	f.Show(started(call))
	f.Show(ended(call, "clean"))
	f.Show(core.Event{Kind: "text", Text: "Done."})
	f.Finish("session-1", "1 in · 2 out")

	if got, want := f.reply.String(), "# Plan\n\nCheck **this**\n\nDone.\n"; got != want {
		t.Fatalf("reply = %q, want %q", got, want)
	}
	if strings.Contains(f.work.String(), "\x1b") {
		t.Fatalf("a pipe must get no escape sequences: %q", f.work.String())
	}
	requireRows(t, f.work, "· Working", "Bash git status", "", "Session: session-1", "Tokens: 1 in · 2 out")
}

// The API provider sends a blank line between its steps, and Claude sends
// nothing. Either way, the text after a tool call is one blank line apart.
func TestPipedReplyPartsStepsWithOneBlankLine(t *testing.T) {
	for _, separator := range []string{"", "\n\n"} {
		f := piped()
		call := bashCall("1", "ls")

		f.Show(core.Event{Kind: "text", Text: "Before."})
		f.Show(started(call))
		f.Show(ended(call, "a"))
		f.Show(core.Event{Kind: "text", Text: separator})
		f.Show(core.Event{Kind: "text", Text: "After."})
		f.Finish("id", "tokens")

		if got, want := f.reply.String(), "Before.\n\nAfter.\n"; got != want {
			t.Fatalf("separator %q: reply = %q, want %q", separator, got, want)
		}
	}
}

func TestPipedReplyKeepsTheModelsOwnBlankLines(t *testing.T) {
	f := piped()
	for _, chunk := range []string{"One.\n", "\n", "\n", "Two.\n"} {
		f.Show(core.Event{Kind: "text", Text: chunk})
	}
	f.Finish("id", "tokens")

	if got, want := f.reply.String(), "One.\n\n\nTwo.\n"; got != want {
		t.Fatalf("reply = %q, want %q", got, want)
	}
}

func TestPipedReplyDoesNotOpenWithABlankLine(t *testing.T) {
	f := piped()
	f.Show(core.Event{Kind: "status", Text: "Working"})
	f.Show(core.Event{Kind: "text", Text: "Hello."})
	f.Finish("id", "tokens")

	if got, want := f.reply.String(), "Hello.\n"; got != want {
		t.Fatalf("reply = %q, want %q", got, want)
	}
}

func TestPipedReplyRemovesControlSequences(t *testing.T) {
	f := piped()
	f.Show(core.Event{Kind: "text", Text: "safe\x1b]52;c;ZXZpbA==\x07 text\x07"})
	f.Finish("id", "tokens")
	if got, want := f.reply.String(), "safe text\n"; got != want {
		t.Fatalf("reply = %q, want %q", got, want)
	}
}

func TestTerminalReplyIsSetAsInTheConversation(t *testing.T) {
	f := onTerminal()
	f.Show(core.Event{Kind: "text", Text: "# Plan\n\nCheck **this** first.\n"})
	f.Finish("id", "tokens")

	reply := f.plainReply()
	if !strings.HasPrefix(reply, "✦ ") {
		t.Fatalf("a reply opens with the spark:\n%s", reply)
	}
	if strings.Contains(reply, "#") || strings.Contains(reply, "**") {
		t.Fatalf("the Markdown must be rendered:\n%s", reply)
	}
	if !strings.Contains(f.reply.String(), "\x1b[") {
		t.Fatalf("a terminal must get colors: %q", f.reply.String())
	}
}

func TestTerminalReplyPrintsEachBlockWhenItIsComplete(t *testing.T) {
	f := onTerminal()

	f.Show(core.Event{Kind: "text", Text: "First paragraph.\n\n"})
	if f.reply.Len() != 0 {
		t.Fatalf("the block may still go on: %q", f.reply.String())
	}

	f.Show(core.Event{Kind: "text", Text: "Second para"})
	if f.reply.Len() != 0 {
		t.Fatalf("an unfinished line decides nothing: %q", f.reply.String())
	}

	f.Show(core.Event{Kind: "text", Text: "graph.\n"})
	if !strings.Contains(f.plainReply(), "First paragraph.") || strings.Contains(f.plainReply(), "Second") {
		t.Fatalf("only the first block is complete:\n%s", f.plainReply())
	}

	f.Finish("id", "tokens")
	reply := f.plainReply()
	if !strings.Contains(reply, "\n\n  Second paragraph.") {
		t.Fatalf("the last block prints at the end, a blank row apart and aligned with the first:\n%s", reply)
	}
	if strings.Count(reply, "✦") != 1 {
		t.Fatalf("one reply has one spark:\n%s", reply)
	}
}

func TestCodeCardOffersNoCopyInTheScrollback(t *testing.T) {
	f := onTerminal()
	f.Show(core.Event{Kind: "text", Text: "```sh\necho hello\n```\n"})
	f.Finish("id", "tokens")

	reply := f.plainReply()
	if !strings.Contains(reply, "echo hello") {
		t.Fatalf("the code must show:\n%s", reply)
	}
	if strings.Contains(reply, "copy") {
		t.Fatalf("nothing can be clicked in the scrollback:\n%s", reply)
	}
}

func TestReplyEndsItsLineBeforeTheNextRow(t *testing.T) {
	call := bashCall("1", "git status")

	// The case that used to print "...the repo.  › Bash" on one row.
	terminal := onTerminal()
	shared := terminal.sharing()
	terminal.Show(core.Event{Kind: "text", Text: "I'll check the repo."})
	terminal.Show(started(call))
	requireRows(t, shared, "✦ I'll check the repo.", "", "Bash git status")

	pipe := piped()
	shared = pipe.sharing()
	pipe.Show(core.Event{Kind: "text", Text: "I'll check the repo."})
	pipe.Show(started(call))
	requireRows(t, shared, "I'll check the repo.", "Bash git status")
}

func TestResultGoesUnderItsTitle(t *testing.T) {
	f := onTerminal()
	call := bashCall("1", "git status")

	f.Show(started(call))
	requireRows(t, f.work, "Bash git status")

	f.Show(ended(call, "M main.go\n?? new.go"))
	requireRows(t, f.work, "Bash git status", "M main.go")
}

func TestWebFetchTitleNamesItsURL(t *testing.T) {
	f := piped()
	arguments, _ := json.Marshal(map[string]string{"url": "https://example.com/docs"})

	f.Show(started(&core.ToolCall{ID: "1", Name: "WebFetch", Arguments: arguments}))
	requireRows(t, f.work, "WebFetch https://example.com/docs")
}

func TestCallWithoutOutputPrintsNoFurtherRow(t *testing.T) {
	f := onTerminal()
	call := bashCall("1", "git status --short")

	f.Show(started(call))
	f.Show(ended(call, ""))
	requireRows(t, f.work, "Bash git status --short")
}

func TestResultAfterOtherRowsNamesItsCall(t *testing.T) {
	f := onTerminal()
	call := bashCall("1", "curl example.com")

	f.Show(started(call))
	f.Show(core.Event{Kind: "status", Text: "Automatic review in progress…"})
	f.Show(ended(call, "ok"))

	requireRows(t, f.work, "Bash curl example.com", "· Automatic review in progress…", "Bash curl example.com · ok")
}

func TestParallelResultsNameTheirCalls(t *testing.T) {
	f := onTerminal()
	first, second := bashCall("1", "first"), bashCall("2", "second")

	f.Show(started(first))
	f.Show(started(second))
	f.Show(ended(first, "one"))
	f.Show(ended(second, "two"))

	requireRows(t, f.work, "Bash first", "Bash second", "Bash first · one", "Bash second · two")
}

func TestFailedCallIsMarked(t *testing.T) {
	f := onTerminal()
	call := bashCall("1", "go test")

	f.Show(started(call))
	f.Show(core.Event{Kind: "tool_end", Call: call, Result: &core.ToolResult{Content: "exit status 1", IsError: true}})

	requireRows(t, f.work, "Bash go test", "! Bash go test · exit status 1")
}

// A log keeps what ran. Output can hold a secret, and a log outlives the run.
func TestLogLeavesOutToolOutput(t *testing.T) {
	f := piped()
	worked, failed := bashCall("1", "printenv TOKEN"), bashCall("2", "cat secrets")

	f.Show(started(worked))
	f.Show(ended(worked, "sk-worked"))
	f.Show(started(failed))
	f.Show(core.Event{Kind: "tool_end", Call: failed, Result: &core.ToolResult{Content: "sk-failed", IsError: true}})

	requireRows(t, f.work, "Bash printenv TOKEN", "Bash cat secrets", "! Bash cat secrets")
}

// Reply text printed after a title means the title is no longer the last row.
func TestResultAfterPipedReplyTextNamesItsCall(t *testing.T) {
	f := piped()
	f.Transcript.work.terminal = true
	call := bashCall("1", "git status")

	f.Show(started(call))
	f.Show(core.Event{Kind: "text", Text: "Meanwhile."})
	f.Show(ended(call, "M main.go"))

	requireRows(t, f.work, "Bash git status", "Bash git status · M main.go")
}

func TestReplayedImageWithoutPixelsHasNoCaption(t *testing.T) {
	f := onTerminal()
	call := bashCall("1", "shot")

	f.Show(started(call))
	f.Show(core.Event{Kind: "tool_end", Call: call, Result: &core.ToolResult{Content: "ok", Images: []core.Image{{}}}})

	requireRows(t, f.work, "Bash shot", "ok")
}

// A light terminal runs on the ASCII profile: no color can hide the text there,
// and the weights still set a heading apart.
func TestLightTerminalProfileKeepsWeightsWithoutColor(t *testing.T) {
	f := fixtureWith(true, colorprofile.ASCII)
	call := bashCall("1", "ls")

	f.Show(core.Event{Kind: "text", Text: "# Plan\n\nCheck this first.\n"})
	f.Show(started(call))
	f.Show(ended(call, "a"))
	f.Finish("id", "tokens")

	printed := f.reply.String() + f.work.String()
	if strings.Contains(printed, "38;") || strings.Contains(printed, "48;") {
		t.Fatalf("a color got through: %q", printed)
	}
	if !strings.Contains(printed, "\x1b[1m") {
		t.Fatalf("the weights must stay: %q", printed)
	}
	if plain := ansi.Strip(printed); !strings.Contains(plain, "Check this first.") || !strings.Contains(plain, "Bash") {
		t.Fatalf("the text must stay:\n%s", plain)
	}
}

func TestSubagentCallWithoutTargetShowsOnlyTheRole(t *testing.T) {
	f := piped()
	agent := &core.Subagent{ID: "child", Role: "scout"}

	f.Show(core.Event{Kind: "tool_start", Call: &core.ToolCall{ID: "1", Name: "List", Arguments: json.RawMessage(`{}`)}, Subagent: agent})

	requireRows(t, f.work, "List scout")
}

func TestSubagentCallsShowUnderTheRole(t *testing.T) {
	f := piped()
	agent := &core.Subagent{ID: "child", Role: "scout"}
	call := bashCall("1", "ls")

	f.Show(core.Event{Kind: "text", Text: "the child's report", Subagent: agent})
	f.Show(core.Event{Kind: "tool_start", Call: call, Subagent: agent})
	f.Show(core.Event{Kind: "tool_end", Call: call, Result: &core.ToolResult{Content: "listing"}, Subagent: agent})

	if f.reply.Len() != 0 {
		t.Fatalf("a child's text is not output: %q", f.reply.String())
	}
	requireRows(t, f.work, "Bash scout · ls")
}

func TestQuestionWaitsForItsResult(t *testing.T) {
	f := piped()
	arguments, _ := json.Marshal(core.Question{Text: "Which one?"})
	call := &core.ToolCall{ID: "q", Name: core.QuestionTool, Arguments: arguments}

	f.Show(started(call))
	if f.work.Len() != 0 {
		t.Fatalf("nobody can answer a headless question: %q", f.work.String())
	}

	f.Show(core.Event{Kind: "tool_end", Call: call, Result: &core.ToolResult{Content: "skipped", IsError: true}})
	if !strings.Contains(f.work.String(), "Which one?") {
		t.Fatalf("the card shows the question whole:\n%s", f.work.String())
	}
}

func TestLongNoteHasOneMark(t *testing.T) {
	f := piped()
	f.Show(core.Event{Kind: "status", Text: strings.TrimSpace(strings.Repeat("Automatic review approved ", 8))})

	lines := strings.Split(strings.TrimSpace(f.work.String()), "\n")
	if len(lines) < 2 {
		t.Fatalf("the note must wrap: %q", lines)
	}
	if !strings.HasPrefix(lines[0], "· ") || !strings.HasPrefix(lines[1], "  ") {
		t.Fatalf("only the first row has the mark: %q", lines)
	}
}

func TestWarningIsMarked(t *testing.T) {
	f := piped()
	f.Show(core.Event{Kind: "warning", Text: "Careful."})
	requireRows(t, f.work, "! Careful.")
}

func TestCompleteBlocks(t *testing.T) {
	cases := []struct {
		name, text, complete string
	}{
		{"one unfinished paragraph", "Some text", ""},
		{"one finished line", "Some text\n", ""},
		{"a block with nothing after it", "Some text\n\n", ""},
		{"the next line is unfinished", "Some text\n\nMore", ""},
		{"the next line is finished", "Some text\n\nMore\n", "Some text\n\n"},
		{"the last complete block wins", "One\n\nTwo\n\nThree\n", "One\n\nTwo\n\n"},
		{"a list stays whole", "- a\n\n- b\n\n1. c\n", ""},
		{"a list ends at a paragraph", "- a\n\n- b\n\nAfter\n", "- a\n\n- b\n\n"},
		{"an indented line stays with its block", "- a\n\n  more of a\n", ""},
		{"a rule is not a list item", "One\n\n---\n", "One\n\n"},
		{"bold is not a list item", "One\n\n**Two**\n", "One\n\n"},
		{"a number is not a list item", "One\n\n100 tests pass\n", "One\n\n"},
		{"a fence starts a block", "One\n\n```go\n", "One\n\n"},
		{"an open fence hides blank lines", "```go\na\n\nb\n\nc\n", ""},
		{"a closed fence is a block", "```go\na\n\nb\n```\n\nAfter\n", "```go\na\n\nb\n```\n\n"},
		{"a longer fence needs a longer close", "````\n```\n\nb\n```\n\nc\n", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			complete, rest := completeBlocks(c.text)
			if complete != c.complete || complete+rest != c.text {
				t.Fatalf("completeBlocks(%q) = %q, %q; want complete %q", c.text, complete, rest, c.complete)
			}
		})
	}
}

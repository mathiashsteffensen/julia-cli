package main

import (
	"context"
	"encoding/json"
	"io"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"julia/internal/core"
)

// Generous ceilings for what a record may hold: the tool already bounds both
// fields, so these only protect against oddly large saved sessions.
const (
	maxContextQuestion = 4 << 10
	maxContextAnswer   = 24 << 10
)

// taskContext is what the action reviewer treats as the user's authorization:
// every user message, plus the user's own answers to questions the agent asked
// and their decisions on the findings of a review.
type taskContext struct {
	mu      sync.Mutex
	entries []core.TaskEntry
}

// newTaskContext replays a saved conversation and then adds the current prompt.
func newTaskContext(messages []core.Message, prompt string) *taskContext {
	t := &taskContext{}
	// A question is answered by the tool message that follows the assistant
	// message which asked it, once. Matching on the call ID alone would let a
	// later tool output that reuses an ID pass as the user's reply.
	var asked map[string]core.ToolCall
	for _, message := range messages {
		switch message.Role {
		case "user":
			asked = nil
			t.addUser(message.Content)
		case "assistant":
			asked = questionsAsked(message.ToolCalls)
		case "tool":
			if call, ok := asked[message.ToolCallID]; ok {
				delete(asked, message.ToolCallID)
				t.entries = append(t.entries, answerEntries(call, message.Content)...)
			}
		}
	}
	t.addUser(prompt)
	return t
}

// questionsAsked indexes the question and review calls of one assistant
// message by call ID, dropping empty IDs and any ID used more than once.
func questionsAsked(calls []core.ToolCall) map[string]core.ToolCall {
	asked := make(map[string]core.ToolCall)
	counts := make(map[string]int)
	for _, call := range calls {
		counts[call.ID]++
		if call.ID != "" && (call.Name == core.QuestionTool || call.Name == core.ReviewTool) {
			asked[call.ID] = call
		}
	}
	for id, n := range counts {
		if n > 1 {
			delete(asked, id)
		}
	}
	return asked
}

func (t *taskContext) addUser(text string) {
	if strings.TrimSpace(text) != "" {
		t.entries = append(t.entries, core.TaskEntry{UserMessage: text})
	}
}

// steered records a message the user sent while the turn ran and returns
// the updated context.
func (t *taskContext) steered(text string) string {
	t.mu.Lock()
	t.addUser(text)
	t.mu.Unlock()
	return t.String()
}

// steeredTask is a turn's Steering that records each message the backend
// takes as the user's own.
type steeredTask struct {
	core.Steering
	record func(string)
}

func (s steeredTask) Take() []core.Prompt {
	taken := s.Steering.Take()
	for _, p := range taken {
		s.record(p.Text)
	}
	return taken
}

func (t *taskContext) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return core.EncodeTask(t.entries)
}

// answered records a finished question or review call and returns the updated
// context. It reports false for any other call, a failed call, or one the user
// skipped or left without a decision.
func (t *taskContext) answered(call core.ToolCall, result core.ToolResult) (string, bool) {
	if result.IsError {
		return "", false
	}
	entries := answerEntries(call, result.Content)
	if len(entries) == 0 {
		return "", false
	}
	t.mu.Lock()
	t.entries = append(t.entries, entries...)
	t.mu.Unlock()
	return t.String(), true
}

// answerEntries records what the user said in reply to one question or review call.
func answerEntries(call core.ToolCall, content string) []core.TaskEntry {
	switch call.Name {
	case core.QuestionTool:
		var question core.Question
		if json.Unmarshal(call.Arguments, &question) != nil {
			return nil
		}
		if entry, ok := answerEntry(question, content); ok {
			return []core.TaskEntry{entry}
		}
	case core.ReviewTool:
		var review core.Review
		if json.Unmarshal(call.Arguments, &review) != nil {
			return nil
		}
		return reviewEntries(review, content)
	}
	return nil
}

// triageLabels name each decision as the card offered it to the user.
var triageLabels = map[string]string{core.TriageFix: "Fix", core.TriageIgnore: "Ignore", core.TriageFixed: "I fixed it"}

// reviewEntries records the user's decision on each finding they decided. The
// finding is the agent's; a picked decision is the user's choice among those
// the card offered, and a reply is the user's own words.
func reviewEntries(review core.Review, content string) []core.TaskEntry {
	var result core.ReviewResult
	decoder := json.NewDecoder(strings.NewReader(content))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&result) != nil || decoder.Decode(new(any)) != io.EOF || result.Skipped {
		return nil
	}
	var entries []core.TaskEntry
	for _, decision := range result.Decisions {
		if decision.Finding < 1 || decision.Finding > len(review.Findings) {
			continue
		}
		finding := review.Findings[decision.Finding-1]
		ask := "Code review finding " + strconv.Itoa(decision.Finding) + " [" + finding.Severity + "] " + finding.Title
		if finding.Location != "" {
			ask += " (" + finding.Location + ")"
		}
		if finding.Detail != "" {
			ask += ": " + finding.Detail
		}
		entry := core.TaskEntry{AssistantQuestion: clip(strings.TrimSpace(core.VisibleText(ask+"\nFix, ignore, already fixed by you, or reply?", true)), maxContextQuestion)}
		if label, ok := triageLabels[decision.Decision]; ok {
			entry.UserSelectedOption = label
		} else if reply := core.VisibleText(decision.Reply, true); decision.Decision == core.TriageReply && strings.TrimSpace(reply) != "" {
			entry.UserTypedAnswer = clip(reply, maxContextAnswer)
		} else {
			continue
		}
		entries = append(entries, entry)
	}
	return entries
}

// answerEntry records one answered question. The question and any option text
// are written by the agent; only a typed answer is the user's own words, while a
// picked option is the user's choice among things the agent offered. The fields
// say which, and the reviewer policy says how much each one authorizes.
func answerEntry(question core.Question, content string) (core.TaskEntry, bool) {
	var result core.QuestionResult
	decoder := json.NewDecoder(strings.NewReader(content))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&result) != nil || decoder.Decode(new(any)) != io.EOF || result.Skipped || (result.Source != "option" && result.Source != "freeform") {
		return core.TaskEntry{}, false
	}
	answer := core.VisibleText(result.Answer, true)
	if strings.TrimSpace(answer) == "" {
		return core.TaskEntry{}, false
	}
	entry := core.TaskEntry{AssistantQuestion: clip(strings.TrimSpace(core.VisibleText(question.Text, true)), maxContextQuestion)}
	if result.Source == "option" {
		entry.UserSelectedOption = clip(answer, maxContextAnswer)
	} else {
		entry.UserTypedAnswer = clip(answer, maxContextAnswer)
	}
	return entry, true
}

func clip(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	for limit > 0 && !utf8.RuneStart(text[limit]) {
		limit--
	}
	return text[:limit] + "…"
}

// withAnswerContext reports each answered question to record, so approvals later
// in the same turn are judged with the user's answer in view.
func withAnswerContext(base core.ToolExecutor, task *taskContext, record func(string)) core.ToolExecutor {
	return &answerObserver{ToolExecutor: base, task: task, record: record}
}

type answerObserver struct {
	core.ToolExecutor
	task   *taskContext
	record func(string)
}

func (o *answerObserver) Execute(ctx context.Context, call core.ToolCall) core.ToolResult {
	result := o.ToolExecutor.Execute(ctx, call)
	if updated, ok := o.task.answered(call, result); ok {
		o.record(updated)
	}
	return result
}

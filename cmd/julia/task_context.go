package main

import (
	"context"
	"encoding/json"
	"io"
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
// every user message, plus the user's own answers to questions the agent asked.
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
	var asked map[string]core.Question
	for _, message := range messages {
		switch message.Role {
		case "user":
			asked = nil
			t.addUser(message.Content)
		case "assistant":
			asked = questionsAsked(message.ToolCalls)
		case "tool":
			if question, ok := asked[message.ToolCallID]; ok {
				delete(asked, message.ToolCallID)
				if entry, ok := answerEntry(question, message.Content); ok {
					t.entries = append(t.entries, entry)
				}
			}
		}
	}
	t.addUser(prompt)
	return t
}

// questionsAsked indexes the question calls of one assistant message by call ID,
// dropping empty IDs and any ID used more than once.
func questionsAsked(calls []core.ToolCall) map[string]core.Question {
	asked := make(map[string]core.Question)
	counts := make(map[string]int)
	for _, call := range calls {
		counts[call.ID]++
		var question core.Question
		if call.ID != "" && call.Name == core.QuestionTool && json.Unmarshal(call.Arguments, &question) == nil {
			asked[call.ID] = question
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

func (t *taskContext) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return core.EncodeTask(t.entries)
}

// answered records a finished question call and returns the updated context.
// It reports false for any other call, a failed call, or a skipped question.
func (t *taskContext) answered(call core.ToolCall, result core.ToolResult) (string, bool) {
	if call.Name != core.QuestionTool || result.IsError {
		return "", false
	}
	var question core.Question
	if json.Unmarshal(call.Arguments, &question) != nil {
		return "", false
	}
	entry, ok := answerEntry(question, result.Content)
	if !ok {
		return "", false
	}
	t.mu.Lock()
	t.entries = append(t.entries, entry)
	t.mu.Unlock()
	return t.String(), true
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

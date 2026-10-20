package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"julia/internal/core"
)

const (
	maxQuestionBytes    = 1000
	minQuestionOptions  = 2
	maxQuestionOptions  = 8
	maxOptionLabelBytes = 120
	maxOptionDescBytes  = 400
	maxAnswerBytes      = 16 << 10
)

// QuestionDefinition describes the interactive clarification tool. It is not a
// workspace tool, so it is registered by WithQuestions rather than Definitions;
// headless runs register it too, with an asker that skips at once.
func QuestionDefinition() core.ToolDefinition {
	option := schema(map[string]any{
		"label":       str("Short choice text the user sees and picks, at most 120 bytes. Unique among the options."),
		"description": str("Optional one-sentence trade-off or consequence, at most 400 bytes."),
	}, "label")
	return core.ToolDefinition{
		Name: core.QuestionTool,
		Description: "Ask the user one question and wait for the reply. Use it only when you are blocked on a decision that is genuinely theirs " +
			"(a preference, a missing requirement, a choice between materially different approaches) and the request, the code, and sensible defaults cannot settle it. " +
			"Do not use it to ask permission for tools or routine steps, to confirm work you can verify yourself, or to report progress. One focused question per call. " +
			"Pass options (2–8 distinct choices, each with a short label and an optional one-sentence description) when a few answers are likely: the user can pick one or write their own answer. " +
			"Put your recommended choice first and end its label with \" (Recommended)\". Omit options for an open-ended question. " +
			"The reply is the user's own answer and direction for this task. The user may skip the question; then continue on your best judgment and state the assumption you made.",
		Parameters: schema(map[string]any{
			"question": str("The question, self-contained and at most 1000 bytes. State the decision and its context briefly and end with the actual ask; put long background in a normal message first. On small terminals a long question is shortened in the middle, so keep the decision in the first lines and the ask last."),
			"options":  map[string]any{"type": "array", "minItems": minQuestionOptions, "maxItems": maxQuestionOptions, "items": option, "description": "Predefined answers. Omit for a free-form question; the user can always write their own answer instead."},
		}, "question"),
	}
}

// WithQuestions adds the interactive question tool outside Executor's lock, so a
// slow human never blocks workspace tools. Wrap the outermost executor: tools
// handed to subagents come from the base executor and so cannot ask the user.
// A nil ask returns base unchanged, leaving the tool unregistered.
func WithQuestions(base core.ToolExecutor, ask core.Asker) core.ToolExecutor {
	if ask == nil {
		return base
	}
	return WithInteractions(base, ask, nil)
}

type questionExecutor struct {
	base   core.ToolExecutor
	ask    core.Asker
	triage core.Triager
	slot   chan struct{}
}

func (e *questionExecutor) Definitions() []core.ToolDefinition {
	definitions := append([]core.ToolDefinition(nil), e.base.Definitions()...)
	if e.ask != nil {
		definitions = append(definitions, QuestionDefinition())
	}
	if e.triage != nil {
		definitions = append(definitions, ReviewDefinition())
	}
	return definitions
}

func (e *questionExecutor) Execute(ctx context.Context, call core.ToolCall) core.ToolResult {
	if !(call.Name == core.QuestionTool && e.ask != nil) && !(call.Name == core.ReviewTool && e.triage != nil) {
		return e.base.Execute(ctx, call)
	}
	// One question on screen at a time, even if a provider issues tool calls
	// concurrently. A call waiting its turn still honors its own cancellation.
	select {
	case e.slot <- struct{}{}:
		defer func() { <-e.slot }()
	case <-ctx.Done():
		return failure(ctx.Err())
	}
	if err := ctx.Err(); err != nil {
		return failure(err)
	}
	if call.Name == core.ReviewTool {
		return e.review(ctx, call)
	}
	question, err := parseQuestion(call.Arguments)
	if err != nil {
		return failure(err)
	}
	answer, err := e.ask(ctx, question)
	if err != nil {
		return failure(fmt.Errorf("ask user: %w", err))
	}
	result := core.QuestionResult{}
	switch {
	case answer.Skipped:
		result.Skipped = true
		result.Note = "The question went unanswered: the user skipped it or is not available. Do not ask it again. Continue on your best judgment and state the assumption you made, or stop and explain what you need."
	case answer.Option != 0:
		// The label comes from the validated question, never from the asker.
		if answer.Option < 1 || answer.Option > len(question.Options) {
			return failure(fmt.Errorf("asker returned option %d of %d", answer.Option, len(question.Options)))
		}
		result.Answer, result.Source, result.Option = question.Options[answer.Option-1].Label, "option", answer.Option
	default:
		text := strings.TrimSpace(strings.ToValidUTF8(answer.Text, ""))
		if text == "" {
			return failure(errors.New("asker returned an empty answer"))
		}
		if len(text) > maxAnswerBytes {
			text = strings.ToValidUTF8(text[:maxAnswerBytes], "") + "\n[answer truncated]"
		}
		result.Answer, result.Source = text, "freeform"
	}
	data, err := encodeQuestionResult(result)
	if err != nil {
		return failure(err)
	}
	return core.ToolResult{Content: data}
}

// encodeQuestionResult keeps the JSON within the tool output limit. Escaping can
// expand a legal-sized answer several times over (control characters become
// \u00XX), so an oversized encoding gets a shorter answer rather than failing.
func encodeQuestionResult(result core.QuestionResult) (string, error) {
	const marker = "\n[answer truncated]"
	text := result.Answer
	for {
		var out bytes.Buffer
		encoder := json.NewEncoder(&out)
		encoder.SetEscapeHTML(false)
		if err := encoder.Encode(result); err != nil {
			return "", err
		}
		data := strings.TrimSuffix(out.String(), "\n")
		if len(data) <= maxOutput || len(text) < 2 {
			return data, nil
		}
		text = strings.ToValidUTF8(text[:len(text)/2], "")
		result.Answer = text + marker
	}
}

// parseQuestion validates the model's arguments and normalizes whitespace in the
// single-line fields so labels cannot break the picker layout.
func parseQuestion(raw json.RawMessage) (core.Question, error) {
	var question core.Question
	// The JSON decoder would silently turn invalid bytes into U+FFFD.
	if !utf8.Valid(raw) {
		return question, errors.New("arguments must be valid UTF-8")
	}
	if err := requireFields(raw, "question"); err != nil {
		return question, err
	}
	if err := decode(raw, &question); err != nil {
		return question, err
	}
	var wire struct {
		Options []json.RawMessage `json:"options"`
	}
	_ = json.Unmarshal(raw, &wire)
	for i, option := range wire.Options {
		if err := requireFields(option, "label"); err != nil {
			return question, fmt.Errorf("option %d: %w", i+1, err)
		}
	}
	// Hidden characters would let text the user cannot see ride along into their
	// answer, so everything the card shows is reduced to what a reader can see.
	if strings.ContainsRune(question.Text, 0) {
		return question, errors.New("question must not contain NUL")
	}
	question.Text = strings.TrimSpace(core.VisibleText(question.Text, true))
	if !core.HasVisibleText(question.Text) || len(question.Text) > maxQuestionBytes {
		return question, fmt.Errorf("question must be nonempty UTF-8 text of at most %d bytes", maxQuestionBytes)
	}
	if len(question.Options) == 0 {
		question.Options = nil
		return question, nil
	}
	if len(question.Options) < minQuestionOptions || len(question.Options) > maxQuestionOptions {
		return question, fmt.Errorf("provide between %d and %d options, or omit options for a free-form question", minQuestionOptions, maxQuestionOptions)
	}
	seen := make(map[string]bool, len(question.Options))
	for i := range question.Options {
		option := &question.Options[i]
		if strings.ContainsRune(option.Label, 0) || strings.ContainsRune(option.Description, 0) {
			return question, fmt.Errorf("option %d: text must not contain NUL", i+1)
		}
		option.Label = strings.Join(strings.Fields(core.VisibleText(option.Label, false)), " ")
		option.Description = strings.Join(strings.Fields(core.VisibleText(option.Description, false)), " ")
		if !core.HasVisibleText(option.Label) || len(option.Label) > maxOptionLabelBytes {
			return question, fmt.Errorf("option %d: label must be nonempty UTF-8 text of at most %d bytes", i+1, maxOptionLabelBytes)
		}
		if len(option.Description) > maxOptionDescBytes {
			return question, fmt.Errorf("option %d: description must be UTF-8 text of at most %d bytes", i+1, maxOptionDescBytes)
		}
		key := strings.ToLower(option.Label)
		if seen[key] {
			return question, fmt.Errorf("option %d: duplicate label %q", i+1, option.Label)
		}
		seen[key] = true
	}
	return question, nil
}

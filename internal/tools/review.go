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
	maxFindings        = 25
	maxSummaryBytes    = 1000
	maxFindingTitle    = 200
	maxFindingLocation = 300
	maxFindingDetail   = 2000
	maxReplyBytes      = 4 << 10
)

// reviewSeverities are the levels the reviewer role reports, most severe first.
var reviewSeverities = []string{"P0", "P1", "P2"}

// ReviewDefinition describes the review triage tool. Like the question tool it
// is not a workspace tool and is registered by WithInteractions.
func ReviewDefinition() core.ToolDefinition {
	finding := schema(map[string]any{
		"severity": map[string]any{"type": "string", "enum": reviewSeverities, "description": "P0 blocks merging, P1 should be fixed before release, P2 is a note."},
		"title":    str("The problem in one line, at most 200 bytes, e.g. \"Cancel leaves the approval channel blocked\"."),
		"location": str("Where it is, as path:line (or path:start-end), at most 300 bytes."),
		"detail":   str("The evidence and the smallest fix, at most 2000 bytes. Plain text; a short code reference is fine."),
	}, "severity", "title")
	return core.ToolDefinition{
		Name: core.ReviewTool,
		Description: "Put the findings of a code review in front of the user and wait for their decision on each. Use it whenever you reviewed code, a diff or a PR at the user's request " +
			"(yourself or through a reviewer subagent) and found at least one concrete, evidenced problem; with no findings, say so in your reply instead. " +
			"Call it once per review with every finding, most severe first, and do not edit code for these findings before the decisions come back. " +
			"For each finding the user picks fix (make the smallest fix and verify it), ignore (leave it alone and do not raise it again), fixed (they fixed it themselves: check that it is resolved rather than redoing it) " +
			"or reply (their own words about it, which are direction for that finding), or leaves it open (no decision: change nothing for it and list it in your report). " +
			"The decisions are the user's own direction for this task, but authorize only what they plainly say. If the user skips the review, change nothing and summarize the findings in your reply.",
		Parameters: schema(map[string]any{
			"summary":  str("Optional one- or two-sentence overview of the review, at most 1000 bytes: what was reviewed and the overall verdict."),
			"findings": map[string]any{"type": "array", "minItems": 1, "maxItems": maxFindings, "items": finding, "description": "The findings, most severe first."},
		}, "findings"),
	}
}

// WithInteractions adds the question tool and, when triage is set, the review
// tool. They share one slot, so at most one of them is on screen at a time.
// Like WithQuestions, wrap the outermost executor: subagents never reach them.
func WithInteractions(base core.ToolExecutor, ask core.Asker, triage core.Triager) core.ToolExecutor {
	if ask == nil && triage == nil {
		return base
	}
	return &questionExecutor{base: base, ask: ask, triage: triage, slot: make(chan struct{}, 1)}
}

func (e *questionExecutor) review(ctx context.Context, call core.ToolCall) core.ToolResult {
	review, err := parseReview(call.Arguments)
	if err != nil {
		return failure(err)
	}
	answer, err := e.triage(ctx, review)
	if err != nil {
		return failure(fmt.Errorf("ask user: %w", err))
	}
	result, err := reviewResult(review, answer)
	if err != nil {
		return failure(err)
	}
	data, err := encodeReviewResult(result)
	if err != nil {
		return failure(err)
	}
	return core.ToolResult{Content: data}
}

// reviewResult turns the user's triage into the model's view of it. Titles
// come from the validated review, never from the triager.
func reviewResult(review core.Review, answer core.Answer) (core.ReviewResult, error) {
	if answer.Skipped {
		return core.ReviewResult{Skipped: true, Note: "The user closed the review without deciding. Change no code for these findings; summarize them in your reply and stop."}, nil
	}
	if len(answer.Triage) != len(review.Findings) {
		return core.ReviewResult{}, fmt.Errorf("triager returned %d decisions for %d findings", len(answer.Triage), len(review.Findings))
	}
	result := core.ReviewResult{Note: "Each decision is the user's direction for that finding. fix: make the smallest fix and verify it. " +
		"fixed: the user fixed it; check that it is resolved and do not redo it. ignore: leave it and do not raise it again. " +
		"reply: the user's own words about that finding; follow them. open: no decision; change nothing for it and list it in your report."}
	for i, triage := range answer.Triage {
		decision := core.FindingDecision{Finding: i + 1, Title: review.Findings[i].Title, Decision: triage.Decision}
		switch triage.Decision {
		case "", core.TriageOpen:
			decision.Decision = core.TriageOpen
		case core.TriageFix, core.TriageIgnore, core.TriageFixed:
		case core.TriageReply:
			text := strings.TrimSpace(strings.ToValidUTF8(triage.Reply, ""))
			if text == "" {
				return core.ReviewResult{}, fmt.Errorf("triager returned an empty reply for finding %d", i+1)
			}
			if len(text) > maxReplyBytes {
				text = strings.ToValidUTF8(text[:maxReplyBytes], "") + "\n[reply truncated]"
			}
			decision.Reply = text
		default:
			return core.ReviewResult{}, fmt.Errorf("triager returned unknown decision %q for finding %d", triage.Decision, i+1)
		}
		result.Decisions = append(result.Decisions, decision)
	}
	return result, nil
}

// encodeReviewResult keeps the JSON within the tool output limit by halving the
// longest reply until it fits, so one long reply never costs the others theirs.
func encodeReviewResult(result core.ReviewResult) (string, error) {
	const marker = "\n[reply truncated]"
	for {
		var out bytes.Buffer
		encoder := json.NewEncoder(&out)
		encoder.SetEscapeHTML(false)
		if err := encoder.Encode(result); err != nil {
			return "", err
		}
		data := strings.TrimSuffix(out.String(), "\n")
		longest := -1
		for i, decision := range result.Decisions {
			if len(decision.Reply) >= 2 && (longest < 0 || len(decision.Reply) > len(result.Decisions[longest].Reply)) {
				longest = i
			}
		}
		if len(data) <= maxOutput || longest < 0 {
			return data, nil
		}
		reply := strings.TrimSuffix(result.Decisions[longest].Reply, marker)
		result.Decisions[longest].Reply = strings.ToValidUTF8(reply[:len(reply)/2], "") + marker
	}
}

// parseReview validates the model's arguments and reduces every field to what a
// reader can see, so nothing hidden rides along into what the user decides on.
func parseReview(raw json.RawMessage) (core.Review, error) {
	var review core.Review
	if !utf8.Valid(raw) {
		return review, errors.New("arguments must be valid UTF-8")
	}
	if err := requireFields(raw, "findings"); err != nil {
		return review, err
	}
	if err := decode(raw, &review); err != nil {
		return review, err
	}
	var wire struct {
		Findings []json.RawMessage `json:"findings"`
	}
	_ = json.Unmarshal(raw, &wire)
	for i, finding := range wire.Findings {
		if err := requireFields(finding, "severity", "title"); err != nil {
			return review, fmt.Errorf("finding %d: %w", i+1, err)
		}
	}
	if strings.ContainsRune(review.Summary, 0) {
		return review, errors.New("summary must not contain NUL")
	}
	review.Summary = strings.TrimSpace(core.VisibleText(review.Summary, true))
	if len(review.Summary) > maxSummaryBytes {
		return review, fmt.Errorf("summary must be at most %d bytes", maxSummaryBytes)
	}
	if len(review.Findings) < 1 || len(review.Findings) > maxFindings {
		return review, fmt.Errorf("provide between 1 and %d findings; with none, say so in your reply instead", maxFindings)
	}
	for i := range review.Findings {
		finding := &review.Findings[i]
		for _, text := range []string{finding.Severity, finding.Title, finding.Location, finding.Detail} {
			if strings.ContainsRune(text, 0) {
				return review, fmt.Errorf("finding %d: text must not contain NUL", i+1)
			}
		}
		finding.Severity = strings.ToUpper(strings.TrimSpace(finding.Severity))
		valid := false
		for _, severity := range reviewSeverities {
			valid = valid || finding.Severity == severity
		}
		if !valid {
			return review, fmt.Errorf("finding %d: severity must be one of %s", i+1, strings.Join(reviewSeverities, ", "))
		}
		finding.Title = strings.Join(strings.Fields(core.VisibleText(finding.Title, false)), " ")
		finding.Location = strings.Join(strings.Fields(core.VisibleText(finding.Location, false)), " ")
		finding.Detail = strings.TrimSpace(core.VisibleText(finding.Detail, true))
		if !core.HasVisibleText(finding.Title) || len(finding.Title) > maxFindingTitle {
			return review, fmt.Errorf("finding %d: title must be nonempty UTF-8 text of at most %d bytes", i+1, maxFindingTitle)
		}
		if len(finding.Location) > maxFindingLocation {
			return review, fmt.Errorf("finding %d: location must be at most %d bytes", i+1, maxFindingLocation)
		}
		if len(finding.Detail) > maxFindingDetail {
			return review, fmt.Errorf("finding %d: detail must be at most %d bytes", i+1, maxFindingDetail)
		}
	}
	return review, nil
}

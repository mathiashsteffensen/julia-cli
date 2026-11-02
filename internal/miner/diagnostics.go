package miner

import (
	"errors"
	"strings"
)

// Keep diagnostic storage bounded even when the source corpus is much larger
// than the shortlist. Counts include every rejection; only references are capped.
const maxRejections = 10000

type transcriptError struct {
	reason  string
	message string
}

func (e *transcriptError) Error() string { return e.message }

func invalidTranscript(reason, message string) error {
	return &transcriptError{reason: reason, message: message}
}

func rejectionReason(err error) string {
	var invalid *transcriptError
	if errors.As(err, &invalid) {
		return invalid.reason
	}
	return "encoding_error"
}

func validRejectionStage(stage string) bool {
	switch stage {
	case "non_regular", "invalid_session", "cyclic_parent", "sensitive",
		"changed_or_invalid_session", "private_session", "orphan_message",
		"steered_turn", "invalid_turn", "invalid_context", "sensitive_feedback", "too_large":
		return true
	}
	return false
}

func validRejectionReason(reason string) bool {
	switch reason {
	case "non_regular", "invalid_session_id", "unreadable_or_invalid_session",
		"invalid_parent_chain", "secret_metadata", "private_group",
		"orphan_message", "steered_turn", "pending_tool", "image",
		"secret_text", "secret_arguments", "incomplete_turn", "unexpected_user",
		"interrupted", "invalid_context_start", "invalid_tool_definitions",
		"unresolved_call", "empty_assistant", "invalid_call_id", "unknown_tool",
		"private_tool", "invalid_arguments_json", "arguments_not_object",
		"argument_enum", "argument_type", "unsupported_schema",
		"missing_argument", "unexpected_argument", "orphan_result",
		"unexpected_role", "dangling_call", "encoding_error", "size_limit":
		return true
	}
	return false
}

func validRejectionKey(key string) bool {
	stage, reason, ok := strings.Cut(key, "/")
	return ok && validRejectionStage(stage) && validRejectionReason(reason)
}

func (r *Report) reject(rejection Rejection) {
	r.Stats.Skipped[rejection.Stage]++
	if r.Stats.RejectionReasons == nil {
		r.Stats.RejectionReasons = make(map[string]int)
	}
	r.Stats.RejectionReasons[rejection.Stage+"/"+rejection.Reason]++
	if len(r.Rejections) == maxRejections {
		r.Stats.RejectionsOmitted++
		return
	}
	r.Rejections = append(r.Rejections, rejection)
}

package provider

import (
	"bytes"
	"encoding/json"
	"strings"

	"julia/internal/core"
)

// Runtime instructions are reconstructed by the host for each Run. They are
// never inferred from provider history or persisted as conversation messages.
func sessionInstructions(session *core.Session) string {
	if session != nil && strings.TrimSpace(session.Instructions) != "" {
		return session.Instructions
	}
	return core.SystemPrompt
}

// Keep both reviewers on the same explicit facts. The executing agent's system
// instructions and skill bundle are intentionally not part of this payload.
// HTML escaping stays off so the reviewer reads && and > in commands as written.
func reviewPayload(approval core.Approval) ([]byte, error) {
	var payload bytes.Buffer
	encoder := json.NewEncoder(&payload)
	encoder.SetEscapeHTML(false)
	err := encoder.Encode(map[string]any{
		"task":           reviewTask(approval.Task),
		"tool":           approval.Call.Name,
		"arguments":      approval.Call.Arguments,
		"reason":         approval.Reason,
		"execution_mode": approval.ExecutionMode,
		"directory":      approval.Directory,
		"justification":  approval.Justification,
		"network":        approval.Network,
	})
	if err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(payload.Bytes(), []byte("\n")), nil
}

// reviewTask embeds an encoded task list (core.EncodeTask) as the JSON array
// the policy describes. Marshalled as a string, it reached the reviewer as an
// escaped blob, which reviewers rejected as a malformed task. Any other text
// stays a plain string.
func reviewTask(task string) any {
	trimmed := strings.TrimSpace(task)
	if strings.HasPrefix(trimmed, "[") && json.Valid([]byte(trimmed)) {
		return json.RawMessage(trimmed)
	}
	return task
}

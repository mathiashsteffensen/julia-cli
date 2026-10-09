package provider

import (
	"encoding/json"
	"strings"

	"jarvis/internal/core"
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
func reviewPayload(approval core.Approval) ([]byte, error) {
	return json.Marshal(map[string]any{
		"task":           approval.Task,
		"tool":           approval.Call.Name,
		"arguments":      approval.Call.Arguments,
		"reason":         approval.Reason,
		"execution_mode": approval.ExecutionMode,
		"directory":      approval.Directory,
		"justification":  approval.Justification,
		"network":        approval.Network,
	})
}

package core

import (
	"bytes"
	"encoding/json"
	"strings"
)

// TaskEntry is one user-side entry of the conversation that the action reviewer
// judges authorization against: a message the user sent, or the user's answer to
// a question the assistant asked. Exactly one of the answer fields accompanies a
// question. The question is assistant-written; only the answer is the user's.
type TaskEntry struct {
	UserMessage        string `json:"user_message,omitempty"`
	AssistantQuestion  string `json:"assistant_question,omitempty"`
	UserSelectedOption string `json:"user_selected_option,omitempty"`
	UserTypedAnswer    string `json:"user_typed_answer,omitempty"`
}

// EncodeTask renders entries, oldest first, as the JSON array Approval.Task
// carries. A structured list keeps each entry's author unambiguous and lets
// a reviewer weigh a later message or answer as fully as the first one, which
// prose with follow-up headings did not. Shell operators stay unescaped so the
// reviewer reads commands as the user wrote them.
func EncodeTask(entries []TaskEntry) string {
	if entries == nil {
		entries = []TaskEntry{}
	}
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(entries); err != nil {
		return "[]"
	}
	return strings.TrimSuffix(encoded.String(), "\n")
}

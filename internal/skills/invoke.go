package skills

import (
	"fmt"
	"strings"
)

// invocationTag opens the block that carries a skill the user ran; Typed
// finds the user's own words before it.
const invocationTag = "\n\n<skill name="

// Invocation is the message the model receives when the user runs skill as a
// slash command: the command as they typed it, then the skill's SKILL.md.
// What follows the command word is the skill's input.
func Invocation(typed string, skill Skill) (string, error) {
	instructions, err := skill.Read("")
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s%s%q source=%q>\nI ran the %s skill with the command above; what follows the command is its input. Follow its instructions:\n\n%s\n</skill>",
		strings.TrimSpace(typed), invocationTag, skill.Name, skill.Dir, skill.Name, strings.TrimSpace(instructions)), nil
}

// Typed is what the user typed of a message that may be an Invocation: the
// command alone, without the skill it brought. Any other message is returned
// as it is.
func Typed(message string) string {
	if !strings.HasPrefix(message, "/") || !strings.HasSuffix(message, "\n</skill>") {
		return message
	}
	if typed, _, found := strings.Cut(message, invocationTag); found {
		return typed
	}
	return message
}

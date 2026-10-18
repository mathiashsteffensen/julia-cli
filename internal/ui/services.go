package ui

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// slackSummary is a Slack call's title row: the action, then the channel and
// what the call says or reacts with. A read's limits follow when given.
func slackSummary(arguments json.RawMessage) (string, bool) {
	var a struct {
		Action, Channel, Text, Timestamp, Name, Oldest string
		ThreadTS                                       string `json:"thread_ts"`
		Limit                                          int
	}
	if json.Unmarshal(arguments, &a) != nil || a.Action == "" {
		return "", false
	}
	parts := []string{a.Action}
	if a.Channel != "" {
		parts = append(parts, slackChannel(a.Channel))
	}
	switch a.Action {
	case "post_message":
		parts = append(parts, strconv.Quote(typedLine(a.Text)))
		if a.ThreadTS != "" {
			parts = append(parts, "in thread "+a.ThreadTS)
		}
	case "add_reaction":
		parts = append(parts, ":"+strings.Trim(a.Name, ":")+":")
		if a.Timestamp != "" {
			parts = append(parts, "on "+a.Timestamp)
		}
	case "read_thread":
		if a.ThreadTS != "" {
			parts = append(parts, "thread "+a.ThreadTS)
		}
	case "read_channel":
		if a.Oldest != "" {
			parts = append(parts, "since "+a.Oldest)
		}
	}
	if a.Limit > 0 && strings.HasPrefix(a.Action, "read_") {
		parts = append(parts, fmt.Sprintf("limit %d", a.Limit))
	}
	return strings.Join(parts, " · "), true
}

// slackChannel shows a channel as people write it. A name gets its #; a Slack
// id (C…, G…, D…) stays as it is.
func slackChannel(channel string) string {
	if strings.HasPrefix(channel, "#") || strings.HasPrefix(channel, "@") {
		return channel
	}
	if len(channel) >= 9 && strings.ContainsRune("CGD", rune(channel[0])) && strings.ToUpper(channel) == channel {
		return channel
	}
	return "#" + channel
}

// linearSummary is a Linear call's title row: the action, then what it acts on
// (a team, an issue) and the fields it sets or filters by.
func linearSummary(arguments json.RawMessage) (string, bool) {
	var a struct {
		Action, Team, Project, Identifier, Title, State, Priority, Assignee, Body, Labels, Name, Color string
		ReplaceLabels                                                                                  bool `json:"replace_labels"`
		Limit                                                                                          int
	}
	if json.Unmarshal(arguments, &a) != nil || a.Action == "" {
		return "", false
	}
	parts := []string{a.Action}
	field := func(label, value string) {
		if value != "" {
			parts = append(parts, label+" "+typedLine(value))
		}
	}
	switch a.Action {
	case "list_projects":
		if a.Team != "" {
			parts = append(parts, a.Team)
		}
	case "list_issues":
		if a.Team != "" {
			parts = append(parts, a.Team)
		}
		field("project", a.Project)
		field("state", a.State)
		field("assignee", a.Assignee)
		if a.Limit > 0 {
			parts = append(parts, fmt.Sprintf("limit %d", a.Limit))
		}
	case "read_issue":
		field("", a.Identifier)
	case "create_issue":
		if a.Team != "" {
			parts = append(parts, a.Team)
		}
		if a.Title != "" {
			parts = append(parts, strconv.Quote(typedLine(a.Title)))
		}
		field("project", a.Project)
		field("state", a.State)
		field("priority", a.Priority)
		field("assignee", a.Assignee)
		field("labels", a.Labels)
	case "update_issue":
		field("", a.Identifier)
		if a.Title != "" {
			parts = append(parts, "title "+strconv.Quote(typedLine(a.Title)))
		}
		field("project", a.Project)
		field("state", a.State)
		field("priority", a.Priority)
		field("assignee", a.Assignee)
		if a.Labels != "" {
			verb := "labels"
			if a.ReplaceLabels {
				verb = "labels ="
			}
			parts = append(parts, verb+" "+typedLine(a.Labels))
		}
	case "add_comment":
		field("", a.Identifier)
		if a.Body != "" {
			parts = append(parts, strconv.Quote(typedLine(a.Body)))
		}
	case "create_label":
		if a.Name != "" {
			parts = append(parts, strconv.Quote(typedLine(a.Name)))
		}
		if a.Team != "" {
			parts = append(parts, a.Team)
		} else {
			parts = append(parts, "workspace")
		}
		field("", a.Color)
	}
	for i, part := range parts {
		parts[i] = strings.TrimPrefix(part, " ")
	}
	return strings.Join(parts, " · "), true
}

// memorySummary is a Memory call's title row: the action, then the note it
// names, and for a save its type, its scope when global, and the lesson last,
// quoted, so a narrow row truncates the lesson and never the name.
func memorySummary(arguments json.RawMessage) (string, bool) {
	var a struct {
		Action, Name, Description, Type, Scope string
	}
	if json.Unmarshal(arguments, &a) != nil || a.Action == "" {
		return "", false
	}
	parts := []string{a.Action}
	if a.Name != "" && a.Action != "list" {
		parts = append(parts, typedLine(a.Name))
	}
	switch a.Action {
	case "save":
		if a.Type != "" {
			parts = append(parts, a.Type)
		}
		if a.Scope == "global" {
			parts = append(parts, a.Scope)
		}
		if a.Description != "" {
			parts = append(parts, strconv.Quote(typedLine(a.Description)))
		}
	case "read", "forget":
		if a.Scope != "" {
			parts = append(parts, a.Scope)
		}
	}
	return strings.Join(parts, " · "), true
}

// argument is one named value of a tool call, as the model wrote it.
type argument struct{ key, value string }

// callArguments lists a call's arguments in the order the model wrote them, each
// scalar as its text and anything nested as compact JSON. Long text keeps its
// line breaks; the expanded tool view wraps it. Unparseable arguments come back
// as one "arguments" entry holding the raw text.
func callArguments(raw json.RawMessage) []argument {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		if strings.TrimSpace(string(raw)) == "" {
			return nil
		}
		return []argument{{"arguments", string(raw)}}
	}
	var out []argument
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			break
		}
		key, ok := token.(string)
		if !ok {
			break
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			if len(out) == 0 {
				return []argument{{"arguments", string(raw)}}
			}
			break
		}
		var text string
		var scalar any
		if json.Unmarshal(value, &scalar) == nil {
			switch v := scalar.(type) {
			case string:
				text = v
			case nil:
				text = "null"
			case bool:
				text = strconv.FormatBool(v)
			case float64:
				text = strconv.FormatFloat(v, 'f', -1, 64)
			default:
				text = string(value)
			}
		} else {
			text = string(value)
		}
		out = append(out, argument{key, text})
	}
	return out
}

// argumentsShown says whether a tool's arguments appear in full under its title
// when tools are expanded: the tools whose calls carry text the title row can
// only hint at, such as a message, an issue description or a note's body.
func argumentsShown(name string) bool {
	switch name {
	case "Slack", "Linear", "Memory":
		return true
	}
	return false
}

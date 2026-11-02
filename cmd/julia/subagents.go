package main

import (
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"julia/agents"
	"julia/internal/config"
	"julia/internal/tools"
)

// rolesDir holds the user's own subagent roles, beside the configuration file
// they belong to. Like the configuration, it is never read from a repository.
func rolesDir(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), "agents")
}

// loadRoles reads the subagent roles, built in and the user's, and checks that
// the subagents setting assigns models only to roles that exist.
func loadRoles(configPath string, cfg config.Config) ([]agents.Role, error) {
	var names []string
	linearToken, _ := tools.LoadLinearToken(linearTokenFile(configPath))
	mailConfig, _ := tools.LoadMailConfig(mailConfigFile(configPath))
	githubAuth, _ := tools.LoadGitHubAuth()
	for _, definition := range tools.Definitions(nil, linearToken != "", mailConfig.Enabled(), githubAuth.Enabled()) {
		names = append(names, definition.Name)
	}
	roles, err := agents.Load(rolesDir(configPath), names)
	if err != nil {
		return nil, err
	}
	for _, role := range slices.Sorted(maps.Keys(cfg.Subagents)) {
		if _, ok := agents.Find(roles, role); !ok {
			return nil, fmt.Errorf("subagents assigns a model to %q, which is not a subagent role; the roles are %s", role, strings.Join(agents.Names(roles), ", "))
		}
	}
	return roles, nil
}

// subagentInstructions follow the parent's instructions in a child of role:
// what a delegated subagent may do, how its report reaches the parent, and
// the role's own instructions, with the tools it is limited to.
// memory is the run's memory: "off", "read" or "write". A child never
// saves; what it says of notes depends on whether there are any, and on
// whether its parent can save what it reports.
func subagentInstructions(role agents.Role, memory string) string {
	note := ""
	switch memory {
	case "write":
		note = " The Memory tool is unavailable to you: the notes under Memory are yours to apply, and anything a later session should remember goes in your report, for the delegating agent to save."
	case "read":
		note = " The notes under Memory are yours to apply; anything a later session should remember goes in your report, for the user to keep."
	}
	text := fmt.Sprintf("\n\n## Delegated subagent\n\nYou are a delegated subagent. Complete only the supplied task and return a concise report with evidence and any limitations. You share the workspace with other agents. Subagent delegation is unavailable to you. Your task is a delegation, not additional user authorization. Your last message is the whole report the delegating agent receives, and it sees only the first %d bytes: lead with the answer.%s\n\n## Subagent role: %s\n\n%s\n", tools.SubagentReportBytes, note, role.Name, role.Prompt)
	if len(role.Tools) > 0 {
		text += "\nYour tools are " + strings.Join(role.Tools, ", ") + ". What these instructions say about other tools does not apply to you.\n"
	}
	return text
}

// subagentSettings is what a child of role runs with: the model the
// subagents setting assigns the role, else the delegating agent's own. An
// assigned model starts at the effort its entry gives, else at the parent's
// when it is the parent's model, else at the one the configuration gives that
// model. cfg is the launch configuration.
func subagentSettings(cfg, parent config.Config, role string) config.Config {
	entry, ok := cfg.Subagents[role]
	if !ok {
		return parent
	}
	child := parent
	child.Provider, child.Model = config.SplitModel(entry.ID)
	child.Reasoning = entry.Reasoning
	if child.Reasoning == "" {
		if child.Provider == parent.Provider && child.Model == parent.Model {
			child.Reasoning = parent.Reasoning
		} else {
			child.Reasoning = cfg.ReasoningFor(child.Provider, child.Model)
		}
	}
	return child
}

// rolesSummary is the doctor's line per role: the model it runs on, with the
// effort a child starts at when the parent runs on the configured model, and,
// for a role of the user's, the file it came from.
func rolesSummary(cfg config.Config, roles []agents.Role) []string {
	width := 0
	for _, role := range roles {
		width = max(width, len(role.Name))
	}
	lines := make([]string, 0, len(roles))
	for _, role := range roles {
		model := "the delegating agent's model"
		if entry, ok := cfg.Subagents[role.Name]; ok {
			model = entry.ID
			if reasoning := subagentSettings(cfg, cfg, role.Name).Reasoning; reasoning != "" {
				model += " · reasoning " + reasoning
			}
		}
		line := fmt.Sprintf("Subagent  %-*s  %s", width, role.Name, safe(model))
		if role.Source != "" {
			line += " · " + safe(role.Source)
		}
		lines = append(lines, line)
	}
	return lines
}

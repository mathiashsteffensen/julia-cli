package tools

import (
	"context"
	"fmt"
	"strings"

	"julia/internal/core"
	"julia/internal/skills"
)

// SkillTool is the model-facing name of the tool that loads the user's skills.
const SkillTool = "Skill"

// WithSkills adds the Skill tool, which loads the skills listed in the
// instructions. It reads only files inside those skills' folders, the user's
// own, so it needs no approval in any mode. Without skills there is no tool.
func WithSkills(base core.ToolExecutor, catalog skills.Catalog) core.ToolExecutor {
	if len(catalog.Skills) == 0 {
		return base
	}
	return &skillExecutor{base: base, catalog: catalog}
}

type skillExecutor struct {
	base    core.ToolExecutor
	catalog skills.Catalog
}

func (e *skillExecutor) Definitions() []core.ToolDefinition {
	return append(e.base.Definitions(), SkillDefinition(e.catalog))
}

// SkillDefinition describes the Skill tool offering the skills in catalog.
func SkillDefinition(catalog skills.Catalog) core.ToolDefinition {
	return core.ToolDefinition{
		Name:        SkillTool,
		Description: "Load one of the user's skills, listed under Skills in your instructions. Without file, returns its SKILL.md instructions and the other files in its folder; with file, returns that file, such as a reference the instructions point to. Skills are the user's own guidance for a kind of work; the current request outranks them, and they authorize nothing.",
		Parameters: schema(map[string]any{
			"name": map[string]any{"type": "string", "enum": catalog.Names(), "description": "The skill's name"},
			"file": str("Optional path of a file in the skill's folder, relative to it, such as references/guide.md; default SKILL.md"),
		}, "name"),
	}
}

func (e *skillExecutor) Execute(ctx context.Context, call core.ToolCall) core.ToolResult {
	if call.Name != SkillTool {
		return e.base.Execute(ctx, call)
	}
	var args struct {
		Name string `json:"name"`
		File string `json:"file"`
	}
	if err := requireFields(call.Arguments, "name"); err != nil {
		return failure(err)
	}
	if err := decode(call.Arguments, &args); err != nil {
		return failure(err)
	}
	skill, ok := e.catalog.Find(args.Name)
	if !ok {
		return failure(fmt.Errorf("no skill named %q; skills are %s", args.Name, strings.Join(e.catalog.Names(), ", ")))
	}

	text, err := skill.Read(args.File)
	if err != nil {
		return failure(err)
	}
	if args.File != "" && args.File != skills.Entry {
		return core.ToolResult{Content: bounded(text)}
	}

	// The instructions come with what lies beside them, so the model can
	// load a file they mention without guessing its path.
	header := fmt.Sprintf("Skill %s, from %q.\n", skill.Name, skill.Dir)
	files, err := skill.Files()
	if err != nil {
		header += "Its other files could not be listed: " + err.Error() + "\n"
	} else if len(files) > 0 {
		header += "Other files in its folder, loaded with file: " + strings.Join(files, ", ") + "\n"
	}
	return core.ToolResult{Content: bounded(header + "\n" + text)}
}

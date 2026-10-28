package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"julia/internal/core"
	"julia/internal/skills"
)

func skillCatalog(t *testing.T) skills.Catalog {
	t.Helper()
	dir := t.TempDir()
	for path, content := range map[string]string{
		"writing/SKILL.md":           "---\nname: writing\ndescription: Writing for agents.\n---\nRead MECHANICS.md before editing a skill.",
		"writing/MECHANICS.md":       "Skills load in stages.",
		"writing/references/tone.md": "Plain words.",
		"kero/SKILL.md":              "---\nname: kero\ndescription: Drive terminal sessions.\n---\nUse kero panes.",
	} {
		path = filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	catalog, warnings, err := skills.Discover(dir)
	if err != nil || len(warnings) != 0 {
		t.Fatal(err, warnings)
	}
	return catalog
}

func callSkill(executor core.ToolExecutor, arguments string) core.ToolResult {
	return executor.Execute(context.Background(), core.ToolCall{ID: "s1", Name: SkillTool, Arguments: json.RawMessage(arguments)})
}

func TestSkillToolLoadsInstructionsAndTheirFiles(t *testing.T) {
	executor := WithSkills(stubTools{content: "base"}, skillCatalog(t))

	definitions := executor.Definitions()
	if len(definitions) != 1 || definitions[0].Name != SkillTool {
		t.Fatalf("definitions %+v", definitions)
	}
	names := definitions[0].Parameters["properties"].(map[string]any)["name"].(map[string]any)["enum"]
	if got, _ := json.Marshal(names); string(got) != `["kero","writing"]` {
		t.Fatalf("the name enum must list the skills: %s", got)
	}

	loaded := callSkill(executor, `{"name":"writing"}`)
	for _, want := range []string{"Skill writing, from", "MECHANICS.md, references/tone.md", "Read MECHANICS.md before editing a skill."} {
		if loaded.IsError || !strings.Contains(loaded.Content, want) {
			t.Fatalf("missing %q in %+v", want, loaded)
		}
	}
	if file := callSkill(executor, `{"name":"writing","file":"references/tone.md"}`); file.IsError || file.Content != "Plain words." {
		t.Fatalf("companion file: %+v", file)
	}
	for _, arguments := range []string{`{"name":"missing"}`, `{"name":"writing","file":"../kero/SKILL.md"}`, `{"file":"SKILL.md"}`, `{"name":"writing","path":"x"}`} {
		if result := callSkill(executor, arguments); !result.IsError {
			t.Errorf("%s was accepted: %q", arguments, result.Content)
		}
	}

	other := executor.Execute(context.Background(), core.ToolCall{Name: "Read", Arguments: json.RawMessage(`{}`)})
	if other.Content != "base" {
		t.Fatal("other tools must pass through")
	}
}

func TestNoSkillsNoTool(t *testing.T) {
	base := stubTools{}
	if WithSkills(base, skills.Catalog{}) != core.ToolExecutor(base) {
		t.Fatal("an empty catalog must not add the Skill tool")
	}
}

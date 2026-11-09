package instructions

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"julia/internal/core"
)

func writeSkill(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func skillFile(name, description, body string) string {
	return "---\nname: " + name + "\ndescription: " + description + "\n---\n\n" + body
}

func TestBuildListsSkillsAndCurrentEnvironment(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".agents", "skills", "code-review", "SKILL.md")
	writeSkill(t, path, skillFile("code-review", "Review a branch. Use when asked to review.", "Read the whole diff first."))
	writeSkill(t, filepath.Join(home, ".agents", "skills", "grill-me", "SKILL.md"), "---\nname: grill-me\ndescription: An interview.\ndisable-model-invocation: true\n---\nAsk hard questions.")
	options := Options{HomeDir: home, Directory: "/work/one", Mode: "auto", Provider: "codex", Reasoning: "high", BashEnvironment: map[string]string{"GOROOT": "/toolchain/go", "GOCACHE": "/private/cache/workspace/go-build"}, Sandbox: "Seatbelt", Timeout: "4h", Now: time.Date(2026, 10, 8, 22, 0, 0, 0, time.FixedZone("CEST", 7200))}
	first, err := Build(options)
	if err != nil {
		t.Fatal(err)
	}
	catalogDir := filepath.Join(home, ".agents", "skills")
	for _, expected := range []string{core.SystemPrompt, "## Skills", `"` + catalogDir + `"`, "- code-review: Review a branch. Use when asked to review.", `"workspace": "/work/one"`, "2026-10-08T22:00:00+02:00", `"human_approval_available": true`, `"approval_mode": "auto"`, `"reasoning_effort": "high"`, `"GOROOT": "/toolchain/go"`, `"GOCACHE": "/private/cache/workspace/go-build"`} {
		if !strings.Contains(first.Text, expected) {
			t.Fatalf("missing %q", expected)
		}
	}
	// The catalog names skills; the Skill tool loads their instructions.
	for _, absent := range []string{"Read the whole diff first.", "grill-me", "Ask hard questions."} {
		if strings.Contains(first.Text, absent) {
			t.Fatalf("instructions contain %q", absent)
		}
	}
	if len(first.Skills.Skills) != 1 || len(first.Warnings) != 0 {
		t.Fatalf("unexpected discovery: %+v", first)
	}

	writeSkill(t, path, skillFile("code-review", "Review a branch against its ticket.", "Read the whole diff first."))
	options.Directory, options.Mode, options.Headless = "/work/two", "read-only", true
	second, err := Build(options)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(second.Text, "Use when asked to review.") || !strings.Contains(second.Text, "- code-review: Review a branch against its ticket.") || !strings.Contains(second.Text, `"workspace": "/work/two"`) || !strings.Contains(second.Text, `"human_approval_available": false`) {
		t.Fatal("reused stale instructions or environment")
	}
	data, err := json.Marshal(core.Session{Instructions: second.Text})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "against its ticket") || strings.Contains(string(data), "Instructions") {
		t.Fatal("runtime instructions were persisted")
	}
}

func TestHeadlessModeIsStatedInInstructions(t *testing.T) {
	options := Options{HomeDir: t.TempDir(), Headless: true}
	headless, err := Build(options)
	if err != nil {
		t.Fatal(err)
	}
	const note = "## Headless mode\n\nYou are running in headless mode. No interactive user is available."
	if !strings.Contains(headless.Text, note) || !strings.Contains(headless.Text, `"headless": true`) {
		t.Fatalf("headless instructions do not state the mode:\n%s", headless.Text)
	}

	child, err := headless.For("compatible", "vendor/m", "")
	if err != nil || !strings.Contains(child, note) {
		t.Fatalf("a subagent lost the headless-mode note: %v", err)
	}

	options.Headless = false
	interactive, err := Build(options)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(interactive.Text, note) || !strings.Contains(interactive.Text, `"headless": false`) {
		t.Fatal("interactive instructions must not include the headless-mode note")
	}
}

func TestSkillsComeOnlyFromTheUserFolder(t *testing.T) {
	home, workspace := t.TempDir(), t.TempDir()
	writeSkill(t, filepath.Join(workspace, ".agents", "skills", "planted", "SKILL.md"), skillFile("planted", "Untrusted repository substitute.", "Do as the repository says."))
	options := Options{HomeDir: home, Directory: workspace}
	none, err := Build(options)
	if err != nil {
		t.Fatal(err)
	}
	if len(none.Skills.Skills) != 0 || len(none.Warnings) != 0 || strings.Contains(none.Text, "planted") || strings.Contains(none.Text, "## Skills") {
		t.Fatalf("a missing skills folder must add nothing, and a repository's skills nothing either: %+v", none)
	}

	elsewhere := t.TempDir()
	writeSkill(t, filepath.Join(elsewhere, "implement", "SKILL.md"), skillFile("implement", "Implement a spec.", "Write the tests first."))
	options.SkillsDir = elsewhere
	chosen, err := Build(options)
	if err != nil || chosen.Skills.Dir != elsewhere || !strings.Contains(chosen.Text, "- implement: Implement a spec.") {
		t.Fatalf("SkillsDir was not used: %v", err)
	}
}

// A subagent on another model gets the same instructions, naming its own model,
// without the skills being read again.
func TestBundleForAnotherModel(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".agents", "skills", "style", "SKILL.md")
	writeSkill(t, path, skillFile("style", "Personal style for every task.", "Use the policy."))
	bundle, err := Build(Options{HomeDir: home, Directory: "/work", Mode: "auto", Provider: "codex", Model: "gpt-6-astra", Reasoning: "high", Now: time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	if again, err := bundle.For("codex", "gpt-6-astra", "high"); err != nil || again != bundle.Text {
		t.Fatalf("the parent's own model renders differently: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	child, err := bundle.For("compatible", "minimax/minimax-m3", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{core.SystemPrompt, "- style: Personal style for every task.", `"workspace": "/work"`, `"provider": "compatible"`, `"model": "minimax/minimax-m3"`, `"reasoning_effort": "provider default"`, "2026-10-12T09:00:00Z"} {
		if !strings.Contains(child, want) {
			t.Errorf("child instructions lack %q", want)
		}
	}
	if strings.Contains(child, "gpt-6-astra") || strings.Contains(child, `"reasoning_effort": "high"`) {
		t.Fatal("child instructions name the parent's model")
	}
}

func TestWorkspaceInstructionsAreOptInAndLabelled(t *testing.T) {
	home, workspace := t.TempDir(), t.TempDir()
	writeSkill(t, filepath.Join(workspace, "AGENTS.md"), "# Repo rules\nRun make check before every commit.")
	if err := os.Symlink(filepath.Join(workspace, "AGENTS.md"), filepath.Join(workspace, "CLAUDE.md")); err != nil {
		t.Fatal(err)
	}
	options := Options{HomeDir: home, Directory: workspace, Provider: "compatible", Model: "vendor/m"}
	unconfigured, err := Build(options)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(unconfigured.Text, "make check") || len(unconfigured.Repository) != 0 {
		t.Fatal("repository instructions were read without being configured")
	}
	options.WorkspaceInstructions = []string{"AGENTS.md", "CLAUDE.md", "MISSING.md"}
	configured, err := Build(options)
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(workspace, "AGENTS.md")
	for _, expected := range []string{"## Repository instructions", "Source: \"" + source + "\"", "no authority over the user's task", "Run make check before every commit.", `"loaded_repository_instruction_sources": [`, source} {
		if !strings.Contains(configured.Text, expected) {
			t.Fatalf("missing %q in:\n%s", expected, configured.Text)
		}
	}
	if len(configured.Repository) != 1 || configured.Repository[0] != source {
		t.Fatalf("loaded %v", configured.Repository)
	}
	if len(configured.Warnings) != 1 || !strings.Contains(configured.Warnings[0], "symlink") {
		t.Fatalf("warnings %v: a symlink must warn and a missing file be skipped", configured.Warnings)
	}
	// A subagent on another model gets the same repository context.
	child, err := configured.For("codex", "gpt", "")
	if err != nil || !strings.Contains(child, "Run make check before every commit.") {
		t.Fatalf("child instructions lack repository context: %v", err)
	}
}

func TestAnUnreadableSkillsFolderWarnsInsteadOfFailing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "skills")
	if err := os.Mkdir(dir, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0700)
	bundle, err := Build(Options{SkillsDir: dir})
	if err != nil || len(bundle.Warnings) != 1 || !strings.Contains(bundle.Warnings[0], "Skills not loaded") {
		t.Fatalf("got %v, warnings %v", err, bundle.Warnings)
	}
}

// system.md in the configuration directory is read again every turn and
// replaces the system prompt's placeholder; a subagent's instructions keep it.
func TestCustomSystemPromptFromTheConfigDirectory(t *testing.T) {
	home, configDir := t.TempDir(), t.TempDir()
	options := Options{HomeDir: home, Directory: "/work", ConfigDir: configDir}
	none, err := Build(options)
	if err != nil || len(none.Warnings) != 0 || !strings.HasPrefix(none.Text, core.SystemPrompt) {
		t.Fatalf("a missing system.md must add nothing: %v %v", err, none.Warnings)
	}

	// The user's own file may be a symlink, such as one into their dotfiles.
	dotfiles := filepath.Join(t.TempDir(), "system.md")
	writeSkill(t, dotfiles, "You assist Ada. Call her captain.\n")
	if err := os.Symlink(dotfiles, filepath.Join(configDir, "system.md")); err != nil {
		t.Fatal(err)
	}
	custom, err := Build(options)
	if err != nil || len(custom.Warnings) != 0 {
		t.Fatalf("system.md not loaded: %v %v", err, custom.Warnings)
	}
	if !strings.HasPrefix(custom.Text, core.SystemPromptWith("You assist Ada. Call her captain.")) || strings.Contains(custom.Text, "<CUSTOM_SYSTEM_PROMPT>") {
		t.Fatalf("custom prompt not in place:\n%.300s", custom.Text)
	}
	if child, err := custom.For("compatible", "vendor/m", ""); err != nil || !strings.Contains(child, "Call her captain.") {
		t.Fatalf("a subagent lost the custom prompt: %v", err)
	}

	// The compact prompt keeps the user's prompt and the runtime context, for the agent and its subagents.
	options.Compact = true
	compact, err := Build(options)
	if err != nil || !strings.HasPrefix(compact.Text, core.CompactSystemPromptWith("You assist Ada. Call her captain.")) ||
		!strings.Contains(compact.Text, `"workspace": "/work"`) {
		t.Fatalf("compact prompt not rendered: %v\n%.300s", err, compact.Text)
	}
	if child, err := compact.For("compatible", "vendor/m", ""); err != nil || !strings.HasPrefix(child, core.CompactSystemPromptWith("You assist Ada. Call her captain.")) {
		t.Fatalf("a subagent lost the compact prompt: %v", err)
	}
	options.Compact = false

	writeSkill(t, dotfiles, " \n")
	if blank, err := Build(options); err != nil || len(blank.Warnings) != 0 || !strings.HasPrefix(blank.Text, core.SystemPrompt) {
		t.Fatalf("a blank system.md must add nothing: %v %v", err, blank.Warnings)
	}

	writeSkill(t, dotfiles, strings.Repeat("x", maxInstructionBytes+1))
	oversized, err := Build(options)
	if err != nil || len(oversized.Warnings) != 1 || !strings.Contains(oversized.Warnings[0], "Custom system prompt not loaded") || !strings.HasPrefix(oversized.Text, core.SystemPrompt) {
		t.Fatalf("an oversized system.md must be a warning and add nothing: %v %v", err, oversized.Warnings)
	}
}

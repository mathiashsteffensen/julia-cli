package skills

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverOffersWellFormedSkillsTheModelMayRun(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "diagnosing-bugs", Entry), "---\nname: diagnosing-bugs\ndescription: >\n  Diagnosis loop for hard bugs.\n  Use when something is broken.\nlicense: MIT\nmetadata:\n  author: me\n---\n\n# Diagnosing\n")
	write(t, filepath.Join(dir, "implement", Entry), "---\nname: implement\ndescription: \"Implement a spec.\"\n---\nBody")
	write(t, filepath.Join(dir, "grill-me", Entry), "---\nname: grill-me\ndescription: An interview.\ndisable-model-invocation: true\n---\nBody")
	write(t, filepath.Join(dir, "notes", "README.md"), "not a skill")
	write(t, filepath.Join(dir, ".system", "hidden", Entry), "---\nname: hidden\ndescription: Hidden.\n---\n")
	write(t, filepath.Join(dir, "loose.md"), "a file beside the skills")
	write(t, filepath.Join(dir, "no-header", Entry), "# Just markdown")
	write(t, filepath.Join(dir, "renamed", Entry), "---\nname: other-name\ndescription: Mismatched.\n---\n")
	write(t, filepath.Join(dir, "silent", Entry), "---\nname: silent\n---\n")
	write(t, filepath.Join(dir, "Bad_Name", Entry), "---\nname: Bad_Name\ndescription: Invalid name.\n---\n")
	write(t, filepath.Join(dir, "broken", Entry), "---\nname: [unclosed\n---\n")

	elsewhere := filepath.Join(t.TempDir(), "kept-elsewhere")
	write(t, filepath.Join(elsewhere, Entry), "---\nname: linked\ndescription: Lives in another repository.\n---\n")
	if err := os.Symlink(elsewhere, filepath.Join(dir, "linked")); err != nil {
		t.Fatal(err)
	}

	catalog, warnings, err := Discover(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := catalog.Names(), []string{"diagnosing-bugs", "implement", "linked"}; !slices.Equal(got, want) {
		t.Fatalf("offered %v, want %v", got, want)
	}
	if skill, _ := catalog.Find("diagnosing-bugs"); skill.Description != "Diagnosis loop for hard bugs. Use when something is broken." {
		t.Fatalf("a folded description must read as one line: %q", skill.Description)
	}
	if skill, _ := catalog.Find("linked"); skill.Dir != mustResolve(t, elsewhere) {
		t.Fatalf("a symlinked skill's folder must be resolved: %q", skill.Dir)
	}
	if len(warnings) != 5 {
		t.Fatalf("each malformed skill must warn once, got %d:\n%s", len(warnings), strings.Join(warnings, "\n"))
	}
	for _, name := range []string{"no-header", "renamed", "silent", "Bad_Name", "broken"} {
		if !slices.ContainsFunc(warnings, func(w string) bool { return strings.Contains(w, `"`+name+`"`) }) {
			t.Errorf("no warning names %s", name)
		}
	}
}

func TestDiscoverWithoutAFolderOffersNothing(t *testing.T) {
	catalog, warnings, err := Discover(filepath.Join(t.TempDir(), "missing"))
	if err != nil || len(catalog.Skills) != 0 || len(warnings) != 0 {
		t.Fatalf("a missing folder must be quietly empty: %+v %v %v", catalog, warnings, err)
	}
}

func TestReadStaysInsideTheSkillFolder(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "writing", Entry), "---\nname: writing\ndescription: Writing.\n---\nSee MECHANICS.md.")
	write(t, filepath.Join(dir, "writing", "MECHANICS.md"), "How skills load.")
	write(t, filepath.Join(dir, "writing", "references", "style.md"), "Short sentences.")
	write(t, filepath.Join(dir, "writing", ".git", "config"), "hidden")
	write(t, filepath.Join(dir, "secret.txt"), "outside the skill")
	if err := os.Symlink(filepath.Join(dir, "secret.txt"), filepath.Join(dir, "writing", "escape.md")); err != nil {
		t.Fatal(err)
	}
	catalog, _, err := Discover(dir)
	if err != nil {
		t.Fatal(err)
	}
	skill, ok := catalog.Find("writing")
	if !ok {
		t.Fatal("skill not found")
	}

	if text, err := skill.Read(""); err != nil || !strings.Contains(text, "See MECHANICS.md.") {
		t.Fatalf("default read: %q %v", text, err)
	}
	if text, err := skill.Read("references/style.md"); err != nil || text != "Short sentences." {
		t.Fatalf("companion read: %q %v", text, err)
	}
	for _, file := range []string{"../secret.txt", filepath.Join(dir, "secret.txt"), "escape.md", "missing.md", "references"} {
		if text, err := skill.Read(file); err == nil {
			t.Errorf("read %q: %q", file, text)
		}
	}

	files, err := skill.Files()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"MECHANICS.md", "escape.md", "references/style.md"}; !slices.Equal(files, want) {
		t.Fatalf("listed %v, want %v", files, want)
	}
}

func TestReadRefusesFilesThatAreNotText(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "s", Entry), "---\nname: s\ndescription: S.\n---\n")
	catalog, _, err := Discover(dir)
	if err != nil {
		t.Fatal(err)
	}
	skill, _ := catalog.Find("s")
	for name, content := range map[string]string{"empty.md": "", "blank.md": "  \n", "binary.md": string([]byte{0xff}), "nul.md": "bad\x00text", "huge.md": strings.Repeat("a", MaxFileBytes+1)} {
		write(t, filepath.Join(dir, "s", name), content)
		if _, err := skill.Read(name); err == nil {
			t.Errorf("accepted %s", name)
		}
	}
}

func TestAFIFOCannotBlockTheTurn(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "fifo"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "fifo", Entry), 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan []string, 1)
	go func() { _, warnings, _ := Discover(dir); done <- warnings }()
	select {
	case warnings := <-done:
		if len(warnings) != 1 {
			t.Fatalf("a FIFO must be refused with a warning: %v", warnings)
		}
	case <-time.After(time.Second):
		t.Fatal("discovery blocked on a FIFO")
	}
}

func mustResolve(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

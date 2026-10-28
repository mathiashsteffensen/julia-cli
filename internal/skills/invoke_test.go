package skills

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestInvocationCarriesTheSkillAndTypedRecoversTheCommand(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "implement", Entry), "---\nname: implement\ndescription: Implement a spec.\ndisable-model-invocation: true\n---\n\nWrite the tests first.\n")
	catalog, _, err := Discover(dir)
	if err != nil {
		t.Fatal(err)
	}
	skill, _ := catalog.Find("implement")

	typed := "/implement the spec in docs/plan.md"
	message, err := Invocation(typed+"\n", skill)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{typed + "\n\n<skill name=\"implement\"", "I ran the implement skill", "Write the tests first.", skill.Dir} {
		if !strings.Contains(message, want) {
			t.Fatalf("missing %q in:\n%s", want, message)
		}
	}
	if got := Typed(message); got != typed {
		t.Fatalf("Typed = %q, want %q", got, typed)
	}
	for _, plain := range []string{"Fix the bug", "/usr/bin/env is missing", "see\n\n<skill name=\"x\">\n</skill>"} {
		if got := Typed(plain); got != plain {
			t.Errorf("Typed changed an ordinary message %q to %q", plain, got)
		}
	}
}

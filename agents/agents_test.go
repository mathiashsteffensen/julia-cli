package agents_test

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"julia/agents"
	"julia/internal/tools"
)

// childTools are the tools a subagent has, which a role may list.
func childTools() []string {
	var names []string
	for _, definition := range tools.Definitions(nil, false) {
		names = append(names, definition.Name)
	}
	return names
}

func TestBuiltinRoles(t *testing.T) {
	roles := agents.Builtin()
	if got := agents.Names(roles); !slices.Equal(got, []string{"delegate", "oracle", "researcher", "reviewer", "scout", "worker"}) {
		t.Fatalf("built-in roles %v", got)
	}
	readOnly := []string{"Read", "List", "Grep", "ViewImage", "Bash"}
	for _, role := range roles {
		if role.Source != "" || role.Description == "" || role.Prompt == "" {
			t.Errorf("%s is incomplete: %+v", role.Name, role)
		}
		for _, tool := range role.Tools {
			if !slices.Contains(childTools(), tool) {
				t.Errorf("%s lists %s, which a subagent does not have", role.Name, tool)
			}
		}
		// Roles describe JULIA's own tools, never another harness's.
		for _, foreign := range []string{"contact_supervisor", "progress.md", "fetch_content", "source_check", "watchdog", "intercom", "pi ", "Codex", "Claude"} {
			if strings.Contains(role.Prompt, foreign) || strings.Contains(role.Description, foreign) {
				t.Errorf("%s mentions %q", role.Name, foreign)
			}
		}
	}
	for name, want := range map[string][]string{"scout": readOnly, "reviewer": readOnly, "oracle": readOnly, "researcher": {"WebSearch", "WebFetch", "Browser", "Read", "List", "Grep"}, "worker": nil, "delegate": nil} {
		if role, _ := agents.Find(roles, name); !slices.Equal(role.Tools, want) {
			t.Errorf("%s tools %v, want %v", name, role.Tools, want)
		}
	}
	if _, err := agents.Load(filepath.Join(t.TempDir(), "missing"), childTools()); err != nil {
		t.Fatalf("built-in roles fail to load: %v", err)
	}
}

func TestParse(t *testing.T) {
	for _, data := range []string{
		"---\r\n# a comment\r\nname: auditor\r\ndescription: \"Audits: claims\"\r\n\r\ntools: [Read, Grep]\r\n---\r\n\r\nAudit the claims.\r\n",
		// An editor's byte order mark, delimiters with trailing blanks, and a quoted YAML list.
		"\ufeff--- \nname: auditor\ndescription: 'Audits: claims'\ntools: [\"Read\", 'Grep']\n---\t\nAudit the claims.",
	} {
		role, err := agents.Parse("auditor.md", []byte(data))
		if err != nil {
			t.Fatalf("%q: %v", data, err)
		}
		if role.Name != "auditor" || role.Description != "Audits: claims" || !slices.Equal(role.Tools, []string{"Read", "Grep"}) || role.Prompt != "Audit the claims." {
			t.Fatalf("%q parsed as %+v", data, role)
		}
	}
	for _, tc := range []struct{ file, data, want string }{
		{"a.md", "name: a\ndescription: d\n\nBody", "starts with a --- line"},
		{"a.md", "", "starts with a --- line"},
		{"a.md", "---\nname: a\ndescription: d\nBody", "no closing --- line"},
		{"a.md", "---\nname: a\ndescription: d\n---", "must be nonempty"},
		{"a.md", "---\nname: a\ndescription: d\nthinking: high\n---\nBody", `unknown frontmatter key "thinking"`},
		{"a.md", "---\nname: a\nname: a\ndescription: d\n---\nBody", "repeats name"},
		{"a.md", "---\nname a\ndescription: d\n---\nBody", "is not key: value"},
		{"a.md", "---\nname: b\ndescription: d\n---\nBody", `name "b" must match the file name, a`},
		{"A.md", "---\nname: A\ndescription: d\n---\nBody", `name "A" must be ` + agents.NameRule},
		{"1a.md", "---\nname: 1a\ndescription: d\n---\nBody", `name "1a" must be ` + agents.NameRule},
		{"a.md", "---\nname: a\n---\nBody", "description must be one line"},
		{"a.md", "---\nname: a\ndescription: " + strings.Repeat("d", 401) + "\n---\nBody", "description must be one line"},
		{"a.md", "---\nname: a\ndescription: d\ntools: Read, Read\n---\nBody", "distinct tool names"},
		{"a.md", "---\nname: a\ndescription: d\ntools:\n---\nBody", "distinct tool names"},
		{"a.md", "---\nname: a\ndescription: d\n---\n \n", "must be nonempty"},
		{"a.md", "---\nname: a\ndescription: d\n---\n" + strings.Repeat("x", 32<<10+1), "at most 32 KiB"},
		{"a.md", "---\nname: a\ndescription: d\n---\nBody\x00", "UTF-8 text"},
		{"a.md", "---\nname: a\ndescription: d\n---\nBody\xff", "UTF-8 text"},
	} {
		if _, err := agents.Parse(tc.file, []byte(tc.data)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: %v, want %q", tc.data, err, tc.want)
		}
	}
}

func TestLoadMergesTheUsersRoles(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("scout.md", "---\nname: scout\ndescription: My scout.\n---\nScout my way.\n")
	write("auditor.md", "---\nname: auditor\ndescription: Audits.\ntools: Read\n---\nAudit.\n")
	write("notes.txt", "not a role")
	write(".draft.md", "not yet a role")
	os.Mkdir(filepath.Join(dir, "old.md"), 0o700)
	roles, err := agents.Load(dir, childTools())
	if err != nil {
		t.Fatal(err)
	}
	if got := agents.Names(roles); !slices.Equal(got, []string{"auditor", "delegate", "oracle", "researcher", "reviewer", "scout", "worker"}) {
		t.Fatalf("roles %v", got)
	}
	scout, _ := agents.Find(roles, "scout")
	if scout.Description != "My scout." || scout.Prompt != "Scout my way." || scout.Tools != nil || scout.Source != filepath.Join(dir, "scout.md") {
		t.Fatalf("the user's scout did not replace the built-in one: %+v", scout)
	}
	if builtin, _ := agents.Find(agents.Builtin(), "scout"); builtin.Description == "My scout." {
		t.Fatal("a user role changed the built-in one")
	}

	write("broken.md", "---\nname: broken\n---\nNo description.\n")
	if _, err := agents.Load(dir, childTools()); err == nil || !strings.Contains(err.Error(), filepath.Join(dir, "broken.md")) {
		t.Fatalf("a broken role file is not named: %v", err)
	}
	os.Remove(filepath.Join(dir, "broken.md"))
	write("asker.md", "---\nname: asker\ndescription: Asks.\ntools: Read, Subagent\n---\nAsk.\n")
	if _, err := agents.Load(dir, childTools()); err == nil || !strings.Contains(err.Error(), `role asker lists tool "Subagent"`) {
		t.Fatalf("a tool children lack: %v", err)
	}
	os.Remove(filepath.Join(dir, "asker.md"))
	write("huge.md", "---\nname: huge\ndescription: d\n---\n"+strings.Repeat("x", 64<<10))
	if _, err := agents.Load(dir, childTools()); err == nil || !strings.Contains(err.Error(), "a role file is a regular file of at most 64 KiB") {
		t.Fatalf("an oversized role file: %v", err)
	}
	os.Remove(filepath.Join(dir, "huge.md"))
	if err := syscall.Mkfifo(filepath.Join(dir, "fifo.md"), 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := agents.Load(dir, childTools()); done <- err }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "a role file is a regular file") {
			t.Fatalf("a FIFO as a role: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("loading roles blocked on a FIFO")
	}
	os.Remove(filepath.Join(dir, "fifo.md"))
	for i := range 32 {
		name := fmt.Sprintf("role%d", i)
		write(name+".md", "---\nname: "+name+"\ndescription: d\n---\nBody\n")
	}
	if _, err := agents.Load(dir, childTools()); err == nil || !strings.Contains(err.Error(), "more than 32 roles") {
		t.Fatalf("too many roles: %v", err)
	}
}

func TestValidName(t *testing.T) {
	for _, name := range []string{"a", "scout", "code-reviewer", "r2", strings.Repeat("a", 32)} {
		if !agents.ValidName(name) {
			t.Errorf("%q refused", name)
		}
	}
	for _, name := range []string{"", "Scout", "2r", "-a", "a_b", "a b", "a.md", "ä", strings.Repeat("a", 33)} {
		if agents.ValidName(name) {
			t.Errorf("%q accepted", name)
		}
	}
}

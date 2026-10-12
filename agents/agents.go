// Package agents holds the subagent roles a delegated task may name. Each role
// is a Markdown file: frontmatter giving its name, a description the
// delegating agent chooses by, and optionally the only tools it may use, then
// the instructions its children add to the parent's. The roles in this
// directory are built in; files in the user's agents directory add to them or
// replace them by name.
package agents

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"unicode"
	"unicode/utf8"
)

//go:embed *.md
var builtin embed.FS

// Role is a kind of subagent: what it is for, the tools it may use, and the
// instructions its children run with.
type Role struct {
	Name        string
	Description string
	// Tools are the only tools the role's children may use. Empty is every
	// tool a child has.
	Tools []string
	// Prompt is the file's body, added to a child's instructions.
	Prompt string
	// Source is the user's file the role was read from; empty for a built-in role.
	Source string
}

const (
	maxNameLength        = 32
	maxDescriptionLength = 400
	maxPromptBytes       = 32 << 10
	maxFileBytes         = 64 << 10
	maxUserRoles         = 32
)

// Builtin returns the roles JARVIS ships, sorted by name.
func Builtin() []Role {
	entries, err := fs.ReadDir(builtin, ".")
	if err != nil {
		panic(err)
	}
	roles := make([]Role, 0, len(entries))
	for _, entry := range entries {
		data, err := builtin.ReadFile(entry.Name())
		if err != nil {
			panic(err)
		}
		role, err := Parse(entry.Name(), data)
		if err != nil {
			panic(err)
		}
		roles = append(roles, role)
	}
	return roles
}

// Load returns the built-in roles merged with the user's from dir, sorted by
// name: a user file replaces the built-in role of the same name. A missing dir
// adds none. Every role's tools must be among tools, the ones a child has.
func Load(dir string, tools []string) ([]Role, error) {
	roles := Builtin()
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		entries, err = nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read subagent roles: %w", err)
	}
	count := 0
	for _, entry := range entries {
		if entry.IsDir() || strings.HasPrefix(entry.Name(), ".") || filepath.Ext(entry.Name()) != ".md" {
			continue
		}
		if count++; count > maxUserRoles {
			return nil, fmt.Errorf("%s holds more than %d roles", dir, maxUserRoles)
		}
		path := filepath.Join(dir, entry.Name())
		data, err := readFile(path)
		if err != nil {
			return nil, err
		}
		role, err := Parse(entry.Name(), data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		role.Source = path
		if i := slices.IndexFunc(roles, func(r Role) bool { return r.Name == role.Name }); i >= 0 {
			roles[i] = role
		} else {
			roles = append(roles, role)
		}
	}
	slices.SortFunc(roles, func(a, b Role) int { return strings.Compare(a.Name, b.Name) })
	for _, role := range roles {
		for _, tool := range role.Tools {
			if !slices.Contains(tools, tool) {
				return nil, fmt.Errorf("role %s lists tool %q; a subagent's tools are %s", role.Name, tool, strings.Join(tools, ", "))
			}
		}
	}
	return roles, nil
}

// readFile reads a role file, refusing anything but a regular file of a
// role's size. A FIFO must not hang startup: it is opened without blocking and
// refused once open.
func readFile(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("read subagent role: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("read subagent role: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > maxFileBytes {
		return nil, fmt.Errorf("%s: a role file is a regular file of at most %d KiB", path, maxFileBytes>>10)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read subagent role %s: %w", path, err)
	}
	if len(data) > maxFileBytes {
		return nil, fmt.Errorf("%s: a role file is a regular file of at most %d KiB", path, maxFileBytes>>10)
	}
	return data, nil
}

// Find returns the role named name.
func Find(roles []Role, name string) (Role, bool) {
	i := slices.IndexFunc(roles, func(r Role) bool { return r.Name == name })
	if i < 0 {
		return Role{}, false
	}
	return roles[i], true
}

// Names lists the roles' names in order.
func Names(roles []Role) []string {
	names := make([]string, len(roles))
	for i, role := range roles {
		names[i] = role.Name
	}
	return names
}

// NameRule says what ValidName accepts.
const NameRule = "at most 32 lowercase letters, digits or hyphens, starting with a letter"

// ValidName reports whether name can name a role; see NameRule.
func ValidName(name string) bool {
	if name == "" || len(name) > maxNameLength || name[0] < 'a' || name[0] > 'z' {
		return false
	}
	return strings.IndexFunc(name, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-')
	}) < 0
}

// Parse reads a role from the contents of file, whose name, less its .md, is
// the role's. Frontmatter between --- lines holds one key per line: name and
// description, which are required, and tools, a comma-separated list.
func Parse(file string, data []byte) (Role, error) {
	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return Role{}, errors.New("a role file is UTF-8 text")
	}
	lines := strings.Split(strings.TrimPrefix(strings.ReplaceAll(string(data), "\r\n", "\n"), "\ufeff"), "\n")
	delimiter := func(line string) bool { return strings.TrimRight(line, " \t") == "---" }
	if !delimiter(lines[0]) {
		return Role{}, errors.New("a role file starts with a --- line, then its frontmatter")
	}
	end := slices.IndexFunc(lines[1:], delimiter) + 1
	if end == 0 {
		return Role{}, errors.New("the frontmatter has no closing --- line")
	}
	var role Role
	seen := map[string]bool{}
	for _, line := range lines[1:end] {
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		key, value = strings.TrimSpace(key), unquote(strings.TrimSpace(value))
		if !ok || key == "" {
			return Role{}, fmt.Errorf("frontmatter line %q is not key: value", line)
		}
		if seen[key] {
			return Role{}, fmt.Errorf("frontmatter repeats %s", key)
		}
		seen[key] = true
		switch key {
		case "name":
			role.Name = value
		case "description":
			role.Description = value
		case "tools":
			for tool := range strings.SplitSeq(strings.TrimSuffix(strings.TrimPrefix(value, "["), "]"), ",") {
				tool = unquote(strings.TrimSpace(tool))
				if tool == "" || slices.Contains(role.Tools, tool) {
					return Role{}, fmt.Errorf("tools %q must list distinct tool names separated by commas", value)
				}
				role.Tools = append(role.Tools, tool)
			}
		default:
			return Role{}, fmt.Errorf("unknown frontmatter key %q; a role has name, description and tools", key)
		}
	}
	if stem := strings.TrimSuffix(filepath.Base(file), ".md"); role.Name != stem {
		return Role{}, fmt.Errorf("name %q must match the file name, %s", role.Name, stem)
	}
	if !ValidName(role.Name) {
		return Role{}, fmt.Errorf("name %q must be %s", role.Name, NameRule)
	}
	if role.Description == "" || len(role.Description) > maxDescriptionLength || strings.IndexFunc(role.Description, unicode.IsControl) >= 0 {
		return Role{}, fmt.Errorf("description must be one line of at most %d bytes", maxDescriptionLength)
	}
	role.Prompt = strings.TrimSpace(strings.Join(lines[end+1:], "\n"))
	if role.Prompt == "" || len(role.Prompt) > maxPromptBytes {
		return Role{}, fmt.Errorf("the instructions after the frontmatter must be nonempty and at most %d KiB", maxPromptBytes>>10)
	}
	return role, nil
}

// unquote strips one pair of matching quotes, as YAML would.
func unquote(value string) string {
	if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
		return value[1 : len(value)-1]
	}
	return value
}

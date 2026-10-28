// Package skills finds the user's Agent Skills (https://agentskills.io) and
// reads them for the model.
//
// A skill is a folder holding a SKILL.md whose YAML frontmatter gives its
// name and a description of when to use it, followed by its instructions. The
// model sees every skill's name and description each turn and loads the
// instructions, and any file beside them, only when a task calls for them.
package skills

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/goccy/go-yaml"
)

const (
	// Entry is the file that makes a folder a skill.
	Entry = "SKILL.md"

	// MaxFileBytes bounds every file a skill offers, SKILL.md included.
	MaxFileBytes = 64 << 10

	// maxSkills bounds the catalog, which is in every turn's instructions.
	maxSkills = 64

	// maxListedFiles bounds the files listed beside a skill's instructions.
	maxListedFiles = 100

	maxNameLength        = 64
	maxDescriptionLength = 1024
)

var validName = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// DefaultDir is where the user's skills live: ~/.agents/skills.
func DefaultDir(home string) string {
	return filepath.Join(home, ".agents", "skills")
}

// Skill is one of the user's skills.
type Skill struct {
	Name        string
	Description string
	// Dir is the skill's folder with symlinks resolved; every file the skill
	// offers lies inside it.
	Dir string
	// UserOnly marks a skill whose frontmatter sets disable-model-invocation:
	// the user runs it as a slash command, and the model is never offered it.
	UserOnly bool
}

// Catalog is the skills found in one directory, sorted by name.
type Catalog struct {
	Dir    string
	Skills []Skill
}

// Discover finds the skills in dir, those only the user may run included
// (see Offered). A missing directory has none. A folder without SKILL.md is
// not a skill and is passed over; a skill whose SKILL.md is unreadable or
// malformed is left out with a warning.
func Discover(dir string) (Catalog, []string, error) {
	catalog := Catalog{Dir: dir}
	root, err := filepath.EvalSymlinks(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return catalog, nil, nil
	}
	if err != nil {
		return catalog, nil, fmt.Errorf("find skills in %q: %w", dir, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return catalog, nil, fmt.Errorf("find skills in %q: %w", dir, err)
	}

	var warnings []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		skill, found, err := load(root, entry.Name())
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("Skill %q not loaded: %v", entry.Name(), err))
			continue
		}
		if !found {
			continue
		}
		if len(catalog.Skills) == maxSkills {
			warnings = append(warnings, fmt.Sprintf("Only the first %d skills in %s are offered; the rest are left out.", maxSkills, dir))
			break
		}
		catalog.Skills = append(catalog.Skills, skill)
	}
	return catalog, warnings, nil
}

// load reads the skill in folder name under root. found is false for a
// folder that is not a skill.
func load(root, name string) (skill Skill, found bool, err error) {
	dir, err := filepath.EvalSymlinks(filepath.Join(root, name))
	if err != nil {
		return skill, false, err
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return skill, false, err
	}
	text, err := readText(filepath.Join(dir, Entry))
	if errors.Is(err, fs.ErrNotExist) {
		return skill, false, nil
	}
	if err != nil {
		return skill, false, err
	}

	header, err := parseFrontmatter(text)
	if err != nil {
		return skill, false, err
	}
	if err := header.validate(name); err != nil {
		return skill, false, err
	}
	description := strings.Join(strings.Fields(header.Description), " ")
	return Skill{Name: header.Name, Description: description, Dir: dir, UserOnly: header.DisableModelInvocation}, true, nil
}

// frontmatter is what JULIA uses from a SKILL.md header; other fields, such
// as license or metadata, are allowed and ignored.
type frontmatter struct {
	Name                   string `yaml:"name"`
	Description            string `yaml:"description"`
	DisableModelInvocation bool   `yaml:"disable-model-invocation"`
}

func parseFrontmatter(text string) (frontmatter, error) {
	var header frontmatter
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	if strings.TrimSpace(lines[0]) != "---" {
		return header, errors.New("SKILL.md must start with YAML frontmatter between --- lines")
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			if err := yaml.Unmarshal([]byte(strings.Join(lines[1:i], "\n")), &header); err != nil {
				return header, fmt.Errorf("SKILL.md frontmatter: %w", err)
			}
			return header, nil
		}
	}
	return header, errors.New("SKILL.md frontmatter has no closing --- line")
}

// validate applies the Agent Skills rules that matter to finding a skill by
// name: a short lowercase name that is also its folder's, and a description.
func (h frontmatter) validate(folder string) error {
	switch {
	case h.Name == "":
		return errors.New("SKILL.md frontmatter has no name")
	case len(h.Name) > maxNameLength || !validName.MatchString(h.Name):
		return fmt.Errorf("name %q must be at most %d lowercase letters, digits and single hyphens", h.Name, maxNameLength)
	case h.Name != folder:
		return fmt.Errorf("name %q must match its folder's name", h.Name)
	case strings.TrimSpace(h.Description) == "":
		return errors.New("SKILL.md frontmatter has no description")
	case utf8.RuneCountInString(h.Description) > maxDescriptionLength:
		return fmt.Errorf("description is longer than %d characters", maxDescriptionLength)
	}
	return nil
}

// Offered is the catalog the model sees: every skill but those only the
// user may run.
func (c Catalog) Offered() Catalog {
	offered := Catalog{Dir: c.Dir}
	for _, skill := range c.Skills {
		if !skill.UserOnly {
			offered.Skills = append(offered.Skills, skill)
		}
	}
	return offered
}

// With is the catalog with skill in it too, kept in name order; a skill of
// that name already there is replaced.
func (c Catalog) With(skill Skill) Catalog {
	skills := slices.DeleteFunc(slices.Clone(c.Skills), func(s Skill) bool { return s.Name == skill.Name })
	skills = append(skills, skill)
	slices.SortFunc(skills, func(a, b Skill) int { return strings.Compare(a.Name, b.Name) })
	return Catalog{Dir: c.Dir, Skills: skills}
}

// Find returns the skill named name.
func (c Catalog) Find(name string) (Skill, bool) {
	for _, skill := range c.Skills {
		if skill.Name == name {
			return skill, true
		}
	}
	return Skill{}, false
}

// Names lists the skills' names in catalog order.
func (c Catalog) Names() []string {
	names := make([]string, len(c.Skills))
	for i, skill := range c.Skills {
		names[i] = skill.Name
	}
	return names
}

// Read returns one file of a skill: its SKILL.md when file is empty, else a
// path relative to the skill's folder. Nothing outside that folder can be
// read, through ".." or through a symlink.
func (s Skill) Read(file string) (string, error) {
	if file == "" {
		file = Entry
	}
	if !filepath.IsLocal(file) {
		return "", fmt.Errorf("file %q must be a path inside the %s skill's folder", file, s.Name)
	}
	path, err := filepath.EvalSymlinks(filepath.Join(s.Dir, file))
	if errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("the %s skill has no file %q", s.Name, file)
	}
	if err != nil {
		return "", err
	}
	if relative, err := filepath.Rel(s.Dir, path); err != nil || !filepath.IsLocal(relative) {
		return "", fmt.Errorf("file %q leads outside the %s skill's folder", file, s.Name)
	}
	return readText(path)
}

// Files lists the files in a skill's folder other than SKILL.md, as paths
// relative to it, so the model knows what the instructions may refer to.
// Hidden files and folders are left out.
func (s Skill) Files() ([]string, error) {
	var files []string
	err := filepath.WalkDir(s.Dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path != s.Dir && strings.HasPrefix(entry.Name(), ".") {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(s.Dir, path)
		if err != nil || relative == Entry {
			return err
		}
		if len(files) == maxListedFiles {
			return fs.SkipAll
		}
		files = append(files, filepath.ToSlash(relative))
		return nil
	})
	return files, err
}

// readText reads a regular file of nonempty UTF-8 text of at most
// MaxFileBytes. path must already have its symlinks resolved.
func readText(path string) (string, error) {
	// O_NONBLOCK keeps a FIFO from hanging the turn; O_NOFOLLOW refuses a
	// symlink swapped in after path was resolved.
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return "", err
	}
	defer file.Close()

	invalid := fmt.Errorf("%q must be a file of nonempty UTF-8 text of at most %d KiB", path, MaxFileBytes>>10)
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() > MaxFileBytes {
		return "", invalid
	}
	data, err := io.ReadAll(io.LimitReader(file, MaxFileBytes+1))
	if err != nil {
		return "", fmt.Errorf("read %q: %w", path, err)
	}
	text := string(data)
	if len(data) > MaxFileBytes || !utf8.Valid(data) || strings.TrimSpace(text) == "" || strings.ContainsRune(text, 0) {
		return "", invalid
	}
	return text, nil
}

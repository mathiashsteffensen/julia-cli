package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// DefaultProfile is the profile config.toml or config.json holds.
const DefaultProfile = "default"

// ProfileRule describes a valid profile name, for error messages.
const ProfileRule = "1 to 64 letters, digits, '-' or '_'"

// ValidProfileName reports whether name may name a configuration profile.
func ValidProfileName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	return strings.IndexFunc(name, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_')
	}) < 0
}

// ProfilePath resolves a profile to TOML when it exists, otherwise JSON when
// it exists, otherwise the TOML path for a new profile. Errors other than a
// missing TOML file stay on that path so loading does not hide them.
func ProfilePath(dir, name string) string {
	base := "config"
	if name != DefaultProfile {
		base += "." + name
	}
	tomlPath := filepath.Join(dir, base+".toml")
	if _, err := os.Lstat(tomlPath); err == nil || !errors.Is(err, os.ErrNotExist) {
		return tomlPath
	}
	jsonPath := filepath.Join(dir, base+".json")
	if _, err := os.Lstat(jsonPath); err == nil || !errors.Is(err, os.ErrNotExist) {
		return jsonPath
	}
	return tomlPath
}

// ProfileName is the profile the file at path holds, and whether it holds one.
// The default is config.toml or config.json, not config.default.*.
func ProfileName(path string) (string, bool) {
	base := filepath.Base(path)
	if base == "config.json" || base == "config.toml" {
		return DefaultProfile, true
	}
	name, ok := strings.CutPrefix(base, "config.")
	if !ok {
		return "", false
	}
	ext := filepath.Ext(name)
	if ext != ".json" && ext != ".toml" {
		return "", false
	}
	name = strings.TrimSuffix(name, ext)
	if name == DefaultProfile || !ValidProfileName(name) {
		return "", false
	}
	return name, true
}

// Profiles lists the profiles in the configuration directory dir: the default
// first, which exists even without a file since Load then reads the
// defaults, then the others by name.
func Profiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	seen := make(map[string]bool)
	var names []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if name, ok := ProfileName(entry.Name()); ok && name != DefaultProfile && !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return append([]string{DefaultProfile}, names...), nil
}

// LoadProfile loads profile name from the configuration directory dir. Unlike
// the default profile, a named one must exist: a missing file is most likely
// a mistyped name, which must not silently run on the defaults.
func LoadProfile(dir, name string) (Config, error) {
	if !ValidProfileName(name) {
		return Default(), fmt.Errorf("profile %q is not a profile name, which is %s", name, ProfileRule)
	}
	path := ProfilePath(dir, name)
	if name != DefaultProfile {
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			return Default(), fmt.Errorf("profile %q not found: create %s", name, path)
		}
	}
	cfg, err := Load(path)
	if err != nil {
		return cfg, fmt.Errorf("profile %s: %w", name, err)
	}
	return cfg, nil
}

package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// DefaultProfile is the profile config.json holds.
const DefaultProfile = "default"

// ProfileRule describes a valid profile name, for error messages.
const ProfileRule = "1 to 64 letters, digits, '-' or '_'"

// ValidProfileName reports whether name may name a profile: config.<name>.json
// beside config.json, or config.json itself for DefaultProfile.
func ValidProfileName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	return strings.IndexFunc(name, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_')
	}) < 0
}

// ProfilePath is the file holding profile name in the configuration directory
// dir: config.json for the default profile, config.<name>.json for any other.
func ProfilePath(dir, name string) string {
	if name == DefaultProfile {
		return filepath.Join(dir, "config.json")
	}
	return filepath.Join(dir, "config."+name+".json")
}

// ProfileName is the profile the file at path holds, and whether it holds one:
// config.json is the default profile and config.<name>.json is name. Another
// file, such as one --config names elsewhere, is no profile. config.default.json
// is none either: the default profile is config.json.
func ProfileName(path string) (string, bool) {
	base := filepath.Base(path)
	if base == "config.json" {
		return DefaultProfile, true
	}
	name, ok := strings.CutPrefix(base, "config.")
	if !ok {
		return "", false
	}
	name, ok = strings.CutSuffix(name, ".json")
	if !ok || name == DefaultProfile || !ValidProfileName(name) {
		return "", false
	}
	return name, true
}

// Profiles lists the profiles in the configuration directory dir: the default
// first, which exists even without config.json since Load then reads the
// defaults, then the others by name.
func Profiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if name, ok := ProfileName(entry.Name()); ok && name != DefaultProfile {
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

package browser

import (
	"cmp"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
)

// macApps are the Chromium browsers looked for in /Applications and
// ~/Applications, most faithful first.
var macApps = []string{
	"Google Chrome", "Chromium", "Google Chrome for Testing", "Brave Browser", "Microsoft Edge",
	"Google Chrome Beta", "Google Chrome Dev", "Google Chrome Canary",
}

// linuxNames are the commands looked for on PATH on Linux.
var linuxNames = []string{
	"google-chrome-stable", "google-chrome", "chromium", "chromium-browser", "chrome",
	"brave-browser", "microsoft-edge-stable", "microsoft-edge",
}

// Find returns the Chrome or Chromium executable to launch. explicit, when
// set (from configuration or JARVIS_BROWSER), wins and must exist: a binary or
// a macOS .app bundle. Otherwise Find looks for an installed browser (Google
// Chrome, Chromium, Chrome for Testing, Brave, Edge) in /Applications and
// ~/Applications, then in Playwright's and Puppeteer's caches (the newest full
// Chrome for Testing before any headless shell, since full Chrome renders most
// faithfully), then on Linux for the usual commands on PATH.
func Find(explicit string) (string, error) {
	home, _ := os.UserHomeDir()
	return finder{
		explicit:   explicit,
		goos:       runtime.GOOS,
		goarch:     runtime.GOARCH,
		home:       home,
		playwright: os.Getenv("PLAYWRIGHT_BROWSERS_PATH"),
		lookPath:   exec.LookPath,
		apps:       []string{"/Applications", filepath.Join(home, "Applications")},
	}.find()
}

type finder struct {
	explicit, goos, goarch, home, playwright string
	lookPath                                 func(string) (string, error)
	apps                                     []string // macOS application folders
}

func (f finder) find() (string, error) {
	if f.explicit != "" {
		path := expandHome(f.explicit, f.home)
		if strings.HasSuffix(strings.TrimSuffix(path, "/"), ".app") {
			path = appExecutable(path)
		}
		if err := executable(path); err != nil {
			return "", fmt.Errorf("browser %s: %w", f.explicit, err)
		}
		return path, nil
	}
	var looked []string
	if f.goos == "darwin" {
		for _, dir := range f.apps {
			looked = append(looked, dir)
			for _, app := range macApps {
				path := filepath.Join(dir, app+".app", "Contents", "MacOS", app)
				if executable(path) == nil {
					return path, nil
				}
			}
		}
	}
	if cache := f.playwrightCache(); cache != "" {
		looked = append(looked, cache)
		if path := f.newest(cache, "chromium-", f.fullPatterns()); path != "" {
			return path, nil
		}
		if path := f.newest(cache, "chromium_headless_shell-", f.shellPatterns()); path != "" {
			return path, nil
		}
	}
	if f.home != "" {
		cache := filepath.Join(f.home, ".cache", "puppeteer")
		looked = append(looked, cache)
		if path := f.newest(filepath.Join(cache, "chrome"), "", f.fullPatterns()); path != "" {
			return path, nil
		}
		if path := f.newest(filepath.Join(cache, "chrome-headless-shell"), "", f.shellPatterns()); path != "" {
			return path, nil
		}
	}
	if f.goos == "linux" {
		looked = append(looked, "PATH ("+strings.Join(linuxNames[:5], ", ")+", …)")
		for _, name := range linuxNames {
			if path, err := f.lookPath(name); err == nil {
				return path, nil
			}
		}
	}
	return "", fmt.Errorf("no Chrome or Chromium found (looked in %s). Install one with `npx playwright install chromium`, or point JARVIS_BROWSER at a Chrome executable", strings.Join(looked, ", "))
}

func (f finder) playwrightCache() string {
	if f.playwright != "" && f.playwright != "0" {
		return expandHome(f.playwright, f.home)
	}
	if f.home == "" {
		return ""
	}
	if f.goos == "darwin" {
		return filepath.Join(f.home, "Library", "Caches", "ms-playwright")
	}
	return filepath.Join(f.home, ".cache", "ms-playwright")
}

func (f finder) fullPatterns() []string {
	if f.goos == "darwin" {
		return []string{"*/Google Chrome for Testing.app/Contents/MacOS/Google Chrome for Testing", "*/Chromium.app/Contents/MacOS/Chromium"}
	}
	return []string{"*/chrome"}
}

func (f finder) shellPatterns() []string {
	return []string{"*/chrome-headless-shell", "*/headless_shell"}
}

// newest finds the first executable matching patterns inside the
// subdirectories of dir named prefix+version, newest version first. Within
// one version a build for this machine's architecture wins over one that
// would run under emulation.
func (f finder) newest(dir, prefix string, patterns []string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	type candidate struct {
		dir     string
		version []int
	}
	var candidates []candidate
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || !strings.HasPrefix(name, prefix) {
			continue
		}
		candidates = append(candidates, candidate{filepath.Join(dir, name), versionOf(strings.TrimPrefix(name, prefix))})
	}
	slices.SortFunc(candidates, func(a, b candidate) int { return -slices.Compare(a.version, b.version) })
	native := "x64"
	if f.goarch == "arm64" {
		native = "arm64"
	}
	for _, c := range candidates {
		var found []string
		for _, pattern := range patterns {
			matches, _ := filepath.Glob(filepath.Join(c.dir, pattern))
			found = append(found, matches...)
		}
		slices.SortStableFunc(found, func(a, b string) int {
			return cmp.Compare(boolInt(!strings.Contains(a, native)), boolInt(!strings.Contains(b, native)))
		})
		for _, path := range found {
			if executable(path) == nil {
				return path
			}
		}
	}
	return ""
}

// versionOf reads the numbers in a cache directory's version ("1234",
// "mac_arm-151.0.7922.34") for ordering.
func versionOf(s string) []int {
	var v []int
	for _, field := range strings.FieldsFunc(s, func(r rune) bool { return r < '0' || r > '9' }) {
		n, _ := strconv.Atoi(field)
		v = append(v, n)
	}
	return v
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func expandHome(path, home string) string {
	if home != "" && (path == "~" || strings.HasPrefix(path, "~/")) {
		return filepath.Join(home, strings.TrimPrefix(path, "~"))
	}
	return path
}

// appExecutable finds the executable inside a macOS .app bundle: the one
// named like the bundle, else the only one in Contents/MacOS.
func appExecutable(app string) string {
	dir := filepath.Join(app, "Contents", "MacOS")
	named := filepath.Join(dir, strings.TrimSuffix(filepath.Base(strings.TrimSuffix(app, "/")), ".app"))
	if executable(named) == nil {
		return named
	}
	entries, err := os.ReadDir(dir)
	if err == nil && len(entries) == 1 {
		return filepath.Join(dir, entries[0].Name())
	}
	return named
}

func executable(path string) error {
	info, err := os.Stat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return errors.New("not found")
	case err != nil:
		return err
	case !info.Mode().IsRegular():
		return errors.New("not a file")
	case info.Mode().Perm()&0o111 == 0:
		return errors.New("not executable")
	}
	return nil
}

// isHeadlessShell reports whether path is chrome-headless-shell, the
// separate old-headless build, rather than a full Chrome.
func isHeadlessShell(path string) bool {
	base := filepath.Base(path)
	return strings.Contains(base, "headless_shell") || strings.Contains(base, "headless-shell")
}

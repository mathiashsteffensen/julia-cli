package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

const testGitHubToken = "gh_test_1234567890abcdef"

func fakeGitHubCLI(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestGitHubAuthEnvironment(t *testing.T) {
	for _, tc := range []struct {
		name, gh, github, source, token string
		invalid                         bool
	}{
		{"first wins", testGitHubToken, "github_123456789abcdef", "GH_TOKEN", testGitHubToken, false},
		{"second selected", "", testGitHubToken, "GITHUB_TOKEN", testGitHubToken, false},
		{"short fails closed", "short", testGitHubToken, "", "", true},
		{"space fails closed", "invalid token 123456789", testGitHubToken, "", "", true},
		{"newline fails closed", "invalidtoken123456\n", testGitHubToken, "", "", true},
		{"unicode fails closed", "invalidtoken123456é", testGitHubToken, "", "", true},
		{"large fails closed", strings.Repeat("a", 4097), testGitHubToken, "", "", true},
		{"invalid second", "", "bad", "", "", true},
		{"lower bound", strings.Repeat("x", 16), "", "GH_TOKEN", strings.Repeat("x", 16), false},
		{"upper bound", strings.Repeat("x", 4096), "", "GH_TOKEN", strings.Repeat("x", 4096), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GH_TOKEN", tc.gh)
			t.Setenv("GITHUB_TOKEN", tc.github)
			t.Setenv("PATH", t.TempDir())
			auth, err := LoadGitHubAuth()
			if tc.invalid {
				if err == nil || auth.Enabled() {
					t.Fatalf("invalid credential accepted: auth=%+v err=%v", auth, err)
				}
				if strings.Contains(err.Error(), tc.gh) && tc.gh != "" || strings.Contains(err.Error(), tc.github) && tc.github != "" {
					t.Fatal("error exposed credential")
				}
				return
			}
			if err != nil || !auth.Enabled() || auth.Summary() != tc.source {
				t.Fatalf("discovery: enabled=%v summary=%q err=%v", auth.Enabled(), auth.Summary(), err)
			}
			got, err := auth.resolve(context.Background())
			if err != nil || got != tc.token {
				t.Fatalf("resolve failed or returned wrong token: %v", err)
			}
			if strings.Contains(auth.Summary(), tc.token) {
				t.Fatal("summary exposed credential")
			}
		})
	}
}

func TestGitHubAuthDiscoveryAndCommand(t *testing.T) {
	dir := t.TempDir()
	fake := fakeGitHubCLI(t, `printf '%s\n' "$@" > "$GH_CONFIG_DIR/args"
/usr/bin/env > "$GH_CONFIG_DIR/env"
pwd > "$GH_CONFIG_DIR/cwd"
printf '%s\n' 'gh_test_1234567890abcdef'`)
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "xdg"))
	t.Setenv("GH_CONFIG_DIR", dir)
	t.Setenv("PATH", filepath.Dir(fake))
	t.Setenv("GH_DEBUG", "api")
	t.Setenv("GH_ENTERPRISE_TOKEN", "should_not_be_inherited")
	t.Setenv("UNRELATED_SECRET", "should_not_be_inherited")

	auth, err := LoadGitHubAuth()
	if err != nil || !auth.Enabled() || auth.Summary() != "gh login" || auth.binary != fake || !filepath.IsAbs(auth.binary) {
		t.Fatalf("unexpected discovery: enabled=%v summary=%q binary=%q err=%v", auth.Enabled(), auth.Summary(), auth.binary, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "args")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("discovery executed gh: %v", err)
	}
	got, err := auth.resolve(context.Background())
	if err != nil || got != testGitHubToken {
		t.Fatalf("resolve failed or returned wrong token: %v", err)
	}
	read := func(name string) string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSuffix(string(data), "\n")
	}
	if got := read("args"); got != "auth\ntoken\n--hostname\ngithub.com" {
		t.Fatalf("unexpected arguments: %q", got)
	}
	if got := read("cwd"); got != string(filepath.Separator) {
		t.Fatalf("unexpected working directory: %q", got)
	}
	env := strings.Split(read("env"), "\n")
	want := []string{
		"HOME=" + dir, "XDG_CONFIG_HOME=" + filepath.Join(dir, "xdg"), "GH_CONFIG_DIR=" + dir,
		"PATH=" + filepath.Dir(fake), "GH_PROMPT_DISABLED=1", "GH_NO_UPDATE_NOTIFIER=1",
		"GH_NO_EXTENSION_UPDATE_NOTIFIER=1", "NO_COLOR=1", "GH_TELEMETRY=false",
	}
	// Shell implementations may add these variables before invoking env.
	filtered := env[:0]
	for _, item := range env {
		if strings.HasPrefix(item, "PWD=") || strings.HasPrefix(item, "SHLVL=") || strings.HasPrefix(item, "_=") {
			continue
		}
		filtered = append(filtered, item)
	}
	env = filtered
	sort.Strings(env)
	sort.Strings(want)
	if !reflect.DeepEqual(env, want) {
		t.Fatalf("unexpected environment: got %q want %q", env, want)
	}
}

func TestGitHubAuthNoExecutable(t *testing.T) {
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("PATH", t.TempDir())
	auth, err := LoadGitHubAuth()
	if err != nil || auth.Enabled() || auth.Summary() != "" {
		t.Fatalf("unexpected auth: %+v, %v", auth, err)
	}
	if _, err := auth.resolve(context.Background()); err == nil {
		t.Fatal("resolve succeeded without authentication")
	}
}

func TestGitHubAuthResolveErrors(t *testing.T) {
	for _, tc := range []struct {
		name, script, message string
	}{
		{"exit and redaction", "printf '%s' 'secret-token-on-stdout'; printf '%s' 'secret-token-on-stderr' >&2; exit 1", "failed"},
		{"invalid output", "printf '%s' 'secret-token-with spaces'", "invalid token"},
		{"oversized output", "/usr/bin/head -c 100000 /dev/zero", "too large"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			auth := GitHubAuth{binary: fakeGitHubCLI(t, tc.script), source: "gh login"}
			got, err := auth.resolve(context.Background())
			if err == nil || got != "" || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("got token=%q err=%v", got, err)
			}
			if strings.Contains(err.Error(), "secret-token") {
				t.Fatal("error exposed credential output")
			}
		})
	}
}

func TestGitHubAuthResolveCancellation(t *testing.T) {
	auth := GitHubAuth{binary: fakeGitHubCLI(t, "exec /bin/sleep 10"), source: "gh login"}
	for _, tc := range []struct {
		name string
		ctx  func() (context.Context, context.CancelFunc)
		want error
	}{
		{"canceled", func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx, cancel
		}, context.Canceled},
		{"deadline", func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), 50*time.Millisecond)
		}, context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := tc.ctx()
			defer cancel()
			if _, err := auth.resolve(ctx); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

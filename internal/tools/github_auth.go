package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	githubTokenMinLength = 16
	githubTokenMaxLength = 4096
	githubAuthTimeout    = 15 * time.Second
)

// GitHubAuth selects a credential source without reading the gh login at
// discovery time. Its token and process output must never appear in notices.
type GitHubAuth struct {
	token  string
	binary string
	source string
}

// LoadGitHubAuth discovers the environment or gh source without running gh or
// contacting GitHub. An invalid selected environment token is an error, never a
// fallback to the gh login, which may be a different account.
func LoadGitHubAuth() (GitHubAuth, error) {
	for _, name := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
		if token := os.Getenv(name); token != "" {
			if !validGitHubToken(token) {
				return GitHubAuth{}, fmt.Errorf("invalid %s: expected a printable token of 16 to 4096 bytes without spaces", name)
			}
			return GitHubAuth{token: token, source: name}, nil
		}
	}

	binary, err := exec.LookPath("gh")
	if errors.Is(err, exec.ErrNotFound) {
		return GitHubAuth{}, nil
	}
	if err != nil {
		return GitHubAuth{}, errors.New("cannot locate gh executable safely")
	}
	binary, err = filepath.Abs(binary)
	if err != nil {
		return GitHubAuth{}, errors.New("cannot resolve gh executable path")
	}
	return GitHubAuth{binary: binary, source: "gh login"}, nil
}

// Enabled reports whether a credential source was discovered, not whether the
// gh login is valid or has access to any particular repository.
func (a GitHubAuth) Enabled() bool {
	return a.token != "" || a.binary != ""
}

// Summary identifies only the selected source, never the credential.
func (a GitHubAuth) Summary() string {
	return a.source
}

// resolve reads the selected credential only when it is needed. gh receives a
// curated environment so debug settings and token overrides cannot expose or
// replace the selected login. Neither stdout nor stderr is included in errors.
func (a GitHubAuth) resolve(ctx context.Context) (string, error) {
	if a.token != "" {
		return a.token, nil
	}
	if a.binary == "" {
		return "", errors.New("GitHub authentication is not configured")
	}

	ctx, cancel := context.WithTimeout(ctx, githubAuthTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, a.binary, "auth", "token", "--hostname", "github.com")
	cmd.Dir = string(filepath.Separator)
	cmd.Env = []string{
		"GH_PROMPT_DISABLED=1",
		"GH_NO_UPDATE_NOTIFIER=1",
		"GH_NO_EXTENSION_UPDATE_NOTIFIER=1",
		"NO_COLOR=1",
		"GH_TELEMETRY=false",
	}
	for _, name := range []string{"HOME", "XDG_CONFIG_HOME", "GH_CONFIG_DIR", "PATH"} {
		if value, ok := os.LookupEnv(name); ok {
			cmd.Env = append(cmd.Env, name+"="+value)
		}
	}
	cmd.WaitDelay = 2 * time.Second
	var stdout githubAuthOutput
	cmd.Stdout = &stdout
	// A nil Stderr discards it; it may contain credentials or sensitive config.
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("gh authentication canceled or timed out: %w", ctx.Err())
		}
		if stdout.overflow {
			return "", errors.New("gh authentication output is too large")
		}
		return "", errors.New("gh authentication failed")
	}
	if stdout.overflow {
		return "", errors.New("gh authentication output is too large")
	}
	token := strings.TrimSuffix(stdout.buffer.String(), "\n")
	token = strings.TrimSuffix(token, "\r")
	if !validGitHubToken(token) {
		return "", errors.New("gh authentication returned an invalid token")
	}
	return token, nil
}

func validGitHubToken(token string) bool {
	if len(token) < githubTokenMinLength || len(token) > githubTokenMaxLength {
		return false
	}
	for i := 0; i < len(token); i++ {
		if token[i] < '!' || token[i] > '~' {
			return false
		}
	}
	return true
}

// githubAuthOutput bounds memory even if the executable produces endless output.
// The extra bytes permit the newline gh appends to its token.
type githubAuthOutput struct {
	buffer   bytes.Buffer
	overflow bool
}

func (out *githubAuthOutput) Write(p []byte) (int, error) {
	if len(p) > githubTokenMaxLength+2-out.buffer.Len() {
		out.overflow = true
		return 0, errors.New("authentication output limit exceeded")
	}
	return out.buffer.Write(p)
}

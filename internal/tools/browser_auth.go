package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"time"

	"julia/internal/browser"
)

// Browser sign-in. The user's browser_auth configuration names, for each
// origin, the headers its requests carry, each value a string bash expands as
// it would a double-quoted one: "Bearer $(gcloud secrets versions access …)".
// The values run on the host with JULIA's own environment, as the
// configuration is the user's own, and the agent never sees what they expand
// to.
//
// An origin's headers are expanded the first time a turn may reach it (once
// it is approved, or is a server the agent's terminals started) and kept for
// the rest of the turn. They go only with the origin's requests of itself and
// with top-level GET navigations to it: another site's page cannot act there
// with the user's credentials, and a redirect away from the origin is a
// request of its own, which goes without them.

// browserAuth is an origin's expanded headers, or why they could not be.
type browserAuth struct {
	headers map[string]string
	err     error
}

const (
	browserAuthTimeout = 60 * time.Second
	maxBrowserAuthOut  = 16 * 1024
)

// signIn expands the headers of every configured origin the browser may now
// reach and this turn has not expanded yet, and says what came of it.
func (e *Executor) signIn(ctx context.Context) string {
	st := e.browserState()
	var lines []string

	for _, origin := range slices.Sorted(maps.Keys(e.config.BrowserAuth)) {
		if st.signInTried(origin) || !st.reachable(origin) {
			continue
		}

		values := e.config.BrowserAuth[origin]
		names := strings.Join(slices.Sorted(maps.Keys(values)), ", ")
		e.emit("status", "Signing the browser in to "+origin+"…")

		headers, err := expandHeaders(ctx, values)
		st.setSignIn(origin, browserAuth{headers: headers, err: err})

		if err != nil {
			lines = append(lines, fmt.Sprintf("browser_auth for %s failed, so its requests go without %s: %v", origin, names, err))
		} else {
			lines = append(lines, fmt.Sprintf("Signed in to %s: its requests carry %s from browser_auth.", origin, names))
		}
	}

	return strings.Join(lines, "\n")
}

// signInNotice tells whoever approves origins which of them browser_auth
// would sign in to, and with which headers.
func (e *Executor) signInNotice(origins ...string) string {
	var parts []string
	for _, origin := range origins {
		if values, ok := e.config.BrowserAuth[origin]; ok {
			parts = append(parts, origin+" ("+strings.Join(slices.Sorted(maps.Keys(values)), ", ")+")")
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return " The user's browser_auth configuration signs the browser in to " + strings.Join(parts, ", ") + " with those headers."
}

func (st *browserState) signInTried(origin string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	_, ok := st.auth[origin]
	return ok
}

func (st *browserState) setSignIn(origin string, a browserAuth) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.auth == nil {
		st.auth = map[string]browserAuth{}
	}
	st.auth[origin] = a
}

// reachable reports whether the browser may load origin without asking.
func (st *browserState) reachable(origin string) bool {
	u, err := url.Parse(origin)
	return err == nil && (st.approved(origin) || st.terminalServer(u))
}

// allowSigned allows r to origin, with the origin's headers when r is the
// origin's request of itself or a top-level GET navigation to it.
func (st *browserState) allowSigned(r browser.Request, origin string) browser.Decision {
	allow := browser.Decision{Allow: true}

	st.mu.Lock()
	headers := st.auth[origin].headers
	st.mu.Unlock()
	if headers == nil {
		return allow
	}

	own := requestOrigin(r.From) == origin
	navigation := r.MainFrame && r.Navigation && r.Method == "GET"
	if own || navigation {
		allow.Headers = headers
	}
	return allow
}

// requestOrigin is the origin of the document or worker a request comes
// from, "" for one without a network origin.
func requestOrigin(from string) string {
	u, err := url.Parse(from)
	if err != nil {
		return ""
	}
	if strings.EqualFold(u.Scheme, "blob") {
		if u, err = url.Parse(u.Opaque); err != nil {
			return ""
		}
	}
	origin, _ := browserOrigin(u)
	return origin
}

func expandHeaders(ctx context.Context, values map[string]string) (map[string]string, error) {
	headers := make(map[string]string, len(values))
	for _, name := range slices.Sorted(maps.Keys(values)) {
		value, err := expandHeader(ctx, values[name])
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		headers[name] = value
	}
	return headers, nil
}

// expandHeader has bash expand value as a double-quoted string, as
// bash -c 'echo "<value>"' would, except that a command it runs failing
// fails the expansion rather than leaving a hole in the header. It runs in
// the home directory with JULIA's environment.
func expandHeader(ctx context.Context, value string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, browserAuthTimeout)
	defer cancel()

	// An assignment exits as the last command substitution in it did, which
	// set -e makes bash's own exit.
	script := "set -e\nvalue=\"" + value + "\"\nprintf '%s' \"$value\""
	cmd := exec.CommandContext(ctx, "/bin/bash", "--noprofile", "--norc", "-c", script)
	if home, err := os.UserHomeDir(); err == nil {
		cmd.Dir = home
	}

	// What the value runs (gcloud, say) is stopped with bash.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second

	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	if err := cmd.Run(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("took longer than %s", browserAuthTimeout)
		}
		if detail := clipText(stderr.String(), 300); detail != "" {
			return "", fmt.Errorf("%v: %s", err, detail)
		}
		return "", err
	}

	out := stdout.String()
	switch {
	case out == "":
		return "", errors.New("expanded to nothing")
	case len(out) > maxBrowserAuthOut:
		return "", fmt.Errorf("expanded to more than %d bytes", maxBrowserAuthOut)
	case strings.ContainsAny(out, "\r\n\x00"):
		return "", errors.New("expanded to more than one line")
	}
	return out, nil
}

package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"julia/internal/core"
)

func TestReviewFallsBackToHuman(t *testing.T) {
	calls := 0
	human := func(_ context.Context, a core.Approval) (core.Decision, error) {
		calls++
		if !a.Network || a.Task != "research" {
			t.Errorf("lost approval context: %+v", a)
		}
		return core.Decision{Allow: false}, nil
	}
	e, _ := fixture(t, "auto", human)
	e.SetTask("research")
	e.config.Review = func(context.Context, core.Approval) (core.Decision, error) {
		return core.Decision{}, errors.New("unavailable")
	}
	if r := invoke(e, "Bash", map[string]any{"command": "true", "network": true}); !r.IsError || calls != 1 {
		t.Fatal(r, calls)
	}
}
func TestSeatbeltProfile(t *testing.T) {
	p := seatbeltProfile("/tmp/project", "/tmp/run", "/tmp/scratch", offline, []int{5432})
	if !strings.Contains(p, "(deny default)") || strings.Contains(p, "network") || !strings.Contains(p, "(deny file-read* file-write*") {
		t.Fatal(p)
	}
	p = seatbeltProfile("/tmp/project", "/tmp/run", "/tmp/scratch", online, nil)
	if !strings.Contains(p, "(allow network*)") {
		t.Fatal("missing explicit network grant")
	}
	p = seatbeltProfile("/tmp/project", "/tmp/run", "/tmp/scratch", loopbackOnly, []int{5432, 6379})
	if strings.Contains(p, "(allow network*)") || !strings.Contains(p, `(allow network-outbound (remote tcp "localhost:*") (remote udp "localhost:*"))`) || !strings.Contains(p, `(allow network-bind network-inbound (local ip "localhost:*"))`) {
		t.Fatal("loopback grants more or less than localhost:\n" + p)
	}
	// The machine's own services are closed, after the grant they narrow.
	if i := strings.Index(p, `(deny network-outbound (remote tcp "localhost:5432") (remote udp "localhost:5432"))`); i < strings.Index(p, `(allow network-outbound`) || !strings.Contains(p, `(deny network-outbound (remote tcp "localhost:6379") (remote udp "localhost:6379"))`) {
		t.Fatal("loopback reaches the machine's own services:\n" + p)
	}
}

// Run with JULIA_SANDBOX_TEST=1 on a host that permits launching an OS sandbox.
// Some parent sandboxes forbid nested sandbox creation; default unit tests do not
// mistake that environmental limitation for successful containment.
func TestSandboxIntegration(t *testing.T) {
	if os.Getenv("JULIA_SANDBOX_TEST") != "1" {
		t.Skip("set JULIA_SANDBOX_TEST=1 for host sandbox integration")
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("unsupported OS")
	}
	e, dir := fixture(t, "auto", nil)
	if err := e.CheckSandbox(context.Background()); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret")
	os.WriteFile(outside, []byte("outside-secret"), 0600)
	os.WriteFile(filepath.Join(dir, ".env"), []byte("inside-secret"), 0600)
	tests := []struct {
		name, command string
		fail          bool
	}{
		{"workspace write", "printf allowed > allowed; cat allowed", false},
		{"outside read", "cat " + outside, true},
		{"secret read", "cat .env", true},
		{"secret case alias", "cat .ENV", true},
		{"secret symlink", "ln -s .env alias; cat alias", true},
		{"secret hardlink", "ln .env hardalias; cat hardalias", true},
		{"secret write", "printf forbidden > .env", true},
		{"outside write", "printf forbidden > " + outside, true},
		{"no network", "/usr/bin/curl --max-time 2 -I https://example.com", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := e.runSandbox(context.Background(), tt.command, false, 5*time.Second)
			if (err != nil) != tt.fail {
				t.Fatalf("output=%q err=%v", out, err)
			}
		})
	}
	start := time.Now()
	_, err := e.runSandbox(context.Background(), "sleep 30 & wait", false, 150*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 3*time.Second {
		t.Fatal("timeout failed", err, time.Since(start))
	}
	t.Setenv("JULIA_TEST_SECRET", "must-not-leak")
	out, err := e.runSandbox(context.Background(), "printf '%s' \"${JULIA_TEST_SECRET-unset}\"", false, 5*time.Second)
	if err != nil || out != "unset" {
		t.Fatal("environment leaked", out, err)
	}
	if os.Getenv("JULIA_NETWORK_TEST") == "1" {
		out, err = e.runSandbox(context.Background(), "/usr/bin/curl --fail --silent --show-error --max-time 15 https://example.com", true, 20*time.Second)
		if err != nil || !strings.Contains(out, "Example Domain") {
			t.Fatal("authorized HTTPS request failed", out, err)
		}
	}
}

// Run with JULIA_SANDBOX_TEST=1. A loopback command serves and connects on
// localhost, IPv4 and IPv6, and reaches nothing else; an offline one cannot
// even listen.
func TestSandboxIntegrationLoopback(t *testing.T) {
	if os.Getenv("JULIA_SANDBOX_TEST") != "1" || runtime.GOOS != "darwin" {
		t.Skip("set JULIA_SANDBOX_TEST=1 for host macOS sandbox integration")
	}
	e, _ := fixture(t, "auto", nil)
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "host-server") }))
	defer host.Close()
	port := func() string {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		_, p, _ := net.SplitHostPort(l.Addr().String())
		return p
	}
	serve := func(addr string) string {
		p := port()
		return fmt.Sprintf("/usr/bin/nc -l %s %s > got & sleep 0.5; printf served | /usr/bin/nc -w 2 %s %s; wait; cat got; grep -q served got", addr, p, addr, p)
	}
	for _, tc := range []struct {
		name, command string
		reach         netAccess
		want          string // output that must appear; empty means the command must fail
	}{
		{"listen and connect on 127.0.0.1", serve("127.0.0.1"), loopbackOnly, "served"},
		{"listen and connect on ::1", serve("::1"), loopbackOnly, "served"},
		// A service already running here, such as a database that trusts
		// local clients, is out of reach: the browser's proxy is one.
		{"no host server on loopback", "/usr/bin/curl --silent --show-error --max-time 5 " + host.URL, loopbackOnly, ""},
		{"no host server by name", "/usr/bin/curl --silent --show-error --max-time 5 " + strings.Replace(host.URL, "127.0.0.1", "localhost", 1), loopbackOnly, ""},
		{"no connection beyond loopback", "/usr/bin/nc -z -w 3 1.1.1.1 80", loopbackOnly, ""},
		{"no UDP beyond loopback", "printf x | /usr/bin/nc -u -w 1 8.8.8.8 53 && /usr/bin/nc -z -u -w 1 8.8.8.8 53", loopbackOnly, ""},
		{"no curl beyond loopback", "/usr/bin/curl --silent --show-error --max-time 5 http://1.1.1.1/", loopbackOnly, ""},
		{"offline cannot listen", serve("127.0.0.1"), offline, ""},
		{"offline cannot connect to loopback", "/usr/bin/curl --silent --show-error --max-time 5 " + host.URL, offline, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := e.runBash(context.Background(), tc.command, tc.reach, 15*time.Second, false)
			if tc.want == "" && err == nil {
				t.Fatalf("succeeded: %q", out)
			}
			if tc.want != "" && (err != nil || !strings.Contains(out, tc.want)) {
				t.Fatalf("output=%q err=%v", out, err)
			}
		})
	}
	// A server a loopback terminal of the executor runs is the agent's own:
	// a loopback command reaches it, and a later terminal too.
	start := terminalCall(t, e, map[string]any{"action": "start", "command": "exec python3 -m http.server 0 --bind 127.0.0.1", "loopback": true, "wait_for": `port \d+`, "timeout": 20})
	m := regexp.MustCompile(`port (\d+)`).FindStringSubmatch(start.Content)
	if start.IsError || m == nil {
		t.Fatalf("server: %s", start.Content)
	}
	defer terminalCall(t, e, map[string]any{"action": "stop", "id": "t1"})
	out, err := e.runBash(context.Background(), "/usr/bin/curl --silent --show-error --max-time 5 -o /dev/null -w '%{http_code}' http://localhost:"+m[1]+"/", loopbackOnly, 15*time.Second, false)
	if err != nil || out != "200" {
		t.Fatalf("the terminal's own server: %q, %v", out, err)
	}
}

// The machine's own listeners are closed to loopback commands; a terminal's
// are not.
func TestHostPorts(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sockets are listed on macOS only")
	}
	e, _ := fixture(t, "auto", nil)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	ports, err := e.hostPorts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(ports, l.Addr().(*net.TCPAddr).Port) {
		t.Fatalf("this process's listener %s is not closed: %v", l.Addr(), ports)
	}
}

func TestSandboxGitStatus(t *testing.T) {
	if os.Getenv("JULIA_SANDBOX_TEST") != "1" || runtime.GOOS != "darwin" {
		t.Skip("requires host macOS sandbox integration")
	}
	e, dir := fixture(t, "auto", nil)
	e.config.Review = func(context.Context, core.Approval) (core.Decision, error) {
		return core.Decision{Allow: true, Reason: "read repository state"}, nil
	}
	init := exec.Command("/usr/bin/git", "init", "--quiet", dir)
	if output, err := init.CombinedOutput(); err != nil {
		t.Fatalf("fixture git init: %v: %s", err, output)
	}
	if err := os.WriteFile(filepath.Join(dir, "untracked.txt"), []byte("hello"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("before"), 0600); err != nil {
		t.Fatal(err)
	}
	add := exec.Command("/usr/bin/git", "-C", dir, "add", "tracked.txt")
	if out, err := add.CombinedOutput(); err != nil {
		t.Fatalf("fixture git add: %v: %s", err, out)
	}
	indexBefore, err := os.ReadFile(filepath.Join(dir, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("after"), 0600); err != nil {
		t.Fatal(err)
	}
	// A repository-supplied fsmonitor hook must not run during ordinary status.
	hook := filepath.Join(dir, "monitor.sh")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nprintf unsafe > monitor-ran\n"), 0700); err != nil {
		t.Fatal(err)
	}
	config := exec.Command("/usr/bin/git", "-C", dir, "config", "core.fsmonitor", hook)
	if out, err := config.CombinedOutput(); err != nil {
		t.Fatalf("fixture fsmonitor: %v: %s", err, out)
	}
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.fsmonitor")
	t.Setenv("GIT_CONFIG_VALUE_0", hook)
	result := invoke(e, "Bash", map[string]any{"command": "git status --short"})
	if result.IsError || !strings.Contains(result.Content, "?? untracked.txt") || !strings.Contains(result.Content, "AM tracked.txt") {
		t.Fatalf("git status through Bash failed: %s", result.Content)
	}
	indexAfter, err := os.ReadFile(filepath.Join(dir, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(indexBefore, indexAfter) {
		t.Fatal("git status changed repository index")
	}
	if _, err := os.Stat(filepath.Join(dir, "monitor-ran")); !os.IsNotExist(err) {
		t.Fatal("repository fsmonitor hook executed")
	}
	if result := invoke(e, "Bash", map[string]any{"command": "xcrun --find git"}); result.IsError || !strings.Contains(result.Content, "/git") {
		t.Fatal("xcrun discovery failed", result.Content)
	}
	if result := invoke(e, "Bash", map[string]any{"command": "git diff --stat"}); result.IsError || !strings.Contains(result.Content, "tracked.txt") {
		t.Fatal("git diff failed", result.Content)
	}
	for _, command := range []string{"git add untracked.txt", "git config --local julia.test denied", "printf denied > .git/HEAD", "ln .git/config config-alias && printf denied > config-alias"} {
		if result := invoke(e, "Bash", map[string]any{"command": command}); !result.IsError {
			t.Fatalf("git metadata mutation escaped sandbox: %s", command)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, ".git", "credentials"), []byte("fixture-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if result := invoke(e, "Bash", map[string]any{"command": "cat .git/credentials"}); !result.IsError {
		t.Fatal("git credential data exposed", result.Content)
	}
	outside := filepath.Join(t.TempDir(), "included.conf")
	if err := os.WriteFile(outside, []byte("[julia]\nsecret = outside-secret-fixture\n"), 0600); err != nil {
		t.Fatal(err)
	}
	include := exec.Command("/usr/bin/git", "-C", dir, "config", "include.path", outside)
	if out, err := include.CombinedOutput(); err != nil {
		t.Fatalf("fixture include: %v: %s", err, out)
	}
	if result := invoke(e, "Bash", map[string]any{"command": "git config --get julia.secret"}); !result.IsError || strings.Contains(result.Content, "outside-secret-fixture") {
		t.Fatal("outside git-config include was readable", result.Content)
	}
}
func TestScratchDirPersistsOutsideWorkspace(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandboxed Bash requires macOS")
	}
	e, dir := fixture(t, "auto", nil)
	e.config.ScratchDir = filepath.Join(t.TempDir(), "session")
	if r := invoke(e, "Bash", map[string]any{"command": `printf kept > "$JULIA_SCRATCH/main.tf"`}); r.IsError {
		t.Fatal(r.Content)
	}
	r := invoke(e, "Bash", map[string]any{"command": `cat "$JULIA_SCRATCH/main.tf"`})
	if r.IsError || !strings.Contains(r.Content, "kept") {
		t.Fatal(r.Content)
	}
	entries, _ := os.ReadDir(dir)
	for _, entry := range entries {
		if entry.Name() == "main.tf" {
			t.Fatal("scratch file landed in the workspace")
		}
	}
}
func TestCloseRemovesOnlyEmptyScratchDir(t *testing.T) {
	for _, keep := range []bool{false, true} {
		e, _ := fixture(t, "auto", nil)
		e.config.ScratchDir = filepath.Join(t.TempDir(), "session")
		scratch, err := e.scratchDir()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(scratch, "downloads"), 0o700); err != nil {
			t.Fatal(err)
		}
		if keep {
			if err := os.WriteFile(filepath.Join(scratch, "main.tf"), []byte("kept"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		e.Close()
		if _, err := os.Stat(scratch); keep != (err == nil) {
			t.Fatalf("keep=%v: scratch directory after Close: %v", keep, err)
		}
		if _, err := os.Stat(filepath.Join(scratch, "main.tf")); keep && err != nil {
			t.Fatal("Close removed a kept scratch file")
		}
	}
}
func TestScratchDirRejectsWorkspace(t *testing.T) {
	e, dir := fixture(t, "auto", nil)
	e.config.ScratchDir = filepath.Join(dir, ".scratch")
	if _, err := e.scratchDir(); err == nil {
		t.Fatal("accepted a scratch directory inside the workspace")
	}
}

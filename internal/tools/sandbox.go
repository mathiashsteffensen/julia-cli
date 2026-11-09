package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"julia/internal/core"
)

// boundedBuffer consumes all writes, retaining only its limit. A mutex makes
// stdout/stderr writes safe without letting a verbose child exhaust memory.
type boundedBuffer struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	limit     int
	truncated bool
	onWrite   func([]byte)
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	remaining := b.limit - b.buf.Len()
	if remaining < n {
		b.truncated = true
		p = p[:max(remaining, 0)]
	}
	b.buf.Write(p)
	if len(p) > 0 && b.onWrite != nil {
		b.onWrite(p)
	}
	return n, nil
}
func (b *boundedBuffer) String() string { b.mu.Lock(); defer b.mu.Unlock(); return b.buf.String() }
func SandboxInfo() string {
	switch runtime.GOOS {
	case "darwin":
		if info, err := os.Stat("/usr/bin/sandbox-exec"); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0 {
			return "macOS Seatbelt (sandbox-exec), deny by default"
		}
	}
	return "unavailable: sandboxed Bash is disabled (no automatic fallback)"
}
func (e *Executor) SandboxInfo() string {
	if e.config.Mode == "yolo" {
		return "none: yolo mode runs Bash directly on the host, outside any sandbox, with the process environment"
	}
	return SandboxInfo()
}
func (e *Executor) CheckSandbox(ctx context.Context) error {
	_, err := e.runSandbox(ctx, "printf julia-sandbox-ok", false, 5*time.Second)
	return err
}

// netAccess is how far a sandboxed command's sockets reach.
type netAccess int

const (
	offline netAccess = iota
	// loopbackOnly lets a command listen and connect on localhost and
	// nowhere else: a dev server and the browser or tests that use it.
	loopbackOnly
	online
)

// access is the reach a call asked for with its network and loopback flags.
func access(network, loopback bool) (netAccess, error) {
	switch {
	case network && loopback:
		return offline, errors.New("network already includes loopback; set only one of them")
	case network:
		return online, nil
	case loopback:
		return loopbackOnly, nil
	}
	return offline, nil
}
func (e *Executor) bash(ctx context.Context, call core.ToolCall) (string, error) {
	if e.config.Mode == "read-only" {
		return "", errors.New("read-only mode prohibits Bash")
	}
	var a struct {
		Command       string `json:"command"`
		Timeout       int    `json:"timeout"`
		Network       bool   `json:"network"`
		Loopback      bool   `json:"loopback"`
		Permissions   string `json:"sandbox_permissions"`
		Justification string `json:"justification"`
	}
	a.Permissions = "use_default"
	if err := decode(call.Arguments, &a); err != nil {
		return "", err
	}
	if strings.TrimSpace(a.Command) == "" || len(a.Command) > 128*1024 {
		return "", errors.New("command is required and must be at most 128 KiB")
	}
	if a.Timeout == 0 {
		a.Timeout = 120
	}
	if a.Timeout < 1 || a.Timeout > 600 {
		return "", errors.New("timeout must be between 1 and 600 seconds")
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(call.Arguments, &fields)
	for _, key := range []string{"sandbox_permissions", "justification"} {
		if value, ok := fields[key]; ok && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return "", fmt.Errorf("%s must not be null", key)
		}
	}
	if a.Permissions != "use_default" && a.Permissions != "require_escalated" {
		return "", errors.New("sandbox_permissions must be use_default or require_escalated")
	}
	if len(a.Justification) > 8192 {
		return "", errors.New("justification must be at most 8 KiB")
	}
	escalated := a.Permissions == "require_escalated"
	if escalated && strings.TrimSpace(a.Justification) == "" {
		return "", errors.New("require_escalated needs a nonempty justification explaining why this exact command needs host access")
	}
	reach, err := access(a.Network, a.Loopback)
	if err != nil {
		return "", err
	}
	approval := core.Approval{Call: call, Task: e.config.Task, Directory: e.directory, Justification: a.Justification, Network: a.Network, ExecutionMode: "sandboxed", Reason: "Run this command in the workspace OS sandbox?"}
	switch reach {
	case online:
		approval.Reason = "Run this sandboxed command with network access for this invocation?"
	case loopbackOnly:
		approval.Reason = "Run this command in the workspace OS sandbox, able to listen on localhost and connect to the servers it or its terminals start there?"
	}
	if escalated {
		approval.ExecutionMode = "unsandboxed"
		approval.Network = true
		approval.Reason = "Run this exact command OUTSIDE the OS sandbox? It will have normal host-user filesystem access and unrestricted network access. No administrator privileges are added. Approval applies to this invocation only."
	}
	// Offline sandboxed Bash reaches no further than the workspace file tools,
	// which auto mode already permits: writes stay in the workspace (never .git),
	// credentials are unreadable and there is no network. Loopback adds only
	// the servers the agent starts itself, in the command or its terminals:
	// the machine's own services are closed to it. Review only what leaves
	// that boundary.
	review := true
	if escalated && namesKeyMaterial(a.Command) {
		// A private key leaving the machine must not rest on a model's score.
		review = false
		approval.Reason += " It names a private key or login file, so automatic review cannot approve it."
	}
	if e.config.Mode != "auto" || escalated || a.Network {
		if err := e.authorizeRequest(ctx, approval, review); err != nil {
			return "", err
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if escalated {
		e.emit("warning", "Executing the approved command outside the OS sandbox with host-user filesystem and network access.")
	}
	return e.runBashStreaming(ctx, a.Command, reach, time.Duration(a.Timeout)*time.Second, escalated, func(output []byte) {
		if e.config.Emit != nil {
			e.config.Emit(core.Event{Kind: "tool_output", Call: &call, Text: string(output)})
		}
	})
}

// keyMaterial matches private keys and long-lived login files by name. It is
// narrower than the sandbox's credential denials on purpose: `credentials` and
// `.config` also name ordinary packages and settings, and each match here
// interrupts the user.
var keyMaterial = regexp.MustCompile(`(?i)(^|[^a-z0-9._-])(\.ssh|\.gnupg|\.netrc|\.aws/credentials)($|[^a-z0-9._-])|(^|[^a-z0-9])id_(rsa|dsa|ecdsa|ed25519)`)

// namesKeyMaterial reports whether a command spells out key material. This is a
// tripwire, not a boundary: a glob or a variable gets past it, so automatic
// review still judges every other escalated command.
func namesKeyMaterial(command string) bool {
	return keyMaterial.MatchString(command)
}

// scratchDir creates the session scratch directory on first use. Without a
// configured one, a fresh directory serves this executor and is removed on Close.
// Execute holds e.mu for the whole call, so this must not lock it again.
func (e *Executor) scratchDir() (string, error) {
	if e.scratch != "" {
		return e.scratch, nil
	}
	dir := e.config.ScratchDir
	if dir == "" {
		temp, err := os.MkdirTemp("", "julia-scratch-")
		if err != nil {
			return "", err
		}
		e.ownScratch = true
		dir = temp
	} else if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", err
	}
	if inside, _ := filepath.Rel(e.directory, dir); inside == "." || (!filepath.IsAbs(inside) && !strings.HasPrefix(inside, "..")) {
		return "", errors.New("scratch directory must be outside the workspace")
	}
	e.scratch = dir
	return dir, nil
}
func (e *Executor) runSandbox(ctx context.Context, command string, network bool, timeout time.Duration) (string, error) {
	reach, _ := access(network, false)
	return e.runBash(ctx, command, reach, timeout, false)
}
func (e *Executor) runBash(ctx context.Context, command string, reach netAccess, timeout time.Duration, escalated bool) (string, error) {
	return e.runBashStreaming(ctx, command, reach, timeout, escalated, nil)
}

func (e *Executor) runBashStreaming(ctx context.Context, command string, reach netAccess, timeout time.Duration, escalated bool, onOutput func([]byte)) (string, error) {
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	sh, err := e.shell(runCtx, command, reach, escalated, "")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(sh.tmp)
	mode := "approved host command"
	if e.config.Mode == "yolo" {
		mode = "host command"
	} else if !escalated {
		mode = "sandboxed command"
	}
	cmd := exec.CommandContext(runCtx, sh.path, sh.args...)
	cmd.Dir, cmd.Env = e.directory, sh.env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = time.Second
	output := &boundedBuffer{limit: maxOutput, onWrite: onOutput}
	cmd.Stdout = output
	cmd.Stderr = output
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("start %s (no fallback): %w", mode, err)
	}
	err = cmd.Wait() // Also reap grandchildren that closed their output and outlived the shell.
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	content := output.String()
	if output.truncated {
		content = bounded(content + "\n[output truncated]")
	}
	if runCtx.Err() != nil {
		return "", fmt.Errorf("command stopped: %w\n%s", runCtx.Err(), content)
	}
	if err != nil {
		return "", fmt.Errorf("%s failed: %w\n%s", mode, err, content)
	}
	if content == "" {
		content = "[command completed with no output]"
	}
	return content, nil
}

// shellCommand is a prepared `bash -c` invocation. Bash and Terminal both
// build theirs with shell, so they share one sandbox profile and one curated
// environment.
type shellCommand struct {
	path string // sandbox-exec, or bash itself for an approved host command
	args []string
	env  []string
	// tmp is the invocation's private HOME, TMPDIR and caches. The caller
	// removes it once the command has finished.
	tmp string
}

// shell prepares command to run in the workspace. tty is the terminal device
// a Terminal program runs on, or empty for Bash; the profile then grants that
// one device besides everything Bash may use.
func (e *Executor) shell(ctx context.Context, command string, reach netAccess, escalated bool, tty string) (shellCommand, error) {
	if e.config.Mode == "yolo" {
		return e.hostShell(command, tty)
	}
	if !escalated && runtime.GOOS != "darwin" {
		return shellCommand{}, errors.New("Bash requires macOS Seatbelt in this version; other platforms fail closed")
	}
	if escalated && runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		return shellCommand{}, errors.New("explicit host Bash execution requires macOS or Linux")
	}
	tmp, err := os.MkdirTemp("", "julia-sandbox-")
	if err != nil {
		return shellCommand{}, fmt.Errorf("sandbox temporary directory: %w", err)
	}
	prepared := false
	defer func() {
		if !prepared {
			os.RemoveAll(tmp)
		}
	}()
	tmp, err = filepath.EvalSymlinks(tmp)
	if err != nil {
		return shellCommand{}, err
	}
	scratch, err := e.scratchDir()
	if err != nil {
		return shellCommand{}, fmt.Errorf("session scratch directory: %w", err)
	}
	home := filepath.Join(tmp, "home")
	if escalated {
		home, err = os.UserHomeDir()
		if err != nil {
			return shellCommand{}, fmt.Errorf("resolve host home for escalated command: %w", err)
		}
	} else {
		if err := os.Mkdir(home, 0700); err != nil {
			return shellCommand{}, err
		}
	}
	var goInfo EnvironmentInfo
	if escalated {
		// Host caches are used, so only the toolchain location matters here.
		if development, err := e.discoverGo(ctx); err != nil {
			e.emit("warning", "Go toolchain discovery unavailable; escalated command will use host Go defaults: "+err.Error())
		} else {
			goInfo.GoRoot = development.Info.GoRoot
		}
	} else if development, prepareErr := e.prepareGoEnvironment(ctx); prepareErr != nil {
		e.emit("warning", "Go cache preparation unavailable; using temporary caches: "+prepareErr.Error())
		if e.goEnv != nil {
			goInfo.GoRoot = e.goEnv.Info.GoRoot
		}
	} else {
		goInfo = development.Info
	}
	sh := shellCommand{path: "/bin/bash", args: []string{"--noprofile", "--norc", "-c", command}, tmp: tmp}
	if !escalated {
		sh.path = "/usr/bin/sandbox-exec"
		if info, statErr := os.Stat(sh.path); statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
			return shellCommand{}, errors.New("Bash disabled: macOS sandbox-exec is unavailable")
		}
		var closed []int
		if reach == loopbackOnly {
			if closed, err = e.hostPorts(ctx); err != nil {
				return shellCommand{}, fmt.Errorf("loopback needs to know which local ports belong to services already running here, and could not (%v); use network=true instead", err)
			}
		}
		profile := seatbeltProfile(e.directory, tmp, scratch, reach, closed, goInfo)
		if tty != "" {
			rules, err := terminalProfile(tty)
			if err != nil {
				return shellCommand{}, err
			}
			profile += rules
		}
		sh.args = append([]string{"-p", profile, "/bin/bash"}, sh.args...)
	}
	sh.env = []string{"PATH=/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin", "HOME=" + home, "TMPDIR=" + tmp, "TMP=" + tmp, "TEMP=" + tmp, "JULIA_SCRATCH=" + scratch, "LANG=en_US.UTF-8", "LC_ALL=en_US.UTF-8", "TERM=dumb", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=core.fsmonitor", "GIT_CONFIG_VALUE_0=false"}
	if escalated {
		// Keep the environment curated, but let host tools use their normal
		// persistent configuration and cache locations when explicitly approved.
		for _, key := range []string{"XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "GOPATH", "GOBIN", "GOCACHE", "GOMODCACHE", "GOENV"} {
			if value := os.Getenv(key); value != "" {
				sh.env = append(sh.env, key+"="+value)
			}
		}
	} else {
		sh.env = append(sh.env,
			"XDG_CONFIG_HOME="+filepath.Join(tmp, "config"),
			"XDG_CACHE_HOME="+filepath.Join(tmp, "cache"),
			"XDG_DATA_HOME="+filepath.Join(tmp, "data"),
			"GOCACHE="+filepath.Join(tmp, "go-cache"),
			"GOMODCACHE="+filepath.Join(tmp, "go-mod"),
			"GOPATH="+filepath.Join(tmp, "go"),
		)
	}
	if goInfo.GoRoot != "" {
		sh.env = append(sh.env, "GOROOT="+goInfo.GoRoot)
		if !escalated {
			sh.env = append(sh.env, "GOENV=off")
		}
		sh.env[0] = "PATH=" + filepath.Join(goInfo.GoRoot, "bin") + ":/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
	}
	if !escalated {
		for i, value := range sh.env {
			if strings.HasPrefix(value, "GOCACHE=") && goInfo.GoBuildCache != "" {
				sh.env[i] = "GOCACHE=" + goInfo.GoBuildCache
			}
			if strings.HasPrefix(value, "GOMODCACHE=") && goInfo.GoModuleCache != "" {
				sh.env[i] = "GOMODCACHE=" + goInfo.GoModuleCache
			}
		}
	}
	prepared = true
	return sh, nil
}

// hostShell prepares command for yolo mode: bash on the host as it is, with
// the process environment, no sandbox and no private HOME or caches. Whoever
// chose yolo mode runs JULIA where the host itself is the boundary, such as
// a container made for one task, so the command sees what the process sees:
// its tokens, its configured tools and its network. Only the scratch
// directory is added. tty is as for shell.
func (e *Executor) hostShell(command, tty string) (shellCommand, error) {
	scratch, err := e.scratchDir()
	if err != nil {
		return shellCommand{}, fmt.Errorf("session scratch directory: %w", err)
	}
	env := append(os.Environ(), "JULIA_SCRATCH="+scratch, "GIT_TERMINAL_PROMPT=0")
	if tty == "" {
		env = append(env, "TERM=dumb")
	}
	return shellCommand{path: "/bin/bash", args: []string{"--noprofile", "--norc", "-c", command}, env: env}, nil
}

// seatbeltProfile is the sandbox for a command. closed are the local ports a
// loopback command may not connect to: the machine's own services.
func seatbeltProfile(directory, tmp, scratch string, reach netAccess, closed []int, goEnvironments ...EnvironmentInfo) string {
	var p strings.Builder
	p.WriteString("(version 1)\n(deny default)\n(allow file-read* file-test-existence (literal \"/\"))\n(allow system-mac-syscall (mac-policy-name \"vnguard\"))\n(allow system-mac-syscall (require-all (mac-policy-name \"Sandbox\") (mac-syscall-number 67)))\n")
	// Restrict all process operations to the sandbox. Descendants inherit Seatbelt.
	p.WriteString("(allow process-exec)\n(allow process-fork)\n(allow signal (target same-sandbox))\n(allow sysctl-read)\n")
	p.WriteString("(allow mach-lookup (global-name \"com.apple.system.logger\") (global-name \"com.apple.system.opendirectoryd.libinfo\") (global-name \"com.apple.cfprefsd.daemon\"))\n")
	for _, path := range []string{"/bin", "/sbin", "/usr", "/System", "/Library/Apple", "/Library/Developer/CommandLineTools", "/opt/homebrew", "/usr/local", directory, tmp, scratch} {
		fmt.Fprintf(&p, "(allow file-read* file-map-executable (subpath %s))\n", strconv.Quote(path))
	}
	for _, path := range []string{"/", "/private", "/private/tmp", "/private/var", "/private/var/folders", "/Users", "/Volumes", "/dev", "/etc", "/var", "/tmp"} {
		fmt.Fprintf(&p, "(allow file-read-metadata (literal %s))\n", strconv.Quote(path))
	}
	fmt.Fprintf(&p, "(allow file-read-metadata (path-ancestors %s))\n", strconv.Quote(directory))
	for _, path := range []string{"/dev/null", "/dev/zero", "/dev/random", "/dev/urandom", "/private/etc/hosts", "/private/etc/resolv.conf", "/private/etc/services", "/private/etc/protocols", "/private/etc/localtime"} {
		fmt.Fprintf(&p, "(allow file-read* (literal %s))\n", strconv.Quote(path))
	}
	// Apple's /usr/bin tool shims consult this symlink before launching the
	// selected developer tools. Permit discovery without exposing /private/var.
	p.WriteString("(allow file-read-metadata (literal \"/var/select\") (literal \"/private/var/select\"))\n")
	p.WriteString("(allow file-read* (literal \"/var/select/developer_dir\") (literal \"/private/var/select/developer_dir\"))\n")
	p.WriteString("(allow file-read* (subpath \"/private/etc/ssl\") (subpath \"/dev/fd\"))\n(allow file-write* (literal \"/dev/null\"))\n")
	fmt.Fprintf(&p, "(allow file-write* (subpath %s) (subpath %s) (subpath %s))\n", strconv.Quote(directory), strconv.Quote(tmp), strconv.Quote(scratch))
	fmt.Fprintf(&p, "(allow file-read-metadata (path-ancestors %s))\n", strconv.Quote(scratch))
	for _, goInfo := range goEnvironments {
		if goInfo.GoRoot != "" {
			fmt.Fprintf(&p, "(allow file-read* file-map-executable (subpath %s))\n(allow file-read-metadata (path-ancestors %s))\n", strconv.Quote(goInfo.GoRoot), strconv.Quote(goInfo.GoRoot))
		}
		for _, path := range []string{goInfo.GoBuildCache, goInfo.GoModuleCache} {
			if path != "" {
				fmt.Fprintf(&p, "(allow file-read* file-write* file-map-executable (subpath %s))\n(allow file-read-metadata (path-ancestors %s))\n", strconv.Quote(path), strconv.Quote(path))
			}
		}
	}
	// Explicit denials override workspace grants, including symlink resolution.
	pattern := `^.*/([.]ssh|[.]aws|[.]azure|[.]config|[.]codex|[.]julia|[.]gnupg|[.]kube|[.]netrc|[.]npmrc|[.]pypirc|[.]env[^/]*|credentials|credentials[.]json|auth[.]json|bugsnag[.]json|id_rsa[^/]*|id_ed25519[^/]*|[^/]+[.]pem|[^/]+[.]key)(/.*)?$`
	// Seatbelt regex literals do not use Go's backslash escaping. Character
	// classes keep punctuation literal and explicit letter classes also protect
	// aliases on case-insensitive APFS volumes.
	var insensitive strings.Builder
	for _, c := range pattern {
		if c >= 'a' && c <= 'z' {
			fmt.Fprintf(&insensitive, "[%c%c]", c, c-'a'+'A')
		} else {
			insensitive.WriteRune(c)
		}
	}
	fmt.Fprintf(&p, "(deny file-read* file-write* (require-all (subpath %s) (regex #%s)))\n", strconv.Quote(directory), strconv.Quote(insensitive.String()))
	// Git needs repository metadata for status/diff, but no shell command may
	// mutate it. Credential patterns above still apply within .git directories.
	fmt.Fprintf(&p, "(deny file-write* (require-all (subpath %s) (regex #\"^.*/[.][gG][iI][tT](/.*)?$\")))\n", strconv.Quote(directory))
	// The task cannot signal, inspect, or attach to processes outside its sandbox;
	// network grants apply to this process tree only and do not add filesystem access.
	switch reach {
	case online:
		p.WriteString("(allow network*)\n")
	case loopbackOnly:
		// Seatbelt's "localhost" is the loopback addresses, 127.0.0.1 and ::1.
		// Listening is network-inbound, checked against the socket's own
		// address, which cannot tell a wildcard from loopback: a server bound
		// to every interface still accepts the local network, as it would if
		// the user ran it. Connections out reach loopback only, so nothing
		// can be sent beyond the machine, and not the ports of services that
		// were listening before: the command's own servers, and its
		// terminals', are what it may use. Unix sockets stay denied. The
		// grant names tcp and udp rather than ip: after an ip grant, Seatbelt
		// ignores a later denial of one port.
		p.WriteString("(allow network-bind network-inbound (local ip \"localhost:*\"))\n(allow network-outbound (remote tcp \"localhost:*\") (remote udp \"localhost:*\"))\n")
		for _, port := range closed {
			fmt.Fprintf(&p, "(deny network-outbound (remote tcp \"localhost:%d\") (remote udp \"localhost:%d\"))\n", port, port)
		}
	}
	return p.String()
}

// terminalProfile lets a Terminal program use its own terminal through the
// descriptors it inherits and through /dev/tty, which always means the
// caller's controlling terminal: the ioctls that switch to raw mode and read
// the window size, and the metadata and /dev listing ttyname() needs. The
// device's own path is never opened: macOS reuses ttys numbers, and a process
// that escaped the terminal's end could otherwise read whatever is typed into
// an unrelated terminal that later gets the same name.
func terminalProfile(tty string) (string, error) {
	if filepath.Dir(tty) != "/dev" || !strings.HasPrefix(filepath.Base(tty), "tty") {
		return "", fmt.Errorf("unexpected terminal device %q", tty)
	}
	return fmt.Sprintf("(allow file-ioctl file-read-metadata (literal %s))\n(allow file-read* file-write* file-ioctl (literal \"/dev/tty\"))\n(allow file-read-data (literal \"/dev\"))\n", strconv.Quote(tty)), nil
}

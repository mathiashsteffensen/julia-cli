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
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"jarvis/internal/core"
)

// boundedBuffer consumes all writes, retaining only its limit. A mutex makes
// stdout/stderr writes safe without letting a verbose child exhaust memory.
type boundedBuffer struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	limit     int
	truncated bool
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
func (e *Executor) SandboxInfo() string { return SandboxInfo() }
func (e *Executor) CheckSandbox(ctx context.Context) error {
	_, err := e.runSandbox(ctx, "printf jarvis-sandbox-ok", false, 5*time.Second)
	return err
}
func (e *Executor) bash(ctx context.Context, call core.ToolCall) (string, error) {
	if e.config.Mode == "read-only" {
		return "", errors.New("read-only mode prohibits Bash")
	}
	var a struct {
		Command       string `json:"command"`
		Timeout       int    `json:"timeout"`
		Network       bool   `json:"network"`
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
	approval := core.Approval{Call: call, Task: e.config.Task, Directory: e.directory, Justification: a.Justification, Network: a.Network, ExecutionMode: "sandboxed", Reason: "Run this command in the workspace OS sandbox?"}
	if a.Network {
		approval.Reason = "Run this sandboxed command with network access for this invocation?"
	}
	if escalated {
		approval.ExecutionMode = "unsandboxed"
		approval.Network = true
		approval.Reason = "Run this exact command OUTSIDE the OS sandbox? It will have normal host-user filesystem access and unrestricted network access. No administrator privileges are added. Approval applies to this invocation only."
	}
	if err := e.authorizeRequest(ctx, approval, true); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if escalated {
		e.emit("warning", "Executing the approved command outside the OS sandbox with host-user filesystem and network access.")
	}
	return e.runBash(ctx, a.Command, a.Network, time.Duration(a.Timeout)*time.Second, escalated)
}
func (e *Executor) runSandbox(ctx context.Context, command string, network bool, timeout time.Duration) (string, error) {
	return e.runBash(ctx, command, network, timeout, false)
}
func (e *Executor) runBash(ctx context.Context, command string, network bool, timeout time.Duration, escalated bool) (string, error) {
	if !escalated && runtime.GOOS != "darwin" {
		return "", errors.New("Bash requires macOS Seatbelt in this version; other platforms fail closed")
	}
	if escalated && runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		return "", errors.New("explicit host Bash execution requires macOS or Linux")
	}
	tmp, err := os.MkdirTemp("", "jarvis-sandbox-")
	if err != nil {
		return "", fmt.Errorf("sandbox temporary directory: %w", err)
	}
	defer os.RemoveAll(tmp)
	tmp, err = filepath.EvalSymlinks(tmp)
	if err != nil {
		return "", err
	}
	home := filepath.Join(tmp, "home")
	if escalated {
		home, err = os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve host home for escalated command: %w", err)
		}
	} else {
		if err := os.Mkdir(home, 0700); err != nil {
			return "", err
		}
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var goInfo EnvironmentInfo
	if escalated {
		// Host caches are used, so only the toolchain location matters here.
		if development, err := e.discoverGo(runCtx); err != nil {
			e.emit("warning", "Go toolchain discovery unavailable; escalated command will use host Go defaults: "+err.Error())
		} else {
			goInfo.GoRoot = development.Info.GoRoot
		}
	} else if development, prepareErr := e.prepareGoEnvironment(runCtx); prepareErr != nil {
		e.emit("warning", "Go cache preparation unavailable; using temporary caches: "+prepareErr.Error())
		if e.goEnv != nil {
			goInfo.GoRoot = e.goEnv.Info.GoRoot
		}
	} else {
		goInfo = development.Info
	}
	executable := "/bin/bash"
	args := []string{"--noprofile", "--norc", "-c", command}
	mode := "approved host command"
	if !escalated {
		mode = "sandboxed command"
		executable = "/usr/bin/sandbox-exec"
		if info, statErr := os.Stat(executable); statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
			return "", errors.New("Bash disabled: macOS sandbox-exec is unavailable")
		}
		args = append([]string{"-p", seatbeltProfile(e.directory, tmp, network, goInfo), "/bin/bash"}, args...)
	}
	cmd := exec.CommandContext(runCtx, executable, args...)
	cmd.Dir = e.directory
	cmd.Env = []string{"PATH=/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin", "HOME=" + home, "TMPDIR=" + tmp, "TMP=" + tmp, "TEMP=" + tmp, "LANG=en_US.UTF-8", "LC_ALL=en_US.UTF-8", "TERM=dumb", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=core.fsmonitor", "GIT_CONFIG_VALUE_0=false"}
	if escalated {
		// Keep the environment curated, but let host tools use their normal
		// persistent configuration and cache locations when explicitly approved.
		for _, key := range []string{"XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "GOPATH", "GOBIN", "GOCACHE", "GOMODCACHE", "GOENV"} {
			if value := os.Getenv(key); value != "" {
				cmd.Env = append(cmd.Env, key+"="+value)
			}
		}
	} else {
		cmd.Env = append(cmd.Env,
			"XDG_CONFIG_HOME="+filepath.Join(tmp, "config"),
			"XDG_CACHE_HOME="+filepath.Join(tmp, "cache"),
			"XDG_DATA_HOME="+filepath.Join(tmp, "data"),
			"GOCACHE="+filepath.Join(tmp, "go-cache"),
			"GOMODCACHE="+filepath.Join(tmp, "go-mod"),
			"GOPATH="+filepath.Join(tmp, "go"),
		)
	}
	if goInfo.GoRoot != "" {
		cmd.Env = append(cmd.Env, "GOROOT="+goInfo.GoRoot)
		if !escalated {
			cmd.Env = append(cmd.Env, "GOENV=off")
		}
		cmd.Env[0] = "PATH=" + filepath.Join(goInfo.GoRoot, "bin") + ":/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
	}
	if !escalated {
		for i, value := range cmd.Env {
			if strings.HasPrefix(value, "GOCACHE=") && goInfo.GoBuildCache != "" {
				cmd.Env[i] = "GOCACHE=" + goInfo.GoBuildCache
			}
			if strings.HasPrefix(value, "GOMODCACHE=") && goInfo.GoModuleCache != "" {
				cmd.Env[i] = "GOMODCACHE=" + goInfo.GoModuleCache
			}
		}
	}
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
	output := &boundedBuffer{limit: maxOutput}
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
func seatbeltProfile(directory, tmp string, network bool, goEnvironments ...EnvironmentInfo) string {
	var p strings.Builder
	p.WriteString("(version 1)\n(deny default)\n(allow file-read* file-test-existence (literal \"/\"))\n(allow system-mac-syscall (mac-policy-name \"vnguard\"))\n(allow system-mac-syscall (require-all (mac-policy-name \"Sandbox\") (mac-syscall-number 67)))\n")
	// Restrict all process operations to the sandbox. Descendants inherit Seatbelt.
	p.WriteString("(allow process-exec)\n(allow process-fork)\n(allow signal (target same-sandbox))\n(allow sysctl-read)\n")
	p.WriteString("(allow mach-lookup (global-name \"com.apple.system.logger\") (global-name \"com.apple.system.opendirectoryd.libinfo\") (global-name \"com.apple.cfprefsd.daemon\"))\n")
	for _, path := range []string{"/bin", "/sbin", "/usr", "/System", "/Library/Apple", "/Library/Developer/CommandLineTools", "/opt/homebrew", "/usr/local", directory, tmp} {
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
	fmt.Fprintf(&p, "(allow file-write* (subpath %s) (subpath %s))\n", strconv.Quote(directory), strconv.Quote(tmp))
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
	pattern := `^.*/([.]ssh|[.]aws|[.]azure|[.]config|[.]codex|[.]jarvis|[.]gnupg|[.]kube|[.]netrc|[.]npmrc|[.]pypirc|[.]env[^/]*|credentials|credentials[.]json|auth[.]json|id_rsa[^/]*|id_ed25519[^/]*|[^/]+[.]pem|[^/]+[.]key)(/.*)?$`
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
	if network {
		p.WriteString("(allow network*)\n")
	}
	return p.String()
}

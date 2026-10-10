// Package tools provides provider-independent JARVIS tools. All file
// access is rooted in the workspace. Shell execution is sandboxed by default;
// leaving the sandbox requires an explicitly requested and approved command.
package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"jarvis/internal/core"
)

const (
	maxOutput = 32 * 1024
	maxFile   = 4 * 1024 * 1024
	maxSearch = 16 * 1024 * 1024
)

type Config struct {
	Directory, Mode string
	Approve, Review core.Approver
	Task            string
	Emit            core.Emit
}
type Executor struct {
	config    Config
	root      *os.Root
	directory string
	mu        sync.Mutex
	goEnv     *goEnvironment
	webClient *http.Client
}

func New(c Config) (*Executor, error) {
	if c.Mode == "" {
		c.Mode = "auto"
	}
	if c.Mode != "auto" && c.Mode != "ask" && c.Mode != "read-only" {
		return nil, fmt.Errorf("invalid permission mode %q", c.Mode)
	}
	dir, err := filepath.Abs(c.Directory)
	if err != nil {
		return nil, fmt.Errorf("workspace: %w", err)
	}
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace: %w", err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("open workspace: %w", err)
	}
	return &Executor{config: c, root: root, directory: dir}, nil
}
func (e *Executor) Close() error {
	if e.webClient != nil {
		e.webClient.CloseIdleConnections()
	}
	return e.root.Close()
}
func (e *Executor) SetTask(task string) { e.mu.Lock(); defer e.mu.Unlock(); e.config.Task = task }
func (e *Executor) emit(kind, text string) {
	if e.config.Emit != nil {
		e.config.Emit(core.Event{Kind: kind, Text: text})
	}
}
func schema(properties map[string]any, required ...string) map[string]any {
	return map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
}
func str(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}
func integer(description string) map[string]any {
	return map[string]any{"type": "integer", "description": description}
}
func (e *Executor) Definitions() []core.ToolDefinition { return Definitions(false) }

// Definitions returns the same registry used by execution, without opening a workspace.
func Definitions(subagents bool) []core.ToolDefinition {
	path := str("Workspace-relative path. Absolute paths, symlinks and protected credential/git paths are forbidden.")
	edits := map[string]any{"type": "array", "minItems": 1, "maxItems": 100, "items": schema(map[string]any{"old_string": str("Nonempty exact text occurring once"), "new_string": str("Replacement text")}, "old_string", "new_string")}
	definitions := []core.ToolDefinition{
		{Name: "Read", Description: "Read a UTF-8 text file with numbered lines. Defaults to 200 lines; at most 1000 lines and 32 KiB output.", Parameters: schema(map[string]any{"path": path, "offset": integer("First line, 1-based; default 1"), "limit": integer("Line count; default 200, max 1000")}, "path")},
		{Name: "Edit", Description: "Atomically replace one unique exact string in one file. Fails if old_string is absent or ambiguous. Maximum file size 4 MiB.", Parameters: schema(map[string]any{"path": path, "old_string": str("Nonempty exact text occurring once"), "new_string": str("Replacement text")}, "path", "old_string", "new_string")},
		{Name: "Write", Description: "Atomically create or replace a UTF-8 file up to 4 MiB; creates parent directories.", Parameters: schema(map[string]any{"path": path, "content": str("Complete file contents")}, "path", "content")},
		{Name: "MultiEdit", Description: "Apply ordered exact replacements to ONE file atomically. Every old_string must occur exactly once at its step; any failure leaves the file untouched.", Parameters: schema(map[string]any{"path": path, "edits": edits}, "path", "edits")},
		{Name: "Bash", Description: "Run Bash in the workspace. Every call is reviewed or approved. Default execution uses the OS sandbox; explicit require_escalated execution requires a justification and separate approval for that exact command, then runs outside the sandbox with host-user filesystem and unrestricted network access. No automatic retry or added sudo. Both modes strip inherited credentials, cap output at 32 KiB, and stop the ordinary process group on completion or timeout.", Parameters: schema(map[string]any{"command": str("Bash command"), "timeout": integer("Seconds, default 120, maximum 600"), "network": map[string]any{"type": "boolean", "description": "Enable network for this sandboxed invocation only; default false. Outside the sandbox network is always unrestricted regardless of this flag."}, "sandbox_permissions": map[string]any{"type": "string", "enum": []string{"use_default", "require_escalated"}, "description": "Default use_default keeps the OS sandbox. require_escalated requests approval to run this exact command outside the OS sandbox, with normal host-user privileges."}, "justification": str("Required for require_escalated: why this exact command needs host access and how it follows the user's task. This explanation is not itself authorization.")}, "command")},
		{Name: "Grep", Description: "Search text with ripgrep regex, without following symlinks or reading protected files. Scans up to 16 MiB of files, each at most 4 MiB; output capped at 32 KiB.", Parameters: schema(map[string]any{"pattern": str("Ripgrep regex"), "path": str("Workspace-relative file/directory, default ."), "glob": str("Optional filename glob such as *.go"), "ignore_case": map[string]any{"type": "boolean"}}, "pattern")},
		webSearchDefinition(),
		{Name: "List", Description: "List workspace entries, excluding symlinks and protected paths. Defaults to depth 1; maximum depth 5 and 2000 entries.", Parameters: schema(map[string]any{"path": str("Workspace-relative directory, default ."), "depth": integer("Depth 1–5, default 1")})},
	}
	if subagents {
		definitions = append(definitions, subagentDefinition())
	}
	return definitions
}
func decode(raw json.RawMessage, dst any) error {
	if len(raw) > maxFile*3 {
		return errors.New("tool arguments exceed size limit")
	}
	if trimmed := bytes.TrimSpace(raw); len(trimmed) == 0 || trimmed[0] != '{' {
		return errors.New("tool arguments must be a JSON object")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("arguments must be one JSON object")
	}
	return nil
}
func (e *Executor) Execute(ctx context.Context, call core.ToolCall) core.ToolResult {
	if err := ctx.Err(); err != nil {
		return failure(err)
	}
	// A single executor serializes tools so read/modify/write edits cannot lose one
	// another's updates. os.Root separately confines filesystem access.
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return failure(err)
	}
	for _, definition := range e.Definitions() {
		if definition.Name == call.Name {
			required, _ := definition.Parameters["required"].([]string)
			if err := requireFields(call.Arguments, required...); err != nil {
				return failure(err)
			}
			break
		}
	}
	var content string
	var err error
	switch call.Name {
	case "Read":
		content, err = e.read(call)
	case "List":
		content, err = e.list(call)
	case "Grep":
		content, err = e.grep(ctx, call)
	case "WebSearch":
		content, err = e.webSearch(ctx, call)
	case "Write", "Edit", "MultiEdit":
		if e.config.Mode == "read-only" {
			err = errors.New("read-only mode prohibits file changes")
			break
		}
		if e.config.Mode == "ask" {
			err = e.authorize(ctx, call, false, false)
			if err != nil {
				break
			}
		}
		content, err = e.modify(call)
	case "Bash":
		content, err = e.bash(ctx, call)
	default:
		err = fmt.Errorf("unknown tool %q", call.Name)
	}
	if err != nil {
		return failure(err)
	}
	return core.ToolResult{Content: bounded(content)}
}
func failure(err error) core.ToolResult {
	return core.ToolResult{Content: bounded(err.Error()), IsError: true}
}
func bounded(s string) string {
	if len(s) <= maxOutput {
		return s
	}
	n := maxOutput - len("\n[output truncated]")
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "\n[output truncated]"
}
func protected(name string) bool {
	for _, part := range strings.Split(filepath.ToSlash(name), "/") {
		p := strings.ToLower(part)
		if p == ".git" || p == ".ssh" || p == ".aws" || p == ".azure" || p == ".config" || p == ".codex" || p == ".jarvis" || p == ".gnupg" || p == ".netrc" || p == ".npmrc" || p == ".pypirc" || p == ".kube" || p == "credentials" || p == "credentials.json" || p == "auth.json" || strings.HasPrefix(p, ".env") || strings.HasPrefix(p, "id_rsa") || strings.HasPrefix(p, "id_ed25519") || strings.HasSuffix(p, ".pem") || strings.HasSuffix(p, ".key") {
			return true
		}
	}
	return false
}
func (e *Executor) path(name string, allowRoot bool) (string, error) {
	if name == "" {
		if allowRoot {
			name = "."
		} else {
			return "", errors.New("path is required")
		}
	}
	if filepath.IsAbs(name) || strings.ContainsRune(name, 0) {
		return "", errors.New("path must be workspace-relative")
	}
	for _, part := range strings.Split(filepath.ToSlash(name), "/") {
		if part == ".." {
			return "", errors.New("parent traversal is forbidden")
		}
	}
	name = filepath.Clean(name)
	if name == "." && !allowRoot {
		return "", errors.New("file path is required")
	}
	if protected(name) {
		return "", errors.New("protected credential or metadata path is forbidden")
	}
	current := ""
	for _, part := range strings.Split(name, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := e.root.Lstat(current)
		if os.IsNotExist(err) {
			break
		}
		if err != nil {
			return "", fmt.Errorf("inspect path: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("symlink paths are forbidden")
		}
	}
	return name, nil
}
func (e *Executor) readFile(name string) ([]byte, os.FileMode, error) {
	f, err := e.openNoFollow(name)
	if err != nil {
		return nil, 0, fmt.Errorf("open %s: %w", name, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	if !info.Mode().IsRegular() {
		return nil, 0, errors.New("only regular files are supported")
	}
	if info.Size() > maxFile {
		return nil, 0, errors.New("file exceeds 4 MiB limit")
	}
	b, err := io.ReadAll(io.LimitReader(f, maxFile+1))
	if err != nil {
		return nil, 0, err
	}
	if len(b) > maxFile {
		return nil, 0, errors.New("file exceeds 4 MiB limit")
	}
	if !utf8.Valid(b) || bytes.IndexByte(b, 0) >= 0 {
		return nil, 0, errors.New("file is not UTF-8 text")
	}
	return b, info.Mode().Perm(), nil
}
func (e *Executor) read(call core.ToolCall) (string, error) {
	var a struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if err := decode(call.Arguments, &a); err != nil {
		return "", err
	}
	name, err := e.path(a.Path, false)
	if err != nil {
		return "", err
	}
	if a.Offset == 0 {
		a.Offset = 1
	}
	if a.Limit == 0 {
		a.Limit = 200
	}
	if a.Offset < 1 || a.Limit < 1 || a.Limit > 1000 {
		return "", errors.New("offset must be positive; limit must be between 1 and 1000")
	}
	b, _, err := e.readFile(name)
	if err != nil {
		return "", err
	}
	lines := strings.Split(string(b), "\n")
	var out strings.Builder
	for i := a.Offset - 1; i < len(lines) && i-a.Offset+1 < a.Limit; i++ {
		fmt.Fprintf(&out, "%d\t%s\n", i+1, lines[i])
		if out.Len() > maxOutput {
			break
		}
	}
	if a.Offset-1 >= len(lines) {
		return "[offset beyond end of file]", nil
	}
	return bounded(out.String()), nil
}

type replacement struct {
	Old string `json:"old_string"`
	New string `json:"new_string"`
}

func requireFields(raw json.RawMessage, fields ...string) error {
	if len(raw) > maxFile*3 {
		return errors.New("tool arguments exceed size limit")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return errors.New("tool arguments must be a JSON object")
	}
	for _, field := range fields {
		value, ok := object[field]
		if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return fmt.Errorf("required argument %q is missing or null", field)
		}
	}
	return nil
}

func (e *Executor) modify(call core.ToolCall) (string, error) {
	var name, content string
	mode := os.FileMode(0644)
	if call.Name == "Write" {
		var a struct {
			Path    string `json:"path"`
			Content string `json:"content"`
		}
		if err := decode(call.Arguments, &a); err != nil {
			return "", err
		}
		var err error
		name, err = e.path(a.Path, false)
		if err != nil {
			return "", err
		}
		content = a.Content
		if info, err := e.root.Lstat(name); err == nil {
			if !info.Mode().IsRegular() {
				return "", errors.New("destination must be a regular file")
			}
			mode = info.Mode().Perm()
		} else if !os.IsNotExist(err) {
			return "", err
		}
	} else {
		var edits []replacement
		var rawPath string
		if call.Name == "Edit" {
			var a struct {
				Path string `json:"path"`
				Old  string `json:"old_string"`
				New  string `json:"new_string"`
			}
			if err := decode(call.Arguments, &a); err != nil {
				return "", err
			}
			rawPath = a.Path
			edits = []replacement{{a.Old, a.New}}
		} else {
			var a struct {
				Path  string        `json:"path"`
				Edits []replacement `json:"edits"`
			}
			if err := decode(call.Arguments, &a); err != nil {
				return "", err
			}
			var wire struct {
				Edits []json.RawMessage `json:"edits"`
			}
			_ = json.Unmarshal(call.Arguments, &wire)
			for i, raw := range wire.Edits {
				if err := requireFields(raw, "old_string", "new_string"); err != nil {
					return "", fmt.Errorf("edit %d: %w", i+1, err)
				}
			}
			rawPath = a.Path
			edits = a.Edits
		}
		if len(edits) == 0 || len(edits) > 100 {
			return "", errors.New("provide between 1 and 100 edits")
		}
		var err error
		name, err = e.path(rawPath, false)
		if err != nil {
			return "", err
		}
		var b []byte
		b, mode, err = e.readFile(name)
		if err != nil {
			return "", err
		}
		content = string(b)
		for i, edit := range edits {
			if edit.Old == "" {
				return "", fmt.Errorf("edit %d: old_string must be nonempty", i+1)
			}
			n := strings.Count(content, edit.Old)
			if n != 1 {
				return "", fmt.Errorf("edit %d: old_string must match exactly once (found %d); no changes written", i+1, n)
			}
			content = strings.Replace(content, edit.Old, edit.New, 1)
			if len(content) > maxFile {
				return "", errors.New("result exceeds 4 MiB; no changes written")
			}
		}
	}
	if len(content) > maxFile || !utf8.ValidString(content) || strings.ContainsRune(content, 0) {
		return "", errors.New("content must be UTF-8 text no larger than 4 MiB")
	}
	if err := e.atomicWrite(name, []byte(content), mode); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s: wrote %d bytes", name, len(content)), nil
}
func (e *Executor) atomicWrite(name string, b []byte, mode os.FileMode) error {
	return e.atomicWriteNoFollow(name, b, mode)
}
func (e *Executor) list(call core.ToolCall) (string, error) {
	var a struct {
		Path  string `json:"path"`
		Depth int    `json:"depth"`
	}
	if err := decode(call.Arguments, &a); err != nil {
		return "", err
	}
	name, err := e.path(a.Path, true)
	if err != nil {
		return "", err
	}
	if a.Depth == 0 {
		a.Depth = 1
	}
	if a.Depth < 1 || a.Depth > 5 {
		return "", errors.New("depth must be between 1 and 5")
	}
	var out strings.Builder
	count := 0
	err = e.walk(name, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == name {
			return nil
		}
		if protected(path) || d.Type()&os.ModeSymlink != 0 {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(name, path)
		if err != nil {
			return err
		}
		depth := strings.Count(rel, string(filepath.Separator)) + 1
		if depth > a.Depth {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		count++
		if count > 2000 || out.Len() > maxOutput {
			out.WriteString("[listing truncated]\n")
			return fs.SkipAll
		}
		out.WriteString(path)
		if d.IsDir() {
			out.WriteByte('/')
		}
		out.WriteByte('\n')
		return nil
	})
	return bounded(out.String()), err
}
func (e *Executor) authorize(ctx context.Context, call core.ToolCall, network, review bool) error {
	a := core.Approval{Call: call, Network: network, Task: e.config.Task, Directory: e.directory, Reason: "Allow this workspace file change?"}
	if call.Name == "Bash" {
		a.ExecutionMode = "sandboxed"
		a.Reason = "Run this command in the workspace OS sandbox?"
		if network {
			a.Reason = "Run this sandboxed command with network access for this invocation?"
		}
	}
	return e.authorizeRequest(ctx, a, review)
}
func (e *Executor) authorizeRequest(ctx context.Context, a core.Approval, review bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	label := ""
	if a.ExecutionMode == "unsandboxed" {
		label = " outside the OS sandbox"
	}
	if review && e.config.Mode == "auto" {
		if e.config.Review == nil {
			a.Reason += " Automatic review unavailable: no reviewer is configured."
		} else {
			e.emit("status", "Automatic review in progress"+label+"…")
			decision, err := e.config.Review(ctx, a)
			if err == nil && decision.Allow {
				e.emit("status", "Automatic review approved"+label+": "+decision.Reason)
				return nil
			}
			if err != nil {
				a.Reason += " Automatic review failed: " + bounded(err.Error())
			} else {
				a.Reason += " Automatic approval not granted: " + bounded(decision.Reason)
			}
		}
	}
	e.emit("warning", a.Reason)
	if e.config.Approve == nil {
		return errors.New("approval required; no human approval handler is available")
	}
	decision, err := e.config.Approve(ctx, a)
	if err != nil {
		return fmt.Errorf("approval: %w", err)
	}
	if !decision.Allow {
		e.emit("warning", "Approval denied: "+decision.Reason)
		return fmt.Errorf("approval denied: %s", decision.Reason)
	}
	e.emit("status", "Human approval granted"+label+": "+decision.Reason)
	return nil
}

// Grep gives rg only verified file contents via stdin: it never opens workspace
// paths itself. This keeps os.Root's confinement even with concurrent symlink swaps.
func (e *Executor) grep(ctx context.Context, call core.ToolCall) (string, error) {
	var a struct {
		Pattern    string `json:"pattern"`
		Path       string `json:"path"`
		Glob       string `json:"glob"`
		IgnoreCase bool   `json:"ignore_case"`
	}
	if err := decode(call.Arguments, &a); err != nil {
		return "", err
	}
	if len(a.Pattern) > 16*1024 {
		return "", errors.New("pattern exceeds 16 KiB")
	}
	name, err := e.path(a.Path, true)
	if err != nil {
		return "", err
	}
	if a.Glob != "" {
		if _, err := filepath.Match(a.Glob, ""); err != nil {
			return "", fmt.Errorf("invalid glob: %w", err)
		}
	}
	type source struct {
		path  string
		first int
	}
	var sources []source
	var input bytes.Buffer
	line := 1
	scanned := 0
	truncated := false
	err = e.walk(name, func(path string, d fs.DirEntry, walkErr error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if walkErr != nil {
			return walkErr
		}
		if protected(path) || d.Type()&os.ModeSymlink != 0 {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if d.Name() == "node_modules" || d.Name() == "vendor" {
				return fs.SkipDir
			}
			return nil
		}
		if a.Glob != "" {
			match, _ := filepath.Match(a.Glob, d.Name())
			if !match {
				return nil
			}
		}
		if _, err := e.path(path, false); err != nil {
			return nil
		}
		b, _, err := e.readFile(path)
		if err != nil {
			return nil
		}
		scanned++
		if scanned > 5000 || input.Len()+len(b)+1 > maxSearch {
			truncated = true
			return fs.SkipAll
		}
		if len(b) == 0 {
			return nil
		}
		sources = append(sources, source{path, line})
		input.Write(b)
		n := bytes.Count(b, []byte{'\n'})
		if b[len(b)-1] != '\n' {
			input.WriteByte('\n')
			n++
		}
		line += n
		return nil
	})
	if err != nil {
		return "", err
	}
	if len(sources) == 0 {
		return "No searchable text files.", nil
	}
	rg, err := exec.LookPath("rg")
	if err != nil {
		return "", errors.New("ripgrep is required; install rg and put it on PATH")
	}
	rg, err = filepath.EvalSymlinks(rg)
	if err != nil {
		return "", fmt.Errorf("resolve ripgrep: %w", err)
	}
	if rel, relErr := filepath.Rel(e.directory, rg); relErr == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("ripgrep must be installed outside the workspace")
	}
	args := []string{"--json", "--no-config", "--color", "never", "--max-count", "300", "-e", a.Pattern}
	if a.IgnoreCase {
		args = append(args, "--ignore-case")
	}
	searchCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(searchCtx, rg, args...)
	cmd.Stdin = &input
	cmd.Dir = e.directory
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=en_US.UTF-8"}
	out := &boundedBuffer{limit: maxOutput * 8}
	cmd.Stdout = out
	cmd.Stderr = out
	err = cmd.Run()
	if searchCtx.Err() != nil {
		return "", searchCtx.Err()
	}
	var exit *exec.ExitError
	if err != nil && (!errors.As(err, &exit) || exit.ExitCode() != 1) {
		return "", fmt.Errorf("rg: %w: %s", err, bounded(out.String()))
	}
	var result strings.Builder
	scan := bufio.NewScanner(strings.NewReader(out.String()))
	scan.Buffer(make([]byte, 4096), maxOutput*8)
	matches := 0
	for scan.Scan() {
		var row struct {
			Type string `json:"type"`
			Data struct {
				Line  int `json:"line_number"`
				Lines struct {
					Text string `json:"text"`
				} `json:"lines"`
			} `json:"data"`
		}
		if json.Unmarshal(scan.Bytes(), &row) != nil || row.Type != "match" {
			continue
		}
		i := sort.Search(len(sources), func(i int) bool { return sources[i].first > row.Data.Line }) - 1
		if i < 0 {
			continue
		}
		fmt.Fprintf(&result, "%s:%d:%s", sources[i].path, row.Data.Line-sources[i].first+1, row.Data.Lines.Text)
		matches++
		if result.Len() > maxOutput {
			break
		}
	}
	if matches == 0 {
		if out.truncated {
			result.WriteString("Matching output exceeded the limit; narrow the pattern or path.\n")
		} else {
			result.WriteString("No matches.\n")
		}
	}
	if truncated || out.truncated || matches >= 300 {
		result.WriteString("[search truncated by scan/output limit]\n")
	}
	return bounded(result.String()), nil
}

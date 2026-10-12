package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"julia/internal/core"
)

func fixture(t *testing.T, mode string, approve core.Approver) (*Executor, string) {
	t.Helper()
	dir := t.TempDir()
	e, err := New(Config{Directory: dir, Mode: mode, Approve: approve})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	return e, dir
}
func invoke(e *Executor, name string, args any) core.ToolResult {
	b, _ := json.Marshal(args)
	return e.Execute(context.Background(), core.ToolCall{Name: name, Arguments: b})
}
func TestConfinement(t *testing.T) {
	e, dir := fixture(t, "auto", nil)
	for _, path := range []string{"../escape", "/tmp/escape", ".env", "sub/.env.prod", ".git/config", ".ssh/id_rsa", ".aws/credentials"} {
		if r := invoke(e, "Write", map[string]any{"path": path, "content": "unsafe"}); !r.IsError {
			t.Fatalf("allowed %q", path)
		}
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if r := invoke(e, "Write", map[string]any{"path": "link/escape", "content": "unsafe"}); !r.IsError {
		t.Fatal("followed outside symlink")
	}
	os.WriteFile(filepath.Join(dir, ".env"), []byte("secret"), 0600)
	os.Symlink(".env", filepath.Join(dir, "alias"))
	if r := invoke(e, "Read", map[string]any{"path": "alias"}); !r.IsError {
		t.Fatal("followed protected symlink")
	}
}
func TestMultiEditAtomic(t *testing.T) {
	e, dir := fixture(t, "auto", nil)
	os.WriteFile(filepath.Join(dir, "a"), []byte("alpha beta"), 0644)
	r := invoke(e, "MultiEdit", map[string]any{"path": "a", "edits": []map[string]string{{"old_string": "alpha", "new_string": "changed"}, {"old_string": "absent", "new_string": "x"}}})
	if !r.IsError {
		t.Fatal("missing match accepted")
	}
	b, _ := os.ReadFile(filepath.Join(dir, "a"))
	if string(b) != "alpha beta" {
		t.Fatal("partial write", string(b))
	}
	r = invoke(e, "MultiEdit", map[string]any{"path": "a", "edits": []map[string]string{{"old_string": "alpha", "new_string": "changed"}, {"old_string": "beta", "new_string": "done"}}})
	if r.IsError {
		t.Fatal(r.Content)
	}
	b, _ = os.ReadFile(filepath.Join(dir, "a"))
	if string(b) != "changed done" {
		t.Fatal(string(b))
	}
}
func TestAmbiguousEditRejected(t *testing.T) {
	e, dir := fixture(t, "auto", nil)
	os.WriteFile(filepath.Join(dir, "a"), []byte("x x"), 0644)
	if r := invoke(e, "Edit", map[string]any{"path": "a", "old_string": "x", "new_string": "y"}); !r.IsError {
		t.Fatal("ambiguous edit accepted")
	}
}
func TestApprovalModes(t *testing.T) {
	calls := 0
	approve := func(context.Context, core.Approval) (core.Decision, error) {
		calls++
		return core.Decision{Allow: false, Reason: "denied"}, nil
	}
	e, _ := fixture(t, "ask", approve)
	if r := invoke(e, "Write", map[string]any{"path": "a", "content": "x"}); !r.IsError {
		t.Fatal("denial ignored")
	}
	if calls != 1 {
		t.Fatal(calls)
	}
	e, _ = fixture(t, "read-only", approve)
	if r := invoke(e, "Bash", map[string]any{"command": "true"}); !r.IsError {
		t.Fatal("read-only bash allowed")
	}
	if calls != 1 {
		t.Fatal("prompted for forbidden action")
	}
	e, _ = fixture(t, "auto", nil)
	if r := invoke(e, "Write", map[string]any{"path": "a", "content": "x"}); r.IsError {
		t.Fatal(r.Content)
	}
}
func TestReadAndListBounded(t *testing.T) {
	e, dir := fixture(t, "auto", nil)
	os.WriteFile(filepath.Join(dir, "a"), []byte(strings.Repeat("abc\n", 20000)), 0644)
	r := invoke(e, "Read", map[string]any{"path": "a", "offset": 2, "limit": 3})
	if r.IsError || !strings.Contains(r.Content, "2\tabc") || strings.Contains(r.Content, "5\tabc") {
		t.Fatal(r.Content)
	}
	r = invoke(e, "Read", map[string]any{"path": "a", "limit": 100000})
	if !r.IsError {
		t.Fatal("unbounded limit accepted")
	}
}
func TestGrepHidesSecrets(t *testing.T) {
	e, dir := fixture(t, "auto", nil)
	os.WriteFile(filepath.Join(dir, "a"), []byte("needle\nother\n"), 0644)
	os.WriteFile(filepath.Join(dir, ".env"), []byte("needle secret"), 0600)
	r := invoke(e, "Grep", map[string]any{"pattern": "needle"})
	if r.IsError || !strings.Contains(r.Content, "a:1:needle") || strings.Contains(r.Content, "secret") {
		t.Fatal(r.Content)
	}
}
func TestYoloRunsEverythingWithoutSandboxOrReview(t *testing.T) {
	t.Setenv("JULIA_TEST_YOLO", "inherited")
	e, _ := fixture(t, "yolo", nil)
	dir := e.directory // resolved, as $PWD reports it
	// No reviewer and no approver: nothing may be asked in yolo mode.
	e.config.Review = func(context.Context, core.Approval) (core.Decision, error) {
		t.Fatal("yolo mode consulted the reviewer")
		return core.Decision{}, nil
	}
	if !strings.Contains(e.SandboxInfo(), "yolo") {
		t.Fatalf("sandbox info %q", e.SandboxInfo())
	}
	for _, args := range []map[string]any{
		{"command": "printf \"$JULIA_TEST_YOLO:$JULIA_SCRATCH\"", "network": true},
		{"command": "printf \"$JULIA_TEST_YOLO:$JULIA_SCRATCH\"", "sandbox_permissions": "require_escalated", "justification": "host access"},
	} {
		r := invoke(e, "Bash", args)
		if r.IsError || !strings.HasPrefix(r.Content, "inherited:") || strings.Contains(r.Content, dir) {
			t.Fatalf("yolo Bash %v: %+v", args, r)
		}
	}
	if r := invoke(e, "Write", map[string]any{"path": "note.txt", "content": "ok"}); r.IsError {
		t.Fatalf("yolo Write: %+v", r)
	}
	if r := invoke(e, "Bash", map[string]any{"command": "cat note.txt && printf ' in %s' \"$PWD\""}); r.IsError || !strings.Contains(r.Content, "ok in "+dir) {
		t.Fatalf("yolo Bash runs in the workspace: %+v", r)
	}
	if r := invoke(e, "Bash", map[string]any{"command": "exit 3"}); !r.IsError || !strings.Contains(r.Content, "host command failed") {
		t.Fatalf("yolo Bash failure: %+v", r)
	}
}

func TestBashRequiresReviewOrApproval(t *testing.T) {
	e, _ := fixture(t, "auto", nil)
	r := invoke(e, "Bash", map[string]any{"command": "true", "network": true})
	if !r.IsError || !strings.Contains(r.Content, "approval") {
		t.Fatal(r.Content)
	}
}
func TestAutoRunsOfflineSandboxedBashWithoutReview(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandboxed Bash requires macOS")
	}
	e, _ := fixture(t, "auto", nil)
	e.config.Review = func(context.Context, core.Approval) (core.Decision, error) {
		t.Error("offline sandboxed Bash was sent to review")
		return core.Decision{}, nil
	}
	if r := invoke(e, "Bash", map[string]any{"command": "true"}); r.IsError {
		t.Fatal(r.Content)
	}
}
func TestAskStillApprovesOfflineSandboxedBash(t *testing.T) {
	humans := 0
	e, _ := fixture(t, "ask", func(context.Context, core.Approval) (core.Decision, error) {
		humans++
		return core.Decision{Allow: false}, nil
	})
	if r := invoke(e, "Bash", map[string]any{"command": "true"}); !r.IsError || humans != 1 {
		t.Fatal(r.Content, humans)
	}
}
func TestBoundedBuffer(t *testing.T) {
	b := &boundedBuffer{limit: 12}
	n, err := b.Write([]byte(strings.Repeat("a", 100)))
	if n != 100 || err != nil || len(b.String()) != 12 || !b.truncated {
		t.Fatal(n, err, b.String())
	}
}

func TestMissingWriteContentCannotErase(t *testing.T) {
	e, dir := fixture(t, "auto", nil)
	os.WriteFile(filepath.Join(dir, "a"), []byte("keep"), 0644)
	for _, name := range []string{"Write", "Edit", "MultiEdit"} {
		var args any
		switch name {
		case "Write":
			args = map[string]any{"path": "a"}
		case "Edit":
			args = map[string]any{"path": "a", "old_string": "keep"}
		case "MultiEdit":
			args = map[string]any{"path": "a", "edits": []map[string]string{{"old_string": "keep"}}}
		}
		if r := invoke(e, name, args); !r.IsError {
			t.Fatalf("%s accepted missing replacement", name)
		}
	}
	b, _ := os.ReadFile(filepath.Join(dir, "a"))
	if string(b) != "keep" {
		t.Fatal(string(b))
	}
}

func TestNoFollowAtOpenTime(t *testing.T) {
	e, dir := fixture(t, "auto", nil)
	os.Mkdir(filepath.Join(dir, ".config"), 0700)
	os.WriteFile(filepath.Join(dir, ".config", "secret"), []byte("secret"), 0600)
	os.Mkdir(filepath.Join(dir, "gate"), 0700)
	if _, err := e.path("gate/secret", false); err != nil {
		t.Fatal(err)
	}
	// Replace a path AFTER validation, reproducing the check/open race.
	os.Remove(filepath.Join(dir, "gate"))
	os.Symlink(".config", filepath.Join(dir, "gate"))
	if f, err := e.openNoFollow("gate/secret"); err == nil {
		f.Close()
		t.Fatal("followed directory symlink after path validation")
	}
	if err := e.atomicWriteNoFollow("gate/secret", []byte("changed"), 0600); err == nil {
		t.Fatal("wrote through raced directory symlink")
	}
	os.Symlink(".config/secret", filepath.Join(dir, "alias"))
	if f, err := e.openNoFollow("alias"); err == nil {
		f.Close()
		t.Fatal("followed final symlink")
	}
	b, _ := os.ReadFile(filepath.Join(dir, ".config", "secret"))
	if string(b) != "secret" {
		t.Fatal("protected content changed")
	}
}

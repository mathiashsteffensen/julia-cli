package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"julia/internal/core"
)

// Each operation starts with the same file so its result and side effects can be checked.
type absoluteOperation struct {
	name  string
	write bool
}

var absoluteOperations = []absoluteOperation{
	{"Read", false}, {"List", false}, {"Grep", false},
	{"Edit", true}, {"MultiEdit", true}, {"Write", true},
}

func (op absoluteOperation) args(dir string) map[string]any {
	file := filepath.Join(dir, "note.txt")
	switch op.name {
	case "Read":
		return map[string]any{"path": file}
	case "List":
		return map[string]any{"path": dir}
	case "Grep":
		return map[string]any{"path": dir, "pattern": "needle"}
	case "Edit":
		return map[string]any{"path": file, "old_string": "needle", "new_string": "changed"}
	case "MultiEdit":
		return map[string]any{"path": file, "edits": []map[string]string{{"old_string": "needle", "new_string": "changed"}, {"old_string": "tail", "new_string": "done"}}}
	case "Write":
		return map[string]any{"path": file, "content": "changed"}
	}
	panic("unknown operation")
}

func absoluteTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func absoluteSeed(t *testing.T, dir string) string {
	t.Helper()
	file := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(file, []byte("needle tail"), 0600); err != nil {
		t.Fatal(err)
	}
	return file
}

func absoluteFileContent(t *testing.T, file string) string {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func absoluteCheckSuccess(t *testing.T, op absoluteOperation, result core.ToolResult, file string) {
	t.Helper()
	if result.IsError {
		t.Fatalf("%s: %s", op.name, result.Content)
	}
	if op.write {
		want := "changed"
		if op.name == "Edit" {
			want = "changed tail"
		} else if op.name == "MultiEdit" {
			want = "changed done"
		}
		if got := absoluteFileContent(t, file); got != want {
			t.Fatalf("file = %q, want %q", got, want)
		}
	} else {
		want := "needle"
		if op.name == "List" {
			want = "note.txt"
		}
		if !strings.Contains(result.Content, want) {
			t.Fatalf("%s did not return %q: %s", op.name, want, result.Content)
		}
	}
}

func TestAbsolutePathsWorkspaceModes(t *testing.T) {
	for _, mode := range []string{"auto", "ask", "read-only", "yolo"} {
		for _, op := range absoluteOperations {
			t.Run(mode+"/"+op.name, func(t *testing.T) {
				humans, reviews := 0, 0
				e, _ := fixture(t, mode, func(context.Context, core.Approval) (core.Decision, error) {
					humans++
					return core.Decision{Allow: true}, nil
				})
				e.config.Review = func(context.Context, core.Approval) (core.Decision, error) {
					reviews++
					return core.Decision{Allow: true}, nil
				}
				file := absoluteSeed(t, e.directory)
				result := invoke(e, op.name, op.args(e.directory))
				if mode == "read-only" && op.write {
					if !result.IsError || absoluteFileContent(t, file) != "needle tail" {
						t.Fatalf("read-only mutation: %+v", result)
					}
				} else {
					absoluteCheckSuccess(t, op, result, file)
				}
				wantHumans := 0
				if mode == "ask" && op.write {
					wantHumans = 1
				}
				if humans != wantHumans || reviews != 0 {
					t.Fatalf("approval calls: human=%d reviewer=%d", humans, reviews)
				}
			})
		}
	}
}

func TestAbsolutePathsMatchRelativeWorkspaceResults(t *testing.T) {
	for _, op := range absoluteOperations {
		t.Run(op.name, func(t *testing.T) {
			e, _ := fixture(t, "auto", nil)
			file := absoluteSeed(t, e.directory)
			relative := invoke(e, op.name, op.args("."))
			if relative.IsError {
				t.Fatalf("relative call: %s", relative.Content)
			}
			if err := os.WriteFile(file, []byte("needle tail"), 0600); err != nil {
				t.Fatal(err)
			}
			absolute := invoke(e, op.name, op.args(e.directory))
			if absolute.IsError || absolute.Content != relative.Content {
				t.Fatalf("relative=%+v absolute=%+v", relative, absolute)
			}
		})
	}
}

func TestAbsolutePathsConfiguredWorkspaceAlias(t *testing.T) {
	for _, op := range absoluteOperations {
		t.Run(op.name, func(t *testing.T) {
			base := absoluteTempDir(t)
			workspace := filepath.Join(base, "workspace")
			alias := filepath.Join(base, "alias")
			if err := os.Mkdir(workspace, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(workspace, alias); err != nil {
				t.Fatal(err)
			}
			e, err := New(Config{Directory: alias})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { e.Close() })
			e.config.Review = func(context.Context, core.Approval) (core.Decision, error) {
				t.Fatal("configured workspace alias sent to reviewer")
				return core.Decision{}, nil
			}
			file := absoluteSeed(t, workspace)
			absoluteCheckSuccess(t, op, invoke(e, op.name, op.args(alias)), file)
		})
	}
}

func TestAbsolutePathsExternalModes(t *testing.T) {
	for _, mode := range []string{"auto", "ask", "read-only", "yolo"} {
		for _, op := range absoluteOperations {
			t.Run(mode+"/"+op.name, func(t *testing.T) {
				var approvals []core.Approval
				var humans, reviews int
				e, _ := fixture(t, mode, func(_ context.Context, a core.Approval) (core.Decision, error) {
					humans++
					approvals = append(approvals, a)
					return core.Decision{Allow: true}, nil
				})
				e.config.Review = func(_ context.Context, a core.Approval) (core.Decision, error) {
					reviews++
					approvals = append(approvals, a)
					return core.Decision{Allow: true}, nil
				}
				e.SetTask("original task")
				dir := absoluteTempDir(t)
				file := absoluteSeed(t, dir)
				args, err := json.Marshal(op.args(dir))
				if err != nil {
					t.Fatal(err)
				}
				call := core.ToolCall{ID: "original-id", Name: op.name, Arguments: args}
				result := e.Execute(context.Background(), call)
				if mode == "read-only" {
					if !result.IsError || strings.Contains(result.Content, "needle") || absoluteFileContent(t, file) != "needle tail" {
						t.Fatalf("read-only external access: %+v", result)
					}
				} else {
					absoluteCheckSuccess(t, op, result, file)
				}
				wantHumans, wantReviews := 0, 0
				if mode == "ask" {
					wantHumans = 1
				}
				if mode == "auto" {
					wantReviews = 1
				}
				if humans != wantHumans || reviews != wantReviews {
					t.Fatalf("approval calls: human=%d reviewer=%d", humans, reviews)
				}
				if len(approvals) == 1 {
					a := approvals[0]
					if a.Task != "original task" || a.Directory != e.directory || a.Call.ID != call.ID || a.Call.Name != call.Name || !bytes.Equal(a.Call.Arguments, call.Arguments) {
						t.Fatalf("approval lost original context: %+v", a)
					}
				}
			})
		}
	}
}

func TestAbsolutePathsDeniedReviewHasNoEffects(t *testing.T) {
	for _, op := range absoluteOperations {
		t.Run(op.name, func(t *testing.T) {
			calls := 0
			e, _ := fixture(t, "auto", nil)
			e.config.Review = func(context.Context, core.Approval) (core.Decision, error) {
				calls++
				return core.Decision{Reason: "denied"}, nil
			}
			dir := absoluteTempDir(t)
			file := absoluteSeed(t, dir)
			result := invoke(e, op.name, op.args(dir))
			if !result.IsError || strings.Contains(result.Content, "needle tail") || absoluteFileContent(t, file) != "needle tail" || calls != 1 {
				t.Fatalf("denial not enforced: result=%+v reviews=%d", result, calls)
			}
		})
	}
}

func TestAbsolutePathApprovalIsNotBlanket(t *testing.T) {
	e, _ := fixture(t, "auto", nil)
	calls := 0
	e.config.Review = func(context.Context, core.Approval) (core.Decision, error) {
		calls++
		return core.Decision{Allow: calls == 1, Reason: "second action denied"}, nil
	}
	dir := absoluteTempDir(t)
	absoluteSeed(t, dir)
	first := invoke(e, "Read", map[string]any{"path": filepath.Join(dir, "note.txt")})
	second := invoke(e, "Write", map[string]any{"path": filepath.Join(dir, "other.txt"), "content": "changed"})
	if first.IsError || !second.IsError || calls != 2 {
		t.Fatalf("first=%+v second=%+v reviews=%d", first, second, calls)
	}
	if _, err := os.Stat(filepath.Join(dir, "other.txt")); !os.IsNotExist(err) {
		t.Fatalf("denied write created a file: %v", err)
	}
}

func TestAbsolutePathSiblingIsExternal(t *testing.T) {
	e, _ := fixture(t, "auto", nil)
	// The sibling shares the workspace's string prefix but is not inside it.
	sibling := e.directory + "-sibling"
	if err := os.Mkdir(sibling, 0700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sibling) })
	absoluteSeed(t, sibling)
	calls := 0
	e.config.Review = func(context.Context, core.Approval) (core.Decision, error) {
		calls++
		return core.Decision{Reason: "denied"}, nil
	}
	r := invoke(e, "Read", map[string]any{"path": filepath.Join(sibling, "note.txt")})
	if !r.IsError || calls != 1 || strings.Contains(r.Content, "needle tail") {
		t.Fatalf("sibling treated as workspace: %+v reviews=%d", r, calls)
	}
}

func TestAbsolutePathsProtectedAndTraversal(t *testing.T) {
	for _, outside := range []bool{false, true} {
		e, _ := fixture(t, "yolo", nil)
		dir := e.directory
		if outside {
			dir = absoluteTempDir(t)
		}
		for _, name := range []string{".env", ".git/config", ".ssh/id_rsa", ".aws/credentials", "auth.json"} {
			path := filepath.Join(dir, name)
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("needle secret"), 0600); err != nil {
				t.Fatal(err)
			}
			for _, op := range []string{"Read", "Write"} {
				args := map[string]any{"path": path}
				if op == "Write" {
					args["content"] = "changed"
				}
				if r := invoke(e, op, args); !r.IsError {
					t.Errorf("%s allowed %s", op, path)
				}
			}
			if got := absoluteFileContent(t, path); got != "needle secret" {
				t.Fatalf("protected file changed: %q", got)
			}
		}
		for _, op := range []string{"List", "Grep"} {
			args := map[string]any{"path": dir}
			if op == "Grep" {
				args["pattern"] = "needle"
			}
			r := invoke(e, op, args)
			if r.IsError || strings.Contains(r.Content, "secret") || strings.Contains(r.Content, ".env") || strings.Contains(r.Content, ".git") {
				t.Errorf("%s exposed protected entries: %+v", op, r)
			}
		}
		// Do not use Join here: it would remove the traversal before Execute sees it.
		path := dir + string(filepath.Separator) + "sub/../note.txt"
		if r := invoke(e, "Write", map[string]any{"path": path, "content": "changed"}); !r.IsError {
			t.Error("absolute traversal accepted")
		}
	}
}

func TestAbsolutePathsRejectSymlinks(t *testing.T) {
	for _, op := range absoluteOperations {
		for _, position := range []string{"leaf", "parent"} {
			for _, scope := range []string{"workspace", "external"} {
				t.Run(op.name+"/"+position+"/"+scope, func(t *testing.T) {
					e, _ := fixture(t, "yolo", nil)
					base := e.directory
					if scope == "external" {
						base = absoluteTempDir(t)
					}
					target := filepath.Join(base, "target")
					if err := os.Mkdir(target, 0700); err != nil {
						t.Fatal(err)
					}
					link := filepath.Join(base, "link")
					var dir, file string
					if position == "parent" {
						child := filepath.Join(target, "child")
						if err := os.Mkdir(child, 0700); err != nil {
							t.Fatal(err)
						}
						file = absoluteSeed(t, child)
						dir = filepath.Join(link, "child")
						if err := os.Symlink(target, link); err != nil {
							t.Fatal(err)
						}
					} else if op.name == "List" || op.name == "Grep" {
						file = absoluteSeed(t, target)
						dir = link
						if err := os.Symlink(target, link); err != nil {
							t.Fatal(err)
						}
					} else {
						file = absoluteSeed(t, target)
						dir = link
						if err := os.Mkdir(link, 0700); err != nil {
							t.Fatal(err)
						}
						if err := os.Symlink(file, filepath.Join(link, "note.txt")); err != nil {
							t.Fatal(err)
						}
					}
					r := invoke(e, op.name, op.args(dir))
					if !r.IsError || absoluteFileContent(t, file) != "needle tail" {
						t.Fatalf("symlink followed: %+v", r)
					}
				})
			}
		}
	}
}

func TestAbsolutePathParentSwapDuringReview(t *testing.T) {
	for _, op := range absoluteOperations {
		t.Run(op.name, func(t *testing.T) {
			e, _ := fixture(t, "auto", nil)
			base := absoluteTempDir(t)
			gate := filepath.Join(base, "gate")
			target := filepath.Join(base, "target")
			for _, dir := range []string{gate, target} {
				if err := os.Mkdir(dir, 0700); err != nil {
					t.Fatal(err)
				}
			}
			file := absoluteSeed(t, target)
			absoluteSeed(t, gate)
			calls := 0
			e.config.Review = func(context.Context, core.Approval) (core.Decision, error) {
				calls++
				if err := os.Rename(gate, filepath.Join(base, "original")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, gate); err != nil {
					t.Fatal(err)
				}
				return core.Decision{Allow: true}, nil
			}
			r := invoke(e, op.name, op.args(gate))
			if !r.IsError || calls != 1 || strings.Contains(r.Content, "needle tail") || absoluteFileContent(t, file) != "needle tail" {
				t.Fatalf("followed swapped parent: %+v reviews=%d", r, calls)
			}
		})
	}
}

func TestAbsoluteExternalMultiEditAtomicAndMissingContent(t *testing.T) {
	e, _ := fixture(t, "auto", nil)
	e.config.Review = func(context.Context, core.Approval) (core.Decision, error) { return core.Decision{Allow: true}, nil }
	file := absoluteSeed(t, absoluteTempDir(t))
	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"MultiEdit", map[string]any{"path": file, "edits": []map[string]string{{"old_string": "needle", "new_string": "changed"}, {"old_string": "absent", "new_string": "x"}}}},
		{"Write", map[string]any{"path": file}},
	} {
		r := invoke(e, tc.name, tc.args)
		if !r.IsError || absoluteFileContent(t, file) != "needle tail" {
			t.Fatalf("%s changed file: %+v", tc.name, r)
		}
	}
}

func TestAbsoluteExternalWriteCreatesParentsOnlyAfterApproval(t *testing.T) {
	for _, allow := range []bool{false, true} {
		t.Run(map[bool]string{false: "denied", true: "approved"}[allow], func(t *testing.T) {
			e, _ := fixture(t, "auto", nil)
			base := absoluteTempDir(t)
			parent := filepath.Join(base, "new", "nested")
			calls := 0
			e.config.Review = func(context.Context, core.Approval) (core.Decision, error) {
				calls++
				if _, err := os.Stat(filepath.Join(base, "new")); !os.IsNotExist(err) {
					t.Errorf("parent exists before approval: %v", err)
				}
				return core.Decision{Allow: allow, Reason: "decision"}, nil
			}
			file := filepath.Join(parent, "note.txt")
			r := invoke(e, "Write", map[string]any{"path": file, "content": "created"})
			if calls != 1 || r.IsError == allow {
				t.Fatalf("result=%+v reviews=%d", r, calls)
			}
			if allow {
				if got := absoluteFileContent(t, file); got != "created" {
					t.Fatalf("file=%q", got)
				}
			} else if _, err := os.Stat(filepath.Join(base, "new")); !os.IsNotExist(err) {
				t.Fatalf("denied write created parent: %v", err)
			}
		})
	}
}

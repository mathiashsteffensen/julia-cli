package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"julia/internal/core"
)

// fileSystem holds only filesystem access, not the executor's approval state.
// The workspace root stays pinned for the executor's lifetime. An external call
// gets its own root after approval and closes it when that call ends.
type fileSystem struct {
	root *os.Root
}

type fileTarget struct {
	fileSystem
	name    string
	outside bool
}

func (t *fileTarget) close() {
	if t.outside {
		t.root.Close()
	}
}

func (t *fileTarget) display(name string) string {
	if t.outside {
		return filepath.Join(string(filepath.Separator), name)
	}
	return name
}

// workspaceRelative accepts either spelling of the workspace: the configured
// directory or the resolved directory pinned by os.Root. It never resolves a
// caller-supplied symlink or treats a sibling with the same prefix as contained.
func workspaceRelative(path string, directories ...string) (string, bool) {
	for _, base := range directories {
		if base == "" {
			continue
		}
		if rel, err := filepath.Rel(base, path); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return rel, true
		}
	}
	return "", false
}

// fileTarget gates all six text/directory tools before opening external paths.
// Review sees the original call and workspace; it grants only this invocation.
func (e *Executor) fileTarget(ctx context.Context, call core.ToolCall, path string, allowRoot bool) (*fileTarget, error) {
	name, err := cleanFilePath(path, allowRoot)
	if err != nil {
		return nil, err
	}

	target := &fileTarget{fileSystem: e.fileSystem, name: name}
	if filepath.IsAbs(name) {
		if rel, inside := workspaceRelative(name, e.directory, e.config.Directory); inside {
			target.name = rel
		} else {
			target.outside = true
			target.name = strings.TrimPrefix(name, string(filepath.Separator))
			if target.name == "" {
				target.name = "."
			}
		}
	}
	if target.name == "." && !allowRoot {
		return nil, errors.New("file path is required")
	}
	if protected(target.name) {
		return nil, errors.New("protected credential or metadata path is forbidden")
	}

	mutation := call.Name == "Write" || call.Name == "Edit" || call.Name == "MultiEdit"
	if e.config.Mode == "read-only" && (mutation || target.outside) {
		return nil, errors.New("read-only mode prohibits file changes and paths outside the workspace")
	}
	if target.outside {
		approval := core.Approval{
			Call: call, Task: e.config.Task, Directory: e.directory,
			Reason: fmt.Sprintf("Allow %s outside the workspace at %s? Symlinks and protected paths remain forbidden; approval covers only this call.", call.Name, name),
		}
		if err := e.authorizeRequest(ctx, approval, true); err != nil {
			return nil, err
		}
		root, err := os.OpenRoot(string(filepath.Separator))
		if err != nil {
			return nil, fmt.Errorf("open filesystem root: %w", err)
		}
		target.fileSystem = fileSystem{root: root}
	} else if mutation && e.config.Mode == "ask" {
		if err := e.authorize(ctx, call, false, false); err != nil {
			return nil, err
		}
	}

	target.name, err = target.path(target.name, allowRoot)
	if err != nil {
		target.close()
		return nil, err
	}
	return target, nil
}

// cleanFilePath checks the spelling before cleaning: a/../b must not erase
// evidence of parent traversal, including when a is a symlink.
func cleanFilePath(name string, allowRoot bool) (string, error) {
	if name == "" {
		if !allowRoot {
			return "", errors.New("path is required")
		}
		name = "."
	}
	if strings.ContainsRune(name, 0) {
		return "", errors.New("path contains a NUL byte")
	}
	for _, part := range strings.Split(filepath.ToSlash(name), "/") {
		if part == ".." {
			return "", errors.New("parent traversal is forbidden")
		}
	}
	name = filepath.Clean(name)
	if (name == "." || name == string(filepath.Separator)) && !allowRoot {
		return "", errors.New("file path is required")
	}
	return name, nil
}

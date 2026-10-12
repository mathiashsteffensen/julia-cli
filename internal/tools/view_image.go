package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"jarvis/internal/core"
	"jarvis/internal/images"

	"golang.org/x/sys/unix"
)

// scratchVariable is how the agent knows the session scratch directory.
const scratchVariable = "$JARVIS_SCRATCH"

func viewImageDefinition() core.ToolDefinition {
	return core.ToolDefinition{Name: "ViewImage", Description: fmt.Sprintf("Look at an image file (PNG, JPEG, GIF, WebP, TIFF or BMP, up to %d MiB), such as a screenshot or a rendered chart. It is scaled to at most %d px on its long side. Takes a workspace-relative path, or a file in %s written as %s/name.png or as its absolute path. Symlinks and protected credential/git paths are forbidden.", images.MaxFileBytes>>20, images.MaxEdge, scratchVariable, scratchVariable),
		Parameters: schema(map[string]any{"path": str("Workspace-relative path, or a path inside " + scratchVariable)}, "path")}
}

// viewImage loads an image for the model. Scratch files are allowed besides the
// workspace because that is where the agent renders and screenshots things.
func (e *Executor) viewImage(ctx context.Context, call core.ToolCall) (core.ToolResult, error) {
	var a struct {
		Path string `json:"path"`
	}
	if err := decode(call.Arguments, &a); err != nil {
		return core.ToolResult{}, err
	}
	file, name, err := e.openImage(a.Path)
	if err != nil {
		return core.ToolResult{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return core.ToolResult{}, err
	}
	if !info.Mode().IsRegular() {
		return core.ToolResult{}, fmt.Errorf("%s is not a regular file", name)
	}
	tooLarge := fmt.Errorf("%s is larger than the %d MiB an image may be", name, images.MaxFileBytes>>20)
	if info.Size() > images.MaxFileBytes {
		return core.ToolResult{}, tooLarge
	}
	data, err := io.ReadAll(io.LimitReader(file, images.MaxFileBytes+1))
	if err != nil {
		return core.ToolResult{}, fmt.Errorf("read %s: %w", name, err)
	}
	if len(data) > images.MaxFileBytes {
		return core.ToolResult{}, tooLarge
	}
	if err := ctx.Err(); err != nil {
		return core.ToolResult{}, err
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		if len(data) == 0 {
			return core.ToolResult{}, fmt.Errorf("%s is empty", name)
		}
		if utf8.Valid(data) && bytes.IndexByte(data, 0) < 0 {
			return core.ToolResult{}, fmt.Errorf("%s is a text file, not an image; use Read for text", name)
		}
		return core.ToolResult{}, fmt.Errorf("%s is not an image ViewImage can open (PNG, JPEG, GIF, WebP, TIFF or BMP)", name)
	}
	img, err := images.Prepare(data)
	if err != nil {
		return core.ToolResult{}, fmt.Errorf("%s: %w", name, err)
	}
	// The model learns the size it sees and, when scaled, the original's, so it
	// can relate what it sees to the file's own pixels.
	original := core.ImageInfo{MediaType: "image/" + format, Width: config.Width, Height: config.Height}
	sent := img.Info()
	text := fmt.Sprintf("%s · %d×%d %s", name, original.Width, original.Height, original.Format())
	if sent.Width != original.Width || sent.Height != original.Height || sent.Format() != original.Format() {
		text += fmt.Sprintf(", sent as %d×%d %s", sent.Width, sent.Height, sent.Format())
	}
	return core.ToolResult{Content: text, Images: []core.Image{img}}, nil
}

// openImage opens a ViewImage path without following symlinks and returns it
// with the name the result shows: a workspace-relative path as Read takes it,
// or a path inside the scratch directory as $JARVIS_SCRATCH/….
func (e *Executor) openImage(path string) (*os.File, string, error) {
	inside, scratch, err := e.scratchPath(path)
	if err != nil {
		return nil, "", err
	}
	if !scratch {
		name, err := e.path(path, false)
		if err != nil {
			return nil, "", err
		}
		file, err := e.openNoFollow(name)
		if err != nil {
			return nil, "", fmt.Errorf("open %s: %w", name, err)
		}
		return file, name, nil
	}
	dir, err := e.scratchDir()
	if err != nil {
		return nil, "", fmt.Errorf("scratch directory: %w", err)
	}
	name := scratchVariable + "/" + filepath.ToSlash(inside)
	file, err := openBeneath(dir, inside)
	if err != nil {
		return nil, "", fmt.Errorf("open %s: %w", name, err)
	}
	return file, name, nil
}

// scratchPath reports whether path names a file in the scratch directory, as
// $JARVIS_SCRATCH/… or absolutely, and returns it relative to that directory.
// Any other absolute path is refused: outside the scratch directory only the
// workspace is reachable, and only by relative path.
func (e *Executor) scratchPath(path string) (string, bool, error) {
	if strings.ContainsRune(path, 0) {
		return "", false, errors.New("path contains a NUL byte")
	}
	var rest string
	named := false
	for _, variable := range []string{scratchVariable, "${JARVIS_SCRATCH}"} {
		if after, ok := strings.CutPrefix(path, variable); ok && (after == "" || after[0] == '/') {
			rest, named = strings.TrimPrefix(after, "/"), true
		}
	}
	if !named && !filepath.IsAbs(path) {
		return "", false, nil
	}
	if !named {
		dir, err := e.scratchDir()
		if err != nil {
			return "", false, fmt.Errorf("scratch directory: %w", err)
		}
		// The configured directory may sit under a system symlink such as /var on
		// macOS; the agent can meet it in either spelling.
		for _, base := range []string{dir, filepath.Clean(e.config.ScratchDir)} {
			if relative, err := filepath.Rel(base, filepath.Clean(path)); err == nil && base != "." && relative != "." && !strings.HasPrefix(relative, "..") {
				rest = relative
				break
			}
		}
		if rest == "" {
			return "", false, fmt.Errorf("absolute paths must be inside %s; give workspace files as workspace-relative paths", scratchVariable)
		}
	}
	for _, part := range strings.Split(filepath.ToSlash(rest), "/") {
		if part == ".." {
			return "", false, errors.New("parent traversal is forbidden")
		}
	}
	rest = filepath.Clean(rest)
	if rest == "." {
		return "", false, errors.New("file path is required")
	}
	if protected(rest) {
		return "", false, errors.New("protected credential or metadata path is forbidden")
	}
	return rest, true, nil
}

// openBeneath opens name inside dir, refusing a symlink at every component.
// Each directory is pinned by descriptor, so a component swapped for a symlink
// between checks still cannot lead outside dir.
func openBeneath(dir, name string) (*os.File, error) {
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(filepath.ToSlash(name), "/")
	for i, part := range parts {
		var stat unix.Stat_t
		if err := unix.Fstatat(fd, part, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			unix.Close(fd)
			return nil, err
		}
		if stat.Mode&unix.S_IFMT == unix.S_IFLNK {
			unix.Close(fd)
			return nil, errors.New("symlink paths are forbidden")
		}
		flags := unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_CLOEXEC
		if i < len(parts)-1 {
			flags |= unix.O_DIRECTORY
		} else {
			flags |= unix.O_NONBLOCK // a FIFO must not block the tool
		}
		next, err := unix.Openat(fd, part, flags, 0)
		unix.Close(fd)
		if err != nil {
			return nil, err
		}
		fd = next
	}
	return os.NewFile(uintptr(fd), name), nil
}

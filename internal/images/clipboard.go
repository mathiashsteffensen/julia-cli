package images

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"jarvis/internal/core"
)

// Clipboard returns the image on the system clipboard: image data, such as a
// screenshot, or an image file copied in Finder. It returns ErrNoImage when the
// clipboard holds anything else, such as text.
func Clipboard(ctx context.Context) (core.Image, error) {
	switch runtime.GOOS {
	case "darwin":
		return darwinClipboard(ctx)
	case "linux":
		return linuxClipboard(ctx)
	}
	return core.Image{}, fmt.Errorf("pasting images is not supported on %s", runtime.GOOS)
}

// darwinScript writes the clipboard's image to the file named by its argument
// and prints "data", or prints "file:" and the path of a copied file. A copied
// file comes first: Finder also puts the file's icon on the clipboard as TIFF.
// The PNG representation is preferred to TIFF, which macOS often coerces to
// from whatever the source application provided.
const darwinScript = `on run argv
	try
		return "file:" & POSIX path of (the clipboard as «class furl»)
	end try
	try
		set img to the clipboard as «class PNGf»
	on error
		try
			set img to the clipboard as «class TIFF»
		on error
			try
				set img to the clipboard as JPEG picture
			on error
				return "none"
			end try
		end try
	end try
	set fh to open for access POSIX file (item 1 of argv) with write permission
	try
		set eof fh to 0
		write img to fh
	on error message
		close access fh
		error message
	end try
	close access fh
	return "data"
end run`

func darwinClipboard(ctx context.Context) (core.Image, error) {
	file, err := os.CreateTemp("", "jarvis-clipboard-*")
	if err != nil {
		return core.Image{}, err
	}
	path := file.Name()
	file.Close()
	defer os.Remove(path)
	out, err := exec.CommandContext(ctx, "osascript", "-e", darwinScript, path).Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && len(exit.Stderr) > 0 {
			return core.Image{}, fmt.Errorf("read clipboard: %s", strings.TrimSpace(string(exit.Stderr)))
		}
		return core.Image{}, fmt.Errorf("read clipboard: %w", err)
	}
	result := strings.TrimSpace(string(out))
	switch {
	case result == "data":
		data, err := os.ReadFile(path)
		if err != nil {
			return core.Image{}, err
		}
		return Prepare(data)
	case strings.HasPrefix(result, "file:"):
		copied := strings.TrimPrefix(result, "file:")
		if !IsImagePath(copied) {
			return core.Image{}, ErrNoImage
		}
		return Load(copied)
	}
	return core.Image{}, ErrNoImage
}

// linuxClipboard asks wl-paste on Wayland, then xclip on X11, for a PNG.
func linuxClipboard(ctx context.Context) (core.Image, error) {
	commands := [][]string{
		{"wl-paste", "--no-newline", "--type", "image/png"},
		{"xclip", "-selection", "clipboard", "-target", "image/png", "-out"},
	}
	found := false
	for _, command := range commands {
		if _, err := exec.LookPath(command[0]); err != nil {
			continue
		}
		found = true
		out, err := exec.CommandContext(ctx, command[0], command[1:]...).Output()
		if err == nil && len(out) > 0 && bytes.HasPrefix(out, []byte("\x89PNG")) {
			return Prepare(out)
		}
	}
	if !found {
		return core.Image{}, errors.New("pasting images needs wl-paste (Wayland) or xclip (X11)")
	}
	return core.Image{}, ErrNoImage
}

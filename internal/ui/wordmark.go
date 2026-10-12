package ui

import (
	"bytes"
	_ "embed"
	"image/png"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/ansi/kitty"
)

// The welcome screen draws its wordmark in block lettering (see logo.go), so
// this image is no longer placed. Its upload remains the graphics probe: the
// terminal's acknowledgement is what confirms Kitty graphics work before any
// thumbnail is sent, and its deletion on quit is the test that the delete
// reaches the terminal before the screen is restored.
//
//go:embed assets/wordmark.png
var wordmarkPNG []byte

const (
	wordmarkID      = 0x4a4152
	wordmarkColumns = 18
	wordmarkRows    = 2
)

type wordmarkExitMsg struct{}

func supportsWordmark(env tea.EnvMsg) bool {
	// Multiplexers need separate graphics passthrough support. Keep the normal
	// text wordmark there, and when color conversion would corrupt the image ID.
	return (env.Getenv("TERM_PROGRAM") == "ghostty" || env.Getenv("TERM") == "xterm-ghostty") &&
		env.Getenv("TMUX") == "" && env.Getenv("STY") == "" && env.Getenv("ZELLIJ") == "" &&
		!strings.HasPrefix(env.Getenv("TERM"), "screen") && !strings.HasPrefix(env.Getenv("TERM"), "tmux") &&
		env.Getenv("NO_COLOR") == ""
}

func (m *model) requestWordmark() tea.Cmd {
	if !m.wordmarkEnabled || !m.wordmarkTrueColor || m.quitting {
		m.wordmarkReady = false
		m.wordmarkPending = false
		return nil
	}
	m.wordmarkReady = false
	m.wordmarkPending = true
	// Wait for a terminal round trip before uploading. This ensures the first
	// frame (or resize clear) precedes the upload in the terminal's input stream.
	return tea.Raw(ansi.RequestModeAltScreenSaveCursor)
}

func uploadWordmark() tea.Cmd {
	img, err := png.Decode(bytes.NewReader(wordmarkPNG))
	if err != nil {
		return nil // A decorative asset must never prevent starting the harness.
	}
	var out bytes.Buffer
	if err := kitty.EncodeGraphics(&out, img, &kitty.Options{
		Action: kitty.TransmitAndPut, Transmission: kitty.Direct, Format: kitty.PNG,
		ID: wordmarkID, Columns: wordmarkColumns, Rows: wordmarkRows,
		VirtualPlacement: true, DoNotMoveCursor: true, Chunk: true,
	}); err != nil {
		return nil
	}
	return tea.Raw(out.String())
}

func (m *model) quit() tea.Cmd {
	m.cancelWeeklyUsage()
	m.quitting = true
	if !m.wordmarkEnabled {
		return tea.Quit
	}
	m.wordmarkStopping = true
	// Raw is buffered. Wait for a round trip so deletion reaches the terminal
	// before Bubble Tea restores the main screen, with a bounded fallback.
	deleteImage := ansi.KittyGraphics(nil, "a=d", "d=I", "i="+strconv.Itoa(wordmarkID), "q=2") + thumbnailDeletes(m.thumbnailOrder)
	return tea.Sequence(tea.Raw(deleteImage+ansi.RequestModeTextCursorEnable),
		tea.Tick(250*time.Millisecond, func(time.Time) tea.Msg { return wordmarkExitMsg{} }))
}

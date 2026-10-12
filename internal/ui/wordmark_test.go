package ui

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/ansi/kitty"
)

func TestWordmarkWaitsForTerminalAcknowledgementAndRestoresAfterResize(t *testing.T) {
	m := testModel(t, Config{})
	m.resize(128, 40)
	m.Update(tea.ColorProfileMsg{Profile: colorprofile.TrueColor})
	_, cmd := m.Update(tea.EnvMsg{"TERM_PROGRAM=ghostty", "TERM=xterm-ghostty"})
	if cmd == nil || !strings.Contains(ansi.Strip(m.View().Content), "A little less supervision") {
		t.Fatal("must show the welcome while waiting for terminal")
	}
	if raw := rawOutput(cmd); raw != ansi.RequestModeAltScreenSaveCursor+ansi.WindowOp(ansi.RequestCellSizeWinOp) {
		t.Fatalf("must round trip before uploading, and ask the cell size: %q", raw)
	}
	_, cmd = m.Update(tea.ModeReportMsg{Mode: ansi.ModeAltScreenSaveCursor, Value: ansi.ModeSet})
	if !strings.Contains(rawOutput(cmd), "U=1") {
		t.Fatal("missing virtual image upload")
	}
	m.Update(uv.KittyGraphicsEvent{Options: kitty.Options{ID: wordmarkID}, Payload: []byte("EINVAL")})
	if m.wordmarkReady {
		t.Fatal("failed image must retain text fallback")
	}
	m.Update(uv.KittyGraphicsEvent{Options: kitty.Options{ID: wordmarkID + 1}, Payload: []byte("OK")})
	if m.wordmarkReady {
		t.Fatal("unrelated acknowledgement activated wordmark")
	}
	m.Update(uv.KittyGraphicsEvent{Options: kitty.Options{ID: wordmarkID}, Payload: []byte("OK")})
	if !m.wordmarkReady || !m.graphicsConfirmed {
		t.Fatal("acknowledged upload must confirm graphics")
	}
	// The welcome draws its wordmark in block lettering; the uploaded image is
	// the graphics probe only, so no placeholder cell appears in the view.
	if strings.ContainsRune(m.View().Content, kitty.Placeholder) {
		t.Fatal("welcome placed the probe image")
	}
	_, cmd = m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	if cmd == nil || m.wordmarkReady {
		t.Fatal("resize must reupload after renderer clears the screen")
	}
	m.Update(uv.KittyGraphicsEvent{Options: kitty.Options{ID: wordmarkID}, Payload: []byte("OK")})
	if m.wordmarkReady {
		t.Fatal("stale acknowledgement bypassed resize barrier")
	}
	m.quit()
	_, cmd = m.Update(tea.ModeReportMsg{Mode: ansi.ModeAltScreenSaveCursor, Value: ansi.ModeSet})
	if cmd != nil {
		t.Fatal("in-flight resize response bypassed shutdown handshake")
	}
	_, cmd = m.Update(tea.ModeReportMsg{Mode: ansi.ModeTextCursorEnable, Value: ansi.ModeReset})
	if cmd == nil {
		t.Fatal("did not quit after image deletion reached the terminal")
	}
}

func TestWordmarkFallsBackInUnsupportedTerminals(t *testing.T) {
	for _, env := range []tea.EnvMsg{
		{"TERM_PROGRAM=Apple_Terminal"},
		{"TERM_PROGRAM=ghostty", "TMUX=/tmp/tmux"},
		{"TERM_PROGRAM=ghostty", "TERM=screen-256color"},
		{"TERM_PROGRAM=ghostty", "STY=screen"},
		{"TERM_PROGRAM=ghostty", "ZELLIJ=0"},
		{"TERM_PROGRAM=ghostty", "NO_COLOR=1"},
	} {
		m := testModel(t, Config{})
		m.Update(tea.ColorProfileMsg{Profile: colorprofile.TrueColor})
		_, cmd := m.Update(env)
		if cmd != nil || m.wordmarkEnabled {
			t.Fatalf("graphics enabled for %v", env)
		}
	}
	m := testModel(t, Config{})
	m.Update(tea.ColorProfileMsg{Profile: colorprofile.ANSI256})
	_, cmd := m.Update(tea.EnvMsg{"TERM_PROGRAM=ghostty"})
	if cmd != nil {
		t.Fatal("must not upload when color conversion would damage image IDs")
	}
}

// rawOutput runs cmd, and every command a batch of it holds, and returns what
// they write to the terminal, in order.
func rawOutput(cmd tea.Cmd) string {
	if cmd == nil {
		return ""
	}
	switch msg := cmd().(type) {
	case tea.RawMsg:
		return fmt.Sprint(msg.Msg)
	case tea.BatchMsg:
		var out strings.Builder
		for _, cmd := range msg {
			out.WriteString(rawOutput(cmd))
		}
		return out.String()
	}
	return ""
}

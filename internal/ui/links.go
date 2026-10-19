package ui

import (
	"errors"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// A click on a link opens it in the browser. Mouse reporting keeps the
// terminal from seeing the click, so JULIA finds the link itself: first the
// OSC 8 hyperlink Glamour wraps around markdown links and bare URLs (it carries
// the whole URL even where the text wraps), then a URL in the plain text, which
// is how links appear in your messages and in tool output.

var plainURL = regexp.MustCompile(`https?://[^\s<>"'` + "`" + `\x{00a0}]+`)

type linkOpenedMsg struct {
	url string
	err error
}

// openBrowser is a variable so tests never launch a real browser.
var openBrowser = func(link string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", link)
	case "linux":
		cmd = exec.Command("xdg-open", link)
	default:
		return errors.New("no browser launcher for " + runtime.GOOS)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}

// webLink keeps only http and https: a markdown link from the model must not
// get to launch file:// paths or other URL handlers.
func webLink(link string) bool {
	lower := strings.ToLower(link)
	return strings.HasPrefix(lower, "https://") || strings.HasPrefix(lower, "http://")
}

// linkAt returns the web link shown at a conversation cell, or "".
func (m *model) linkAt(at cell) string {
	if at.row >= len(m.lines) || m.rows[at.row].skip {
		return ""
	}
	if link := hyperlinkAt(m.lines[at.row], at.col); webLink(link) {
		return link
	}
	if link := m.plainLinkAt(at); webLink(link) {
		return link
	}
	return ""
}

// hyperlinkAt returns the OSC 8 target covering cell col of a rendered line.
func hyperlinkAt(line string, col int) string {
	var state byte
	link, cells := "", 0
	for len(line) > 0 {
		seq, width, n, newState := ansi.DecodeSequence(line, state, nil)
		state, line = newState, line[n:]
		if strings.HasPrefix(seq, "\x1b]8;") {
			body := strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(seq, "\x1b]8;"), "\a"), "\x1b\\")
			_, link, _ = strings.Cut(body, ";")
			continue
		}
		if width > 0 && col >= cells && col < cells+width {
			return link
		}
		cells += width
	}
	return ""
}

// plainLinkAt finds a URL in the text of the clicked row, joined with the rows a
// wrap split it across.
func (m *model) plainLinkAt(at cell) string {
	first, last := at.row, at.row
	for first > 0 && m.rows[first].cont {
		first--
	}
	for last+1 < len(m.rows) && m.rows[last+1].cont {
		last++
	}
	var text strings.Builder
	offset := -1
	for row := first; row <= last; row++ {
		ref := m.rows[row]
		plain := ansi.Strip(m.lines[row])
		end := ansi.StringWidth(plain)
		if ref.end > 0 {
			end = min(end, ref.end)
		}
		part := ansi.Cut(plain, ref.gutter, end)
		if row == at.row {
			if at.col < ref.gutter || at.col >= end {
				return ""
			}
			offset = text.Len() + len(ansi.Cut(plain, ref.gutter, at.col))
		}
		if row < last {
			part = strings.TrimRight(part, " ")
		}
		text.WriteString(part)
	}
	joined := text.String()
	for _, span := range plainURL.FindAllStringIndex(joined, -1) {
		link := trimURL(joined[span[0]:span[1]])
		if offset >= span[0] && offset < span[0]+len(link) {
			return link
		}
	}
	return ""
}

// trimURL drops the sentence punctuation that follows a URL in prose, and a
// closing bracket the URL did not open.
func trimURL(link string) string {
	for link != "" {
		last := link[len(link)-1]
		switch {
		case strings.IndexByte(".,:;!?*_", last) >= 0:
		case last == ')' && strings.Count(link, "(") < strings.Count(link, ")"):
		case last == ']' && strings.Count(link, "[") < strings.Count(link, "]"):
		default:
			return link
		}
		link = link[:len(link)-1]
	}
	return link
}

// openLink opens a link in the local browser. Over SSH that browser is on the
// wrong machine, so the link is copied through the terminal instead.
func (m *model) openLink(link string) tea.Cmd {
	if os.Getenv("SSH_TTY") != "" || os.Getenv("SSH_CONNECTION") != "" {
		m.notice = "✓ Copied link · this is a remote session, so it cannot open a browser here"
		return tea.SetClipboard(link)
	}
	m.notice = "Opening " + ansi.Truncate(link, max(20, m.mainWidth-12), "…")
	open := m.openURL
	return func() tea.Msg { return linkOpenedMsg{url: link, err: open(link)} }
}

func (m *model) receiveLinkOpened(msg linkOpenedMsg) {
	if msg.err != nil {
		m.notice = "Could not open the link: " + msg.err.Error()
	}
}

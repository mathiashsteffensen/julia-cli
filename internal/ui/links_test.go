package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func linkModel(t *testing.T, entries ...entry) (*model, *[]string) {
	t.Helper()
	t.Setenv("SSH_TTY", "")
	t.Setenv("SSH_CONNECTION", "")
	m, _ := copyModel(t)
	m.resize(70, 40)
	var opened []string
	m.openURL = func(link string) error { opened = append(opened, link); return nil }
	m.entries = entries
	m.refresh(true)
	m.viewport.GotoTop()
	return m, &opened
}

// clickText clicks the first cell of text on screen and runs what the click returns.
func clickText(t *testing.T, m *model, text string) {
	t.Helper()
	y := screenRow(t, m, text)
	plain := ansi.Strip(m.lines[y-3+m.viewport.YOffset()])
	x := strings.Index(plain, text)
	if x < 0 {
		t.Fatalf("row %q lacks %q", plain, text)
	}
	if cmd := click(m, m.margin()+ansi.StringWidth(plain[:x]), y); cmd != nil {
		if msg, ok := cmd().(linkOpenedMsg); ok {
			m.Update(msg)
		}
	}
}

const longURL = "https://github.com/charmbracelet/bubbletea/blob/main/examples/very/long/path/that/keeps/going/README.md"

func TestClickOnWrappedReplyURLOpensTheWholeURL(t *testing.T) {
	m, opened := linkModel(t, entry{role: "assistant", text: "Read " + longURL + " for more."})
	clickText(t, m, "keeps/going") // the second half of the wrapped URL
	if len(*opened) != 1 || (*opened)[0] != longURL {
		t.Fatalf("opened %q, want %q", *opened, longURL)
	}
	if !strings.HasPrefix(m.notice, "Opening https://github.com") {
		t.Errorf("notice = %q", m.notice)
	}
}

func TestClickOnMarkdownLinkTextOpensItsTarget(t *testing.T) {
	m, opened := linkModel(t, entry{role: "assistant", text: "See [the docs](https://example.com/docs)."})
	clickText(t, m, "docs")
	if len(*opened) != 1 || (*opened)[0] != "https://example.com/docs" {
		t.Fatalf("opened %q", *opened)
	}
}

func TestClickOnURLInYourMessageOpensIt(t *testing.T) {
	m, opened := linkModel(t, entry{role: "user", text: "check " + longURL + ", please"})
	clickText(t, m, "keeps")
	if len(*opened) != 1 || (*opened)[0] != longURL {
		t.Fatalf("opened %q, want %q without the comma", *opened, longURL)
	}
}

func TestClickBesideALinkOpensNothing(t *testing.T) {
	m, opened := linkModel(t, entry{role: "assistant", text: "Plain words here, then https://example.com"})
	clickText(t, m, "words")
	if len(*opened) != 0 {
		t.Fatalf("opened %q", *opened)
	}
}

func TestNonWebLinksAreNotOpened(t *testing.T) {
	m, opened := linkModel(t, entry{role: "assistant", text: "Run [this](file:///Applications/Calculator.app) now."})
	clickText(t, m, "this")
	if len(*opened) != 0 {
		t.Fatalf("opened %q", *opened)
	}
}

func TestRemoteSessionCopiesTheLinkInstead(t *testing.T) {
	m, opened := linkModel(t, entry{role: "assistant", text: "Go to https://example.com/x now."})
	t.Setenv("SSH_TTY", "/dev/ttys001")
	clickText(t, m, "example.com")
	if len(*opened) != 0 || !strings.Contains(m.notice, "Copied link") {
		t.Fatalf("opened %q, notice %q", *opened, m.notice)
	}
}

func TestTrimURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://a.dev/x.":                     "https://a.dev/x",
		"https://a.dev/x),":                    "https://a.dev/x",
		"https://en.wikipedia.org/wiki/Go_(x)": "https://en.wikipedia.org/wiki/Go_(x)",
		"https://a.dev/**":                     "https://a.dev/",
	} {
		if got := trimURL(in); got != want {
			t.Errorf("trimURL(%q) = %q, want %q", in, got, want)
		}
	}
}

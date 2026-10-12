package browser

import (
	"encoding/json/jsontext"
	"net/url"
	"strings"
	"testing"

	cdpruntime "github.com/chromedp/cdproto/runtime"
)

func TestParseKey(t *testing.T) {
	for _, tc := range []struct {
		spec, key, code, char string
		mods                  int64
		commands              string
		err                   string
	}{
		{spec: "a", key: "a", code: "KeyA", char: "a"},
		{spec: "A", key: "a", code: "KeyA", char: "A", mods: modShift},
		{spec: "?", key: "/", code: "Slash", char: "?", mods: modShift},
		{spec: "1", key: "1", code: "Digit1", char: "1"},
		{spec: "é", char: "é"},
		{spec: "Enter", key: "Enter", code: "Enter"},
		{spec: "return", key: "Enter", code: "Enter"},
		{spec: "esc", key: "Escape", code: "Escape"},
		{spec: "ArrowDown", key: "ArrowDown", code: "ArrowDown"},
		{spec: "down", key: "ArrowDown", code: "ArrowDown"},
		{spec: "pgdown", key: "PageDown", code: "PageDown"},
		{spec: "F5", key: "F5", code: "F5"},
		{spec: "space", key: " ", code: "Space", char: " "},
		{spec: "Shift", key: "Shift", code: "ShiftLeft"},
		{spec: "ctrl+a", key: "a", code: "KeyA", char: "a", mods: modCtrl, commands: "selectAll"},
		{spec: "Control+Shift+K", key: "k", code: "KeyK", char: "K", mods: modCtrl | modShift},
		{spec: "meta+z", key: "z", code: "KeyZ", char: "z", mods: modMeta, commands: "undo"},
		{spec: "cmd+shift+z", key: "z", code: "KeyZ", char: "Z", mods: modMeta | modShift, commands: "redo"},
		{spec: "shift+Tab", key: "Tab", code: "Tab", mods: modShift},
		{spec: "alt++", key: "=", code: "Equal", char: "+", mods: modAlt | modShift},
		{spec: "C-c", key: "c", code: "KeyC", char: "c", mods: modCtrl, commands: "copy"},
		{spec: "M-x", key: "x", code: "KeyX", char: "x", mods: modAlt},
		{spec: "hyper+a", err: "unknown modifier"},
		{spec: "Enterr", err: "unknown key"},
		{spec: "\t", err: "control character"},
		{spec: "", err: "empty"},
	} {
		t.Run(tc.spec, func(t *testing.T) {
			k, err := parseKey(tc.spec)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("err = %v, want %q", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if k.def.key != tc.key || k.def.code != tc.code || k.char != tc.char || k.modifiers != tc.mods || strings.Join(k.commands(), ",") != tc.commands {
				t.Fatalf("got key %q code %q char %q mods %d commands %v", k.def.key, k.def.code, k.char, k.modifiers, k.commands())
			}
		})
	}
	if err := CheckKeys("ctrl+a", "Enter"); err != nil {
		t.Fatal(err)
	}
	if err := CheckKeys(); err == nil {
		t.Fatal("no keys accepted")
	}
}

func TestParseTarget(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Target
		err  bool
	}{
		{in: "e12", want: Target{Ref: "e12"}},
		{in: " e3 ", want: Target{Ref: "e3"}},
		{in: "text=Save changes", want: Target{Text: "Save changes"}},
		{in: `"Sign in"`, want: Target{Text: "Sign in"}},
		{in: "css=e12", want: Target{Selector: "e12"}},
		{in: "button.primary", want: Target{Selector: "button.primary"}},
		{in: "#e1", want: Target{Selector: "#e1"}},
		{in: "120, 48.5", want: Target{Point: &Point{120, 48.5}}},
		{in: "e0", want: Target{Selector: "e0"}},
		{in: "", err: true},
	} {
		got, err := ParseTarget(tc.in)
		if tc.err {
			if err == nil {
				t.Errorf("ParseTarget(%q) accepted", tc.in)
			}
			continue
		}
		if err != nil || got.String() != tc.want.String() {
			t.Errorf("ParseTarget(%q) = %s, %v; want %s", tc.in, got, err, tc.want)
		}
	}
	for _, bad := range []Target{{}, {Ref: "e1", Text: "x"}, {Ref: "12"}, {Point: &Point{-1, 4}}} {
		if bad.check() == nil {
			t.Errorf("%+v passed the check", bad)
		}
	}
}

func TestRefusedSchemes(t *testing.T) {
	for raw, want := range map[string]string{
		"http://localhost:3000/":       "",
		"https://example.com/":         "",
		"file:///Users/me/index.html":  "",
		"data:text/html,hi":            "",
		"blob:http://localhost/abc":    "",
		"ws://localhost/socket":        "",
		"about:blank":                  "",
		"about:srcdoc":                 "",
		"about:config":                 "only about:blank",
		"chrome://settings":            "not available",
		"chrome-untrusted://x":         "not available",
		"devtools://devtools/":         "not available",
		"view-source:http://x/":        "not available",
		"javascript:alert(1)":          "evaluate script",
		"filesystem:http://x/temp/a":   "not available",
		"intent://scan/#Intent;end":    "not available",
		"chrome-extension://abcdef/":   "not available",
		"isolated-app://abc/index.htm": "not available",
	} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := refused(u); (want == "") != (got == "") || !strings.Contains(got, want) {
			t.Errorf("refused(%s) = %q, want %q", raw, got, want)
		}
	}
}

func TestIsLoopback(t *testing.T) {
	for host, want := range map[string]bool{
		"localhost": true, "LOCALHOST.": true, "app.localhost": true, "127.0.0.1": true, "127.8.9.1": true,
		"[::1]": true, "::1": true, "localhost.example.com": false, "0.0.0.0": false, "10.0.0.1": false, "example.com": false,
	} {
		if isLoopback(host) != want {
			t.Errorf("isLoopback(%q) = %v", host, !want)
		}
	}
}

func TestConsoleText(t *testing.T) {
	str := func(s string) *cdpruntime.RemoteObject {
		raw, _ := jsontext.AppendQuote(nil, s)
		return &cdpruntime.RemoteObject{Type: "string", Value: raw}
	}
	num := &cdpruntime.RemoteObject{Type: "number", Value: jsontext.Value("36")}
	obj := &cdpruntime.RemoteObject{Type: "object", Preview: &cdpruntime.ObjectPreview{Type: "object", Description: "Object", Properties: []*cdpruntime.PropertyPreview{{Name: "a", Type: "number", Value: "1"}, {Name: "b", Type: "string", Value: "x"}}}}
	arr := &cdpruntime.RemoteObject{Type: "object", Preview: &cdpruntime.ObjectPreview{Type: "object", Subtype: "array", Overflow: true, Properties: []*cdpruntime.PropertyPreview{{Name: "0", Type: "number", Value: "1"}}}}
	for _, tc := range []struct {
		args []*cdpruntime.RemoteObject
		want string
	}{
		{[]*cdpruntime.RemoteObject{str("hello %s, you are %d"), str("Ada"), num}, "hello Ada, you are 36"},
		{[]*cdpruntime.RemoteObject{str("%cstyled"), str("color: red")}, "styled"},
		{[]*cdpruntime.RemoteObject{str("100%"), num}, "100% 36"},
		{[]*cdpruntime.RemoteObject{str("obj"), obj, arr}, `obj {a: 1, b: "x"} [1, …]`},
		{[]*cdpruntime.RemoteObject{{Type: "undefined"}, {Type: "number", UnserializableValue: "NaN"}}, "undefined NaN"},
	} {
		if got := consoleText(tc.args); got != tc.want {
			t.Errorf("consoleText = %q, want %q", got, tc.want)
		}
	}
}

func TestNotesCoalesceBlockedRequests(t *testing.T) {
	c := &Context{}
	c.note("p1", NotePopup, "p1 opened a new page p2")
	for _, path := range []string{"/a.js", "/b.css", "/a.js", "/c.png", "/d.png"} {
		c.blocked(nil, "GET", "https://cdn.example"+path, "Script", "not a loopback address", "page")
	}
	c.blocked(nil, "GET", "https://fonts.example/x", "Font", "not a loopback address", "page")
	notes := c.Notes()
	got := noteText(notes)
	want := `[p1 popup] p1 opened a new page p2
[ blocked] blocked 5 requests to https://cdn.example (GET /a.js, GET /b.css, GET /c.png, …): not a loopback address
[ blocked] blocked GET https://fonts.example/x (font): not a loopback address
`
	if got != want {
		t.Fatalf("notes:\n%s\nwant:\n%s", got, want)
	}
	if len(c.Notes()) != 0 {
		t.Fatal("notes were not drained")
	}
	for i := 0; i < maxNotes+5; i++ {
		c.note("", NoteDialog, "x")
	}
	if notes := c.Notes(); len(notes) != maxNotes+1 || !strings.Contains(notes[maxNotes].Text, "5 more notes") {
		t.Fatalf("overflow: %d notes, last %v", len(notes), notes[len(notes)-1])
	}
}

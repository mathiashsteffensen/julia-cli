package terminal

import (
	"strings"
	"testing"
)

func TestKeyEncoding(t *testing.T) {
	for _, tc := range []struct {
		key       string
		appCursor bool
		want      string
	}{
		{"a", false, "a"},
		{"A", false, "A"},
		{"é", false, "é"},
		{"+", false, "+"},
		{"alt++", false, "\x1b+"},
		{"Enter", false, "\r"},
		{"return", false, "\r"},
		{"alt+enter", false, "\x1b\r"},
		{"tab", false, "\t"},
		{"shift+tab", false, "\x1b[Z"},
		{"BTab", false, "\x1b[Z"},
		{"backspace", false, "\x7f"},
		{"ctrl+backspace", false, "\x08"},
		{"BSpace", false, "\x7f"},
		{"esc", false, "\x1b"},
		{"space", false, " "},
		{"ctrl+space", false, "\x00"},
		{"alt+space", false, "\x1b "},
		{"up", false, "\x1b[A"},
		{"up", true, "\x1bOA"},
		{"left", true, "\x1bOD"},
		{"home", false, "\x1b[H"},
		{"end", true, "\x1bOF"},
		{"shift+up", true, "\x1b[1;2A"},
		{"ctrl+alt+left", false, "\x1b[1;7D"},
		{"C-M-right", false, "\x1b[1;7C"},
		{"pgup", false, "\x1b[5~"},
		{"NPage", false, "\x1b[6~"},
		{"ctrl+pgdown", false, "\x1b[6;5~"},
		{"insert", false, "\x1b[2~"},
		{"delete", false, "\x1b[3~"},
		{"shift+del", false, "\x1b[3;2~"},
		{"f1", false, "\x1bOP"},
		{"F4", false, "\x1bOS"},
		{"alt+f2", false, "\x1b[1;3Q"},
		{"f5", false, "\x1b[15~"},
		{"f12", false, "\x1b[24~"},
		{"shift+f10", false, "\x1b[21;2~"},
		{"ctrl+c", false, "\x03"},
		{"ctrl+C", false, "\x03"},
		{"C-c", false, "\x03"},
		{"control+d", false, "\x04"},
		{"ctrl+[", false, "\x1b"},
		{"ctrl+\\", false, "\x1c"},
		{"ctrl+_", false, "\x1f"},
		{"ctrl+?", false, "\x7f"},
		{"ctrl+alt+x", false, "\x1b\x18"},
		{"alt+x", false, "\x1bx"},
		{"M-x", false, "\x1bx"},
		{"shift+a", false, "A"},
		{"alt+shift+a", false, "\x1bA"},
	} {
		k, err := parseKey(tc.key)
		if err != nil {
			t.Errorf("parseKey(%q): %v", tc.key, err)
			continue
		}
		got, err := k.encode(tc.appCursor)
		if err != nil || got != tc.want {
			t.Errorf("%q (app cursor %v) = %q, %v; want %q", tc.key, tc.appCursor, got, err, tc.want)
		}
	}
}

func TestKeyErrors(t *testing.T) {
	for key, want := range map[string]string{
		"":             "unknown key",
		"hello":        "unknown key",
		"ctrl+foo":     "unknown key",
		"super+a":      "unknown modifier",
		"shift+enter":  "alt+enter",
		"ctrl+enter":   "no legacy",
		"ctrl+tab":     "no legacy",
		"shift+1":      "shifted character",
		"ctrl+shift+a": "no legacy",
		"ctrl+1":       "no legacy",
		"shift+space":  "no legacy",
		"S-space":      "no legacy",
		"\x01":         "not a printable",
	} {
		if err := CheckKeys("a", key); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("key %q: error %v, want one mentioning %q", key, err, want)
		}
	}
	if err := CheckKeys("ctrl+c", "alt+enter", "S-up", "é"); err != nil {
		t.Error(err)
	}
}

func TestMouseEncoding(t *testing.T) {
	for _, tc := range []struct {
		encoding string
		code     int
		release  bool
		col, row int
		want     string
	}{
		{"sgr", 0, false, 5, 3, "\x1b[<0;5;3M"},
		{"sgr", 2, true, 5, 3, "\x1b[<2;5;3m"},
		{"sgr", 64, false, 300, 100, "\x1b[<64;300;100M"},
		{"sgr-pixels", 0, false, 2, 1, "\x1b[<0;23;16M"},
		{"x10", 0, false, 1, 1, "\x1b[M !!"},
		{"x10", 2 | 16, true, 1, 1, "\x1b[M3!!"}, // release is button 3, modifiers kept
		{"urxvt", 1, false, 10, 20, "\x1b[33;10;20M"},
		{"utf8", 0, false, 300, 1, "\x1b[M Ō!"},
	} {
		got, err := encodeMouse(tc.encoding, tc.code, tc.release, tc.col, tc.row)
		if err != nil || got != tc.want {
			t.Errorf("%s %d release=%v at %d,%d = %q, %v; want %q", tc.encoding, tc.code, tc.release, tc.col, tc.row, got, err, tc.want)
		}
	}
	if _, err := encodeMouse("x10", 0, false, 224, 1); err == nil {
		t.Error("X10 encoded a column beyond its range")
	}
}

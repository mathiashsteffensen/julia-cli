package core

import "testing"

func TestVisibleText(t *testing.T) {
	for _, tc := range []struct {
		name, in, out string
		newlines      bool
	}{
		{"plain text is untouched", "Keep it simple, 界字 é", "Keep it simple, 界字 é", false},
		{"newlines kept when asked", "a\nb\r\nc", "a\nb \nc", true},
		{"newlines become spaces otherwise", "a\nb\tc\u2028d\u2029e", "a b c d e", false},
		{"controls and escapes are dropped", "a\x00b\x07c\x1b[2J\x7f", "abc[2J", false},
		{"zero-width and bidi marks are dropped", "a\u200bb\u200ec\u200fd\u202ee\u2066f\u2060g\ufeffh\u00adi", "abcdefghi", false},
		{"tag characters are dropped", "ok\U000e0049\U000e0047\U000e004e\U000e004f\U000e0052\U000e0045", "ok", false},
		{"joiners that emoji and some scripts need are kept", "👨\u200d👩\u200d👧 می\u200cخواهم", "👨\u200d👩\u200d👧 می\u200cخواهم", false},
		{"odd separators become spaces", "a\u0085b\vc\fd", "a b c d", false},
		{"blank code points are dropped", "a\u3164b\u2800c\u034fd\u115fe\uffa0f\U000e0101g", "abcdefg", false},
	} {
		if got := VisibleText(tc.in, tc.newlines); got != tc.out {
			t.Errorf("%s: VisibleText(%q) = %q, want %q", tc.name, tc.in, got, tc.out)
		}
	}
}

func TestHasVisibleText(t *testing.T) {
	for in, want := range map[string]bool{
		"":              false,
		" \t\n":         false,
		"\u200d\u200c":  false,
		"\u0301":        false, // a lone combining mark
		"a":             true,
		"7":             true,
		"?":             true,
		"✓":             true,
		"👨\u200d👩":      true,
		"\u200d界\u200d": true,
	} {
		if got := HasVisibleText(in); got != want {
			t.Errorf("HasVisibleText(%q) = %v, want %v", in, got, want)
		}
	}
}

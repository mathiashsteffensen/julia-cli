package core

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// VisibleText removes what a reader cannot see but a model can read: control
// characters, Unicode format characters such as the tag block, zero-width spaces
// and bidirectional overrides, variation-selector payloads, and code points that
// draw as blank (Hangul fillers, the empty Braille cell, the grapheme joiner).
// Line breaks survive only when keepNewlines is set; tabs and other separators
// become spaces. The zero-width joiners that emoji and some scripts need stay.
func VisibleText(s string, keepNewlines bool) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' && keepNewlines:
			return r
		case r == '\t' || r == '\r' || r == '\n' || r == '\v' || r == '\f' || r == '\u0085' || r == '\u2028' || r == '\u2029':
			return ' '
		case r == '\u200c' || r == '\u200d':
			return r
		case unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || blankCodePoint(r):
			return -1
		}
		return r
	}, s)
}

func blankCodePoint(r rune) bool {
	switch r {
	case '\u034f', '\u115f', '\u1160', '\u2800', '\u3164', '\uffa0':
		return true
	}
	return r >= 0xe0100 && r <= 0xe01ef // variation selectors supplement
}

// HasVisibleText reports whether s contains anything a reader would see: a
// letter, number, punctuation mark or symbol, not only joiners and marks.
func HasVisibleText(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.IsPunct(r) || unicode.IsSymbol(r) {
			return true
		}
	}
	return false
}

// CapText keeps the head and tail of an oversized body: the start says what it
// is and the end how it finished. It cuts on rune boundaries and never makes the
// text longer.
func CapText(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	head, tail := limit/2, len(s)-(limit-limit/2)
	for head > 0 && !utf8.RuneStart(s[head]) {
		head--
	}
	for tail < len(s) && !utf8.RuneStart(s[tail]) {
		tail++
	}
	marker := fmt.Sprintf("\n[%d bytes omitted]\n", tail-head)
	if len(marker) >= tail-head {
		return s
	}
	return s[:head] + marker + s[tail:]
}

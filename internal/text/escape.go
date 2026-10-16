// Package text turns fetched pages into text an agent can read, and
// normalises text for comparing quotes. It is pure.
package text

import (
	"fmt"
	"strings"
	"unicode"
)

// Invisible reports whether a rune would not show, or would change how
// the text around it shows, when the text is printed: control and format
// characters (zero-width, bidi controls, Unicode tag characters), private
// use, unassigned code points, line and paragraph separators, variation
// selectors and the other default-ignorable code points such as the Hangul
// fillers. Newline and tab are the exceptions.
//
// These can carry a message a person reading the page never sees, so the
// agent gets them escaped.
func Invisible(r rune) bool {
	if r == '\n' || r == '\t' {
		return false
	}
	if !unicode.IsGraphic(r) {
		return true
	}
	return unicode.Is(unicode.Variation_Selector, r) ||
		unicode.Is(unicode.Other_Default_Ignorable_Code_Point, r) ||
		r >= 0xE0000 && r <= 0xE0FFF
}

// Escape writes each invisible rune in s as \u{XXXX} and returns how many
// it escaped.
func Escape(s string) (string, int) {
	n := 0
	var b strings.Builder
	for _, r := range s {
		if Invisible(r) {
			n++
			fmt.Fprintf(&b, `\u{%04X}`, r)
			continue
		}
		b.WriteRune(r)
	}
	return b.String(), n
}

// EscapeOnly is Escape without the count, for text the tool prints that
// came from elsewhere: a URL, a title, a search snippet.
func EscapeOnly(s string) string {
	out, _ := Escape(s)
	return out
}

// HasInvisible reports whether s holds an invisible rune.
func HasInvisible(s string) bool {
	return strings.IndexFunc(s, Invisible) >= 0
}

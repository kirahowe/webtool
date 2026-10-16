package text

import (
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

// This file reads an inline style into declarations, tokenising as CSS
// Syntax does for the parts that matter here.

// --- tokens

type kind int

const (
	tIdent kind = iota
	tNumber
	tPercent
	tDimension
	tFunction // text is the name, args the tokens inside
	tBlock    // ( [ or {, args the tokens inside
	tString
	tURL
	tHash
	tDelim
	tSpace
	tUnknown // a value these rules cannot read, such as an unresolved var()
)

type token struct {
	kind kind
	text string  // ASCII-lower-cased ident, function name, unit, hash or delim
	num  float64 // for numbers, percentages and dimensions
	args []token
}

func (t token) numeric() bool {
	return t.kind == tNumber || t.kind == tPercent || t.kind == tDimension
}

// asciiLower lower-cases ASCII letters only, as CSS matches names. Unicode
// lower-casing would turn the Kelvin sign into k.
func asciiLower(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + 'a' - 'A'
		}
		return r
	}, s)
}

type lexer struct {
	s       string
	pos     int
	depth   int
	tooDeep bool
}

// maxNesting bounds brackets inside brackets. A style nested deeper hides
// its element rather than be half read.
const maxNesting = 32

func (l *lexer) peek(k int) byte {
	if l.pos+k < len(l.s) {
		return l.s[l.pos+k]
	}
	return 0
}

func (l *lexer) validEscape(k int) bool {
	return l.peek(k) == '\\' && l.pos+k+1 < len(l.s) && l.peek(k+1) != '\n'
}

func identStart(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_' || c >= 0x80
}

func identChar(c byte) bool { return identStart(c) || c >= '0' && c <= '9' || c == '-' }

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' }

func (l *lexer) startsIdent() bool {
	switch {
	case identStart(l.peek(0)) || l.validEscape(0):
		return true
	case l.peek(0) == '-':
		return identStart(l.peek(1)) || l.peek(1) == '-' || l.validEscape(1)
	}
	return false
}

func (l *lexer) startsNumber() bool {
	c := l.peek(0)
	switch {
	case isDigit(c):
		return true
	case c == '.':
		return isDigit(l.peek(1))
	case c == '+' || c == '-':
		return isDigit(l.peek(1)) || l.peek(1) == '.' && isDigit(l.peek(2))
	}
	return false
}

// escape consumes an escape after its backslash and returns the code point.
func (l *lexer) escape() rune {
	start := l.pos
	for l.pos < len(l.s) && l.pos < start+6 && isHex(l.s[l.pos]) {
		l.pos++
	}
	if l.pos == start {
		if l.pos >= len(l.s) {
			return utf8.RuneError
		}
		r, n := utf8.DecodeRuneInString(l.s[l.pos:])
		l.pos += n
		return r
	}
	cp, _ := strconv.ParseUint(l.s[start:l.pos], 16, 32)
	if isSpace(l.peek(0)) {
		l.pos++
	}
	if cp == 0 || cp > utf8.MaxRune || cp >= 0xD800 && cp <= 0xDFFF {
		return utf8.RuneError
	}
	return rune(cp)
}

func (l *lexer) name() string {
	var b strings.Builder
	for l.pos < len(l.s) {
		switch c := l.s[l.pos]; {
		case identChar(c):
			b.WriteByte(c)
			l.pos++
		case l.validEscape(0):
			l.pos++
			b.WriteRune(l.escape())
		default:
			return b.String()
		}
	}
	return b.String()
}

// number consumes a number as CSS Syntax does: sign, digits, a fraction
// only when a digit follows the dot, an exponent only when a digit
// follows the e.
func (l *lexer) number() float64 {
	start := l.pos
	if c := l.peek(0); c == '+' || c == '-' {
		l.pos++
	}
	for isDigit(l.peek(0)) {
		l.pos++
	}
	if l.peek(0) == '.' && isDigit(l.peek(1)) {
		l.pos++
		for isDigit(l.peek(0)) {
			l.pos++
		}
	}
	if c := l.peek(0); c == 'e' || c == 'E' {
		if isDigit(l.peek(1)) {
			l.pos++
		} else if (l.peek(1) == '+' || l.peek(1) == '-') && isDigit(l.peek(2)) {
			l.pos += 2
		}
		for isDigit(l.peek(0)) {
			l.pos++
		}
	}
	// An out-of-range number is clamped, as browsers clamp it.
	f, _ := strconv.ParseFloat(l.s[start:l.pos], 64)
	if math.IsNaN(f) {
		f = 0
	}
	return f
}

func (l *lexer) skipComment() bool {
	if l.peek(0) != '/' || l.peek(1) != '*' {
		return false
	}
	if end := strings.Index(l.s[l.pos+2:], "*/"); end >= 0 {
		l.pos += 2 + end + 2
	} else {
		l.pos = len(l.s)
	}
	return true
}

// url consumes an unquoted url( ... ) after its "(". It takes no
// nesting and runs to the first ")" that is not escaped.
func (l *lexer) url() token {
	for l.pos < len(l.s) {
		switch l.s[l.pos] {
		case ')':
			l.pos++
			return token{kind: tURL}
		case '\\':
			if l.validEscape(0) {
				l.pos++
				l.escape()
				continue
			}
		}
		l.pos++
	}
	return token{kind: tURL}
}

// tokens reads tokens until the closing byte (0 for the end, or for the
// top level where ";" also ends a declaration).
func (l *lexer) tokens(closing byte, top bool) []token {
	var out []token
	space := func() {
		if len(out) == 0 || out[len(out)-1].kind != tSpace {
			out = append(out, token{kind: tSpace})
		}
	}
	for l.pos < len(l.s) && !l.tooDeep {
		c := l.s[l.pos]
		switch {
		case l.skipComment():
			space()
		case isSpace(c):
			l.pos++
			space()
		case closing != 0 && c == closing:
			l.pos++
			return out
		case top && c == ';':
			l.pos++
			return out
		case c == '"' || c == '\'':
			l.pos++
			var b strings.Builder
			for l.pos < len(l.s) && l.s[l.pos] != c && l.s[l.pos] != '\n' {
				if l.s[l.pos] == '\\' && l.pos+1 < len(l.s) {
					l.pos++
					if l.s[l.pos] == '\n' {
						l.pos++
						continue
					}
					b.WriteRune(l.escape())
					continue
				}
				b.WriteByte(l.s[l.pos])
				l.pos++
			}
			if l.pos < len(l.s) && l.s[l.pos] == c {
				l.pos++
			}
			out = append(out, token{kind: tString, text: b.String()})
		case l.startsNumber():
			f := l.number()
			switch {
			case l.peek(0) == '%':
				l.pos++
				out = append(out, token{kind: tPercent, num: f})
			case l.startsIdent():
				out = append(out, token{kind: tDimension, num: f, text: asciiLower(l.name())})
			default:
				out = append(out, token{kind: tNumber, num: f})
			}
		case l.startsIdent():
			n := l.name()
			if !strings.HasPrefix(n, "--") {
				// Custom property names are case-sensitive; all others
				// match in ASCII lower case.
				n = asciiLower(n)
			}
			if l.peek(0) != '(' {
				out = append(out, token{kind: tIdent, text: n})
				continue
			}
			l.pos++
			if n == "url" {
				for isSpace(l.peek(0)) {
					l.pos++
				}
				if q := l.peek(0); q != '"' && q != '\'' {
					out = append(out, l.url())
					continue
				}
			}
			out = append(out, token{kind: tFunction, text: n, args: l.nested(')')})
		case c == '(' || c == '[' || c == '{':
			l.pos++
			out = append(out, token{kind: tBlock, text: string(c), args: l.nested(map[byte]byte{'(': ')', '[': ']', '{': '}'}[c])})
		case c == '#':
			l.pos++
			out = append(out, token{kind: tHash, text: asciiLower(l.name())})
		default:
			r, n := utf8.DecodeRuneInString(l.s[l.pos:])
			l.pos += n
			out = append(out, token{kind: tDelim, text: string(r)})
		}
	}
	return out
}

func (l *lexer) nested(closing byte) []token {
	if l.depth >= maxNesting {
		l.tooDeep = true
		return nil
	}
	l.depth++
	defer func() { l.depth-- }()
	return l.tokens(closing, false)
}

// preprocess applies CSS's input preprocessing: CR LF, CR and form feed
// become LF, and NUL becomes U+FFFD.
var preprocess = strings.NewReplacer("\r\n", "\n", "\r", "\n", "\f", "\n", "\x00", "�")

// --- declarations

type declaration struct {
	prop      string
	value     []token // without whitespace
	important bool
}

// parseDeclarations reads an inline style. It reports false when the style
// nests brackets too deeply to read.
func parseDeclarations(style string) ([]declaration, bool) {
	l := &lexer{s: preprocess.Replace(style)}
	var out []declaration
	for l.pos < len(l.s) && !l.tooDeep {
		toks := significant(l.tokens(0, true))
		if len(toks) < 2 || toks[0].kind != tIdent || toks[1].kind != tDelim || toks[1].text != ":" {
			continue
		}
		d := declaration{prop: toks[0].text, value: toks[2:]}
		if n := len(d.value); n >= 2 && d.value[n-2].kind == tDelim && d.value[n-2].text == "!" &&
			d.value[n-1].kind == tIdent && d.value[n-1].text == "important" {
			d.important, d.value = true, d.value[:n-2]
		}
		out = append(out, d)
	}
	return out, !l.tooDeep
}

// significant drops whitespace tokens, here and inside functions.
func significant(toks []token) []token {
	var out []token
	for _, t := range toks {
		if t.kind == tSpace {
			continue
		}
		if t.args != nil {
			t.args = significant(t.args)
		}
		out = append(out, t)
	}
	return out
}

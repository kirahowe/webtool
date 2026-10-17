package text

import (
	"context"
	"net/url"
	"strings"
	"unicode"
)

// Extract reads a body of the given media type, already decoded to UTF-8:
// plain text as it is, anything else as HTML.
func Extract(ctx context.Context, body []byte, mediaType string, base *url.URL, limit int) Page {
	if mediaType == "text/plain" {
		return Plain(body, limit)
	}
	return HTML(ctx, body, base, limit)
}

// Compact is the Page with only what the fetch output and check need: the
// agent's text cut at textChars runes (its full length kept in
// TextRunes), and the visible text without the characters quote matching
// drops anyway. Breaks, newlines, paragraph separators and bidi controls
// stay, since Segments reads them.
func (p Page) Compact(textChars int) Page {
	r := []rune(p.Text)
	p.TextRunes = len(r)
	if len(r) > textChars {
		p.Text = string(r[:textChars])
	}
	p.Visible = strings.Map(func(c rune) rune {
		switch {
		case c == Break || c == '\n' || c >= 0x202A && c <= 0x202E || c >= 0x2066 && c <= 0x2069:
			return c
		case c == 0x85 || c == 0x2028 || c == 0x2029:
			// Separators Segments reads as breaks. Each costs JSON no
			// more than twice its own bytes. A vertical tab would cost
			// six times, as \u000b, and so reads as a space below.
			return c
		case unicode.IsSpace(c):
			// Any other space, a vertical tab, a lone CR or a form
			// feed: a space, as
			// Normalise reads it, so the words either side stay apart.
			return ' '
		case Invisible(c):
			return -1
		}
		return c
	}, p.Visible)
	return p
}

// Read is what production reads a body into: Extract, then Compact. The
// reader process returns it, and check's tests read through it too.
func Read(ctx context.Context, body []byte, mediaType string, base *url.URL, limit, textChars int) Page {
	return Extract(ctx, body, mediaType, base, limit).Compact(textChars)
}

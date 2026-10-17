package text

import (
	"strings"
	"unicode"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// punctuation maps the quote marks and dashes a writer, an editor or a
// web page might use for one another onto plain ASCII.
var punctuation = strings.NewReplacer(
	"‘", "'", "’", "'", "‚", "'", "‛", "'", "′", "'", "‵", "'",
	"`", "'", "´", "'", "ʼ", "'", "ʻ", "'",
	"“", `"`, "”", `"`, "„", `"`, "‟", `"`, "″", `"`, "‶", `"`,
	"«", `"`, "»", `"`, "〝", `"`, "〞", `"`,
	"‐", "-", "‑", "-", "‒", "-", "–", "-", "—", "-", "―", "-",
	"−", "-", "⸺", "-", "⸻", "-", "﹘", "-", "﹣", "-",
)

// Normalise prepares text for finding a quote in it. It drops invisible
// characters, applies Unicode NFKC, folds case, maps quote marks and
// dashes to ASCII, and collapses whitespace to single spaces.
//
// The quote and the page go through the same steps, so a quote copied
// from the page as a person sees it is found however its quote marks,
// case and spacing were changed on the way.
func Normalise(s string) string {
	s = strings.Map(func(r rune) rune {
		if Invisible(r) && !unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
	s = norm.NFKC.String(s)
	s = cases.Fold().String(s)
	s = punctuation.Replace(s)
	return strings.Join(strings.FieldsFunc(s, unicode.IsSpace), " ")
}

// Find reports whether quote occurs in the normalised text. An empty
// quote is never found.
func Find(normalisedText, quote string) bool {
	q := Normalise(quote)
	return q != "" && strings.Contains(normalisedText, q)
}

// Segments splits visible text into the runs a quote may lie in: it breaks
// at every Break and at a vertical tab, NEL, or line or paragraph
// separator, and leaves out text inside a bidi embedding, override or
// isolate, since that text may be drawn in another order than written.
// Embeddings and overrides (LRE, RLE, LRO, RLO) end at their PDF and
// isolates (LRI, RLI, FSI) at their PDI, as the Unicode bidi algorithm
// pairs them, and all end with the paragraph (a newline, a paragraph
// separator or NEL). Each segment is normalised.
func Segments(visible string) []string {
	var segs []string
	var cur strings.Builder
	flush := func() {
		if n := Normalise(cur.String()); n != "" {
			segs = append(segs, n)
		}
		cur.Reset()
	}
	// The stack holds true for an isolate, false for an embedding or
	// override; isolates holds the stack positions of the isolates, so a
	// PDI finds its isolate at once.
	var stack []bool
	var isolates []int
	for _, r := range visible {
		switch {
		case r == Break || r == '\v' || r == 0x2028:
			// A vertical tab or line separator is no space in HTML, and a
			// browser may draw nothing for it, so a quote never runs
			// across one.
			flush()
		case r >= 0x202A && r <= 0x202E && r != 0x202C:
			flush()
			stack = append(stack, false)
		case r >= 0x2066 && r <= 0x2068:
			flush()
			isolates = append(isolates, len(stack))
			stack = append(stack, true)
		case r == 0x202C:
			// PDF closes the innermost embedding, but not past an isolate.
			flush()
			if n := len(stack); n > 0 && !stack[n-1] {
				stack = stack[:n-1]
			}
		case r == 0x2069:
			// PDI closes the innermost isolate and everything inside it;
			// with no isolate open it does nothing.
			flush()
			if n := len(isolates); n > 0 {
				stack, isolates = stack[:isolates[n-1]], isolates[:n-1]
			}
		case r == '\n':
			// A paragraph ends every embedding.
			stack, isolates = stack[:0], isolates[:0]
			cur.WriteRune(r)
		case r == 0x2029 || r == 0x85:
			// So does a paragraph separator or NEL, which is also a
			// break, as above.
			flush()
			stack, isolates = stack[:0], isolates[:0]
		case len(stack) > 0:
			// Inside an embedding: not checkable.
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return segs
}

// FindIn reports whether quote lies within one of the segments.
func FindIn(segments []string, quote string) bool {
	q := Normalise(quote)
	if q == "" {
		return false
	}
	for _, s := range segments {
		if strings.Contains(s, q) {
			return true
		}
	}
	return false
}

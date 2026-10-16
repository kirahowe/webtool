package text

import (
	"math"
	"slices"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// check exists to catch a research agent that invents or misquotes a
// citation. Against a page author who hides text from people it is best
// effort: without a rendering engine it cannot be complete, and
// stylesheets, scripts and overlays are known limits. relay never trusts
// free text because check found it; only typed fields can be cleared, and
// those are documented as open to outside influence.
//
// So check fails closed. It matches quotes only in text it can vouch for:
// text inside a fixed set of plain text elements whose inline styles use
// only a short list of properties with plain, bounded values, on a page
// with nothing that can leave the flow or paint over other content. Text
// anywhere else is still read for the agent (under the hiding rules in
// css.go) but never matched, and the visible text breaks around it. A
// layout trick outside these rules makes text unchecked rather than earn a
// rule of its own.

// checkableElements hold text as it is written. Elements that move their
// content (summary, legend, caption, table header and footer), shrink it
// (small, sub, sup), or draw something else are left out.
var checkableElements = map[atom.Atom]bool{
	atom.Html: true, atom.Body: true, atom.Div: true, atom.P: true, atom.Span: true, atom.A: true,
	atom.B: true, atom.Strong: true, atom.I: true, atom.Em: true, atom.U: true, atom.S: true,
	atom.Code: true, atom.Kbd: true, atom.Samp: true, atom.Var: true, atom.Mark: true, atom.Cite: true,
	atom.Abbr: true, atom.Time: true, atom.Data: true, atom.Dfn: true, atom.Ins: true, atom.Del: true,
	atom.Br: true, atom.Wbr: true, atom.H1: true, atom.H2: true, atom.H3: true, atom.H4: true,
	atom.H5: true, atom.H6: true, atom.Ul: true, atom.Ol: true, atom.Li: true, atom.Dl: true,
	atom.Dt: true, atom.Dd: true, atom.Blockquote: true, atom.Figure: true, atom.Figcaption: true,
	atom.Section: true, atom.Article: true, atom.Header: true, atom.Footer: true, atom.Main: true,
	atom.Nav: true, atom.Aside: true, atom.Address: true, atom.Hr: true, atom.Pre: true,
	atom.Table: true, atom.Tbody: true, atom.Tr: true, atom.Td: true, atom.Th: true, atom.Center: true,
	atom.Label: true,
}

// maxPlainPx bounds lengths in a plain style or attribute, and
// maxPlainFontPx font sizes: a box 9999999px wide puts its text where no
// one reads it.
const (
	maxPlainPx     = 2000
	maxPlainFontPx = 200
)

// sizeAttrs are presentation attributes that set a length.
var sizeAttrs = []string{"border", "cellpadding", "cellspacing", "width", "height", "hspace", "vspace"}

// checkable reports whether an element's own markup leaves its text as
// written: a checkable element, no direction but ltr, and an inline style
// check can vouch for.
func checkable(n *html.Node, style string, hasStyle bool) bool {
	if n.Namespace != "" || !checkableElements[n.DataAtom] {
		return false
	}
	if d, ok := attr(n, "dir"); ok && !strings.EqualFold(strings.TrimSpace(d), "ltr") {
		return false
	}
	for _, a := range n.Attr {
		switch a.Key {
		case "popover", "hidden", "inert", "bgcolor", "color", "size", "face", "text", "link", "vlink", "alink",
			"align", "start", "reversed", "value", "type":
			// align floats a table out of the flow; start, reversed, value
			// and type renumber a list, whose markers are not text.
			return false
		}
		if slices.Contains(sizeAttrs, a.Key) {
			if f, ok := attrNumber(a.Val); !ok || f > maxPlainPx {
				return false
			}
		}
	}
	if !hasStyle {
		return true
	}
	if len(style) > maxStyleBytes {
		return false
	}
	decls, ok := parseDeclarations(style)
	if !ok {
		return false
	}
	block := blocks[n.DataAtom] > 0
	for _, d := range decls {
		if strings.HasPrefix(d.prop, "--") {
			continue // used only through var(), which is never plain
		}
		if !plainDeclaration(d.prop, d.value, block) {
			return false
		}
	}
	return true
}

// attrNumber reads a presentation attribute's leading number.
func attrNumber(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	end := 0
	for end < len(s) && (s[end] >= '0' && s[end] <= '9' || s[end] == '.') {
		end++
	}
	if end == 0 {
		return 0, false
	}
	l := &lexer{s: s[:end]}
	return l.number(), true
}

// bounded reports a length no larger than maxPlainPx, taking em at 16px.
func bounded(t token) bool {
	if t.kind == tPercent || t.kind == tNumber {
		return t.num <= 100 || t.kind == tNumber && t.num == 0
	}
	p, ok := px(t)
	return ok && p <= maxPlainPx
}

// plainDeclaration reports a declaration that cannot hide, shrink, move or
// reorder text.
func plainDeclaration(prop string, v []token, block bool) bool {
	if len(v) == 0 || hasVar(v) {
		return false
	}
	idents := func(set ...string) bool { return allIdents(v, set) }
	lengths := func(negative bool) bool {
		for _, t := range v {
			if !(t.kind == tNumber && t.num == 0 || t.kind == tDimension && absolute(t.text) || t.kind == tPercent ||
				t.kind == tDimension && (t.text == "em" || t.text == "rem")) || !negative && t.num < 0 || !bounded(t) {
				return false
			}
		}
		return len(v) <= 4
	}
	switch prop {
	case "color", "background-color":
		return len(v) == 1 && opaqueColour(v[0])
	case "font-weight":
		return len(v) == 1 && (v[0].kind == tNumber && v[0].num >= 100 || isIdent(v, "normal", "bold", "bolder", "lighter"))
	case "font-style":
		return idents("normal", "italic", "oblique")
	case "font-variant", "font-variant-caps":
		return idents("normal", "small-caps")
	// font-family is left out: a symbol font such as Wingdings draws
	// letters as other glyphs.
	case "font-size":
		// Only sizes that do not compound: an absolute size of 8px or
		// more, or a keyword from small up.
		if len(v) != 1 {
			return false
		}
		if v[0].kind == tDimension && absolute(v[0].text) {
			p, _ := px(v[0])
			return p >= 8 && p <= maxPlainFontPx
		}
		return isIdent(v, "small", "medium", "large", "x-large", "xx-large", "xxx-large")
	case "line-height":
		// A percentage or length is taken from the parent's font size, so
		// only plain numbers in a sane range are plain.
		return len(v) == 1 && (isIdent(v, "normal") || v[0].kind == tNumber && v[0].num >= 1 && v[0].num <= 3)
	case "text-align":
		return idents("left", "right", "center", "justify", "start", "end")
	case "vertical-align":
		return idents("baseline", "top", "middle", "bottom", "text-top", "text-bottom")
	case "text-decoration", "text-decoration-line", "text-decoration-style":
		return idents("none", "underline", "overline", "line-through", "solid", "double", "dotted", "dashed", "wavy")
	case "text-transform":
		return idents("none", "uppercase", "lowercase", "capitalize")
	case "white-space":
		return idents("normal", "nowrap", "pre", "pre-wrap", "pre-line", "break-spaces")
	case "display":
		if block {
			// list-item draws a marker, which is not text.
			return idents("block")
		}
		return idents("inline")
	case "padding", "padding-left", "padding-right", "padding-top", "padding-bottom",
		"border-width", "border-radius", "margin", "margin-left", "margin-right", "margin-top", "margin-bottom":
		// A margin, padding or border on an inline element moves its
		// words apart; on a block it only moves the block, which already
		// starts a line.
		if !block {
			for _, t := range v {
				if !(t.numeric() && t.num == 0) {
					return false
				}
			}
		}
		return lengths(false)
	case "min-height":
		return block && (lengths(false) || idents("auto"))
	case "border", "border-top", "border-bottom", "border-left", "border-right", "border-style", "border-color":
		if !block {
			return false
		}
		for _, t := range v {
			if !(t.kind == tIdent && !slices.Contains(systemColours, t.text) || t.kind == tHash ||
				length(t, false) && t.kind != tFunction && bounded(t)) {
				return false
			}
		}
		return true
	case "list-style", "list-style-type", "list-style-position":
		return idents("none", "disc", "circle", "square", "decimal", "lower-alpha", "upper-alpha",
			"lower-roman", "upper-roman", "inside", "outside")
	case "cursor":
		return len(v) == 1 && v[0].kind == tIdent
	}
	return false
}

// opaqueColour is a named colour, a hex colour without alpha, or rgb() or
// hsl() with three plain components or an alpha of exactly 1.
func opaqueColour(t token) bool {
	switch t.kind {
	case tIdent:
		// System colours follow the viewer's theme, and canvas is the
		// page's own background, so they are not plain.
		return slices.Contains(namedColors, t.text) && t.text != "transparent" && !slices.Contains(systemColours, t.text)
	case tHash:
		return (len(t.text) == 3 || len(t.text) == 6) && strings.Trim(t.text, "0123456789abcdef") == ""
	case tFunction:
		if !slices.Contains([]string{"rgb", "rgba", "hsl", "hsla"}, t.text) {
			return false
		}
		args, ok := colourArgs(t.args)
		if !ok {
			return false
		}
		for i, a := range args {
			hue := i == 0 && strings.HasPrefix(t.text, "hsl")
			if i < 3 && !(a.kind == tNumber || a.kind == tPercent || hue && a.kind == tDimension && slices.Contains(angleUnits, a.text)) {
				return false
			}
		}
		return len(args) == 3 || args[3].kind == tNumber && args[3].num == 1 || args[3].kind == tPercent && args[3].num == 100
	}
	return false
}

var systemColours = []string{"canvas", "canvastext", "linktext", "visitedtext", "activetext", "buttonface",
	"buttontext", "buttonborder", "field", "fieldtext", "highlight", "highlighttext", "selecteditem",
	"selecteditemtext", "mark", "marktext", "graytext", "accentcolor", "accentcolortext"}

// maxRefreshSeconds: a page that refreshes to another this soon is not
// the page a person reads.
const maxRefreshSeconds = 5

// RefreshesSoon reports a Refresh header or meta refresh value whose delay
// is maxRefreshSeconds or less.
func RefreshesSoon(content string) bool {
	delay, _, _ := strings.Cut(strings.TrimSpace(content), ";")
	delay, _, _ = strings.Cut(delay, ",")
	f, ok := attrNumber(delay)
	// Browsers read the delay's whole seconds only: 5.9 is 5.
	return ok && math.Floor(f) <= maxRefreshSeconds || strings.TrimSpace(delay) == ""
}

// pageUncheckable says why no text on a page can be vouched for, or "".
// Anything that can leave the flow or paint outside its box could cover
// any text, so one such element anywhere makes the whole page unchecked:
// a position other than static, a float, a transform, a negative margin, a
// z-index, an opacity below 1, or an SVG or replaced element whose
// overflow is visible. So does a refresh to another page within seconds.
//
// It walks the parsed tree, not the tokens: only the tree builder knows
// which text is raw (a <style> in HTML) and which is markup (a <style> in
// SVG, after which a <div> breaks back out into HTML), and a scan of
// tokens that guessed wrong would miss what the browser draws. A page too
// long to parse whole is unchecked instead (see HTML).
func pageUncheckable(doc *html.Node) string {
	reason := ""
	walkNodes(doc, func(n *html.Node) bool { return reason != "" }, func(n *html.Node) {
		if n.Type != html.ElementNode {
			return
		}
		// An open dialog is positioned over the page by the browser's
		// own stylesheet.
		if n.DataAtom == atom.Dialog && n.Namespace == "" {
			if _, open := attr(n, "open"); open {
				reason = "an open dialog is drawn over the page"
				return
			}
		}
		// MathML can pad a box over its neighbours and paint it.
		if n.Namespace == "math" {
			_, bg := attr(n, "mathbackground")
			if bg || n.Data == "mpadded" {
				reason = "MathML can pad or paint over other text"
				return
			}
		}
		if n.DataAtom == atom.Meta && n.Namespace == "" {
			if v, ok := attr(n, "http-equiv"); ok && strings.EqualFold(strings.TrimSpace(v), "refresh") {
				if c, ok := attr(n, "content"); ok && RefreshesSoon(c) {
					reason = "the page refreshes to another within seconds"
				}
			}
			return
		}
		paints := n.Namespace != "" || replaced[n.DataAtom]
		if v, ok := attr(n, "overflow"); ok && paints && strings.EqualFold(strings.TrimSpace(v), "visible") {
			reason = "an element paints outside its box"
			return
		}
		style, ok := attr(n, "style")
		if !ok {
			return
		}
		if len(style) > maxStyleBytes {
			reason = "an inline style is too long to read"
			return
		}
		decls, ok := parseDeclarations(style)
		if !ok {
			reason = "an inline style nests too deeply to read"
			return
		}
		// var() is not resolved here: an unresolved one is unknown, and
		// an unknown position, float or opacity counts as one that
		// overlays.
		budget := maxVarTokens
		for i := range decls {
			decls[i].value = substituteBudget(decls[i].value, nil, 0, &budget)
		}
		if overlays(cascade(decls), paints) {
			reason = "an element can leave the flow or paint over other content"
		}
	})
	return reason
}

// overlays reports a style that can take an element out of the flow or
// paint it over other content.
func overlays(d map[string][]token, paints bool) bool {
	set := func(p string, except ...string) bool {
		v, ok := d[p]
		return ok && !isIdent(v, except...)
	}
	if set("position", "static") || set("float", "none") || set("z-index", "auto") || transformed(d) {
		return true
	}
	// A motion path moves the element anywhere.
	for _, p := range []string{"offset", "offset-path", "offset-distance", "offset-position", "offset-anchor", "offset-rotate"} {
		if _, ok := d[p]; ok {
			return true
		}
	}
	if v, ok := d["opacity"]; ok && !(len(v) == 1 && v[0].numeric() && fraction(v[0]) >= 1) {
		return true
	}
	for _, p := range []string{"margin", "margin-left", "margin-top", "margin-right", "margin-bottom",
		"margin-inline", "margin-inline-start", "margin-inline-end", "margin-block", "margin-block-start", "margin-block-end"} {
		for _, t := range d[p] {
			if t.kind == tUnknown || t.numeric() && t.num < 0 || t.kind == tFunction {
				return true
			}
		}
	}
	if paints {
		for _, p := range []string{"overflow-x", "overflow-y"} {
			if isIdent(d[p], "visible") || unknown(d[p]) {
				return true
			}
		}
	}
	return false
}

// colsStyled reports a table whose columns are styled or sized: a column
// can collapse or narrow its cells out of sight.
func colsStyled(table *html.Node) bool {
	styled := false
	var look func(n *html.Node)
	look = func(n *html.Node) {
		for c := n.FirstChild; c != nil && !styled; c = c.NextSibling {
			if c.Type != html.ElementNode {
				continue
			}
			switch c.DataAtom {
			case atom.Col, atom.Colgroup:
				for _, a := range c.Attr {
					if a.Key == "style" || a.Key == "width" || a.Key == "span" {
						styled = true
					}
				}
				if c.DataAtom == atom.Colgroup {
					look(c)
				}
			}
		}
	}
	look(table)
	return styled
}

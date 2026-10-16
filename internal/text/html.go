package text

import (
	"bytes"
	"context"
	"net/url"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// Break is written into Page.Visible wherever something was hidden,
// replaced (an image, a form control, a frame), moved out of its place or
// drawn in another order. A quote is only found between two breaks, so
// none of those can join the words around them into a quote.
const Break = '\x00'

// Page is the readable text of a fetched document.
type Page struct {
	Title    string // at most MaxTitleRunes
	TitleCut bool   // the title was longer
	// Text is what the agent reads: the visible text, not yet escaped,
	// with notes added (link targets, image descriptions, list and
	// heading marks).
	Text string
	// TextRunes is Text's full length when Compact has cut it, else 0.
	TextRunes int
	// Visible is the visible text alone, without the notes, with a Break
	// wherever the text a person reads may not run on. check looks for
	// quotes in it.
	Visible string
	// Hidden counts the characters of text in elements a person would
	// not see, which were dropped.
	Hidden int
	// Cut and VisibleCut say that Text or Visible reached the extraction
	// limit; the rest of the document is not in it.
	Cut, VisibleCut bool
	// Deep is set when the document nests elements too deeply to be
	// read: more than the HTML parser's limit of 512, or MaxDepth in the
	// tree it builds. What lies deeper is dropped.
	Deep bool
	// TooManyTokens says the document had more than MaxTokens and was
	// read only that far.
	TooManyTokens bool
	// Uncheckable, when set, says why no quote can be checked on the page
	// at all (see checkable.go).
	Uncheckable string
}

// Limits on extraction, so a small document cannot make a large text.
const (
	MaxTitleRunes = 1000
	MaxDepth      = 1024
	// MaxTokens bounds the tags, text runs and comments parsed. The HTML
	// parser joins adjacent text in a way that costs the square of their
	// number, so a document past this is read only that far.
	MaxTokens   = 400_000
	maxURLBytes = 2048 // longer link targets and <base> values are ignored
)

// Plain is a text/plain document as a Page, cut at limit runes.
func Plain(body []byte, limit int) Page {
	// CR LF, a lone CR and a form feed each end a line.
	s := strings.NewReplacer("\r\n", "\n", "\r", "\n", "\f", "\n").Replace(string(body))
	p := Page{}
	if utf8.RuneCountInString(s) > limit {
		s, p.Cut, p.VisibleCut = string([]rune(s)[:limit]), true, true
	}
	p.Text, p.Visible = s, strings.ReplaceAll(s, string(Break), "")
	return p
}

// silent elements are never drawn and hold nothing a person reads, so
// they are dropped without a break and without counting as hidden.
var silent = map[atom.Atom]bool{
	atom.Script: true, atom.Style: true, atom.Noscript: true, atom.Template: true, atom.Head: true,
	atom.Title: true, atom.Datalist: true, atom.Noembed: true, atom.Noframes: true, atom.Param: true,
	atom.Source: true, atom.Track: true, atom.Link: true, atom.Meta: true, atom.Base: true,
}

// replaced elements show something other than text written in the page:
// an image, a control and its value, a frame, a drawing, a formula. The
// agent gets an image's description; the visible text gets a break.
var replaced = map[atom.Atom]bool{
	atom.Img: true, atom.Input: true, atom.Select: true,
	atom.Iframe: true, atom.Object: true, atom.Embed: true, atom.Canvas: true, atom.Video: true,
	atom.Audio: true, atom.Picture: true, atom.Svg: true, atom.Math: true, atom.Meter: true,
	atom.Progress: true, atom.Frame: true, atom.Frameset: true, atom.Applet: true, atom.Area: true,
	atom.Map: true,
}

// boxed elements hold page text in a box of their own: a button's label,
// a text area's contents. Their text is read, with a break on each side.
var boxed = map[atom.Atom]bool{atom.Button: true, atom.Textarea: true, atom.Output: true}

// blocks start on a line of their own; paragraphs also leave a blank line.
var blocks = map[atom.Atom]int{
	atom.Address: 1, atom.Article: 1, atom.Aside: 1, atom.Body: 1, atom.Dd: 1,
	atom.Details: 1, atom.Dialog: 1, atom.Div: 1, atom.Dt: 1, atom.Fieldset: 1,
	atom.Figcaption: 1, atom.Footer: 1, atom.Form: 1, atom.Header: 1, atom.Hgroup: 1,
	atom.Li: 1, atom.Main: 1, atom.Nav: 1, atom.Section: 1, atom.Summary: 1,
	atom.Tr: 1, atom.Caption: 1, atom.Legend: 1, atom.Tbody: 1, atom.Thead: 1, atom.Tfoot: 1,
	atom.P: 2, atom.H1: 2, atom.H2: 2, atom.H3: 2, atom.H4: 2, atom.H5: 2, atom.H6: 2,
	atom.Pre: 2, atom.Blockquote: 2, atom.Ul: 2, atom.Ol: 2, atom.Dl: 2, atom.Table: 2,
	atom.Figure: 2, atom.Hr: 2, atom.Center: 1,
}

var headings = map[atom.Atom]string{
	atom.H1: "# ", atom.H2: "## ", atom.H3: "### ", atom.H4: "#### ", atom.H5: "##### ", atom.H6: "###### ",
}

// HTML extracts the visible text of an HTML document, already decoded to
// UTF-8, stopping once the text reaches limit runes. Links keep their
// target, made absolute against base (or the document's own <base>), as
// "text <url>", so the agent can follow them.
//
// Extraction stops early, with what it has, once ctx is done.
func HTML(ctx context.Context, body []byte, base *url.URL, limit int) Page {
	body, tooMany := withinTokens(body, MaxTokens)
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		// Reading a byte slice cannot fail, so this is the parser's own
		// limit: it refuses elements nested more than 512 deep.
		return Page{Deep: true}
	}
	e := &extractor{ctx: ctx, w: writer{limit: limit}, v: writer{limit: limit}}
	e.setBase(base)
	e.findHead(doc)
	e.walk(doc, 0, inherited{})
	title := []rune(e.title)
	p := Page{
		Text: e.w.String(), Visible: e.v.String(),
		Hidden: e.hidden, Cut: e.w.full, VisibleCut: e.v.full, Deep: e.deep,
	}
	if len(title) > MaxTitleRunes {
		title, p.TitleCut = title[:MaxTitleRunes], true
	}
	p.Title = string(title)
	uncheckable := pageUncheckable(doc)
	if tooMany {
		// What lies past the cut could cover what was read.
		p.TooManyTokens = true
		uncheckable = "the page is too long to check"
	}
	// Nothing on an uncheckable page is vouched for, so its visible
	// text is empty as well as flagged.
	if p.Uncheckable = uncheckable; p.Uncheckable != "" {
		p.Visible = ""
	}
	return p
}

// withinTokens cuts a document after its first max tokens, at a token's
// end, and reports whether it did.
//
// It counts "<" rather than tokens. Every token but text starts with one,
// and text lies between them, so a document with n of them has at most
// 2n+1 tokens however the tree builder reads it: a tokenizer would have
// to guess which text is raw (a <style> in HTML) and which is markup (a
// <style> in SVG), and a wrong guess would let a document past the cap.
// Counting "<" in raw text as well only cuts sooner.
func withinTokens(body []byte, max int) ([]byte, bool) {
	at := 0
	for n := 0; n < max/2; n++ {
		i := bytes.IndexByte(body[at:], '<')
		if i < 0 {
			return body, false
		}
		at += i + 1
	}
	if i := bytes.IndexByte(body[at:], '<'); i >= 0 {
		return body[:at+i], true
	}
	return body, false
}

// inherited is what an element passes down to what is inside it.
type inherited struct {
	invisible   bool // visibility hidden
	transparent bool // colour transparent
	fill        int  // -webkit-text-fill-color: 0 unset, 1 opaque, -1 transparent
	// unchecked is set inside an element whose characters are drawn in
	// another order (a bidi override): the agent reads its text, but it
	// is not text a quote can be checked against.
	unchecked bool
	rtl       bool      // direction is right to left
	heading   bool      // inside a heading
	vars      *varScope // custom properties in force
	opacity   float64   // the product of the opacities above, 0 meaning 1
	fontPx    float64   // the font size in px, 0 meaning 16
}

func (inh inherited) hidden() bool {
	return inh.invisible || inh.fill < 0 || inh.fill == 0 && inh.transparent
}

type extractor struct {
	ctx       context.Context
	nodes     int // visited, for checking ctx now and then
	stopped   bool
	base      *url.URL
	here      string // base without its fragment, to spot links to this page
	title     string
	titleSeen bool
	hidden    int
	deep      bool
	openNamed map[string]bool // named <details> groups that have an open member
	w         writer          // what the agent reads
	v         writer          // the visible text alone, with breaks
}

func (e *extractor) setBase(u *url.URL) {
	if u == nil {
		return
	}
	here := *u
	here.Fragment, here.RawFragment = "", ""
	e.base, e.here = u, here.String()
}

// full is when there is nothing more worth reading: both texts are at
// their limit, or time is up. Each writer has its own budget, so notes
// for the agent never crowd out the visible text quotes are checked in.
func (e *extractor) full() bool { return e.stopped || e.w.full && e.v.full }

// text is page text, which both writers get, or only the agent's when
// it cannot be checked.
func (e *extractor) text(s string, inh inherited) {
	e.w.text(s)
	if inh.unchecked {
		e.v.brk()
	} else {
		e.v.text(s)
	}
}

// note is text webtool adds for the agent, which is not on the page. It
// leaves the visible text as it was, so "no<a>where</a>" still reads
// "nowhere" there. Nothing is noted inside hidden text.
func (e *extractor) note(s string, inh inherited) {
	if !inh.hidden() {
		e.w.text(s)
	}
}

// cell separates table cells: a mark for the agent, a space between the
// cells' words in the visible text.
func (e *extractor) cell(inh inherited) {
	e.note(" | ", inh)
	e.v.brk()
}

func (e *extractor) newline(n int) {
	e.w.newline(n)
	e.v.newline(n)
}

// drop counts the text of an element a person does not see, and breaks
// the visible text there.
func (e *extractor) drop(n *html.Node) {
	e.hidden += utf8.RuneCountInString(collapse(visibleTextOf(n)))
	e.v.brk()
}

// inert elements are not part of the rendered document: template
// contents, scripts, foreign content.
func inert(n *html.Node) bool {
	if n.Type != html.ElementNode {
		return false
	}
	if n.Namespace != "" {
		return true
	}
	switch n.DataAtom {
	case atom.Template, atom.Script, atom.Style, atom.Noscript, atom.Noembed, atom.Noframes:
		return true
	}
	return replaced[n.DataAtom]
}

// findHead takes the first title and the first <base href> in the
// document, outside inert elements, as a browser does.
func (e *extractor) findHead(doc *html.Node) {
	baseSet := false
	walkNodes(doc, inert, func(n *html.Node) {
		if n.Type != html.ElementNode {
			return
		}
		switch n.DataAtom {
		case atom.Title:
			// The first title is the document's, even when it is empty.
			if !e.titleSeen {
				e.title, e.titleSeen = collapse(textOf(n)), true
			}
		case atom.Base:
			href, ok := attr(n, "href")
			if !ok || baseSet || e.base == nil || len(href) > maxURLBytes {
				return
			}
			baseSet = true
			if u, err := e.base.Parse(strings.TrimSpace(href)); err == nil && (u.Scheme == "http" || u.Scheme == "https") {
				e.setBase(u)
			}
		}
	})
}

func (e *extractor) walk(n *html.Node, depth int, inh inherited) {
	// Time is checked every 256 nodes, and at every styled element,
	// since one style can cost more than many plain nodes.
	if e.nodes++; (e.nodes%256 == 0 || hasStyleAttr(n)) && e.ctx.Err() != nil {
		e.stopped = true
	}
	if e.full() {
		return
	}
	switch n.Type {
	case html.TextNode:
		if inh.hidden() {
			if t := collapse(n.Data); t != "" {
				e.hidden += utf8.RuneCountInString(t)
				e.v.brk()
			} else if n.Data != "" {
				// An invisible space still takes its room on the line.
				e.w.space, e.v.space = true, true
			}
			return
		}
		e.text(n.Data, inh)
		return
	case html.DocumentNode:
		e.children(n, depth, inh, false)
		return
	case html.ElementNode:
	default:
		return
	}
	if depth >= MaxDepth {
		e.deep = true
		e.v.brk()
		return
	}
	if n.Namespace != "" {
		e.v.brk()
		return
	}
	if silent[n.DataAtom] {
		// Its text is not read, but a style could show it, so no quote
		// runs across it.
		e.v.brk()
		return
	}
	prune, eff, next := hiddenBy(n, inh)
	if prune {
		e.drop(n)
		return
	}
	inh = next
	if eff.override || n.DataAtom == atom.Bdo {
		inh.unchecked = true
	}
	_, isHeading := headings[n.DataAtom]
	nestedHeading := isHeading && inh.heading
	inh.heading = inh.heading || isHeading
	// A heading inside a heading compounds its font size, and a table
	// whose columns are styled can collapse its cells.
	if style, hasStyle := attr(n, "style"); !inh.unchecked && (!checkable(n, style, hasStyle) || nestedHeading ||
		n.DataAtom == atom.Table && colsStyled(n)) {
		// Read for the agent, never matched (see checkable.go).
		inh.unchecked = true
		e.v.brk()
		defer e.v.brk()
	}
	// An element with a declarative shadow root draws the shadow tree,
	// not its own children, so what is written in it is not what shows.
	if replaced[n.DataAtom] || shadowHost(n) {
		if alt, ok := attr(n, "alt"); ok && n.DataAtom == atom.Img && strings.TrimSpace(alt) != "" {
			e.note("[image: "+collapse(alt)+"]", inh)
		}
		e.v.brk()
		return
	}
	if eff.moved || boxed[n.DataAtom] || drawnElsewhere[n.DataAtom] {
		e.v.brk()
		defer e.v.brk()
	}
	switch n.DataAtom {
	case atom.Br:
		e.newline(1)
		return
	case atom.Td, atom.Th:
		if hasCellBefore(n) {
			e.cell(inh)
		}
	case atom.Details:
		_, open := attr(n, "open")
		// In a named group only the first open member is open.
		if name, ok := attr(n, "name"); ok && name != "" && open {
			if e.openNamed[name] {
				open = false
			} else {
				if e.openNamed == nil {
					e.openNamed = map[string]bool{}
				}
				e.openNamed[name] = true
			}
		}
		if !open {
			e.closedDetails(n, depth, inh)
			return
		}
	}
	block := blockLevel(n, eff)
	if block > 0 {
		e.newline(block)
	}
	if p, ok := headings[n.DataAtom]; ok {
		e.note(p, inh)
	}
	if n.DataAtom == atom.Li {
		// The marker is drawn, but is not text: no quote runs across it.
		e.v.brk()
		e.note("- ", inh)
	}
	if n.DataAtom == atom.Pre {
		e.w.pre++
		e.v.pre++
	}
	start := e.w.written
	// Right to left, a flex row, a grid or a table row is laid out from
	// the right, so the children may not read in the order written.
	e.children(n, depth, inh, eff.reorders || inh.rtl)
	if n.DataAtom == atom.Pre {
		e.w.pre--
		e.v.pre--
	}
	if n.DataAtom == atom.A {
		if target := e.link(n); target != "" {
			if e.w.written == start {
				e.note("<"+target+">", inh)
			} else {
				e.note(" <"+target+">", inh)
			}
		}
	}
	if block > 0 {
		e.newline(block)
	}
}

// drawnElsewhere elements are drawn away from where they are written: a
// table's header and footer at its top and bottom, a caption on its
// edge, ruby text above its base.
var drawnElsewhere = map[atom.Atom]bool{atom.Thead: true, atom.Tfoot: true, atom.Caption: true, atom.Rt: true}

// blockLevel is how many newlines an element starts and ends with: its
// own by default, or what its inline display says.
func blockLevel(n *html.Node, eff effect) int {
	switch eff.display {
	case "", "inherit", "revert", "revert-layer":
		return blocks[n.DataAtom]
	case "initial", "unset":
		return 0
	case "inline", "inline-block", "inline-flex", "inline-grid", "inline-table", "contents", "ruby", "-webkit-inline-box":
		return 0
	}
	return max(blocks[n.DataAtom], 1)
}

// shadowHost reports an element with a template that attaches a shadow
// root.
func shadowHost(n *html.Node) bool {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode && c.DataAtom == atom.Template {
			if _, ok := attr(c, "shadowrootmode"); ok {
				return true
			}
		}
	}
	return false
}

// closedDetails shows only the summary of a <details> that is not open,
// as a browser does, and counts the rest as hidden.
func (e *extractor) closedDetails(n *html.Node, depth int, inh inherited) {
	var summary *html.Node
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode && c.DataAtom == atom.Summary {
			summary = c
			break
		}
	}
	e.newline(1)
	if summary != nil {
		e.walk(summary, depth+1, inh)
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c != summary && (c.Type == html.TextNode || c.Type == html.ElementNode && !silent[c.DataAtom]) {
			e.hidden += utf8.RuneCountInString(collapse(visibleTextOf(c)))
		}
	}
	e.v.brk()
	e.newline(1)
}

// children walks n's children; when n draws them in another order than
// written, the visible text breaks between each.
func (e *extractor) children(n *html.Node, depth int, inh inherited, reordered bool) {
	for c := n.FirstChild; c != nil && !e.full(); c = c.NextSibling {
		if reordered {
			e.v.brk()
		}
		e.walk(c, depth+1, inh)
	}
	if reordered {
		e.v.brk()
	}
}

// link is an anchor's absolute http or https target, or "" for none, for
// another scheme, for a target too long to be worth showing, and for a
// link to elsewhere on the same page.
func (e *extractor) link(n *html.Node) string {
	href, ok := attr(n, "href")
	if !ok || e.base == nil || len(href) > maxURLBytes {
		return ""
	}
	u, err := e.base.Parse(strings.TrimSpace(href))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	s := u.String()
	if len(s) > maxURLBytes {
		return ""
	}
	same := *u
	same.Fragment, same.RawFragment = "", ""
	if same.String() == e.here {
		return ""
	}
	return s
}

// hasCellBefore reports whether a table cell follows another in its row.
func hasCellBefore(n *html.Node) bool {
	for p := n.PrevSibling; p != nil; p = p.PrevSibling {
		if p.Type == html.ElementNode {
			return p.DataAtom == atom.Td || p.DataAtom == atom.Th
		}
	}
	return false
}

// Hidden reports whether an element is hidden from a person by its own
// markup, with everything in it: see hiddenBy.
func Hidden(n *html.Node) bool {
	prune, _, inh := hiddenBy(n, inherited{})
	return prune || inh.hidden()
}

// hiddenBy says whether an element is hidden with everything in it, what
// its style does, and what it passes down to its children otherwise. The
// element is hidden by the hidden attribute (unless its own style sets
// display), by aria-hidden="true", as a dialog that is not open, as a
// hidden input, or by an inline style that hides it (see HiddenStyle).
// Visibility and colour are inherited, and a child can set them back, so
// they are passed down rather than drop the subtree. Stylesheets and
// classes are not evaluated.
func hiddenBy(n *html.Node, inh inherited) (bool, effect, inherited) {
	style, hasStyle := attr(n, "style")
	eff := effect{vars: inh.vars}
	if hasStyle {
		eff = styleEffect(style, inh.vars)
	}
	if _, ok := attr(n, "hidden"); ok && !(hasStyle && setsDisplay(style, inh.vars)) {
		return true, eff, inh
	}
	if v, ok := attr(n, "aria-hidden"); ok && strings.EqualFold(strings.TrimSpace(v), "true") {
		return true, eff, inh
	}
	if _, open := attr(n, "open"); n.DataAtom == atom.Dialog && !open {
		return true, eff, inh
	}
	// A popover is hidden until a script or a button shows it, and <rp>
	// is hidden wherever ruby is drawn.
	if _, ok := attr(n, "popover"); ok || n.DataAtom == atom.Rp {
		return true, eff, inh
	}
	if v, ok := attr(n, "type"); ok && n.DataAtom == atom.Input && strings.EqualFold(strings.TrimSpace(v), "hidden") {
		return true, eff, inh
	}
	if eff.prune {
		return true, eff, inh
	}
	// An element of another namespace, in XHTML, may be an HTML element
	// such as h:style that this parser does not know.
	if strings.Contains(n.Data, ":") {
		return true, eff, inh
	}
	// Opacity and font size compound down the tree.
	op := inh.opacity
	if op == 0 {
		op = 1
	}
	if op *= eff.opacity; hasStyle && op < 0.1 {
		return true, eff, inh
	}
	if hasStyle {
		inh.opacity = op
	}
	size := inh.fontPx
	if size == 0 {
		size = 16
	}
	switch {
	case eff.fontPx > 0:
		size = eff.fontPx
	case eff.fontScale > 0:
		size *= eff.fontScale
	case n.DataAtom == atom.Small || n.DataAtom == atom.Sub || n.DataAtom == atom.Sup:
		size *= 0.83
	}
	if size < 2 {
		return true, eff, inh
	}
	inh.fontPx = size
	inh.vars = eff.vars
	if d, ok := attr(n, "dir"); ok {
		switch strings.ToLower(strings.TrimSpace(d)) {
		case "rtl", "auto":
			inh.rtl = true
		case "ltr":
			inh.rtl = false
		}
	}
	switch eff.rtl {
	case 1:
		inh.rtl = true
	case -1:
		inh.rtl = false
	}
	switch eff.visible {
	case -1:
		inh.invisible = true
	case 1:
		inh.invisible = false
	}
	switch eff.opaque {
	case -1:
		inh.transparent = true
	case 1:
		inh.transparent = false
	}
	switch eff.fill {
	case colourReset:
		inh.fill = 0
	case 1, -1:
		inh.fill = eff.fill
	}
	return false, eff, inh
}

// setsDisplay reports whether a style gives display a valid value other
// than none, which overrides the hidden attribute.
func setsDisplay(style string, vars *varScope) bool {
	if len(style) > maxStyleBytes {
		return false
	}
	decls, ok := parseDeclarations(style)
	if !ok {
		return false
	}
	if vars, ok = customProperties(decls, vars); !ok {
		return false
	}
	budget := maxVarTokens
	for i := range decls {
		decls[i].value = substituteBudget(decls[i].value, vars, 0, &budget)
	}
	v := cascade(decls)["display"]
	return valid("display", v) && !isIdent(v, "none") && !isIdent(v, globalKeywords...)
}

func hasStyleAttr(n *html.Node) bool {
	_, ok := attr(n, "style")
	return n.Type == html.ElementNode && ok
}

func attr(n *html.Node, key string) (string, bool) {
	for _, a := range n.Attr {
		if a.Namespace == "" && a.Key == key {
			return a.Val, true
		}
	}
	return "", false
}

// walkNodes visits n and the nodes under it in document order, without
// recursion, so nesting depth costs nothing. Nodes for which skip is true
// are not visited, nor is anything under them.
func walkNodes(n *html.Node, skip func(*html.Node) bool, visit func(*html.Node)) {
	stack := []*html.Node{n}
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if skip(n) {
			continue
		}
		visit(n)
		for c := n.LastChild; c != nil; c = c.PrevSibling {
			stack = append(stack, c)
		}
	}
}

func textUnder(n *html.Node, skip func(*html.Node) bool) string {
	var b strings.Builder
	walkNodes(n, skip, func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
			b.WriteByte(' ')
		}
	})
	return b.String()
}

// textOf is all the text under n.
func textOf(n *html.Node) string {
	return textUnder(n, func(*html.Node) bool { return false })
}

// visibleTextOf is the text under n that would show if n itself were not
// hidden: what was hidden from a person, not scripts and styles.
func visibleTextOf(n *html.Node) string {
	root := n
	return textUnder(n, func(c *html.Node) bool {
		return c != root && c.Type == html.ElementNode && silent[c.DataAtom]
	})
}

// htmlSpace is whitespace as HTML collapses it. Other spaces, such as a
// no-break space, show and are kept.
func htmlSpace(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\f'
}

func collapse(s string) string {
	return strings.Join(strings.FieldsFunc(s, htmlSpace), " ")
}

// writer builds the text, collapsing whitespace as a browser would and
// keeping at most one blank line between blocks.
type writer struct {
	b       strings.Builder
	space   bool // a space is owed before the next word
	breaks  int  // newlines owed before the next word
	pre     int  // inside <pre>, whitespace is kept
	written int  // runes written so far
	limit   int  // the most runes it will hold
	full    bool // it reached limit; everything since was dropped
}

func (w *writer) newline(n int) {
	if w.b.Len() == 0 {
		return
	}
	w.breaks = max(w.breaks, n)
}

func (w *writer) flush() {
	if w.full {
		return
	}
	if w.b.Len() > 0 {
		sep := ""
		if w.breaks > 0 {
			sep = strings.Repeat("\n", w.breaks)
		} else if w.space {
			sep = " "
		}
		if w.written+len(sep) >= w.limit && sep != "" {
			w.full = true
			return
		}
		w.b.WriteString(sep)
		w.written += len(sep)
	}
	w.breaks, w.space = 0, false
}

// word writes s, cut so the text never passes the limit.
func (w *writer) word(s string) {
	w.flush()
	if w.full {
		return
	}
	n := utf8.RuneCountInString(s)
	if room := w.limit - w.written; n > room {
		r := []rune(s)
		s, n, w.full = string(r[:max(room, 0)]), max(room, 0), true
	}
	w.b.WriteString(s)
	w.written += n
}

func (w *writer) text(s string) {
	if w.pre > 0 {
		lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
		for i, l := range lines {
			if i > 0 {
				w.flush()
				if w.written >= w.limit {
					w.full = true
				}
				if w.full {
					return
				}
				w.b.WriteByte('\n')
				w.written++
			}
			if l != "" {
				w.word(l)
			}
		}
		return
	}
	if s == "" {
		return
	}
	if htmlSpace(rune(s[0])) {
		w.space = true
	}
	words := strings.FieldsFunc(s, htmlSpace)
	for i, word := range words {
		if i > 0 {
			w.space = true
		}
		w.word(word)
	}
	if len(words) > 0 && htmlSpace(rune(s[len(s)-1])) {
		w.space = true
	}
}

func (w *writer) String() string { return w.b.String() }

// brk writes a Break, at most one in a row. Pending whitespace is dropped,
// since the break separates anyway.
func (w *writer) brk() {
	if w.full || w.b.Len() == 0 || strings.HasSuffix(w.b.String(), string(Break)) {
		return
	}
	if w.written+1 > w.limit {
		w.full = true
		return
	}
	w.b.WriteRune(Break)
	w.written++
	w.space, w.breaks = false, 0
}

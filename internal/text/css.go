package text

import (
	"slices"
	"strconv"
	"strings"
)

// These rules decide what the agent is not shown, and they lean toward
// hiding: when unsure, the agent loses some visible text rather than see
// text a person would not. A value they cannot read for a property that
// can hide (an unresolved var(), a function they do not know) hides.
//
// check leans the same way and more: the visible text it searches has a
// break wherever anything was hidden, replaced, moved or reordered, and a
// quote must lie between two breaks. So dropping a visible word, or
// moving one, cannot join the words around it into a quote.

// maxStyleBytes bounds an inline style. A longer one is not parsed, and
// hides its element.
const maxStyleBytes = 16 << 10

// effect is what an inline style does to its element and, through
// inheritance, to what is inside it.
type effect struct {
	prune bool // the element and everything in it are hidden
	// visible, opaque and fill say what the style sets for the inherited
	// visibility, colour and -webkit-text-fill-color: 0 when it leaves
	// them, 1 when it makes text visible or opaque, -1 when it hides it
	// or makes it transparent. A child can set them back.
	visible, opaque, fill int
	moved                 bool // drawn away from its place in the text: positioned, floated, ordered, pulled by a negative margin
	reorders              bool // its children are drawn in another order than written
	override              bool // its characters are drawn in another order or mirrored (a bidi override, a transform)
	// rtl is 1 when the style sets direction:rtl, -1 for ltr, 0 when it
	// leaves direction alone. It is inherited.
	rtl int
	// display is the display value the style sets, "" for none set.
	display string
	// opacity is the element's own opacity (1 when unset); fontPx an
	// absolute font size it sets, fontScale a relative one (0 when
	// unset). Both compound down the tree.
	opacity, fontPx, fontScale float64
	vars                       *varScope // custom properties in force inside it
}

// HiddenStyle reports whether an inline style alone hides its element
// from a person: display:none, visibility:hidden or collapse,
// content-visibility:hidden, an opacity below 0.1, a font size below 2px
// or 0.1em, a transparent colour or text fill, a zero scale on either
// axis, an absolutely positioned clip to nothing, a clip-path that
// removes the whole box, a zero height or width on an axis that clips, a
// text indent, margin or absolute position at least 999px off the page,
// or content replaced by an image.
//
// The style is tokenised as CSS is: comments separate tokens, strings and
// brackets keep their semicolons, and an escape is never syntax. var() is
// resolved against the custom properties in force. The last declaration
// of a property wins unless an earlier one is !important, and the all,
// font and overflow shorthands take part. A later declaration that would
// show the element counts only when these rules read it as valid.
func HiddenStyle(style string) bool {
	e := styleEffect(style, nil)
	return e.prune || e.visible < 0 || e.opaque < 0 || e.fill < 0
}

func styleEffect(style string, vars *varScope) effect {
	e := effect{vars: vars}
	if len(style) > maxStyleBytes {
		e.prune = true
		return e
	}
	decls, ok := parseDeclarations(style)
	if !ok {
		e.prune = true
		return e
	}
	if e.vars, ok = customProperties(decls, vars); !ok {
		e.prune = true
		return e
	}
	// One budget for the whole element, so many declarations each just
	// under the budget cannot add up.
	budget := maxVarTokens
	for i := range decls {
		if !strings.HasPrefix(decls[i].prop, "--") {
			decls[i].value = substituteBudget(decls[i].value, e.vars, 0, &budget)
		}
	}
	d := cascade(decls)
	switch {
	case isIdent(d["visibility"], "hidden", "collapse") || unknown(d["visibility"]):
		e.visible = -1
	case isIdent(d["visibility"], "visible"):
		e.visible = 1
	}
	e.opaque = colourEffect(d["color"])
	e.fill = colourEffect(d["-webkit-text-fill-color"])
	e.prune = pruned(d)
	e.moved = moved(d)
	e.reorders = reorders(d)
	e.override = isIdent(d["unicode-bidi"], "bidi-override", "isolate-override", "plaintext") || unknown(d["unicode-bidi"]) ||
		transformed(d)
	switch {
	case isIdent(d["direction"], "rtl") || unknown(d["direction"]):
		e.rtl = 1
	case isIdent(d["direction"], "ltr"):
		e.rtl = -1
	}
	if v := d["display"]; len(v) == 1 && v[0].kind == tIdent {
		e.display = v[0].text
	}
	e.opacity = 1
	if v := d["opacity"]; len(v) == 1 && v[0].numeric() {
		e.opacity = fraction(v[0])
	}
	if v := d["font-size"]; len(v) == 1 {
		switch t := v[0]; {
		case t.kind == tDimension && absolute(t.text):
			e.fontPx, _ = px(t)
		case t.kind == tDimension && (t.text == "em" || t.text == "rem" || t.text == "ex" || t.text == "ch"):
			e.fontScale = t.num
		case t.kind == tPercent:
			e.fontScale = t.num / 100
		case isIdent(v, "smaller"):
			e.fontScale = 0.83
		}
	}
	return e
}

// colourReset is what colourEffect returns for a fill that follows the
// colour again (currentcolor, initial, unset).
const colourReset = 2

func colourEffect(v []token) int {
	if isIdent(v, "currentcolor", "initial", "unset") {
		return colourReset
	}
	// An unknown colour, such as var() of a property a stylesheet sets, is
	// left alone: stylesheets are out of reach anyway, and hiding every
	// text coloured that way would hide whole sites.
	switch {
	case len(v) == 1 && transparent(v[0]):
		return -1
	case len(v) == 1 && color(v[0]) && !isIdent(v, "currentcolor", "inherit", "unset", "revert", "revert-layer"):
		return 1
	}
	return 0
}

func unknown(v []token) bool { return len(v) == 1 && v[0].kind == tUnknown }

// mayHide are the properties for which a value these rules cannot read
// hides the element.
var mayHide = []string{
	"display", "visibility", "content-visibility", "opacity", "font-size", "transform", "scale", "filter",
	"clip", "clip-path", "text-indent", "content", "unicode-bidi", "mask", "mask-image", "-webkit-mask",
	"-webkit-mask-image", "-webkit-text-security",
}

func pruned(d map[string][]token) bool {
	get := func(p string) []token { return d[p] }
	for _, p := range mayHide {
		if p != "visibility" && p != "unicode-bidi" && unknown(get(p)) {
			return true
		}
	}
	// A table column box draws none of the content inside it.
	if isIdent(get("display"), "none", "table-column", "table-column-group") || isIdent(get("content-visibility"), "hidden") {
		return true
	}
	if v := get("opacity"); len(v) == 1 && v[0].numeric() && fraction(v[0]) < 0.1 {
		return true
	}
	if v := get("font-size"); len(v) == 1 && tiny(v[0]) {
		return true
	}
	if scaledAway(get("transform"), get("scale")) || filteredAway(get("filter")) {
		return true
	}
	positioned := isIdent(get("position"), "absolute", "fixed")
	if positioned && clippedAway(get("clip")) {
		return true
	}
	// Any clip-path or mask can remove the text; reading their shapes is
	// not worth the risk of reading one wrong.
	for _, p := range []string{"clip-path", "mask", "mask-image", "-webkit-mask", "-webkit-mask-image", "-webkit-text-security"} {
		if v := get(p); len(v) > 0 && !isIdent(v, "none", "initial", "unset", "revert", "revert-layer") {
			return true
		}
	}
	if v := get("text-indent"); len(v) >= 1 {
		if p, ok := px(v[0]); ok && p <= -100 || offPage(v[0]) {
			return true
		}
	}
	for _, p := range []string{"margin", "margin-left", "margin-top", "margin-right", "margin-bottom",
		"margin-inline", "margin-inline-start", "margin-inline-end", "margin-block", "margin-block-start", "margin-block-end"} {
		for _, t := range get(p) {
			if offPage(t) {
				return true
			}
		}
	}
	if positioned || isIdent(get("position"), "relative", "sticky", "-webkit-sticky") {
		for _, p := range []string{"left", "top", "right", "bottom", "inset", "inset-inline", "inset-block",
			"inset-inline-start", "inset-inline-end", "inset-block-start", "inset-block-end"} {
			for _, t := range get(p) {
				if offPage(t) {
					return true
				}
			}
		}
	}
	for _, t := range get("content") {
		if t.kind == tURL || t.kind == tFunction && slices.Contains(imageFunctions, t.text) {
			return true // the element's content is replaced by an image
		}
	}
	x, y := overflow(d)
	for _, p := range []string{"height", "max-height", "block-size", "max-block-size"} {
		if v := get(p); y && len(v) == 1 && zero(v[0]) {
			return true
		}
	}
	for _, p := range []string{"width", "max-width", "inline-size", "max-inline-size"} {
		if v := get(p); x && len(v) == 1 && zero(v[0]) {
			return true
		}
	}
	return false
}

var imageFunctions = []string{"url", "image", "image-set", "-webkit-image-set", "linear-gradient", "radial-gradient",
	"conic-gradient", "repeating-linear-gradient", "repeating-radial-gradient", "repeating-conic-gradient",
	"cross-fade", "element", "paint"}

// moved reports a style that draws the element away from its place in
// the text, so the words before and after it need not read on into it.
func moved(d map[string][]token) bool {
	pos := d["position"]
	if isIdent(pos, "absolute", "fixed") {
		return true
	}
	if isIdent(pos, "relative", "sticky", "-webkit-sticky") {
		for _, p := range []string{"left", "top", "right", "bottom", "inset", "inset-inline", "inset-block",
			"inset-inline-start", "inset-inline-end", "inset-block-start", "inset-block-end"} {
			for _, t := range d[p] {
				if !(t.kind == tIdent && t.text == "auto") && !zero(t) {
					return true
				}
			}
		}
	}
	if v := d["float"]; len(v) > 0 && !isIdent(v, "none") {
		return true
	}
	if transformed(d) {
		return true
	}
	// A header or footer group, or a caption, is drawn at the table's
	// top or bottom wherever it is written.
	if isIdent(d["display"], "table-header-group", "table-footer-group", "table-caption") {
		return true
	}
	// A negative margin can pull a box back over the text before it.
	for _, p := range []string{"margin", "margin-left", "margin-top", "margin-right", "margin-bottom",
		"margin-inline", "margin-inline-start", "margin-inline-end", "margin-block", "margin-block-start", "margin-block-end"} {
		for _, t := range d[p] {
			if t.kind == tUnknown || t.numeric() && t.num < 0 || t.kind == tFunction && slices.Contains(mathFunctions, t.text) {
				return true
			}
		}
	}
	if v := d["order"]; len(v) > 0 && !(len(v) == 1 && v[0].kind == tNumber && v[0].num == 0) {
		return true
	}
	for _, p := range []string{"grid-area", "grid-row", "grid-column", "grid-row-start", "grid-row-end",
		"grid-column-start", "grid-column-end"} {
		if v := d[p]; len(v) > 0 && !isIdent(v, "auto") {
			return true
		}
	}
	return false
}

// transformed reports a transform of any kind, including the individual
// scale, translate and rotate properties. A transform can move, mirror or
// turn text, so what it holds is read for the agent but not checked.
func transformed(d map[string][]token) bool {
	for _, p := range []string{"transform", "translate", "rotate", "scale"} {
		if v := d[p]; len(v) > 0 && !isIdent(v, "none") && !isIdent(v, globalKeywords...) {
			return true
		}
	}
	return false
}

// reorders reports a style that lays its children out in another order
// than they are written.
func reorders(d map[string][]token) bool {
	has := func(p string, words ...string) bool {
		for _, t := range d[p] {
			if t.kind == tUnknown || t.kind == tIdent && slices.Contains(words, t.text) {
				return true
			}
		}
		return false
	}
	return has("flex-direction", "row-reverse", "column-reverse") ||
		has("flex-flow", "row-reverse", "column-reverse", "wrap-reverse") ||
		has("flex-wrap", "wrap-reverse") || has("grid-auto-flow", "dense") ||
		has("direction", "rtl") || has("writing-mode", "vertical-rl", "sideways-rl")
}

// --- custom properties

// varScope holds the custom properties in force at an element: its own,
// resolved, over its parent's. Scopes are linked, never copied, so a deep
// tree of elements that each set properties costs only what they set.
type varScope struct {
	own    map[string][]token // a nil value is invalid: initial, or a cycle
	parent *varScope
	count  int // properties set here and above
}

func (s *varScope) lookup(name string) ([]token, bool) {
	for ; s != nil; s = s.parent {
		if v, ok := s.own[name]; ok {
			return v, true
		}
	}
	return nil, false
}

func (s *varScope) total() int {
	if s == nil {
		return 0
	}
	return s.count
}

// maxCustomProperties bounds the custom properties set on an element and
// its ancestors. An element past it is hidden.
const maxCustomProperties = 1024

// customProperties are those in force inside an element: its parent's,
// with its own declarations over them. It reports false when there are
// more than maxCustomProperties.
func customProperties(decls []declaration, inherited *varScope) (*varScope, bool) {
	var own map[string][]token
	important := map[string]bool{}
	for _, d := range decls {
		if !strings.HasPrefix(d.prop, "--") {
			continue
		}
		if important[d.prop] && !d.important {
			continue
		}
		if own == nil {
			own = map[string][]token{}
		}
		own[d.prop], important[d.prop] = d.value, d.important
	}
	if own == nil {
		return inherited, true
	}
	if inherited.total()+len(own) > maxCustomProperties {
		return nil, false
	}
	// A custom property's value is computed where it is set: var() in it
	// is resolved against what is in force there, its siblings included,
	// and a CSS-wide keyword keeps the inherited value (inherit, unset,
	// revert) or makes it invalid (initial).
	pending := &varScope{own: map[string][]token{}, parent: inherited}
	for k, v := range own {
		if !isIdent(v, globalKeywords...) {
			pending.own[k] = v
		}
	}
	out := &varScope{own: map[string][]token{}, parent: inherited, count: inherited.total() + len(own)}
	budget := maxVarTokens
	for k, v := range own {
		switch {
		case isIdent(v, "initial"):
			out.own[k] = nil
		case isIdent(v, globalKeywords...):
			// Keeps the inherited value: nothing set here.
		default:
			// While k is being resolved it refers to nothing, so a cycle
			// through it is invalid.
			saved := pending.own[k]
			pending.own[k] = nil
			out.own[k] = substituteBudget(v, pending, 0, &budget)
			if unknown(out.own[k]) {
				out.own[k] = nil
			}
			pending.own[k] = saved
		}
	}
	return out, true
}

// maxVarDepth bounds var() inside var(), and maxVarTokens the tokens
// substitution may produce, since each var() can double them.
const (
	maxVarDepth  = 8
	maxVarTokens = 4096
)

// substitute replaces var(--x) and var(--x, fallback) with their values. A
// var() that cannot be resolved makes the whole value unknown.
func substitute(v []token, vars *varScope, depth int) []token {
	budget := maxVarTokens
	return substituteBudget(v, vars, depth, &budget)
}

func substituteBudget(v []token, vars *varScope, depth int, budget *int) []token {
	if !hasVar(v) {
		return v
	}
	if depth >= maxVarDepth || *budget <= 0 {
		return []token{{kind: tUnknown}}
	}
	var out []token
	for _, t := range v {
		if t.kind == tFunction && t.text == "var" {
			if len(t.args) == 0 || t.args[0].kind != tIdent || !strings.HasPrefix(t.args[0].text, "--") {
				return []token{{kind: tUnknown}}
			}
			val, ok := vars.lookup(t.args[0].text)
			// A property that is missing, refers to itself or did not
			// resolve is invalid at computed time, and var() takes its
			// fallback.
			if ok && val != nil {
				val = substituteBudget(val, vars, depth+1, budget)
			}
			if !ok || val == nil || unknown(val) {
				if len(t.args) >= 2 && t.args[1].kind == tDelim && t.args[1].text == "," {
					val = substituteBudget(t.args[2:], vars, depth+1, budget)
				} else {
					return []token{{kind: tUnknown}}
				}
			}
			if unknown(val) {
				return val
			}
			if *budget -= size(val); *budget < 0 {
				return []token{{kind: tUnknown}}
			}
			out = append(out, val...)
			continue
		}
		if t.args != nil {
			t.args = substituteBudget(t.args, vars, depth+1, budget)
			if unknown(t.args) {
				return t.args
			}
		}
		out = append(out, t)
	}
	return out
}

// size counts tokens, those inside functions and blocks too.
func size(v []token) int {
	n := len(v)
	for _, t := range v {
		n += size(t.args)
	}
	return n
}

func hasVar(v []token) bool {
	for _, t := range v {
		if t.kind == tFunction && t.text == "var" || hasVar(t.args) {
			return true
		}
	}
	return false
}

// --- cascade

// inspected are the properties these rules read; "all" resets them.
var inspected = []string{
	"display", "visibility", "content-visibility", "opacity", "font-size", "color", "-webkit-text-fill-color",
	"transform", "scale", "translate", "rotate", "filter", "position", "clip", "clip-path", "text-indent",
	"left", "top", "right", "bottom", "overflow-x", "overflow-y", "height", "max-height", "width", "max-width",
	"content", "float", "order",
	// all leaves direction and unicode-bidi alone.
}

// cascade keeps the winning declaration of each property. A declaration
// counts when it is valid or when it would hide. A value these rules cannot
// read for a property that can hide becomes unknown, which hides; for a
// colour it counts only if it names transparent. The overflow, font and
// all shorthands set their longhands in their place.
func cascade(decls []declaration) map[string][]token {
	type entry struct {
		value     []token
		important bool
	}
	won := map[string]entry{}
	set := func(prop string, value []token, important bool) {
		if prev, ok := won[prop]; ok && prev.important && !important {
			return
		}
		won[prop] = entry{value, important}
	}
	for _, d := range decls {
		if len(d.value) == 0 || strings.HasPrefix(d.prop, "--") {
			continue
		}
		switch {
		case unknown(d.value):
			set(d.prop, d.value, d.important)
		case d.prop == "all":
			if isIdent(d.value, globalKeywords...) {
				for _, p := range inspected {
					set(p, d.value, d.important)
				}
			} else {
				for _, p := range inspected {
					set(p, []token{{kind: tUnknown}}, d.important)
				}
			}
		case d.prop == "overflow":
			if valid(d.prop, d.value) {
				set("overflow-x", d.value[:1], d.important)
				set("overflow-y", d.value[len(d.value)-1:], d.important)
			}
		case d.prop == "font":
			if size, ok := fontSize(d.value); ok {
				set("font-size", size, d.important)
			}
		case valid(d.prop, d.value) || hides(d.prop, d.value):
			set(d.prop, d.value, d.important)
		case slices.Contains(mayHide, d.prop):
			set(d.prop, []token{{kind: tUnknown}}, d.important)
		case (d.prop == "color" || d.prop == "-webkit-text-fill-color") && (mentions(d.value, "transparent") || alphaZero(d.value)):
			set(d.prop, []token{{kind: tIdent, text: "transparent"}}, d.important)
		}
	}
	out := map[string][]token{}
	for p, e := range won {
		out[p] = e.value
	}
	return out
}

// alphaZero reports a colour function of any kind whose alpha, after "/",
// is zero or none: color(srgb 0 0 0 / 0), rgb(from red r g b / 0).
func alphaZero(v []token) bool {
	for _, t := range v {
		if t.kind != tFunction {
			continue
		}
		for i, a := range t.args {
			if a.kind == tDelim && a.text == "/" && i == len(t.args)-2 {
				b := t.args[i+1]
				if b.numeric() && b.num == 0 || b.kind == tIdent && b.text == "none" {
					return true
				}
			}
		}
	}
	return false
}

func mentions(v []token, ident string) bool {
	for _, t := range v {
		if t.kind == tIdent && t.text == ident || mentions(t.args, ident) {
			return true
		}
	}
	return false
}

// hides reports whether one declaration, read on its own, would hide.
func hides(prop string, v []token) bool {
	return pruned(map[string][]token{prop: v}) ||
		prop == "visibility" && isIdent(v, "hidden", "collapse") ||
		(prop == "color" || prop == "-webkit-text-fill-color") && len(v) == 1 && transparent(v[0])
}

// fontSize finds the size in a valid font shorthand: a length, percentage
// or size keyword followed, after any line height, by a family. A system
// font keyword sets an ordinary size.
func fontSize(v []token) ([]token, bool) {
	if isIdent(v, globalKeywords...) {
		return v, true
	}
	if isIdent(v, "caption", "icon", "menu", "message-box", "small-caption", "status-bar") {
		return []token{{kind: tIdent, text: "medium"}}, true
	}
	seen := map[string]bool{}
	for i, t := range v {
		if !(fontLength(t) || t.kind == tIdent && slices.Contains(sizeKeywords, t.text)) {
			// Before the size come only style, variant, weight and
			// stretch, each at most once, as a keyword or a weight.
			cat := fontCategory(t)
			if cat == "" || cat != "normal" && seen[cat] {
				return nil, false
			}
			seen[cat] = true
			continue
		}
		rest := v[i+1:]
		if len(rest) >= 2 && rest[0].kind == tDelim && rest[0].text == "/" {
			lh := rest[1]
			if !(isIdent(rest[1:2], "normal") || lh.numeric() && lh.num >= 0 && (lh.kind != tDimension || slices.Contains(lengthUnits, lh.text))) {
				return nil, false
			}
			rest = rest[2:]
		}
		if !familyList(rest) {
			return nil, false
		}
		return []token{t}, true
	}
	return nil, false
}

func fontCategory(t token) string {
	switch {
	case t.kind == tNumber && t.num >= 1 && t.num <= 1000:
		return "weight"
	case t.kind != tIdent:
		return ""
	case t.text == "normal":
		return "normal"
	case t.text == "italic" || t.text == "oblique":
		return "style"
	case t.text == "small-caps":
		return "variant"
	case t.text == "bold" || t.text == "bolder" || t.text == "lighter":
		return "weight"
	case strings.HasSuffix(t.text, "condensed") || strings.HasSuffix(t.text, "expanded"):
		return "stretch"
	}
	return ""
}

// familyList is one or more font families separated by single commas,
// each a string or a run of names.
func familyList(v []token) bool {
	var group []token
	groups := [][]token{}
	for _, t := range v {
		if t.kind == tDelim && t.text == "," {
			groups, group = append(groups, group), nil
			continue
		}
		group = append(group, t)
	}
	groups = append(groups, group)
	for _, g := range groups {
		switch {
		case len(g) == 1 && g[0].kind == tString:
		case len(g) > 0 && allKind(g, tIdent):
		default:
			return false
		}
	}
	return true
}

func allKind(v []token, k kind) bool {
	for _, t := range v {
		if t.kind != k {
			return false
		}
	}
	return true
}

func fontLength(t token) bool {
	return t.kind != tFunction && length(t, false) && t.kind != tNumber
}

var globalKeywords = []string{"inherit", "initial", "unset", "revert", "revert-layer"}

var fontPrefixKeywords = []string{"normal", "italic", "oblique", "small-caps", "bold", "bolder", "lighter",
	"ultra-condensed", "extra-condensed", "condensed", "semi-condensed", "semi-expanded", "expanded",
	"extra-expanded", "ultra-expanded"}

// displayKeywords are display values a current browser accepts. Unsure
// ones are left out: an override to one of them is ignored, which hides.
var displayKeywords = []string{
	"none", "block", "inline", "inline-block", "flex", "inline-flex", "grid", "inline-grid", "flow-root",
	"table", "inline-table", "table-row", "table-cell", "table-row-group", "table-header-group",
	"table-footer-group", "table-column", "table-column-group", "table-caption", "list-item", "contents",
	"ruby", "math", "-webkit-box", "-webkit-inline-box",
}

var sizeKeywords = []string{"xx-small", "x-small", "small", "medium", "large", "x-large", "xx-large", "xxx-large", "smaller", "larger", "math"}

var overflowKeywords = []string{"visible", "hidden", "clip", "scroll", "auto", "overlay"}

var lengthKeywords = []string{"auto", "none", "min-content", "max-content", "fit-content", "stretch", "-webkit-fill-available"}

var mathFunctions = []string{"calc", "min", "max", "clamp", "-webkit-calc"}

var colorFunctions = []string{"rgb", "rgba", "hsl", "hsla", "hwb", "lab", "lch", "oklab", "oklch"}

var lengthUnits = []string{
	"px", "em", "rem", "ex", "rex", "ch", "rch", "cap", "rcap", "ic", "ric", "lh", "rlh", "vw", "vh", "vi", "vb", "vmin", "vmax",
	"svw", "svh", "lvw", "lvh", "dvw", "dvh", "cqw", "cqh", "cqi", "cqb", "cqmin", "cqmax",
	"cm", "mm", "q", "in", "pt", "pc",
}

var angleUnits = []string{"deg", "rad", "grad", "turn"}

// valid reports whether a value is one these rules read as valid for a
// property they inspect. It accepts the plain forms and refuses anything
// it cannot read.
func valid(prop string, v []token) bool {
	if isIdent(v, globalKeywords...) {
		return true
	}
	one := len(v) == 1
	switch prop {
	case "display":
		return one && isIdent(v, displayKeywords...)
	case "visibility":
		return isIdent(v, "visible", "hidden", "collapse")
	case "content-visibility":
		return isIdent(v, "visible", "auto", "hidden")
	case "unicode-bidi":
		return isIdent(v, "normal", "embed", "isolate", "bidi-override", "isolate-override", "plaintext")
	case "position":
		return isIdent(v, "static", "relative", "absolute", "fixed", "sticky", "-webkit-sticky")
	case "overflow":
		return (len(v) == 1 || len(v) == 2) && allIdents(v, overflowKeywords)
	case "overflow-x", "overflow-y":
		return isIdent(v, overflowKeywords...)
	case "opacity":
		return one && (v[0].kind == tNumber || v[0].kind == tPercent)
	case "font-size":
		return one && (fontLength(v[0]) || isIdent(v, sizeKeywords...))
	case "color", "-webkit-text-fill-color":
		return one && color(v[0])
	case "transform":
		return isIdent(v, "none") || transforms(v)
	case "scale":
		if isIdent(v, "none") {
			return true
		}
		for _, t := range v {
			if t.kind != tNumber && t.kind != tPercent {
				return false
			}
		}
		return len(v) <= 3
	case "filter":
		return isIdent(v, "none") || filters(v)
	case "clip":
		return isIdent(v, "auto") || one && rect(v[0])
	case "clip-path":
		// inset() and circle() are judged by clipPathAway; other shapes
		// could clip everything and are not read, so they hide.
		return isIdent(v, "none") || one && v[0].kind == tFunction && (v[0].text == "inset" || v[0].text == "circle")
	case "content":
		return isIdent(v, "normal", "none") || onlyStrings(v)
	case "text-indent":
		return one && length(v[0], true)
	case "left", "top", "right", "bottom":
		return one && (length(v[0], true) || isIdent(v, "auto"))
	case "height", "max-height", "width", "max-width", "block-size", "max-block-size", "inline-size", "max-inline-size":
		return one && (length(v[0], false) || isIdent(v, lengthKeywords...))
	}
	return true
}

func onlyStrings(v []token) bool {
	for _, t := range v {
		if t.kind != tString {
			return false
		}
	}
	return true
}

func allIdents(v []token, set []string) bool {
	for _, t := range v {
		if t.kind != tIdent || !slices.Contains(set, t.text) {
			return false
		}
	}
	return true
}

// length reports whether a token is a CSS length or percentage; a bare
// zero is one too, and so is calc() and its kin. Negative values are
// allowed only where negative is.
func length(t token, negative bool) bool {
	if !negative && t.num < 0 {
		return false
	}
	switch t.kind {
	case tPercent:
		return true
	case tNumber:
		return t.num == 0
	case tDimension:
		return slices.Contains(lengthUnits, t.text)
	case tFunction:
		return slices.Contains(mathFunctions, t.text)
	}
	return false
}

// components splits function arguments into their values. With spaces
// allowed, they are either all separated by commas (a, b, c) or all by
// spaces with an optional "/" before the last (a b c / d); without, only
// the comma form is accepted. It reports false for any other shape, such
// as an empty value between commas.
func components(args []token, spaces bool) ([]token, bool) {
	if len(args) == 0 {
		return nil, true
	}
	sep := func(a token, s string) bool { return a.kind == tDelim && a.text == s }
	commaForm := false
	for _, a := range args {
		if sep(a, ",") {
			commaForm = true
		}
	}
	var out []token
	if commaForm || !spaces {
		for i, a := range args {
			if i%2 == 0 {
				if sep(a, ",") || sep(a, "/") {
					return nil, false
				}
				out = append(out, a)
			} else if !sep(a, ",") {
				return nil, false
			}
		}
		return out, len(args)%2 == 1
	}
	for i, a := range args {
		if sep(a, "/") {
			if i != len(args)-2 || i == 0 {
				return nil, false
			}
			continue
		}
		out = append(out, a)
	}
	return out, true
}

// argument counts and kinds for each transform function.
var transformArgs = map[string]struct {
	min, max int
	kind     string // "length", "number", "angle", "mixed"
}{
	"translate": {1, 2, "length"}, "translatex": {1, 1, "length"}, "translatey": {1, 1, "length"},
	"translatez": {1, 1, "length"}, "translate3d": {3, 3, "length"},
	"scale": {1, 2, "number"}, "scalex": {1, 1, "number"}, "scaley": {1, 1, "number"},
	"scalez": {1, 1, "number"}, "scale3d": {3, 3, "number"},
	"rotate": {1, 1, "angle"}, "rotatex": {1, 1, "angle"}, "rotatey": {1, 1, "angle"}, "rotatez": {1, 1, "angle"},
	"rotate3d": {4, 4, "mixed"}, "skew": {1, 2, "angle"}, "skewx": {1, 1, "angle"}, "skewy": {1, 1, "angle"},
	"matrix": {6, 6, "number"}, "matrix3d": {16, 16, "number"}, "perspective": {1, 1, "length"},
}

func argKind(a token, kind string) bool {
	switch kind {
	case "length":
		return length(a, true) && a.kind != tFunction
	case "number":
		return a.kind == tNumber || a.kind == tPercent
	case "angle":
		return a.kind == tNumber && a.num == 0 || a.kind == tDimension && slices.Contains(angleUnits, a.text)
	case "mixed":
		return a.kind == tNumber || a.kind == tDimension && slices.Contains(angleUnits, a.text)
	}
	return false
}

func transforms(v []token) bool {
	for _, t := range v {
		spec, ok := transformArgs[t.text]
		if t.kind != tFunction || !ok {
			return false
		}
		args, ok := components(t.args, false)
		if !ok || len(args) < spec.min || len(args) > spec.max {
			return false
		}
		for i, a := range args {
			if !argKind(a, spec.kind) {
				return false
			}
			// A z length has no box to be a percentage of.
			if a.kind == tPercent && (t.text == "translatez" || t.text == "perspective" || t.text == "translate3d" && i == 2) {
				return false
			}
		}
	}
	return len(v) > 0
}

var filterArgs = map[string]string{
	"blur": "length", "brightness": "amount", "contrast": "amount", "grayscale": "amount", "hue-rotate": "angle",
	"invert": "amount", "opacity": "amount", "saturate": "amount", "sepia": "amount",
}

func filters(v []token) bool {
	for _, t := range v {
		kind, ok := filterArgs[t.text]
		if t.kind != tFunction || !ok {
			return false
		}
		args, ok := components(t.args, false)
		if !ok || len(args) > 1 {
			return false
		}
		for _, a := range args {
			switch {
			case kind == "amount" && (a.kind == tNumber || a.kind == tPercent) && a.num >= 0:
			case kind == "length" && length(a, false) && a.kind != tFunction && a.kind != tPercent:
			case kind == "angle" && argKind(a, "angle"):
			default:
				return false
			}
		}
	}
	return len(v) > 0
}

func rect(t token) bool {
	if t.kind != tFunction || t.text != "rect" {
		return false
	}
	args, ok := components(t.args, true)
	if !ok || len(args) != 4 {
		return false
	}
	for _, a := range args {
		if !(length(a, true) && a.kind != tFunction && a.kind != tPercent || a.kind == tIdent && a.text == "auto") {
			return false
		}
	}
	return true
}

func isIdent(v []token, names ...string) bool {
	return len(v) == 1 && v[0].kind == tIdent && slices.Contains(names, v[0].text)
}

// --- values

// px is a length in CSS pixels, taking em and other relative units at the
// usual 16px.
func px(t token) (float64, bool) {
	scale := map[string]float64{
		"px": 1, "in": 96, "cm": 96 / 2.54, "mm": 96 / 25.4, "q": 96 / 101.6, "pt": 96.0 / 72, "pc": 16,
		"em": 16, "rem": 16, "ex": 8, "ch": 8, "lh": 19, "rlh": 19, "vw": 10, "vh": 10, "vmin": 10, "vmax": 10,
	}
	switch t.kind {
	case tNumber:
		return t.num, t.num == 0
	case tDimension:
		f, ok := scale[t.text]
		return t.num * f, ok
	}
	return 0, false
}

// zero reports a length of zero; for calc() and its kin, one whose every
// number is zero.
func zero(t token) bool {
	if t.kind == tFunction && slices.Contains(mathFunctions, t.text) {
		seen := false
		for _, a := range t.args {
			if a.numeric() {
				if a.num != 0 {
					return false
				}
				seen = true
			}
		}
		return seen
	}
	return t.numeric() && t.num == 0
}

// tiny reports whether a font size is too small to read: below 2px, or
// below a tenth of its reference.
func tiny(t token) bool {
	switch t.kind {
	case tNumber:
		return t.num == 0
	case tPercent:
		return t.num < 10
	case tDimension:
		if absolute(t.text) {
			p, _ := px(t)
			return p < 2
		}
		return slices.Contains(lengthUnits, t.text) && t.num < 0.1
	case tFunction:
		return slices.Contains(mathFunctions, t.text) // cannot tell, so hide
	}
	return false
}

func absolute(unit string) bool {
	return slices.Contains([]string{"px", "in", "cm", "mm", "q", "pt", "pc"}, unit)
}

// offPage reports a length at least 999px to the left of or above the
// page; for calc() and its kin, one with any such term.
func offPage(t token) bool {
	if t.kind == tFunction && slices.Contains(mathFunctions, t.text) {
		for _, a := range t.args {
			if offPage(a) {
				return true
			}
		}
		return false
	}
	p, ok := px(t)
	return ok && p <= -999
}

// fraction is a number or percentage as a fraction of one; anything else
// is one, which hides nothing.
func fraction(t token) float64 {
	switch t.kind {
	case tPercent:
		return t.num / 100
	case tNumber:
		return t.num
	}
	return 1
}

var namedColors = strings.Fields(`aliceblue antiquewhite aqua aquamarine azure beige bisque black
blanchedalmond blue blueviolet brown burlywood cadetblue chartreuse chocolate coral cornflowerblue
cornsilk crimson cyan darkblue darkcyan darkgoldenrod darkgray darkgreen darkgrey darkkhaki darkmagenta
darkolivegreen darkorange darkorchid darkred darksalmon darkseagreen darkslateblue darkslategray
darkslategrey darkturquoise darkviolet deeppink deepskyblue dimgray dimgrey dodgerblue firebrick
floralwhite forestgreen fuchsia gainsboro ghostwhite gold goldenrod gray green greenyellow grey honeydew
hotpink indianred indigo ivory khaki lavender lavenderblush lawngreen lemonchiffon lightblue lightcoral
lightcyan lightgoldenrodyellow lightgray lightgreen lightgrey lightpink lightsalmon lightseagreen
lightskyblue lightslategray lightslategrey lightsteelblue lightyellow lime limegreen linen magenta maroon
mediumaquamarine mediumblue mediumorchid mediumpurple mediumseagreen mediumslateblue mediumspringgreen
mediumturquoise mediumvioletred midnightblue mintcream mistyrose moccasin navajowhite navy oldlace olive
olivedrab orange orangered orchid palegoldenrod palegreen paleturquoise palevioletred papayawhip
peachpuff peru pink plum powderblue purple rebeccapurple red rosybrown royalblue saddlebrown salmon
sandybrown seagreen seashell sienna silver skyblue slateblue slategray slategrey snow springgreen
steelblue tan teal thistle tomato turquoise violet wheat white whitesmoke yellow yellowgreen
transparent currentcolor canvas canvastext linktext visitedtext activetext buttonface buttontext
buttonborder field fieldtext highlight highlighttext selecteditem selecteditemtext mark marktext
graytext accentcolor accentcolortext`)

// color reports whether a token is a colour these rules can read: a named
// colour, a hex colour, or a colour function with three components and an
// optional alpha, all plain numbers.
func color(t token) bool {
	switch t.kind {
	case tIdent:
		return slices.Contains(namedColors, t.text)
	case tHash:
		if strings.Trim(t.text, "0123456789abcdef") != "" {
			return false
		}
		n := len(t.text)
		return n == 3 || n == 4 || n == 6 || n == 8
	case tFunction:
		if !slices.Contains(colorFunctions, t.text) {
			return false
		}
		args, ok := colourArgs(t.args)
		if !ok {
			return false
		}
		// lab(), lch(), oklab(), oklch() and hwb() take only the space form.
		if commaSeparated(t.args) && !slices.Contains([]string{"rgb", "rgba", "hsl", "hsla"}, t.text) {
			return false
		}
		for i, a := range args {
			hue := i == 0 && (strings.HasPrefix(t.text, "hsl") || t.text == "hwb") || i == 2 && strings.HasSuffix(t.text, "lch")
			switch {
			case a.kind == tNumber || a.kind == tPercent:
			case hue && a.kind == tDimension && slices.Contains(angleUnits, a.text):
			case a.kind == tIdent && a.text == "none" && !commaSeparated(t.args):
			default:
				return false
			}
		}
		return true
	}
	return false
}

func commaSeparated(args []token) bool {
	return len(args) > 1 && args[1].kind == tDelim && args[1].text == ","
}

// colourArgs are a colour function's three components and optional
// alpha: commas between all four, or spaces with "/" before the alpha.
func colourArgs(args []token) ([]token, bool) {
	out, ok := components(args, true)
	if !ok || len(out) < 3 || len(out) > 4 {
		return nil, false
	}
	for _, a := range args {
		if a.kind == tDelim && a.text == "/" && len(out) != 4 {
			return nil, false // rgb(0 0 / 0) has two components
		}
	}
	slash := false
	for _, a := range args {
		if a.kind == tDelim && a.text == "/" {
			slash = true
		}
	}
	comma := len(args) > 1 && args[1].kind == tDelim && args[1].text == ","
	if len(out) == 4 && !comma && !slash {
		return nil, false // rgb(0 0 0 1) needs its "/"
	}
	return out, true
}

// transparent reports whether a colour's alpha is below 0.1, as an opacity
// below 0.1 hides.
func transparent(t token) bool {
	switch t.kind {
	case tIdent:
		return t.text == "transparent"
	case tHash:
		h := t.text
		if len(h) == 4 {
			return h[3] == '0' || h[3] == '1'
		}
		if len(h) == 8 {
			a, err := strconv.ParseUint(h[6:], 16, 8)
			return err == nil && a < 0x1a
		}
		return false
	case tFunction:
		if !slices.Contains(colorFunctions, t.text) {
			return false
		}
		args, ok := colourArgs(t.args)
		if !ok || len(args) != 4 {
			return false
		}
		a := args[3]
		return (a.kind == tNumber || a.kind == tPercent) && fraction(a) < 0.1 || a.kind == tIdent && a.text == "none"
	}
	return false
}

// scaledAway reports a transform or scale that flattens the element on
// the x or y axis.
func scaledAway(transform, scale []token) bool {
	for _, f := range transform {
		args := numbers(f.args)
		switch f.text {
		case "scale":
			if len(args) >= 1 && args[0] == 0 || len(args) >= 2 && args[1] == 0 {
				return true
			}
		case "scalex", "scaley":
			if len(args) >= 1 && args[0] == 0 {
				return true
			}
		case "scale3d":
			if len(args) >= 2 && (args[0] == 0 || args[1] == 0) {
				return true
			}
		}
	}
	if len(scale) >= 1 && (scale[0].kind == tNumber || scale[0].kind == tPercent) {
		x := fraction(scale[0])
		y := x
		if len(scale) >= 2 && (scale[1].kind == tNumber || scale[1].kind == tPercent) {
			y = fraction(scale[1])
		}
		return x == 0 || y == 0
	}
	return false
}

// numbers are a function's numeric arguments, commas dropped; nil if any
// argument is something else.
func numbers(args []token) []float64 {
	var out []float64
	for _, a := range args {
		switch {
		case a.kind == tDelim && a.text == ",":
		case a.kind == tNumber || a.kind == tPercent:
			out = append(out, fraction(a))
		default:
			return nil
		}
	}
	return out
}

func filteredAway(filter []token) bool {
	for _, f := range filter {
		if f.kind == tFunction && f.text == "opacity" {
			if args := numbers(f.args); len(args) == 1 && args[0] < 0.1 {
				return true
			}
		}
	}
	return false
}

// clippedAway reports clip: rect() with no area.
func clippedAway(clip []token) bool {
	if len(clip) != 1 || clip[0].kind != tFunction || clip[0].text != "rect" {
		return false
	}
	args, _ := components(clip[0].args, true)
	var edges []float64
	for _, a := range args {
		p, ok := px(a)
		if !ok {
			return false
		}
		edges = append(edges, p)
	}
	return len(edges) == 4 && (edges[0] >= edges[2] || edges[3] >= edges[1])
}

// clipPathAway reports an inset() that removes the whole box, or a circle
// of radius zero.
func clipPathAway(v []token) bool {
	if len(v) < 1 || v[0].kind != tFunction {
		return false
	}
	switch v[0].text {
	case "inset":
		var pct []float64
		for _, a := range v[0].args {
			if a.kind == tIdent && a.text == "round" {
				break
			}
			switch {
			case a.kind == tPercent:
				pct = append(pct, a.num)
			case a.kind == tNumber && a.num == 0:
				pct = append(pct, 0)
			default:
				// A length needs the box's size to judge. Leaning toward
				// hiding, an inset of 50px or more takes it all.
				if p, ok := px(a); !ok || p >= 50 {
					return true
				}
				pct = append(pct, 0)
			}
		}
		// One to four values, as for margin: top, right, bottom, left.
		var t, r, b, l float64
		switch len(pct) {
		case 1:
			t, r, b, l = pct[0], pct[0], pct[0], pct[0]
		case 2:
			t, r, b, l = pct[0], pct[1], pct[0], pct[1]
		case 3:
			t, r, b, l = pct[0], pct[1], pct[2], pct[1]
		case 4:
			t, r, b, l = pct[0], pct[1], pct[2], pct[3]
		default:
			return false
		}
		return t+b >= 100 || l+r >= 100
	case "circle":
		args := v[0].args
		return len(args) >= 1 && args[0].numeric() && args[0].num == 0
	}
	return false
}

// overflow reports whether each axis clips, with visible computing to auto
// when the other axis is not visible or clip, as CSS Overflow says.
func overflow(d map[string][]token) (x, y bool) {
	get := func(p string) string {
		if v := d[p]; isIdent(v, overflowKeywords...) {
			return v[0].text
		}
		return "visible"
	}
	ox, oy := get("overflow-x"), get("overflow-y")
	if ox == "visible" && oy != "visible" && oy != "clip" {
		ox = "auto"
	}
	if oy == "visible" && ox != "visible" && ox != "clip" {
		oy = "auto"
	}
	return ox != "visible", oy != "visible"
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

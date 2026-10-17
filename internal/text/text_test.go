package text

import (
	"context"
	"fmt"
	"net/url"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"golang.org/x/net/html"
	"pgregory.net/rapid"
)

func page(t *testing.T, body, base string) Page {
	t.Helper()
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	return HTML(context.Background(), []byte(body), u, 1<<20)
}

func TestHiddenElementsDroppedAndCounted(t *testing.T) {
	body := `<html><head><title> The  title </title><style>p{}</style></head><body>
<p>Hi <span style="display:none">secret</span> there</p>
<div hidden>abc</div>
<div aria-hidden="TRUE">xy</div>
<p style="visibility: hidden">vis</p>
<p style="font-size:0px">fs</p>
<p style="opacity:0.0 !important">op</p>
<p style="color:red; display : NONE">dn</p>
<p style="display:n\6f ne">esc</p>
<dialog>dlg</dialog>
<dialog open>shown</dialog>
<script>var x = "script text"</script>
<noscript>noscript text</noscript>
<p style="font-size:12px;opacity:0.5">seen</p>
</body></html>`
	p := page(t, body, "https://example.com/")
	if p.Title != "The title" {
		t.Errorf("title %q", p.Title)
	}
	want := "Hi there\n\nshown\n\nseen"
	if p.Text != want {
		t.Errorf("text %q, want %q", p.Text, want)
	}
	hidden := []string{"secret", "abc", "xy", "vis", "fs", "op", "dn", "esc", "dlg"}
	if want := len(strings.Join(hidden, "")); p.Hidden != want {
		t.Errorf("hidden %d, want %d", p.Hidden, want)
	}
	for _, s := range append(hidden, "script text", "noscript text") {
		if strings.Contains(p.Text, s) {
			t.Errorf("%q leaked into %q", s, p.Text)
		}
	}
}

func TestCSSEscapesHide(t *testing.T) {
	for _, style := range []string{`display:n\6f ne`, `display:\6e\6f\6e\65`, `vis\ibility:hidden`, `font-size:0`} {
		if !HiddenStyle(style) {
			t.Errorf("%s not hidden", style)
		}
	}
	for _, style := range []string{`display:block`, `font-size:10px`, `opacity:1`, `color:red`, `display`} {
		if HiddenStyle(style) {
			t.Errorf("%s hidden", style)
		}
	}
}

func TestLinksAbsolute(t *testing.T) {
	body := `<p>See <a href="../c?x=1">the docs</a>, <a href="https://other.org/">other</a>,
<a href="#top">top</a>, <a href="javascript:alert(1)">js</a>, <a href="mailto:a@b.c">mail</a>,
<a href="/img"><img alt="logo"></a> and <a href="/bare"></a>.</p>`
	p := page(t, body, "https://example.com/a/b.html")
	want := "See the docs <https://example.com/c?x=1>, other <https://other.org/>, top, js, mail, [image: logo] <https://example.com/img> and <https://example.com/bare>."
	if p.Text != want {
		t.Errorf("got  %q\nwant %q", p.Text, want)
	}
}

func TestBaseElement(t *testing.T) {
	body := `<html><head><base href="https://cdn.example.org/x/"></head><body><a href="y">y</a></body></html>`
	p := page(t, body, "https://example.com/")
	if p.Text != "y <https://cdn.example.org/x/y>" {
		t.Errorf("got %q", p.Text)
	}
}

func TestStructure(t *testing.T) {
	body := `<h1>Title</h1><p>one
two</p><ul><li>a</li><li>b</li></ul><table><tr><td>x</td><td>y</td></tr></table><pre>  keep
   this</pre>`
	p := page(t, body, "https://example.com/")
	want := "# Title\n\none two\n\n- a\n- b\n\nx | y\n\n  keep\n   this"
	if p.Text != want {
		t.Errorf("got  %q\nwant %q", p.Text, want)
	}
}

func TestEscape(t *testing.T) {
	in := "a​b‮c\U000E0041de­fㅤg️h\tx\ny\r"
	out, n := Escape(in)
	want := `a\u{200B}b\u{202E}c\u{E0041}d\u{E000}e\u{00AD}f\u{3164}g\u{FE0F}h` + "\tx\ny" + `\u{000D}`
	if out != want || n != 8 {
		t.Errorf("got %q (%d), want %q", out, n, want)
	}
}

func TestEscapeProperties(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		s := rapid.String().Draw(t, "s")
		out, n := Escape(s)
		if HasInvisible(out) {
			t.Fatalf("invisible rune left in %q", out)
		}
		count := 0
		for _, r := range s {
			if Invisible(r) {
				count++
			}
		}
		if n != count {
			t.Fatalf("counted %d, want %d", n, count)
		}
		if count == 0 && out != s {
			t.Fatalf("changed %q to %q", s, out)
		}
	})
}

// Words that are shown and words that are hidden come from different
// alphabets, so a leak is unambiguous.
var (
	shownWord  = rapid.StringMatching(`[a-z]{1,8}`)
	hiddenWord = rapid.StringMatching(`[A-Z]{1,8}`)
)

var hiders = []string{
	`<span hidden>%s</span>`, `<div aria-hidden="true">%s</div>`, `<p style="display:none">%s</p>`,
	`<b style="visibility:hidden">%s</b>`, `<i style="font-size:0">%s</i>`, `<em style="opacity:0">%s</em>`,
	`<div hidden><p>%s</p></div>`,
}

var showers = []string{`<span>%s</span>`, `<p>%s</p>`, `<div><b>%s</b></div>`, `%s`, `<li>%s</li>`, `<td>%s</td>`}

func TestHiddenNeverLeaks(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		n := rapid.IntRange(1, 12).Draw(t, "n")
		var b strings.Builder
		var shown []string
		hiddenChars := 0
		for range n {
			if rapid.Bool().Draw(t, "hide") {
				w := hiddenWord.Draw(t, "hidden")
				hiddenChars += utf8.RuneCountInString(w)
				fmt.Fprintf(&b, rapid.SampledFrom(hiders).Draw(t, "hider"), w)
			} else {
				w := shownWord.Draw(t, "shown")
				shown = append(shown, w)
				fmt.Fprintf(&b, rapid.SampledFrom(showers).Draw(t, "shower"), w)
			}
			b.WriteString(" ")
		}
		p := HTML(context.Background(), []byte(b.String()), &url.URL{Scheme: "https", Host: "example.com", Path: "/"}, 1<<20)
		if strings.ContainsAny(p.Text, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") {
			t.Fatalf("hidden text leaked: %q from %q", p.Text, b.String())
		}
		for _, w := range shown {
			if !strings.Contains(p.Text, w) {
				t.Fatalf("%q missing from %q", w, p.Text)
			}
		}
		if p.Hidden != hiddenChars {
			t.Fatalf("hidden %d, want %d", p.Hidden, hiddenChars)
		}
	})
}

// Text a page could hold: letters with and without accents, combining
// marks, fullwidth and ligature forms, curly quotes, dashes, odd spaces
// and invisible characters.
var pageRune = rapid.SampledFrom([]rune{
	'a', 'b', 'c', 'X', 'Y', 'é', 'Å', 'ß', 'σ', 'ς', 'Σ', 'и', 'Ж', '日', 'ｆ', 'ﬁ', '①', '²',
	'́', '̈', '‘', '’', '“', '”', '"', '\'', '–', '—', '-', '.', ',', '…',
	' ', ' ', ' ', ' ', '　', '\t', '\n', '​', '⁠', '­',
})

func TestQuoteFromTextIsFound(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		text := string(rapid.SliceOfN(pageRune, 1, 80).Draw(t, "text"))
		// A quote is a run of whole words from the text, as a person
		// would select it.
		words := strings.Fields(text)
		if len(words) == 0 {
			return
		}
		i := rapid.IntRange(0, len(words)-1).Draw(t, "start")
		j := rapid.IntRange(i+1, len(words)).Draw(t, "end")
		quote := strings.Join(words[i:j], " ")
		if Normalise(quote) == "" {
			return
		}
		if !Find(Normalise(text), quote) {
			t.Fatalf("%q not found in %q (%q in %q)", quote, text, Normalise(quote), Normalise(text))
		}
	})
}

var asciiWord = rapid.StringMatching(`[A-Za-z0-9'"-]{1,8}`)

func TestWhitespaceAndCaseDoNotMatter(t *testing.T) {
	spaces := rapid.SampledFrom([]string{" ", "  ", "\n", "\t", " ", " \n ", "\r\n"})
	rapid.Check(t, func(t *rapid.T) {
		words := rapid.SliceOfN(asciiWord, 1, 10).Draw(t, "words")
		text := "Before. " + strings.Join(words, " ") + " After."
		var q strings.Builder
		for k, w := range words {
			if k > 0 {
				q.WriteString(spaces.Draw(t, "space"))
			}
			switch rapid.IntRange(0, 2).Draw(t, "case") {
			case 0:
				w = strings.ToUpper(w)
			case 1:
				w = strings.ToLower(w)
			}
			q.WriteString(w)
		}
		quote := spaces.Draw(t, "lead") + q.String() + spaces.Draw(t, "trail")
		if !Find(Normalise(text), quote) {
			t.Fatalf("%q not found in %q", quote, text)
		}
	})
}

func TestQuoteMarksAndDashes(t *testing.T) {
	text := Normalise(`He said “it’s fine” — then left.`)
	for _, q := range []string{`"it's fine" - then`, `“IT’S FINE”`, `said "it's`, "it​'s fine"} {
		if !Find(text, q) {
			t.Errorf("%q not found in %q", q, text)
		}
	}
	if Find(text, "it is fine") || Find(text, "") || Find(text, " ​ ") {
		t.Error("found what is not there")
	}
}

func TestInlineStyleCascade(t *testing.T) {
	hidden := []string{
		`display:none ! important`, `display:none!IMPORTANT`, `display:block; display:none`,
		`display:none !important; display:block`, `color:#0000`, `color:#ff000000`, `color:rgba(0,0,0,0)`,
		`color:rgb(0 0 0 / 0%)`, `color:hsla(0, 0%, 0%, 0)`, `font-size:1px`, `font-size:0.05em`, `font-size:5%`,
		`opacity:0.01`, `filter:opacity(0)`, `transform:scale(0)`, `transform:translateX(1px) scaleY(0)`,
		`position:absolute;clip:rect(0 0 0 0)`, `position:fixed;clip:rect(0,0,0,0)`, `clip-path:inset(50%)`, `clip-path:circle(0)`,
		`height:0;overflow:hidden`, `max-width:0px;overflow-x:hidden`, `text-indent:-9999px`,
		`position:absolute;left:-10000px`, `position:fixed;top:-5000px`, `display:/* x */none`,
		// An invalid declaration is dropped, so it overrides nothing.
		`display:none;display:bogus`, `display:none;display:inline block extra more`,
		// Leaning toward hiding: a value these rules cannot read hides,
		// though a browser shows these.
		`display:none\3b color:red`, `display:n/**/one`, `font-size:\30`, `clip-path:polygon(0 0, 1px 0, 0 1px)`,
		`color:transparent;color:bogus`, `opacity:0;opacity:1px`,
		`height:0;overflow:visible;overflow-y:hidden`, `scale:1 0`, `clip-path:inset(50% 0)`,
		`font-size:0.01in`, `DISPLAY:NONE`, `display:\6e one`,
	}
	for _, style := range hidden {
		if !HiddenStyle(style) {
			t.Errorf("%s not hidden", style)
		}
	}
	shown := []string{
		`display:none; display:inline`, `display:block`, `font-size:10px`, `font-size:1em`, `opacity:1`,
		`opacity:0.5`, `color:red`, `color:#000`, `color:rgba(0,0,0,0.5)`, `transform:scale(1.2)`,
		`height:0`, `overflow:hidden;height:10px`, `text-indent:-1em`, `left:-10000px`, `display`,
		// Each of these is read differently by a browser than by naive
		// splitting: comments split tokens, an escaped "!" is not syntax,
		// a string holds its semicolons.
		`display:none\!important;display:block`, `--x:";display:none;"`,
		`content:"x;display:none"`, `font-size:1in`,
		`height:0;overflow:hidden;overflow-y:visible;overflow-x:visible`, `transform:scale3d(1,1,0)`,
		`clip:rect(0,0,0,0)`, `display:none;display:inline !important`,
	}
	for _, style := range shown {
		if HiddenStyle(style) {
			t.Errorf("%s hidden", style)
		}
	}
}

func TestHiddenOverrideShowsText(t *testing.T) {
	p := page(t, `<p>We <span style="display:none;display:inline">do not </span>approve.</p>`, "https://example.com/")
	if p.Text != "We do not approve." {
		t.Fatalf("%q", p.Text)
	}
}

func TestInertTitleAndBaseIgnored(t *testing.T) {
	body := `<html><head><template><title>Forged</title><base href="https://evil.example.org/"></template>
<title>Actual</title></head><body><svg><title>svg title</title></svg><a href="/x">x</a><title>body title</title></body></html>`
	p := page(t, body, "https://example.com/")
	if p.Title != "Actual" || p.Text != "x <https://example.com/x>" {
		t.Fatalf("%q %q", p.Title, p.Text)
	}
}

func TestVisibleHasNoNotes(t *testing.T) {
	p := page(t, `<h2>Head</h2><ul><li><a href="https://example.com/?claim=APPROVED">read</a> <img alt="APPROVED"></li></ul><table><tr><td>a</td><td>b</td></tr></table>`, "https://example.com/")
	if strings.Contains(p.Visible, "APPROVED") || strings.Contains(p.Visible, "#") || strings.Contains(p.Visible, "|") {
		t.Fatalf("visible %q", p.Visible)
	}
	if !strings.Contains(p.Text, "APPROVED") || !strings.Contains(p.Visible, "read") || !strings.Contains(p.Visible, "a\x00b") {
		t.Fatalf("text %q visible %q", p.Text, p.Visible)
	}
}

// A small document cannot make a large text, or take long to read.
func TestExtractionBounded(t *testing.T) {
	cases := map[string]string{
		"long base":  `<base href="https://example.com/` + strings.Repeat("a", 1_000_000) + `">` + strings.Repeat(`<a href="?x">x</a>`, 100_000),
		"long hrefs": strings.Repeat(`<a href="https://example.com/`+strings.Repeat("b", 3000)+`">x</a>`, 1000),
		"comments":   `<p style="color:red;` + strings.Repeat("/**/", 200_000) + `">c</p>`,
		"deep":       strings.Repeat("<div>", 200_000) + "deep text",
		"deep b":     strings.Repeat("<b>", 200_000) + "deep text",
		"deep a":     strings.Repeat("<a href=x>", 200_000) + "deep text",
		"deep table": strings.Repeat("<table><tr><td>", 50_000) + "deep text",
		"links":      strings.Repeat(`<a href="/`+strings.Repeat("c", 1500)+`">x</a>`, 50_000),
	}
	for name, body := range cases {
		start := time.Now()
		p := HTML(context.Background(), []byte(body), &url.URL{Scheme: "https", Host: "example.com", Path: "/"}, 100_000)
		if n := utf8.RuneCountInString(p.Text); n > 110_000 {
			t.Errorf("%s: %d runes", name, n)
		}
		if d := time.Since(start); d > 20*time.Second {
			t.Errorf("%s: took %s", name, d)
		}
		if name == "deep" && (!p.Deep || p.Text != "") {
			t.Errorf("deep: %v %q", p.Deep, p.Text)
		}
	}
}

func TestLimitCuts(t *testing.T) {
	p := HTML(context.Background(), []byte(strings.Repeat("<p>word</p>", 1000)), nil, 100)
	if !p.Cut || utf8.RuneCountInString(p.Text) > 110 {
		t.Fatalf("%v %d", p.Cut, len(p.Text))
	}
	if q := Plain([]byte(strings.Repeat("é", 50)), 10); !q.Cut || q.Text != strings.Repeat("é", 10) || q.Visible != q.Text {
		t.Fatalf("%+v", q)
	}
	title := HTML(context.Background(), []byte("<title>"+strings.Repeat("t", 5000)+"</title>"), nil, 100)
	if utf8.RuneCountInString(title.Title) != MaxTitleRunes {
		t.Fatalf("title %d", len(title.Title))
	}
}

func TestNotesLeaveVisibleWhitespace(t *testing.T) {
	p := page(t, `<p><a href="/x">no</a>where and <a href="/y">not</a>able, <img alt="i">x</p>`, "https://example.com/")
	if p.Visible != "nowhere and notable,\x00x" {
		t.Fatalf("visible %q", p.Visible)
	}
}

func TestEmptyFirstTitleIsTheTitle(t *testing.T) {
	p := page(t, `<title></title><title>FORGED</title><p>Actual</p>`, "https://example.com/")
	if p.Title != "" || strings.Contains(p.Text+p.Visible, "FORGED") {
		t.Fatalf("%+v", p)
	}
}

func TestClosedDetailsShowOnlyTheSummary(t *testing.T) {
	p := page(t, `<details><summary>Details</summary>FORGED<p>more</p></details><details open><summary>S</summary>shown</details>`, "https://example.com/")
	if strings.Contains(p.Text, "FORGED") || strings.Contains(p.Text, "more") || !strings.Contains(p.Text, "Details") ||
		!strings.Contains(p.Text, "shown") || p.Hidden != len("FORGED")+len("more") {
		t.Fatalf("%+v", p)
	}
}

func TestLimitIsHard(t *testing.T) {
	base := &url.URL{Scheme: "https", Host: "example.com", Path: "/"}
	for _, body := range []string{
		"<p>" + strings.Repeat("x", 200) + "</p>",
		"<pre>" + strings.Repeat("line\n", 100) + "</pre>",
		strings.Repeat(`<a href="/`+strings.Repeat("d", 1000)+`">x</a>`, 50) + "<p>" + strings.Repeat("y", 500) + "</p>",
	} {
		p := HTML(context.Background(), []byte(body), base, 100)
		if n := utf8.RuneCountInString(p.Text); n > 100 || !p.Cut {
			t.Errorf("%d runes, cut %v", n, p.Cut)
		}
		if n := utf8.RuneCountInString(p.Visible); n > 100 {
			t.Errorf("visible %d runes", n)
		}
	}
}

// Link targets fill the agent's text but not the visible text, so a quote
// at the end of a page full of links is still found.
func TestNotesDoNotCrowdOutVisible(t *testing.T) {
	body := `<base href="https://example.com/` + strings.Repeat("p", 1900) + `/">` +
		strings.Repeat(`<a href="?x">x</a>`, 2700) + `<p>ACTUAL QUOTE</p>`
	p := HTML(context.Background(), []byte(body), &url.URL{Scheme: "https", Host: "example.com", Path: "/"}, len(body))
	if !p.Cut || p.VisibleCut || !strings.HasSuffix(p.Visible, "ACTUAL QUOTE") {
		t.Fatalf("cut %v visible cut %v, visible ends %q", p.Cut, p.VisibleCut, p.Visible[max(0, len(p.Visible)-20):])
	}
}

func TestCSSLinear(t *testing.T) {
	for _, style := range []string{
		"transform:" + strings.Repeat("scale(", 200_000) + "1",
		"color:red;" + strings.Repeat("/**/", 200_000),
		strings.Repeat("(", 1_000_000),
		strings.Repeat("a:b;", 200_000) + "display:none",
		strings.Repeat(`\`, 500_000),
		strings.Repeat(`"`, 500_000),
	} {
		start := time.Now()
		HiddenStyle(style)
		if d := time.Since(start); d > 2*time.Second {
			t.Errorf("%.20q...: %s", style, d)
		}
	}
	if !HiddenStyle(strings.Repeat("a:b;", 200_000) + "display:none") {
		t.Error("the last declaration was lost")
	}
}

func TestExtractionStopsWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := HTML(ctx, []byte(strings.Repeat("<p>word</p>", 10_000)), nil, 1<<20)
	if strings.Count(p.Text, "word") > 300 {
		t.Fatalf("read %d paragraphs after cancellation", strings.Count(p.Text, "word"))
	}
}

// Round 3: tokenizer and validation cases checked against Chrome, plus
// the cases where these rules hide on purpose.
func TestTokenizerAgainstBrowser(t *testing.T) {
	deep := strings.Repeat("(", 33) + "x" + strings.Repeat(")", 33)
	hidden := []string{
		`opacity:0;opacity:1.`, `scale:1.0.0`, `display:none;display:bloc` + "K",
		`display:no\6e` + "\f" + `e`, `--x:"` + "\f" + `;display:none`,
		`display:none;display:block block`, `display:none;display:calc(1)`,
		`transform:scale(0);transform:translateX(bogus)`, `color:transparent;color:rgb(bogus)`,
		`--x:` + deep + `;display:none`,
		// Leaning toward hiding: Chrome shows these, these rules do not.
		`display:none;--x:` + deep + `;display:block`, `transform:scale(0,1,1)`, `clip-path:circle(0) garbage`,
		`color:rgba(0,0,0,0.05)`, `color:#0001`, `color:#00000019`, `filter:opacity(-1)`, `scale:calc(1)`,
	}
	for _, style := range hidden {
		if !HiddenStyle(style) {
			t.Errorf("%q not hidden", style)
		}
	}
	shown := []string{
		`opacity:0;opacity:1e999`, `display:none;--x:url(a(;x);display:block`, `scale:initial`, `scale:unset`,
		`display:none;all:initial`, `display:none !important;all:initial !important`,
		`font-size:0;font:16px serif`, `font-size:0;font:italic bold 12px/30px Georgia`, `font-size:0;font:menu`,
		`color:rgba(0,0,0,0.5)`, `color:#000a`,
	}
	for _, style := range shown {
		if HiddenStyle(style) {
			t.Errorf("%q hidden", style)
		}
	}
}

func TestVisibilityAndColourInherit(t *testing.T) {
	p := page(t, `<p>We <span style="visibility:hidden">x <span style="visibility:visible">do not </span>y </span>approve.</p>`+
		`<p>A <span style="color:transparent">z <b style="color:black">still </b></span>here.</p>`, "https://example.com/")
	if p.Text != "We do not approve.\n\nA still here." || p.Hidden != len("xyz") {
		t.Fatalf("%q hidden %d", p.Text, p.Hidden)
	}
}

func TestHiddenAttributeYieldsToDisplay(t *testing.T) {
	p := page(t, `<p>We <span hidden style="display:inline">do not </span>approve <span hidden style="display:none;color:red">x</span><span hidden>y</span>.</p>`, "https://example.com/")
	if p.Text != "We do not approve ." {
		t.Fatalf("%q", p.Text)
	}
}

// The visible text breaks wherever something was hidden, replaced, moved
// or reordered, so no quote can run across.
func TestVisibleBreaks(t *testing.T) {
	for _, body := range []string{
		`<p>We <span style="display:none">x</span>approve.</p>`,
		`<p>We <span aria-hidden="true">do not </span>approve.</p>`,
		`<p>We <img src="x.png">approve.</p>`,
		`<p>We <input value="do not">approve.</p>`,
		`<p>We <span><template shadowrootmode="open">do not </template></span>approve.</p>`,
		`<p>We <span style="position:absolute">do not</span> approve.</p>`,
		`<p>We <span style="float:left">x</span> approve.</p>`,
		`<p style="display:flex;flex-direction:row-reverse"><span>We</span> <span>approve.</span></p>`,
		`<p>We <span style="order:-1">x</span> approve.</p>`,
		`<p>We <span style="visibility:hidden">x</span> approve.</p>`,
		`<p>We <svg><text>do not</text></svg> approve.</p>`,
		`<p>We <button>do not</button> approve.</p>`,
		`<p>We <span style="transform:translateX(-50px)">x</span> approve.</p>`,
	} {
		p := page(t, body, "https://example.com/")
		if FindIn(Segments(p.Visible), "We approve.") {
			t.Errorf("quote found across %q from %s", p.Visible, body)
		}
	}
	// And nothing breaks ordinary text.
	p := page(t, `<p>We <b>do</b> <a href="/x">not</a> <span style="color:red">approve</span>.</p>`, "https://example.com/")
	if p.Visible != "We do not approve." {
		t.Errorf("%q", p.Visible)
	}
}

func TestBidiOverrideUnchecked(t *testing.T) {
	p := page(t, `<p>We <bdo dir="rtl">evorppa</bdo> and <span style="unicode-bidi:bidi-override;direction:rtl">DEGROF</span></p>`, "https://example.com/")
	if strings.Contains(p.Visible, "evorppa") || strings.Contains(p.Visible, "DEGROF") || !strings.Contains(p.Text, "DEGROF") {
		t.Fatalf("visible %q text %q", p.Visible, p.Text)
	}
	segs := Segments("ok \u202eDEGROF\u202c fine\u2067ABC")
	if FindIn(segs, "DEGROF") || FindIn(segs, "ABC") || !FindIn(segs, "fine") || !FindIn(segs, "ok") {
		t.Fatalf("%q", segs)
	}
}

func TestVarResolved(t *testing.T) {
	hidden := []string{`--o:0;opacity:var(--o)`, `opacity:var(--missing)`, `--x:none;display:var(--x)`,
		`opacity:calc(0)`, `left:var(--l);position:absolute;--l:-9999px`, `-webkit-text-fill-color:transparent`,
		`color:rgb(from transparent r g b)`, `--a:var(--a);opacity:var(--a)`}
	for _, style := range hidden {
		if !HiddenStyle(style) {
			t.Errorf("%q not hidden", style)
		}
	}
	shown := []string{`--o:1;opacity:var(--o)`, `opacity:var(--missing, 1)`, `color:var(--brand)`,
		`-webkit-text-fill-color:transparent;-webkit-text-fill-color:red`}
	for _, style := range shown {
		if HiddenStyle(style) {
			t.Errorf("%q hidden", style)
		}
	}
	p := page(t, `<div style="--o:0"><p>a <span style="opacity:var(--o)">b</span> c</p></div>`, "https://example.com/")
	if strings.Contains(p.Visible, "b") {
		t.Fatalf("inherited custom property ignored: %q", p.Visible)
	}
}

func TestStrictValidators(t *testing.T) {
	for _, style := range []string{
		`color:transparent;color:rgb(1)`, `transform:scale(0);transform:translateX(1px,2px)`,
		`filter:opacity(0);filter:blur(1px,2px)`, `font-size:0;font:16px`,
		`position:absolute;clip:rect(0,0,0,0);clip:rect(1px)`, `display:none;display:-ms-flexbox`,
		`display:none;--x:url(a\);display:block;)`, `content:url(x.png)`,
	} {
		if !HiddenStyle(style) {
			t.Errorf("%q not hidden", style)
		}
	}
	if p := page(t, `<p hidden style="display:-ms-flexbox">FORGED</p>`, "https://example.com/"); strings.Contains(p.Text, "FORGED") {
		t.Errorf("%q", p.Text)
	}
	if p := page(t, `<p style="visibility:hidden"><img alt="FORGED"><a href="https://evil.example/">x</a></p>`, "https://example.com/"); strings.Contains(p.Text, "FORGED") || strings.Contains(p.Text, "evil") {
		t.Errorf("note under hidden text: %q", p.Text)
	}
}

func TestStyleMemoryBounded(t *testing.T) {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	HTML(context.Background(), []byte(`<p style="`+strings.Repeat(" ", 4_999_900)+`">x</p>`), nil, 5_000_000)
	runtime.ReadMemStats(&after)
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 200<<20 {
		t.Fatalf("allocated %d MB", grew>>20)
	}
	if !HiddenStyle(strings.Repeat(" ", maxStyleBytes+1) + "color:red") {
		t.Fatal("an oversized style was read")
	}
}

func TestVarExpansionBounded(t *testing.T) {
	var b strings.Builder
	b.WriteString("--a0:x x;")
	for i := 1; i < 30; i++ {
		fmt.Fprintf(&b, "--a%d:var(--a%d) var(--a%d);", i, i-1, i-1)
	}
	b.WriteString("opacity:var(--a29)")
	start := time.Now()
	if !HiddenStyle(b.String()) {
		t.Error("an over-expanded var() did not hide")
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("took %s", d)
	}
	if !HiddenStyle(`--a:var(--b);--b:var(--a);opacity:var(--a)`) {
		t.Error("a var() cycle did not hide")
	}
}

// The agent's text drops these too (round 6, checked against Chrome).
func TestRound6HiddenFromAgent(t *testing.T) {
	for _, body := range []string{
		`<p style="color:transparent;color:rgb(0deg 0 0)">FORGED</p>`,
		`<p style="font-size:0;font:italic italic 16px serif">FORGED</p>`,
		`<p style="font-size:0;font:16px/-1 serif">FORGED</p>`,
		`<p style="font-size:0;font:16px serif,">FORGED</p>`,
		`<p style="position:absolute;clip:rect(0,0,0,0);clip:rect(10%,100px,100px,0px)">FORGED</p>`,
		`<p style="color:transparent;color:lab(0,0,0)">FORGED</p>`,
		`<p style="color:transparent;color:rgb(0 0 / 0)">FORGED</p>`,
		`<p style="transform:scale(0);transform:translateZ(10%)">FORGED</p>`,
		`<p style="--a:var(--missing);color:var(--a,transparent)">FORGED</p>`,
		`<p style="--a:var(--b);--b:var(--a);color:var(--a,transparent)">FORGED</p>`,
		`<p style="color:rgb(0 0 0 / none)">FORGED</p>`,
		`<p style="color:color(srgb 0 0 0 / 0)">FORGED</p>`,
		`<p style="color:rgb(from red r g b / 0)">FORGED</p>`,
		`<div style="-webkit-text-fill-color:black"><p style="color:transparent;-webkit-text-fill-color:currentcolor">FORGED</p></div>`,
		`<div style="-webkit-text-fill-color:black"><p style="color:transparent;-webkit-text-fill-color:initial">FORGED</p></div>`,
		`<p style="mask-image:linear-gradient(transparent,transparent)">FORGED</p>`,
		`<p style="-webkit-text-security:disc">FORGED</p>`,
		`<p style="font:16px/16px monospace;clip-path:inset(10px)">FORGED</p>`,
		`<p style="clip-path:circle(1px at -100px -100px)">FORGED</p>`,
		`<p style="position:relative;left:-1000px">FORGED</p>`,
		`<p style="text-indent:-100px;overflow:hidden">FORGED</p>`,
		`<div style="opacity:.1"><div style="opacity:.1"><p style="opacity:.1">FORGED</p></div></div>`,
		`<div style="font-size:2px"><p style="font-size:10%">FORGED</p></div>`,
		`<details name="a" open><summary>One</summary>shown</details><details name="a" open><summary>Two</summary>FORGED</details>`,
		`<html xmlns:h="http://www.w3.org/1999/xhtml"><body><h:style>FORGED</h:style></body></html>`,
	} {
		if p := page(t, body, "https://example.com/"); strings.Contains(p.Text, "FORGED") {
			t.Errorf("%s gave %q", body, p.Text)
		}
	}
	// And these still show.
	for _, body := range []string{
		`<p style="font:italic bold 16px/1.5 Georgia, 'Times New Roman', serif">SHOWN</p>`,
		`<p style="color:rgb(10, 20, 30)">SHOWN</p>`, `<p style="color:hsl(120deg 50% 50% / 1)">SHOWN</p>`,
		`<div style="opacity:.5"><p style="opacity:.5">SHOWN</p></div>`, `<p style="font-size:0.8em">SHOWN</p>`,
	} {
		if p := page(t, body, "https://example.com/"); !strings.Contains(p.Text, "SHOWN") {
			t.Errorf("%s gave %q", body, p.Text)
		}
	}
}

// Custom properties built from each other on nested elements cannot
// grow without bound: a 30-deep chain where each doubles the last.
func TestNestedVarExpansionBounded(t *testing.T) {
	var b strings.Builder
	b.WriteString(`<div style="--a0:x">`)
	for i := 1; i < 30; i++ {
		fmt.Fprintf(&b, `<div style="--a%d:f(var(--a%d) var(--a%d))">`, i, i-1, i-1)
	}
	b.WriteString(`<p style="opacity:var(--a29)">FORGED</p>`)
	start := time.Now()
	p := page(t, b.String(), "https://example.com/")
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("took %s", d)
	}
	if strings.Contains(p.Text, "FORGED") {
		t.Fatalf("an unknown opacity showed its text: %q", p.Text)
	}
}

func TestUnmatchedPDILinear(t *testing.T) {
	s := strings.Repeat("‮", 200_000) + strings.Repeat("⁩", 200_000)
	start := time.Now()
	Segments(s)
	if d := time.Since(start); d > time.Second {
		t.Fatalf("took %s", d)
	}
}

// The reviewer's construction: many elements deep, each setting many
// custom properties. Scopes are linked and capped, so memory stays small.
func TestCustomPropertiesMemoryBounded(t *testing.T) {
	name := func(n int) string { return fmt.Sprintf("--p%d", n) }
	build := func(depth, props int) []byte {
		var b strings.Builder
		n := 0
		for d := 0; d < depth; d++ {
			b.WriteString(`<span style="`)
			for p := 0; p < props; p++ {
				fmt.Fprintf(&b, "%s:0;", name(n))
				n++
			}
			b.WriteString(`">`)
		}
		b.WriteString("x")
		return []byte(b.String())
	}
	for _, c := range []struct{ depth, props int }{{150, 1800}, {400, 2}, {200, 5}} {
		body := build(c.depth, c.props)
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		HTML(context.Background(), body, nil, len(body))
		runtime.ReadMemStats(&after)
		if grew := after.TotalAlloc - before.TotalAlloc; grew > 64<<20+uint64(len(body))*40 {
			t.Errorf("depth %d, %d each: allocated %d MB for a %d KB body", c.depth, c.props, grew>>20, len(body)>>10)
		}
	}
	// Past the cap the element is hidden.
	var b strings.Builder
	b.WriteString(`<p style="`)
	for i := 0; i <= maxCustomProperties; i++ {
		fmt.Fprintf(&b, "%s:0;", name(i))
	}
	b.WriteString(`">FORGED</p>`)
	if p := page(t, b.String(), "https://example.com/"); strings.Contains(p.Text, "FORGED") {
		t.Fatal("an element past the custom property cap was read")
	}
}

// Adjacent text runs cost the parser the square of their number, so a
// document is parsed only to MaxTokens. Text between start tags, end
// tags, comments, doctypes and bogus comments is cut, and each finishes
// well inside the default 20 second fetch limit.
func TestParserTokensBounded(t *testing.T) {
	for _, pat := range []string{"<td>x", "</z>x", "<tr>x", "x<td>", "</z>xx", "<!--c-->x", "<!doctype html>x", "<?x>x", "<!x>x"} {
		body := []byte(strings.Repeat(pat, 5_000_000/len(pat)))
		start := time.Now()
		p := HTML(context.Background(), body, nil, len(body))
		if d := time.Since(start); d > 5*time.Second {
			t.Errorf("%q: took %s", pat, d)
		}
		if !p.TooManyTokens {
			t.Errorf("%q: a document past MaxTokens was not marked", pat)
		}
	}
	if b, cut := withinTokens([]byte("<p>a</p><p>b</p>"), 4); !cut || string(b) != "<p>a</p>" {
		t.Fatalf("%q %v", b, cut)
	}
	if _, cut := withinTokens([]byte("<p>a</p>"), 4); cut {
		t.Fatal("a document of exactly the limit was cut")
	}
}

func TestRefreshesSoon(t *testing.T) {
	for in, want := range map[string]bool{"0": true, "5; url=x": true, "3,https://x": true, "6": false, "60;url=x": false, "": true, "x": false} {
		if got := RefreshesSoon(in); got != want {
			t.Errorf("%q: %v", in, got)
		}
	}
}

func TestUncheckablePageHasNoVisibleText(t *testing.T) {
	p := page(t, `<div style="position:fixed"></div><p>words</p>`, "https://example.com/")
	if p.Uncheckable == "" || p.Visible != "" || !strings.Contains(p.Text, "words") {
		t.Fatalf("%+v", p)
	}
}

// The reviewer's construction: one large custom property used many times
// by many elements. One budget per element keeps each small.
func TestVarAmplificationPerElement(t *testing.T) {
	x := strings.TrimSpace(strings.Repeat("a ", 2000))
	decl := strings.Repeat("b:var(--x);", (16<<10)/11-1)
	var b strings.Builder
	fmt.Fprintf(&b, `<div style="--x: %s">`, x)
	for range 20 {
		fmt.Fprintf(&b, `<span style="%s">w</span>`, decl)
	}
	b.WriteString("</div>")
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	start := time.Now()
	HTML(context.Background(), []byte(b.String()), nil, 1<<20)
	runtime.ReadMemStats(&after)
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("took %s", d)
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 256<<20 {
		t.Errorf("allocated %d MB", grew>>20)
	}
}

// Differential: for random fragments mixing foreign content, raw text,
// CDATA and comments, the page is uncheckable exactly when the tree
// x/net/html builds holds the covering element. This proves the overlay
// rule reads every element the tree builder makes, wherever it puts it
// (in foreign content, a template, a breakout from SVG), and nothing it
// did not make (text in a comment or HTML raw text). It does not prove the
// tree builder agrees with a browser; that is x/net/html's job.
func TestOverlayFollowsTheTree(t *testing.T) {
	pieces := []string{
		"<svg>", "</svg>", "<math>", "</math>", "<style>", "</style>", "<title>", "</title>",
		"<textarea>", "</textarea>", "<script>", "</script>", "<![CDATA[", "]]>", "<!--", "-->",
		"<foreignObject>", "</foreignObject>", "<template>", "</template>", "<p>x</p>", "<xmp>", "</xmp>",
		"<noscript>", "</noscript>", "<desc>", "<mi>", "<annotation-xml encoding=text/html>", ">", `"`,
		`<div style="position:fixed">c</div>`,
	}
	rapid.Check(t, func(t *rapid.T) {
		var b strings.Builder
		for _, p := range rapid.SliceOfN(rapid.SampledFrom(pieces), 1, 12).Draw(t, "pieces") {
			b.WriteString(p)
		}
		body := b.String()
		doc, err := html.Parse(strings.NewReader(body))
		if err != nil {
			t.Skip()
		}
		inTree := false
		walkNodes(doc, func(*html.Node) bool { return false }, func(n *html.Node) {
			if v, ok := attr(n, "style"); ok && n.Type == html.ElementNode && strings.Contains(v, "position:fixed") {
				inTree = true
			}
		})
		got := HTML(context.Background(), []byte(body), nil, 1<<20).Uncheckable != ""
		if got != inTree {
			t.Fatalf("%q: uncheckable %v, covering element in the tree %v", body, got, inTree)
		}
	})
}

// Compact keeps whitespace-class characters apart as spaces, and Plain
// ends a line at a lone CR or form feed, so the words either side never
// join into a quote.
func TestCompactKeepsWordsApart(t *testing.T) {
	for _, body := range []string{
		"<p>foo bar</p>", "<p>foo bar</p>", "<p>foo\u0085bar</p>", "<p>foo\vbar</p>",
		"<pre>foo&#13;bar</pre>", "<pre>foo&#12;bar</pre>", "<p>foo&#13;bar</p>", "<p>foo&#x2028;bar</p>",
	} {
		p := Read(context.Background(), []byte(body), "text/html", nil, 1000, 1000)
		if FindIn(Segments(p.Visible), "foobar") {
			t.Errorf("%q: foobar found in %q", body, p.Visible)
		}
	}
	for _, body := range []string{"foo\rbar", "foo\fbar", "foo\vbar", "foo bar"} {
		p := Read(context.Background(), []byte(body), "text/plain", nil, 1000, 1000)
		if FindIn(Segments(p.Visible), "foobar") {
			t.Errorf("plain %q: foobar found in %q", body, p.Visible)
		}
	}
	if p := Read(context.Background(), []byte("<p>foo​bar</p>"), "text/html", nil, 1000, 1000); !FindIn(Segments(p.Visible), "foobar") {
		t.Error("a zero-width space split a word")
	}
}

// A vertical tab, NEL, and the line and paragraph separators are breaks:
// no quote runs across one, in the page's text or its title. NEL and the
// paragraph separator end the paragraph, so a bidi override before them
// does not hide the text after; the others do not.
func TestSeparatorsBreak(t *testing.T) {
	for _, sep := range []string{"\v", "\u0085", "\u2028", "\u2029"} {
		p := Read(context.Background(), []byte("<p>foo"+sep+"bar</p>"), "text/html", nil, 1000, 1000)
		if FindIn(Segments(p.Visible), "foo bar") {
			t.Errorf("%q: a quote ran across it in %q", sep, p.Visible)
		}
		ends := sep == "\u0085" || sep == "\u2029"
		p = Read(context.Background(), []byte("<p>&#x202E;evil"+sep+"The drug is safe.</p>"), "text/html", nil, 1000, 1000)
		if got := FindIn(Segments(p.Visible), "The drug is safe."); got != ends {
			t.Errorf("%q: after an override, found %v in %q", sep, got, p.Visible)
		}
	}
	for _, sep := range []string{"\v", "\u0085", "\u2028", "\u2029"} {
		if FindIn(Segments("foo"+sep+"bar"), "foo bar") {
			t.Errorf("title %q: a quote ran across it", sep)
		}
	}
}

// Fail closed: an open dialog, a motion path, and MathML that pads or
// paints make the page uncheckable.
func TestRound11Uncheckable(t *testing.T) {
	for _, body := range []string{
		`<p>The drug is safe.</p><dialog open>cover</dialog>`,
		`<p>The drug is safe.</p><div style="offset-path:path('M 0 0')">c</div>`,
		`<p>The drug is safe.</p><div style="offset-distance:10px">c</div>`,
		`<p>The drug is safe.</p><math><mpadded voffset="40px"><mtext>Covered here</mtext></mpadded></math>`,
		`<p>The drug is safe.</p><math><mi mathbackground="white">x</mi></math>`,
	} {
		if p := page(t, body, "https://example.com/"); p.Uncheckable == "" {
			t.Errorf("%s is checkable", body)
		}
	}
	if p := page(t, `<p>The drug is safe.</p><dialog>closed</dialog><math><mi>x</mi></math>`, "https://example.com/"); p.Uncheckable != "" {
		t.Errorf("a closed dialog or plain MathML: %q", p.Uncheckable)
	}
}

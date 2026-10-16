package check

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"

	"golang.org/x/net/idna"

	"github.com/kirahowe/webtool/internal/envelope"
	"github.com/kirahowe/webtool/internal/fetch"
	"github.com/kirahowe/webtool/internal/guard"
	"github.com/kirahowe/webtool/internal/text"
)

func input(t *testing.T, s string) map[string]json.RawMessage {
	t.Helper()
	m, err := envelope.Object([]byte(s), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestParseInput(t *testing.T) {
	items, err := ParseInput(input(t, `{"items": [{"url": "https://a.com/", "quotes": ["x", "y"]}, {"url": "https://b.com/"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(items, []Item{{"https://a.com/", []string{"x", "y"}}, {"https://b.com/", nil}}) {
		t.Fatalf("%+v", items)
	}
	for _, bad := range []string{
		`{}`, `{"items": []}`, `{"items": {}}`, `{"items": [{}]}`, `{"items": [{"url": 1}]}`,
		`{"items": [{"url": ""}]}`, `{"items": [{"url": "u", "quotes": "x"}]}`, `{"items": [{"url": "u", "quotes": [1]}]}`,
		`{"items": [{"url": "u", "URL": "v"}]}`, `{"items": [{"url": "u"}], "extra": 1}`,
		`{"items": [{"url": "u", "url": "v"}]}`,
	} {
		m, err := envelope.Object([]byte(bad), nil, nil)
		if err != nil {
			continue // refused even earlier
		}
		if _, err := ParseInput(m); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

func TestHost(t *testing.T) {
	lookalike, _ := idna.ToASCII("аpple.com") // Cyrillic а, then Latin
	whole, _ := idna.ToASCII("аррӏе.com")     // all Cyrillic, Latin TLD
	jp, _ := idna.ToASCII("例えば.jp")
	cases := []struct {
		host         string
		punycode     bool
		unicode      string
		scripts      []string
		mixedScripts bool
	}{
		{"example.com", false, "example.com", []string{"Latin"}, false},
		{lookalike, true, "аpple.com", []string{"Cyrillic", "Latin"}, true},
		{whole, true, "аррӏе.com", []string{"Cyrillic", "Latin"}, false},
		{jp, true, "例えば.jp", []string{"Han", "Hiragana", "Latin"}, false},
		{"93.184.215.14", false, "93.184.215.14", []string{}, false},
	}
	for _, c := range cases {
		h := Host(c.host)
		if h.Punycode != c.punycode || h.Unicode != c.unicode || !slices.Equal(h.Scripts, c.scripts) || h.MixedScripts != c.mixedScripts {
			t.Errorf("%s: %+v", c.host, h)
		}
	}
}

func TestDomain(t *testing.T) {
	for host, want := range map[string]string{
		"www.bbc.co.uk": "bbc.co.uk", "a.b.example.com": "example.com", "example.com": "example.com",
		"co.uk": "co.uk", "93.184.215.14": "93.184.215.14", "foo.github.io": "foo.github.io",
		"例え.jp": "xn--r8jz45g.jp",
	} {
		if got := Domain(host); got != want {
			t.Errorf("%s: %s, want %s", host, got, want)
		}
	}
}

func result(final string, hops []fetch.Hop, body string) *fetch.Result {
	return &fetch.Result{Status: 200, FinalURL: final, Redirects: hops, SHA256: "abc", Page: text.Page{Text: body, Visible: body, Hidden: 3}}
}

func TestDescribe(t *testing.T) {
	it := Item{URL: "https://news.example.com/a", Quotes: []string{"the “quick” brown", "THE \"QUICK\"", "absent", "quick”​ brown", ""}}
	res := result("https://www.example.com/b", []fetch.Hop{{URL: it.URL, Status: 301}}, `The "quick"  brown fox.`)
	rep := Describe(it, res, nil)
	if !rep.Fetched || rep.Error != "" || rep.Status != 200 || rep.CrossesDomains || rep.Domain != "example.com" ||
		rep.FinalDomain != "example.com" || rep.SHA256 != "abc" || rep.HiddenChars != 3 {
		t.Fatalf("%+v", rep)
	}
	want := []QuoteReport{
		{"the “quick” brown", true, false},
		{`THE "QUICK"`, true, false},
		{"absent", false, false},
		{`quick”\u{200B} brown`, true, true},
		{"", false, false},
	}
	if !reflect.DeepEqual(rep.Quotes, want) {
		t.Fatalf("%+v", rep.Quotes)
	}
}

func TestCrossDomainRedirect(t *testing.T) {
	cases := []struct {
		hops  []fetch.Hop
		final string
		want  bool
	}{
		{nil, "https://a.example.com/", false},
		{[]fetch.Hop{{URL: "https://a.example.com/", Status: 302}}, "https://b.example.com/", false},
		{[]fetch.Hop{{URL: "https://a.example.com/", Status: 302}}, "https://example.org/", true},
		// Out and back again still crossed.
		{[]fetch.Hop{{URL: "https://a.example.com/", Status: 302}, {URL: "https://t.co/x", Status: 301}}, "https://example.com/", true},
		{[]fetch.Hop{{URL: "https://a.example.com/", Status: 302}}, "https://a.example.co.uk/", true},
	}
	for _, c := range cases {
		rep := Describe(Item{URL: "https://a.example.com/"}, result(c.final, c.hops, ""), nil)
		if rep.CrossesDomains != c.want {
			t.Errorf("%v -> %s: %v", c.hops, c.final, rep.CrossesDomains)
		}
	}
}

func TestFailedFetchFindsNothing(t *testing.T) {
	it := Item{URL: "http://10.0.0.1/", Quotes: []string{"x"}}
	rep := Describe(it, &fetch.Result{URL: it.URL}, &guard.Refusal{Reason: "10.0.0.1 is a private address"})
	if rep.Fetched || rep.Error != "refused: 10.0.0.1 is a private address" || rep.Quotes[0].Found || rep.Domain != "10.0.0.1" || rep.FinalHost != nil {
		t.Fatalf("%+v", rep)
	}
}

type fakeFetcher map[string]string

func (f fakeFetcher) Fetch(_ context.Context, u string) (*fetch.Result, error) {
	body, ok := f[u]
	if !ok {
		return &fetch.Result{URL: u}, errors.New("could not connect")
	}
	return result(u, nil, body), nil
}

func TestRunKeepsOrder(t *testing.T) {
	f := fakeFetcher{"https://a.com/": "alpha", "https://c.com/": "gamma"}
	rep := Run(context.Background(), f, []Item{
		{"https://a.com/", []string{"alpha"}}, {"https://b.com/", []string{"beta"}}, {"https://c.com/", []string{"alpha"}},
	})
	got := []bool{rep.Items[0].Quotes[0].Found, rep.Items[1].Fetched, rep.Items[2].Quotes[0].Found}
	if !slices.Equal(got, []bool{true, false, false}) || rep.Items[1].Error != "could not connect" {
		t.Fatalf("%+v", rep)
	}
}

// The shape relay parses: every documented key, always present.
func TestShape(t *testing.T) {
	rep := Run(context.Background(), fakeFetcher{}, []Item{{"https://a.com/", []string{"q"}}})
	raw, _ := json.Marshal(rep)
	var m map[string][]map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for k := range m["items"][0] {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	want := []string{"complete", "crosses_domains", "domain", "error", "fetched", "final_domain", "final_host", "final_url",
		"hidden_chars", "host", "quotes", "redirects", "sha256", "status", "uncheckable", "url"}
	if !slices.Equal(keys, want) {
		t.Fatalf("keys %v", keys)
	}
	host := m["items"][0]["host"].(map[string]any)
	if len(host) != 5 {
		t.Fatalf("host %v", host)
	}
	quote := m["items"][0]["quotes"].([]any)[0].(map[string]any)
	if len(quote) != 3 {
		t.Fatalf("quote %v", quote)
	}
}

// Link targets and image descriptions are notes for the agent, not text
// on the page, so a quote found only in them is not found.
func TestNotesAreNotThePage(t *testing.T) {
	p := readHTML([]byte(`<a href="https://example.com/?claim=APPROVED">read</a> <img alt="SIGNED">`), &url.URL{Scheme: "https", Host: "example.com", Path: "/"}, 1<<20)
	res := &fetch.Result{Status: 200, FinalURL: "https://example.com/", Page: p}
	rep := Describe(Item{URL: "https://example.com/", Quotes: []string{"APPROVED", "SIGNED", "read"}}, res, nil)
	if rep.Quotes[0].Found || rep.Quotes[1].Found || !rep.Quotes[2].Found {
		t.Fatalf("%+v", rep.Quotes)
	}
}

func TestEveryFieldEscaped(t *testing.T) {
	bidi := "\u202e"
	res := &fetch.Result{Status: 302, Redirects: []fetch.Hop{{URL: "https://a.com/?" + bidi, Status: 302}}, LastURL: "https://b.com/" + bidi}
	rep := Describe(Item{URL: "https://a.com/?" + bidi, Quotes: []string{bidi}}, res, errors.New("refused: "+bidi))
	raw, _ := json.Marshal(rep)
	if strings.ContainsRune(string(raw), '\u202e') {
		t.Fatalf("raw bidi control in %s", raw)
	}
	// In the host itself, where IDNA fails and the domain falls back to
	// the name as given.
	rep = Describe(Item{URL: "https://ab" + bidi + ".com/"}, &fetch.Result{}, errors.New("refused"))
	raw, _ = json.Marshal(rep)
	if strings.ContainsRune(string(raw), '\u202e') || rep.Domain == "" {
		t.Fatalf("raw bidi control in %s", raw)
	}
	m, _ := envelope.Object([]byte(`{"`+bidi+`": 1}`), nil, nil)
	if _, err := ParseInput(m); err == nil || strings.ContainsRune(err.Error(), '\u202e') {
		t.Fatalf("%v", err)
	}
}

// A redirect counts even when its target failed before answering.
func TestCrossingToAFailedTarget(t *testing.T) {
	res := &fetch.Result{Redirects: []fetch.Hop{{URL: "https://a.example.com/", Status: 302}}, LastURL: "https://other.example.org/"}
	rep := Describe(Item{URL: "https://a.example.com/"}, res, errors.New("could not connect"))
	if !rep.CrossesDomains {
		t.Fatal("crossing to a failed target missed")
	}
}

func TestIdeographicFullStop(t *testing.T) {
	res := result("https://b.example.com/", []fetch.Hop{{URL: "https://a.example.com。/", Status: 302}}, "")
	rep := Describe(Item{URL: "https://a.example.com。/"}, res, nil)
	if rep.Domain != "example.com" || rep.CrossesDomains {
		t.Fatalf("%+v", rep)
	}
}

func TestComplete(t *testing.T) {
	for _, c := range []struct {
		page      text.Page
		truncated bool
		want      bool
	}{
		{text.Page{Visible: "x"}, false, true},
		{text.Page{Visible: "x", Cut: true}, false, true}, // only the agent's text was cut
		{text.Page{Visible: "x", VisibleCut: true}, false, false},
		{text.Page{Visible: "x", TitleCut: true}, false, false},
		{text.Page{Deep: true}, false, false},
		{text.Page{Visible: "x"}, true, false},
	} {
		res := &fetch.Result{Status: 200, FinalURL: "https://a.com/", Truncated: c.truncated, Page: c.page}
		if got := Describe(Item{URL: "https://a.com/"}, res, nil).Complete; got != c.want {
			t.Errorf("%+v truncated %v: complete %v", c.page, c.truncated, got)
		}
	}
	if Describe(Item{URL: "https://a.com/"}, &fetch.Result{}, errors.New("x")).Complete {
		t.Error("a failed fetch is complete")
	}
}

// Each page shows "We do not approve." to a person, and webtool's visible
// text leans toward hiding, so it may read "We approve.". That quote must
// never be found.
func TestDroppedWordsNeverMakeAQuote(t *testing.T) {
	deep := strings.Repeat("(", 33) + "x" + strings.Repeat(")", 33)
	pages := []string{
		`<p>We <span aria-hidden="true">do not </span>approve.</p>`,
		`<p>We <span hidden style="display:inline">do not </span>approve.</p>`,
		`<p>We <select><option>do not</option></select> approve.</p>`,
		`<p>We <span style="height:0;overflow:hidden">do not </span>approve.</p>`,
		`<p>We <span style="transform:scale(0)">do not </span>approve.</p>`,
		`<p>We <span style="display:none;--x:` + deep + `;display:inline">do not </span>approve.</p>`,
		`<p>We <span style="font-size:1px">do not </span>approve.</p>`,
		`<p>We <span style="` + strings.Repeat(" ", 20_000) + `">do not </span>approve.</p>`,
		`<p>We <script>document.write("x")</script><span style="display:none">do not </span>approve.</p>`,
		`<p>We <details><summary></summary>do not </details>approve.</p>`,
		// Round 4: a hidden copy of the quote elsewhere on the page.
		`<p>We <span aria-hidden="true">do not </span>approve.</p><p hidden>We approve.</p>`,
		`<p>We <span><template shadowrootmode="open">do not </template></span>approve.</p>`,
		`<p>We <input value="do not" size="6" readonly> approve.</p>`,
		`<p>We <img alt="do not"> approve.</p>`,
		`<p style="display:flex;flex-direction:row-reverse;gap:4px"><span>approve.</span> <span>We</span></p>`,
		`<p>We <span style="unicode-bidi:bidi-override;direction:rtl">ton od</span> approve.</p>`,
	}
	for _, body := range pages {
		p := readHTML([]byte(body), &url.URL{Scheme: "https", Host: "example.com", Path: "/"}, 1<<20)
		res := &fetch.Result{Status: 200, FinalURL: "https://example.com/", Page: p}
		if Describe(Item{URL: "https://example.com/", Quotes: []string{"We approve."}}, res, nil).Quotes[0].Found {
			t.Errorf("found in %s (visible %q)", body, p.Visible)
		}
	}
}

// Hidden text is found in neither.
func TestHiddenTextIsNotFound(t *testing.T) {
	p := readHTML([]byte(`<p>Hello <span style="display:none;display:bogus">FORGED</span> world</p>`),
		&url.URL{Scheme: "https", Host: "example.com", Path: "/"}, 1<<20)
	res := &fetch.Result{Status: 200, FinalURL: "https://example.com/", Page: p}
	rep := Describe(Item{URL: "https://example.com/", Quotes: []string{"FORGED", "Hello world", "Hello"}}, res, nil)
	if rep.Quotes[0].Found || rep.Quotes[1].Found || !rep.Quotes[2].Found {
		t.Fatalf("%+v", rep.Quotes)
	}
}

func TestTitleAndBodyAreSeparate(t *testing.T) {
	p := readHTML([]byte(`<title>We</title><p>approve.</p>`), &url.URL{Scheme: "https", Host: "example.com", Path: "/"}, 1<<20)
	res := &fetch.Result{Status: 200, FinalURL: "https://example.com/", Page: p}
	rep := Describe(Item{URL: "https://example.com/", Quotes: []string{"We approve.", "We", "approve."}}, res, nil)
	if rep.Quotes[0].Found || !rep.Quotes[1].Found || !rep.Quotes[2].Found {
		t.Fatalf("%+v", rep.Quotes)
	}
}

func TestLongTitleIsIncomplete(t *testing.T) {
	p := readHTML([]byte(`<title>`+strings.Repeat("x", 1000)+`TAILQUOTE</title><p>body</p>`), nil, 1<<20)
	res := &fetch.Result{Status: 200, FinalURL: "https://example.com/", Page: p}
	if Describe(Item{URL: "https://example.com/"}, res, nil).Complete {
		t.Fatal("a cut title left the page complete")
	}
}

// Round 5 cases, each checked against Chrome: the quote is hidden, or a
// person does not read its words in that order.
func TestRound5CasesNotFound(t *testing.T) {
	cases := []struct{ body, quote string }{
		{`<p style="--A:0;--a:1;opacity:var(--A)">FORGED</p>`, "FORGED"},
		{`<div style="--a:0;--b:var(--a)"><p style="--a:1;opacity:var(--b)">FORGED</p></div>`, "FORGED"},
		{`<div style="--c:transparent"><p style="--c:inherit;color:var(--c)">FORGED</p></div>`, "FORGED"},
		{`<p style="color:transparent;color:rgb(0 0 0 1)">FORGED</p>`, "FORGED"},
		{`<p style="color:transparent;color:rgb(0,,0 0)">FORGED</p>`, "FORGED"},
		{`<p style="font-size:0;font:bogus 16px serif">FORGED</p>`, "FORGED"},
		{`<p style="filter:opacity(0);filter:blur(10%)">FORGED</p>`, "FORGED"},
		{`<p style="transform:scale(0);transform:translate3d(1px 2px 3px)">FORGED</p>`, "FORGED"},
		{`<title>&#x202e;DEGROF&#x202c;</title><p>Actual</p>`, "DEGROF"},
		{`<p>&#x2067;&#x202c;אבג ABC&#x2069;</p>`, "אבג ABC"},
		{`<p>&#x202e;&#x2069;DEGROF&#x202c;</p>`, "DEGROF"},
		{`<p style="display:table-column">FORGED</p>`, "FORGED"},
		{`<div popover>FORGED</div>`, "FORGED"},
		{`<table><tfoot><tr><td>We</td></tr></tfoot><tbody><tr><td>approve</td></tr></tbody></table>`, "We approve"},
		{`<div style="display:table"><div style="display:table-footer-group">We</div><div style="display:table-row-group">approve</div></div>`, "We approve"},
		{`<span style="display:block">no</span><span style="display:block">where</span>`, "nowhere"},
		{`<div style="display:inline">no</div><div style="display:inline">where</div>`, "no where"},
		{`<div><template shadowrootmode="open">Actual</template>FORGED</div>`, "FORGED"},
		{`<p style="unicode-bidi:bidi-override;direction:rtl;all:initial">DEGROF</p>`, "DEGROF"},
		{`<p style="scale:-1 1">DEGROF</p>`, "DEGROF"},
		{`<ruby>A<rp>(</rp><rt>B</rt><rp>)</rp></ruby>`, "(B)"},
		{`<ruby>base<rp>FORGED</rp><rt>note</rt></ruby>`, "FORGED"},
		{`<p>no<span style="visibility:hidden"> </span>where</p>`, "nowhere"},
		{`<div style="font:16px monospace"><span style="display:inline-block;margin-left:100px">We </span><span style="display:inline-block;margin-left:-100px">approve</span></div>`, "We approve"},
		{`<div style="direction:rtl"><div style="display:flex"><span>We </span><span>approve</span></div></div>`, "We approve"},
		{`<div dir="rtl" style="display:flex"><span>We </span><span>approve</span></div>`, "We approve"},
		{`<p style="clip-path:inset(1000px)">FORGED</p>`, "FORGED"},
	}
	for _, c := range cases {
		p := readHTML([]byte(c.body), &url.URL{Scheme: "https", Host: "example.com", Path: "/"}, 1<<20)
		res := &fetch.Result{Status: 200, FinalURL: "https://example.com/", Page: p}
		if Describe(Item{URL: "https://example.com/", Quotes: []string{c.quote}}, res, nil).Quotes[0].Found {
			t.Errorf("%q found in %s (visible %q, text %q)", c.quote, c.body, p.Visible, p.Text)
		}
	}
	// And the ordinary cases still are.
	for _, c := range []struct{ body, quote string }{
		{`<p style="color:rgb(0 0 0 / 1)">shown</p>`, "shown"},
		{`<p style="font-weight:bold;font-size:16px">shown text</p>`, "shown text"},
		{`<p style="margin:0 0 1em;color:#333;text-align:center">centred</p>`, "centred"},
		{`<ruby>漢<rp>(</rp><rt>kan</rt><rp>)</rp></ruby> and more`, "and more"},
		{`<p>no<span> </span>where</p>`, "no where"},
		{`<p dir="ltr">We approve</p>`, "We approve"},
		{`<title>Plain title</title>`, "Plain title"},
	} {
		p := readHTML([]byte(c.body), &url.URL{Scheme: "https", Host: "example.com", Path: "/"}, 1<<20)
		res := &fetch.Result{Status: 200, FinalURL: "https://example.com/", Page: p}
		if !Describe(Item{URL: "https://example.com/", Quotes: []string{c.quote}}, res, nil).Quotes[0].Found {
			t.Errorf("%q not found in %s (visible %q)", c.quote, c.body, p.Visible)
		}
	}
}

// Round 6 cases, each checked against Chrome by the reviewer. Since check
// matches only text in plain elements with plain inline styles, none of
// them is found.
func TestRound6CasesNotFound(t *testing.T) {
	cases := []struct{ body, quote string }{
		{`<p style="color:transparent;color:rgb(0deg 0 0)">FORGED</p>`, "FORGED"},
		{`<p style="font-size:0;font:italic italic 16px serif">FORGED</p>`, "FORGED"},
		{`<p style="position:absolute;clip:rect(0,0,0,0);clip:rect(10%,100px,100px,0px)">FORGED</p>`, "FORGED"},
		{`<p style="--a:var(--missing);color:var(--a,transparent)">FORGED</p>`, "FORGED"},
		{`<p style="color:rgb(0 0 0 / none)">FORGED</p>`, "FORGED"},
		{`<p style="color:color(srgb 0 0 0 / 0)">FORGED</p>`, "FORGED"},
		{`<div style="-webkit-text-fill-color:black"><p style="color:transparent;-webkit-text-fill-color:currentcolor">FORGED</p></div>`, "FORGED"},
		{`<p style="mask-image:linear-gradient(transparent,transparent)">FORGED</p>`, "FORGED"},
		{`<p style="-webkit-text-security:disc">FORGED</p>`, "FORGED"},
		{`<p style="font:16px/16px monospace;clip-path:inset(10px)">FORGED</p>`, "FORGED"},
		{`<div style="height:20px;overflow:hidden"><div style="height:40px">shown</div>FORGED</div>`, "FORGED"},
		{`<p style="position:relative;left:-1000px">FORGED</p>`, "FORGED"},
		{`<div style="opacity:.1"><div style="opacity:.1"><p style="opacity:.1">FORGED</p></div></div>`, "FORGED"},
		{`<div style="font-size:2px"><p style="font-size:10%">FORGED</p></div>`, "FORGED"},
		{`<details name="a" open><summary>One</summary>shown</details><details name="a" open><summary>Two</summary>FORGED</details>`, "FORGED"},
		{`<html xmlns:h="http://www.w3.org/1999/xhtml"><body><h:style>FORGED</h:style></body></html>`, "FORGED"},
		{`<p>We <datalist style="display:inline">do not </datalist>approve</p>`, "We approve"},
		{`<details open>We<summary>approve</summary></details>`, "We approve"},
		{`<fieldset>We<legend>approve</legend></fieldset>`, "We approve"},
		{`<div style="display:initial">no</div><div style="display:initial">where</div>`, "no where"},
		{`<p style="font:16px monospace;word-spacing:-150px;padding-left:200px">We approve</p>`, "We approve"},
		{`<p>no<span style="margin-left:100px">where</span></p>`, "nowhere"},
		{`<p>no<span style="margin-left:100px"></span>where</p>`, "nowhere"},
		{`<p>We <em2>do not</em2> approve</p>`, "We approve"},
	}
	for _, c := range cases {
		p := readHTML([]byte(c.body), &url.URL{Scheme: "https", Host: "example.com", Path: "/"}, 1<<20)
		res := &fetch.Result{Status: 200, FinalURL: "https://example.com/", Page: p}
		if Describe(Item{URL: "https://example.com/", Quotes: []string{c.quote}}, res, nil).Quotes[0].Found {
			t.Errorf("%q found in %s (visible %q)", c.quote, c.body, p.Visible)
		}
	}
}

// Found by self-review after the reviewer's quota ran out: fonts and
// inline spacing.
func TestAllowlistTightened(t *testing.T) {
	for _, c := range []struct{ body, quote string }{
		{`<p style="font-family:Wingdings">FORGED</p>`, "FORGED"},
		{`<p>no<span style="padding-left:100px">where</span></p>`, "nowhere"},
		{`<p>no<span style="border-left:100px solid red"></span>where</p>`, "nowhere"},
		{`<body text="white"><p>FORGED</p></body>`, "FORGED"},
	} {
		p := readHTML([]byte(c.body), &url.URL{Scheme: "https", Host: "example.com", Path: "/"}, 1<<20)
		res := &fetch.Result{Status: 200, FinalURL: "https://example.com/", Page: p}
		if Describe(Item{URL: "https://example.com/", Quotes: []string{c.quote}}, res, nil).Quotes[0].Found {
			t.Errorf("%q found in %s (visible %q)", c.quote, c.body, p.Visible)
		}
	}
	p := readHTML([]byte(`<div style="padding:1em;border:1px solid #ccc"><p style="margin:0">a plain quote</p></div>`), nil, 1<<20)
	res := &fetch.Result{Status: 200, FinalURL: "https://example.com/", Page: p}
	if !Describe(Item{URL: "https://example.com/", Quotes: []string{"a plain quote"}}, res, nil).Quotes[0].Found {
		t.Errorf("plain block styles made a quote unfindable: %q", p.Visible)
	}
}

func describeHTML(body, quote string) ItemReport {
	p := readHTML([]byte(body), &url.URL{Scheme: "https", Host: "example.com", Path: "/"}, 1<<20)
	res := &fetch.Result{Status: 200, FinalURL: "https://example.com/", Page: p}
	return Describe(Item{URL: "https://example.com/", Quotes: []string{quote}}, res, nil)
}

// Round 8: what makes a whole page unchecked, and what makes one element
// unchecked. Each quote here is one a person may not read.
func TestRound8(t *testing.T) {
	pages := []struct{ body, quote string }{
		// 1: anything that can leave the flow or paint over other text.
		{`<div style="position:fixed;top:0;left:0;width:100%;height:100%;background-color:white">An innocuous page</div><p>The quote check will vouch for</p>`, "The quote check will vouch for"},
		{`<p>The quote check will vouch for</p><svg width=1 height=1 style="overflow:visible"><rect x=-10 y=-100 width=2000 height=200 fill="white"/></svg>`, "The quote check will vouch for"},
		{`<p>The quote</p><svg overflow="visible"></svg>`, "The quote"},
		{`<p>The quote</p><div style="float:left"></div>`, "The quote"},
		{`<p>The quote</p><div style="z-index:1"></div>`, "The quote"},
		{`<p>The quote</p><div style="opacity:0.99"></div>`, "The quote"},
		{`<p>The quote</p><div style="margin-top:-5px"></div>`, "The quote"},
		{`<p>The quote</p><div style="rotate:1deg"></div>`, "The quote"},
		{`<p>The quote</p><div style="position:var(--p)"></div>`, "The quote"},
		{`<p>The quote</p><span hidden style="position:absolute"></span>`, "The quote"},
		{`<p>The quote</p><img style="overflow:visible">`, "The quote"},
		// 15: a refresh to another page within seconds.
		{`<meta http-equiv="refresh" content="0;url=https://example.org/"><p>never seen</p>`, "never seen"},
		{`<meta http-equiv="Refresh" content="3"><p>never seen</p>`, "never seen"},
		// 4-7, 11-14: one element unchecked.
		{strings.Repeat("<h6><span>", 12) + "tiny secret words" + strings.Repeat("</span></h6>", 12), "tiny secret words"},
		{`<div>alpha <table align=left><tr><td>beta</td></tr></table> gamma</div>`, "beta"},
		{`<p align=right>beta</p>`, "beta"},
		{`<table><colgroup><col style="visibility:collapse"></colgroup><tr><td>hidden cell text</td><td>shown</td></tr></table>`, "hidden cell text"},
		{`<table><col width=0><tr><td>narrow cell text</td></tr></table>`, "narrow cell text"},
		{`<table><colgroup span=2></colgroup><tr><td>cell</td></tr></table>`, "cell"},
		{`<div style="font-size:8px;line-height:100%"><p style="font-size:200px">one two three</p></div>`, "one two three"},
		{`<p style="line-height:2em">em height</p>`, "em height"},
		{`<p style="line-height:9">tall lines</p>`, "tall lines"},
		{`<table><tr><td><div style="width:0;white-space:nowrap">covered text here</div></td><td>other words</td></tr></table>`, "covered text here"},
		{`<p style="max-width:1px">narrow</p>`, "narrow"},
		{`<p style="min-width:9999px">wide</p>`, "wide"},
		{`<table><tr><td>alpha</td><td>beta</td></tr></table>`, "alpha beta"},
		{`<div style="margin-left:9999999px">far away text</div>`, "far away text"},
		{`<div style="padding-left:2001px">far text</div>`, "far text"},
		{`<div style="font-size:201px">huge</div>`, "huge"},
		{`<table border=9999999><tr><td>far text</td></tr></table>`, "far text"},
		{`<table cellpadding=5000><tr><td>far text</td></tr></table>`, "far text"},
		{`<p>The total was</p><ol start=1000000><li>dollars</li></ol>`, "the total was dollars"},
		{`<ol><li>apples</li><li>pears</li></ol>`, "apples pears"},
		{`<ol reversed><li>pears</li></ol>`, "pears"},
		{`<ol><li value=7>pears</li></ol>`, "pears"},
		{`<ul type=square><li>pears</li></ul>`, "pears"},
		{`<p style="color:canvas">invisible on any theme</p>`, "invisible on any theme"},
		{`<p style="background-color:canvastext">themed</p>`, "themed"},
		// 9: <center> starts its own line.
		{`<div>no<center>where</center></div>`, "nowhere"},
	}
	for _, c := range pages {
		if rep := describeHTML(c.body, c.quote); rep.Quotes[0].Found {
			t.Errorf("%q found in %s", c.quote, c.body)
		}
	}
	for _, body := range []string{pages[0].body, pages[1].body, pages[11].body} {
		if describeHTML(body, "x").Uncheckable == "" {
			t.Errorf("%s not reported uncheckable", body)
		}
	}
	// Plain pages are still checkable.
	for _, c := range []struct{ body, quote string }{
		{`<meta http-equiv="refresh" content="60"><p>slow refresh</p>`, "slow refresh"},
		{`<table border=1 cellpadding=4><tr><td>cell words</td></tr></table>`, "cell words"},
		{`<p style="line-height:1.5;font-size:18px;color:#222">styled words</p>`, "styled words"},
		{`<ol><li>one item</li></ol>`, "one item"},
		{`<h1>a <span>heading</span></h1>`, "a heading"},
	} {
		rep := describeHTML(c.body, c.quote)
		if !rep.Quotes[0].Found || rep.Uncheckable != "" {
			t.Errorf("%q not found in %s (%q)", c.quote, c.body, rep.Uncheckable)
		}
	}
}

func TestUncheckablePageFindsNothing(t *testing.T) {
	res := &fetch.Result{Status: 200, FinalURL: "https://example.com/", Page: text.Page{Title: "T", Visible: "words", Uncheckable: "the page is XHTML"}}
	rep := Describe(Item{URL: "https://example.com/", Quotes: []string{"words", "T"}}, res, nil)
	if rep.Quotes[0].Found || rep.Quotes[1].Found || rep.Uncheckable != "the page is XHTML" {
		t.Fatalf("%+v", rep)
	}
}

// Round 9: an overlay or refresh past the token cap is still seen, a
// list-item display is not plain, and numbering attributes matter.
func TestRound9(t *testing.T) {
	pad := strings.Repeat("<i></i>", text.MaxTokens/2+1)
	for _, c := range []struct{ body, quote string }{
		{`<p>The quote check will vouch for</p>` + pad + `<div style="position:fixed;top:0;left:0;width:100%;height:100%;background-color:white;z-index:9">An innocuous page</div>`, "The quote check will vouch for"},
		{`<p>never seen</p>` + pad + `<meta http-equiv=refresh content="0;url=https://example.org/">`, "never seen"},
		{`<p>words</p><svg><g style="overflow:visible"></g></svg>`, "words"},
		{`<p>words</p><meta http-equiv=refresh content="5.9">`, "words"},
	} {
		rep := describeHTML(c.body, c.quote)
		if rep.Quotes[0].Found || rep.Uncheckable == "" {
			t.Errorf("%.60q: found %v, uncheckable %q", c.body, rep.Quotes[0].Found, rep.Uncheckable)
		}
	}
	for _, body := range []string{
		`<p>Revenue grew</p><div style="display:list-item;list-style-type:decimal;list-style-position:inside">0% this year</div>`,
		`<p>Revenue grew</p><p style="display:list-item;list-style:decimal inside">0% this year</p>`,
		`<li>Revenue grew</li><li>0% this year</li>`,
	} {
		if describeHTML(body, "Revenue grew 0% this year").Quotes[0].Found {
			t.Errorf("found across a list marker in %s", body)
		}
		if describeHTML(body, "0% this year").Quotes[0].Found && strings.Contains(body, "display:list-item") {
			t.Errorf("text under display:list-item was matched in %s", body)
		}
	}
	// The numbering attribute itself makes the list unchecked: the same
	// item without it is found.
	if !describeHTML(`<ol><li>dollars and more</li></ol>`, "dollars and more").Quotes[0].Found {
		t.Error("a plain list item was not found")
	}
	for _, attr := range []string{"start=1000000", "reversed", "type=I"} {
		if describeHTML(`<ol `+attr+`><li>dollars and more</li></ol>`, "dollars and more").Quotes[0].Found {
			t.Errorf("found in <ol %s>", attr)
		}
	}
	if describeHTML(`<ol><li value=9>dollars and more</li></ol>`, "dollars and more").Quotes[0].Found {
		t.Error("found in <li value>")
	}
	// A long page short of the cap is still checkable.
	if rep := describeHTML(`<p>words</p>`+strings.Repeat("<i></i>", 1000), "words"); !rep.Quotes[0].Found || !rep.Complete {
		t.Errorf("%+v", rep)
	}
}

// Round 10: in SVG and MathML, <style>, <title>, <textarea> and the like
// hold markup, not raw text, and a <div> breaks back out into HTML. The
// overlay is judged on the parsed tree, so these are seen.
func TestForeignContentOverlays(t *testing.T) {
	cover := `<div style="position:fixed;top:0;left:0;width:100%;height:100%;background-color:white;z-index:9">An innocuous page</div>`
	refresh := `<meta http-equiv=refresh content="0;url=https://example.org/">`
	for _, b := range []string{
		`<p>The quote</p><svg><style>` + cover,
		`<p>The quote</p><svg><![CDATA[>]]><title>` + cover,
		`<p>The quote</p><svg><textarea>` + cover + `</textarea></svg>`,
		`<p>The quote</p><math><style>` + cover,
		`<p>The quote</p><svg><style><p style="position:absolute;top:0">cover text</p></style></svg>`,
		`<p>The quote</p><svg><style>` + refresh + `</style></svg>`,
		`<p>The quote</p><image src=x style="overflow:visible">`,
		`<p>The quote</p><image src=x overflow=visible>`,
		`<p>The quote</p><svg><foreignObject><style><!--</style>` + cover + `</foreignObject></svg>`,
	} {
		rep := describeHTML(b, "The quote")
		if rep.Quotes[0].Found || rep.Uncheckable == "" {
			t.Errorf("%s: found %v uncheckable %q", b, rep.Quotes[0].Found, rep.Uncheckable)
		}
	}
	// Raw text in HTML is not markup: these are not overlays.
	for _, b := range []string{
		`<p>The quote</p><style>` + cover + `</style>`,
		`<p>The quote</p><script>"` + cover + `"</script>`,
		`<p>The quote</p><textarea>` + cover + `</textarea>`,
	} {
		if rep := describeHTML(b, "The quote"); !rep.Quotes[0].Found || rep.Uncheckable != "" {
			t.Errorf("%s: found %v uncheckable %q", b, rep.Quotes[0].Found, rep.Uncheckable)
		}
	}
}

// readHTML reads a page as production does: text.Read, as the reader
// process returns it, compacted.
func readHTML(body []byte, base *url.URL, limit int) text.Page {
	return text.Read(context.Background(), body, "text/html", base, limit, limit)
}

// Package check fetches pages a report cites and says whether the quotes
// it gives are on them, for relay's report verifier. The answer is JSON in
// a fixed shape, documented in the README, since relay parses it.
package check

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/idna"
	"golang.org/x/net/publicsuffix"

	"github.com/kirahowe/webtool/internal/envelope"
	"github.com/kirahowe/webtool/internal/fetch"
	"github.com/kirahowe/webtool/internal/text"
)

// Limits on one call.
const (
	MaxItems      = 20
	MaxQuotes     = 20
	MaxQuoteRunes = 2000
	MaxURLBytes   = 4096
)

// Item is one cited page and the quotes said to be on it.
type Item struct {
	URL    string
	Quotes []string
}

// ParseInput reads {items: [{url, quotes}]} strictly.
func ParseInput(in map[string]json.RawMessage) ([]Item, error) {
	for k := range in {
		if k != "items" {
			return nil, errors.New("there is no argument called " + text.EscapeOnly(k) + "; the input is {items: [{url, quotes}]}")
		}
	}
	raw, ok := in["items"]
	if !ok {
		return nil, errors.New("the input needs items")
	}
	list, err := envelope.List(raw)
	if err != nil {
		return nil, errors.New("items must be a list")
	}
	if len(list) == 0 || len(list) > MaxItems {
		return nil, fmt.Errorf("items must hold 1 to %d items", MaxItems)
	}
	var out []Item
	for i, r := range list {
		obj, err := envelope.Object(r, []string{"url"}, []string{"quotes"})
		if err != nil {
			return nil, fmt.Errorf("item %d: %v", i+1, err)
		}
		var it Item
		if it.URL, err = envelope.String(obj["url"]); err != nil || it.URL == "" || len(it.URL) > MaxURLBytes {
			return nil, fmt.Errorf("item %d: url must be a string of 1 to %d bytes", i+1, MaxURLBytes)
		}
		if q, ok := obj["quotes"]; ok {
			qs, err := envelope.List(q)
			if err != nil || len(qs) > MaxQuotes {
				return nil, fmt.Errorf("item %d: quotes must be a list of at most %d strings", i+1, MaxQuotes)
			}
			for _, q := range qs {
				s, err := envelope.String(q)
				if err != nil || utf8.RuneCountInString(s) > MaxQuoteRunes {
					return nil, fmt.Errorf("item %d: each quote must be a string of at most %d characters", i+1, MaxQuoteRunes)
				}
				it.Quotes = append(it.Quotes, s)
			}
		}
		out = append(out, it)
	}
	return out, nil
}

// Report is the answer: one entry per item, in the order given.
type Report struct {
	Items []ItemReport `json:"items"`
}

// ItemReport is what was found for one item. Every field is always
// present; a fetch that failed leaves the fields it never reached at
// their zero values and says why in error. Every string has invisible
// characters escaped as \u{XXXX}, as everywhere else webtool prints.
type ItemReport struct {
	URL     string `json:"url"`
	Fetched bool   `json:"fetched"`
	// Complete says the whole page was read: the body was not cut at
	// max_body_bytes, the visible text not at its limit, and nothing was
	// nested too deeply. A quote not found on an incomplete page may be
	// in the part not read.
	Complete bool `json:"complete"`
	// Uncheckable, when not empty, says why no quote on the page can be
	// vouched for: then every quote is reported not found.
	Uncheckable    string        `json:"uncheckable"`
	Error          string        `json:"error"`
	Status         int           `json:"status"`
	FinalURL       string        `json:"final_url"`
	Redirects      []fetch.Hop   `json:"redirects"`
	CrossesDomains bool          `json:"crosses_domains"`
	Domain         string        `json:"domain"`
	FinalDomain    string        `json:"final_domain"`
	Host           *HostInfo     `json:"host"`
	FinalHost      *HostInfo     `json:"final_host"`
	SHA256         string        `json:"sha256"`
	HiddenChars    int           `json:"hidden_chars"`
	Quotes         []QuoteReport `json:"quotes"`
}

// HostInfo describes a host name as a reader might misread it.
type HostInfo struct {
	Name         string   `json:"name"`          // as given, lower case
	Punycode     bool     `json:"punycode"`      // a label starts with xn--
	Unicode      string   `json:"unicode"`       // the name as it displays
	Scripts      []string `json:"scripts"`       // scripts of its letters, sorted
	MixedScripts bool     `json:"mixed_scripts"` // some label mixes scripts
}

// QuoteReport says whether one quote is on the page.
type QuoteReport struct {
	Quote       string `json:"quote"` // with invisible characters escaped
	Found       bool   `json:"found"`
	HiddenChars bool   `json:"hidden_chars"` // the quote holds invisible characters
}

// Fetcher is what Run fetches with: in production, the same guarded
// fetcher the fetch command uses.
type Fetcher interface {
	Fetch(ctx context.Context, url string) (*fetch.Result, error)
}

// Run fetches each item in turn and reports on it.
func Run(ctx context.Context, f Fetcher, items []Item) Report {
	r := Report{Items: []ItemReport{}}
	for _, it := range items {
		res, err := f.Fetch(ctx, it.URL)
		r.Items = append(r.Items, Describe(it, res, err))
	}
	return r
}

// Describe builds the report for one item from its fetch.
func Describe(it Item, res *fetch.Result, err error) ItemReport {
	rep := ItemReport{URL: text.EscapeOnly(it.URL), Redirects: []fetch.Hop{}, Quotes: []QuoteReport{}}
	if res == nil {
		res = &fetch.Result{}
	}
	rep.Status = res.Status
	rep.FinalURL = text.EscapeOnly(res.FinalURL)
	for _, h := range res.Redirects {
		rep.Redirects = append(rep.Redirects, fetch.Hop{URL: text.EscapeOnly(h.URL), Status: h.Status})
	}
	// Domains are compared unescaped and reported escaped.
	if h := hostOf(it.URL); h != "" {
		rep.Host, rep.Domain = Host(h), text.EscapeOnly(Domain(h))
	}
	if h := hostOf(res.FinalURL); h != "" {
		rep.FinalHost, rep.FinalDomain = Host(h), text.EscapeOnly(Domain(h))
	}
	rep.CrossesDomains = crosses(chain(it.URL, res))
	if err != nil {
		rep.Error = text.EscapeOnly(err.Error())
	} else {
		rep.Fetched = true
		p := res.Page
		rep.Complete = !res.Truncated && !p.VisibleCut && !p.TitleCut && !p.Deep && !p.TooManyTokens
		rep.SHA256 = res.SHA256
		rep.HiddenChars = res.Page.Hidden
	}
	// Only what a person can read counts: the title on its own, or a run
	// of the visible text between breaks. The visible text leans toward
	// hiding and breaks wherever something was hidden, replaced, moved or
	// reordered, so neither a wrong reading of the page's styles nor a
	// hidden copy of the quote can join words into it.
	title := text.Segments(res.Page.Title)
	segments := text.Segments(res.Page.Visible)
	if err == nil && res.Page.Uncheckable != "" {
		rep.Uncheckable = text.EscapeOnly(res.Page.Uncheckable)
		segments, title = nil, nil
	}
	found := func(q string) bool {
		return text.FindIn(title, q) || text.FindIn(segments, q)
	}
	for _, q := range it.Quotes {
		rep.Quotes = append(rep.Quotes, QuoteReport{
			Quote:       text.EscapeOnly(q),
			Found:       err == nil && found(q),
			HiddenChars: text.HasInvisible(q),
		})
	}
	return rep
}

// chain is every URL the fetch requested or was sent to, in order, ending
// at the final URL or at the redirect target it failed on.
func chain(start string, res *fetch.Result) []string {
	urls := []string{start}
	for i, h := range res.Redirects {
		if i > 0 {
			urls = append(urls, h.URL)
		}
	}
	for _, last := range []string{res.LastURL, res.FinalURL} {
		if last != "" && last != urls[len(urls)-1] {
			urls = append(urls, last)
		}
	}
	return urls
}

// crosses reports whether any step of a chain moves to another
// registrable domain.
func crosses(urls []string) bool {
	for i := 1; i < len(urls); i++ {
		a, b := hostOf(urls[i-1]), hostOf(urls[i])
		if a != "" && b != "" && Domain(a) != Domain(b) {
			return true
		}
	}
	return false
}

// hostOf is a URL's host, lower case and mapped by IDNA as net/http dials
// it, without a trailing dot. The dot goes after the mapping, which turns
// an ideographic full stop into one.
func hostOf(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	h := u.Hostname()
	if _, err := netip.ParseAddr(h); err == nil {
		return h
	}
	if ascii, err := idna.Lookup.ToASCII(h); err == nil {
		h = ascii
	}
	return strings.TrimSuffix(strings.ToLower(h), ".")
}

// Domain is a host's registrable domain (its eTLD+1 by the public suffix
// list), in ASCII. An IP address, or a name that is itself a public
// suffix, is its own domain.
func Domain(host string) string {
	if _, err := netip.ParseAddr(host); err == nil {
		return host
	}
	ascii, err := idna.Lookup.ToASCII(host)
	if err != nil {
		ascii = host
	}
	d, err := publicsuffix.EffectiveTLDPlusOne(ascii)
	if err != nil {
		return ascii
	}
	return d
}

// Host describes a host name: whether it is punycode, how it displays,
// and whether a label mixes scripts in a way UTS #39's highly restrictive
// level would not allow (Latin with Cyrillic, say, as in a lookalike of a
// Latin name).
func Host(host string) *HostInfo {
	h := &HostInfo{Name: text.EscapeOnly(host), Unicode: text.EscapeOnly(host), Scripts: []string{}}
	if _, err := netip.ParseAddr(host); err == nil {
		return h
	}
	display := host
	if u, err := idna.Display.ToUnicode(host); err == nil {
		display = u
	}
	h.Unicode = text.EscapeOnly(display)
	all := map[string]bool{}
	for _, label := range strings.Split(display, ".") {
		set := map[string]bool{}
		for _, r := range label {
			if s := scriptOf(r); s != "" {
				set[s], all[s] = true, true
			}
		}
		if mixed(set) {
			h.MixedScripts = true
		}
	}
	for _, label := range strings.Split(host, ".") {
		if strings.HasPrefix(label, "xn--") {
			h.Punycode = true
		}
	}
	for s := range all {
		h.Scripts = append(h.Scripts, s)
	}
	slices.Sort(h.Scripts)
	return h
}

// allowedMixes are the combinations of scripts that UTS #39 counts as one
// writing system.
var allowedMixes = [][]string{
	{"Latin", "Han", "Hiragana", "Katakana"},
	{"Latin", "Han", "Bopomofo"},
	{"Latin", "Han", "Hangul"},
}

func mixed(set map[string]bool) bool {
	if len(set) <= 1 {
		return false
	}
	for _, mix := range allowedMixes {
		within := true
		for s := range set {
			if !slices.Contains(mix, s) {
				within = false
			}
		}
		if within {
			return false
		}
	}
	return true
}

// common scripts are tried first; the rest in name order, so the answer
// does not depend on map order.
var scriptOrder = func() []string {
	first := []string{"Latin", "Cyrillic", "Greek", "Han", "Hiragana", "Katakana", "Hangul", "Arabic", "Hebrew", "Armenian", "Georgian"}
	var rest []string
	for name := range unicode.Scripts {
		if !slices.Contains(first, name) && name != "Common" && name != "Inherited" {
			rest = append(rest, name)
		}
	}
	slices.Sort(rest)
	return append(first, rest...)
}()

// scriptOf is a rune's script, or "" for Common and Inherited ones such
// as digits, hyphens and combining marks.
func scriptOf(r rune) string {
	for _, name := range scriptOrder {
		if unicode.Is(unicode.Scripts[name], r) {
			return name
		}
	}
	return ""
}

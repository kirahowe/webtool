package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/kirahowe/webtool/internal/envelope"
	"github.com/kirahowe/webtool/internal/fetch"
	"github.com/kirahowe/webtool/internal/search"
	"github.com/kirahowe/webtool/internal/text"
)

// MaxAskPages is the most pages one ask fetches.
const MaxAskPages = 5

// DefaultAskPages is how many pages ask fetches when the call does not
// say.
const DefaultAskPages = 3

// ask searches and fetches the top n results. The input has no URL: the
// only URLs fetched are the ones the search backend returned, so the
// caller's one channel out is the query, as for search.
func (a app) ask(ctx context.Context, cfg Config, f pageFetcher, in map[string]json.RawMessage) envelope.Result {
	obj, err := checkKeys(in, []string{"query"}, []string{"n"})
	if err != nil {
		return badCall(err)
	}
	query, err := envelope.String(obj["query"])
	query = strings.TrimSpace(query)
	if err != nil || query == "" || utf8.RuneCountInString(query) > MaxQueryRunes {
		return badCall(fmt.Errorf("query must be a string of 1 to %d characters", MaxQueryRunes))
	}
	n := DefaultAskPages
	if raw, ok := obj["n"]; ok {
		if n, err = envelope.Int(raw); err != nil || n < 1 || n > MaxAskPages {
			return badCall(fmt.Errorf("n must be a whole number from 1 to %d", MaxAskPages))
		}
	}
	// Ask for more results than pages, so a URL that comes twice still
	// leaves n pages to read.
	results, err := a.newBackend(cfg.Search).Search(ctx, query, min(2*n, search.MaxCount))
	if err != nil {
		return failed(err)
	}
	pages := askPages(results, n)
	shown := map[string]bool{}
	for _, r := range results {
		shown[r.URL] = true
	}
	var rendered []askedPage
	ok := 0
	for _, r := range pages {
		p := askedPage{result: r}
		res, err := followShown(ctx, f, r.URL, shown)
		if err != nil {
			p.err = err
		} else {
			p.body = renderPage(res, cfg.Fetch.MaxBodyBytes, cfg.Fetch.MaxTextChars)
			ok++
		}
		rendered = append(rendered, p)
	}
	return envelope.Result{
		Content: renderAsk(query, rendered, boundary()),
		IsError: len(pages) > 0 && ok == 0,
		Labels:  contaminated,
	}
}

// MaxAskHops is how many redirects ask follows for one result.
const MaxAskHops = 5

// followShown fetches u and follows a redirect only to a URL that
// appeared verbatim in the search results. Any other target is not
// fetched: the moved page comes back as fetch reports it, so the search
// backend, not a page, picks every URL ask reads.
func followShown(ctx context.Context, f pageFetcher, u string, shown map[string]bool) (*fetch.Result, error) {
	for hops := 0; ; hops++ {
		res, err := f.Fetch(ctx, u)
		if err != nil || res.Moved == "" || !shown[res.Moved] || hops == MaxAskHops {
			return res, err
		}
		u = res.Moved
	}
}

// askPages is the results ask fetches: the first n, each URL once.
func askPages(results []search.Result, n int) []search.Result {
	seen := map[string]bool{}
	var out []search.Result
	for _, r := range results {
		if len(out) == n {
			break
		}
		if seen[r.URL] {
			continue
		}
		seen[r.URL] = true
		out = append(out, r)
	}
	return out
}

type askedPage struct {
	result search.Result
	body   string // renderPage's output when the fetch worked
	err    error
}

// boundary is a random token that marks where each page starts, so text
// on a page cannot pass for the start of another page.
func boundary() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand does not fail on supported platforms
	}
	return hex.EncodeToString(b)
}

// renderAsk lists the pages, numbered from 1 in search order. Each starts
// with a line holding the boundary token, then the search result's title
// and URL, then the page as fetch renders it or the reason it was not
// read.
func renderAsk(query string, pages []askedPage, token string) string {
	if len(pages) == 0 {
		return "No results for \"" + oneLine(query) + "\"."
	}
	var b strings.Builder
	count := fmt.Sprintf("%d pages", len(pages))
	if len(pages) == 1 {
		count = "1 page"
	}
	fmt.Fprintf(&b, "%s for \"%s\". Each page starts with a line \"=== page N %s ===\".\n", count, oneLine(query), token)
	for i, p := range pages {
		fmt.Fprintf(&b, "\n=== page %d %s ===\n", i+1, token)
		fmt.Fprintf(&b, "result_title: %s\nresult_url: %s\n", oneLine(p.result.Title), oneLine(p.result.URL))
		if p.err != nil {
			fmt.Fprintf(&b, "error: %s\n", oneLine(p.err.Error()))
			continue
		}
		b.WriteString(p.body)
		b.WriteString("\n")
	}
	return b.String()
}

// oneLine escapes invisible characters, newlines and tabs, so a value
// stays on its line.
func oneLine(s string) string {
	return strings.NewReplacer("\n", `\u{000A}`, "\t", `\u{0009}`, "\r", `\u{000D}`).Replace(text.EscapeOnly(s))
}

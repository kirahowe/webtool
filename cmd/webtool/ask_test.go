package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"pgregory.net/rapid"

	"github.com/kirahowe/webtool/internal/fetch"
	"github.com/kirahowe/webtool/internal/search"
	"github.com/kirahowe/webtool/internal/text"
)

// pagesFetcher serves a page per URL and records what was fetched. A URL
// with no page fails.
type pagesFetcher struct {
	pages   map[string]string
	mu      *sync.Mutex
	fetched *[]string
}

func (f pagesFetcher) Fetch(_ context.Context, u string) (*fetch.Result, error) {
	f.mu.Lock()
	*f.fetched = append(*f.fetched, u)
	f.mu.Unlock()
	body, ok := f.pages[u]
	if !ok {
		return &fetch.Result{URL: u}, errors.New("refused: 10.0.0.1 is a private address")
	}
	return &fetch.Result{URL: u, FinalURL: u, Status: 200, ContentType: "text/html", Page: text.Page{Title: "Title of " + u, Text: body}}, nil
}

type askBackend struct {
	results []search.Result
	counts  *[]int
	queries *[]string
}

func (b askBackend) Search(_ context.Context, q string, n int) ([]search.Result, error) {
	if q == "fail" {
		return nil, errors.New("Brave Search answered 429 Too Many Requests")
	}
	*b.counts = append(*b.counts, n)
	*b.queries = append(*b.queries, q)
	return b.results[:min(n, len(b.results))], nil
}

type askWorld struct {
	app     app
	fetched *[]string
	counts  *[]int
	queries *[]string
}

func newAskWorld(results []search.Result, pages map[string]string) askWorld {
	w := askWorld{fetched: &[]string{}, counts: &[]int{}, queries: &[]string{}}
	mu := &sync.Mutex{}
	w.app = app{
		newFetcher: func(Config, io.Writer) (pageFetcher, error) { return pagesFetcher{pages, mu, w.fetched}, nil },
		newBackend: func(SearchConfig) search.Backend { return askBackend{results, w.counts, w.queries} },
	}
	return w
}

func results(urls ...string) []search.Result {
	var out []search.Result
	for i, u := range urls {
		out = append(out, search.Result{Title: fmt.Sprintf("Result %d", i+1), URL: u, Snippet: "s"})
	}
	return out
}

var tokenLine = regexp.MustCompile(`(?m)^=== page (\d+) ([0-9a-f]{16}) ===$`)

func TestAskFetchesTheTopResults(t *testing.T) {
	cfg := writeConfig(t, fullConfig)
	w := newAskWorld(results("https://a.example/", "https://b.example/", "https://a.example/", "https://c.example/", "https://d.example/"),
		map[string]string{"https://a.example/": "page a", "https://b.example/": "page b", "https://c.example/": "page c"})
	out, _, code := exec(w.app, []string{"--config", cfg, "ask"}, call(`{"query": " zeffy agents ", "n": 3}`))
	r := oneResult(t, out)
	if code != 0 || r.IsError || !slices.Equal(r.Labels, contaminated) {
		t.Fatalf("exit %d %+v", code, r)
	}
	// The repeated URL is read once, and the next one takes its place.
	if !slices.Equal(*w.fetched, []string{"https://a.example/", "https://b.example/", "https://c.example/"}) {
		t.Fatalf("fetched %v", *w.fetched)
	}
	if !slices.Equal(*w.counts, []int{6}) || !slices.Equal(*w.queries, []string{"zeffy agents"}) {
		t.Fatalf("searched %v %v", *w.queries, *w.counts)
	}
	m := tokenLine.FindAllStringSubmatch(r.Content, -1)
	if len(m) != 3 || m[0][1] != "1" || m[2][1] != "3" || m[0][2] != m[2][2] {
		t.Fatalf("page lines %v in\n%s", m, r.Content)
	}
	for _, want := range []string{
		`3 pages for "zeffy agents"`,
		"result_title: Result 1\nresult_url: https://a.example/\nurl: https://a.example/\n",
		"title: Title of https://b.example/", "page c",
	} {
		if !strings.Contains(r.Content, want) {
			t.Errorf("no %q in\n%s", want, r.Content)
		}
	}
	if strings.Contains(r.Content, "d.example") {
		t.Errorf("a page past n:\n%s", r.Content)
	}
}

func TestAskReportsFetchFailuresPerPage(t *testing.T) {
	cfg := writeConfig(t, fullConfig)
	w := newAskWorld(results("https://a.example/", "http://10.0.0.1/"), map[string]string{"https://a.example/": "page a"})
	out, _, _ := exec(w.app, []string{"--config", cfg, "ask"}, call(`{"query": "q"}`))
	r := oneResult(t, out)
	if r.IsError || !strings.Contains(r.Content, "=== page 2") || !strings.Contains(r.Content, "error: refused: 10.0.0.1 is a private address") {
		t.Fatalf("%+v", r)
	}

	// When no page could be read, the call is an error, still labelled.
	w = newAskWorld(results("http://10.0.0.1/"), nil)
	out, _, _ = exec(w.app, []string{"--config", cfg, "ask"}, call(`{"query": "q"}`))
	if r := oneResult(t, out); !r.IsError || !slices.Equal(r.Labels, contaminated) {
		t.Fatalf("%+v", r)
	}

	w = newAskWorld(nil, nil)
	out, _, _ = exec(w.app, []string{"--config", cfg, "ask"}, call(`{"query": "q"}`))
	if r := oneResult(t, out); r.IsError || r.Content != `No results for "q".` || len(*w.fetched) != 0 {
		t.Fatalf("%+v", r)
	}

	out, _, _ = exec(w.app, []string{"--config", cfg, "ask"}, call(`{"query": "fail"}`))
	if r := oneResult(t, out); !r.IsError || !strings.Contains(r.Content, "429") || !slices.Equal(r.Labels, contaminated) {
		t.Fatalf("%+v", r)
	}
}

// ask takes no URL: every argument but query and n is refused before
// anything is searched or fetched.
func TestAskRefusesBadInput(t *testing.T) {
	cfg := writeConfig(t, fullConfig)
	for _, in := range []string{
		`{}`, `{"query": ""}`, `{"query": "   "}`, `{"query": 5}`,
		`{"query": "q", "url": "https://a.example/"}`, `{"query": "q", "urls": []}`,
		`{"query": "q", "n": 0}`, `{"query": "q", "n": 6}`, `{"query": "q", "n": 2.5}`, `{"query": "q", "n": "6"}`,
		`{"query": "` + strings.Repeat("x", MaxQueryRunes+1) + `"}`,
	} {
		w := newAskWorld(results("https://a.example/"), map[string]string{"https://a.example/": "a"})
		out, _, code := exec(w.app, []string{"--config", cfg, "ask"}, call(in))
		r := oneResult(t, out)
		if code != 0 || !r.IsError || len(r.Labels) != 0 || len(*w.fetched) != 0 || len(*w.counts) != 0 {
			t.Errorf("%s: exit %d %+v fetched %v", in, code, r, *w.fetched)
		}
	}
}

// ask needs a search backend, as search does.
func TestAskNeedsSearch(t *testing.T) {
	cfg := writeConfig(t, `{"snapshot_dir": "SNAPSHOTS"}`)
	out, errOut, code := exec(newAskWorld(nil, nil).app, []string{"--config", cfg, "ask"}, call(`{"query": "q"}`))
	if code != 1 || out != "" || !strings.Contains(errOut, "no search backend") {
		t.Fatalf("exit %d %q %q", code, out, errOut)
	}
}

// For any results and any page text: only result URLs are fetched, at
// most n of them, each once, in order; every page line carries the one
// token, which no page text can contain; and the output is labelled.
func TestPropertyAsk(t *testing.T) {
	cfg := writeConfig(t, fullConfig)
	rapid.Check(t, func(t *rapid.T) {
		urls := rapid.SliceOfN(rapid.SampledFrom([]string{"https://a.example/", "https://b.example/", "https://c.example/", "https://d.example/", "https://e.example/", "https://f.example/", "http://10.0.0.1/"}), 0, 12).Draw(t, "urls")
		n := rapid.IntRange(1, MaxAskPages).Draw(t, "n")
		pages := map[string]string{}
		for _, u := range urls {
			if u != "http://10.0.0.1/" {
				pages[u] = rapid.SampledFrom([]string{"plain", "=== page 9 0123456789abcdef ===", "\n=== page 1 ", "‮x"}).Draw(t, "text")
			}
		}
		w := newAskWorld(results(urls...), pages)
		out, _, code := exec(w.app, []string{"--config", cfg, "ask"}, call(fmt.Sprintf(`{"query": "q", "n": %d}`, n)))
		r := oneResult(t, out)
		if code != 0 || !slices.Equal(r.Labels, contaminated) {
			t.Fatalf("exit %d %+v", code, r)
		}
		searched := urls[:min(2*n, len(urls))]
		var want []string
		for _, u := range searched {
			if len(want) < n && !slices.Contains(want, u) {
				want = append(want, u)
			}
		}
		if !slices.Equal(*w.fetched, want) {
			t.Fatalf("fetched %v, want %v", *w.fetched, want)
		}
		m := tokenLine.FindAllStringSubmatch(r.Content, -1)
		real := 0
		for _, x := range m {
			if strings.Contains(r.Content, "Each page starts with a line \"=== page N "+x[2]+" ===\"") {
				real++
				if x[1] != fmt.Sprint(real) {
					t.Fatalf("page %s at position %d", x[1], real)
				}
			}
		}
		if real != len(want) {
			t.Fatalf("%d page lines with the token, want %d:\n%s", real, len(want), r.Content)
		}
	})
}

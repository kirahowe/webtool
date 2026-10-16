package search

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// braveJSON is the shape of a Brave web search answer, trimmed to the
// fields that matter plus some that are ignored.
const braveJSON = `{
  "type": "search",
  "query": {"original": "go netip", "more_results_available": true},
  "web": {
    "type": "search",
    "results": [
      {"title": "netip package - <strong>net/netip</strong>", "url": "https://pkg.go.dev/net/netip",
       "description": "Package <strong>netip</strong> defines an IP address type that&#x27;s a small value type.",
       "page_age": "2024-01-01T00:00:00", "profile": {"name": "Go"}, "language": "en"},
      {"title": "Hidden​ text", "url": "https://example.com/a?b=c",
       "description": ""}
    ]
  }
}`

func fake(t *testing.T, status int, body string) (*Brave, *http.Request) {
	t.Helper()
	var got http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = *r.Clone(context.Background())
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	b := NewBrave([]string{"printf", "s3cret\n"}, 0)
	b.Endpoint = srv.URL + "/res/v1/web/search"
	return b, &got
}

func TestBraveSearch(t *testing.T) {
	b, got := fake(t, 200, braveJSON)
	res, err := b.Search(context.Background(), "go netip", 5)
	if err != nil {
		t.Fatal(err)
	}
	if got.Header.Get("X-Subscription-Token") != "s3cret" || got.URL.Query().Get("q") != "go netip" || got.URL.Query().Get("count") != "5" {
		t.Fatalf("request %v %v", got.Header, got.URL)
	}
	if len(res) != 2 || res[0].Title != "netip package - net/netip" ||
		res[0].Snippet != "Package netip defines an IP address type that's a small value type." {
		t.Fatalf("%+v", res)
	}
	want := `1. netip package - net/netip
   https://pkg.go.dev/net/netip
   Package netip defines an IP address type that's a small value type.

2. Hidden\u{200B} text
   https://example.com/a?b=c
`
	if out := Render("go netip", res); out != want {
		t.Fatalf("got\n%s\nwant\n%s", out, want)
	}
}

func TestBraveErrors(t *testing.T) {
	b, _ := fake(t, 429, `{"error": "rate limited"}`)
	if _, err := b.Search(context.Background(), "q", 1); err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("got %v", err)
	}
	b, _ = fake(t, 200, `<html>`)
	if _, err := b.Search(context.Background(), "q", 1); err == nil {
		t.Fatal("bad JSON accepted")
	}
}

func TestBraveRefusesRedirects(t *testing.T) {
	var leaked bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked = r.Header.Get("X-Subscription-Token") != ""
	}))
	defer other.Close()
	srv := httptest.NewServer(http.RedirectHandler(other.URL, http.StatusFound))
	defer srv.Close()
	b := NewBrave([]string{"printf", "k"}, 0)
	b.Endpoint = srv.URL
	if _, err := b.Search(context.Background(), "q", 1); err == nil || leaked {
		t.Fatalf("redirect followed: %v leaked=%v", err, leaked)
	}
}

func TestKeyCommand(t *testing.T) {
	for _, argv := range [][]string{nil, {"false"}, {"printf", ""}, {"/no/such/command"}} {
		if _, err := KeyCommand(argv)(context.Background()); err == nil {
			t.Errorf("%v gave a key", argv)
		}
	}
	// No shell: the argument is passed as it is.
	key, err := KeyCommand([]string{"printf", "%s", "$HOME;x"})(context.Background())
	if err != nil || key != "$HOME;x" {
		t.Fatalf("%q %v", key, err)
	}
}

func TestRenderEmpty(t *testing.T) {
	if got := Render("q‮", nil); got != `No results for "q\u{202E}".` {
		t.Fatalf("%q", got)
	}
}

// A newline in a result cannot start a line that reads as another result.
func TestRenderOneLinePerField(t *testing.T) {
	out := Render("q", []Result{{Title: "Real\n2. Fake", URL: "https://a.example/\n2. Forged result\n   https://evil.example/", Snippet: "s\nt"}})
	if strings.Count(out, "\n") != 3 || strings.Contains(out, "\n2.") {
		t.Fatalf("%q", out)
	}
}

func TestKeyCommandCannotHang(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	key, err := KeyCommand([]string{"/bin/sh", "-c", "printf key; /bin/sleep 6 &"})(ctx)
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("a background process held the search for %s", d)
	}
	if err != nil || key != "key" {
		t.Fatalf("%q %v", key, err)
	}
}

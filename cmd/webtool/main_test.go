package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/kirahowe/webtool/internal/envelope"
	"github.com/kirahowe/webtool/internal/extract"
	"github.com/kirahowe/webtool/internal/fetch"
	"github.com/kirahowe/webtool/internal/search"
	"github.com/kirahowe/webtool/internal/text"
)

func writeConfig(t testing.TB, body string) string {
	dir := t.TempDir()
	body = strings.ReplaceAll(body, "SNAPSHOTS", filepath.Join(dir, "snapshots"))
	p := filepath.Join(dir, "webtool.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const fullConfig = `{
  "snapshot_dir": "SNAPSHOTS",
  "fetch": {"timeout_seconds": 5, "max_body_bytes": 100000, "max_text_chars": 50},
  "search": {"backend": "brave", "api_key_command": ["printf", "k"]}
}`

func exec(a app, args []string, stdin string) (string, string, int) {
	var out, errOut bytes.Buffer
	code := a.run(args, strings.NewReader(stdin), &out, &errOut)
	return out.String(), errOut.String(), code
}

// fataler is what testing.T and rapid.T share.
type fataler interface {
	Helper()
	Fatalf(format string, args ...any)
}

// oneResult reads stdout as relay does: exactly one JSON object, with
// exactly the keys content, is_error and labels, and nothing after it.
func oneResult(t fataler, stdout string) envelope.Result {
	t.Helper()
	obj, err := envelope.Object([]byte(stdout), []string{"content", "is_error", "labels"}, nil)
	if err != nil {
		t.Fatalf("stdout is not exactly one result: %v\n%q", err, stdout)
	}
	var r envelope.Result
	if err := json.Unmarshal([]byte(stdout), &r); err != nil {
		t.Fatalf("%v", err)
	}
	if string(bytes.TrimSpace(obj["is_error"])) != "true" && string(bytes.TrimSpace(obj["is_error"])) != "false" {
		t.Fatalf("is_error %s", obj["is_error"])
	}
	for _, l := range r.Labels {
		if l != "contaminated" && l != "private" {
			t.Fatalf("label %q", l)
		}
	}
	return r
}

func call(input string) string {
	return `{"input": ` + input + `, "labels": ["private"], "call_id": "toolu_1"}`
}

type fakeFetcher struct{ res *fetch.Result }

func (f fakeFetcher) Fetch(_ context.Context, u string) (*fetch.Result, error) {
	if f.res == nil {
		return &fetch.Result{URL: u}, errors.New("could not connect")
	}
	r := *f.res
	r.URL = u
	return &r, nil
}

type fakeBackend struct{ results []search.Result }

func (b fakeBackend) Search(_ context.Context, q string, n int) ([]search.Result, error) {
	if q == "fail" {
		return nil, errors.New("Brave Search answered 429 Too Many Requests")
	}
	return b.results[:min(n, len(b.results))], nil
}

func fakeApp(res *fetch.Result) app {
	return app{
		newFetcher: func(Config, io.Writer) (pageFetcher, error) { return fakeFetcher{res}, nil },
		newBackend: func(SearchConfig) search.Backend {
			return fakeBackend{[]search.Result{{Title: "One", URL: "https://one.example/", Snippet: "first"}, {Title: "Two", URL: "https://two.example/"}}}
		},
	}
}

func TestMalformedCallsGetOneResult(t *testing.T) {
	cfg := writeConfig(t, fullConfig)
	bad := []string{
		"", "not json", "{}", "[]", "null",
		`{"input": {}, "labels": []}`,
		`{"input": {}, "labels": [], "call_id": "x"} {"input": {}}`,
		`{"input": {}, "labels": [], "call_id": "x", "call_id": "y"}`,
		`{"input": {}, "labels": [], "call_id": "x", "Call_ID": "y"}`,
		`{"input": [], "labels": [], "call_id": "x"}`,
		`{"input": {}, "labels": ["secret"], "call_id": "x"}`,
		`{"input": {}, "labels": "private", "call_id": "x"}`,
		`{"input": {"url": "a", "url": "b"}, "labels": [], "call_id": "x"}`,
		call(`{}`), call(`{"url": 5}`), call(`{"url": ""}`), call(`{"url": "https://a.com/", "headers": {}}`),
		call(`{"URL": "https://a.com/"}`),
	}
	for _, in := range bad {
		out, _, code := exec(fakeApp(nil), []string{"--config", cfg, "fetch"}, in)
		if code != 0 {
			t.Errorf("%q: exit %d", in, code)
			continue
		}
		r := oneResult(t, out)
		if !r.IsError || len(r.Labels) != 0 {
			t.Errorf("%q: %+v", in, r)
		}
	}
}

func TestAnyInputGetsOneResult(t *testing.T) {
	cfg := writeConfig(t, fullConfig)
	rapid.Check(t, func(t *rapid.T) {
		cmd := rapid.SampledFrom([]string{"search", "fetch", "check", "ask"}).Draw(t, "command")
		var in string
		if rapid.Bool().Draw(t, "envelope") {
			key := rapid.SampledFrom([]string{"url", "query", "count", "n", "items", "quotes", "x"}).Draw(t, "key")
			val := rapid.SampledFrom([]string{`"https://a.example.com/"`, `""`, `3`, `[]`, `{}`, `null`, `[{"url": "u"}]`, `"‮"`}).Draw(t, "val")
			in = call(`{"` + key + `": ` + val + `}`)
		} else {
			in = rapid.String().Draw(t, "stdin")
		}
		out, _, code := exec(fakeApp(&fetch.Result{Status: 200}), []string{"--config", cfg, cmd}, in)
		if code != 0 {
			t.Fatalf("exit %d", code)
		}
		oneResult(t, out)
	})
}

// The production app refuses these without touching the network.
func TestFetchRefusals(t *testing.T) {
	cfg := writeConfig(t, fullConfig)
	for u, want := range map[string]string{
		"http://127.0.0.1:2586/mytopic?message=hi": "refused: port 2586",
		"http://127.0.0.1/":                        "refused: 127.0.0.1 is a loopback address",
		"http://10.0.0.1/":                         "refused: 10.0.0.1 is a private address",
		"http://2130706433/":                       "refused:",
		"http://router.lan/":                       "refused:",
		"http://box.tail1234.ts.net/":              "refused:",
		"file:///etc/passwd":                       "refused: only http and https",
		"http://[::ffff:192.168.1.1]/":             "refused: 192.168.1.1 is a private address",
	} {
		out, _, code := exec(newApp(), []string{"--config", cfg, "fetch"}, call(`{"url": "`+u+`"}`))
		r := oneResult(t, out)
		if code != 0 || !r.IsError || !strings.HasPrefix(r.Content, want) || !slices.Equal(r.Labels, contaminated) {
			t.Errorf("%s: %d %+v", u, code, r)
		}
	}
}

func TestFetchOutput(t *testing.T) {
	cfg := writeConfig(t, fullConfig)
	page := strings.Repeat("word ", 20) + "‮end"
	res := &fetch.Result{
		FinalURL: "https://b.example.com/", Status: 200, ContentType: "text/html", SHA256: "abc",
		Redirects: []fetch.Hop{{URL: "https://a.example.com/", Status: 301}}, Truncated: true,
		Page: text.Page{Title: "T​", Text: page, Hidden: 7},
	}
	out, _, _ := exec(fakeApp(res), []string{"--config", cfg, "fetch"}, call(`{"url": "https://a.example.com/"}`))
	r := oneResult(t, out)
	want := `url: https://a.example.com/
final_url: https://b.example.com/
status: 200
content_type: text/html
sha256: abc
redirects: https://a.example.com/ (301) -> https://b.example.com/
title: T\u{200B}
warning: 7 characters of hidden text dropped
warning: 2 invisible or formatting characters escaped as \u{XXXX}
warning: the body is longer than 100000 bytes and only that much was read

` + strings.Repeat("word ", 10) + `

[text truncated: showing 50 of 104 characters]`
	if r.IsError || r.Content != want || !slices.Equal(r.Labels, contaminated) {
		t.Fatalf("got\n%s\nwant\n%s", r.Content, want)
	}
}

func TestSearch(t *testing.T) {
	cfg := writeConfig(t, fullConfig)
	out, _, _ := exec(fakeApp(nil), []string{"--config", cfg, "search"}, call(`{"query": "go", "count": 1}`))
	r := oneResult(t, out)
	if r.IsError || r.Content != "1. One\n   https://one.example/\n   first\n" || !slices.Equal(r.Labels, contaminated) {
		t.Fatalf("%+v", r)
	}
	out, _, _ = exec(fakeApp(nil), []string{"--config", cfg, "search"}, call(`{"query": "go", "count": "2"}`))
	if r := oneResult(t, out); r.IsError || !strings.Contains(r.Content, "2. Two") {
		t.Fatalf("%+v", r)
	}
	out, _, _ = exec(fakeApp(nil), []string{"--config", cfg, "search"}, call(`{"query": "fail"}`))
	if r := oneResult(t, out); !r.IsError || !strings.Contains(r.Content, "429") {
		t.Fatalf("%+v", r)
	}
	for _, in := range []string{`{"query": ""}`, `{"query": "  "}`, `{"query": "go", "count": 0}`, `{"query": "go", "count": 21}`,
		`{"query": "go", "count": 1.5}`, `{"q": "go"}`, `{"query": "` + strings.Repeat("a", 401) + `"}`} {
		out, _, _ := exec(fakeApp(nil), []string{"--config", cfg, "search"}, call(in))
		if r := oneResult(t, out); !r.IsError || len(r.Labels) != 0 {
			t.Errorf("%s: %+v", in, r)
		}
	}
}

func TestCheck(t *testing.T) {
	cfg := writeConfig(t, fullConfig)
	out, _, _ := exec(newApp(), []string{"--config", cfg, "check"},
		call(`{"items": [{"url": "http://192.168.0.1/", "quotes": ["admin"]}, {"url": "http://localhost/"}]}`))
	r := oneResult(t, out)
	var rep struct {
		Items []struct {
			URL     string `json:"url"`
			Fetched bool   `json:"fetched"`
			Error   string `json:"error"`
			Quotes  []struct {
				Found bool `json:"found"`
			} `json:"quotes"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(r.Content), &rep); err != nil {
		t.Fatalf("%v: %s", err, r.Content)
	}
	if r.IsError || len(rep.Items) != 2 || rep.Items[0].Fetched || !strings.HasPrefix(rep.Items[0].Error, "refused: 192.168.0.1") ||
		rep.Items[0].Quotes[0].Found || !strings.HasPrefix(rep.Items[1].Error, "refused:") {
		t.Fatalf("%+v", rep)
	}
}

func TestOperatorErrorsPrintNothing(t *testing.T) {
	noSearch := writeConfig(t, `{"snapshot_dir": "SNAPSHOTS"}`)
	cases := []struct {
		args []string
		code int
	}{
		{nil, 2},
		{[]string{"fetch"}, 2},
		{[]string{"--config", noSearch}, 2},
		{[]string{"--config", noSearch, "browse"}, 2},
		{[]string{"--config", noSearch, "fetch", "extra"}, 2},
		{[]string{"--config", noSearch, "search"}, 1},
		{[]string{"--config", "/no/such/file", "fetch"}, 1},
		{[]string{"--config", writeConfig(t, `{"snapshot_dir": "relative"}`), "fetch"}, 1},
		{[]string{"--config", writeConfig(t, `{"snapshot_dir": "SNAPSHOTS", "fetch": {"timeout": 5}}`), "fetch"}, 1},
		{[]string{"--config", writeConfig(t, `{"snapshot_dir": "SNAPSHOTS", "search": {"backend": "bing", "api_key_command": ["x"]}}`), "search"}, 1},
		{[]string{"--config", writeConfig(t, `{"snapshot_dir": "SNAPSHOTS"} {}`), "fetch"}, 1},
	}
	for _, c := range cases {
		out, errOut, code := exec(newApp(), c.args, call(`{"url": "http://10.0.0.1/"}`))
		if code != c.code || out != "" || errOut == "" {
			t.Errorf("%v: exit %d, stdout %q, stderr %q", c.args, code, out, errOut)
		}
	}
}

func TestExampleConfigLoads(t *testing.T) {
	c, err := loadConfig("../../webtool.example.json")
	if err != nil {
		t.Fatal(err)
	}
	if c.searchReady() != nil || c.Fetch.TimeoutSeconds != 20 || c.Fetch.MaxBodyBytes != 5_000_000 {
		t.Fatalf("%+v", c)
	}
}

func TestTitleWithinTextLimit(t *testing.T) {
	cfg := writeConfig(t, fullConfig)
	res := &fetch.Result{Status: 200, Page: text.Page{Title: strings.Repeat("t", 900), Text: "x", Cut: true, Deep: true}}
	out, _, _ := exec(fakeApp(res), []string{"--config", cfg, "fetch"}, call(`{"url": "https://a.example.com/"}`))
	r := oneResult(t, out)
	if !strings.Contains(r.Content, "title: "+strings.Repeat("t", 50)+"\n") ||
		!strings.Contains(r.Content, "the rest was not read") || !strings.Contains(r.Content, "too deeply") {
		t.Fatalf("%s", r.Content)
	}
}

// A value in the header lines is one line: a newline in a URL or title
// cannot start a line of its own, such as a fake warning.
func TestHeaderLinesStayOneLine(t *testing.T) {
	cfg := writeConfig(t, fullConfig)
	res := &fetch.Result{FinalURL: "https://b.example.com/", Status: 200, Page: text.Page{Title: "T\nwarning: fake", Text: "x"}}
	out, _, _ := exec(fakeApp(res), []string{"--config", cfg, "fetch"}, call(`{"url": "https://a.example.com/\nstatus: 200"}`))
	r := oneResult(t, out)
	for _, l := range strings.Split(r.Content, "\n") {
		if strings.HasPrefix(l, "warning: fake") || strings.HasPrefix(l, "status: 200") && !strings.Contains(r.Content, "status: 200\ncontent_type") {
			t.Fatalf("a value broke its line:\n%s", r.Content)
		}
	}
	if !strings.Contains(r.Content, `url: https://a.example.com/\u{000A}status: 200`) {
		t.Fatalf("%s", r.Content)
	}
}

// The test binary stands in for webtool's child, as main does.
func TestMain(m *testing.M) {
	if len(os.Args) == 2 && os.Args[1] == extract.Command {
		os.Exit(extract.Child(os.Stdin, os.Stdout))
	}
	os.Exit(m.Run())
}

// childFetcher reads a fixed body through a real child, as a fetch would.
type childFetcher struct {
	body   []byte
	runner extract.Runner
	ctx    time.Duration
}

func (c childFetcher) Fetch(ctx context.Context, u string) (*fetch.Result, error) {
	ctx, cancel := context.WithTimeout(ctx, c.ctx)
	defer cancel()
	p, err := c.runner.Extract(ctx, c.body, "text/html", nil, len(c.body))
	if err != nil {
		return &fetch.Result{URL: u}, err
	}
	return &fetch.Result{URL: u, Status: 200, Page: p}, nil
}

// A page that runs the child out of memory or time still gets exactly
// one envelope, with a plain reason.
func TestCostlyPageOneEnvelope(t *testing.T) {
	cfg := writeConfig(t, fullConfig)
	var b strings.Builder
	b.WriteString("<div>")
	for i := 0; i < 500; i++ {
		fmt.Fprintf(&b, "<b id=%d%s>", i, strings.Repeat(" a", 1000))
	}
	b.WriteString(strings.Repeat("</div><div>x", 5000))
	for want, cf := range map[string]childFetcher{
		"the page was too costly to read":   {[]byte(b.String()), extract.Runner{Argv: []string{os.Args[0], extract.Command}, Memory: 64 << 20}, time.Minute},
		"timed out reading the page's text": {[]byte(b.String()), extract.Runner{Argv: []string{os.Args[0], extract.Command}, Memory: 8 << 30}, 300 * time.Millisecond},
	} {
		a := fakeApp(nil)
		a.newFetcher = func(Config, io.Writer) (pageFetcher, error) { return cf, nil }
		start := time.Now()
		out, _, code := exec(a, []string{"--config", cfg, "fetch"}, call(`{"url": "https://a.example.com/"}`))
		r := oneResult(t, out)
		if code != 0 || !r.IsError || r.Content != want {
			t.Errorf("want %q: %d %+v after %s", want, code, r, time.Since(start))
		}
	}
}

func TestTokenCapWarning(t *testing.T) {
	p := text.HTML(context.Background(), []byte(strings.Repeat("<i>w</i> ", text.MaxTokens/3+1)), nil, 5_000_000)
	out := renderPage(&fetch.Result{Status: 200, Page: p}, 5_000_000, 20_000)
	if !strings.Contains(out, fmt.Sprintf("more than %d tags", text.MaxTokens)) || strings.Contains(out, "characters of text (max_body_bytes)") {
		t.Fatalf("%s", out[:strings.Index(out, "\n\n")])
	}
}

// The production path reads pages in a sandboxed child: dropping the
// child or the sandbox fails here.
func TestProductionReadsInASandboxedChild(t *testing.T) {
	cfg, err := loadConfig(writeConfig(t, fullConfig))
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	fc, r, err := productionFetchConfig(cfg, &stderr)
	if runtime.GOOS != "darwin" {
		if cfg.Fetch.ExtractSandbox != "off" || err != nil || fc.Extract == nil || r.Sandbox {
			t.Fatalf("%v %+v", err, r)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Fetch.ExtractSandbox != "required" || !r.Sandbox || fc.Extract == nil {
		t.Fatalf("sandbox %q, runner %+v, extract set %v", cfg.Fetch.ExtractSandbox, r, fc.Extract != nil)
	}
	if argv := r.Command(); argv[0] != extract.SandboxPath || argv[len(argv)-1] != extract.Command {
		t.Fatalf("argv %q", argv)
	}
	// And it works: the test binary stands in as the child (TestMain).
	p, err := fc.Extract(context.Background(), []byte("<p>through the child</p>"), "text/html", nil, 1000)
	if err != nil || p.Text != "through the child" {
		t.Fatalf("%v %+v (stderr %s)", err, p, stderr.String())
	}
}

func TestSandboxSetting(t *testing.T) {
	off, err := loadConfig(writeConfig(t, `{"snapshot_dir": "SNAPSHOTS", "fetch": {"extract_sandbox": "off"}}`))
	if err != nil || off.Fetch.ExtractSandbox != "off" {
		t.Fatalf("%v %+v", err, off.Fetch)
	}
	if _, r, err := productionFetchConfig(off, io.Discard); err != nil || r.Sandbox || r.Command()[0] == extract.SandboxPath {
		t.Fatalf("off still sandboxed: %v %+v", err, r)
	}
	for _, bad := range []string{
		`{"snapshot_dir": "SNAPSHOTS", "fetch": {"extract_sandbox": "maybe"}}`,
		`{"snapshot_dir": "SNAPSHOTS", "fetch": {"max_body_bytes": 268435457}}`,
	} {
		if _, err := loadConfig(writeConfig(t, bad)); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

// When the reader has cut the text, the note gives its full length.
func TestTruncationNoteUsesFullLength(t *testing.T) {
	out := renderPage(&fetch.Result{Status: 200, Page: text.Page{Text: strings.Repeat("a", 50), TextRunes: 900}}, 1000, 50)
	if !strings.Contains(out, "showing 50 of 900 characters") {
		t.Fatalf("%s", out)
	}
}

// newApp's own fetcher reads through the child: a page that blows past
// the child's memory limit is refused as too costly, which reading in
// this process could not do.
func TestNewAppReadsThroughTheChild(t *testing.T) {
	cfg, err := loadConfig(writeConfig(t, `{"snapshot_dir": "SNAPSHOTS", "fetch": {"max_read_memory_bytes": 67108864}}`))
	if err != nil {
		t.Fatal(err)
	}
	pf, err := newApp().newFetcher(cfg, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	f, ok := pf.(*fetch.Fetcher)
	if !ok {
		t.Fatalf("newApp built a %T", pf)
	}
	var b strings.Builder
	b.WriteString("<div>")
	for i := 0; i < 500; i++ {
		fmt.Fprintf(&b, "<b id=%d%s>", i, strings.Repeat(" a", 1000))
	}
	b.WriteString(strings.Repeat("</div><div>x", 5000))
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := f.Extractor()(ctx, []byte(b.String()), "text/html", nil, b.Len()); !errors.Is(err, extract.ErrTooCostly) {
		t.Fatalf("got %v", err)
	}
}

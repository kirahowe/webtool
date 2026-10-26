// Command webtool gives relay's research roles the web, as an external
// tool on relay's envelope protocol:
//
//	webtool --config FILE search|fetch|check|ask
//
// The call comes on stdin and exactly one result goes to stdout;
// diagnostics go to stderr. Exit status: 0 whenever a result was printed,
// refusals and bad input included; 1 when the config or the machine
// stops it running at all, with nothing on stdout; 2 for a usage error.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kirahowe/webtool/internal/check"
	"github.com/kirahowe/webtool/internal/envelope"
	"github.com/kirahowe/webtool/internal/extract"
	"github.com/kirahowe/webtool/internal/fetch"
	"github.com/kirahowe/webtool/internal/search"
	"github.com/kirahowe/webtool/internal/text"
)

// contaminated is the label on everything webtool returns from the web.
var contaminated = []string{"contaminated"}

// MaxQueryRunes is Brave's limit on a query.
const MaxQueryRunes = 400

// DefaultCount is how many search results come back when the call does
// not say.
const DefaultCount = 10

type pageFetcher interface {
	Fetch(ctx context.Context, url string) (*fetch.Result, error)
}

// app holds what the commands are built from, so tests can swap the
// network side. Production uses newApp.
type app struct {
	newFetcher func(Config, io.Writer) (pageFetcher, error)
	newBackend func(SearchConfig) search.Backend
}

func newApp() app {
	return app{
		newFetcher: func(cfg Config, stderr io.Writer) (pageFetcher, error) {
			fc, _, err := productionFetchConfig(cfg, stderr)
			if err != nil {
				return nil, err
			}
			return fetch.New(fc)
		},
		newBackend: func(c SearchConfig) search.Backend {
			if c.Backend == "fixture" {
				return fixtureBackend(c)
			}
			return search.NewBrave(c.APIKeyCommand, time.Duration(c.TimeoutSeconds)*time.Second)
		},
	}
}

// productionFetchConfig is the fetch configuration webtool runs with:
// pages are read in a child process that can be killed and capped
// (internal/extract), sandboxed unless the config says off. It refuses to
// run when the sandbox is required and cannot be applied, since reading
// unsandboxed must never happen silently.
func productionFetchConfig(cfg Config, stderr io.Writer) (fetch.Config, extract.Runner, error) {
	fc := cfg.fetchConfig()
	sandbox := cfg.Fetch.ExtractSandbox == "required"
	if sandbox {
		if err := extract.SandboxAvailable(); err != nil {
			return fetch.Config{}, extract.Runner{}, fmt.Errorf("%v; set fetch.extract_sandbox to \"off\" only to test inside another sandbox", err)
		}
	}
	r, err := extract.Self(extract.Runner{
		Memory:    cfg.Fetch.MaxReadMemoryBytes,
		TextChars: cfg.Fetch.MaxTextChars,
		BodyBytes: int(cfg.Fetch.MaxBodyBytes),
		Sandbox:   sandbox,
		Stderr:    stderr,
	})
	if err != nil {
		return fetch.Config{}, extract.Runner{}, err
	}
	fc.Extract = r.Extract
	return fc, r, nil
}

func main() {
	// The hidden subcommand that reads one page's text for its parent.
	if len(os.Args) == 2 && os.Args[1] == extract.Command {
		os.Exit(extract.Child(os.Stdin, os.Stdout))
	}
	os.Exit(newApp().run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func (a app) run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("webtool", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "read settings from `FILE` (required)")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: webtool --config FILE search|fetch|check|ask")
		fmt.Fprintln(fs.Output(), "The call comes on stdin in relay's envelope protocol; see the README.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 || *configPath == "" {
		fs.Usage()
		return 2
	}
	command := fs.Arg(0)
	if command != "search" && command != "fetch" && command != "check" && command != "ask" {
		fmt.Fprintf(stderr, "webtool: unknown command %q\n", command)
		fs.Usage()
		return 2
	}
	cfg, err := loadConfig(*configPath)
	if err != nil {
		fmt.Fprintln(stderr, "webtool:", err)
		return 1
	}
	if command == "search" || command == "ask" {
		if err := cfg.searchReady(); err != nil {
			fmt.Fprintln(stderr, "webtool:", err)
			return 1
		}
	}

	var res envelope.Result
	call, err := envelope.Read(stdin)
	if err != nil {
		res = badCall(err)
	} else {
		ctx := context.Background()
		switch command {
		case "search":
			res = a.search(ctx, cfg, call.Input)
		case "fetch", "check", "ask":
			f, err := a.newFetcher(cfg, stderr)
			if err != nil {
				fmt.Fprintln(stderr, "webtool:", err)
				return 1
			}
			switch command {
			case "fetch":
				res = fetchPage(ctx, f, cfg, call.Input)
			case "check":
				res = checkItems(ctx, f, call.Input)
			default:
				res = a.ask(ctx, cfg, f, call.Input)
			}
		}
	}
	if err := envelope.Write(stdout, res); err != nil {
		fmt.Fprintln(stderr, "webtool:", err)
		return 1
	}
	return 0
}

// badCall answers a call that could not be read. Nothing was fetched, so
// it carries no label.
func badCall(err error) envelope.Result {
	return envelope.Result{Content: "webtool: " + err.Error(), IsError: true, Labels: []string{}}
}

func failed(err error) envelope.Result {
	return envelope.Result{Content: text.EscapeOnly(err.Error()), IsError: true, Labels: contaminated}
}

func (a app) search(ctx context.Context, cfg Config, in map[string]json.RawMessage) envelope.Result {
	obj, err := checkKeys(in, []string{"query"}, []string{"count"})
	if err != nil {
		return badCall(err)
	}
	query, err := envelope.String(obj["query"])
	query = strings.TrimSpace(query)
	if err != nil || query == "" || utf8.RuneCountInString(query) > MaxQueryRunes {
		return badCall(fmt.Errorf("query must be a string of 1 to %d characters", MaxQueryRunes))
	}
	count := DefaultCount
	if raw, ok := obj["count"]; ok {
		if count, err = envelope.Int(raw); err != nil || count < 1 || count > search.MaxCount {
			return badCall(fmt.Errorf("count must be a whole number from 1 to %d", search.MaxCount))
		}
	}
	results, err := a.newBackend(cfg.Search).Search(ctx, query, count)
	if err != nil {
		return failed(err)
	}
	return envelope.Result{Content: search.Render(query, results), Labels: contaminated}
}

func fetchPage(ctx context.Context, f pageFetcher, cfg Config, in map[string]json.RawMessage) envelope.Result {
	obj, err := checkKeys(in, []string{"url"}, nil)
	if err != nil {
		return badCall(err)
	}
	u, err := envelope.String(obj["url"])
	if err != nil || u == "" || len(u) > check.MaxURLBytes {
		return badCall(fmt.Errorf("url must be a string of 1 to %d bytes", check.MaxURLBytes))
	}
	res, err := f.Fetch(ctx, u)
	if err != nil {
		return failed(err)
	}
	return envelope.Result{Content: renderPage(res, cfg.Fetch.MaxBodyBytes, cfg.Fetch.MaxTextChars), Labels: contaminated}
}

func checkItems(ctx context.Context, f pageFetcher, in map[string]json.RawMessage) envelope.Result {
	items, err := check.ParseInput(in)
	if err != nil {
		return badCall(err)
	}
	b, err := json.Marshal(check.Run(ctx, f, items))
	if err != nil {
		return failed(err)
	}
	return envelope.Result{Content: string(b), Labels: contaminated}
}

// checkKeys applies a command's key rules to the input object.
func checkKeys(in map[string]json.RawMessage, required, optional []string) (map[string]json.RawMessage, error) {
	for k := range in {
		if !slices.Contains(required, k) && !slices.Contains(optional, k) {
			return nil, errors.New("there is no argument called " + text.EscapeOnly(k))
		}
	}
	for _, k := range required {
		if _, ok := in[k]; !ok {
			return nil, errors.New("the input needs " + k)
		}
	}
	return in, nil
}

// renderPage is what the agent reads of a fetched page: where it came
// from, warnings, then its text, cut to maxChars characters.
func renderPage(res *fetch.Result, maxBody int64, maxChars int) string {
	var b strings.Builder
	// Each value is one line: a newline or tab in it is escaped too, so
	// nothing in a URL or title can start a line of its own.
	line := func(k, v string) {
		v = strings.NewReplacer("\n", `\u{000A}`, "\t", `\u{0009}`).Replace(text.EscapeOnly(v))
		fmt.Fprintf(&b, "%s: %s\n", k, v)
	}
	line("url", res.URL)
	if res.Moved != "" {
		line("status", fmt.Sprint(res.Status))
		// The address stands alone on its line, exactly as it is to be
		// fetched: relay lets a later fetch pass only a URL it was shown.
		fmt.Fprintf(&b, "\nThis page has moved. It was not followed. Its new address is:\n\n%s\n", res.Moved)
		return b.String()
	}
	line("final_url", res.FinalURL)
	line("status", fmt.Sprint(res.Status))
	line("content_type", res.ContentType)
	line("sha256", res.SHA256)
	if title := []rune(res.Page.Title); len(title) > 0 {
		line("title", string(title[:min(len(title), maxChars)]))
	}

	body := []rune(res.Page.Text)
	shown := body
	if len(body) > maxChars {
		shown = body[:maxChars]
	}
	// The reader may have cut the text already; TextRunes is its length.
	total := max(len(body), res.Page.TextRunes)
	_, invisible := text.Escape(res.Page.Text + res.Page.Title)
	escaped, _ := text.Escape(string(shown))
	if res.Page.Hidden > 0 {
		fmt.Fprintf(&b, "warning: %d characters of hidden text dropped\n", res.Page.Hidden)
	}
	if invisible > 0 {
		fmt.Fprintf(&b, "warning: %d invisible or formatting characters escaped as \\u{XXXX}\n", invisible)
	}
	if res.Truncated {
		fmt.Fprintf(&b, "warning: the body is longer than %d bytes and only that much was read\n", maxBody)
	}
	if res.Page.Cut {
		fmt.Fprintf(&b, "warning: reading the page stopped at %d characters of text (max_body_bytes), and the rest was not read\n", maxBody)
	}
	if res.Page.TooManyTokens {
		fmt.Fprintf(&b, "warning: the page has more than %d tags, text runs and comments, and was read only that far\n", text.MaxTokens)
	}
	if res.Page.Deep {
		b.WriteString("warning: the page nests elements too deeply, and what lies deeper was not read\n")
	}
	b.WriteString("\n")
	b.WriteString(escaped)
	if len(shown) < total {
		fmt.Fprintf(&b, "\n\n[text truncated: showing %d of %d characters]", len(shown), total)
	}
	return b.String()
}

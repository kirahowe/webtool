// Package search runs a web search through a backend and renders the
// results for an agent. Brave Search is the one backend.
package search

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/html"

	"github.com/kirahowe/webtool/internal/text"
)

// Result is one search hit.
type Result struct {
	Title   string
	URL     string
	Snippet string
}

// Backend runs a search.
type Backend interface {
	Search(ctx context.Context, query string, count int) ([]Result, error)
}

// BraveEndpoint is Brave Search's web search API.
const BraveEndpoint = "https://api.search.brave.com/res/v1/web/search"

// MaxCount is the most results Brave returns for one query.
const MaxCount = 20

// maxResponseBytes bounds what is read of Brave's answer.
const maxResponseBytes = 4 << 20

// Brave searches with the Brave Search API.
type Brave struct {
	Endpoint string
	Key      func(context.Context) (string, error)
	Client   *http.Client
}

// NewBrave is the Brave backend production uses: the real endpoint, the
// key from keyCommand, no proxy, and no redirects, since a redirect would
// carry the key's header to wherever it pointed.
func NewBrave(keyCommand []string, timeout time.Duration) *Brave {
	return &Brave{
		Endpoint: BraveEndpoint,
		Key:      KeyCommand(keyCommand),
		Client: &http.Client{
			Timeout:   timeout,
			Transport: &http.Transport{Proxy: nil, ForceAttemptHTTP2: true, MaxResponseHeaderBytes: 64 << 10},
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return errors.New("Brave Search answered with a redirect, which is not followed")
			},
		},
	}
}

// KeyCommand runs a command, without a shell, and takes its output as the
// API key. The command gets only PATH and HOME from the environment.
func KeyCommand(argv []string) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		if len(argv) == 0 {
			return "", errors.New("api_key_command is empty")
		}
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		cmd.Env = baseEnv()
		// A command that leaves a process holding its stdout cannot keep
		// the search waiting: a second after it exits, or at the deadline,
		// the pipe is closed.
		cmd.WaitDelay = time.Second
		var out bytes.Buffer
		cmd.Stdout = &out
		// ErrWaitDelay means the command exited 0 but left its stdout
		// held open; what it printed before exiting is the key.
		if err := cmd.Run(); err != nil && !errors.Is(err, exec.ErrWaitDelay) {
			return "", fmt.Errorf("api_key_command failed: %v", err)
		}
		key := strings.TrimSpace(out.String())
		if key == "" {
			return "", errors.New("api_key_command printed nothing")
		}
		return key, nil
	}
}

func baseEnv() []string {
	var env []string
	for _, k := range []string{"PATH", "HOME"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// braveResponse is the part of Brave's answer used here.
type braveResponse struct {
	Web struct {
		Results []struct {
			Title       string `json:"title"`
			URL         string `json:"url"`
			Description string `json:"description"`
		} `json:"results"`
	} `json:"web"`
}

// Search asks Brave for count results.
func (b *Brave) Search(ctx context.Context, query string, count int) ([]Result, error) {
	key, err := b.Key(ctx)
	if err != nil {
		return nil, err
	}
	q := url.Values{"q": {query}, "count": {strconv.Itoa(count)}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.Endpoint+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Subscription-Token", key)
	resp, err := b.Client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // the URL is the endpoint and the query, nothing new
		}
		return nil, fmt.Errorf("Brave Search could not be reached: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("Brave Search's answer could not be read: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Brave Search answered %d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	var br braveResponse
	if err := json.Unmarshal(body, &br); err != nil {
		return nil, errors.New("Brave Search's answer was not the JSON expected")
	}
	var out []Result
	for _, r := range br.Web.Results {
		out = append(out, Result{Title: stripTags(r.Title), URL: r.URL, Snippet: stripTags(r.Description)})
	}
	return out, nil
}

// stripTags turns Brave's snippet markup (<strong>, entities) into text.
func stripTags(s string) string {
	z := html.NewTokenizer(strings.NewReader(s))
	var b strings.Builder
	for {
		switch z.Next() {
		case html.ErrorToken:
			return strings.Join(strings.Fields(b.String()), " ")
		case html.TextToken:
			b.Write(z.Text())
		}
	}
}

// oneLine escapes invisible characters, and newlines and tabs too, so
// nothing in a result can start a line that reads as another result.
func oneLine(s string) string {
	return strings.NewReplacer("\n", `\u{000A}`, "\t", `\u{0009}`).Replace(text.EscapeOnly(s))
}

// Render lists results for the agent, numbered, with title, URL and
// snippet, and invisible characters escaped.
func Render(query string, results []Result) string {
	if len(results) == 0 {
		return "No results for \"" + text.EscapeOnly(query) + "\"."
	}
	var b strings.Builder
	for i, r := range results {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "%d. %s\n   %s\n", i+1, oneLine(r.Title), oneLine(r.URL))
		if r.Snippet != "" {
			fmt.Fprintf(&b, "   %s\n", oneLine(r.Snippet))
		}
	}
	return b.String()
}

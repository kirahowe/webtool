package extract

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The test binary stands in for webtool: run with Command, it is a child.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == Command {
		os.Exit(Child(os.Stdin, os.Stdout))
	}
	os.Exit(m.Run())
}

func runner(memory int64) Runner {
	return Runner{Argv: []string{os.Args[0], Command}, Memory: memory}
}

func TestRoundTrip(t *testing.T) {
	base, _ := url.Parse("https://example.com/a/")
	p, err := runner(DefaultMemory).Extract(context.Background(), []byte(`<title>T</title><p>Hello <a href="b">there</a></p>`), "text/html", base, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if p.Title != "T" || p.Text != "Hello there <https://example.com/a/b>" || p.Visible != "Hello there" {
		t.Fatalf("%+v", p)
	}
	p, err = runner(DefaultMemory).Extract(context.Background(), []byte("plain\x00text"), "text/plain", nil, 1000)
	if err != nil || p.Text != "plain\x00text" {
		t.Fatalf("%v %+v", err, p)
	}
}

// The reviewer's construction: formatting elements the parser rebuilds
// for every block grow its memory without bound.
func afe(n, attrs, m int) []byte {
	var ab strings.Builder
	for j := 0; j < attrs; j++ {
		fmt.Fprintf(&ab, " a%d", j)
	}
	var b strings.Builder
	b.WriteString("<div>")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "<b id=%d%s>", i, ab.String())
	}
	for i := 0; i < m; i++ {
		b.WriteString("</div><div>x")
	}
	return []byte(b.String())
}

func TestMemoryCapped(t *testing.T) {
	body := afe(500, 1000, 200)
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	// The memory cap stops the child, not the deadline: the deadline here
	// is far longer than the child takes to reach the cap (well under a
	// second, or some seconds under the race detector).
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	start := time.Now()
	_, err := runner(48<<20).Extract(ctx, body, "text/html", nil, len(body))
	// ErrTooCostly is the child's exit status 3, its watchdog.
	if !errors.Is(err, ErrTooCostly) || ctx.Err() != nil {
		t.Fatalf("got %v after %s", err, time.Since(start))
	}
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	if grew := int64(after.HeapInuse) - int64(before.HeapInuse); grew > 64<<20 {
		t.Fatalf("the parent's heap grew %d MB", grew>>20)
	}
}

func TestDeadlineKills(t *testing.T) {
	body := afe(500, 1000, 5000)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := runner(8<<30).Extract(ctx, body, "text/html", nil, len(body))
	// ErrTimedOut is the kill at the deadline, not the watchdog.
	if !errors.Is(err, ErrTimedOut) {
		t.Fatalf("got %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("the deadline was not honoured: %s", d)
	}
}

// Whatever a child writes, only one well-formed Page with exit 0 is a
// Page: not extra data, not unknown keys, not too much, not a crash.
func TestStrictReadBack(t *testing.T) {
	for name, script := range map[string]string{
		"trailing":  `printf '{"Text":"a"} {"Text":"b"}'`,
		"unknown":   `printf '{"Text":"a","Evil":1}'`,
		"exit 1":    `printf '{"Text":"a"}'; exit 1`,
		"garbage":   `printf 'Text: a'`,
		"too large": `head -c 3000000 /dev/zero | tr '\\0' a`,
		"killed":    `kill -9 $$`,
	} {
		r := Runner{Argv: []string{"/bin/sh", "-c", "cat >/dev/null; " + script}, Memory: DefaultMemory}
		if p, err := r.Extract(context.Background(), []byte("<p>x</p>"), "text/html", nil, 10); err == nil {
			t.Errorf("%s: accepted %+v", name, p)
		}
	}
	r := Runner{Argv: []string{"/bin/sh", "-c", `cat >/dev/null; printf '{"Text":"a"}'`}, Memory: DefaultMemory}
	if p, err := r.Extract(context.Background(), nil, "text/html", nil, 10); err != nil || p.Text != "a" {
		t.Fatalf("a well-formed page was refused: %v", err)
	}
}

// The child has no environment (sh adds PWD, SHLVL and _ of its own), and
// on macOS no network and no file writes.
func TestChildConfined(t *testing.T) {
	r := Runner{Argv: []string{"/bin/sh", "-c", `cat >/dev/null; if /usr/bin/env | grep -qv '^PWD=\|^SHLVL=\|^_='; then exit 1; fi; printf '{"Text":"clean"}'`}, Memory: DefaultMemory}
	if p, err := r.Extract(context.Background(), nil, "text/html", nil, 10); err != nil || p.Text != "clean" {
		t.Fatalf("the child saw an environment: %v %+v", err, p)
	}
	if runtime.GOOS != "darwin" {
		t.Skip("the network sandbox is macOS only")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	probe := func(cmd string) error {
		argv := Runner{Argv: []string{"/bin/sh", "-c", cmd}, Sandbox: true}.Command()
		return exec.Command(argv[0], argv[1:]...).Run()
	}
	if err := exec.Command("/usr/bin/curl", "-sf", "-m", "2", srv.URL).Run(); err != nil {
		t.Skipf("curl cannot reach the test server outside the sandbox: %v", err)
	}
	if probe("/usr/bin/curl -sf -m 2 http://127.0.0.1:"+port+"/") == nil {
		t.Error("the sandboxed child reached the network")
	}
	dir := t.TempDir()
	if probe("echo x > "+dir+"/f") == nil {
		t.Error("the sandboxed child wrote a file")
	}
}

// A failed child is reported to the operator, with the end of its stderr,
// and the agent gets only the plain error.
func TestFailureReported(t *testing.T) {
	var log strings.Builder
	r := Runner{Argv: []string{"/bin/sh", "-c", "cat >/dev/null; echo reason for the log >&2; exit 7"}, Memory: DefaultMemory, Stderr: &log}
	_, err := r.Extract(context.Background(), nil, "text/html", nil, 10)
	if !errors.Is(err, ErrUnreadable) || !strings.Contains(log.String(), "reason for the log") || !strings.Contains(log.String(), "exit status 7") {
		t.Fatalf("%v %q", err, log.String())
	}
	long := &url.URL{Scheme: "https", Host: "example.com", Path: "/" + strings.Repeat("a", maxBaseBytes)}
	if _, err := runner(DefaultMemory).Extract(context.Background(), nil, "text/html", long, 10); err == nil || !strings.Contains(err.Error(), "URL is longer") {
		t.Fatalf("%v", err)
	}
}

// The parent holds a compact Page, however costly the page. The peak of
// its heap is sampled during the read: here 5 MB of invalid UTF-8, which
// comes back as three bytes of U+FFFD for each, the most any page can
// write, of entities, of plain words, and of quote marks, which JSON
// doubles.
func TestParentMemoryBounded(t *testing.T) {
	for name, body := range map[string][]byte{
		"invalid":  []byte("<p>" + strings.Repeat("\xff", 5_000_000) + "</p>"),
		"entities": []byte("<p>" + strings.Repeat("&lt;", 1_250_000) + "</p>"),
		"text":     []byte("<p>" + strings.Repeat("word ", 1_000_000) + "</p>"),
		"quotes":   []byte("<p>" + strings.Repeat(`"`, 5_000_000) + "</p>"),
	} {
		runtime.GC()
		var before runtime.MemStats
		runtime.ReadMemStats(&before)
		var peak atomic.Uint64
		stop := make(chan struct{})
		sampled := make(chan struct{})
		go func() {
			defer close(sampled)
			var m runtime.MemStats
			for {
				runtime.ReadMemStats(&m)
				if m.HeapInuse > peak.Load() {
					peak.Store(m.HeapInuse)
				}
				select {
				case <-stop:
					return
				case <-time.After(time.Millisecond):
				}
			}
		}()
		r := runner(DefaultMemory)
		r.TextChars, r.BodyBytes = 20_000, len(body)
		p, err := r.Extract(context.Background(), body, "text/html", nil, len(body))
		close(stop)
		<-sampled
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len([]rune(p.Text)) > 20_000 || p.TextRunes == 0 {
			t.Errorf("%s: text %d runes, full %d", name, len([]rune(p.Text)), p.TextRunes)
		}
		// What is read back is at most about 3x the body. Reading it and
		// decoding the Page peaks near five times that: the decoder
		// grows its buffer as the output arrives, holding the old and
		// the new at once, beside the decoded strings. Invalid UTF-8
		// peaks at about 4.8 times, with or without the race detector.
		readBack := 3*len(body) + 16*r.TextChars
		if grew := int64(peak.Load()) - int64(before.HeapInuse); grew > int64(5*readBack)+16<<20 {
			t.Errorf("%s: the parent's heap peaked %d MB above where it began, for a %d MB body", name, grew>>20, len(body)>>20)
		}
		runtime.KeepAlive(p)
	}
}

// The read-back cap leaves a third to spare over the largest pages the
// child can write: invalid UTF-8, characters JSON doubles or escapes six
// bytes long, in the text, the visible text and the title.
func TestReadBackMargin(t *testing.T) {
	ctl := strings.Repeat("\x01", 1000)
	for name, body := range map[string]string{
		"invalid":  "<title>" + ctl + "</title><p>" + strings.Repeat("\xff", 100_000),
		"quotes":   "<title>" + ctl + "</title><p>" + strings.Repeat(`"`, 100_000),
		"newlines": "<title>" + ctl + "</title><pre>" + strings.Repeat("\n", 100_000),
		"controls": "<title>" + ctl + "</title><p>" + strings.Repeat("\x01", 100_000),
		"plain":    strings.Repeat("\xff\x01", 50_000),
		// Separators Compact keeps, which JSON may escape, after the
		// costliest byte there is.
		"vt":      "<p>a" + strings.Repeat("\v", 100_000) + "b</p>",
		"ffvt":    "<p>" + strings.Repeat("\xff\v", 50_000),
		"ls":      "<p>" + strings.Repeat("\u2028", 50_000),
		"ffls":    "<p>" + strings.Repeat("\xff\u2028", 50_000),
		"ffps":    "<p>" + strings.Repeat("\xff\u2029", 50_000),
		"ffnel":   "<p>" + strings.Repeat("\xff\u0085", 50_000),
		"ffbr":    "<p>" + strings.Repeat("\xff<br>", 50_000),
		"ffnl":    "<pre>" + strings.Repeat("\xff\n", 50_000),
		"plainvt": strings.Repeat("\xff\v", 50_000),
	} {
		for _, textChars := range []int{1, 20_000, len(body)} {
			mt := "text/html"
			if name == "plain" || name == "plainvt" {
				mt = "text/plain"
			}
			header := fmt.Sprintf(`{"media_type":%q,"base":"","limit":%d,"text_chars":%d,"memory":%d,"body_bytes":%d}`+"\n",
				mt, len(body), textChars, DefaultMemory, len(body))
			var out strings.Builder
			if code := Child(strings.NewReader(header+body), &out); code != 0 {
				t.Fatalf("%s: exit %d", name, code)
			}
			if limit := readBackCap(len(body), textChars); int64(out.Len())*4/3 > limit {
				t.Errorf("%s, %d chars: wrote %d bytes, cap %d", name, textChars, out.Len(), limit)
			}
		}
	}
}

// The reviewer's page of vertical tabs reads through the child. Kept as
// themselves they came back six bytes each, past the read-back cap, and
// the page was refused as unreadable.
func TestVerticalTabsReadBack(t *testing.T) {
	for _, body := range []string{
		"<p>a" + strings.Repeat("\v", 100_000) + "b</p>",
		"<p>" + strings.Repeat("\xff\v", 50_000),
		strings.Repeat("\xff\v", 50_000),
	} {
		mt := "text/html"
		if !strings.HasPrefix(body, "<") {
			mt = "text/plain"
		}
		r := runner(DefaultMemory)
		r.TextChars, r.BodyBytes = 20_000, len(body)
		if _, err := r.Extract(context.Background(), []byte(body), mt, nil, len(body)); err != nil {
			t.Errorf("%.12q: %v", body, err)
		}
	}
}

// The reviewer's body: raw text in SVG's <style> is markup, which a
// tokenizer would count as one run. The cap counts "<", so it cuts this
// quickly, and the page is uncheckable.
func TestTokenCapCannotBeDodged(t *testing.T) {
	for _, body := range []string{
		"<svg><style>" + strings.Repeat("a</x>", 999_990),
		"<p>" + strings.Repeat("a</x>", 999_990),
		"<script><!--</script><svg><style>" + strings.Repeat("a</x>", 999_990),
	} {
		r := runner(DefaultMemory)
		r.TextChars = 20_000
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		start := time.Now()
		p, err := r.Extract(ctx, []byte(body), "text/html", nil, 5_000_000)
		cancel()
		if err != nil || !p.TooManyTokens || p.Uncheckable == "" {
			t.Errorf("%.30q: %v, too many %v, uncheckable %q", body, err, p.TooManyTokens, p.Uncheckable)
		}
		if d := time.Since(start); d > 10*time.Second {
			t.Errorf("%.30q: took %s", body, d)
		}
	}
}

// The child reads exactly the body its header announces: shorter or
// longer input is a failure, and nothing is written.
func TestChildBodyLength(t *testing.T) {
	header := func(n int) string {
		return fmt.Sprintf(`{"media_type":"text/plain","base":"","limit":100,"text_chars":100,"memory":%d,"body_bytes":%d}`+"\n", DefaultMemory, n)
	}
	for name, in := range map[string]string{
		"short": header(10) + "12345",
		"long":  header(3) + "12345",
	} {
		var out strings.Builder
		if code := Child(strings.NewReader(in), &out); code == 0 || out.Len() != 0 {
			t.Errorf("%s: exit %d, wrote %q", name, code, out.String())
		}
	}
	var out strings.Builder
	if code := Child(strings.NewReader(header(5)+"12345"), &out); code != 0 || !strings.Contains(out.String(), `"Text":"12345"`) {
		t.Fatalf("exact: exit %d, %q", code, out.String())
	}
}

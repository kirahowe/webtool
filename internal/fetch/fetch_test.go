package fetch

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/kirahowe/webtool/internal/guard"
	"github.com/kirahowe/webtool/internal/text"
)

// server starts an httptest server and a fetcher that may reach it: the
// test seam allows 127.0.0.1 and the server's port, and names resolve
// through names. Nothing outside this package can do this.
func server(t *testing.T, h http.Handler, names map[string]string) (*httptest.Server, *Fetcher) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	f, err := New(Config{Timeout: 5 * time.Second, MaxBodyBytes: 1 << 20, SnapshotDir: t.TempDir(), MaxSnapshotBytes: 1 << 30, Extract: InProcess(1 << 20)})
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	f.policy.Allow = []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}
	f.policy.Ports = []string{"80", "443", port}
	f.lookup = fakeLookup(names)
	return srv, f
}

func fakeLookup(names map[string]string) func(context.Context, string) ([]netip.Addr, error) {
	return func(_ context.Context, host string) ([]netip.Addr, error) {
		v, ok := names[host]
		if !ok {
			return nil, errors.New("no such host")
		}
		var out []netip.Addr
		for _, s := range strings.Split(v, ",") {
			out = append(out, netip.MustParseAddr(s))
		}
		return out, nil
	}
}

func port(srv *httptest.Server) string {
	_, p, _ := net.SplitHostPort(srv.Listener.Addr().String())
	return p
}

func refusal(t *testing.T, err error, want string) {
	t.Helper()
	var r *guard.Refusal
	if !errors.As(err, &r) {
		t.Fatalf("want a refusal containing %q, got %v", want, err)
	}
	if !strings.Contains(r.Error(), want) {
		t.Fatalf("want a refusal containing %q, got %q", want, r.Error())
	}
}

func TestFetchHTML(t *testing.T) {
	var got http.Header
	srv, f := server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<title>T</title><p>Hello <a href="/next">next</a><span hidden>psst</span></p>`)
	}), nil)
	res, err := f.Fetch(context.Background(), srv.URL+"/page")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != 200 || res.FinalURL != srv.URL+"/page" || res.ContentType != "text/html" {
		t.Fatalf("%+v", res)
	}
	if want := "Hello next <" + srv.URL + "/next>"; res.Page.Text != want || res.Page.Hidden != 4 {
		t.Fatalf("text %q hidden %d", res.Page.Text, res.Page.Hidden)
	}
	if got.Get("User-Agent") != UserAgent || got.Get("Cookie") != "" || got.Get("Authorization") != "" {
		t.Fatalf("headers %v", got)
	}
	base := filepath.Join(f.cfg.SnapshotDir, res.SHA256)
	body, err := os.ReadFile(base + ".body")
	if err != nil || !strings.Contains(string(body), "psst") {
		t.Fatalf("raw body not kept: %v", err)
	}
	if txt, _ := os.ReadFile(base + ".txt"); !strings.HasPrefix(string(txt), "title: T\n\nHello") {
		t.Fatalf("text %q", txt)
	}
	var meta Meta
	raw, _ := os.ReadFile(base + ".json")
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatal(err)
	}
	if f.controls.Load() == 0 {
		t.Fatal("the dialer's Control hook never ran")
	}
	if meta.URL != srv.URL+"/page" || meta.Status != 200 || meta.SHA256 != res.SHA256 || meta.HiddenChars != 4 || meta.FetchedAt.IsZero() {
		t.Fatalf("meta %+v", meta)
	}
}

// A redirect is reported, not followed: the result says where the page
// moved, as an absolute URL, and the target is never requested.
func TestRedirectNotFollowed(t *testing.T) {
	var hits []string
	var srv *httptest.Server
	srv, f := server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, r.URL.Path)
		switch r.URL.Path {
		case "/rel":
			http.Redirect(w, r, "/next?a=1#frag", http.StatusMovedPermanently)
		case "/abs":
			http.Redirect(w, r, "http://b.example.com:"+port(srv)+"/3", http.StatusFound)
		case "/seeother":
			http.Redirect(w, r, "../up", http.StatusSeeOther)
		default:
			t.Errorf("redirect target requested: %s", r.URL.Path)
		}
	}), map[string]string{"b.example.com": "127.0.0.1"})
	for path, want := range map[string]string{
		"/rel":      srv.URL + "/next?a=1#frag",
		"/abs":      "http://b.example.com:" + port(srv) + "/3",
		"/seeother": srv.URL + "/up",
	} {
		res, err := f.Fetch(context.Background(), srv.URL+path)
		if err != nil || res.Moved != want || res.FinalURL != "" || res.SHA256 != "" || res.Status < 301 {
			t.Fatalf("%s: %v %+v", path, err, res)
		}
	}
	if len(hits) != 3 {
		t.Fatalf("hits %v", hits)
	}
	if entries, _ := os.ReadDir(f.cfg.SnapshotDir); len(entries) != 0 {
		t.Fatalf("a snapshot was kept for a moved page: %v", entries)
	}
}

// A 3xx without a Location is an ordinary page.
func TestRedirectWithoutLocation(t *testing.T) {
	srv, f := server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusNotModified)
	}), nil)
	res, err := f.Fetch(context.Background(), srv.URL+"/")
	if err != nil || res.Moved != "" || res.Status != 304 {
		t.Fatalf("%v %+v", err, res)
	}
}

// The target goes through the same guards as any URL, and is still not
// requested.
func TestRedirectTargetChecked(t *testing.T) {
	cases := map[string]string{
		"http://10.0.0.1/":           "10.0.0.1 is a private address",
		"http://localhost/":          "local name",
		"http://127.0.0.1:2586/x":    "port 2586",
		"ftp://example.com/":         "only http and https",
		"http://user:pw@example.com": "credentials",
		"http://0x7f.1/":             "non-canonical",
		"http://[::ffff:10.1.2.3]/":  "10.1.2.3 is a private address",
		"http://a%0Ab.example.com/x": "",
	}
	for target, want := range cases {
		t.Run(target, func(t *testing.T) {
			srv, f := server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", target)
				w.WriteHeader(http.StatusFound)
			}), nil)
			res, err := f.Fetch(context.Background(), srv.URL+"/")
			if want == "" {
				if err == nil {
					t.Fatalf("accepted %q: %+v", target, res)
				}
				return
			}
			refusal(t, err, want)
		})
	}
}

// The address shown holds no invisible character: one in the path comes
// out percent-encoded, and one that would stay raw is refused.
func TestMovedShowsNoInvisibleCharacters(t *testing.T) {
	var loc string
	srv, f := server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", loc)
		w.WriteHeader(http.StatusFound)
	}), nil)
	loc = "/a\u202eb"
	res, err := f.Fetch(context.Background(), srv.URL+"/")
	if err != nil || res.Moved != text.EscapeOnly(res.Moved) || !strings.Contains(res.Moved, "%E2%80%AE") {
		t.Fatalf("%v %q", err, res.Moved)
	}
	loc = "/a?q=\u200b"
	res, err = f.Fetch(context.Background(), srv.URL+"/")
	refusal(t, err, "invisible characters")
	if res.Moved != "" {
		t.Fatalf("an unreadable target was reported: %q", res.Moved)
	}
}

// A target holding any Unicode space is refused, not only ASCII ones:
// a no-break space or an ideographic space would read as a gap to the
// agent, which could then copy the address wrongly or in part.
func TestMovedRefusesUnicodeSpaces(t *testing.T) {
	var loc string
	srv, f := server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", loc)
		w.WriteHeader(http.StatusFound)
	}), nil)
	for _, l := range []string{"/a?q=x\u00a0y", "/a?q=x\u3000y", "/a?q=x\u2003y", "/a?q=x\u0085y"} {
		loc = l
		res, err := f.Fetch(context.Background(), srv.URL+"/")
		refusal(t, err, "spaces")
		if res.Moved != "" {
			t.Fatalf("%q: a target with a space was reported: %q", l, res.Moved)
		}
	}
	loc = "/a?q=x%C2%A0y"
	res, err := f.Fetch(context.Background(), srv.URL+"/")
	if err != nil || !strings.HasSuffix(res.Moved, "/a?q=x%C2%A0y") {
		t.Fatalf("an encoded space is part of the address: %v %q", err, res.Moved)
	}
}

// The redirect target is not even resolved, so a name that would rebind
// is never looked up.
func TestRedirectTargetNotResolved(t *testing.T) {
	var srv *httptest.Server
	srv, f := server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://other.example.com:"+port(srv)+"/2", http.StatusFound)
	}), nil)
	var asked []string
	f.lookup = func(_ context.Context, host string) ([]netip.Addr, error) {
		asked = append(asked, host)
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}
	res, err := f.Fetch(context.Background(), "http://rebind.example.com:"+port(srv)+"/1")
	if err != nil || res.Moved != "http://other.example.com:"+port(srv)+"/2" {
		t.Fatalf("%v %+v", err, res)
	}
	if fmt.Sprint(asked) != "[rebind.example.com]" {
		t.Fatalf("resolved %v", asked)
	}
}

// Whatever the target looks like, the shown address is exactly one the
// guard would let through as a first URL, and fetching it again gives
// the same judgement.
func TestMovedIsAFetchableURL(t *testing.T) {
	var target string
	var code int
	srv, f := server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", target)
		w.WriteHeader(code)
	}), map[string]string{"pub.example.com": "127.0.0.1"})
	rapid.Check(t, func(t *rapid.T) {
		target = rapid.SampledFrom([]string{"/p", "/p/q?x=1", "../x", "http://pub.example.com/a", "https://pub.example.com:443/", "//pub.example.com/z", "?q=2", "#f"}).Draw(t, "target")
		code = rapid.SampledFrom([]int{301, 302, 303, 307, 308}).Draw(t, "code")
		res, err := f.Fetch(context.Background(), srv.URL+"/a/b")
		if err != nil {
			return // refused by port rules for 443: fine
		}
		u, perr := url.Parse(res.Moved)
		if perr != nil || !u.IsAbs() || f.policy.CheckURL(u) != nil || res.Moved != u.String() || res.Status != code {
			t.Fatalf("%q -> %q (%v)", target, res.Moved, perr)
		}
	})
}

// A name that resolves to a refused address is refused at the dial, so
// DNS rebinding cannot get past a check made earlier.
func TestDialChecksResolvedAddress(t *testing.T) {
	srv, f := server(t, http.NotFoundHandler(), map[string]string{
		"private.example.com": "10.1.1.1",
		"mixed.example.com":   "93.184.215.14,127.0.0.2",
		"loop.example.com":    "127.0.0.2",
		"mapped.example.com":  "::ffff:169.254.169.254",
	})
	for name, want := range map[string]string{
		"private.example.com": "10.1.1.1 is a private address",
		"mixed.example.com":   "127.0.0.2 is a loopback address",
		"loop.example.com":    "127.0.0.2 is a loopback address",
		"mapped.example.com":  "169.254.169.254 is a link-local address",
	} {
		_, err := f.Fetch(context.Background(), "http://"+name+":"+port(srv)+"/")
		refusal(t, err, want)
	}
}

func TestControlChecksTheSocketAddress(t *testing.T) {
	f, err := New(Config{Timeout: time.Second, MaxBodyBytes: 1, SnapshotDir: t.TempDir(), MaxSnapshotBytes: 1 << 30, Extract: InProcess(1 << 20)})
	if err != nil {
		t.Fatal(err)
	}
	for _, addr := range []string{"127.0.0.1:80", "[::1]:443", "10.0.0.1:80", "[fe80::1%en0]:80", "8.8.8.8:2586", "[::ffff:127.0.0.1]:80"} {
		if f.control("tcp", addr, nil) == nil {
			t.Errorf("%s allowed", addr)
		}
	}
	if err := f.control("tcp", "8.8.8.8:443", nil); err != nil {
		t.Errorf("8.8.8.8:443: %v", err)
	}
}

// The fetcher production builds has no allowance, and cannot reach a
// server on this machine even when the name and port are made to fit.
func TestProductionFetcherNeverAllowsLocal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("request reached the local server: %s", r.URL)
	}))
	defer srv.Close()
	f, err := New(Config{Timeout: 5 * time.Second, MaxBodyBytes: 1 << 20, SnapshotDir: t.TempDir(), MaxSnapshotBytes: 1 << 30, Extract: InProcess(1 << 20)})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.policy.Allow) != 0 || f.policy.Ports != nil || f.transport.Proxy != nil {
		t.Fatalf("production policy %+v", f.policy)
	}
	for _, u := range []string{srv.URL, "http://127.0.0.1/", "http://localhost/", "http://[::1]/", "http://2130706433/"} {
		_, err := f.Fetch(context.Background(), u)
		refusal(t, err, "")
	}
	// Open the port and point a public-looking name at the server: the
	// address is still refused.
	f.policy.Ports = []string{port(srv)}
	f.lookup = fakeLookup(map[string]string{"public.example.com": "127.0.0.1"})
	_, err = f.Fetch(context.Background(), "http://public.example.com:"+port(srv)+"/")
	refusal(t, err, "127.0.0.1 is a loopback address")
	_, err = f.Fetch(context.Background(), srv.URL)
	refusal(t, err, "127.0.0.1 is a loopback address")
}

func TestBodyCap(t *testing.T) {
	srv, f := server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, strings.Repeat("a", 5000))
	}), nil)
	f.cfg.MaxBodyBytes = 1000
	res, err := f.Fetch(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated || res.BodyBytes != 1000 || len(res.Page.Text) != 1000 {
		t.Fatalf("%+v", res)
	}
}

func TestTimeout(t *testing.T) {
	srv, f := server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}), nil)
	f.cfg.Timeout = 200 * time.Millisecond
	start := time.Now()
	_, err := f.Fetch(context.Background(), srv.URL)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("got %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("the timeout did not bound the fetch")
	}
}

func TestContentTypes(t *testing.T) {
	cases := map[string]string{
		"application/pdf":          "PDF",
		"image/png":                "image/png",
		"application/octet-stream": "application/octet-stream",
		"text/html; charset=utf-8": "",
		"application/xhtml+xml":    "",
		"text/plain":               "",
		"":                         "", // sniffed as HTML
	}
	for ct, want := range cases {
		srv, f := server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header()["Content-Type"] = []string{ct}
			if ct == "" {
				w.Header()["Content-Type"] = nil
			}
			fmt.Fprint(w, "<html><body><p>hi</p></body></html>")
		}), nil)
		res, err := f.Fetch(context.Background(), srv.URL)
		if want == "" {
			if err != nil || !strings.Contains(res.Page.Text, "hi") {
				t.Errorf("%q: %v %+v", ct, err, res)
			}
			continue
		}
		refusal(t, err, want)
		if res.Status != 200 {
			t.Errorf("%q: status %d", ct, res.Status)
		}
	}
}

func TestCharset(t *testing.T) {
	srv, f := server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=iso-8859-1")
		w.Write([]byte("<p>caf\xe9</p>"))
	}), nil)
	res, err := f.Fetch(context.Background(), srv.URL)
	if err != nil || res.Page.Text != "café" {
		t.Fatalf("%v %q", err, res.Page.Text)
	}
}

func TestBadURLs(t *testing.T) {
	f, err := New(Config{Timeout: time.Second, MaxBodyBytes: 1, SnapshotDir: t.TempDir(), MaxSnapshotBytes: 1 << 30, Extract: InProcess(1 << 20)})
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range []string{"", "example.com", "/path", "http://%zz/", "http://[::1/"} {
		_, err := f.Fetch(context.Background(), u)
		refusal(t, err, "")
	}
}

func TestUnresolvable(t *testing.T) {
	srv, f := server(t, http.NotFoundHandler(), nil)
	_, err := f.Fetch(context.Background(), "http://nowhere.example.com:"+port(srv)+"/")
	if err == nil || err.Error() != "could not resolve nowhere.example.com" {
		t.Fatalf("got %v", err)
	}
}

// Every address on this machine's interfaces is refused by the fetcher
// production builds, public or not.
func TestOwnAddressesRefused(t *testing.T) {
	f, err := New(Config{Timeout: time.Second, MaxBodyBytes: 1, SnapshotDir: t.TempDir(), MaxSnapshotBytes: 1 << 30, Extract: InProcess(1 << 20)})
	if err != nil {
		t.Fatal(err)
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip, _ := netip.AddrFromSlice(ipnet.IP)
		if err := f.policy.CheckAddr(ip); err == nil {
			t.Errorf("own address %s allowed", ip)
		}
		if f.control("tcp", netip.AddrPortFrom(ip.Unmap(), 443).String(), nil) == nil {
			t.Errorf("own address %s allowed at the dial", ip)
		}
	}
	if len(f.policy.Local) == 0 {
		t.Fatal("no local networks found")
	}
}

// HTTPS and HTTP/2 dial through the same checks.
func TestTLSAndHTTP2(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/away" {
			http.Redirect(w, r, "https://10.0.0.1/", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, r.Proto)
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()
	f, err := New(Config{Timeout: 5 * time.Second, MaxBodyBytes: 1 << 20, SnapshotDir: t.TempDir(), MaxSnapshotBytes: 1 << 30, Extract: InProcess(1 << 20)})
	if err != nil {
		t.Fatal(err)
	}
	f.policy.Allow = []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}
	f.policy.Ports = []string{port(srv)}
	f.transport.TLSClientConfig.RootCAs = srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	res, err := f.Fetch(context.Background(), srv.URL+"/")
	if err != nil || res.Page.Text != "HTTP/2.0" {
		t.Fatalf("%v %+v", err, res)
	}
	if f.controls.Load() == 0 {
		t.Fatal("the TLS dial skipped the Control hook")
	}
	_, err = f.Fetch(context.Background(), srv.URL+"/away")
	refusal(t, err, "port 443")
	f.policy.Ports = []string{port(srv), "443"}
	_, err = f.Fetch(context.Background(), srv.URL+"/away")
	refusal(t, err, "10.0.0.1 is a private address")
}

// Each connection resolves and checks afresh, so a name that answers
// differently the second time is caught on the second dial.
func TestRebindingBetweenRequests(t *testing.T) {
	srv, f := server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "ok")
	}), nil)
	calls := 0
	f.lookup = func(context.Context, string) ([]netip.Addr, error) {
		calls++
		if calls == 1 {
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		}
		return []netip.Addr{netip.MustParseAddr("10.0.0.1")}, nil
	}
	if _, err := f.Fetch(context.Background(), "http://rebind.example.com:"+port(srv)+"/1"); err != nil {
		t.Fatal(err)
	}
	_, err2 := f.Fetch(context.Background(), "http://rebind.example.com:"+port(srv)+"/1")
	refusal(t, err2, "10.0.0.1 is a private address")
}

func TestExtractionUnderDeadline(t *testing.T) {
	srv, f := server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "<p>x</p>")
	}), nil)
	f.cfg.Timeout = 300 * time.Millisecond
	// A reader that takes three seconds, and returns at the deadline as
	// an Extractor must.
	f.extract = func(ctx context.Context, _ []byte, _ string, _ *url.URL, _ int) (text.Page, error) {
		done := make(chan text.Page, 1)
		go func() { time.Sleep(3 * time.Second); done <- text.Page{} }()
		select {
		case p := <-done:
			return p, nil
		case <-ctx.Done():
			return text.Page{}, errors.New("timed out reading the page's text")
		}
	}
	start := time.Now()
	_, err := f.Fetch(context.Background(), srv.URL)
	if err == nil || !strings.Contains(err.Error(), "timed out") || time.Since(start) > 2*time.Second {
		t.Fatalf("%v after %s", err, time.Since(start))
	}
}

func TestGzipBombCapped(t *testing.T) {
	var bomb bytes.Buffer
	zw := gzip.NewWriter(&bomb)
	zeros := make([]byte, 1<<20)
	for range 50 {
		zw.Write(zeros)
	}
	zw.Close()
	srv, f := server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Content-Encoding", "gzip")
		w.Write(bomb.Bytes())
	}), nil)
	f.cfg.MaxBodyBytes = 1 << 20
	res, err := f.Fetch(context.Background(), srv.URL)
	if err != nil || !res.Truncated || res.BodyBytes != 1<<20 {
		t.Fatalf("%v %+v", err, res)
	}
}

func TestHugeHeadersRefused(t *testing.T) {
	srv, f := server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Big", strings.Repeat("a", 200<<10))
		fmt.Fprint(w, "x")
	}), nil)
	if _, err := f.Fetch(context.Background(), srv.URL); err == nil {
		t.Fatal("a 200 KB header was accepted")
	}
}

func TestSnapshotsPruned(t *testing.T) {
	n := 0
	srv, f := server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintf(w, "%d %s", n, strings.Repeat("x", 10_000))
	}), nil)
	f.cfg.MaxSnapshotBytes = 50_000
	var shas []string
	for range 6 {
		res, err := f.Fetch(context.Background(), srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		shas = append(shas, res.SHA256)
		time.Sleep(10 * time.Millisecond) // distinct modification times
	}
	var total int64
	entries, _ := os.ReadDir(f.cfg.SnapshotDir)
	for _, e := range entries {
		info, _ := e.Info()
		total += info.Size()
	}
	if total > f.cfg.MaxSnapshotBytes {
		t.Fatalf("snapshots hold %d bytes", total)
	}
	if _, err := os.Stat(filepath.Join(f.cfg.SnapshotDir, shas[5]+".body")); err != nil {
		t.Fatal("the newest snapshot was removed")
	}
	if _, err := os.Stat(filepath.Join(f.cfg.SnapshotDir, shas[0]+".body")); err == nil {
		t.Fatal("the oldest snapshot was kept")
	}
}

func TestSnapshotMetaEscaped(t *testing.T) {
	srv, f := server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "x")
	}), nil)
	res, err := f.Fetch(context.Background(), srv.URL+"/?q=\u202e")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(f.cfg.SnapshotDir, res.SHA256+".json"))
	if strings.ContainsRune(string(raw), '\u202e') {
		t.Fatalf("raw bidi control in %s", raw)
	}
}

func TestPruneTouchesOnlySnapshots(t *testing.T) {
	srv, f := server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, r.URL.Path, strings.Repeat("x", 10_000))
	}), nil)
	dir := f.cfg.SnapshotDir
	os.MkdirAll(dir, 0o700)
	for _, name := range []string{"notes.txt", "old.json", strings.Repeat("g", 64) + ".body"} {
		os.WriteFile(filepath.Join(dir, name), []byte(strings.Repeat("n", 100_000)), 0o600)
	}
	f.cfg.MaxSnapshotBytes = 25_000
	for i := range 4 {
		if _, err := f.Fetch(context.Background(), fmt.Sprintf("%s/%d", srv.URL, i)); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"notes.txt", "old.json", strings.Repeat("g", 64) + ".body"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s was removed", name)
		}
	}
}

// Fetches sharing a snapshot directory leave every snapshot whole.
func TestConcurrentSnapshotsStayWhole(t *testing.T) {
	srv, base := server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, r.URL.Path, strings.Repeat("x", 5_000))
	}), nil)
	dir := base.cfg.SnapshotDir
	done := make(chan error)
	for g := range 8 {
		go func() {
			f, err := New(Config{Timeout: 5 * time.Second, MaxBodyBytes: 1 << 20, SnapshotDir: dir, MaxSnapshotBytes: 40_000, Extract: InProcess(1 << 20)})
			if err != nil {
				done <- err
				return
			}
			f.policy = base.policy
			for i := range 10 {
				if _, err := f.Fetch(context.Background(), fmt.Sprintf("%s/%d-%d", srv.URL, g, i)); err != nil {
					done <- err
					return
				}
			}
			done <- nil
		}()
	}
	for range 8 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	counts := map[string]int{}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if id, ok := snapshotName(e.Name()); ok {
			counts[id]++
		}
	}
	for id, n := range counts {
		if n != 3 {
			t.Errorf("snapshot %s has %d files", id, n)
		}
	}
}

// A held lock cannot keep a fetch past its time limit.
func TestSnapshotLockBoundedByDeadline(t *testing.T) {
	srv, f := server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "x")
	}), nil)
	os.MkdirAll(f.cfg.SnapshotDir, 0o700)
	unlock, err := lockDir(context.Background(), f.cfg.SnapshotDir)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	f.cfg.Timeout = 300 * time.Millisecond
	start := time.Now()
	_, err = f.Fetch(context.Background(), srv.URL)
	if err == nil || !strings.Contains(err.Error(), "snapshot") || time.Since(start) > 2*time.Second {
		t.Fatalf("%v after %s", err, time.Since(start))
	}
}

// Fetches that share a fetcher read one page's text at a time.
func TestOneExtractionAtATime(t *testing.T) {
	srv, f := server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "<p>x</p>")
	}), nil)
	var running, most atomic.Int64
	f.extract = func(context.Context, []byte, string, *url.URL, int) (text.Page, error) {
		n := running.Add(1)
		for m := most.Load(); n > m && !most.CompareAndSwap(m, n); m = most.Load() {
		}
		time.Sleep(50 * time.Millisecond)
		running.Add(-1)
		return text.Page{}, nil
	}
	done := make(chan error)
	for range 4 {
		go func() {
			_, err := f.Fetch(context.Background(), srv.URL)
			done <- err
		}()
	}
	for range 4 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if most.Load() != 1 {
		t.Fatalf("%d extractions ran at once", most.Load())
	}
}

// A body cut at the limit is read only to its last tag, so a cut entity
// cannot make a word the page does not have.
func TestCutBodyEndsAtATag(t *testing.T) {
	body := "<p>ok</p><p>FORGE&#680;</p>"
	srv, f := server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, body)
	}), nil)
	f.cfg.MaxBodyBytes = int64(strings.Index(body, "&#68") + len("&#68"))
	res, err := f.Fetch(context.Background(), srv.URL)
	if err != nil || !res.Truncated || strings.Contains(res.Page.Text, "FORGED") || !strings.Contains(res.Page.Text, "ok") {
		t.Fatalf("%v %+v", err, res.Page)
	}
}

func TestLocalIPv6WidenedTo56(t *testing.T) {
	cases := map[string]string{
		"2600:1700:abcd:12::5/64": "2600:1700:abcd::/56",
		"2600:1700:abcd:12::5/48": "2600:1700:abcd::/48",
		"fd7a:115c:a1e0::5/64":    "fd7a:115c:a1e0::/64",
		"192.168.1.5/24":          "192.168.1.0/24",
	}
	for in, want := range cases {
		p := netip.MustParsePrefix(in)
		if got := localPrefix(p.Addr(), p.Bits()); got.String() != want {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
	}
}

func TestXHTMLAndRefreshUncheckable(t *testing.T) {
	srv, f := server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/x":
			w.Header().Set("Content-Type", "application/xhtml+xml")
		case "/r":
			w.Header().Set("Content-Type", "text/html")
			w.Header().Set("Refresh", "1; url=https://example.org/")
		case "/slow":
			w.Header().Set("Content-Type", "text/html")
			w.Header().Set("Refresh", "600")
		}
		fmt.Fprint(w, `<html xmlns="http://www.w3.org/1999/xhtml"><body><p>words</p></body></html>`)
	}), nil)
	for path, want := range map[string]bool{"/x": true, "/r": true, "/slow": false} {
		res, err := f.Fetch(context.Background(), srv.URL+path)
		if err != nil {
			t.Fatal(err)
		}
		if got := res.Page.Uncheckable != ""; got != want {
			t.Errorf("%s: uncheckable %q", path, res.Page.Uncheckable)
		}
		// The agent still reads the page; only check stops vouching.
		if !strings.Contains(res.Page.Text, "words") || want && res.Page.Visible != "" {
			t.Errorf("%s: text %q visible %q", path, res.Page.Text, res.Page.Visible)
		}
	}
}

func TestStaleTempFilesPruned(t *testing.T) {
	srv, f := server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, r.URL.Path, strings.Repeat("x", 1000))
	}), nil)
	dir := f.cfg.SnapshotDir
	os.MkdirAll(dir, 0o700)
	stale, fresh := filepath.Join(dir, ".tmp-stale"), filepath.Join(dir, ".tmp-fresh")
	os.WriteFile(stale, []byte("x"), 0o600)
	os.WriteFile(fresh, []byte("x"), 0o600)
	old := time.Now().Add(-time.Hour)
	os.Chtimes(stale, old, old)
	f.cfg.MaxSnapshotBytes = 1
	if _, err := f.Fetch(context.Background(), srv.URL+"/a"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); err == nil {
		t.Error("a stale temp file was kept")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("a fresh temp file was removed")
	}
}

func TestDuplicateContentTypeAndCutUncheckable(t *testing.T) {
	srv, f := server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/dup" {
			w.Header()["Content-Type"] = []string{"text/html", "application/xhtml+xml"}
		} else {
			w.Header().Set("Content-Type", "text/html")
		}
		fmt.Fprint(w, `<p>words</p>`+strings.Repeat("<p>more</p>", 100))
	}), nil)
	res, err := f.Fetch(context.Background(), srv.URL+"/dup")
	if err != nil || res.Page.Uncheckable == "" || !strings.Contains(res.Page.Text, "words") {
		t.Fatalf("duplicate content type: %v %q", err, res.Page.Uncheckable)
	}
	f.cfg.MaxBodyBytes = 100
	res, err = f.Fetch(context.Background(), srv.URL+"/cut")
	if err != nil || !res.Truncated || res.Page.Uncheckable == "" || res.Page.Visible != "" {
		t.Fatalf("cut body: %v %+v", err, res.Page)
	}
}

func TestBodyLimitBounded(t *testing.T) {
	if _, err := New(Config{Timeout: time.Second, MaxBodyBytes: MaxBodyLimit + 1, SnapshotDir: t.TempDir(), MaxSnapshotBytes: 1}); err == nil {
		t.Fatal("a body limit past MaxBodyLimit was accepted")
	}
}

// A fetcher never reads in-process unless told to.
func TestExtractorRequired(t *testing.T) {
	if _, err := New(Config{Timeout: time.Second, MaxBodyBytes: 1, SnapshotDir: t.TempDir(), MaxSnapshotBytes: 1}); err == nil {
		t.Fatal("a fetcher without a page reader was built")
	}
}

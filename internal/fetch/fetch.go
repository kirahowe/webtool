// Package fetch gets one web page for an agent, on the assumption that
// the URL was chosen by an attacker. The URL passes guard's rules before
// it is requested. A redirect is never followed: the fetch reports where
// the page moved, after the same rules have looked at the target, and the
// caller decides whether to ask for it. Every
// connection is checked again at the address actually dialed, so a name
// that resolves somewhere it should not is refused however it resolves.
package fetch

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/net/html/charset"

	"github.com/kirahowe/webtool/internal/guard"
	"github.com/kirahowe/webtool/internal/text"
)

// UserAgent is sent with every request, and nothing else identifies the
// operator.
const UserAgent = "webtool/1 (+https://github.com/kirahowe/webtool)"

// MaxBodyLimit bounds Config.MaxBodyBytes, so the limit plus one and
// its use as a count of runes cannot overflow.
const MaxBodyLimit = 256 << 20

// Config bounds a fetch.
type Config struct {
	Timeout      time.Duration // the whole fetch, redirects and body included
	MaxBodyBytes int64         // reading stops here
	SnapshotDir  string        // where raw bodies and their text are kept
	// MaxSnapshotBytes bounds the snapshot directory. Past it the oldest
	// snapshots are removed.
	MaxSnapshotBytes int64
	// Extract reads a body's text. It is required: webtool sets it to
	// read in a child process (internal/extract), which can be killed, and
	// tests set InProcess.
	Extract Extractor
	// ExtractMemory is the memory a child reading a page may use.
	ExtractMemory int64
	// FixtureOrigin is the one loopback address and port the fixture
	// build may fetch from, for a local server of test pages. New
	// refuses it in any other build.
	FixtureOrigin netip.AddrPort
}

// Extractor reads a body, decoded to UTF-8, into a Page. It returns
// before ctx's deadline.
type Extractor func(ctx context.Context, body []byte, mediaType string, base *url.URL, limit int) (text.Page, error)

// Result is what a fetch found. A failed fetch fills in as much as it
// got: the status once a response came, and the redirect target if there
// was one.
type Result struct {
	URL string
	// FinalURL is the URL that answered. It is empty when the page moved.
	FinalURL string
	// Moved is the absolute URL a 3xx response with a Location pointed
	// to, resolved against URL. It is set only when the target passed
	// the guard (or when the guard refused it, alongside the error), and
	// it is never fetched. No body is read and no snapshot kept.
	Moved       string
	Status      int
	ContentType string
	SHA256      string    // of the raw body as read
	BodyBytes   int       // as read
	Truncated   bool      // the body was longer than MaxBodyBytes
	Page        text.Page // the text a person would see
	FetchedAt   time.Time
}

// Fetcher fetches pages. Build one with New.
type Fetcher struct {
	cfg       Config
	policy    guard.Policy
	lookup    func(ctx context.Context, host string) ([]netip.Addr, error)
	now       func() time.Time
	transport *http.Transport
	// extract turns a body into text; tests slow it down.
	extract Extractor
	// controls counts sockets the dialer's Control hook checked, so a
	// test can tell the hook is wired in.
	controls atomic.Int64
	// extracting holds a token while a page's text is being read. Reading
	// is left behind when a fetch runs out of time, and this keeps the
	// next fetch from starting another beside it.
	extracting chan struct{}
}

// New builds the fetcher production uses: guard's rules with this
// machine's own networks refused, the system resolver, no proxy, and no
// way to allow anything more. The fixture build alone may allow one
// loopback origin.
func New(cfg Config) (*Fetcher, error) {
	if cfg.Timeout <= 0 || cfg.MaxBodyBytes <= 0 || cfg.SnapshotDir == "" || cfg.MaxSnapshotBytes <= 0 {
		return nil, errors.New("fetch needs a timeout, a body limit, a snapshot directory and a limit on it")
	}
	if cfg.MaxBodyBytes > MaxBodyLimit {
		return nil, fmt.Errorf("the body limit may be at most %d bytes", MaxBodyLimit)
	}
	local, err := LocalNetworks()
	if err != nil {
		return nil, fmt.Errorf("could not list this machine's addresses, so cannot tell what is local: %w", err)
	}
	if cfg.Extract == nil {
		return nil, errors.New("fetch needs a way to read pages (Config.Extract)")
	}
	origin, err := fixtureOrigin(cfg.FixtureOrigin)
	if err != nil {
		return nil, err
	}
	f := &Fetcher{
		cfg:    cfg,
		policy: guard.Policy{Local: local, Origin: origin},
		lookup: func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		},
		now:        time.Now,
		extract:    cfg.Extract,
		extracting: make(chan struct{}, 1),
	}
	f.transport = &http.Transport{
		Proxy:                  nil, // never a proxy, whatever the environment says
		DialContext:            f.dial,
		ForceAttemptHTTP2:      true,
		DisableKeepAlives:      true,
		TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:    10 * time.Second,
		ResponseHeaderTimeout:  cfg.Timeout,
		MaxResponseHeaderBytes: 64 << 10,
	}
	return f, nil
}

// LocalNetworks lists every address of this machine's interfaces with its
// prefix.
func LocalNetworks() ([]netip.Prefix, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	var out []netip.Prefix
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip, ok := netip.AddrFromSlice(ipnet.IP)
		if !ok {
			continue
		}
		ones, bits := ipnet.Mask.Size()
		if ip.Is4In6() {
			ip = ip.Unmap()
			if bits == 128 {
				ones -= 96
			}
		}
		if ones < 0 || ones > ip.BitLen() {
			ones = ip.BitLen()
		}
		out = append(out, localPrefix(ip.WithZone(""), ones))
	}
	return out, nil
}

// localPrefix is the network refused around one of this machine's
// addresses. A global IPv6 address sits in a /64, but a home router is
// usually delegated a /56 and hands its other /64s to other networks in
// the house, so the whole /56 is refused. Prefixes delegated beyond that,
// and the router's own public IPv4 address, are not known here.
func localPrefix(ip netip.Addr, ones int) netip.Prefix {
	if ip.Is6() && ip.IsGlobalUnicast() && !ip.IsPrivate() && ones > 56 {
		ones = 56
	}
	return netip.PrefixFrom(ip, ones).Masked()
}

// dial connects to a host only when every address it resolves to is
// allowed. The dialer's Control checks the address once more as the
// socket connects.
func (f *Fetcher) dial(ctx context.Context, _, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, &guard.Refusal{Reason: "bad address " + address}
	}
	pn, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return nil, &guard.Refusal{Reason: "bad port " + port}
	}
	var addrs []netip.Addr
	if a, err := netip.ParseAddr(host); err == nil {
		if err := f.policy.CheckAddrPort(a, port); err != nil {
			return nil, err
		}
		addrs = []netip.Addr{a}
	} else {
		if err := f.policy.CheckPort(port); err != nil {
			return nil, err
		}
		if _, err := guard.CheckHost(host); err != nil {
			return nil, err
		}
		if addrs, err = f.lookup(ctx, host); err != nil || len(addrs) == 0 {
			return nil, resolveError(host)
		}
		// One refused address refuses the host: a name that answers
		// with a public address and a private one is trying something.
		for _, a := range addrs {
			if err := f.policy.CheckAddr(a); err != nil {
				return nil, prefixed(host+" resolves to a refused address: ", err)
			}
		}
	}
	d := net.Dialer{Timeout: 10 * time.Second, Control: f.control}
	var last error
	for _, a := range addrs {
		conn, err := d.DialContext(ctx, "tcp", netip.AddrPortFrom(a.Unmap().WithZone(""), uint16(pn)).String())
		if err == nil {
			return conn, nil
		}
		last = err
	}
	return nil, last
}

// control is the last check, on the address the socket is connecting to.
func (f *Fetcher) control(_, address string, _ syscall.RawConn) error {
	f.controls.Add(1)
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return &guard.Refusal{Reason: "bad address " + address}
	}
	return f.policy.CheckAddrPort(ap.Addr(), strconv.Itoa(int(ap.Port())))
}

// allowedTypes are the content types fetch reads.
var allowedTypes = map[string]bool{"text/html": true, "application/xhtml+xml": true, "text/plain": true}

// Fetch gets one URL and keeps a snapshot of what it read. It follows no
// redirect: a 3xx response with a Location returns a Result with Moved
// set. A refusal is a *guard.Refusal.
func (f *Fetcher) Fetch(ctx context.Context, raw string) (*Result, error) {
	res := &Result{URL: raw, FetchedAt: f.now().UTC()}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || !u.IsAbs() {
		return res, &guard.Refusal{Reason: "not an absolute URL"}
	}
	if err := f.policy.CheckURL(u); err != nil {
		return res, err
	}
	ctx, cancel := context.WithTimeout(ctx, f.cfg.Timeout)
	defer cancel()

	client := &http.Client{
		Transport: f.transport,
		// A redirect is reported, never followed.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return res, &guard.Refusal{Reason: "not a URL that can be requested"}
	}
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,text/plain;q=0.9")
	resp, err := client.Do(req)
	if resp != nil {
		res.Status = resp.StatusCode
	}
	if err != nil {
		return res, f.explain(ctx, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode < 400 && resp.Header.Get("Location") != "" {
		return res, f.moved(res, resp)
	}
	res.FinalURL = resp.Request.URL.String()

	mediaType := ""
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		mt, _, err := mime.ParseMediaType(ct)
		if err != nil {
			return res, &guard.Refusal{Reason: "the content type is not valid"}
		}
		mediaType = mt
		if err := typeAllowed(mt); err != nil {
			res.ContentType = mt
			return res, err
		}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, f.cfg.MaxBodyBytes+1))
	if err != nil {
		return res, f.explain(ctx, err)
	}
	if int64(len(body)) > f.cfg.MaxBodyBytes {
		body, res.Truncated = body[:f.cfg.MaxBodyBytes], true
	}
	if mediaType == "" {
		mediaType, _, _ = mime.ParseMediaType(http.DetectContentType(body))
		if err := typeAllowed(mediaType); err != nil {
			res.ContentType = mediaType
			return res, err
		}
	}
	res.ContentType = mediaType
	sum := sha256.Sum256(body)
	res.SHA256 = hex.EncodeToString(sum[:])
	res.BodyBytes = len(body)

	// Extraction is bounded in size and runs under the fetch's deadline
	// too, so no document can hold the answer up. It stops on its own
	// soon after the deadline; the answer does not wait for that.
	decoded := decode(readable(body, mediaType, res.Truncated), resp.Header.Get("Content-Type"))
	select {
	case f.extracting <- struct{}{}:
	case <-ctx.Done():
		return res, fmt.Errorf("timed out after %s waiting to read the page's text", f.cfg.Timeout)
	}
	page, err := func() (text.Page, error) {
		defer func() { <-f.extracting }()
		return f.extract(ctx, decoded, mediaType, resp.Request.URL, int(f.cfg.MaxBodyBytes))
	}()
	if err != nil {
		return res, err
	}
	res.Page = page
	switch {
	case len(resp.Header.Values("Content-Type")) > 1:
		// Two content types: a browser may take the other one.
		res.Page.Uncheckable = "the page gives more than one content type"
	case res.Truncated:
		res.Page.Uncheckable = "the page is longer than max_body_bytes, and the rest could hold anything"
	case mediaType == "application/xhtml+xml":
		// XML namespaces can make any element an HTML one, which this
		// HTML parser does not see.
		res.Page.Uncheckable = "the page is XHTML"
	case text.RefreshesSoon(resp.Header.Get("Refresh")) && resp.Header.Get("Refresh") != "":
		res.Page.Uncheckable = "the page refreshes to another within seconds"
	}
	if res.Page.Uncheckable != "" {
		res.Page.Visible = ""
	}
	if err := f.snapshot(ctx, res, body); err != nil {
		return res, fmt.Errorf("could not keep the snapshot: %w", err)
	}
	return res, nil
}

// moved fills in res for a redirect response. The target is resolved
// against the request URL and judged by the same rules as any URL, but
// never requested. A target the agent could not read back exactly, such
// as one holding invisible characters, is refused, since relay compares
// the line verbatim.
func (f *Fetcher) moved(res *Result, resp *http.Response) error {
	target, err := resp.Location()
	if err != nil {
		return &guard.Refusal{Reason: "the page moved to an address that is not a URL"}
	}
	res.Moved = target.String()
	if err := f.policy.CheckURL(target); err != nil {
		return prefixed("a redirect to "+res.Moved+": ", err)
	}
	if res.Moved != text.EscapeOnly(res.Moved) || strings.ContainsAny(res.Moved, " \t\r\n") {
		res.Moved = ""
		return &guard.Refusal{Reason: "the page moved to an address with invisible characters or spaces"}
	}
	return nil
}

// resolveError is a host that did not resolve.
type resolveError string

func (e resolveError) Error() string { return "could not resolve " + string(e) }

// prefixed adds context to a refusal's reason.
func prefixed(context string, err error) error {
	var r *guard.Refusal
	if errors.As(err, &r) {
		return &guard.Refusal{Reason: context + r.Reason}
	}
	return err
}

// readable is the part of a body that can be read as it was written. A
// body cut at the limit may end inside a tag or an entity, where "&#68"
// would read as "D" although the whole page reads "&#680;", so a cut HTML
// body is read only up to its last ">".
func readable(body []byte, mediaType string, truncated bool) []byte {
	if !truncated || mediaType == "text/plain" {
		return body
	}
	if i := bytes.LastIndexByte(body, '>'); i >= 0 {
		return body[:i+1]
	}
	return nil
}

// InProcess reads a body in this process, as the child would, under
// ctx's deadline. The read itself cannot be stopped and goes on after a
// timeout, so it is for tests: webtool reads in a child instead.
func InProcess(textChars int) Extractor {
	return func(ctx context.Context, body []byte, mediaType string, base *url.URL, limit int) (text.Page, error) {
		done := make(chan text.Page, 1)
		go func() { done <- text.Read(ctx, body, mediaType, base, limit, textChars) }()
		select {
		case p := <-done:
			return p, nil
		case <-ctx.Done():
			return text.Page{}, errors.New("timed out reading the page's text")
		}
	}
}

// Extractor returns the fetcher's page reader, so a test can tell which
// one production built.
func (f *Fetcher) Extractor() Extractor { return f.extract }

func typeAllowed(mt string) error {
	switch {
	case allowedTypes[mt]:
		return nil
	case mt == "application/pdf":
		return &guard.Refusal{Reason: "the page is a PDF, and webtool reads only HTML and plain text"}
	default:
		return &guard.Refusal{Reason: fmt.Sprintf("the content type is %s, and webtool reads only HTML and plain text", mt)}
	}
}

// decode converts a body to UTF-8 by its declared or sniffed charset.
func decode(body []byte, contentType string) []byte {
	enc, _, _ := charset.DetermineEncoding(body, contentType)
	out, err := enc.NewDecoder().Bytes(body)
	if err != nil {
		return []byte(strings.ToValidUTF8(string(body), "�"))
	}
	return out
}

// explain turns a client error into a plain reason, keeping a refusal as
// it is.
func (f *Fetcher) explain(ctx context.Context, err error) error {
	var r *guard.Refusal
	if errors.As(err, &r) {
		return r
	}
	var re resolveError
	if errors.As(err, &re) {
		return re
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("timed out after %s", f.cfg.Timeout)
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	var oe *net.OpError
	if errors.As(err, &oe) && oe.Err != nil {
		return fmt.Errorf("could not connect: %v", oe.Err)
	}
	return fmt.Errorf("could not fetch: %v", err)
}

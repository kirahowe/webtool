package guard

import (
	"fmt"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/net/idna"
)

// Refusal is an error that says why a URL or address is not fetched.
type Refusal struct{ Reason string }

func (r *Refusal) Error() string { return "refused: " + r.Reason }

func refuse(format string, args ...any) error {
	return &Refusal{Reason: fmt.Sprintf(format, args...)}
}

// DefaultPorts are the only ports fetch connects to.
var DefaultPorts = []string{"80", "443"}

// Policy is what fetch may reach.
type Policy struct {
	// Local holds this machine's own networks: every interface address
	// with its prefix. Addresses in them are refused even when public,
	// since a server listening on all interfaces answers on each, and an
	// on-link IPv6 prefix holds the other devices on the LAN.
	Local []netip.Prefix
	// Ports are the ports allowed; nil means DefaultPorts.
	Ports []string
	// Allow lets addresses through that every other rule would refuse.
	// Only tests set it, to reach an httptest server on 127.0.0.1. The
	// fetcher that production builds never does.
	Allow []netip.Prefix
	// Origin is one address and port let through that every other rule
	// would refuse. Only the fixture build sets it, to reach a local
	// server of test pages; see fetch.Config.FixtureOrigin.
	Origin netip.AddrPort
}

// CheckAddrPort refuses an address and port fetch must not connect to:
// anything CheckPort or CheckAddr refuses, unless it is exactly Origin.
func (p Policy) CheckAddrPort(a netip.Addr, port string) error {
	if p.Origin.IsValid() && a.WithZone("").Unmap() == p.Origin.Addr() && port == strconv.Itoa(int(p.Origin.Port())) {
		return nil
	}
	if err := p.CheckPort(port); err != nil {
		return err
	}
	return p.CheckAddr(a)
}

func (p Policy) allowed(a netip.Addr) bool {
	for _, pre := range p.Allow {
		if pre.Contains(a) {
			return true
		}
	}
	return false
}

// CheckAddr refuses an address fetch must not connect to.
func (p Policy) CheckAddr(a netip.Addr) error {
	a = a.WithZone("")
	if a.Is4In6() {
		a = a.Unmap()
	}
	if p.allowed(a) {
		return nil
	}
	if why := Classify(a); why != "" {
		return refuse("%s is %s", a, why)
	}
	addrs := []netip.Addr{a}
	if v4, _, ok := embedded(a); ok {
		addrs = append(addrs, v4)
	}
	for _, x := range addrs {
		for _, pre := range p.Local {
			if pre.Contains(x) {
				return refuse("%s is on one of this machine's own networks (%s)", a, pre)
			}
		}
	}
	return nil
}

// CheckPort refuses a port other than the allowed ones. The port is
// compared as written, so "080" is refused although it means 80.
func (p Policy) CheckPort(port string) error {
	ports := p.Ports
	if ports == nil {
		ports = DefaultPorts
	}
	if !slices.Contains(ports, port) {
		return refuse("port %s; only ports %s are fetched", port, strings.Join(ports, " and "))
	}
	return nil
}

// CheckURL refuses a URL fetch must not request, before anything is
// resolved: a scheme other than http or https, credentials, a port other
// than the allowed ones, a host named by the rules in CheckHost, and an
// IP literal CheckAddr refuses. The address actually dialed is checked
// again when connecting, which is what stops a name that resolves to a
// refused address.
func (p Policy) CheckURL(u *url.URL) error {
	switch u.Scheme {
	case "http", "https":
	default:
		return refuse("only http and https URLs are fetched")
	}
	if u.Opaque != "" || u.Host == "" {
		return refuse("the URL has no host")
	}
	if u.User != nil {
		return refuse("the URL carries credentials")
	}
	port := u.Port()
	if port == "" {
		if strings.HasSuffix(u.Host, ":") {
			return refuse("the URL has an empty port")
		}
		port = map[string]string{"http": "80", "https": "443"}[u.Scheme]
	}
	host := u.Hostname()
	if a, err := netip.ParseAddr(host); err == nil {
		if a.Zone() != "" {
			return refuse("%s has a zone", host)
		}
		return p.CheckAddrPort(a, port)
	}
	if err := p.CheckPort(port); err != nil {
		return err
	}
	if strings.Contains(u.Host, "[") {
		return refuse("%q is not a valid IPv6 address", host)
	}
	_, err := CheckHost(host)
	return err
}

// Refused names: these end in a suffix that only ever means something on
// a local network, or the name has no dot at all, which a resolver may
// complete with a local search domain ("router" becoming "router.lan").
var refusedSuffixes = []string{
	"localhost", "local", "internal", "ts.net", "home.arpa", "arpa",
	"lan", "home", "localdomain", "intranet", "corp", "private",
	"test", "invalid", "example", "onion",
}

// CheckHost applies the hostname rules to a host that is not a canonical
// IP literal, and returns its ASCII form. The rules see the name as the
// HTTP client will dial it: mapped to ASCII with IDNA, lower case, without
// a trailing dot. So "ＬＯＣＡＬＨＯＳＴ" and "localhost." are refused like
// "localhost".
//
// The name returned is the one checked. The fetcher still dials the name
// as net/http maps it, and checks the address it connects to.
func CheckHost(host string) (string, error) {
	if host == "" {
		return "", refuse("the URL has no host")
	}
	// net/http maps a non-ASCII host the same way and leaves an ASCII
	// one as it is.
	ascii := host
	if !isASCII(host) {
		var err error
		if ascii, err = idna.Lookup.ToASCII(host); err != nil {
			return "", refuse("%q is not a valid host name", host)
		}
	}
	name := strings.TrimSuffix(strings.ToLower(ascii), ".")
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '.' || r == '_') {
			return "", refuse("%q is not a valid host name", host)
		}
	}
	labels := strings.Split(name, ".")
	if slices.Contains(labels, "") {
		return "", refuse("%q has an empty label", host)
	}
	if numeric(labels[len(labels)-1]) {
		// What a browser, or getaddrinfo, would read as an IPv4 address:
		// 2130706433, 0x7f.1, 0177.0.0.1, 127.1.
		if a, err := netip.ParseAddr(name); err == nil && a.Is4() {
			return "", refuse("%q is an IP address written as a name", host)
		}
		return "", refuse("%q is an IP address in a non-canonical form", host)
	}
	if len(labels) < 2 {
		return "", refuse("%q has no dot, so it can only be a local name", host)
	}
	for _, s := range refusedSuffixes {
		if name == s || strings.HasSuffix(name, "."+s) {
			return "", refuse("%q is a local name (.%s)", host, s)
		}
	}
	return name, nil
}

// numeric reports whether a label is a number in the forms inet_aton and
// the WHATWG URL parser accept: decimal, octal with a leading 0, or hex
// with a leading 0x.
func numeric(label string) bool {
	if l := strings.ToLower(label); strings.HasPrefix(l, "0x") {
		return strings.Trim(l[2:], "0123456789abcdef") == ""
	}
	return label != "" && strings.Trim(label, "0123456789") == ""
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

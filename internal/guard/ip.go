// Package guard decides which URLs and addresses fetch may reach. It is
// pure: the caller supplies this machine's networks.
//
// The threat is a URL chosen by someone who wants to reach a service on
// the operator's machine or network: a local server that acts on a GET,
// the tailnet, the router. So every address that is not plainly on the
// public internet is refused, and so is every way of writing one.
package guard

import (
	"fmt"
	"net/netip"
)

type rule struct {
	prefix netip.Prefix
	reason string
}

func rules(reason string, prefixes ...string) []rule {
	out := make([]rule, len(prefixes))
	for i, p := range prefixes {
		out[i] = rule{netip.MustParsePrefix(p), reason}
	}
	return out
}

func join(rs ...[]rule) []rule {
	var out []rule
	for _, r := range rs {
		out = append(out, r...)
	}
	return out
}

// v4Rules are the IPv4 ranges that are refused, broadcast first so its
// reason wins over the reserved range around it.
var v4Rules = join(
	rules("the broadcast address", "255.255.255.255/32"),
	rules("in 0.0.0.0/8, which means this network", "0.0.0.0/8"),
	rules("a loopback address", "127.0.0.0/8"),
	rules("a private address", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"),
	rules("a shared address (CGNAT, and Tailscale's range)", "100.64.0.0/10"),
	rules("a link-local address", "169.254.0.0/16"),
	rules("reserved for protocol assignments", "192.0.0.0/24"),
	rules("reserved for documentation", "192.0.2.0/24", "198.51.100.0/24", "203.0.113.0/24"),
	rules("the old 6to4 relay range", "192.88.99.0/24"),
	rules("reserved for benchmarking", "198.18.0.0/15"),
	rules("a multicast address", "224.0.0.0/4"),
	rules("a reserved address", "240.0.0.0/4"),
	// Public addresses that a cloud host answers on itself, the way
	// 169.254.169.254 serves metadata: Azure's WireServer.
	rules("a cloud host's own service address", "168.63.129.16/32"),
)

// v6Rules are the IPv6 ranges that are refused outright. Ranges that embed
// an IPv4 address are handled by embedded, before these.
var v6Rules = join(
	rules("the unspecified address", "::/128"),
	rules("a loopback address", "::1/128"),
	rules("an IPv4-compatible address, which is deprecated", "::/96"),
	rules("a local-use NAT64 address", "64:ff9b:1::/48"),
	rules("a discard-only address", "100::/64"),
	rules("an IETF special-purpose address", "2001::/23"),
	rules("reserved for documentation", "2001:db8::/32", "3fff::/20"),
	rules("an SRv6 segment identifier", "5f00::/16"),
	rules("a unique-local address", "fc00::/7"),
	rules("a link-local address", "fe80::/10"),
	rules("a site-local address, which is deprecated", "fec0::/10"),
	rules("a multicast address", "ff00::/8"),
)

var (
	globalUnicast = netip.MustParsePrefix("2000::/3")
	nat64         = netip.MustParsePrefix("64:ff9b::/96")
	sixToFour     = netip.MustParsePrefix("2002::/16")
)

// embedded returns the IPv4 address an IPv6 address carries, for the forms
// that reach it: IPv4-mapped (::ffff:a.b.c.d), NAT64 (64:ff9b::a.b.c.d) and
// 6to4 (2002:aabb:ccdd::).
func embedded(a netip.Addr) (netip.Addr, string, bool) {
	if !a.Is6() {
		return netip.Addr{}, "", false
	}
	b := a.As16()
	switch {
	case a.Is4In6():
		return a.Unmap(), "the IPv4-mapped form of", true
	case nat64.Contains(a):
		return netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}), "the NAT64 form of", true
	case sixToFour.Contains(a):
		return netip.AddrFrom4([4]byte{b[2], b[3], b[4], b[5]}), "the 6to4 form of", true
	}
	return netip.Addr{}, "", false
}

// Classify says why an address is not on the public internet, or returns
// "" when it is. It knows nothing of this machine; see Policy.CheckAddr.
func Classify(a netip.Addr) string {
	a = a.WithZone("")
	if !a.IsValid() {
		return "not a valid address"
	}
	if v4, form, ok := embedded(a); ok {
		if why := Classify(v4); why != "" {
			return fmt.Sprintf("%s %s, which is %s", form, v4, why)
		}
		return ""
	}
	if a.Is4() {
		for _, r := range v4Rules {
			if r.prefix.Contains(a) {
				return r.reason
			}
		}
	} else {
		for _, r := range v6Rules {
			if r.prefix.Contains(a) {
				return r.reason
			}
		}
		if !globalUnicast.Contains(a) {
			return "not a global unicast address"
		}
	}
	// The standard library's own view, in case a range was missed above.
	if a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsMulticast() || a.IsUnspecified() {
		return "not a public address"
	}
	return ""
}

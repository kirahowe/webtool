package guard

import (
	"fmt"
	"net/netip"
	"net/url"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// blockedV4 are the IPv4 ranges the threat model names.
var blockedV4 = []string{
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
	"172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "192.168.0.0/16",
	"198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
	"255.255.255.255/32", "168.63.129.16/32",
}

var blockedV6 = []string{
	"::/128", "::1/128", "::/96", "64:ff9b:1::/48", "100::/64", "2001::/23", "2001:db8::/32",
	"3fff::/20", "5f00::/16", "fc00::/7", "fd7a:115c:a1e0::/48", "fe80::/10", "fec0::/10", "ff00::/8",
	// Not global unicast at all.
	"4000::/2", "8000::/1", "1::/16",
}

// inPrefix draws a random address inside p.
func inPrefix(p netip.Prefix) *rapid.Generator[netip.Addr] {
	return rapid.Custom(func(t *rapid.T) netip.Addr {
		base := p.Masked().Addr().AsSlice()
		bits := p.Bits()
		for i := range base {
			r := byte(rapid.IntRange(0, 255).Draw(t, "byte"))
			// Keep the prefix's bits, randomise the rest.
			keep := bits - 8*i
			switch {
			case keep >= 8:
			case keep <= 0:
				base[i] = r
			default:
				mask := byte(0xff << (8 - keep))
				base[i] = base[i]&mask | r&^mask
			}
		}
		a, _ := netip.AddrFromSlice(base)
		return a
	})
}

func anyPrefix(ps []string) *rapid.Generator[netip.Addr] {
	return rapid.Custom(func(t *rapid.T) netip.Addr {
		p := netip.MustParsePrefix(rapid.SampledFrom(ps).Draw(t, "prefix"))
		return inPrefix(p).Draw(t, "addr")
	})
}

// publicV4 is an independent statement of which IPv4 addresses are public,
// written as octet comparisons rather than prefixes.
func publicV4(a netip.Addr) bool {
	b := a.As4()
	o0, o1, o2 := b[0], b[1], b[2]
	switch {
	case o0 == 0, o0 == 10, o0 == 127, o0 >= 224:
		return false
	case o0 == 100 && o1 >= 64 && o1 < 128:
		return false
	case o0 == 169 && o1 == 254:
		return false
	case o0 == 172 && o1 >= 16 && o1 < 32:
		return false
	case o0 == 192 && o1 == 168:
		return false
	case o0 == 192 && o1 == 0 && (o2 == 0 || o2 == 2):
		return false
	case o0 == 192 && o1 == 88 && o2 == 99:
		return false
	case o0 == 198 && (o1 == 18 || o1 == 19):
		return false
	case o0 == 198 && o1 == 51 && o2 == 100:
		return false
	case o0 == 203 && o1 == 0 && o2 == 113:
		return false
	case b == [4]byte{168, 63, 129, 16}:
		return false
	}
	return true
}

var anyV4 = rapid.Custom(func(t *rapid.T) netip.Addr {
	return netip.AddrFrom4([4]byte(rapid.SliceOfN(rapid.Byte(), 4, 4).Draw(t, "v4")))
})

func TestBlockedV4Refused(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		a := anyPrefix(blockedV4).Draw(t, "addr")
		if Classify(a) == "" {
			t.Fatalf("%s allowed", a)
		}
		if (Policy{}).CheckAddr(a) == nil {
			t.Fatalf("%s allowed by the policy", a)
		}
	})
}

func TestV4ClassifiedAsTheIndependentRuleSays(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		a := anyV4.Draw(t, "addr")
		if got, want := Classify(a) == "", publicV4(a); got != want {
			t.Fatalf("%s: allowed %v, want %v (%q)", a, got, want, Classify(a))
		}
	})
}

func TestPublicV4Allowed(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		a := anyV4.Filter(publicV4).Draw(t, "addr")
		if err := (Policy{}).CheckAddr(a); err != nil {
			t.Fatalf("%s: %v", a, err)
		}
	})
}

// Every way of writing a blocked IPv4 address as IPv6 is refused.
func TestBlockedV4EmbeddedInV6Refused(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		v4 := anyPrefix(blockedV4).Draw(t, "addr").As4()
		forms := map[string]netip.Addr{
			"mapped":     netip.AddrFrom16([16]byte{10: 0xff, 11: 0xff, 12: v4[0], 13: v4[1], 14: v4[2], 15: v4[3]}),
			"compatible": netip.AddrFrom16([16]byte{12: v4[0], 13: v4[1], 14: v4[2], 15: v4[3]}),
			"nat64":      netip.AddrFrom16([16]byte{0: 0x00, 1: 0x64, 2: 0xff, 3: 0x9b, 12: v4[0], 13: v4[1], 14: v4[2], 15: v4[3]}),
			"6to4":       netip.AddrFrom16([16]byte{0: 0x20, 1: 0x02, 2: v4[0], 3: v4[1], 4: v4[2], 5: v4[3], 15: 1}),
		}
		for name, a := range forms {
			if err := (Policy{}).CheckAddr(a); err == nil {
				t.Fatalf("%s form %s allowed", name, a)
			}
		}
	})
}

func TestPublicV4MappedAllowed(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		v4 := anyV4.Filter(publicV4).Draw(t, "addr")
		a := netip.AddrFrom16(v4.As16())
		if err := (Policy{}).CheckAddr(a); err != nil {
			t.Fatalf("%s: %v", a, err)
		}
	})
}

func TestBlockedV6Refused(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		a := anyPrefix(blockedV6).Draw(t, "addr")
		if err := (Policy{}).CheckAddr(a); err == nil {
			t.Fatalf("%s allowed", a)
		}
	})
}

func TestZonedLinkLocalRefused(t *testing.T) {
	a := netip.MustParseAddr("fe80::1%en0")
	if (Policy{}).CheckAddr(a) == nil {
		t.Fatal("a zoned link-local address was allowed")
	}
}

// Addresses in the regional registries' IPv6 blocks are public.
func TestPublicV6Allowed(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		a := anyPrefix([]string{"2400::/12", "2600::/12", "2800::/12", "2a00::/12", "2c00::/12"}).Draw(t, "addr")
		if err := (Policy{}).CheckAddr(a); err != nil {
			t.Fatalf("%s: %v", a, err)
		}
	})
}

func TestLocalNetworksRefused(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		v6 := netip.PrefixFrom(inPrefix(netip.MustParsePrefix("2600::/12")).Draw(t, "v6"), 64).Masked()
		v4 := netip.PrefixFrom(anyV4.Filter(publicV4).Draw(t, "v4"), 32)
		p := Policy{Local: []netip.Prefix{v6, v4}}
		for _, a := range []netip.Addr{inPrefix(v6).Draw(t, "in v6"), v4.Addr(), netip.AddrFrom16(v4.Addr().As16())} {
			if p.CheckAddr(a) == nil {
				t.Fatalf("%s on a local network allowed", a)
			}
		}
	})
}

func TestAllowSeam(t *testing.T) {
	p := Policy{Allow: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}}
	if err := p.CheckAddr(netip.MustParseAddr("127.0.0.1")); err != nil {
		t.Fatal(err)
	}
	if p.CheckAddr(netip.MustParseAddr("127.0.0.2")) == nil {
		t.Fatal("127.0.0.2 allowed")
	}
}

func TestRefusalReason(t *testing.T) {
	err := (Policy{}).CheckAddr(netip.MustParseAddr("10.0.0.1"))
	if err == nil || err.Error() != "refused: 10.0.0.1 is a private address" {
		t.Fatalf("got %v", err)
	}
}

func TestCheckURL(t *testing.T) {
	refused := []string{
		"file:///etc/passwd", "ftp://example.com/", "gopher://example.com/", "javascript:alert(1)",
		"http://user:pass@example.com/", "http://user@example.com/",
		"http://example.com:8080/", "https://example.com:2586/", "http://example.com:080/", "http://example.com:/",
		"http://localhost/", "http://LOCALHOST/", "http://localhost./", "http://foo.localhost/",
		"http://ＬＯＣＡＬＨＯＳＴ/", "http://printer.local/", "http://db.internal/", "http://box.tail1234.ts.net/",
		"http://router.home.arpa/", "http://1.0.0.127.in-addr.arpa/", "http://router.lan/", "http://router/",
		"http://127.0.0.1/", "http://127.0.0.1:2586/publish", "http://[::1]/", "http://[::ffff:127.0.0.1]/",
		"http://[::ffff:7f00:1]/", "http://[64:ff9b::a00:1]/", "http://[fe80::1%25en0]/", "http://[fd7a:115c:a1e0::1]/",
		"http://100.100.100.100/", "http://192.168.1.1/", "http://10.0.0.1/", "http://169.254.169.254/latest/meta-data/",
		"http://0.0.0.0/", "http://255.255.255.255/", "http://168.63.129.16/?comp=versions",
		"http://2130706433/", "http://0x7f.1/", "http://0x7f000001/", "http://0177.0.0.1/", "http://127.1/",
		"http://017700000001/", "http://127.0.0.0x1/", "http://１２７.０.０.１/", "http://example.123/",
		"http:///path", "http://a..b.com/", "http://exa mple.com/",
	}
	for _, s := range refused {
		u, err := url.Parse(s)
		if err != nil {
			continue // unparseable is refused before this
		}
		if err := (Policy{}).CheckURL(u); err == nil {
			t.Errorf("%s allowed", s)
		}
	}
	allowed := []string{
		"http://example.com/", "https://example.com/a?b=c", "https://example.com:443/", "http://example.com:80/",
		"https://EXAMPLE.com./", "https://xn--r8jz45g.jp/", "https://例え.jp/", "http://8.8.8.8/", "http://[2606:4700::1111]/",
		"https://sub_domain.example.org/",
	}
	for _, s := range allowed {
		u, err := url.Parse(s)
		if err != nil {
			t.Fatalf("%s: %v", s, err)
		}
		if err := (Policy{}).CheckURL(u); err != nil {
			t.Errorf("%s: %v", s, err)
		}
	}
}

// Any number in a form inet_aton accepts is refused as a host.
func TestNumericHostsRefused(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		n := rapid.Uint32().Draw(t, "n")
		forms := []string{
			fmt.Sprintf("%d", n), fmt.Sprintf("0x%x", n), fmt.Sprintf("0X%X", n), fmt.Sprintf("0%o", n),
			fmt.Sprintf("%d.%d", n>>24, n&0xffffff), fmt.Sprintf("0x%x.0x%x", n>>24, n&0xffffff),
			fmt.Sprintf("%d.%d.%d", n>>24, n>>16&0xff, n&0xffff),
			fmt.Sprintf("0%o.0%o.0%o.0%o", n>>24, n>>16&0xff, n>>8&0xff, n&0xff),
		}
		for _, h := range forms {
			if _, err := CheckHost(h); err == nil {
				t.Fatalf("%s allowed", h)
			}
			u := &url.URL{Scheme: "http", Host: h, Path: "/"}
			if (Policy{}).CheckURL(u) == nil {
				t.Fatalf("http://%s/ allowed", h)
			}
		}
	})
}

var label = rapid.StringMatching(`[a-z][a-z0-9-]{0,10}[a-z0-9]`)

func TestOrdinaryNamesAllowed(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		labels := rapid.SliceOfN(label, 1, 3).Draw(t, "labels")
		tld := rapid.SampledFrom([]string{"com", "org", "net", "io", "ca", "co.uk", "dev"}).Draw(t, "tld")
		h := strings.Join(append(labels, tld), ".")
		got, err := CheckHost(h)
		if err != nil {
			t.Fatalf("%s: %v", h, err)
		}
		if got != h {
			t.Fatalf("%s came back as %s", h, got)
		}
	})
}

func TestLocalSuffixesRefused(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		labels := rapid.SliceOfN(label, 0, 3).Draw(t, "labels")
		suffix := rapid.SampledFrom(refusedSuffixes).Draw(t, "suffix")
		h := strings.Join(append(labels, suffix), ".")
		if rapid.Bool().Draw(t, "upper") {
			h = strings.ToUpper(h)
		}
		if rapid.Bool().Draw(t, "dot") {
			h += "."
		}
		if _, err := CheckHost(h); err == nil {
			t.Fatalf("%s allowed", h)
		}
	})
}

// An Origin lets through exactly its own address and port: any other
// address or port is judged as it would be without one, and so is any
// URL naming the origin by a host name.
func TestOriginIsExact(t *testing.T) {
	p := Policy{Origin: netip.MustParseAddrPort("127.0.0.1:8123")}
	for _, u := range []string{"http://127.0.0.1:8123/", "https://127.0.0.1:8123/x", "http://[::ffff:127.0.0.1]:8123/"} {
		pu, _ := url.Parse(u)
		if err := p.CheckURL(pu); err != nil {
			t.Errorf("%s: %v", u, err)
		}
	}
	for _, u := range []string{"http://127.0.0.1/", "http://127.0.0.1:8124/", "http://127.0.0.2:8123/", "http://[::1]:8123/",
		"http://localhost:8123/", "http://fixture.example.com:8123/", "http://u@127.0.0.1:8123/", "ftp://127.0.0.1:8123/"} {
		pu, _ := url.Parse(u)
		if err := p.CheckURL(pu); err == nil {
			t.Errorf("%s allowed", u)
		}
	}
	rapid.Check(t, func(t *rapid.T) {
		a := netip.AddrFrom4([4]byte{byte(rapid.IntRange(0, 255).Draw(t, "a")), byte(rapid.IntRange(0, 255).Draw(t, "b")), 0, 1})
		port := rapid.SampledFrom([]string{"80", "443", "8123", "8124"}).Draw(t, "port")
		if a == p.Origin.Addr() && port == "8123" {
			return
		}
		got, want := p.CheckAddrPort(a, port), Policy{}.CheckAddrPort(a, port)
		if (got == nil) != (want == nil) {
			t.Fatalf("%s port %s: %v with the origin, %v without", a, port, got, want)
		}
	})
}

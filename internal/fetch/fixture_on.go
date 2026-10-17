//go:build fixture

package fetch

import (
	"fmt"
	"net/netip"
)

// fixtureOrigin allows o, which must be a loopback address with a port.
// Nothing else that guard refuses is let through.
func fixtureOrigin(o netip.AddrPort) (netip.AddrPort, error) {
	if !o.IsValid() {
		return netip.AddrPort{}, nil
	}
	if !o.Addr().Unmap().IsLoopback() || o.Addr().Zone() != "" || o.Port() == 0 {
		return netip.AddrPort{}, fmt.Errorf("the fixture origin %s is not a loopback address with a port", o)
	}
	return netip.AddrPortFrom(o.Addr().Unmap(), o.Port()), nil
}

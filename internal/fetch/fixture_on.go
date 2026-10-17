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
		return netip.AddrPort{}, fmt.Errorf("fixture_origin %s must be a loopback address and port, such as 127.0.0.1:8123", o)
	}
	return netip.AddrPortFrom(o.Addr().Unmap(), o.Port()), nil
}

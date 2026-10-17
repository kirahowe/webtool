//go:build !fixture

package fetch

import (
	"errors"
	"net/netip"
)

// fixtureOrigin refuses any origin: the release build reaches nothing
// local.
func fixtureOrigin(o netip.AddrPort) (netip.AddrPort, error) {
	if o.IsValid() {
		return netip.AddrPort{}, errors.New("a fixture origin needs the fixture build (go build -tags fixture)")
	}
	return netip.AddrPort{}, nil
}

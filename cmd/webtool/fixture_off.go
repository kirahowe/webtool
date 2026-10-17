//go:build !fixture

package main

import (
	"context"
	"errors"
	"net/netip"

	"github.com/kirahowe/webtool/internal/search"
)

// errNotFixture is why the release build refuses the fixture settings.
var errNotFixture = errors.New("fixture_origin, results_file and the fixture search backend need the fixture build (make fixture)")

// checkFixture refuses every fixture setting: the release build reaches
// nothing local and searches only through Brave.
func checkFixture(c Config) error {
	if c.Fetch.FixtureOrigin != "" || c.Search.ResultsFile != "" || c.Search.Backend == "fixture" {
		return errNotFixture
	}
	return nil
}

func fixtureOrigin(Config) netip.AddrPort { return netip.AddrPort{} }

func fixtureBackend(SearchConfig) search.Backend { return refusedBackend{} }

type refusedBackend struct{}

func (refusedBackend) Search(context.Context, string, int) ([]search.Result, error) {
	return nil, errNotFixture
}

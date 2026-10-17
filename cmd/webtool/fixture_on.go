//go:build fixture

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"

	"github.com/kirahowe/webtool/internal/search"
)

// checkFixture reads the fixture settings: fetch.fixture_origin must be
// an address and port (fetch.New refuses one that is not loopback), and
// the fixture search backend needs an absolute results_file.
func checkFixture(c Config) error {
	if o := c.Fetch.FixtureOrigin; o != "" {
		if _, err := netip.ParseAddrPort(o); err != nil {
			return fmt.Errorf("fixture_origin %q must be an address and port, such as 127.0.0.1:8123", o)
		}
	}
	switch {
	case c.Search.Backend == "fixture" && !filepath.IsAbs(c.Search.ResultsFile):
		return errors.New("the fixture search backend needs an absolute results_file")
	case c.Search.Backend != "fixture" && c.Search.ResultsFile != "":
		return errors.New("results_file is read only by the fixture search backend")
	}
	return nil
}

func fixtureOrigin(c Config) netip.AddrPort {
	ap, _ := netip.ParseAddrPort(c.Fetch.FixtureOrigin)
	return ap
}

func fixtureBackend(c SearchConfig) search.Backend { return fileBackend(c.ResultsFile) }

// fileBackend answers every query with the results in a JSON file, the
// first count of them.
type fileBackend string

func (f fileBackend) Search(_ context.Context, _ string, count int) ([]search.Result, error) {
	raw, err := os.ReadFile(string(f))
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var results []struct {
		Title   string `json:"title"`
		URL     string `json:"url"`
		Snippet string `json:"snippet"`
	}
	if err := dec.Decode(&results); err != nil {
		return nil, fmt.Errorf("%s: %v", f, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("%s: something follows the results", f)
	}
	var out []search.Result
	for _, r := range results[:min(count, len(results))] {
		out = append(out, search.Result{Title: r.Title, URL: r.URL, Snippet: r.Snippet})
	}
	return out, nil
}

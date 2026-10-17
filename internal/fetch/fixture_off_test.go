//go:build !fixture

package fetch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"
)

// The release build cannot be given a fixture origin, and the fetcher it
// builds refuses a local fixture server even with its port opened.
func TestReleaseRefusesFixtureOrigin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("request reached the fixture server: %s", r.URL)
	}))
	defer srv.Close()
	cfg := Config{Timeout: 5 * time.Second, MaxBodyBytes: 1 << 20, SnapshotDir: t.TempDir(), MaxSnapshotBytes: 1 << 30, Extract: InProcess(1 << 20)}
	cfg.FixtureOrigin = netip.MustParseAddrPort(srv.Listener.Addr().String())
	if _, err := New(cfg); err == nil {
		t.Fatal("the release build accepted a fixture origin")
	}
	cfg.FixtureOrigin = netip.AddrPort{}
	f, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.Fetch(context.Background(), srv.URL)
	refusal(t, err, "port "+port(srv))
	f.policy.Ports = []string{port(srv)}
	_, err = f.Fetch(context.Background(), srv.URL)
	refusal(t, err, "127.0.0.1 is a loopback address")
}

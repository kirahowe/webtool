//go:build fixture

package fetch

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"
)

func fixtureConfig(t *testing.T, origin string) Config {
	return Config{Timeout: 5 * time.Second, MaxBodyBytes: 1 << 20, SnapshotDir: t.TempDir(), MaxSnapshotBytes: 1 << 30,
		Extract: InProcess(1 << 20), FixtureOrigin: netip.MustParseAddrPort(origin)}
}

// The fixture build reaches its one origin and nothing else local: not
// another port, another loopback address, a name, or any redirect out of
// the origin into a private range or the tailnet.
func TestFixtureOriginOnly(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("request reached the other server: %s", r.URL)
	}))
	defer other.Close()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		to := map[string]string{
			"/same":     "/page",
			"/tailnet":  "http://100.100.100.100/",
			"/private":  "http://10.0.0.1/",
			"/router":   "http://192.168.1.1/",
			"/other":    other.URL,
			"/port80":   "http://127.0.0.1/",
			"/v6":       "http://[::1]:" + port(srv) + "/",
			"/name":     "http://localhost:" + port(srv) + "/",
			"/rebind":   "http://rebind.example.com:" + port(srv) + "/",
			"/rebind80": "http://rebind.example.com/",
		}[r.URL.Path]
		if to != "" {
			http.Redirect(w, r, to, http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "fixture page")
	}))
	defer srv.Close()
	f, err := New(fixtureConfig(t, srv.Listener.Addr().String()))
	if err != nil {
		t.Fatal(err)
	}
	f.lookup = fakeLookup(map[string]string{"rebind.example.com": "127.0.0.1"})
	res, err := f.Fetch(context.Background(), srv.URL+"/page")
	if err != nil || res.Page.Text != "fixture page" {
		t.Fatalf("/page: %v %+v", err, res)
	}
	// A redirect inside the origin is reported, not followed.
	res, err = f.Fetch(context.Background(), srv.URL+"/same")
	if err != nil || res.Moved != srv.URL+"/page" {
		t.Fatalf("/same: %v %+v", err, res)
	}
	if f.controls.Load() == 0 {
		t.Fatal("the dial skipped the Control hook")
	}
	for path, want := range map[string]string{
		"/tailnet": "100.100.100.100 is a shared address",
		"/private": "10.0.0.1 is a private address",
		"/router":  "192.168.1.1 is a private address",
		"/other":   "port " + port(other),
		"/port80":  "127.0.0.1 is a loopback address",
		"/v6":      "port " + port(srv),
		"/name":    "port " + port(srv),
		"/rebind":  "port " + port(srv),
	} {
		_, err := f.Fetch(context.Background(), srv.URL+path)
		refusal(t, err, want)
	}
	// A name the guard cannot judge by its form is reported unresolved.
	// The target is never dialed, so the resolver is not asked about it.
	res, err = f.Fetch(context.Background(), srv.URL+"/rebind80")
	if err != nil || res.Moved != "http://rebind.example.com/" {
		t.Fatalf("/rebind80: %v %+v", err, res)
	}
	for u, want := range map[string]string{
		other.URL:                             "port " + port(other),
		"http://127.0.0.2:" + port(srv) + "/": "port " + port(srv),
		"http://localhost:" + port(srv) + "/": "port " + port(srv),
	} {
		_, err := f.Fetch(context.Background(), u)
		refusal(t, err, want)
	}
}

// Only a loopback address and port can be the origin.
func TestFixtureOriginMustBeLoopback(t *testing.T) {
	for _, o := range []string{"10.0.0.1:8123", "100.100.100.100:80", "0.0.0.0:8123", "8.8.8.8:443", "127.0.0.1:0"} {
		if _, err := New(fixtureConfig(t, o)); err == nil {
			t.Errorf("%s accepted as a fixture origin", o)
		}
	}
	if _, err := New(fixtureConfig(t, "[::1]:8123")); err != nil {
		t.Errorf("[::1]:8123: %v", err)
	}
}

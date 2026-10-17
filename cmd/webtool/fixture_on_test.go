//go:build fixture

package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFixtureSettings(t *testing.T) {
	for _, bad := range []string{
		`{"snapshot_dir": "SNAPSHOTS", "fetch": {"fixture_origin": "localhost:8123"}}`,
		`{"snapshot_dir": "SNAPSHOTS", "fetch": {"fixture_origin": "127.0.0.1"}}`,
		`{"snapshot_dir": "SNAPSHOTS", "search": {"backend": "fixture"}}`,
		`{"snapshot_dir": "SNAPSHOTS", "search": {"backend": "fixture", "results_file": "results.json"}}`,
		`{"snapshot_dir": "SNAPSHOTS", "search": {"backend": "brave", "results_file": "/tmp/results.json"}}`,
	} {
		if _, err := loadConfig(writeConfig(t, bad)); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	// A parsed origin that is not loopback stops fetch and check with
	// the reason, before anything is fetched.
	cfg := writeConfig(t, `{"snapshot_dir": "SNAPSHOTS", "fetch": {"fixture_origin": "10.0.0.1:80", "extract_sandbox": "off"}}`)
	out, errOut, code := exec(newApp(), []string{"--config", cfg, "fetch"}, call(`{"url": "http://10.0.0.1/"}`))
	if code != 1 || out != "" || !strings.Contains(errOut, "fixture_origin 10.0.0.1:80 must be a loopback address") {
		t.Fatalf("exit %d, %q, %q", code, out, errOut)
	}
}

// The fixture build, through newApp as the binary runs it, searches from
// a results file and fetches from its origin alone.
func TestFixtureEndToEnd(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("request reached the other server: %s", r.URL)
	}))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/tailnet" {
			http.Redirect(w, r, "http://100.100.100.100/", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<p>an attack page</p>")
	}))
	defer srv.Close()
	results := filepath.Join(t.TempDir(), "results.json")
	if err := os.WriteFile(results, []byte(`[{"title": "One", "url": "`+srv.URL+`/", "snippet": "first"}, {"title": "Two", "url": "https://two.example/"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := writeConfig(t, `{"snapshot_dir": "SNAPSHOTS",
	  "fetch": {"fixture_origin": "`+srv.Listener.Addr().String()+`", "extract_sandbox": "off"},
	  "search": {"backend": "fixture", "results_file": "`+results+`"}}`)

	out, errOut, code := exec(newApp(), []string{"--config", cfg, "search"}, call(`{"query": "anything", "count": 1}`))
	res := oneResult(t, out)
	if code != 0 || res.IsError || !strings.Contains(res.Content, "1. One") || strings.Contains(res.Content, "Two") {
		t.Fatalf("search: exit %d %+v %s", code, res, errOut)
	}

	out, errOut, code = exec(newApp(), []string{"--config", cfg, "fetch"}, call(`{"url": "`+srv.URL+`/"}`))
	res = oneResult(t, out)
	if code != 0 || res.IsError || !strings.Contains(res.Content, "an attack page") {
		t.Fatalf("fetch: exit %d %+v %s", code, res, errOut)
	}
	for u, want := range map[string]string{
		srv.URL + "/tailnet": "100.100.100.100 is a shared address",
		other.URL + "/":      "refused: port",
	} {
		out, _, _ = exec(newApp(), []string{"--config", cfg, "fetch"}, call(`{"url": "`+u+`"}`))
		if res := oneResult(t, out); !res.IsError || !strings.Contains(res.Content, want) {
			t.Errorf("%s: %+v", u, res)
		}
	}
}

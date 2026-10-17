//go:build !fixture

package main

import "testing"

// The release build refuses every fixture setting, so a fixture config
// handed to the release binary fails loudly instead of fetching.
func TestReleaseRefusesFixtureSettings(t *testing.T) {
	for _, bad := range []string{
		`{"snapshot_dir": "SNAPSHOTS", "fetch": {"fixture_origin": "127.0.0.1:8123"}}`,
		`{"snapshot_dir": "SNAPSHOTS", "search": {"backend": "fixture", "results_file": "/tmp/results.json"}}`,
		`{"snapshot_dir": "SNAPSHOTS", "search": {"backend": "fixture"}}`,
		`{"snapshot_dir": "SNAPSHOTS", "search": {"backend": "brave", "results_file": "/tmp/results.json"}}`,
	} {
		if _, err := loadConfig(writeConfig(t, bad)); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

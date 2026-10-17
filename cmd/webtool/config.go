package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/kirahowe/webtool/internal/extract"
	"github.com/kirahowe/webtool/internal/fetch"
)

// Config is the operator's settings file. Nothing is read from the
// environment.
type Config struct {
	SnapshotDir string       `json:"snapshot_dir"`
	Fetch       FetchConfig  `json:"fetch"`
	Search      SearchConfig `json:"search"`
}

// FetchConfig bounds fetch and check. Zero means the default.
type FetchConfig struct {
	TimeoutSeconds int   `json:"timeout_seconds"` // default 20
	MaxBodyBytes   int64 `json:"max_body_bytes"`  // default 5000000
	MaxTextChars   int   `json:"max_text_chars"`  // default 20000
	// MaxSnapshotBytes bounds snapshot_dir; the oldest snapshots go
	// first. Default 2 GB.
	MaxSnapshotBytes int64 `json:"max_snapshot_bytes"`
	// MaxReadMemoryBytes bounds the child process that reads a page's
	// text. Default 512 MB.
	MaxReadMemoryBytes int64 `json:"max_read_memory_bytes"`
	// ExtractSandbox is "required" (the default on macOS) or "off" (the
	// default elsewhere, and for testing inside another sandbox).
	ExtractSandbox string `json:"extract_sandbox"`
	// FixtureOrigin is the loopback host:port the fixture build may
	// fetch from. The release build refuses a config that sets it.
	FixtureOrigin string `json:"fixture_origin"`
}

// maxBodyLimit bounds max_body_bytes: the reader is given the body whole.
const maxBodyLimit = fetch.MaxBodyLimit

// SearchConfig picks the search backend.
type SearchConfig struct {
	Backend        string   `json:"backend"`         // "brave", or "fixture" in the fixture build
	APIKeyCommand  []string `json:"api_key_command"` // run without a shell
	TimeoutSeconds int      `json:"timeout_seconds"` // default 20
	// ResultsFile holds the fixture backend's results, a JSON array of
	// {title, url, snippet}.
	ResultsFile string `json:"results_file"`
}

const (
	defaultTimeoutSeconds = 20
	defaultMaxBodyBytes   = 5_000_000
	defaultMaxTextChars   = 20_000
	defaultMaxSnapshots   = 2_000_000_000
)

// loadConfig reads the config file strictly: an unknown key is an error,
// so a misspelt limit is not silently the default.
func loadConfig(path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var c Config
	if err := dec.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("%s: %v", path, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return Config{}, fmt.Errorf("%s: something follows the settings", path)
	}
	if c.Fetch.TimeoutSeconds == 0 {
		c.Fetch.TimeoutSeconds = defaultTimeoutSeconds
	}
	if c.Fetch.MaxBodyBytes == 0 {
		c.Fetch.MaxBodyBytes = defaultMaxBodyBytes
	}
	if c.Fetch.MaxTextChars == 0 {
		c.Fetch.MaxTextChars = defaultMaxTextChars
	}
	if c.Fetch.ExtractSandbox == "" {
		c.Fetch.ExtractSandbox = "off"
		if runtime.GOOS == "darwin" {
			c.Fetch.ExtractSandbox = "required"
		}
	}
	if c.Fetch.MaxReadMemoryBytes == 0 {
		c.Fetch.MaxReadMemoryBytes = extract.DefaultMemory
	}
	if c.Fetch.MaxSnapshotBytes == 0 {
		c.Fetch.MaxSnapshotBytes = defaultMaxSnapshots
	}
	if c.Search.TimeoutSeconds == 0 {
		c.Search.TimeoutSeconds = defaultTimeoutSeconds
	}
	switch {
	case c.SnapshotDir == "" || !filepath.IsAbs(c.SnapshotDir):
		return Config{}, fmt.Errorf("%s: snapshot_dir must be an absolute path", path)
	case c.Fetch.TimeoutSeconds < 0 || c.Fetch.MaxBodyBytes < 0 || c.Fetch.MaxTextChars < 0 || c.Fetch.MaxSnapshotBytes < 0 || c.Fetch.MaxReadMemoryBytes < 0 || c.Search.TimeoutSeconds < 0:
		return Config{}, fmt.Errorf("%s: limits must be positive", path)
	case c.Fetch.MaxBodyBytes > maxBodyLimit:
		return Config{}, fmt.Errorf("%s: max_body_bytes may be at most %d", path, maxBodyLimit)
	case c.Fetch.ExtractSandbox != "required" && c.Fetch.ExtractSandbox != "off":
		return Config{}, fmt.Errorf("%s: extract_sandbox must be \"required\" or \"off\"", path)
	}
	if err := checkFixture(c); err != nil {
		return Config{}, fmt.Errorf("%s: %v", path, err)
	}
	return c, nil
}

func (c Config) fetchConfig() fetch.Config {
	return fetch.Config{
		Timeout:          time.Duration(c.Fetch.TimeoutSeconds) * time.Second,
		MaxBodyBytes:     c.Fetch.MaxBodyBytes,
		SnapshotDir:      c.SnapshotDir,
		MaxSnapshotBytes: c.Fetch.MaxSnapshotBytes,
		ExtractMemory:    c.Fetch.MaxReadMemoryBytes,
		FixtureOrigin:    fixtureOrigin(c),
	}
}

func (c Config) searchReady() error {
	switch {
	case c.Search.Backend == "":
		return errors.New("the config has no search backend")
	case c.Search.Backend == "fixture":
		return nil // checkFixture has read it
	case c.Search.Backend != "brave":
		return fmt.Errorf("unknown search backend %q; the one backend is brave", c.Search.Backend)
	case len(c.Search.APIKeyCommand) == 0:
		return errors.New("the config has no search api_key_command")
	}
	return nil
}

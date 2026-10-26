package fetch

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/kirahowe/webtool/internal/text"
)

// Meta describes one snapshot, in <sha256>.json beside the raw body
// (<sha256>.body) and its text (<sha256>.txt).
type Meta struct {
	URL            string    `json:"url"`
	FinalURL       string    `json:"final_url"`
	Status         int       `json:"status"`
	ContentType    string    `json:"content_type"`
	FetchedAt      time.Time `json:"fetched_at"`
	SHA256         string    `json:"sha256"`
	BodyBytes      int       `json:"body_bytes"`
	Truncated      bool      `json:"truncated"`
	HiddenChars    int       `json:"hidden_chars"`
	InvisibleChars int       `json:"invisible_chars"`
}

// SnapshotText is the text kept for a page: its title and text, with
// invisible characters escaped, as the agent sees it.
func SnapshotText(p text.Page) (string, int) {
	s := p.Text
	if p.Title != "" {
		s = "title: " + p.Title + "\n\n" + s
	}
	return text.Escape(s)
}

// snapshot keeps the raw body, its text and the fetch's metadata. The
// files are named by the body's SHA-256, so a body fetched twice is kept
// once; the metadata describes the latest fetch of it.
func (f *Fetcher) snapshot(ctx context.Context, res *Result, body []byte) error {
	dir := f.cfg.SnapshotDir
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// Other webtool runs may share the directory. The lock keeps a
	// snapshot's three files and the pruning that follows together.
	unlock, err := lockDir(ctx, dir)
	if err != nil {
		return err
	}
	defer unlock()
	txt, invisible := SnapshotText(res.Page)
	// The URLs are the attacker's text too, and the operator may read
	// this file in a terminal.
	meta, err := json.MarshalIndent(Meta{
		URL: text.EscapeOnly(res.URL), FinalURL: text.EscapeOnly(res.FinalURL), Status: res.Status, ContentType: res.ContentType,
		FetchedAt: res.FetchedAt, SHA256: res.SHA256, BodyBytes: res.BodyBytes,
		Truncated: res.Truncated, HiddenChars: res.Page.Hidden, InvisibleChars: invisible,
	}, "", "  ")
	if err != nil {
		return err
	}
	base := filepath.Join(dir, res.SHA256)
	if err := writeFile(base+".body", body); err != nil {
		return err
	}
	if err := writeFile(base+".txt", []byte(txt)); err != nil {
		return err
	}
	if err := writeFile(base+".json", append(meta, '\n')); err != nil {
		return err
	}
	return prune(dir, f.cfg.MaxSnapshotBytes, res.SHA256)
}

// lockDir takes an exclusive lock on dir's .lock file until unlock,
// waiting no longer than ctx allows.
func lockDir(ctx context.Context, dir string) (func(), error) {
	lf, err := os.OpenFile(filepath.Join(dir, ".lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EINTR {
			lf.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			lf.Close()
			return nil, errors.New("another webtool run held the snapshot directory past the time limit")
		case <-time.After(10 * time.Millisecond):
		}
	}
	return func() {
		syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)
		lf.Close()
	}, nil
}

// snapshotName matches the files webtool writes: 64 hex digits, then
// .body, .txt or .json. Nothing else in the directory is touched.
func snapshotName(name string) (string, bool) {
	ext := filepath.Ext(name)
	id := strings.TrimSuffix(name, ext)
	if ext != ".body" && ext != ".txt" && ext != ".json" || len(id) != 64 || strings.Trim(id, "0123456789abcdef") != "" {
		return "", false
	}
	return id, true
}

// prune removes the oldest snapshots until the directory holds at most
// limit bytes, never the one just written. Each snapshot's three files go
// together. Only files named as snapshots are counted or removed.
func prune(dir string, limit int64, keep string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	type snap struct {
		files  []string
		bytes  int64
		newest time.Time
	}
	snaps := map[string]*snap{}
	var total int64
	for _, e := range entries {
		name := e.Name()
		id, ok := snapshotName(name)
		if !ok && strings.HasPrefix(name, ".tmp-") && e.Type().IsRegular() {
			// Left by a run that died mid-write; the lock is held, so
			// no live write owns one older than a minute.
			if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > time.Minute {
				os.Remove(filepath.Join(dir, name))
			}
			continue
		}
		if !ok || !e.Type().IsRegular() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		s := snaps[id]
		if s == nil {
			s = &snap{}
			snaps[id] = s
		}
		s.files = append(s.files, name)
		s.bytes += info.Size()
		if info.ModTime().After(s.newest) {
			s.newest = info.ModTime()
		}
		total += info.Size()
	}
	if total <= limit {
		return nil
	}
	ids := make([]string, 0, len(snaps))
	for id := range snaps {
		if id != keep {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return snaps[ids[i]].newest.Before(snaps[ids[j]].newest) })
	for _, id := range ids {
		if total <= limit {
			break
		}
		for _, name := range snaps[id].files {
			if err := os.Remove(filepath.Join(dir, name)); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		total -= snaps[id].bytes
	}
	return nil
}

// writeFile writes atomically, so a snapshot is never half written.
func writeFile(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

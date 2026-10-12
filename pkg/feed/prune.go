package feed

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/qiangli/coreutils/pkg/lockfile"
)

// RetentionPeriod is how long fetched spool entries and per-subscriber
// cursor files are kept: 30 days. The post itself lives on the author's
// sink for as long as the author keeps it; the local spool is only a cache
// of pointers, so anything expired can be rebuilt by fetching again.
const RetentionPeriod = 30 * 24 * time.Hour

// PruneRetention drops spool entries fetched more than maxAge ago and
// removes cursor files untouched for longer than maxAge. Spool seq numbers
// are never reassigned, so a reader's cursor keeps pointing past the same
// entries. Entries with an unreadable Fetched timestamp are kept.
func PruneRetention(dir string, now time.Time, maxAge time.Duration) (droppedSpool, droppedCursors int, err error) {
	droppedSpool, err = pruneSpool(dir, now, maxAge)
	if err != nil {
		return 0, 0, err
	}
	droppedCursors, err = pruneCursors(dir, now, maxAge)
	if err != nil {
		return droppedSpool, 0, err
	}
	return droppedSpool, droppedCursors, nil
}

func pruneSpool(dir string, now time.Time, maxAge time.Duration) (int, error) {
	cutoff := now.Add(-maxAge)
	lock, err := lockfile.AcquireWithin(filepath.Join(dir, spoolFile+".lock"), 15*time.Second,
		lockfile.Holder{Name: "feed", Intent: "spool prune"})
	if err != nil {
		return 0, fmt.Errorf("feed: spool lock: %w", err)
	}
	defer lock.Release()

	entries, err := ReadSpool(dir)
	if err != nil {
		return 0, err
	}
	keep := entries[:0]
	dropped := 0
	for _, e := range entries {
		fetched, ferr := parseFetched(e.Fetched)
		if ferr == nil && fetched.Before(cutoff) {
			dropped++
			continue
		}
		keep = append(keep, e)
	}
	if dropped == 0 {
		return 0, nil
	}
	var b strings.Builder
	for _, e := range keep {
		line, err := json.Marshal(e)
		if err != nil {
			return 0, err
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	tmp, err := os.CreateTemp(dir, "spool-*.jsonl")
	if err != nil {
		return 0, fmt.Errorf("feed: prune spool: %w", err)
	}
	name := tmp.Name()
	if _, err := tmp.WriteString(b.String()); err != nil {
		tmp.Close()
		os.Remove(name)
		return 0, fmt.Errorf("feed: prune spool: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return 0, fmt.Errorf("feed: prune spool: %w", err)
	}
	if err := os.Chmod(name, 0o600); err != nil {
		os.Remove(name)
		return 0, fmt.Errorf("feed: prune spool: %w", err)
	}
	if err := os.Rename(name, spoolPath(dir)); err != nil {
		os.Remove(name)
		return 0, fmt.Errorf("feed: prune spool: %w", err)
	}
	return dropped, nil
}

func parseFetched(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339Nano, s)
}

// pruneCursors removes per-subscriber cursor files whose content has not
// been touched for longer than maxAge. A missing cursors directory means
// nothing to prune.
func pruneCursors(dir string, now time.Time, maxAge time.Duration) (int, error) {
	cdir := filepath.Join(dir, "cursors")
	names, err := os.ReadDir(cdir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("feed: prune cursors: %w", err)
	}
	cutoff := now.Add(-maxAge)
	dropped := 0
	for _, n := range names {
		if n.IsDir() || strings.HasSuffix(n.Name(), ".tmp") || strings.HasSuffix(n.Name(), ".lock") {
			continue
		}
		info, err := n.Info()
		if err != nil {
			continue
		}
		if !info.ModTime().Before(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(cdir, n.Name())); err != nil && !os.IsNotExist(err) {
			return dropped, fmt.Errorf("feed: prune cursors: %w", err)
		}
		dropped++
	}
	return dropped, nil
}

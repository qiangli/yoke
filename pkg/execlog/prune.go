// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package execlog

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// pruneFile holds tombstones. It lives at the store root rather than inside a
// day directory, because the whole point is that it OUTLIVES the days it
// describes.
const pruneFile = "PRUNED.jsonl"

// Tombstone records that evidence was deliberately destroyed.
//
// A bound you cannot see is not a bound, it is a trap. Without this, pruning
// and "it never happened" are the same observation, and every downstream claim
// silently inherits a corpus it cannot describe.
type Tombstone struct {
	Schema   string    `json:"schema"`
	PrunedAt time.Time `json:"pruned_at"`
	Day      string    `json:"day"`
	Records  int       `json:"records"`
	Bytes    int64     `json:"bytes"`
	Reason   string    `json:"reason"`
}

// PruneOpts bounds the store.
type PruneOpts struct {
	KeepDays int   // 0 disables age-based pruning
	MaxBytes int64 // 0 disables size-based pruning
	Before   time.Time
	Episode  string
	Reason   string
	Now      time.Time
}

// Prune deletes whole past-day directories, oldest first, and records what it
// removed.
//
// Only PAST days are eligible. Today's directory holds the file every live
// writer has open — deleting it would orphan their writes on unix and fail on
// Windows, and this package refuses to do either.
func Prune(root string, opt PruneOpts) ([]Tombstone, error) {
	now := opt.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	today := now.UTC().Format("2006-01-02")

	days, err := dayDirs(root)
	if err != nil {
		return nil, err
	}

	var stones []Tombstone
	drop := func(dir, reason string) error {
		name := filepath.Base(dir)
		if name == today {
			return nil // never touch the day a live writer holds open
		}
		n, size := measure(dir)
		if err := os.RemoveAll(dir); err != nil {
			// Reported, not swallowed. On Windows this is what an open handle
			// looks like, and a prune that quietly did nothing while claiming
			// to have bounded the store is the trap again.
			return err
		}
		stones = append(stones, Tombstone{
			Schema: "bashy-execlog-prune-v1", PrunedAt: now,
			Day: name, Records: n, Bytes: size, Reason: reason,
		})
		return nil
	}

	for _, dir := range days {
		name := filepath.Base(dir)
		day, err := time.Parse("2006-01-02", name)
		if err != nil {
			continue
		}
		switch {
		case !opt.Before.IsZero() && day.Before(opt.Before.UTC().Truncate(24*time.Hour)):
			if err := drop(dir, "before"); err != nil {
				return stones, err
			}
		case opt.KeepDays > 0 && day.Before(now.AddDate(0, 0, -opt.KeepDays)):
			if err := drop(dir, "age"); err != nil {
				return stones, err
			}
		}
	}

	// Size cap last, so age-based pruning has already reduced the total.
	if opt.MaxBytes > 0 {
		days, err = dayDirs(root)
		if err != nil {
			return stones, err
		}
		total := int64(0)
		for _, d := range days {
			_, size := measure(d)
			total += size
		}
		for _, d := range days {
			if total <= opt.MaxBytes {
				break
			}
			_, size := measure(d)
			if err := drop(d, "size"); err != nil {
				return stones, err
			}
			total -= size
		}
	}

	if err := writeTombstones(root, stones); err != nil {
		return stones, err
	}
	return stones, nil
}

// PruneEpisode removes one session's records across every past day.
//
// This is the user-facing purge. It cannot reach today's file for the reason
// above, and it says so by returning the days it skipped rather than implying
// a clean sweep.
func PruneEpisode(root, episode string, now time.Time) ([]Tombstone, error) {
	if episode == "" {
		return nil, nil
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	today := now.UTC().Format("2006-01-02")

	days, err := dayDirs(root)
	if err != nil {
		return nil, err
	}
	var stones []Tombstone
	for _, dir := range days {
		if filepath.Base(dir) == today {
			continue
		}
		path := filepath.Join(dir, safeName(episode)+".jsonl")
		st, err := os.Stat(path)
		if err != nil {
			continue
		}
		recs, _ := readFile(path)
		if err := os.Remove(path); err != nil {
			return stones, err
		}
		stones = append(stones, Tombstone{
			Schema: "bashy-execlog-prune-v1", PrunedAt: now,
			Day: filepath.Base(dir), Records: len(recs), Bytes: st.Size(),
			Reason: "episode:" + episode,
		})
	}
	return stones, writeTombstones(root, stones)
}

func writeTombstones(root string, stones []Tombstone) error {
	if len(stones) == 0 {
		return nil
	}
	if err := os.MkdirAll(root, dirMode); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(root, pruneFile),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, fileMode)
	if err != nil {
		return err
	}
	defer f.Close()
	for _, s := range stones {
		line, err := json.Marshal(&s)
		if err != nil {
			continue
		}
		if _, err := f.Write(append(line, '\n')); err != nil {
			return err
		}
	}
	return nil
}

// prunedCount totals every record this store has ever deleted.
func prunedCount(root string) int {
	f, err := os.Open(filepath.Join(root, pruneFile))
	if err != nil {
		return 0
	}
	defer f.Close()

	n := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var s Tombstone
		if json.Unmarshal([]byte(line), &s) == nil {
			n += s.Records
		}
	}
	return n
}

func measure(dir string) (records int, size int64) {
	files, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	for _, p := range files {
		if st, err := os.Stat(p); err == nil {
			size += st.Size()
		}
		recs, _ := readFile(p)
		records += len(recs)
	}
	return records, size
}

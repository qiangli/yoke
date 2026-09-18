// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package handoff

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// DefaultDir is the host-local store: ~/.bashy/handoff/. This matches the
// convention 7 of 9 existing packages already use (weave, sprint, kb, meet, …)
// rather than inventing a fourth state root.
//
// The store is a CACHE of records, not their definition. A Record is a file and
// means the same thing anywhere — the store is simply where this host keeps the
// ones it knows about. That distinction is the whole portability claim: you can
// hand someone a record on a USB stick and they can resume from it.
func DefaultDir() string {
	if v := os.Getenv("BASHY_HANDOFF_DIR"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "bashy-handoff")
	}
	return filepath.Join(home, ".bashy", "handoff")
}

// NewID mints a stable, sortable id: <utc-timestamp>-<short project hash>.
// Sortable so `handoff list` is chronological without parsing; project-hashed so
// two projects' handoffs never collide.
func NewID(now time.Time, primaryRoot string) string {
	h := sha256.Sum256([]byte(primaryRoot))
	return fmt.Sprintf("%s-%s", now.UTC().Format("20060102T150405Z"), hex.EncodeToString(h[:])[:8])
}

// Save writes a record atomically (temp + rename), so a reader — possibly
// another agent, possibly mid-crash — never sees a half-written handoff. This is
// the same discipline kb uses for its pages.
func Save(dir string, r *Record) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	r.SchemaVersion = SchemaVersion
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, r.ID+".json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", err
	}
	return path, nil
}

// Load reads one record from any path. Deliberately takes a PATH, not an id: a
// record that arrived by scp, by mesh, or in an email attachment must be
// loadable without first being filed into this host's store.
func Load(path string) (*Record, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r Record
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("%s: not a handoff record: %w", path, err)
	}
	if r.SchemaVersion == "" {
		return nil, fmt.Errorf("%s: missing schema_version (not a handoff record)", path)
	}
	return &r, nil
}

// List returns every record in the store, newest first.
func List(dir string) ([]*Record, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []*Record
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		r, err := Load(filepath.Join(dir, e.Name()))
		if err != nil {
			continue // a corrupt file must not hide the healthy ones
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

// Pending returns the un-resumed handoffs whose project intersects the given
// path set, newest first.
//
// INTERSECTION, not equality — a project spans repos. If a session handed off
// while working across bashy + sh + coreutils, an agent that later opens *sh*
// must still be told the handoff exists. Keying on a single .git root is exactly
// the mistake that made a three-repo regression invisible.
//
// This is what `bashy context --json` calls, so a COLD session — of ANY tool, on
// its very first call — discovers the pending work without being told to look.
// That is the more important half of the feature: handoff is only as good as
// resume's discoverability.
func Pending(dir string, roots []string) ([]*Record, error) {
	all, err := List(dir)
	if err != nil {
		return nil, err
	}
	var out []*Record
	for _, r := range all {
		// Claimable = unclaimed and live: a seat mid-transfer or an open task.
		// Excludes active (held), superseded, cancelled, and stale.
		if s := r.Status(); s != "transferring" && s != "open" {
			continue
		}
		if intersects(r.Project.Roots, roots) || intersects([]string{r.Project.Primary}, roots) {
			out = append(out, r)
		}
	}
	return out, nil
}

// Prune deletes RETIRED handoff records — superseded, cancelled, or stale —
// leaving every LIVE one untouched. Crucially it never deletes an ACTIVE seat:
// the record that stamps who holds the seat is the thing that answers "who is
// the steward?", so pruning it would be self-defeating (the bug that lost
// codex's seat trail). A transfer in flight and an open task are kept too.
// Returns the number removed.
func Prune(dir string) (int, error) {
	all, err := List(dir)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range all {
		if r.Live() { // active seat / in-flight transfer / open task — keep
			continue
		}
		if err := os.Remove(filepath.Join(dir, r.ID+".json")); err == nil {
			n++
		}
	}
	return n, nil
}

func intersects(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if x == "" || y == "" {
				continue
			}
			if sameOrUnder(x, y) || sameOrUnder(y, x) {
				return true
			}
		}
	}
	return false
}

// sameOrUnder reports whether p is a or lives beneath it. Path containment, not
// string equality: an agent sitting in <repo>/internal/agentos is working in
// <repo>, and a claim or a handoff on the repo must find it.
func sameOrUnder(parent, p string) bool {
	parent = filepath.Clean(parent)
	p = filepath.Clean(p)
	if parent == p {
		return true
	}
	rel, err := filepath.Rel(parent, p)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

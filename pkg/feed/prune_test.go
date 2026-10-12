package feed

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func spoolPostRaw(t *testing.T, id string) []byte {
	t.Helper()
	raw, err := (&Post{ID: id, Author: "tester", Headline: "H", Body: "body " + id}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestPruneRetentionDropsOldSpoolEntries(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	if _, err := Ingest(dir, "src", "posts/old.md", spoolPostRaw(t, "old"), now.Add(-31*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := Ingest(dir, "src", "posts/new.md", spoolPostRaw(t, "new"), now.Add(-24*time.Hour)); err != nil {
		t.Fatal(err)
	}

	dropped, _, err := PruneRetention(dir, now, RetentionPeriod)
	if err != nil {
		t.Fatal(err)
	}
	if dropped != 1 {
		t.Fatalf("dropped = %d, want 1", dropped)
	}
	entries, err := ReadSpool(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].ID != "new" {
		t.Fatalf("spool = %+v", entries)
	}
	// Seq numbers are stable: the survivor keeps seq 2.
	if entries[0].Seq != 2 {
		t.Fatalf("survivor seq = %d, want 2", entries[0].Seq)
	}

	// Nothing left to prune: no rewrite, no drops.
	if dropped, _, err := PruneRetention(dir, now, RetentionPeriod); err != nil || dropped != 0 {
		t.Fatalf("second prune: dropped %d, err %v", dropped, err)
	}
}

func TestPruneRetentionKeepsMalformedTimestamps(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	e, err := Ingest(dir, "src", "posts/x.md", spoolPostRaw(t, "x"), now)
	if err != nil {
		t.Fatal(err)
	}
	_ = e
	// Corrupt the Fetched field of the single entry.
	raw, _ := os.ReadFile(spoolPath(dir))
	old := `"fetched":"` + now.UTC().Format(time.RFC3339) + `"`
	raw2 := []byte(strings.Replace(string(raw), old, `"fetched":"not-a-time"`, 1))
	if err := os.WriteFile(spoolPath(dir), raw2, 0o600); err != nil {
		t.Fatal(err)
	}
	if dropped, _, err := PruneRetention(dir, now, RetentionPeriod); err != nil || dropped != 0 {
		t.Fatalf("malformed Fetched: dropped %d, err %v", dropped, err)
	}
}

func TestPruneRetentionDropsOldCursors(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	for _, sub := range []string{"oldsub", "newsub"} {
		if err := WriteCursor(dir, sub, 3); err != nil {
			t.Fatal(err)
		}
	}
	oldPath, err := cursorFileForTest(dir, "oldsub")
	if err != nil {
		t.Fatal(err)
	}
	aged := now.Add(-31 * 24 * time.Hour)
	if err := os.Chtimes(oldPath, aged, aged); err != nil {
		t.Fatal(err)
	}

	_, dropped, err := PruneRetention(dir, now, RetentionPeriod)
	if err != nil {
		t.Fatal(err)
	}
	if dropped != 1 {
		t.Fatalf("dropped cursors = %d, want 1", dropped)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("old cursor still present: %v", err)
	}
	newPath, err := cursorFileForTest(dir, "newsub")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(newPath); err != nil {
		t.Fatalf("fresh cursor pruned: %v", err)
	}
	// A missing cursors directory is not an error.
	if _, _, err := PruneRetention(t.TempDir(), now, RetentionPeriod); err != nil {
		t.Fatalf("empty dir: %v", err)
	}
}

func cursorFileForTest(dir, sub string) (string, error) {
	return cursorFile(dir, sub)
}

func TestFetchPrunesExpiredSpoolAndCursors(t *testing.T) {
	ex := &fakeExcerpter{answers: []Excerpt{goodExcerpt}}
	te := newTestEnv(t, ex)
	te.env.Retention = 2 * time.Hour

	sinkDir := t.TempDir()
	te.mustRun("sink", "add", "rclone", "local", sinkDir)
	te.mustRun("post", writeBody(t, "fresh post"), "--headline", "H", "--dek", "D")
	te.mustRun("follow", sinkDir)

	dir := KeyDir()
	// One stale spool entry, predating the retention window.
	if _, err := Ingest(dir, "src", "posts/stale.md", spoolPostRaw(t, "stale"), te.clock.Add(-3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	// One stale cursor file.
	if err := WriteCursor(dir, "ghost", 1); err != nil {
		t.Fatal(err)
	}
	ghostPath, err := cursorFileForTest(dir, "ghost")
	if err != nil {
		t.Fatal(err)
	}
	aged := te.clock.Add(-3 * time.Hour)
	if err := os.Chtimes(ghostPath, aged, aged); err != nil {
		t.Fatal(err)
	}

	te.mustRun("fetch")
	entries, err := ReadSpool(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.ID == "stale" {
			t.Fatalf("stale entry survived fetch: %+v", entries)
		}
	}
	if _, err := os.Stat(ghostPath); !os.IsNotExist(err) {
		t.Fatalf("stale cursor survived fetch: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, spoolFile)); err != nil {
		t.Fatalf("spool file missing after prune: %v", err)
	}
}

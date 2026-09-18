// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package dag

import (
	"bytes"
	"context"
	"testing"
	"time"

	_ "github.com/qiangli/coreutils/cmds/all"
	"github.com/qiangli/coreutils/pkg/weavecli"
)

// cacheFor builds an engine over md with a real on-disk fingerprint+duration
// cache rooted in a temp dir, so Save/load round-trips are exercised.
func cacheFor(t *testing.T, dir, md string, concurrency int) (*Engine, *Cache) {
	t.Helper()
	eng := engineFor(t, dir, md)
	eng.Concurrency = concurrency
	eng.FailFast = false
	c := LoadCache(dir+"/DAG.md", t.TempDir())
	eng.Cache = c
	return eng, c
}

// A target that runs to completion is measured; the value is its wall-clock.
func TestRecordDurationOnDone(t *testing.T) {
	dir := t.TempDir()
	md := "## Tasks\n\n### slow\nGenerates: out.txt\n" +
		block("bash", "sleep 0.05; echo hi > out.txt")

	for _, jobs := range []int{1, 4} { // serial and parallel record sites
		eng, c := cacheFor(t, dir, md, jobs)
		if _, err := eng.Run(context.Background(), "slow"); err != nil {
			t.Fatalf("jobs=%d Run: %v", jobs, err)
		}
		d, ok := c.Duration("slow")
		if !ok {
			t.Fatalf("jobs=%d: completed target not measured", jobs)
		}
		if d < 40*time.Millisecond {
			t.Errorf("jobs=%d: duration %s, want >= ~50ms", jobs, d)
		}
	}
}

// A failed target is left UNMEASURED on purpose: its time is truncated at the
// failure (or pinned to its Timeout), so it is not a cost estimate. Unmeasured
// sorts first under LPT, which is what a fix campaign wants.
func TestNoDurationOnFailure(t *testing.T) {
	dir := t.TempDir()
	md := "## Tasks\n\n### boom\n" + block("bash", "exit 3")

	for _, jobs := range []int{1, 4} {
		eng, c := cacheFor(t, dir, md, jobs)
		report, err := eng.Run(context.Background(), "boom")
		if err != nil {
			t.Fatalf("jobs=%d Run: %v", jobs, err)
		}
		if !report.Failed {
			t.Fatalf("jobs=%d: expected failure", jobs)
		}
		if _, ok := c.Duration("boom"); ok {
			t.Errorf("jobs=%d: failed target must stay unmeasured", jobs)
		}
	}
}

// An up-to-date target is skipped, so its ~0s must NOT overwrite the real cost
// measured on the run that actually built it. This is the regression that would
// silently make a heavy target look cheap and get it scheduled last.
func TestUpToDateDoesNotOverwriteDuration(t *testing.T) {
	dir := t.TempDir()
	md := "## Tasks\n\n### build\nGenerates: out.txt\n" +
		block("bash", "sleep 0.05; echo hi > out.txt")

	eng, c := cacheFor(t, dir, md, 1)
	if _, err := eng.Run(context.Background(), "build"); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	first, ok := c.Duration("build")
	if !ok {
		t.Fatal("first run not measured")
	}

	// Second run over the same graph + cache: the target is up to date.
	eng2 := engineFor(t, dir, md)
	eng2.Cache = c
	report, err := eng2.Run(context.Background(), "build")
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if len(report.Results) != 1 {
		t.Fatalf("second run produced %d results, want 1", len(report.Results))
	}
	if report.Results[0].Status != StatusUpToDate {
		t.Fatalf("second run status = %v, want up-to-date", report.Results[0].Status)
	}
	if got, _ := c.Duration("build"); got != first {
		t.Errorf("up-to-date run overwrote duration: %s -> %s", first, got)
	}
}

// Durations survive Save/load, and a cache file written before durations existed
// (no "durations" key) loads as "nothing measured" rather than panicking.
func TestDurationsPersistAndTolerateOldCache(t *testing.T) {
	dir := t.TempDir()
	c := LoadCache(dir+"/DAG.md", dir)
	c.RecordDuration("a", 1500*time.Millisecond)
	c.Record("a", "fp")
	c.Save()

	reloaded := LoadCache(dir+"/DAG.md", dir)
	if d, ok := reloaded.Duration("a"); !ok || d != 1500*time.Millisecond {
		t.Errorf("Duration after reload = %s, %v; want 1.5s, true", d, ok)
	}

	// A pre-durations cache file: hashes only.
	old := LoadCache(dir+"/Other.md", dir)
	old.Record("b", "fp")
	old.Durations = nil // simulate the older on-disk shape
	old.Save()
	back := LoadCache(dir+"/Other.md", dir)
	if _, ok := back.Duration("b"); ok {
		t.Error("old cache should report b as unmeasured")
	}
	back.RecordDuration("b", time.Second) // must not panic on a nil map
}

// Zero is a legitimate measurement. Only map membership distinguishes
// "measured and instant" from "never measured" — the LPT +inf sentinel depends
// on that distinction.
func TestZeroDurationIsMeasured(t *testing.T) {
	c := &Cache{}
	c.RecordDuration("instant", 0)
	d, ok := c.Duration("instant")
	if !ok || d != 0 {
		t.Errorf("Duration = %s, %v; want 0, true", d, ok)
	}
	if _, ok := c.Duration("never"); ok {
		t.Error("unrecorded target reported as measured")
	}
	c.RecordDuration("negative", -5*time.Second)
	if d, _ := c.Duration("negative"); d != 0 {
		t.Errorf("negative duration stored as %s, want clamped to 0", d)
	}
}

// runTimings reports T, L, and which target sets the critical-path floor.
func TestRunTimingsReportsTotalAndLongest(t *testing.T) {
	d := doc(t, "## Tasks\n\n### a\n"+block("bash", "true")+
		"### b\n"+block("bash", "true")+
		"### c\n"+block("bash", "true"))

	c := &Cache{}
	c.RecordDuration("a", 2*time.Second)
	c.RecordDuration("b", 8*time.Second)
	// c never measured

	out := new(bytes.Buffer)
	if err := runTimings(out, weavecli.OutputJSON, d, c); err != nil {
		t.Fatalf("runTimings: %v", err)
	}
	got := out.String()
	for _, want := range []string{`"total_ms": 10000`, `"longest_ms": 8000`, `"longest": "b"`, `"unmeasured": 1`} {
		if !bytes.Contains([]byte(got), []byte(want)) {
			t.Errorf("timings JSON missing %s\n%s", want, got)
		}
	}
}

//go:build !windows

package jobs

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestListJobsAndRenderTable(t *testing.T) {
	dir := t.TempDir()
	r := NewJobRegistry(dir)

	pid := os.Getpid()
	if err := r.Record(pid, "sleep 10"); err != nil {
		t.Fatalf("Record: %v", err)
	}
	rec, err := r.Get(pid)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	rec.User = "tester"
	rec.StartedAt = time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	if err := writeRecord(filepath.Join(dir, strconv.Itoa(pid)+".json"), rec); err != nil {
		t.Fatalf("rewrite record: %v", err)
	}

	res, err := ListJobs(r, rec.StartedAt.Add(90*time.Second))
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(res.Jobs) != 1 {
		t.Fatalf("jobs=%d, want 1", len(res.Jobs))
	}
	if res.Jobs[0].Elapsed != "1m30s" {
		t.Fatalf("Elapsed=%q, want 1m30s", res.Jobs[0].Elapsed)
	}

	var buf bytes.Buffer
	if err := RenderList(&buf, res); err != nil {
		t.Fatalf("RenderList: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"PID", "USER", "STARTED", "ELAPSED", "CMD", "tester", "1m30s", "sleep 10"} {
		if !strings.Contains(out, want) {
			t.Fatalf("rendered table missing %q:\n%s", want, out)
		}
	}
}

func TestParseSignal(t *testing.T) {
	for _, in := range []string{"TERM", "SIGTERM", "15"} {
		if _, err := parseSignal(in); err != nil {
			t.Fatalf("parseSignal(%q): %v", in, err)
		}
	}
	if _, err := parseSignal("NOPE"); err == nil {
		t.Fatal("parseSignal(NOPE) should error")
	}
}

func TestParsePID(t *testing.T) {
	if pid, err := parsePID("123"); err != nil || pid != 123 {
		t.Fatalf("parsePID(123) = %d, %v", pid, err)
	}
	for _, in := range []string{"", "abc", "0", "-1"} {
		if _, err := parsePID(in); err == nil {
			t.Fatalf("parsePID(%q) should error", in)
		}
	}
}

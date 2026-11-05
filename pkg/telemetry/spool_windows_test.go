//go:build windows

package telemetry

// NATIVE WINDOWS CONTROLS for the P0 spool repair. These run only on a
// Windows host (the incident platform) and pin the four acceptance
// behaviours that platform-specifically broke:
//
//   1. a foreign exclusive byte lock blocking the compaction write must be
//      OBSERVABLE (notice + failed rotation counter), and the hard bound must
//      still hold — never a silent bypass;
//   2. an unremovable legacy scratch file (open without FILE_SHARE_DELETE)
//      must be reported, not absorbed;
//   3. several INDEPENDENT WRITER PROCESSES must keep the shared spool
//      bounded and valid;
//   4. sustained exports from one process must keep total directory usage
//      (spool + scratch) within the documented bound.
//
// The killed-rotator control is spool_rotation_kill_test.go and runs here
// natively too (Process.Kill is TerminateProcess).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"golang.org/x/sys/windows"
)

// A foreign exclusive lock over the data region is what a scanner, indexer or
// legacy rotator holding the file looks like to compaction. The compaction
// write must fail, the failure must be visible, and the hard bound must still
// bind. The spool is prefilled BEFORE the foreign lock is taken: the lock
// blocks compaction I/O, not the appends that grew the file.
func TestWindowsForeignLockMakesRotationFailureObservable(t *testing.T) {
	withSmallSpoolCap(t, 16<<10)
	notice := withNoticeCapture(t)
	path := filepath.Join(t.TempDir(), "spans.jsonl")
	e, err := newSpoolExporter(path)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Shutdown(context.Background())

	for _, ln := range padLines(40, 512) {
		if _, err := e.f.WriteString(ln); err != nil {
			t.Fatal(err)
		}
	}
	foreign, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer foreign.Close()
	ol := &windows.Overlapped{}
	if err := windows.LockFileEx(windows.Handle(foreign.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol); err != nil {
		t.Fatalf("foreign lock: %v", err)
	}
	defer windows.UnlockFileEx(windows.Handle(foreign.Fd()), 0, 1, 0, ol)

	e.mu.Lock()
	e.rotateIfLarge()
	e.mu.Unlock()

	e.mu.Lock()
	failures := e.rotationFailures
	e.mu.Unlock()
	if failures == 0 {
		t.Error("a blocked compaction reported no failure — the disk fills in exactly this silence")
	}
	if !strings.Contains(notice.String(), "compaction failed") {
		t.Errorf("blocked compaction not announced: %q", notice.String())
	}
	// And the hard bound still holds: exports past it are dropped loudly, and
	// no transient observation may cross the bound either.
	sawDrop := false
	var peak int64
	for i := 0; i < 12; i++ {
		if err := e.ExportSpans(context.Background(), []sdktrace.ReadOnlySpan{padSpan(fmt.Sprintf("w%d", i))}); err != nil {
			if !strings.Contains(err.Error(), "hard bound") {
				t.Fatalf("unexpected export error: %v", err)
			}
			sawDrop = true
			break
		}
		if fi, err := os.Stat(path); err == nil && fi.Size() > peak {
			peak = fi.Size()
		}
	}
	if !sawDrop {
		t.Error("exports continued past the hard bound without an observable drop")
	}
	if peak > hardSpoolBytes() {
		t.Errorf("spool transiently crossed the hard bound under a foreign lock: peak %d > %d", peak, hardSpoolBytes())
	}
	if fi, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else if fi.Size() > hardSpoolBytes() {
		t.Errorf("spool grew past the hard bound under a foreign lock: %d > %d", fi.Size(), hardSpoolBytes())
	}
}

// An open handle WITHOUT FILE_SHARE_DELETE is the Windows way a legacy
// scratch file resists cleanup; the sweep must say so instead of pretending
// the directory is clean. The scratch is aged past the live-producer guard so
// the test exercises the removal path, not the deferral.
func TestWindowsUnremovableScratchIsReported(t *testing.T) {
	path := withCanonicalSpoolDir(t)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	scratch := filepath.Join(dir, ".spans-stuck.rotating")
	if err := os.WriteFile(scratch, []byte("orphan"), 0o600); err != nil {
		t.Fatal(err)
	}
	ageFile(t, scratch, 10*time.Minute)
	held, err := windows.CreateFile(windows.StringToUTF16Ptr(scratch),
		windows.GENERIC_READ, windows.FILE_SHARE_READ, nil,
		windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Skipf("cannot hold scratch without share-delete: %v", err)
	}
	defer windows.CloseHandle(held)

	notice := withNoticeCapture(t)
	e, err := newSpoolExporter(path)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Shutdown(context.Background())
	if !strings.Contains(notice.String(), "could NOT be removed") {
		t.Errorf("an unremovable scratch file was not reported: %q", notice.String())
	}
}

const writerChildEnv = "TEST_SPOOL_WINDOWS_WRITER_CHILD=1"

// Simultaneous INDEPENDENT writer processes — the exact incident shape — must
// keep the shared spool within the hard bound and every line valid.
func TestWindowsIndependentWriterProcessesStayBounded(t *testing.T) {
	withSmallSpoolCap(t, 32<<10)
	dir := t.TempDir()
	path := filepath.Join(dir, "spans.jsonl")

	const writers = 4
	cmds := make([]*exec.Cmd, writers)
	outputs := make([]bytes.Buffer, writers)
	for i := range cmds {
		cmd := exec.Command(os.Args[0], "-test.run=^TestWindowsWriterChild$", "-test.v")
		cmd.Env = append(os.Environ(), writerChildEnv, "BASHY_OTEL_SPOOL="+path)
		cmd.Stdout, cmd.Stderr = &outputs[i], &outputs[i]
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		cmds[i] = cmd
	}
	// Transient bound, sampled from the parent while the writers run: every
	// write is admitted under the transaction lock, so no observation — at any
	// instant, by any process — may cross the hard bound.
	var peak atomic.Int64
	stop := make(chan struct{})
	var sampler sync.WaitGroup
	sampler.Add(1)
	go func() {
		defer sampler.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if fi, err := os.Stat(path); err == nil {
				for {
					old := peak.Load()
					if fi.Size() <= old || peak.CompareAndSwap(old, fi.Size()) {
						break
					}
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	childFailed := false
	for i, cmd := range cmds {
		if err := cmd.Wait(); err != nil {
			t.Errorf("writer child %d failed: %v; output:\n%s", i, err, outputs[i].String())
			childFailed = true
		}
	}
	close(stop)
	sampler.Wait()
	if childFailed {
		t.FailNow()
	}
	if got := peak.Load(); got > hardSpoolBytes() {
		t.Errorf("multi-process spool transiently crossed the hard bound: peak %d > %d", got, hardSpoolBytes())
	}

	if names := scratchFiles(t, dir); len(names) != 0 {
		t.Errorf("multi-process writers left scratch files: %v", names)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() > hardSpoolBytes() {
		t.Errorf("multi-process spool exceeded the hard bound: %d > %d", fi.Size(), hardSpoolBytes())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for i, ln := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		var v map[string]any
		if err := json.Unmarshal([]byte(ln), &v); err != nil {
			t.Fatalf("line %d invalid after multi-process writes: %v", i, err)
		}
		n++
	}
	if n == 0 {
		t.Error("no records survived multi-process writing")
	}
}

// TestWindowsWriterChild is one independent writer process of the test above.
func TestWindowsWriterChild(t *testing.T) {
	if os.Getenv("TEST_SPOOL_WINDOWS_WRITER_CHILD") != "1" {
		t.Skip("parent-spawned child only")
	}
	maxSpoolBytes = 32 << 10
	e, err := newSpoolExporter(os.Getenv("BASHY_OTEL_SPOOL"))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 250; i++ {
		if err := e.ExportSpans(context.Background(), []sdktrace.ReadOnlySpan{padSpan(fmt.Sprintf("child-%d", i))}); err != nil {
			lockTimeout := errors.Is(err, context.DeadlineExceeded) &&
				strings.Contains(err.Error(), "spool lock unavailable, dropped")
			if !strings.Contains(err.Error(), "hard bound") && !lockTimeout {
				t.Fatal(err)
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	_ = e.Shutdown(context.Background())
}

// Sustained exports from one process: total directory usage (spool plus any
// scratch) must stay within the documented bound for the whole run.
func TestWindowsSustainedExportsBoundTotalDirectoryUsage(t *testing.T) {
	withSmallSpoolCap(t, 16<<10)
	dir := t.TempDir()
	path := filepath.Join(dir, "spans.jsonl")
	e, err := newSpoolExporter(path)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Shutdown(context.Background())

	drops := 0
	peak := int64(0)
	for i := 0; i < 4000; i++ {
		if err := e.ExportSpans(context.Background(), []sdktrace.ReadOnlySpan{padSpan(fmt.Sprintf("sustained-%d", i))}); err != nil {
			if !strings.Contains(err.Error(), "hard bound") {
				t.Fatalf("unexpected export error at %d: %v", i, err)
			}
			drops++
		}
		if i%200 == 0 {
			// Strict transient bound: admission under the transaction lock
			// keeps every observation at or under the hard bound — no slack.
			total := dirUsage(t, dir)
			if total > peak {
				peak = total
			}
			if total > hardSpoolBytes() {
				t.Fatalf("directory usage %d exceeded the documented bound at iteration %d", total, i)
			}
		}
	}
	if peak > hardSpoolBytes() {
		t.Errorf("peak directory usage %d exceeded the bound", peak)
	}
	if drops > 0 {
		t.Logf("note: %d hard-bound drops occurred (bounded and observable, as designed)", drops)
	}
}

func dirUsage(t *testing.T, dir string) int64 {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, ent := range entries {
		fi, err := ent.Info()
		if err == nil {
			total += fi.Size()
		}
	}
	return total
}

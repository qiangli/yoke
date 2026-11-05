package telemetry

// P0 emergency regression controls for bounded spool compaction.
//
// The incident being controlled: abandoned .spans-*.rotating scratch consumed
// ~1.324 TB on a Windows host while the spool itself reached 584 MB against a
// documented 64 MiB cap. Every control here pins one property the old
// rename-based rotation could not guarantee — no scratch file, no rename, the
// same inode before and after, an observable hard bound, healing after a kill.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// withSmallSpoolCap points the package bounds at a test-friendly size and
// restores them after.
func withSmallSpoolCap(t *testing.T, cap int64) {
	t.Helper()
	old := maxSpoolBytes
	maxSpoolBytes = cap
	t.Cleanup(func() { maxSpoolBytes = old })
}

// withNoticeCapture swaps the notice sink so tests assert observability
// directly instead of scraping stderr.
func withNoticeCapture(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	old, oldInterval := spoolNoticeSink, spoolNoticeInterval
	spoolNoticeSink, spoolNoticeInterval = buf, 0
	t.Cleanup(func() { spoolNoticeSink, spoolNoticeInterval = old, oldInterval })
	return buf
}

// padLines builds n distinct valid jsonline records of roughly size bytes.
func padLines(n, size int) []string {
	filler := strings.Repeat("x", size)
	lines := make([]string, n)
	for i := range lines {
		line, _ := json.Marshal(map[string]any{"_msg": "pad", "seq": i, "filler": filler})
		lines[i] = string(line) + "\n"
	}
	return lines
}

func scratchFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, ent := range entries {
		if strings.HasPrefix(ent.Name(), ".spans-") && strings.HasSuffix(ent.Name(), ".rotating") {
			out = append(out, ent.Name())
		}
	}
	return out
}

// Compaction must happen on the SAME inode with no scratch file and no
// rename: the whole terabyte incident was rename + scratch under concurrent
// Windows writers.
func TestRotateIsInPlaceSameInodeNoScratch(t *testing.T) {
	withSmallSpoolCap(t, 32<<10)
	dir := t.TempDir()
	path := filepath.Join(dir, "spans.jsonl")
	e, err := newSpoolExporter(path)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Shutdown(context.Background())

	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, ln := range padLines(200, 512) {
		if _, err := e.f.WriteString(ln); err != nil {
			t.Fatal(err)
		}
	}
	e.mu.Lock()
	e.rotateIfLarge()
	e.mu.Unlock()

	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("rotation replaced the spool inode: concurrent writers' handles are stranded and Windows renames are the incident")
	}
	if got := after.Size(); got >= maxSpoolBytes {
		t.Errorf("spool not compacted: size=%d, soft bound=%d", got, maxSpoolBytes)
	}
	if names := scratchFiles(t, dir); len(names) != 0 {
		t.Errorf("rotation left scratch files behind: %v", names)
	}
}

// The NEWEST half survives, the oldest half goes, and every surviving line
// stays valid jsonline — the import contract.
func TestRotationKeepsNewestValidRecords(t *testing.T) {
	withSmallSpoolCap(t, 32<<10)
	path := filepath.Join(t.TempDir(), "spans.jsonl")
	e, err := newSpoolExporter(path)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Shutdown(context.Background())

	lines := padLines(160, 512)
	for _, ln := range lines {
		if _, err := e.f.WriteString(ln); err != nil {
			t.Fatal(err)
		}
	}
	e.mu.Lock()
	e.rotateIfLarge()
	e.mu.Unlock()

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body := string(b)
	if strings.Contains(body, `"seq":0,`) {
		t.Error("oldest records survived compaction; the bound does not bind")
	}
	// The newest dozen must all still be present, verbatim.
	for i := len(lines) - 12; i < len(lines); i++ {
		if !strings.Contains(body, strings.TrimSuffix(lines[i], "\n")) {
			t.Errorf("newest record %d lost by compaction", i)
		}
	}
	for i, ln := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
		var v map[string]any
		if err := json.Unmarshal([]byte(ln), &v); err != nil {
			t.Fatalf("line %d invalid after rotation: %v", i, err)
		}
	}
}

// Past the HARD bound with compaction failing, exports must be dropped
// OBSERVABLY: an error to the SDK, a notice, and a size that never crosses the
// bound. This is the control for "cannot silently bypass the bound".
func TestHardBoundDropIsObservableAndBounded(t *testing.T) {
	if runtime.GOOS != "windows" && os.Geteuid() == 0 {
		t.Skip("root ignores file permission bits; the failure injection would not hold")
	}
	withSmallSpoolCap(t, 16<<10)
	notice := withNoticeCapture(t)
	path := filepath.Join(t.TempDir(), "spans.jsonl")
	e, err := newSpoolExporter(path)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Shutdown(context.Background())

	// Pad just past the SOFT bound through the exporter itself, then inject a
	// compaction failure: a file the process cannot open read/write (Windows
	// readonly attribute / unix mode alike). Appends through the already-open
	// handle keep working, which is precisely the trap: writes succeed while
	// the bound cannot re-establish itself.
	for i := 0; i < 24; i++ {
		if err := e.ExportSpans(context.Background(), []sdktrace.ReadOnlySpan{padSpan("grow" + strconv.Itoa(i))}); err != nil {
			t.Fatalf("growth phase export %d: %v", i, err)
		}
	}
	if err := os.Chmod(path, 0o400); err != nil {
		t.Skipf("cannot restrict %s: %v", path, err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
	if f, err := os.OpenFile(path, os.O_RDWR, 0); err == nil {
		_ = f.Close()
		t.Skip("restriction did not take effect on this platform/user")
	}

	dropErr := error(nil)
	for i := 0; i < 40; i++ {
		err := e.ExportSpans(context.Background(), []sdktrace.ReadOnlySpan{padSpan("dropme" + strconv.Itoa(i))})
		if err != nil {
			dropErr = err
			break
		}
	}
	if dropErr == nil || !strings.Contains(dropErr.Error(), "hard bound") {
		t.Fatalf("export past the hard bound must fail loudly, got %v", dropErr)
	}
	if !strings.Contains(notice.String(), "hard bound") {
		t.Errorf("no storage-pressure notice; stderr was the only witness and it said nothing: %q", notice.String())
	}
	if !strings.Contains(notice.String(), "compaction failed") {
		t.Errorf("the failing compaction itself was never reported: %q", notice.String())
	}
	if fi, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else if fi.Size() > hardSpoolBytes() {
		t.Errorf("spool grew past the hard bound: %d > %d", fi.Size(), hardSpoolBytes())
	}
}

// Once exports resume after drops, the FIRST batch must carry a
// telemetry.spool.dropped summary line: the gap has to be provable from the
// data that survived it.
func TestDropSummaryRecordedOnResume(t *testing.T) {
	if runtime.GOOS != "windows" && os.Geteuid() == 0 {
		t.Skip("root ignores file permission bits; the failure injection would not hold")
	}
	withSmallSpoolCap(t, 16<<10)
	withNoticeCapture(t)
	path := filepath.Join(t.TempDir(), "spans.jsonl")
	e, err := newSpoolExporter(path)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Shutdown(context.Background())

	for i := 0; i < 24; i++ {
		if err := e.ExportSpans(context.Background(), []sdktrace.ReadOnlySpan{padSpan("grow" + strconv.Itoa(i))}); err != nil {
			t.Fatalf("growth phase export %d: %v", i, err)
		}
	}
	if err := os.Chmod(path, 0o400); err != nil {
		t.Skipf("cannot restrict %s: %v", path, err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
	dropped := false
	for i := 0; i < 40; i++ {
		if err := e.ExportSpans(context.Background(), []sdktrace.ReadOnlySpan{padSpan("dropme" + strconv.Itoa(i))}); err != nil {
			if !strings.Contains(err.Error(), "hard bound") {
				t.Fatalf("unexpected drop error: %v", err)
			}
			dropped = true
			break
		}
	}
	if !dropped {
		t.Fatal("expected an observable drop before the resume phase")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Skipf("cannot re-enable %s: %v", path, err)
	}
	if err := e.ExportSpans(context.Background(), []sdktrace.ReadOnlySpan{padSpan("after")}); err != nil {
		t.Fatalf("resume export: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body := string(b)
	if !strings.Contains(body, "telemetry.spool.dropped") {
		t.Error("no drop summary record after exports resumed; the truncation is invisible in the data")
	}
	if !strings.Contains(body, `"_msg":"after"`) {
		t.Error("post-resume record missing")
	}
	e.mu.Lock()
	dropCount := e.droppedSpans
	e.mu.Unlock()
	if dropCount != 0 {
		t.Errorf("drop counters not reset on resume: %d", dropCount)
	}
}

// withCanonicalSpoolDir points the Bashy-owned canonical spool directory at a
// temp dir (via the home lookup defaultSpoolPath uses) and returns the
// canonical spool path. The sweep only acts on this directory.
func withCanonicalSpoolDir(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir on Windows
	t.Setenv("BASHY_OTEL_SPOOL", "")
	return SpoolPath()
}

func ageFile(t *testing.T, path string, d time.Duration) {
	t.Helper()
	ago := time.Now().Add(-d)
	if err := os.Chtimes(path, ago, ago); err != nil {
		t.Fatal(err)
	}
}

// Legacy scratch debris from the old implementation (or a not-yet-upgraded
// producer) is swept at open — but ONLY in the Bashy-owned canonical spool
// directory, only for the exact legacy pattern, and only when provably debris
// (regular file, aged, size-stable). Source records, receipts, user files, a
// live producer's current scratch, and everything outside the canonical dir
// are untouched.
func TestLegacyScratchSweptAtOpen(t *testing.T) {
	path := withCanonicalSpoolDir(t)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"_msg":"keep"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".spans-123.rotating", ".spans-abc.rotating"} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("orphan"), 0o600); err != nil {
			t.Fatal(err)
		}
		ageFile(t, p, 10*time.Minute)
	}
	// A FRESH scratch file may belong to a live, not-yet-upgraded producer:
	// deferred, never removed.
	fresh := filepath.Join(dir, ".spans-live.rotating")
	if err := os.WriteFile(fresh, []byte("orphan"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".spans-keep.tmp"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "unrelated.txt"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	notice := withNoticeCapture(t)
	e, err := newSpoolExporter(path)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Shutdown(context.Background())

	if names := scratchFiles(t, dir); len(names) != 1 || names[0] != ".spans-live.rotating" {
		t.Errorf("legacy scratch not swept (want only the deferred fresh file left): %v", names)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("sweep removed a possibly-live scratch file: %v", err)
	}
	for _, keep := range []string{".spans-keep.tmp", "unrelated.txt"} {
		if _, err := os.Stat(filepath.Join(dir, keep)); err != nil {
			t.Errorf("sweep touched %s: %v (recovery must preserve foreign files)", keep, err)
		}
	}
	if b, _ := os.ReadFile(path); !strings.Contains(string(b), "keep") {
		t.Error("sweep modified the spool itself")
	}
	if !strings.Contains(notice.String(), "swept 2") {
		t.Errorf("sweep not observable: %q", notice.String())
	}
	if !strings.Contains(notice.String(), "left alone") {
		t.Errorf("deferred live scratch not reported: %q", notice.String())
	}
}

// An operator override may name a shared or foreign directory: the sweep must
// not delete anything there, whatever pattern it matches. Cleanup of
// overrides is the operator's job, documented in the rollout plan.
func TestLegacyScratchIgnoredOutsideCanonicalDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "spans.jsonl")
	if err := os.WriteFile(path, []byte(`{"_msg":"keep"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	scratch := filepath.Join(dir, ".spans-old.rotating")
	if err := os.WriteFile(scratch, []byte("orphan"), 0o600); err != nil {
		t.Fatal(err)
	}
	ageFile(t, scratch, 10*time.Minute)

	notice := withNoticeCapture(t)
	e, err := newSpoolExporter(path)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Shutdown(context.Background())

	if _, err := os.Stat(scratch); err != nil {
		t.Errorf("sweep removed a file outside the canonical dir: %v", err)
	}
	if got := notice.String(); strings.Contains(got, "swept") {
		t.Errorf("sweep acted outside the canonical dir: %q", got)
	}
}

// A kill mid-compaction (simulated by the partially copied state it leaves)
// must leave a BOUNDED file whose newest records are intact, and the next
// compaction must heal it. See spool_rotation_kill_test.go for the real
// killed-child-process control.
func TestCompactionHealsPartialCopy(t *testing.T) {
	withSmallSpoolCap(t, 32<<10)
	path := filepath.Join(t.TempDir(), "spans.jsonl")
	e, err := newSpoolExporter(path)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Shutdown(context.Background())

	lines := padLines(200, 512)
	var all strings.Builder
	for _, ln := range lines {
		all.WriteString(ln)
	}
	if err := os.WriteFile(path, []byte(all.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	// Reproduce "killed while copying": a partial copy of the tail has landed
	// at offset 0, the truncate never ran.
	body := all.String()
	tail := body[len(body)/2:]
	mangled := tail[:1024] + body
	if err := os.WriteFile(path, []byte(mangled), 0o600); err != nil {
		t.Fatal(err)
	}

	e.mu.Lock()
	e.rotateIfLarge()
	e.mu.Unlock()

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(b)) >= maxSpoolBytes {
		t.Errorf("healing compaction did not bound the file: %d", len(b))
	}
	invalid := 0
	for _, ln := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		var v map[string]any
		if json.Unmarshal([]byte(ln), &v) != nil {
			invalid++
		}
	}
	if invalid != 0 {
		t.Errorf("%d invalid lines after healing; recovery must converge to a fully valid file", invalid)
	}
	for i := len(lines) - 10; i < len(lines); i++ {
		if !strings.Contains(string(b), strings.TrimSuffix(lines[i], "\n")) {
			t.Errorf("newest record %d lost by healing compaction", i)
		}
	}
}

// A torn line INSIDE the kept half — the shape a kill mid-copy leaves when
// the junction lands past the next cut — must be dropped by the copy filter,
// not carried forward forever. Recovery converges to a fully valid file.
func TestCompactionDropsTornLineInKeptHalf(t *testing.T) {
	withSmallSpoolCap(t, 32<<10)
	path := filepath.Join(t.TempDir(), "spans.jsonl")
	e, err := newSpoolExporter(path)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Shutdown(context.Background())

	lines := padLines(200, 512)
	var all strings.Builder
	for _, ln := range lines {
		all.WriteString(ln)
	}
	body := all.String()
	// Replace one whole line in the kept region (past the halfway cut) with
	// garbage: exactly one torn line among valid records, deterministic.
	off := len(body) * 3 / 4
	start := strings.LastIndex(body[:off], "\n") + 1
	rel := strings.Index(body[start:], "\n")
	if rel < 0 {
		t.Fatal("test fixture has no newline past the corruption point")
	}
	mangled := body[:start] + "###TORN###" + body[start+rel:]
	if err := os.WriteFile(path, []byte(mangled), 0o600); err != nil {
		t.Fatal(err)
	}

	e.mu.Lock()
	e.rotateIfLarge()
	e.mu.Unlock()

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(b)) >= maxSpoolBytes {
		t.Errorf("healing compaction did not bound the file: %d", len(b))
	}
	invalid := 0
	for _, ln := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		var v map[string]any
		if json.Unmarshal([]byte(ln), &v) != nil {
			invalid++
		}
	}
	if invalid != 0 {
		t.Errorf("%d invalid lines after healing; the torn kept-half line must be dropped, not preserved", invalid)
	}
	if strings.Contains(string(b), "#") {
		t.Error("corruption bytes survived the healing compaction")
	}
	for i := len(lines) - 10; i < len(lines); i++ {
		if !strings.Contains(string(b), strings.TrimSuffix(lines[i], "\n")) {
			t.Errorf("newest record %d lost by healing compaction", i)
		}
	}
}

// Several exporters sharing one file — the concurrent-writer shape — must
// keep the TOTAL bounded and leave the file valid, with rotation serialized
// by the cross-process lock rather than racing.
func TestConcurrentExportersStayBounded(t *testing.T) {
	withSmallSpoolCap(t, 32<<10)
	dir := t.TempDir()
	path := filepath.Join(dir, "spans.jsonl")
	const writers, batches = 4, 60

	var wg sync.WaitGroup
	errs := make(chan error, writers)
	drops := 0
	var dropMu sync.Mutex
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			e, err := newSpoolExporter(path)
			if err != nil {
				errs <- err
				return
			}
			defer e.Shutdown(context.Background())
			for i := 0; i < batches; i++ {
				if err := e.ExportSpans(context.Background(), []sdktrace.ReadOnlySpan{padSpan("w" + strconv.Itoa(w) + "-" + strconv.Itoa(i))}); err != nil {
					// Both hard-cap pressure and the bounded transaction-lock
					// wait deliberately drop observably instead of queuing.
					// Never accept access errors or other storage failures.
					lockTimeout := errors.Is(err, context.DeadlineExceeded) &&
						strings.Contains(err.Error(), "spool lock unavailable, dropped")
					if !strings.Contains(err.Error(), "hard bound") && !lockTimeout {
						errs <- err
						return
					}
					dropMu.Lock()
					drops++
					dropMu.Unlock()
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent writer failed: %v", err)
		}
	}
	if drops > 0 {
		t.Logf("note: %d bounded observable drops under %d-way contention (bounded and observable, as designed)", drops, writers)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Strict: every write is admitted under the transaction lock, so no
	// observation may ever exceed the hard bound — no slack.
	if fi.Size() > hardSpoolBytes() {
		t.Errorf("shared spool exceeded the hard bound: %d > %d", fi.Size(), hardSpoolBytes())
	}
	if names := scratchFiles(t, dir); len(names) != 0 {
		t.Errorf("concurrent compaction left scratch files: %v", names)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for i, ln := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		var v map[string]any
		if err := json.Unmarshal([]byte(ln), &v); err != nil {
			t.Fatalf("line %d invalid after concurrent compaction: %v", i, err)
		}
		n++
	}
	if n == 0 {
		t.Error("no records survived concurrent compaction")
	}
}

// When another process holds the compaction lock, rotation must give up
// within the bounded wait and say so — not hang, not silently skip forever.
func TestLockContentionGivesUpObservably(t *testing.T) {
	withSmallSpoolCap(t, 16<<10)
	notice := withNoticeCapture(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "spans.jsonl")
	e, err := newSpoolExporter(path)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Shutdown(context.Background())
	for _, ln := range padLines(60, 512) {
		if _, err := e.f.WriteString(ln); err != nil {
			t.Fatal(err)
		}
	}

	// A foreign lock holder (another live compactor).
	holder, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	release, err := lockSpool(context.Background(), holder)
	if err != nil {
		t.Skipf("cross-process lock unavailable here: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		e.mu.Lock()
		e.rotateIfLarge()
		e.mu.Unlock()
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("rotation blocked indefinitely on lock contention")
	}
	release()

	e.mu.Lock()
	failures := e.rotationFailures
	e.mu.Unlock()
	if failures == 0 {
		t.Error("contended rotation did not record a failure — the pressure is invisible")
	}
	if !strings.Contains(notice.String(), "spool lock") {
		t.Errorf("contended rotation not reported: %q", notice.String())
	}
}

// padSpan builds a minimal ReadOnlySpan for ExportSpans tests.
func padSpan(name string) sdktrace.ReadOnlySpan {
	return tracetest.SpanStub{
		Name:        name,
		SpanContext: newTestSpanContext(),
		Resource:    resource.Empty(),
		StartTime:   time.Now().Add(-time.Second),
		EndTime:     time.Now(),
		Attributes:  []attribute.KeyValue{attribute.String("pad", strings.Repeat("p", 768))},
	}.Snapshot()
}

func newTestSpanContext() trace.SpanContext {
	return trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{0x01},
		SpanID:     trace.SpanID{0x02},
		TraceFlags: trace.FlagsSampled,
	})
}

// Bounded lock waits must leave a durable gap marker when writing resumes,
// not merely a transient error/notice that disappears from future context.
func TestLockTimeoutDropSummaryRecordedOnResume(t *testing.T) {
	withSmallSpoolCap(t, 16<<10)
	notice := withNoticeCapture(t)
	path := filepath.Join(t.TempDir(), "spans.jsonl")
	e, err := newSpoolExporter(path)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Shutdown(context.Background())
	holder, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	release, err := lockSpool(context.Background(), holder)
	if err != nil {
		t.Fatal(err)
	}
	released := false
	defer func() {
		if !released {
			_ = release()
		}
	}()
	err = e.ExportSpans(context.Background(), []sdktrace.ReadOnlySpan{padSpan("lost-one"), padSpan("lost-two")})
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "spool lock unavailable, dropped 2 spans") {
		t.Fatalf("want explicit bounded lock drop, got %v", err)
	}
	if !strings.Contains(notice.String(), "export dropped 2 spans") {
		t.Fatalf("drop notice missing: %s", notice.String())
	}
	if fi, err := os.Stat(path); err != nil || fi.Size() != 0 {
		t.Fatalf("failed transaction wrote bytes: info=%v error=%v", fi, err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	released = true
	if err := e.ExportSpans(context.Background(), []sdktrace.ReadOnlySpan{padSpan("after-lock")}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 {
		t.Fatalf("want gap marker and successful export, got %d lines: %s", len(lines), b)
	}
	var summary map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &summary); err != nil {
		t.Fatal(err)
	}
	if summary["_msg"] != "telemetry.spool.dropped" || summary["dropped_batches"] != float64(1) || summary["dropped_spans"] != float64(2) {
		t.Fatalf("wrong durable loss accounting: %v", summary)
	}
	if !strings.Contains(lines[1], `"_msg":"after-lock"`) {
		t.Fatalf("successful export missing: %s", b)
	}
	if e.droppedBatches != 0 || e.droppedSpans != 0 {
		t.Fatal("persisted gap accounting was not consumed")
	}
}

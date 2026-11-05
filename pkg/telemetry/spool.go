package telemetry

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// The spool sink writes spans to a FILE instead of a collector, so telemetry
// costs nothing to have on and needs nothing pre-started.
//
// The problem it solves: an OTLP endpoint means a service must already be
// running at the moment a span is produced. Anything that runs before it, or
// while it is down, or on a host that never started it, exports into a closed
// port and is lost — silently, because a dropped span looks exactly like a
// process that had nothing to say. That failure was found in the field: a stack
// ran for eight days with zero data because nothing was configured to send to
// it, and nobody could tell from the outside.
//
// Records are VictoriaLogs jsonline, which is Victoria's own native ingest
// format — NOT OTLP JSON. That choice is what keeps this cheap: a spool file
// imports with
//
//	curl --data-binary @spool.jsonl '<vlogs>/insert/jsonline?_stream_fields=service'
//
// and is then queryable with real LogsQL. Writing OTLP JSON instead would have
// required a translation step or a query engine of our own. Verified: importing
// a spool into a fresh store and filtering on a structured field (exit_code:2)
// returns the exact record, so the query verbs keep working over spooled data.
//
// Selected by the standard OTEL_TRACES_EXPORTER env var ("file"), per this
// package's rule of standard OTEL vars and nothing bespoke.
//
// # Bounds, and why rotation no longer renames anything
//
// The spool is bounded TWICE. The soft bound (maxSpoolBytes, 64 MiB) triggers
// compaction to the newest half of the file. The hard bound (twice the soft
// bound) drops exports OBSERVABLY — an error back to the SDK, a rate-limited
// stderr notice, and a summary record counted into the spool once exports
// resume — so a spool whose compaction keeps failing can grow past its soft
// bound but never without limit and never silently.
//
// Compaction is IN PLACE on the same inode: the newest line-aligned half is
// copied to the front of the very file everyone appends to, then the file is
// truncated. The previous implementation wrote the survivor to a
// .spans-*.rotating scratch file and renamed it over the spool, and that had
// two unbounded failure modes that field evidence made undeniable: on Windows
// the rename fails outright while any other process holds the spool open, the
// cleanup remove of the scratch can fail too, and every subsequent export
// retried the whole dance leaving a fresh multi-megabyte orphan each time
// (~1.3 TB of abandoned scratch on one host, with the spool itself at 584 MB
// against a documented 64 MiB cap); and a process killed between scratch
// creation and rename abandoned its scratch forever. execlog's package doc
// recorded the rename hazards first — "on Windows the rename fails outright,
// the cap never binds, and nothing reports either" — and this package now
// follows the same never-rename rule. Consequences, all deliberate:
//
//   - no scratch file exists to orphan, so total storage is the spool plus at
//     most one in-flight batch, whatever happens, whoever is killed;
//   - appends through a handle opened before compaction keep landing at the
//     new end of the SAME file (O_APPEND/FILE_APPEND_DATA), so no writer's
//     handle is ever stranded on a replaced inode;
//   - admission, write and compaction share one cross-process transaction
//     lock, so cooperating producers cannot blow the cap together and no
//     append can land between another process's copy and its truncate. A
//     producer that predates this lock (mid-rollout) can still lose a batch
//     to a concurrent truncate — bounded, and resolved by finishing the
//     rollout, which is why old producers are stopped before new ones start;
//   - a process killed MID-COPY leaves a bounded file whose newest records
//     are still intact at the tail (the copy only ever writes below its read
//     offset); the next compaction re-derives its cut from a line boundary
//     and its copy FILTER drops the torn lines — counted and reported — so
//     recovery converges to a fully valid file;
//   - legacy .spans-*.rotating orphans from the old implementation are swept
//     when a new exporter opens the Bashy-owned canonical spool dir (regular
//     files only, aged and size-stable, failures reported), so the rollout of
//     this fix also reclaims the existing debris without touching anything
//     that might still belong to a live producer.

// spoolExporter appends one jsonline record per span.
//
// It is deliberately a plain appender: no batching beyond the SDK's, no index,
// no rotation-by-write. Concurrency safety comes from O_APPEND plus a
// goroutine mutex plus one cross-process transaction lock on the spool file
// itself (spool_lock*.go): admission, write and compaction all happen under
// it, so no two cooperating processes can jointly pass the cap or interleave
// an append with a compaction. Non-cooperating writers (a pre-lock producer
// mid-rollout) still land atomically at the end via O_APPEND; they are bounded
// by the rollout, not by this lock.
type spoolExporter struct {
	mu   sync.Mutex
	path string
	f    *os.File

	// Storage-pressure accounting, guarded by mu. A drop or a failed
	// rotation that nobody can see is the exact disease this package exists
	// to eliminate — these counters plus the rate-limited notices are how
	// the bound stays observable.
	droppedBatches   int64
	droppedSpans     int64
	rotationFailures int64
	lastNotice       time.Time
}

func newSpoolExporter(path string) (*spoolExporter, error) {
	// Bashy is a shell, so its working directory changes routinely. Anchor an
	// operator's relative override once; rotation must never reinterpret it
	// against a later directory and touch an unrelated file.
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve spool path: %w", err)
	}
	path = absPath
	if err := secureSpoolDir(path); err != nil {
		return nil, fmt.Errorf("spool dir: %w", err)
	}
	f, err := openPrivateSpool(path)
	if err != nil {
		return nil, fmt.Errorf("open spool: %w", err)
	}
	sweepLegacyScratch(path)
	return &spoolExporter{path: path, f: f}, nil
}

// legacyScratchMinAge is how old a legacy scratch candidate must be before
// the sweep touches it: a file modified recently may belong to a live,
// not-yet-upgraded producer that is still appending to it. A variable only so
// tests can exercise the guard without waiting.
var legacyScratchMinAge = 5 * time.Minute

// sweepLegacyScratch removes scratch files abandoned by the pre-in-place
// rotation implementation. It runs ONLY for the Bashy-owned canonical spool
// directory: an operator override may name a shared or foreign directory, and
// deleting files there is outside the authority granted by naming it. Within
// the canonical directory only the exact legacy pattern is a candidate, and a
// candidate is removed only when it is provably debris — a regular file (never
// a symlink or directory), older than legacyScratchMinAge (never a live
// producer's current scratch), and size-stable across a re-stat (never growing
// under an active writer). Everything else is left alone and reported, so
// source records, test receipts and user data survive recovery. Removals,
// deferrals and failures are all reported: a delete blocked by a Windows share
// violation must be visible, not absorbed.
func sweepLegacyScratch(spoolPath string) {
	if !isCanonicalSpoolPath(spoolPath) {
		return
	}
	dir := filepath.Dir(spoolPath)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type candidate struct {
		path string
		size int64
	}
	var vetted []candidate
	removed, failed, deferred := 0, 0, 0
	for _, ent := range entries {
		name := ent.Name()
		if ent.IsDir() ||
			!strings.HasPrefix(name, ".spans-") ||
			!strings.HasSuffix(name, ".rotating") {
			continue
		}
		p := filepath.Join(dir, name)
		lfi, err := os.Lstat(p)
		if err != nil || !lfi.Mode().IsRegular() {
			deferred++
			continue
		}
		if time.Since(lfi.ModTime()) < legacyScratchMinAge {
			deferred++
			continue
		}
		vetted = append(vetted, candidate{path: p, size: lfi.Size()})
	}
	// One batched stability wait for all candidates instead of one sleep
	// each: the incident host may hold thousands of orphans, and first-open
	// must not stall minutes per file. A file that grew during the wait is
	// a live producer's scratch and is left alone.
	if len(vetted) > 0 {
		time.Sleep(200 * time.Millisecond)
	}
	for _, c := range vetted {
		sfi, err := os.Stat(c.path)
		if err != nil || !sfi.Mode().IsRegular() || sfi.Size() != c.size {
			deferred++
			continue
		}
		if err := os.Remove(c.path); err != nil {
			failed++
		} else {
			removed++
		}
	}
	if removed == 0 && failed == 0 && deferred == 0 {
		return
	}
	if failed > 0 {
		fmt.Fprintf(spoolNoticeSink, "bashy: telemetry spool: swept %d abandoned rotation scratch file(s) in %s; %d could NOT be removed (locked?) and %d deferred as possibly live — total storage is not bounded until they are gone\n", removed, dir, failed, deferred)
		return
	}
	if deferred > 0 {
		fmt.Fprintf(spoolNoticeSink, "bashy: telemetry spool: swept %d abandoned rotation scratch file(s) in %s; %d recent or unstable file(s) left alone (possibly a live producer)\n", removed, dir, deferred)
		return
	}
	fmt.Fprintf(spoolNoticeSink, "bashy: telemetry spool: swept %d abandoned rotation scratch file(s) in %s\n", removed, dir)
}

// secureSpoolDir makes a directory created for the spool owner-only. It also
// hardens Bashy's canonical .../spool/spans.jsonl directory from older
// versions. An arbitrary operator override may name an existing project or a
// shared temporary directory; changing that directory's mode would be a much
// larger privilege boundary than protecting the spool file itself.
func secureSpoolDir(path string) error {
	dir := filepath.Dir(path)
	lfi, err := os.Lstat(dir)
	switch {
	case os.IsNotExist(err):
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		fi, err := os.Stat(dir)
		if err != nil {
			return err
		}
		if !fi.IsDir() {
			return fmt.Errorf("not a directory")
		}
		return os.Chmod(dir, 0o700)
	case err != nil:
		return err
	}
	fi, err := os.Stat(dir) // a symlink to a directory is a valid relocation
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("not a directory")
	}

	// Only the exact default home path is Bashy-owned. Basenames are not proof:
	// /var/spool and an operator's group-shared or symlinked spool are not ours
	// to chmod.
	if lfi.Mode()&os.ModeSymlink == 0 && isCanonicalSpoolPath(path) {
		return os.Chmod(dir, 0o700)
	}
	return nil
}

// openPrivateSpool opens a regular spool file with owner-only permissions.
// Chmod is necessary for an existing file, since OpenFile's create mode is not
// applied when the file is already present.
func openPrivateSpool(path string) (*os.File, error) {
	existed := false
	if fi, err := os.Lstat(path); err == nil {
		existed = true
		if fi.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("not a regular file")
		}
		if !fi.Mode().IsRegular() {
			if isNullDevice(path, fi) {
				return os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
			}
			return nil, fmt.Errorf("not a regular file")
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	f, err := os.OpenFile(path, spoolAppendFlags, 0o600)
	if err != nil {
		return nil, err
	}
	fi, statErr := f.Stat()
	if statErr != nil || !fi.Mode().IsRegular() {
		_ = f.Close()
		if statErr != nil {
			return nil, statErr
		}
		return nil, fmt.Errorf("not a regular file")
	}
	// A file we created and Bashy's canonical file must be owner-only. An
	// existing operator override may deliberately be a shared append target;
	// changing its mode is outside the authority granted by naming it.
	if !existed || isDefaultSpoolPath(path) {
		if err := f.Chmod(0o600); err != nil {
			_ = f.Close()
			return nil, err
		}
	}
	return f, nil
}

// isNullDevice preserves the historical explicit discard override without
// treating arbitrary devices as spool files. OTEL_TRACES_EXPORTER=none is the
// preferred opt-out, but existing profiles may still name /dev/null.
func isNullDevice(path string, fi os.FileInfo) bool {
	nullInfo, err := os.Stat(os.DevNull)
	return err == nil && os.SameFile(fi, nullInfo)
}

// ExportSpans writes each span as one jsonline record.
//
// Span attributes are flattened to top-level fields rather than nested under an
// "attributes" object, because LogsQL filters on top-level field names: a
// nested shape would make `exit_code:2` unexpressible and reduce the verbs to
// substring matching on the message.
//
// Storage-pressure contract: past the HARD bound (twice maxSpoolBytes) records
// are dropped rather than written, and the drop is observable three ways — an
// error returned to the SDK, a rate-limited stderr notice, and a
// telemetry.spool.dropped summary record written into the spool as soon as
// exports resume, so the gap is visible in the very data that was truncated.
func (e *spoolExporter) ExportSpans(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
	if len(spans) == 0 {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	var b strings.Builder
	for _, s := range spans {
		rec := map[string]any{
			// _time/_msg are VictoriaLogs' conventional field names; the import
			// call names them explicitly so this stays readable as plain JSON.
			"_time":       s.EndTime().UTC().Format(time.RFC3339Nano),
			"_msg":        s.Name(),
			"trace_id":    s.SpanContext().TraceID().String(),
			"span_id":     s.SpanContext().SpanID().String(),
			"duration_ms": s.EndTime().Sub(s.StartTime()).Milliseconds(),
			"status":      s.Status().Code.String(),
		}
		if s.Parent().IsValid() {
			rec["parent_span_id"] = s.Parent().SpanID().String()
		}
		if d := s.Status().Description; d != "" {
			rec["status_message"] = d
		}
		for _, kv := range s.Resource().Attributes() {
			rec[sanitizeField(string(kv.Key))] = kv.Value.Emit()
		}
		// Span attributes last so they win over resource attributes on a clash —
		// the span is the more specific statement about that event.
		for _, kv := range s.Attributes() {
			rec[sanitizeField(string(kv.Key))] = kv.Value.Emit()
		}
		line, err := json.Marshal(rec)
		if err != nil {
			continue // one unmarshalable span must not drop the batch
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	if e.f == nil {
		return nil // compaction never closes e.f any more; kept as a guard
	}
	payload := b.String()
	resuming := e.droppedSpans > 0
	if resuming {
		// The resume summary is part of THIS batch: the admission check below
		// must see the full bytes about to be written, or the summary itself
		// could push a hard-bound export over the cap it reports.
		payload = e.dropSummaryLine() + payload
	}
	// The whole admission → write → compaction sequence holds the
	// cross-process spool lock. Without it, two processes can both pass the
	// size check and both write (blowing the cap), or an append can land
	// between another process's compaction copy and its truncate and be lost.
	// A stat that cannot be taken fails CLOSED: writing blind past the bound
	// is exactly the failure this file exists to prevent.
	release, err := lockSpool(context.Background(), e.f)
	if err != nil {
		e.rotationFailures++
		e.noticef("export dropped %d spans: spool lock unavailable (%v) — contention or a wedged compactor; the batch is dropped observably, never queued unboundedly", len(spans), err)
		return fmt.Errorf("telemetry: spool lock unavailable, dropped %d spans: %w", len(spans), err)
	}
	defer release()
	size, statErr := e.spoolSizeLocked()
	if statErr != nil {
		e.rotationFailures++
		e.noticef("export dropped %d spans: spool stat failed (%v) — refusing to write blind past the bound", len(spans), statErr)
		return fmt.Errorf("telemetry: spool stat failed, refusing possibly-unbounded write: %w", statErr)
	}
	if size+int64(len(payload)) > hardSpoolBytes() {
		// One recovery attempt before dropping: the failure that grew the
		// file may have been transient, or this process may succeed where
		// another gave up.
		e.compactLocked()
		size, statErr = e.spoolSizeLocked()
		if statErr != nil {
			e.rotationFailures++
			e.noticef("export dropped %d spans: spool re-stat failed (%v)", len(spans), statErr)
			return fmt.Errorf("telemetry: spool re-stat failed, refusing possibly-unbounded write: %w", statErr)
		}
		if size+int64(len(payload)) > hardSpoolBytes() {
			e.droppedBatches++
			e.droppedSpans += int64(len(spans))
			e.noticef("hard bound %d bytes exceeded (size=%d, batch=%d); dropped %d spans in %d batch(es); compaction failures so far: %d — storage stays bounded at the hard cap, records do not",
				hardSpoolBytes(), size, len(payload), len(spans), 1, e.rotationFailures)
			return fmt.Errorf("telemetry: spool at %s exceeds the hard bound (%d + %d > %d); dropped %d spans — compaction is failing, see stderr notices", e.path, size, len(payload), hardSpoolBytes(), len(spans))
		}
	}
	if _, err := e.f.WriteString(payload); err != nil {
		return err
	}
	if resuming {
		// The summary for the earlier drops has now landed: only now is the
		// accounting consumed.
		e.droppedBatches, e.droppedSpans = 0, 0
	}
	e.compactLocked()
	return nil
}

// dropSummaryLine records the drop into the spool itself once exports resume,
// so a truncated gap is provable from the data that survived it. Called with
// the lock held. It does NOT reset the counters: the summary is built before
// admission, and an export that is itself dropped must not consume the
// accounting for drops it never recorded. The caller resets after a write.
func (e *spoolExporter) dropSummaryLine() string {
	rec := map[string]any{
		"_time":           time.Now().UTC().Format(time.RFC3339Nano),
		"_msg":            "telemetry.spool.dropped",
		"dropped_batches": e.droppedBatches,
		"dropped_spans":   e.droppedSpans,
		"bound_bytes":     hardSpoolBytes(),
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return ""
	}
	return string(line) + "\n"
}

func (e *spoolExporter) Shutdown(context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.f == nil {
		return nil
	}
	err := e.f.Close()
	e.f = nil
	return err
}

// sanitizeField maps OTel's dotted attribute keys to LogsQL-friendly names.
// "exit.code" would parse as a field access in a query; "exit_code" filters.
func sanitizeField(k string) string {
	return strings.ReplaceAll(k, ".", "_")
}

// SpoolPath is where spans land when the file sink is selected.
// $BASHY_OTEL_SPOOL overrides; otherwise it sits beside the stack's own data so
// one directory holds all observability state.
func SpoolPath() string {
	if p := strings.TrimSpace(os.Getenv("BASHY_OTEL_SPOOL")); p != "" {
		return p
	}
	p, _ := defaultSpoolPath()
	return p
}

// defaultSpoolPath also says whether Bashy owns the containing directory. The
// fallback is a file in the shared temporary directory, so only the file is
// ours there; the user's home-local spool directory is dedicated to Bashy.
func defaultSpoolPath() (path string, ownsDir bool) {
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return filepath.Join(os.TempDir(), "bashy-otel-spool.jsonl"), false
	}
	return filepath.Join(home, ".agents", "otel", "spool", "spans.jsonl"), true
}

func isCanonicalSpoolPath(path string) bool {
	canonical, ownsDir := defaultSpoolPath()
	if !ownsDir {
		return false
	}
	return sameSpoolPath(path, canonical)
}

func isDefaultSpoolPath(path string) bool {
	canonical, _ := defaultSpoolPath()
	return sameSpoolPath(path, canonical)
}

func sameSpoolPath(path, canonical string) bool {
	absCanonical, err := filepath.Abs(canonical)
	return err == nil && filepath.Clean(path) == filepath.Clean(absCanonical)
}

// isFileExporter reports whether the file sink is selected.
//
// OTEL_TRACES_EXPORTER is the spec's own knob (its defined values include
// "otlp", "console" and "none"); "file" is the OTLP file-exporter convention.
// "console" is accepted as a synonym rather than writing to stdout directly,
// because a shell that prints spans into its own stdout corrupts every pipeline
// it is part of — the spool file is the honest reading of the intent.
func isFileExporter() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("OTEL_TRACES_EXPORTER"))) {
	case "file", "console":
		return true
	case "none":
		return false // explicit opt-out
	case "otlp", "grpc", "http", "http/protobuf":
		return false // caller wants the network exporter
	case "":
		// DEFAULT ON. Unset means "nobody configured anything", and the useful
		// behaviour there is to record — to a file, which needs no service, no
		// port and no setup.
		//
		// The old default was total no-op, on the reasoning that a shell should
		// not pay for telemetry it is not exporting. True of a network exporter,
		// which buys nothing when no collector is listening. Not true of a file:
		// the spool is an append on an already-open handle, bounded at 64 MiB
		// (hard-bounded at twice that), and it is what makes a failure
		// explicable AFTER the fact rather than only while someone happened to
		// be watching.
		//
		// That distinction is the whole lesson of this subsystem: every silent
		// failure found here — a QA poller dead for ten days, a red CI nobody
		// saw, a stack up for eight days with zero data — was invisible because
		// telemetry defaulted to off and somebody had to opt in first.
		//
		// Opt out with OTEL_TRACES_EXPORTER=none.
		return !hasOTLPEndpoint()
	}
	return false
}

// hasOTLPEndpoint reports whether a network collector was configured. When one
// is, it wins the unset-exporter case: an operator who set an endpoint asked
// for the network path, and quietly spooling to a file instead would be its own
// silent surprise.
func hasOTLPEndpoint() bool {
	return strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT")) != "" ||
		strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")) != ""
}

func networkEndpoint(exporter string) string {
	if endpoint := strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT")); endpoint != "" {
		return endpoint
	}
	if endpoint := strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")); endpoint != "" {
		return endpoint
	}
	switch exporter {
	case "otlp", "grpc", "http", "http/protobuf":
		if strings.EqualFold(strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_PROTOCOL")), "grpc") || exporter == "grpc" {
			return "http://localhost:4317"
		}
		return "http://localhost:4318"
	default:
		return ""
	}
}

// defaultMaxSpoolBytes bounds the spool so telemetry cannot fill a disk when
// nothing ever imports it — the realistic long-running case, since importing
// requires someone to start the stack. 64 MiB holds on the order of a hundred
// thousand exec spans, far more than any debugging session needs, and is small
// enough that losing the oldest half is not a real loss.
const defaultMaxSpoolBytes = 64 << 20

// maxSpoolBytes is a variable only so the package's own tests can exercise the
// bound without writing 64 MiB fixtures; nothing outside _test.go assigns it.
var maxSpoolBytes = int64(defaultMaxSpoolBytes)

// hardSpoolBytes is the ceiling that is never exceeded silently: between the
// soft bound and this, records still land (compaction is only attempted); past
// it, exports are dropped with an error, a stderr notice and a resume summary
// record. Two soft bounds' worth of slack keeps a failing compaction from
// discarding data while there is still room to hope it recovers.
func hardSpoolBytes() int64 { return 2 * maxSpoolBytes }

// Compaction working set. The chunk bounds memory (never the whole file in
// RAM); the backscan bounds how far a record boundary is searched for before
// compaction refuses rather than cut mid-record; the heal cap bounds the line
// carry of the copy filter (longer lines pass through unvalidated, never
// dropped blind).
const (
	rotateChunk     = 1 << 20
	maxLineBackscan = 64 << 10
	healMaxLine     = 1 << 20
)

// spoolNoticeSink and spoolNoticeInterval exist so tests can observe the
// storage-pressure notices without scraping stderr; they are not configuration.
var (
	spoolNoticeSink     io.Writer = os.Stderr
	spoolNoticeInterval           = 5 * time.Minute

	// spoolRotateHook, when non-nil, is called at compaction stages so tests
	// can kill a process mid-rotation deterministically. Never set outside
	// _test.go.
	spoolRotateHook func(stage string)
)

// rotateIfLarge compacts the spool to its newest half once it passes the soft
// bound.
//
// Dropping the OLDEST is deliberate: when telemetry is being used to explain
// something that just happened, recent spans are the ones that matter. The
// alternative — refusing to write once full — would silently stop recording at
// exactly the moment a long session got interesting.
//
// Called with e.mu held. It takes the cross-process file lock itself; the
// export path holds the lock across admission → write → compactLocked instead
// and must NOT call this (the lock is not re-entrant across handles).
func (e *spoolExporter) rotateIfLarge() {
	if e.f == nil {
		return
	}
	release, err := lockSpool(context.Background(), e.f)
	if err != nil {
		e.rotationFailures++
		e.noticef("compaction skipped: spool lock unavailable (%v) — rotation failures so far: %d", err, e.rotationFailures)
		return
	}
	defer release()
	e.compactLocked()
}

// compactLocked shrinks the spool while the caller holds e.mu AND the
// cross-process file lock. Halving repeats until the file is under the soft
// bound — a file several bounds large cannot get there in one cut.
func (e *spoolExporter) compactLocked() {
	for pass := 0; pass < 8; pass++ {
		fi, err := e.f.Stat()
		if err != nil {
			e.rotationFailures++
			e.noticef("compaction stat failed: %v — refusing to compact blind; the spool stays writable up to the hard bound %d, then exports are dropped observably", err, hardSpoolBytes())
			return
		}
		if fi.Size() < maxSpoolBytes {
			return
		}
		dropped, err := e.compactPass(fi.Size())
		if err != nil {
			e.rotationFailures++
			e.noticef("compaction failed (size=%d, failures=%d): %v — the spool stays writable up to the hard bound %d, then exports are dropped observably",
				fi.Size(), e.rotationFailures, err, hardSpoolBytes())
			return
		}
		if dropped > 0 {
			e.noticef("compaction dropped %d invalid line(s) — a killed rotation or a torn append left bytes that are not valid records; the newest valid records are preserved", dropped)
		}
	}
}

// compactPass performs one halve on a handle separate from the append handle
// (which is never closed, never renamed away from, and stays valid across
// compactions). The caller holds the cross-process lock; this handle takes
// none.
func (e *spoolExporter) compactPass(size int64) (int64, error) {
	f, err := os.OpenFile(e.path, os.O_RDWR, 0)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return compactSpoolInPlace(f, size)
}

// compactSpoolInPlace moves the newest line-aligned half of the spool to the
// front of the SAME file and truncates. There is no scratch file, no rename,
// and no whole-file read: memory is at most three rotateChunk buffers plus one bounded
// line carry, and total storage never exceeds the file's current size.
//
// The copy FILTERS: only complete valid-jsonline records are carried forward,
// and the count of dropped invalid lines is returned for the caller to report.
// A kill mid-copy leaves a torn junction line (the prefix is a partial copy,
// the suffix is original bytes); a kill mid-append by a producer that died
// without the transaction lock can leave a torn tail line. The next compaction
// re-derives its cut from a line boundary and this filter drops the torn
// lines, so recovery converges to a fully valid file with the newest valid
// records intact. Lines longer than healMaxLine are passed through unvalidated
// rather than risk dropping a monster-but-valid record blind.
//
// Crash safety of the copy itself: the filter only ever writes below its read
// offset (emitted bytes can never exceed consumed bytes, and the cut is
// positive), so a kill mid-copy still leaves the newest records untouched at
// the tail and the file exactly as large as it was.
func compactSpoolInPlace(f *os.File, size int64) (int64, error) {
	cut, err := alignCutToLine(f, size/2)
	if err != nil {
		return 0, err
	}
	in := make([]byte, rotateChunk)
	out := make([]byte, 0, rotateChunk)
	var carry []byte
	blind := false // current line exceeded healMaxLine: keep it without validating
	var dropped, dst int64
	flush := func() error {
		if len(out) == 0 {
			return nil
		}
		if _, err := f.WriteAt(out, dst); err != nil {
			return fmt.Errorf("compaction write @%d: %w", dst, err)
		}
		dst += int64(len(out))
		out = out[:0]
		return nil
	}
	emit := func(line []byte, valid bool) error {
		if !valid {
			dropped++
			return nil
		}
		out = append(out, line...)
		out = append(out, '\n')
		if len(out) >= rotateChunk {
			return flush()
		}
		return nil
	}
	// copyRange carries bytes that were never assembled in memory (an
	// over-long line kept blind) straight from source to destination in
	// bounded chunks. Safe for the same reason the filter is: dst only ever
	// trails consumed input, and the range lies ahead of it.
	var copyBuf []byte
	copyRange := func(from, to int64) error {
		if copyBuf == nil {
			// Never overwrite in: its outer range still has unprocessed bytes.
			copyBuf = make([]byte, rotateChunk)
		}
		if err := flush(); err != nil {
			return err
		}
		for off := from; off < to; {
			n := int64(len(copyBuf))
			if rem := to - off; rem < n {
				n = rem
			}
			m, rerr := f.ReadAt(copyBuf[:n], off)
			if m > 0 {
				if _, err := f.WriteAt(copyBuf[:m], dst); err != nil {
					return fmt.Errorf("compaction write @%d: %w", dst, err)
				}
				dst += int64(m)
				off += int64(m)
			}
			if rerr != nil {
				if rerr == io.EOF {
					break
				}
				return fmt.Errorf("compaction read @%d: %w", off, rerr)
			}
			if m == 0 {
				break
			}
		}
		return nil
	}
	lineStart := cut
	for src := cut; src < size; {
		n := int64(len(in))
		if rem := size - src; rem < n {
			n = rem
		}
		m, rerr := f.ReadAt(in[:n], src)
		for i, c := range in[:m] {
			if c == '\n' {
				if blind {
					// Over-long line: carry holds only its prefix, so the
					// whole [lineStart, src+i] range moves directly; the
					// emit below appends just its terminator at the
					// post-copy dst, which is exactly where it belongs.
					if err := copyRange(lineStart, src+int64(i)); err != nil {
						return 0, err
					}
					if err := emit(nil, true); err != nil {
						return 0, err
					}
				} else if err := emit(carry, json.Valid(carry)); err != nil {
					return 0, err
				}
				carry = carry[:0]
				blind = false
				lineStart = src + int64(i) + 1
				continue
			}
			if !blind {
				if len(carry) >= healMaxLine {
					blind = true
				} else {
					carry = append(carry, c)
				}
			}
		}
		src += int64(m)
		if rerr != nil {
			if rerr == io.EOF {
				break
			}
			return 0, fmt.Errorf("compaction read @%d: %w", src, rerr)
		}
		if m == 0 {
			break
		}
		if spoolRotateHook != nil {
			spoolRotateHook("copy")
		}
	}
	if len(carry) > 0 || blind {
		// Trailing bytes without a terminator: a torn tail from a killed
		// append. It is not a record; drop it and count it.
		dropped++
	}
	if err := flush(); err != nil {
		return 0, err
	}
	if spoolRotateHook != nil {
		spoolRotateHook("pre-truncate")
	}
	if err := f.Truncate(dst); err != nil {
		return 0, fmt.Errorf("compaction truncate: %w", err)
	}
	if err := f.Sync(); err != nil {
		return 0, fmt.Errorf("compaction sync: %w", err)
	}
	return dropped, nil
}

// alignCutToLine returns the smallest offset ≥ target at which a record starts
// (the byte before it is a newline), so the survivor stays valid jsonline. A
// record longer than the backscan window is refused loudly rather than cut
// mid-record — corrupting the import format to save space is not a trade.
func alignCutToLine(f *os.File, target int64) (int64, error) {
	if target <= 0 {
		return 0, nil
	}
	lo := target - maxLineBackscan
	if lo < 0 {
		lo = 0
	}
	win := make([]byte, target-lo)
	if _, err := f.ReadAt(win, lo); err != nil && err != io.EOF {
		return 0, fmt.Errorf("boundary scan: %w", err)
	}
	for i := len(win) - 1; i >= 0; i-- {
		if win[i] == '\n' {
			return lo + int64(i) + 1, nil
		}
	}
	return 0, fmt.Errorf("no record boundary within %d bytes before offset %d; refusing to cut a single oversized record", maxLineBackscan, target)
}

// spoolSizeLocked returns the current size of the append handle's file.
// Callers fail CLOSED on error: writing blind past the bound is exactly the
// failure this file exists to prevent. Called with e.mu and the file lock held.
func (e *spoolExporter) spoolSizeLocked() (int64, error) {
	if e.f == nil {
		return 0, fmt.Errorf("spool handle closed")
	}
	fi, err := e.f.Stat()
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

// noticef writes a storage-pressure notice at most once per interval per
// exporter: silent failures are how the disk filled, but a notice per batch
// would itself be a flood. Called with the lock held.
func (e *spoolExporter) noticef(format string, args ...any) {
	now := time.Now()
	if !e.lastNotice.IsZero() && now.Sub(e.lastNotice) < spoolNoticeInterval {
		return
	}
	e.lastNotice = now
	fmt.Fprintf(spoolNoticeSink, "bashy: telemetry spool: "+format+"\n", args...)
}

package telemetry

// TYPED service-status instrumentation with state-change rollup at the
// producer seam.
//
// The disk-exhaustion incident's noisiest single contributor was a poller
// appending one record per status/health poll, healthy or not, forever. The
// operator contract for the P0 repair is a deterministic state-change rollup
// applied AT THE EARLIEST TYPED SEAM — before records reach the spool — so the
// spool does not grow and later (deferred) rollups do not have to shrink it:
//
//   - a stream is keyed by stable service/target identity; distinct targets are
//     never merged;
//   - the FIRST observation of a key emits one record, and every MEANINGFUL
//     change after it (healthy → degraded/error, recovery, changed error
//     signature, unknown) emits exactly one record carrying prior state, new
//     state, both timestamps and the count of suppressed repeats;
//   - identical repeats emit NOTHING and only bump bounded first/last/count
//     metadata — but a short failure between successes still emits, so flapping
//     and recovery survive with their timestamps;
//   - event time (status.observed_at, the caller's clock) is kept distinct from
//     ingestion time (the record's _time, when the span ends);
//   - out-of-order and duplicate input never regresses the state machine;
//   - a status QUERY failing (transport, DNS, refused) is recorded separately
//     from a service REPORTING an unhealthy state — a failed curl is not a
//     dead service, and a successful one is not a healthy service;
//   - a polling gap may be reported (status.stale, status.gap_ms) under an
//     explicit opt-in policy; nothing is ever marked unknown by inference;
//   - GENERIC EXEC SPANS ARE NOT CLASSIFIED. Only callers of these typed
//     helpers are status streams; an `exec curl …` span is not a health
//     observation and must never be reinterpreted as one.
//
// Suppression state survives process restarts (bounded JSON beside the spool,
// guarded by the same cross-process lock family) so a restarted poller does not
// re-emit "first observed healthy" on every launch. This is dedup metadata for
// a bounded transient store — NOT a retention tier, NOT the deferred layered
// retention/rollup/memory platform, and it never deletes raw records: it only
// decides whether a NEW record is worth creating.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// Conventional status states. Free-form values are tolerated (a caller may
// have richer vocabulary), but these are what the seam itself emits and what
// consumers should treat as canonical.
const (
	StatusHealthy  = "healthy"
	StatusDegraded = "degraded"
	StatusError    = "error"
	StatusUnknown  = "unknown"
)

const (
	statusSpanName        = "service.status"
	statusQuerySpanName   = "service.status_query_failed"
	statusDetailLimit     = 256
	maxStatusKeys         = 1024
	statusStateFileMaxLen = 64 << 10
	statusSchemaVersion   = 2
)

// StatusObservation is one typed health observation of a named service target,
// as REPORTED (not inferred). Service and Target together identify the stream:
// "loom on :3000" and "loom on :3001" are two streams, and so are "zot" and
// "zot-mirror". ObservedAt is the EVENT time on the observer's clock; zero
// means "now".
type StatusObservation struct {
	Service    string
	Target     string
	State      string
	Detail     string
	ObservedAt time.Time
}

// StatusQueryFailure records that a status QUERY itself failed — connection
// refused, timeout, DNS. It is deliberately not a State: a poller that cannot
// reach a service has learned nothing about the service, and writing "unknown"
// on its behalf is exactly the inference this seam refuses to make.
type StatusQueryFailure struct {
	Service    string
	Target     string
	Err        string
	ObservedAt time.Time
}

// statusStreamState is the bounded per-stream dedup metadata.
type statusStreamState struct {
	State     string `json:"state"`
	Signature string `json:"sig"`
	// Detail is the truncated display text behind Signature, so a changed
	// error can be reported with its prior text as evidence. Comparison uses
	// Signature (a full hash) only.
	Detail        string    `json:"detail,omitempty"`
	FirstObserved time.Time `json:"first"`
	LastObserved  time.Time `json:"last"`
	Repeats       uint64    `json:"repeats"`
	StaleMarkedAt time.Time `json:"stale_at,omitempty"`
}

type statusDedupFile struct {
	Schema  int                           `json:"schema"`
	Status  map[string]*statusStreamState `json:"status"`
	Queries map[string]*statusStreamState `json:"queries"`
}

var (
	statusMu       sync.Mutex
	statusTable    *statusDedupFile
	statusLoaded   bool
	statusFileLoc  string
	statusNoticeAt time.Time

	// statusStaleOverride is a test seam for the gap policy; production reads
	// the environment once (StatusStaleAfter). Atomic because it is read on
	// the emission path and written by tests.
	statusStaleOverride atomic.Int64
)

// StatusStaleAfter is the explicit gap policy: the silence threshold after
// which the next observation of a stream additionally reports the gap
// (status.stale=true, status.gap_ms) once per gap. Zero (the default) disables
// it — silence is reported, never converted into an inferred state.
// BASHY_TELEMETRY_STATUS_STALE (a Go duration, e.g. "90s") configures it.
func StatusStaleAfter() time.Duration {
	if v := statusStaleOverride.Load(); v != 0 {
		return time.Duration(v)
	}
	return statusStaleCache()
}

var statusStaleCache = sync.OnceValue(func() time.Duration {
	v := strings.TrimSpace(os.Getenv("BASHY_TELEMETRY_STATUS_STALE"))
	if v == "" {
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0
	}
	return d
})

// statusEmit is the decision produced by the dedup step.
type statusEmit struct {
	queryRecovered bool
	query          bool
	state          string
	priorState     string
	priorDetail    string
	first          bool
	recovered      bool
	detail         string
	gapMS          int64
	stale          bool
	repeats        uint64
	observedAt     time.Time
	firstSeen      time.Time
}

// statusCommandMarker is scoped to one middleware dispatch. An argv shape
// alone cannot prove that an external/old service command emits typed facts.
type statusCommandMarker struct{}

// MarkServiceStatusReported is called by the typed service-command producer
// after it has handled the query. It lets exec instrumentation avoid a
// redundant generic poll span without suppressing unrelated commands.
func MarkServiceStatusReported(ctx context.Context) {
	if reported, ok := ctx.Value(statusCommandMarker{}).(*atomic.Bool); ok {
		reported.Store(true)
	}
}

// ServiceStatus records one typed health observation, emitting a telemetry
// record only for first observations and meaningful changes (see the package
// comment in this file). Safe with no provider configured: it is then a pure
// no-op that does not advance suppression state.
func ServiceStatus(ctx context.Context, obs StatusObservation) {
	key, at := normalizeObservation(obs.Service, obs.Target, obs.State, obs.ObservedAt)
	observeStatus(ctx, key, at, obs.State, obs.Detail, false)
}

// ServiceStatusQueryFailed records that a status query failed, separately from
// reported service state, with the same repeat-collapse semantics per error
// signature. A subsequent successful ServiceStatus for the same stream clears
// the query-failure state (the observation itself is the evidence the query
// path works again) emitting a real status observation marked query_recovered when health itself
// is unchanged.
func ServiceStatusQueryFailed(ctx context.Context, fail StatusQueryFailure) {
	key, at := normalizeObservation(fail.Service, fail.Target, "query_failed", fail.ObservedAt)
	observeStatus(ctx, key, at, "query_failed", fail.Err, true)
}

// observeStatus runs one typed observation end to end under statusMu.
//
// Without the file sink the suppression table lives in memory and the span is
// emitted or rolled back exactly as before. With the file sink the whole
// read → compare → update → persist sequence holds one cross-process lock on
// the state file, so two producers cannot both see "first observation" and
// both emit: the table is reloaded fresh from disk on every call and persisted
// on every call (repeats included — lastSeen/count are evidence too), and a
// span that turns out not to be recording discards its decision without ever
// touching the disk, so no rollback pass is needed on that path.
//
// The state lock is a single try, not a wait: a status poll must never block
// on another process's telemetry. If the lock is contended or the state file
// is unavailable, the observation is emitted WITHOUT suppression (evidence
// first) and the fallback is reported through the rate-limited notice.
func observeStatus(ctx context.Context, key string, at time.Time, state, detail string, query bool) {
	statusMu.Lock()
	defer statusMu.Unlock()
	if !isFileExporter() {
		if statusTable == nil {
			statusTable = newStatusDedupFile()
		}
		emit, undo := applyObservation(statusTable, key, at, state, detail, query)
		if emit == nil {
			return
		}
		if emitStatusRecord(ctx, key, emit) {
			return
		}
		rollbackStatus(statusTable, key, undo, query)
		return
	}
	f, release, err := openLockedStatusFile()
	if err != nil {
		statusNotice("status state unavailable (%v); emitting without suppression", err)
		emitStatusRecord(ctx, key, firstEmit(at, state, detail, query))
		return
	}
	defer f.Close()
	defer release()
	t := readStatusFile(f)
	emit, _ := applyObservation(t, key, at, state, detail, query)
	if emit == nil {
		persistStatusFile(f, t)
		return
	}
	if emitStatusRecord(ctx, key, emit) {
		persistStatusFile(f, t)
	}
	// Not recording: the decision is discarded and the disk is untouched, so
	// a disabled provider can never advance or corrupt suppression state.
}

// firstEmit builds an unsuppressed first-observation decision for the
// state-unavailable fallback path.
func firstEmit(at time.Time, state, detail string, query bool) *statusEmit {
	return &statusEmit{
		state: state, first: true, detail: truncateDetail(detail),
		observedAt: at, firstSeen: at, query: query,
	}
}

func normalizeObservation(service, target, state string, at time.Time) (string, time.Time) {
	service = strings.TrimSpace(service)
	if service == "" {
		service = "unnamed"
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	return service + "\x00" + strings.TrimSpace(target), at
}

// statusUndo restores every map an observation may have touched: the stream's
// own previous entry (in whichever table the observation used) plus, on the
// status path, a query-failure signature the observation retires. A nil entry
// means "absent" and restores as a delete.
type statusUndo struct {
	entry   *statusStreamState
	retired *statusStreamState
}

// newStatusDedupFile is the empty suppression table for the current schema.
func newStatusDedupFile() *statusDedupFile {
	return &statusDedupFile{
		Schema:  statusSchemaVersion,
		Status:  map[string]*statusStreamState{},
		Queries: map[string]*statusStreamState{},
	}
}

// A late successful response cannot retire a query failure that happened
// after that response was observed, even if the service-state stream is older.
func queryFailureCanRetire(failure *statusStreamState, at time.Time) bool {
	return failure == nil || !at.Before(failure.LastObserved)
}

// applyObservation runs the deterministic dedup: it mutates the table and
// returns the emission decision plus an undo snapshot for the case where no
// provider turns out to be recording (the state machine must not advance on
// records that were never emitted). The caller holds statusMu (and, on the
// file path, the cross-process state lock); the table is already fresh.
func applyObservation(t *statusDedupFile, key string, at time.Time, state, detail string, query bool) (*statusEmit, statusUndo) {
	state = strings.TrimSpace(state)
	if state == "" {
		state = StatusUnknown
	}
	sig := detailSignature(detail)

	table := t.Status
	if query {
		table = t.Queries
	}
	prev := table[key]
	if prev == nil {
		entry := &statusStreamState{State: state, Signature: sig, Detail: truncateDetail(detail), FirstObserved: at, LastObserved: at}
		table[key] = entry
		undo := statusUndo{}
		if !query {
			// Even a FIRST observation is evidence the query path works —
			// retire any tracked query-failure signature (see the change path).
			undo.retired = t.Queries[key]
			if queryFailureCanRetire(undo.retired, at) {
				delete(t.Queries, key)
			}
		}
		return &statusEmit{
			state: state, first: true, detail: truncateDetail(detail),
			observedAt: at, firstSeen: at, query: query,
		}, undo
	}
	if at.Before(prev.LastObserved) {
		// Out-of-order or duplicate delivery: count it, never regress.
		prev.Repeats++
		return nil, statusUndo{}
	}
	if state == prev.State && sig == prev.Signature {
		if !query && t.Queries[key] != nil && queryFailureCanRetire(t.Queries[key], at) {
			// A successful query is meaningful recovery even if the reported
			// service state did not change. Emitting it also allows the normal
			// no-provider rollback to restore every touched piece of state.
			snapshot := *prev
			retired := t.Queries[key]
			delete(t.Queries, key)
			prev.LastObserved = at
			prev.Repeats = 0
			prev.StaleMarkedAt = time.Time{}
			return &statusEmit{state: state, priorState: state,
				detail: truncateDetail(detail), queryRecovered: true,
				gapMS:   at.Sub(snapshot.LastObserved).Milliseconds(),
				repeats: snapshot.Repeats, observedAt: at,
				firstSeen: snapshot.FirstObserved}, statusUndo{entry: &snapshot, retired: retired}
		}
		gap := at.Sub(prev.LastObserved)
		if threshold := StatusStaleAfter(); threshold > 0 && gap > threshold && prev.StaleMarkedAt.IsZero() {
			snapshot := *prev
			prev.StaleMarkedAt = at
			prev.LastObserved = at
			repeats := prev.Repeats
			prev.Repeats = 0
			undo := statusUndo{entry: &snapshot}
			if !query {
				undo.retired = t.Queries[key]
			}
			return &statusEmit{
				state: state, priorState: state, priorDetail: prev.Detail, detail: truncateDetail(detail),
				gapMS: gap.Milliseconds(), stale: true, repeats: repeats,
				observedAt: at, firstSeen: prev.FirstObserved, query: query,
			}, undo
		}
		prev.LastObserved = at
		prev.Repeats++
		prev.StaleMarkedAt = time.Time{}
		return nil, statusUndo{}
	}
	snapshot := *prev
	next := &statusStreamState{State: state, Signature: sig, Detail: truncateDetail(detail), FirstObserved: at, LastObserved: at}
	table[key] = next
	undo := statusUndo{entry: &snapshot}
	if !query {
		// A real observation answers the query path: forget any tracked
		// query-failure signature so a LATER failure emits as new evidence.
		undo.retired = t.Queries[key]
		if queryFailureCanRetire(undo.retired, at) {
			delete(t.Queries, key)
		}
	}
	return &statusEmit{
		state: state, priorState: prev.State, priorDetail: prev.Detail,
		detail: truncateDetail(detail), gapMS: at.Sub(prev.LastObserved).Milliseconds(),
		repeats: prev.Repeats, recovered: !query && state == StatusHealthy && prev.State != StatusHealthy,
		observedAt: at, firstSeen: at, query: query,
	}, undo
}

// emitStatusRecord turns a decision into (at most) one span. The span carries
// the facts as ATTRIBUTES, not events — the spool serializes span attributes
// only, so event-only facts would be invisible on the file sink. Query-failure
// records deliberately carry no status.state: a failed query learned nothing
// about the service's state and must not be readable as one. It reports
// whether the span is recording so the caller can persist or roll back.
func emitStatusRecord(ctx context.Context, key string, emit *statusEmit) bool {
	service, target, _ := strings.Cut(key, "\x00")
	name := statusSpanName
	if emit.query {
		name = statusQuerySpanName
	}
	base := []attribute.KeyValue{
		attribute.String("status.service", service),
		attribute.String("status.target", target),
		attribute.String("status.observed_at", emit.observedAt.UTC().Format(time.RFC3339Nano)),
		attribute.String("status.first_observed", emit.firstSeen.UTC().Format(time.RFC3339Nano)),
		attribute.Int64("status.repeat_count", int64(emit.repeats)),
	}
	if !emit.query {
		base = append(base, attribute.String("status.state", emit.state))
	}
	_, span := otel.Tracer(instrumentationName).Start(ctx, name,
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(base...),
	)
	if !span.IsRecording() {
		span.End()
		return false
	}
	if !emit.first {
		span.SetAttributes(
			attribute.String("status.prior_state", emit.priorState),
			attribute.Bool("status.recovered", emit.recovered),
		)
		if emit.priorDetail != "" && emit.priorDetail != emit.detail {
			span.SetAttributes(attribute.String("status.prior_detail", emit.priorDetail))
		}
	}
	if emit.queryRecovered {
		span.SetAttributes(attribute.Bool("status.query_recovered", true))
	}
	if emit.gapMS > 0 {
		span.SetAttributes(attribute.Int64("status.gap_ms", emit.gapMS))
	}
	if emit.stale {
		span.SetAttributes(attribute.Bool("status.stale", true))
	}
	if emit.query {
		span.SetAttributes(attribute.String("status.query_error", emit.detail))
	} else if emit.detail != "" {
		span.SetAttributes(attribute.String("status.detail", emit.detail))
	}
	span.End()
	return true
}

// rollbackStatus restores every map the observation may have touched: the
// stream's own entry in the table it used, and — on the status path only —
// a retired query-failure signature. The query path never touches the status
// table, so restoring there would delete evidence it did not create.
func rollbackStatus(t *statusDedupFile, key string, undo statusUndo, query bool) {
	table := t.Status
	if query {
		table = t.Queries
	}
	if undo.entry == nil {
		delete(table, key)
	} else {
		table[key] = undo.entry
	}
	if !query {
		if undo.retired == nil {
			delete(t.Queries, key)
		} else {
			t.Queries[key] = undo.retired
		}
	}
}

// openLockedStatusFile opens the suppression state file and takes the
// cross-process state lock in a single non-blocking try. Callers hold
// statusMu; the returned release must run before it is dropped. A status poll
// must never block on another process's telemetry, so contention (or a
// platform without the lock) is an error and the caller falls back to
// emitting without suppression — evidence first, collapse best-effort.
func openLockedStatusFile() (f *os.File, release func() error, err error) {
	path := statusStatePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, nil, err
	}
	f, err = os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, nil, err
	}
	ok, release, err := trySpoolLock(f)
	if err != nil || !ok {
		_ = f.Close()
		if err == nil {
			err = fmt.Errorf("state file locked by another process")
		}
		return nil, nil, err
	}
	return f, release, nil
}

// readStatusFile loads the suppression table fresh from the state handle. Any
// failure (absent, torn by a crash mid-write, oversized, malformed, wrong
// schema) starts a fresh table and says so: the worst case is one extra
// first-observation record per stream, never a lost raw record — this file is
// metadata, not evidence. The caller holds the state lock.
func readStatusFile(f *os.File) *statusDedupFile {
	t := newStatusDedupFile()
	fi, err := f.Stat()
	if err != nil || fi.Size() > statusStateFileMaxLen {
		if err == nil {
			statusNotice("suppression state oversized (%d bytes); starting fresh (dedup only, no records lost)", fi.Size())
		}
		return t
	}
	data := make([]byte, fi.Size())
	if _, err := f.ReadAt(data, 0); err != nil {
		return t
	}
	var disk statusDedupFile
	if json.Unmarshal(data, &disk) != nil || disk.Schema != statusSchemaVersion {
		statusNotice("suppression state unreadable; starting fresh (dedup only, no records lost)")
		return t
	}
	if disk.Status != nil {
		t.Status = disk.Status
	}
	if disk.Queries != nil {
		t.Queries = disk.Queries
	}
	return t
}

// persistStatusFile rewrites the suppression state in place — same
// never-rename rule as the spool itself, on a file small enough that the
// rewrite is one bounded write. The caller holds statusMu and the state lock.
// Every failure is reported through the rate-limited notice; an oversized
// table is trimmed to bytes (never silently dropped) and a marshal failure
// refuses loudly rather than writing half a state.
func persistStatusFile(f *os.File, t *statusDedupFile) {
	trimStatusTable(t)
	data, err := json.Marshal(t)
	if err != nil {
		statusNotice("cannot persist status state: marshal failed: %v", err)
		return
	}
	for len(data) > statusStateFileMaxLen {
		if !trimOldestStatusStream(t) {
			break
		}
		data, err = json.Marshal(t)
		if err != nil {
			statusNotice("cannot persist status state: marshal failed: %v", err)
			return
		}
	}
	if len(data) > statusStateFileMaxLen {
		statusNotice("cannot persist status state: still %d bytes after trimming to one stream; keeping in-memory state only", len(data))
		return
	}
	if _, err := f.WriteAt(data, 0); err != nil {
		statusNotice("cannot persist status state: %v", err)
		return
	}
	if err := f.Truncate(int64(len(data))); err != nil {
		statusNotice("cannot persist status state: %v", err)
	}
}

func statusStatePath() string {
	if statusFileLoc == "" {
		statusFileLoc = filepath.Join(filepath.Dir(SpoolPath()), "status-dedup.json")
	}
	return statusFileLoc
}

// trimStatusTable bounds the metadata: at most maxStatusKeys streams per
// table, evicting the least-recently-observed. Bounded, deterministic, and
// only ever costs a future first-observation record.
func trimStatusTable(t *statusDedupFile) {
	for _, table := range []map[string]*statusStreamState{t.Status, t.Queries} {
		for len(table) > maxStatusKeys {
			if !trimOldestFrom(table) {
				break
			}
		}
	}
}

func trimOldestFrom(table map[string]*statusStreamState) bool {
	var oldestKey string
	var oldest time.Time
	first := true
	for k, v := range table {
		if first || v.LastObserved.Before(oldest) {
			oldestKey, oldest, first = k, v.LastObserved, false
		}
	}
	if first {
		return false
	}
	delete(table, oldestKey)
	return true
}

// trimOldestStatusStream evicts the single least-recently-observed stream
// across both tables, for the byte-budget loop in persistStatusFile.
func trimOldestStatusStream(t *statusDedupFile) bool {
	var oldestKey string
	var oldest time.Time
	var oldestTable map[string]*statusStreamState
	first := true
	for _, table := range []map[string]*statusStreamState{t.Status, t.Queries} {
		for k, v := range table {
			if first || v.LastObserved.Before(oldest) {
				oldestKey, oldest, oldestTable, first = k, v.LastObserved, table, false
			}
		}
	}
	if first {
		return false
	}
	delete(oldestTable, oldestKey)
	return true
}

// detailSignature identifies an error (or detail text) for change detection.
// It is a hash over the FULL normalized text, not a prefix: two long errors
// that share a 64-byte prefix are different errors and must not collapse into
// one stream.
func detailSignature(detail string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(detail)))
	return hex.EncodeToString(sum[:])
}

func truncateDetail(detail string) string {
	d := strings.TrimSpace(detail)
	if len(d) > statusDetailLimit {
		d = d[:statusDetailLimit]
	}
	return d
}

// statusNotice reports suppression-state problems at most once per interval:
// the dedup layer must not become its own noise source.
func statusNotice(format string, args ...any) {
	now := time.Now()
	if !statusNoticeAt.IsZero() && now.Sub(statusNoticeAt) < spoolNoticeInterval {
		return
	}
	statusNoticeAt = now
	fmt.Fprintf(spoolNoticeSink, "bashy: telemetry status: "+format+"\n", args...)
}

//go:build !aix

package telemetry

// Controls for the P0 status/health state-change rollup (operator directive:
// stop retaining every identical poll; preserve transitions, timestamps,
// changed error signatures and recoveries).

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// resetStatusState gives a test a fresh suppression table (and restores the
// process-wide one after), because suppression state is exactly what must NOT
// leak between these controls.
func resetStatusState(t *testing.T) {
	t.Helper()
	// Fence the filesystem by default: without an exporter selected the
	// suppression state stays in memory, so these controls never read or
	// write the operator's real spool directory. The persistence test below
	// re-enables the file sink explicitly against its own temp dir.
	t.Setenv("OTEL_TRACES_EXPORTER", "none")
	t.Setenv("BASHY_OTEL_SPOOL", filepath.Join(t.TempDir(), "fence", "spans.jsonl"))
	statusMu.Lock()
	oldTable, oldLoaded, oldLoc := statusTable, statusLoaded, statusFileLoc
	statusTable, statusLoaded, statusFileLoc = nil, false, ""
	oldStale := statusStaleOverride.Load()
	statusStaleOverride.Store(0)
	statusMu.Unlock()
	t.Cleanup(func() {
		statusMu.Lock()
		statusTable, statusLoaded, statusFileLoc = oldTable, oldLoaded, oldLoc
		statusMu.Unlock()
		statusStaleOverride.Store(oldStale)
	})
}

func statusAttrs(t *testing.T, sr *tracetest.SpanRecorder) []map[string]string {
	t.Helper()
	spans := sr.Ended()
	out := make([]map[string]string, 0, len(spans))
	for _, s := range spans {
		m := map[string]string{"_name": s.Name()}
		for _, kv := range s.Attributes() {
			m[string(kv.Key)] = kv.Value.Emit()
		}
		out = append(out, m)
	}
	return out
}

func obs(service, target, state, detail string, at time.Time) StatusObservation {
	return StatusObservation{Service: service, Target: target, State: state, Detail: detail, ObservedAt: at}
}

// A poller that sees the same healthy state forever must produce ONE record,
// not one per poll — this collapse is the P0 requirement.
func TestRepeatedHealthyPollsCollapse(t *testing.T) {
	resetStatusState(t)
	sr := withRecorder(t)
	base := time.Now().UTC()
	for i := 0; i < 50; i++ {
		ServiceStatus(context.Background(), obs("loom", "http://127.0.0.1:3000", StatusHealthy, "", base.Add(time.Duration(i)*time.Second)))
	}
	records := statusAttrs(t, sr)
	if len(records) != 1 {
		t.Fatalf("50 identical polls produced %d records, want 1", len(records))
	}
	if records[0]["status.state"] != StatusHealthy || records[0]["status.prior_state"] != "" {
		t.Errorf("first record should be the first-observation record: %v", records[0])
	}
}

// healthy → error → healthy must all survive, with event timestamps and prior
// state, and a short failure between successes must never be erased.
func TestHealthyErrorHealthySurvivesWithTimestamps(t *testing.T) {
	resetStatusState(t)
	sr := withRecorder(t)
	t0 := time.Date(2026, 11, 5, 1, 0, 0, 0, time.UTC)
	t1 := t0.Add(200 * time.Millisecond)
	t2 := t1.Add(200 * time.Millisecond)

	ServiceStatus(context.Background(), obs("loom", ":3000", StatusHealthy, "", t0))
	ServiceStatus(context.Background(), obs("loom", ":3000", StatusError, "conn refused", t1))
	ServiceStatus(context.Background(), obs("loom", ":3000", StatusHealthy, "", t2))

	records := statusAttrs(t, sr)
	if len(records) != 3 {
		t.Fatalf("healthy→error→healthy produced %d records, want 3 (a short failure must survive): %+v", len(records), records)
	}
	if records[1]["status.prior_state"] != StatusHealthy || records[1]["status.state"] != StatusError {
		t.Errorf("transition record wrong: %+v", records[1])
	}
	if records[1]["status.observed_at"] != t1.Format(time.RFC3339Nano) {
		t.Errorf("event time not preserved: got %s want %s", records[1]["status.observed_at"], t1.Format(time.RFC3339Nano))
	}
	if records[2]["status.prior_state"] != StatusError || records[2]["status.recovered"] != "true" {
		t.Errorf("recovery record wrong: %+v", records[2])
	}
}

// Same state but a CHANGED error signature is a meaningful change and must be
// retained, with the prior signature kept as evidence.
func TestChangedErrorSignatureRetained(t *testing.T) {
	resetStatusState(t)
	sr := withRecorder(t)
	base := time.Now().UTC()
	ServiceStatus(context.Background(), obs("zot", ":5000", StatusError, "connection refused", base))
	ServiceStatus(context.Background(), obs("zot", ":5000", StatusError, "connection refused", base.Add(time.Second)))
	ServiceStatus(context.Background(), obs("zot", ":5000", StatusError, "i/o timeout", base.Add(2*time.Second)))

	records := statusAttrs(t, sr)
	if len(records) != 2 {
		t.Fatalf("identical error + changed error produced %d records, want 2", len(records))
	}
	if records[1]["status.detail"] != "i/o timeout" {
		t.Errorf("new signature missing: %+v", records[1])
	}
	if records[1]["status.prior_detail"] != "connection refused" {
		t.Errorf("prior signature missing: %+v", records[1])
	}
}

// Distinct services/targets are separate streams: one target flapping must
// not suppress another's records.
func TestTargetsAreIsolated(t *testing.T) {
	resetStatusState(t)
	sr := withRecorder(t)
	base := time.Now().UTC()
	ServiceStatus(context.Background(), obs("loom", ":3000", StatusHealthy, "", base))
	ServiceStatus(context.Background(), obs("loom", ":3001", StatusHealthy, "", base))
	for i := 0; i < 5; i++ {
		ServiceStatus(context.Background(), obs("loom", ":3000", StatusHealthy, "", base.Add(time.Duration(i)*time.Second)))
	}
	ServiceStatus(context.Background(), obs("loom", ":3000", StatusError, "down", base.Add(10*time.Second)))
	ServiceStatus(context.Background(), obs("loom", ":3001", StatusError, "down", base.Add(10*time.Second)))

	records := statusAttrs(t, sr)
	if len(records) != 4 {
		t.Fatalf("two isolated targets produced %d records, want 4 (2 firsts + 2 transitions)", len(records))
	}
}

// Out-of-order input must not regress the state machine or fabricate
// transitions; duplicates are counted, not recorded.
func TestOutOfOrderDoesNotRegress(t *testing.T) {
	resetStatusState(t)
	sr := withRecorder(t)
	base := time.Now().UTC()
	ServiceStatus(context.Background(), obs("zot", ":5000", StatusError, "x", base))
	// A late delivery claiming a healthy state BEFORE the error: absorbed.
	ServiceStatus(context.Background(), obs("zot", ":5000", StatusHealthy, "", base.Add(-time.Minute)))
	// And the real recovery after it:
	ServiceStatus(context.Background(), obs("zot", ":5000", StatusHealthy, "", base.Add(time.Minute)))

	records := statusAttrs(t, sr)
	if len(records) != 2 {
		t.Fatalf("out-of-order stream produced %d records, want 2 (error, recovery)", len(records))
	}
	if records[1]["status.prior_state"] != StatusError {
		t.Errorf("out-of-order input rewrote history: %+v", records[1])
	}
}

// A failed status QUERY is not a reported state: separate record shape, no
// status.state attribute, and a later real observation does not erase it but
// does retire the query-failure signature.
func TestQueryFailureKeptSeparateFromReportedState(t *testing.T) {
	resetStatusState(t)
	sr := withRecorder(t)
	base := time.Now().UTC()
	for i := 0; i < 10; i++ {
		ServiceStatusQueryFailed(context.Background(), StatusQueryFailure{
			Service: "loom", Target: ":3000", Err: "dial tcp: refused", ObservedAt: base.Add(time.Duration(i) * time.Second),
		})
	}
	ServiceStatus(context.Background(), obs("loom", ":3000", StatusHealthy, "", base.Add(30*time.Second)))

	records := statusAttrs(t, sr)
	if len(records) != 2 {
		t.Fatalf("10 failed queries + recovery produced %d records, want 2", len(records))
	}
	if records[0]["_name"] != "service.status_query_failed" {
		t.Errorf("query failure must be its own record: %+v", records[0])
	}
	if _, hasState := records[0]["status.state"]; hasState {
		t.Errorf("query failure carries a reported status.state — a failed query learned nothing about the state: %+v", records[0])
	}
	if records[0]["status.query_error"] != "dial tcp: refused" {
		t.Errorf("query error missing: %+v", records[0])
	}
	if records[1]["status.state"] != StatusHealthy {
		t.Errorf("recovery record wrong: %+v", records[1])
	}
}

// Suppression state survives a process restart via the bounded state file, so
// a restarted poller does not re-emit "first observed" for an unchanged
// state, while a genuine change still emits.
func TestSuppressionStateSurvivesRestart(t *testing.T) {
	resetStatusState(t)
	dir := t.TempDir()
	t.Setenv("BASHY_OTEL_SPOOL", filepath.Join(dir, "spans.jsonl"))
	t.Setenv("OTEL_TRACES_EXPORTER", "file")
	base := time.Now().UTC()

	sr1 := withRecorder(t)
	ServiceStatus(context.Background(), obs("loom", ":3000", StatusHealthy, "", base))
	if len(sr1.Ended()) != 1 {
		t.Fatalf("first session: %d records, want 1", len(sr1.Ended()))
	}
	statePath := filepath.Join(dir, "status-dedup.json")
	b, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("suppression state not persisted: %v", err)
	}
	if len(b) > statusStateFileMaxLen {
		t.Errorf("state file %d bytes exceeds its bound", len(b))
	}
	var probe statusDedupFile
	if json.Unmarshal(b, &probe) != nil || probe.Schema != statusSchemaVersion {
		t.Fatalf("state file not the documented bounded schema: %.80s", b)
	}

	// Simulated restart: fresh in-memory state, same persisted file.
	sr2 := withRecorder(t)
	statusMu.Lock()
	statusTable, statusLoaded = nil, false
	statusMu.Unlock()
	ServiceStatus(context.Background(), obs("loom", ":3000", StatusHealthy, "", base.Add(time.Hour)))
	if got := len(sr2.Ended()); got != 0 {
		t.Errorf("restarted poller re-emitted an unchanged state (%d records); restarts would reintroduce the flood", got)
	}
	ServiceStatus(context.Background(), obs("loom", ":3000", StatusError, "boom", base.Add(2*time.Hour)))
	records := statusAttrs(t, sr2)
	if len(records) != 1 || records[0]["status.prior_state"] != StatusHealthy {
		t.Errorf("post-restart change must still emit with prior state: %+v", records)
	}
}

// Under the explicit gap policy, silence is REPORTED once (stale marker with
// the gap), never converted into an inferred state.
func TestStaleGapPolicyReportsOnce(t *testing.T) {
	resetStatusState(t)
	statusStaleOverride.Store(int64(time.Minute))
	sr := withRecorder(t)
	base := time.Now().UTC()

	ServiceStatus(context.Background(), obs("zot", ":5000", StatusHealthy, "", base))
	// Five minutes of silence, then the same state again: one stale report.
	ServiceStatus(context.Background(), obs("zot", ":5000", StatusHealthy, "", base.Add(5*time.Minute)))
	// Continued quiet polls: no further stale reports.
	ServiceStatus(context.Background(), obs("zot", ":5000", StatusHealthy, "", base.Add(6*time.Minute)))
	ServiceStatus(context.Background(), obs("zot", ":5000", StatusHealthy, "", base.Add(7*time.Minute)))

	records := statusAttrs(t, sr)
	if len(records) != 2 {
		t.Fatalf("gap policy produced %d records, want 2 (first + one stale report)", len(records))
	}
	if records[1]["status.stale"] != "true" {
		t.Errorf("stale marker missing: %+v", records[1])
	}
	if records[1]["status.gap_ms"] != "300000" {
		t.Errorf("gap_ms wrong: %+v", records[1])
	}
	if records[1]["status.state"] != StatusHealthy || records[1]["status.prior_state"] != StatusHealthy {
		t.Errorf("stale report changed the reported state: %+v", records[1])
	}
}

// With no provider configured the seam is a pure no-op — and it must not
// advance suppression state, or the first records after telemetry is switched
// on would be missing their "first observation" context.
func TestStatusWithoutProviderAdvancesNothing(t *testing.T) {
	resetStatusState(t)
	base := time.Now().UTC()
	ServiceStatus(context.Background(), obs("loom", ":3000", StatusHealthy, "", base))

	statusMu.Lock()
	defer statusMu.Unlock()
	if statusTable != nil && len(statusTable.Status) != 0 {
		t.Errorf("no-provider call advanced suppression state: %+v", statusTable.Status)
	}
}

// Details are bounded and secrets-unfriendly by truncation; the signature
// comparison must distinguish different errors within that bound.
func TestDetailTruncationIsBounded(t *testing.T) {
	resetStatusState(t)
	sr := withRecorder(t)
	base := time.Now().UTC()
	long := strings.Repeat("e", 5000)
	ServiceStatus(context.Background(), obs("zot", ":5000", StatusError, long, base))
	records := statusAttrs(t, sr)
	if len(records) != 1 {
		t.Fatalf("want 1 record, got %d", len(records))
	}
	if got := len(records[0]["status.detail"]); got > statusDetailLimit {
		t.Errorf("detail stored at %d bytes; bound is %d", got, statusDetailLimit)
	}
}

// Concurrent producers on distinct streams must not race the shared table.
func TestConcurrentProducersDistinctStreams(t *testing.T) {
	resetStatusState(t)
	sr := withRecorder(t)
	base := time.Now().UTC()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				ServiceStatus(context.Background(), obs("svc"+string(rune('a'+i)), ":1", StatusHealthy, "", base.Add(time.Duration(j)*time.Second)))
				ServiceStatusQueryFailed(context.Background(), StatusQueryFailure{
					Service: "svc" + string(rune('a'+i)), Target: ":1", Err: "refused", ObservedAt: base.Add(time.Duration(j) * time.Second),
				})
			}
		}(i)
	}
	wg.Wait()
	records := statusAttrs(t, sr)
	if len(records) != 16 {
		t.Errorf("8 streams (status + query each) produced %d records, want 16", len(records))
	}
}

// Change detection hashes the FULL error text: two errors sharing a 64-byte
// prefix are different errors and must not collapse into one stream. A
// prefix-truncated signature would emit 1 record here instead of 2.
func TestChangedErrorBeyondPrefixIsRetained(t *testing.T) {
	resetStatusState(t)
	sr := withRecorder(t)
	base := time.Now().UTC()
	err1 := strings.Repeat("e", 64) + "connection refused"
	err2 := strings.Repeat("e", 64) + "i/o timeout"

	ServiceStatus(context.Background(), obs("zot", ":5000", StatusError, err1, base))
	ServiceStatus(context.Background(), obs("zot", ":5000", StatusError, err1, base.Add(time.Second)))
	ServiceStatus(context.Background(), obs("zot", ":5000", StatusError, err2, base.Add(2*time.Second)))

	records := statusAttrs(t, sr)
	if len(records) != 2 {
		t.Fatalf("prefix-sharing errors produced %d records, want 2 (first + changed): %+v", len(records), records)
	}
	if records[1]["status.detail"] != err2 {
		t.Errorf("new error missing: %+v", records[1])
	}
	if records[1]["status.prior_detail"] != err1 {
		t.Errorf("prior error missing: %+v", records[1])
	}
}

// A rolled-back query failure must restore BOTH maps: the stream's queries
// entry (never emitted) and a status entry it never touched. Losing the
// status entry would re-emit "first observed" for a stream that never
// changed — the flood returning through the back door.
func TestQueryFailureRollbackPreservesStatusEntry(t *testing.T) {
	resetStatusState(t)
	// No recorder: the decision cannot record, so it must roll back.
	base := time.Now().UTC()
	statusMu.Lock()
	statusTable = newStatusDedupFile()
	key, _ := normalizeObservation("loom", ":3000", StatusHealthy, base)
	statusTable.Status[key] = &statusStreamState{State: StatusHealthy, Signature: detailSignature(""), FirstObserved: base, LastObserved: base}
	statusMu.Unlock()

	ServiceStatusQueryFailed(context.Background(), StatusQueryFailure{
		Service: "loom", Target: ":3000", Err: "refused", ObservedAt: base.Add(time.Second),
	})

	statusMu.Lock()
	defer statusMu.Unlock()
	if got := statusTable.Status[key]; got == nil || got.State != StatusHealthy {
		t.Errorf("rollback after a query failure destroyed the status entry: %+v", statusTable.Status[key])
	}
	if _, ok := statusTable.Queries[key]; ok {
		t.Error("rollback after a query failure left a queries entry for a record that was never emitted")
	}
}

// Repeats advance lastSeen/count on disk too, so a restart does not lose the
// suppression context and re-emit "first observed" for a stream that has been
// healthy all along.
func TestRepeatObservationsPersistLastSeen(t *testing.T) {
	resetStatusState(t)
	dir := t.TempDir()
	t.Setenv("BASHY_OTEL_SPOOL", filepath.Join(dir, "spans.jsonl"))
	t.Setenv("OTEL_TRACES_EXPORTER", "file")
	withRecorder(t)
	base := time.Now().UTC()

	ServiceStatus(context.Background(), obs("loom", ":3000", StatusHealthy, "", base))
	later := base.Add(time.Hour)
	ServiceStatus(context.Background(), obs("loom", ":3000", StatusHealthy, "", later))

	b, err := os.ReadFile(filepath.Join(dir, "status-dedup.json"))
	if err != nil {
		t.Fatalf("suppression state not persisted: %v", err)
	}
	var probe statusDedupFile
	if json.Unmarshal(b, &probe) != nil {
		t.Fatalf("state file unreadable: %.80s", b)
	}
	ent := probe.Status["loom\x00:3000"]
	if ent == nil {
		t.Fatalf("stream missing from persisted state: %s", b)
	}
	if !ent.LastObserved.Equal(later) {
		t.Errorf("repeat did not persist lastSeen: got %v want %v", ent.LastObserved, later)
	}
	if ent.Repeats != 1 {
		t.Errorf("repeat count not persisted: got %d want 1", ent.Repeats)
	}
}

// When the state lock is contended the poll is emitted WITHOUT suppression
// (evidence first) and the fallback is reported — never blocked, never
// silently collapsed, never silently dropped.
func TestStatusLockContentionFallsBackObservably(t *testing.T) {
	resetStatusState(t)
	dir := t.TempDir()
	t.Setenv("BASHY_OTEL_SPOOL", filepath.Join(dir, "spans.jsonl"))
	t.Setenv("OTEL_TRACES_EXPORTER", "file")
	sr := withRecorder(t)
	notice := withNoticeCapture(t)

	statePath := statusStatePath()
	if err := os.MkdirAll(filepath.Dir(statePath), 0o700); err != nil {
		t.Fatal(err)
	}
	holder, err := os.OpenFile(statePath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	release, err := lockSpool(context.Background(), holder)
	if err != nil {
		t.Skipf("cross-process lock unavailable here: %v", err)
	}
	defer release()

	base := time.Now().UTC()
	ServiceStatus(context.Background(), obs("loom", ":3000", StatusHealthy, "", base))
	ServiceStatus(context.Background(), obs("loom", ":3000", StatusHealthy, "", base.Add(time.Second)))

	records := statusAttrs(t, sr)
	if len(records) != 2 {
		t.Errorf("contended lock produced %d records, want 2 unsuppressed fallbacks", len(records))
	}
	if !strings.Contains(notice.String(), "emitting without suppression") {
		t.Errorf("fallback not reported: %q", notice.String())
	}
}

func TestSuccessfulUnchangedHealthRetiresQueryFailure(t *testing.T) {
	resetStatusState(t)
	sr := withRecorder(t)
	at := time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)
	ServiceStatus(context.Background(), obs("query-recovery", "target", StatusHealthy, "", at))
	ServiceStatusQueryFailed(context.Background(), StatusQueryFailure{Service: "query-recovery", Target: "target", Err: "transport error", ObservedAt: at.Add(time.Second)})
	ServiceStatus(context.Background(), obs("query-recovery", "target", StatusHealthy, "", at.Add(2*time.Second)))
	ServiceStatusQueryFailed(context.Background(), StatusQueryFailure{Service: "query-recovery", Target: "target", Err: "transport error", ObservedAt: at.Add(3 * time.Second)})
	records := statusAttrs(t, sr)
	if len(records) != 4 || records[2]["status.query_recovered"] != "true" || records[3]["_name"] != "service.status_query_failed" {
		t.Fatalf("query recurrence erased: %v", records)
	}
}

func TestUnchangedHealthQueryRecoveryRollback(t *testing.T) {
	table := newStatusDedupFile()
	at := time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)
	key := "service\x00target"
	applyObservation(table, key, at, StatusHealthy, "", false)
	applyObservation(table, key, at.Add(time.Second), "query_failed", "failure", true)
	emit, undo := applyObservation(table, key, at.Add(2*time.Second), StatusHealthy, "", false)
	if emit == nil || !emit.queryRecovered || table.Queries[key] != nil {
		t.Fatal("successful unchanged query did not retire failure")
	}
	rollbackStatus(table, key, undo, false)
	if table.Queries[key] == nil || !table.Status[key].LastObserved.Equal(at) || table.Status[key].Repeats != 0 {
		t.Fatalf("rollback lost state: %+v", table)
	}
}

func TestLateHealthDoesNotRetireNewerQueryFailure(t *testing.T) {
	at := time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)
	key := "late\x00target"
	for _, sameState := range []bool{true, false} {
		table := newStatusDedupFile()
		applyObservation(table, key, at, StatusHealthy, "", false)
		applyObservation(table, key, at.Add(10*time.Second), "query_failed", "newer failure", true)
		state := StatusHealthy
		if !sameState {
			state = StatusDegraded
		}
		emit, undo := applyObservation(table, key, at.Add(5*time.Second), state, "", false)
		if table.Queries[key] == nil || (emit != nil && emit.queryRecovered) {
			t.Fatalf("late status retired later query failure (same=%v)", sameState)
		}
		if emit != nil {
			rollbackStatus(table, key, undo, false)
			if table.Queries[key] == nil {
				t.Fatal("rollback erased untouched newer query failure")
			}
		}
		applyObservation(table, key, at.Add(11*time.Second), state, "", false)
		if table.Queries[key] != nil {
			t.Fatal("newer successful query did not retire failure")
		}
	}
}

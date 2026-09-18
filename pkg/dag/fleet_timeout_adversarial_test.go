package dag

import (
	"context"
	"testing"
	"time"
)

func TestEngineTimeoutWaitingForSlotIsInfraFailure(t *testing.T) {
	// Create a pool with 1 slot.
	pool := NewPool(nil, &Worker{ID: LocalWorkerID, Venues: []string{VenueUserland}, CPU: 1})

	// Occupy the slot so the next task hangs.
	w, _ := pool.TryAcquire(Constraints{})
	if w == nil {
		t.Fatal("expected slot")
	}

	// Create an engine.
	md := "## Tasks\n\n### blocked-task\n" + block("bash", "echo runs")
	e := engineFor(t, t.TempDir(), md)
	e.Capture = true
	e.Fleet = true
	e.Concurrency = 1
	e.Pool = pool

	// Run with a context that times out before the slot is freed.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	report, _ := e.Run(ctx, "blocked-task")
	if !report.Failed {
		t.Fatal("expected run to fail due to context timeout")
	}

	if len(report.Records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(report.Records))
	}

	rec := report.Records[0]
	if rec.Status == RunFailed {
		t.Fatalf("DEFECT: task that never ran (timeout waiting for slot) was recorded as a conformance failure (RunFailed, code=%s)", rec.Failure.Code)
	}
	if rec.Status != RunInfraFailed {
		t.Fatalf("expected RunInfraFailed, got %s", rec.Status)
	}
}

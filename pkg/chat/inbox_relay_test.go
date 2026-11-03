package chat

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/bus"
)

func TestInboxRelayCommitsOnlyAfterDelivery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	var attempts atomic.Int32
	var committed atomic.Bool

	prepare := func() bus.PreparedPreamble {
		if committed.Load() {
			return bus.PreparedPreamble{}
		}
		return bus.NewPreparedPreamble("pending input", func() error {
			committed.Store(true)
			return nil
		})
	}
	deliver := func(p bus.PreparedPreamble) error {
		if attempts.Add(1) == 1 {
			return errors.New("transport busy")
		}
		return p.Commit()
	}

	go runInboxRelayEvery(ctx, done, nil, prepare, deliver, nil, 5*time.Millisecond)
	deadline := time.Now().Add(time.Second)
	for !committed.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !committed.Load() {
		t.Fatal("pending input was not retried and committed after delivery")
	}
	if got := attempts.Load(); got < 2 {
		t.Fatalf("delivery attempts = %d, want retry after failure", got)
	}
}

func TestInboxRelayWaitsForTransportReadiness(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	var ready atomic.Bool
	var delivered atomic.Bool

	go runInboxRelayEvery(ctx, done, ready.Load,
		func() bus.PreparedPreamble { return bus.NewPreparedPreamble("pending", nil) },
		func(bus.PreparedPreamble) error { delivered.Store(true); return nil },
		nil, 5*time.Millisecond)
	time.Sleep(20 * time.Millisecond)
	if delivered.Load() {
		t.Fatal("relay delivered while the transport reported an active turn")
	}
	ready.Store(true)
	deadline := time.Now().Add(time.Second)
	for !delivered.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !delivered.Load() {
		t.Fatal("relay did not deliver after the transport became ready")
	}
}

// The relay must SAMPLE every tick and SNAPSHOT only when the sample moved.
// Snapshotting is the expensive half (two timeline parses plus the host's
// board and meet scan); an unconditional snapshot inside a 1 s ticker is how
// an idle foreman session burned a core (coreutils story #127).
func TestInboxRelaySnapshotsOnlyWhenStoresChange(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	var fingerprint atomic.Uint64
	fingerprint.Store(1)
	var samples, snapshots atomic.Int32

	gate := bus.NewPollGate(func() (uint64, bool) {
		samples.Add(1)
		return fingerprint.Load(), true
	}, time.Hour)
	prepare := func() bus.PreparedPreamble {
		snapshots.Add(1)
		return bus.PreparedPreamble{}
	}
	go runInboxRelayEvery(ctx, done, nil, prepare, func(bus.PreparedPreamble) error { return nil }, gate, 2*time.Millisecond)

	deadline := time.Now().Add(time.Second)
	for samples.Load() < 20 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := samples.Load(); got < 20 {
		t.Fatalf("relay sampled the fingerprint only %d times", got)
	}
	if got := snapshots.Load(); got != 1 {
		t.Fatalf("snapshots with an unchanged fingerprint = %d, want exactly 1 (the first tick)", got)
	}

	fingerprint.Store(2)
	deadline = time.Now().Add(time.Second)
	for snapshots.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := snapshots.Load(); got != 2 {
		t.Fatalf("snapshots after one change = %d, want exactly 2", got)
	}
}

// A refused delivery must not hide behind an unchanged fingerprint: the input
// is still pending, so the next tick must snapshot and retry.
func TestInboxRelayRetriesRefusedDeliveryDespiteUnchangedFingerprint(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	var attempts atomic.Int32
	var committed atomic.Bool
	gate := bus.NewPollGate(func() (uint64, bool) { return 7, true }, time.Hour)
	prepare := func() bus.PreparedPreamble {
		if committed.Load() {
			return bus.PreparedPreamble{}
		}
		return bus.NewPreparedPreamble("pending input", func() error { committed.Store(true); return nil })
	}
	deliver := func(p bus.PreparedPreamble) error {
		if attempts.Add(1) < 3 {
			return errors.New("transport busy")
		}
		return p.Commit()
	}
	go runInboxRelayEvery(ctx, done, nil, prepare, deliver, gate, 2*time.Millisecond)
	deadline := time.Now().Add(time.Second)
	for !committed.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !committed.Load() {
		t.Fatalf("refused delivery was never retried behind the gate (attempts=%d)", attempts.Load())
	}
}

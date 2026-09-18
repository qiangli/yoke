package foreman

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestManagedObservationLivesOutsideBusyTurnMutex(t *testing.T) {
	if !ControlSupported() {
		t.Skip("native control transport unavailable")
	}
	old := ObserveSession
	t.Cleanup(func() { ObserveSession = old })
	started := make(chan struct{})
	stopped := make(chan struct{})
	var passes atomic.Int64
	ObserveSession = func(ctx context.Context, id, agent string) func() {
		if id != "sprint-138-manager" || agent != "manager" {
			t.Fatalf("bad hook identity %q %q", id, agent)
		}
		ctx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			defer close(done)
			close(started)
			ticker := time.NewTicker(time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					passes.Add(1)
				}
			}
		}()
		return func() { cancel(); <-done; close(stopped) }
	}
	s, err := Start(context.Background(), Options{ID: "sprint-138-manager", Goal: "fixture", Agent: "manager", Root: t.TempDir(), Runner: &stubRunner{}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan string, 1)
	finished := make(chan error, 1)
	go func() { finished <- s.ServeControl(ctx, ready) }()
	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatal("control not ready")
	}
	<-started
	s.mu.Lock()
	before := passes.Load()
	deadline := time.After(time.Second)
	for passes.Load() == before {
		select {
		case <-deadline:
			s.mu.Unlock()
			t.Fatal("observation blocked behind manager turn")
		case <-time.After(time.Millisecond):
		}
	}
	s.mu.Unlock()
	cancel()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("observation did not join during stop")
	}
	select {
	case <-stopped:
	default:
		t.Fatal("observation cleanup absent")
	}
}

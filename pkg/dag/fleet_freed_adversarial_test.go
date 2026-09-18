package dag

import (
	"context"
	"testing"
	"time"
)

func TestPoolWaitersHangWhenMultipleSlotsFreeConcurrently(t *testing.T) {
	pool := NewPool(nil, &Worker{ID: "w1", Venues: []string{VenueUserland}, CPU: 2})

	w1, rel1 := pool.TryAcquire(Constraints{})
	w2, rel2 := pool.TryAcquire(Constraints{})
	if w1 == nil || w2 == nil {
		t.Fatal("expected 2 slots")
	}

	waiter1Done := make(chan struct{})
	waiter2Done := make(chan struct{})

	go func() {
		pool.Acquire(context.Background(), Constraints{})
		close(waiter1Done)
	}()
	go func() {
		pool.Acquire(context.Background(), Constraints{})
		close(waiter2Done)
	}()

	time.Sleep(100 * time.Millisecond)

	pool.mu.Lock()
	pool.free[0] += 2
	pool.busy[0] -= 2
	select {
	case pool.freed <- struct{}{}:
	default:
	}
	select {
	case pool.freed <- struct{}{}:
	default:
	}
	pool.mu.Unlock()

	timeout := time.After(500 * time.Millisecond)
	waitersFinished := 0
	for waitersFinished < 2 {
		select {
		case <-waiter1Done:
			waitersFinished++
			waiter1Done = nil
		case <-waiter2Done:
			waitersFinished++
			waiter2Done = nil
		case <-timeout:
			t.Fatalf("DEFECT: A waiter hung forever because the freed channel dropped the second signal!")
		}
	}
	rel1()
	rel2()
}

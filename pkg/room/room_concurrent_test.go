package room

import (
	"os"
	"sync"
	"testing"
)

// TestJoinConcurrentWritersLeaveOneValidCard storms Join with concurrent
// writers for the same id and requires exactly one intact card afterwards.
// The claim's read-check-write holds the member-claims lock; on the racy
// version concurrent truncating writes tear each other's card file and the
// survivor is missing or unparseable.
func TestJoinConcurrentWritersLeaveOneValidCard(t *testing.T) {
	isolate(t)
	const n = 32
	errs := make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = Join(Card{ID: "storm", Binding: "codex:test", PID: os.Getpid()})
		}(i)
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("Join[%d]: %v", i, err)
		}
	}
	members, err := Members()
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, c := range members {
		if c.ID == "storm" {
			found++
			if c.Binding != "codex:test" {
				t.Fatalf("storm card = %+v, want binding codex:test", c)
			}
		}
	}
	if found != 1 {
		t.Fatalf("members with id storm = %d, want exactly 1", found)
	}
}

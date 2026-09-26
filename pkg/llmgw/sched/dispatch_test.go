package sched

import (
	"sync"
	"testing"
)

func freshSlotTable() *SlotTable {
	return NewSlotTable()
}

func TestSlotTable_AcquireUpToCeiling(t *testing.T) {
	tab := freshSlotTable()
	for i := 0; i < 3; i++ {
		if !tab.Acquire("h", "m", 3) {
			t.Fatalf("acquire %d should succeed", i)
		}
	}
	if tab.Acquire("h", "m", 3) {
		t.Errorf("4th acquire should fail (cap=3)")
	}
	if got := tab.InFlight("h", "m"); got != 3 {
		t.Errorf("in-flight=%d, want 3", got)
	}
}

func TestSlotTable_ReleaseFreesCapacity(t *testing.T) {
	tab := freshSlotTable()
	_ = tab.Acquire("h", "m", 2)
	_ = tab.Acquire("h", "m", 2)
	if tab.Acquire("h", "m", 2) {
		t.Fatal("3rd should fail")
	}
	tab.Release("h", "m")
	if !tab.Acquire("h", "m", 2) {
		t.Errorf("post-release acquire should succeed")
	}
}

func TestSlotTable_DistinctKeysIndependent(t *testing.T) {
	tab := freshSlotTable()
	_ = tab.Acquire("h1", "m1", 1)
	if !tab.Acquire("h2", "m1", 1) {
		t.Errorf("different backend should have its own counter")
	}
	if !tab.Acquire("h1", "m2", 1) {
		t.Errorf("different model on same backend should have its own counter")
	}
}

func TestSlotTable_ZeroCapFallsBackToDefault(t *testing.T) {
	tab := freshSlotTable()
	// maxParallel=0 means "unknown" — slot table uses
	// DefaultDispatchSlotMax (4) as the fallback.
	for i := 0; i < DefaultDispatchSlotMax; i++ {
		if !tab.Acquire("h", "m", 0) {
			t.Fatalf("acquire %d under default cap should succeed", i)
		}
	}
	if tab.Acquire("h", "m", 0) {
		t.Errorf("acquire past default cap should fail")
	}
}

func TestSlotTable_ReleaseClampsAtZero(t *testing.T) {
	tab := freshSlotTable()
	// Release without prior acquire should be a no-op (no negative
	// counters). Followed by acquire to confirm counter is still
	// zero-based.
	tab.Release("h", "m")
	tab.Release("h", "m")
	if got := tab.InFlight("h", "m"); got != 0 {
		t.Errorf("in-flight=%d, want 0 (release should clamp)", got)
	}
	if !tab.Acquire("h", "m", 1) {
		t.Errorf("acquire after spurious releases should still succeed")
	}
}

func TestSlotTable_ConcurrentAcquireRelease(t *testing.T) {
	tab := freshSlotTable()
	const cap = 8
	const workers = 50
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				if tab.Acquire("h", "m", cap) {
					tab.Release("h", "m")
				}
			}
		}()
	}
	wg.Wait()
	if got := tab.InFlight("h", "m"); got != 0 {
		t.Errorf("after balanced acquire/release, in-flight=%d, want 0", got)
	}
}

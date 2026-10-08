package bus

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

// TestPending_MarkReadRacingAppendLosesNothing is the concurrent-writer
// regression test for the bus.MarkRead data-loss race: MarkRead rewrites the
// subscriber's pending file while the sidecar appends to it with O_APPEND.
// A read-modify-write that is not serialized against those appends drops
// whatever landed between its read and its write — a silent notification
// loss that leaves an agent acting on stale assumptions.
//
// Every appended sequence must still be present afterwards (MarkRead retains;
// it only stamps ReadAt). On the racy version this fails with missing seqs.
func TestPending_MarkReadRacingAppendLosesNothing(t *testing.T) {
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	const sub = "race-sub"

	var seq atomic.Int64
	appendOne := func() {
		n := seq.Add(1)
		p := Pending{
			SchemaVersion: SchemaVersion,
			Seq:           n,
			TS:            "2026-10-08T00:00:00Z",
			Principal:     "tester",
			Topic:         "t",
			To:            sub,
			Body:          fmt.Sprintf("msg %d", n),
			Delivery:      DeliveryQueued,
		}
		if err := AppendPending(sub, p); err != nil {
			t.Errorf("AppendPending: %v", err)
		}
	}

	const rounds = 40
	const appendsPerRound = 5
	for r := 0; r < rounds; r++ {
		appendOne() // seed an unread item so the marker below always rewrites
		markThrough := seq.Load()
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			if err := MarkRead(sub, markThrough); err != nil {
				t.Errorf("MarkRead: %v", err)
			}
		}()
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < appendsPerRound; i++ {
				appendOne()
			}
		}()
		close(start)
		wg.Wait()
	}

	total := seq.Load()
	all, err := ReadPending(sub)
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[int64]int, len(all))
	for _, p := range all {
		seen[p.Seq]++
	}
	var missing []int64
	var duplicated []int64
	for n := int64(1); n <= total; n++ {
		switch seen[n] {
		case 0:
			missing = append(missing, n)
		case 1:
		default:
			duplicated = append(duplicated, n)
		}
	}
	if len(missing) > 0 || len(duplicated) > 0 {
		t.Fatalf("pending buffer lost data racing MarkRead vs append: total=%d stored=%d missing=%v duplicated=%v",
			total, len(all), missing, duplicated)
	}
}

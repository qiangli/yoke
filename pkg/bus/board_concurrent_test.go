package bus

import (
	"sync"
	"testing"
)

// TestBoard_ConcurrentPostsAssignUniqueSeq is the concurrent-writer
// regression test for the mb.PostMessage sequence race: the sequence is
// assigned as archivedThrough()+live-count+1 and is also the claims/views
// filename, so two posts sharing one seq collide on disk and one receipt
// record silently stands for both. The read-assign-append must hold the
// board lock; on the racy version this fails with duplicate seqs.
func TestBoard_ConcurrentPostsAssignUniqueSeq(t *testing.T) {
	boardInTempHome(t)
	const n = 50
	seqs := make([]int64, n)
	errs := make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			seqs[i], errs[i] = PostMessageSeq(Post{From: "tester", Body: "concurrent"})
		}(i)
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("PostMessageSeq[%d]: %v", i, err)
		}
	}
	seen := make(map[int64]int, n)
	for _, s := range seqs {
		seen[s]++
		if seen[s] > 1 {
			t.Fatalf("duplicate seq %d in %v", s, seqs)
		}
	}
	all, err := Posts()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != n {
		t.Fatalf("Posts() = %d, want %d (lost posts)", len(all), n)
	}
	for _, p := range all {
		if seen[p.Seq] != 1 {
			t.Fatalf("stored post seq %d not among assigned %v", p.Seq, seqs)
		}
	}
}

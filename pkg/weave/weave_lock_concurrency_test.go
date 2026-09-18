package weave

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// THE INCIDENT THESE TESTS PIN.
//
// weave held ONE coarse exclusive flock on queue.json across whole operations,
// including `weave pull --review-agent`, which runs a review agent subprocess
// and a suite gate — minutes. For that whole window every other weave command
// in the repo blocked: a steward could not read the board, file an issue, or
// merge anything while the autopilot ran, and the only observed escape was
// killing the autopilot. Supervision died so the queue could hold a lock.
//
// Each test below is one clause of the fix: reads never wait, writes hold the
// lock only around the mutation, concurrent adds never lose entries, and a
// second pull is refused promptly instead of blocking.

// withinDeadline runs fn and fails the test if it has not returned in time.
// It deliberately does NOT abandon fn — a leaked goroutine holding a lock
// would poison later tests; the failure message is enough.
func withinDeadline(t *testing.T, d time.Duration, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	start := time.Now()
	go func() { defer close(done); fn() }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s did not return within %s — a read/write path is blocking on a held lock", what, d)
	}
	if el := time.Since(start); el > d {
		t.Fatalf("%s took %s (limit %s)", what, el, d)
	}
}

// TestWeaveReadsDoNotBlockOnAHeldQueueLock is done-criterion 1: `weave list`
// and `weave status` must answer while a writer holds queue.lock. They read
// lock-free (saveWeaveQueue renames into place, so no reader can see a torn
// queue) and their reaper pass is opportunistic.
func TestWeaveReadsDoNotBlockOnAHeldQueueLock(t *testing.T) {
	root := weaveTestRepo(t)
	t.Chdir(root)
	if out, code := runWeave(t, "add", "a run to look at", "--json"); code != 0 {
		t.Fatalf("weave add failed (%d): %s", code, out)
	}
	dir, err := weaveQueueDir(root)
	if err != nil {
		t.Fatal(err)
	}

	// A writer holds the lock for longer than any read is allowed to take.
	release, err := weaveFlock(filepath.Join(dir, "queue.lock"), time.Second)
	if err != nil {
		t.Fatalf("take queue lock: %v", err)
	}
	held := make(chan struct{})
	go func() {
		<-held
		release()
	}()
	defer close(held)

	withinDeadline(t, time.Second, "weave list", func() {
		out, code := runWeave(t, "list", "--json")
		if code != 0 {
			t.Errorf("weave list must succeed while the lock is held (%d): %s", code, out)
		}
		if !strings.Contains(out, "a run to look at") {
			t.Errorf("weave list must show the queue it read lock-free:\n%s", out)
		}
	})
	withinDeadline(t, time.Second, "weave status", func() {
		if out, code := runWeave(t, "status", "1", "--json"); code != 0 {
			t.Errorf("weave status must succeed while the lock is held (%d): %s", code, out)
		}
	})
}

// TestConcurrentWeaveAddKeepsEveryEntry is done-criterion 3: two (here: many)
// callers adding at once must not lose or corrupt entries. Each add is a
// read-modify-write under the lock, so IDs must come out unique and every
// title must survive.
func TestConcurrentWeaveAddKeepsEveryEntry(t *testing.T) {
	root := weaveTestRepo(t)
	t.Chdir(root)
	dir, err := weaveQueueDir(root)
	if err != nil {
		t.Fatal(err)
	}

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			cmd := newWeaveCmd()
			var buf strings.Builder
			cmd.SetOut(&buf)
			cmd.SetErr(&buf)
			cmd.SetArgs([]string{"add", fmt.Sprintf("concurrent run %d", i), "--json"})
			if err := cmd.Execute(); err != nil {
				errs[i] = fmt.Errorf("add %d: %v: %s", i, err, buf.String())
			}
		}(i)
	}
	close(start)
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	q, err := readWeaveQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(q.Items) != n {
		t.Fatalf("concurrent adds lost entries: want %d items, got %d", n, len(q.Items))
	}
	seenID := map[int64]bool{}
	seenTitle := map[string]bool{}
	for _, it := range q.Items {
		if seenID[it.ID] {
			t.Fatalf("duplicate id %d — two adds claimed the same NextID", it.ID)
		}
		seenID[it.ID] = true
		seenTitle[it.Title] = true
	}
	for i := 0; i < n; i++ {
		if !seenTitle[fmt.Sprintf("concurrent run %d", i)] {
			t.Fatalf("add %d was silently dropped: %+v", i, q.Items)
		}
	}
}

// TestPullHoldsNoLocksWhileReviewGateRuns parks pull in its slow pre-commit
// phase and proves both locks remain available. In particular, acquiring
// pull.lock here pins the P0 fix: review/gate time is no longer merge-lock
// hold time.
func TestPullHoldsNoLocksWhileReviewGateRuns(t *testing.T) {
	root := weaveTestRepo(t)
	t.Chdir(root)
	if out, code := runWeave(t, "add", "first run", "--json"); code != 0 {
		t.Fatalf("weave add failed (%d): %s", code, out)
	}
	dir, err := weaveQueueDir(root)
	if err != nil {
		t.Fatal(err)
	}

	pause := filepath.Join(t.TempDir(), "pull-parked")
	if err := os.WriteFile(pause, []byte("park"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WEAVE_TEST_PULL_AFTER_LOAD_FILE", pause)

	pullDone := make(chan struct{})
	go func() {
		defer close(pullDone)
		runWeave(t, "pull", "--json")
	}()

	// Wait for the pull to reach its long phase.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(pause + ".ready"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = os.Remove(pause)
			<-pullDone
			t.Fatal("pull never reached its long phase")
		}
		time.Sleep(10 * time.Millisecond)
	}

	withinDeadline(t, 2*time.Second, "weave list during a pull", func() {
		if out, code := runWeave(t, "list", "--json"); code != 0 {
			t.Errorf("weave list must work mid-pull (%d): %s", code, out)
		}
	})
	withinDeadline(t, 2*time.Second, "weave add during a pull", func() {
		if out, code := runWeave(t, "add", "filed mid-pull", "--json"); code != 0 {
			t.Errorf("weave add must work mid-pull (%d): %s", code, out)
		}
	})

	lockStarted := time.Now()
	releasePull, err := weaveFlock(filepath.Join(dir, "pull.lock"), 0)
	if err != nil {
		t.Fatalf("pull.lock held during slow review/gate phase: %v", err)
	}
	lockElapsed := time.Since(lockStarted)
	releasePull()
	if lockElapsed > time.Second {
		t.Fatalf("taking pull.lock during review/gate took %s; want under 1s", lockElapsed)
	}

	if err := os.Remove(pause); err != nil {
		t.Fatal(err)
	}
	<-pullDone

	// The issue filed mid-pull survived the pull's write-back: pull persists
	// only the items IT changed, onto the queue as it stands at the end.
	q, err := readWeaveQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, it := range q.Items {
		if it.Title == "filed mid-pull" {
			found = true
		}
	}
	if !found {
		t.Fatalf("pull clobbered the issue filed while it ran: %+v", q.Items)
	}
}

// TestWriteBackChangedItemsPreservesConcurrentWrites pins the merge-back rule
// directly: a long operation working on a private copy writes back only the
// items it changed, so entries added — or edited — meanwhile survive.
func TestWriteBackChangedItemsPreservesConcurrentWrites(t *testing.T) {
	dir := t.TempDir()
	if err := withWeaveQueueLock(dir, func(q *weaveQueue) error {
		q.Items = []*weaveItem{
			{ID: 1, Title: "one", State: "submitted"},
			{ID: 2, Title: "two", State: "todo"},
		}
		q.NextID = 3
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// The long operation takes its private copy.
	work, err := readWeaveQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	before := weaveItemFingerprints(work)

	// ... and while it runs, another caller adds #3 and comments on #2.
	if err := withWeaveQueueLock(dir, func(q *weaveQueue) error {
		q.Items = append(q.Items, &weaveItem{ID: 3, Title: "filed meanwhile", State: "todo"})
		q.NextID = 4
		for _, it := range q.Items {
			if it.ID == 2 {
				it.Body = "a steward note"
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// The long operation finishes and records its outcome on #1 only.
	for _, it := range work.Items {
		if it.ID == 1 {
			it.State = "done"
		}
	}
	if err := weaveWriteBackChangedItems(dir, work, before); err != nil {
		t.Fatal(err)
	}

	q, err := readWeaveQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(q.Items) != 3 {
		t.Fatalf("write-back dropped items: %+v", q.Items)
	}
	byID := map[int64]*weaveItem{}
	for _, it := range q.Items {
		byID[it.ID] = it
	}
	if byID[1].State != "done" {
		t.Errorf("the operation's own outcome was not recorded: %+v", byID[1])
	}
	if byID[2].Body != "a steward note" {
		t.Errorf("write-back clobbered a concurrent edit to an untouched item: %+v", byID[2])
	}
	if byID[3] == nil || byID[3].Title != "filed meanwhile" {
		t.Errorf("write-back dropped an item added while the operation ran: %+v", q.Items)
	}
}

// TestQueueLockWaitIsBounded pins the safety valve: a writer waits, but not
// forever. A crashed holder can no longer wedge every weave command in the
// repo — the caller gets a retryable busy error instead.
func TestQueueLockWaitIsBounded(t *testing.T) {
	dir := t.TempDir()
	release, err := weaveFlock(filepath.Join(dir, "queue.lock"), time.Second)
	if err != nil {
		t.Fatalf("take lock: %v", err)
	}
	defer release()

	start := time.Now()
	err = withWeaveQueueLockWait(dir, 150*time.Millisecond, func(*weaveQueue) error {
		t.Error("fn must not run while another holder has the lock")
		return nil
	})
	if !errors.Is(err, errWeaveQueueBusy) {
		t.Fatalf("want errWeaveQueueBusy, got %v", err)
	}
	if el := time.Since(start); el > time.Second {
		t.Fatalf("the bounded wait waited %s — it is not bounded", el)
	}
	if !weaveIsBusy(err) {
		t.Error("weaveIsBusy must classify the busy error as retryable")
	}
}

func TestPullBusyErrorNamesMeasuredHolder(t *testing.T) {
	dir := t.TempDir()
	originalNow := weavePullNow
	t.Cleanup(func() { weavePullNow = originalNow })
	acquiredAt := time.Date(2026, 7, 27, 10, 43, 0, 0, time.Local)
	weavePullNow = func() time.Time { return acquiredAt }

	held := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- withWeaveNamedPullLock(dir, "run #169", func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	defer func() {
		close(release)
		if err := <-done; err != nil {
			t.Errorf("holder failed: %v", err)
		}
	}()
	weavePullNow = func() time.Time { return acquiredAt.Add(4*time.Minute + 12*time.Second) }

	err := withWeavePullLock(dir, func() error {
		t.Error("contender entered while holder owned pull.lock")
		return nil
	})
	if err == nil {
		t.Fatal("contender was admitted while pull.lock was held")
	}
	for _, want := range []string{"merge is already in progress", "run #169", "since 10:43", "4m12s ago", "retry when it finishes"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("busy error %q does not contain %q", err, want)
		}
	}
	if !errors.Is(err, errWeavePullBusy) {
		t.Fatalf("named busy error must retain retryable identity: %v", err)
	}
}

func TestPullBusyErrorDoesNotInventUnreadableHolder(t *testing.T) {
	dir := t.TempDir()
	held := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- withWeaveNamedPullLock(dir, "run #999", func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	// Holder metadata is diagnostic and lives beside the sentinel range inside
	// the stable lock file. Corrupting it must only degrade the busy message;
	// the kernel refusal remains authoritative.
	if err := os.WriteFile(filepath.Join(dir, "pull.lock"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	defer func() {
		close(release)
		if err := <-done; err != nil {
			t.Errorf("holder failed: %v", err)
		}
	}()

	err := withWeavePullLock(dir, func() error {
		t.Error("contender entered while holder owned pull.lock")
		return nil
	})
	if !errors.Is(err, errWeavePullBusy) {
		t.Fatalf("unreadable sidecar changed correct kernel refusal: %v", err)
	}
	if strings.Contains(err.Error(), "run #999") || strings.Contains(err.Error(), "since ") || strings.Contains(err.Error(), "pid ") {
		t.Fatalf("unreadable sidecar produced invented holder evidence: %v", err)
	}
}

func TestStalePullHolderNeverBlocksKernelAdmittedCaller(t *testing.T) {
	dir := t.TempDir()
	stale := []byte(`{"name":"run #1","pid":1,"intent":"merge","since":"2026-01-01T00:00:00Z"}`)
	if err := os.WriteFile(filepath.Join(dir, "pull.lock"), stale, 0o644); err != nil {
		t.Fatal(err)
	}
	called := false
	if err := withWeaveNamedPullLock(dir, "run #2", func() error {
		called = true
		return nil
	}); err != nil {
		t.Fatalf("stale diagnostics blocked a kernel-admitted caller: %v", err)
	}
	if !called {
		t.Fatal("kernel-admitted pull callback did not run")
	}
}

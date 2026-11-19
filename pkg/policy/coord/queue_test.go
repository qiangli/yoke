package coord

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/principal"
)

func queueOwner(name string) principal.Ref { return principal.Ref{Name: name, Episode: name} }
func waitRows(t *testing.T, dir string, n int) []QueueRow {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		rows, err := Queue(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) == n {
			return rows
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("queue did not reach %d rows", n)
	return nil
}

func TestQueueFIFOAndReleaseAuthority(t *testing.T) {
	dir := ledger(t)
	s := store{dir}
	ctx := context.Background()
	a, b, c := queueOwner("holder"), queueOwner("second"), queueOwner("third")
	ref := ParseRef("test-host")
	first, err := AcquireRef(ctx, Request{Ref: ref, Holder: a})
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		g   Grant
		err error
	}
	second, third := make(chan result, 1), make(chan result, 1)
	go func() {
		g, e := AcquireRef(ctx, Request{Ref: ref, Holder: b, Intent: "candidate-B", Wait: 3 * time.Second})
		second <- result{g, e}
	}()
	rows := waitRows(t, dir, 1)
	if rows[0].Blocker == nil || rows[0].Blocker.Epoch != first.Epoch || !strings.Contains(rows[0].Reason, "holder") {
		t.Fatalf("missing holder: %+v", rows)
	}
	go func() {
		g, e := AcquireRef(ctx, Request{Ref: ref, Holder: c, Intent: "candidate-C", Wait: 3 * time.Second})
		third <- result{g, e}
	}()
	rows = waitRows(t, dir, 2)
	if rows[1].Position != 2 || rows[1].Ahead != rows[0].ID {
		t.Fatalf("bad order: %+v", rows)
	}
	if err := ReleaseRef(ctx, ref, b, first.Epoch); !isConflict(err) {
		t.Fatalf("foreign release: %v", err)
	}
	if err := ReleaseRef(ctx, ref, a, first.Epoch); err != nil {
		t.Fatal(err)
	}
	got := <-second
	if got.err != nil {
		t.Fatal(got.err)
	}
	select {
	case r := <-third:
		t.Fatalf("third jumped holder: %+v", r)
	default:
	}
	if got.g.Epoch <= first.Epoch {
		t.Fatal("epoch did not advance")
	}
	if err := ReleaseRef(ctx, ref, a, first.Epoch); !errors.Is(err, ErrFenced) {
		t.Fatalf("old release: %v", err)
	}
	if err := s.release(ref, b, got.g.Epoch); err != nil {
		t.Fatal(err)
	}
	last := <-third
	if last.err != nil {
		t.Fatal(last.err)
	}
	if !SameHolder(last.g.Claim.Holder, c) {
		t.Fatal("wrong final holder")
	}
	waitRows(t, dir, 0)
}

func TestQueueCancellationWhileAttachedHolderLives(t *testing.T) {
	dir := ledger(t)
	ctx := context.Background()
	ref := ParseRef("browser")
	a, b := queueOwner("browser-owner"), queueOwner("waiting-owner")
	g, l, err := AcquireAttachedRef(ctx, Request{Ref: ref, Holder: a})
	if err != nil {
		t.Fatal(err)
	}
	defer ReleaseAttached(dir, g.Claim, l)
	result := make(chan error, 1)
	go func() {
		_, lock, e := AcquireAttachedRef(ctx, Request{Ref: ref, Holder: b, Wait: 5 * time.Second})
		if lock != nil {
			lock.Release()
		}
		result <- e
	}()
	rows := waitRows(t, dir, 1)
	if err := CancelWait(dir, rows[0].ID, a); err == nil {
		t.Fatal("foreign cancellation succeeded")
	}
	if err := CancelWait(dir, rows[0].ID, b); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if !errors.Is(err, ErrQueueCanceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation waited for live holder")
	}
	claims, err := List(dir)
	if err != nil || len(claims) != 1 || !SameHolder(claims[0].Holder, a) {
		t.Fatalf("cancel affected hold: %v %v", claims, err)
	}
}

func TestQueueContextCancelAndTimeout(t *testing.T) {
	for _, attached := range []bool{false, true} {
		t.Run(map[bool]string{false: "lease", true: "attached"}[attached], func(t *testing.T) {
			dir := ledger(t)
			ref := ParseRef("gpu")
			a, b := queueOwner("a"), queueOwner("b")
			_, err := AcquireRef(context.Background(), Request{Ref: ref, Holder: a})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				r := Request{Ref: ref, Holder: b, Wait: time.Minute}
				if attached {
					_, l, e := AcquireAttachedRef(ctx, r)
					if l != nil {
						l.Release()
					}
					done <- e
				} else {
					_, e := AcquireRef(ctx, r)
					done <- e
				}
			}()
			waitRows(t, dir, 1)
			cancel()
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			waitRows(t, dir, 0)
			_, err = AcquireRef(context.Background(), Request{Ref: ref, Holder: b, Wait: 25 * time.Millisecond})
			if err == nil {
				t.Fatal("timeout acquired")
			}
			waitRows(t, dir, 0)
		})
	}
}

func TestQueueStaleAndUnknownOwnership(t *testing.T) {
	dir := ledger(t)
	s := store{dir}
	a, b := queueOwner("old"), queueOwner("new")
	ref := ParseRef("account-allowance")
	g, err := s.acquire(acquireSpec{req: Request{Ref: ref, Holder: a}})
	if err != nil {
		t.Fatal(err)
	}
	// Unknown ownership is not proof of vacancy.
	g.Claim.Heartbeat = time.Time{}
	g.Claim.AcquiredAt = time.Time{}
	if err := writeClaim(keyedClaimPath(dir, ref), g.Claim); err != nil {
		t.Fatal(err)
	}
	if _, err := s.acquire(acquireSpec{req: Request{Ref: ref, Holder: b}}); !isConflict(err) {
		t.Fatalf("unknown stolen: %v", err)
	}
	g.Claim.Heartbeat = time.Now().Add(-2 * TTL)
	g.Claim.AcquiredAt = g.Claim.Heartbeat
	if err := writeClaim(keyedClaimPath(dir, ref), g.Claim); err != nil {
		t.Fatal(err)
	}
	next, err := s.acquireWait(context.Background(), acquireSpec{req: Request{Ref: ref, Holder: b}}, time.Second)
	if err != nil || next.Epoch <= g.Epoch {
		t.Fatalf("stale recovery: %+v %v", next, err)
	}
}

func TestQueueOverlapNoBargingAndExpiredWaiter(t *testing.T) {
	dir := ledger(t)
	s := store{dir}
	a, b := queueOwner("a"), queueOwner("b")
	root := t.TempDir()
	sp := acquireSpec{req: Request{Ref: Ref{Kind: "path", Name: root}, Holder: a}}
	id, err := s.enqueue(sp, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.acquire(acquireSpec{req: Request{Ref: Ref{Kind: "path", Name: filepath.Join(root, "child")}, Holder: b}})
	var blocked *QueueBlocked
	if !errors.As(err, &blocked) || blocked.ID != id {
		t.Fatalf("barged: %v", err)
	}
	// An unrelated resource is independent of the path queue.
	if _, err = s.acquire(acquireSpec{req: Request{Ref: ParseRef("gpu-other"), Holder: b}}); err != nil {
		t.Fatal(err)
	}
	if err = CancelWait(dir, id, a); err != nil {
		t.Fatal(err)
	}
	if _, err = s.enqueue(sp, time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err = s.acquire(acquireSpec{req: Request{Ref: Ref{Kind: "path", Name: root}, Holder: b}}); err != nil {
		t.Fatal(err)
	}
}

func TestQueueCLIAndCorruption(t *testing.T) {
	dir := ledger(t)
	s := store{dir}
	owner := Self()
	id, err := s.enqueue(acquireSpec{req: Request{Ref: ParseRef("test-host"), Holder: owner, Intent: "candidate-A"}}, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := runClaim(context.Background(), "queue", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var rows []QueueRow
	if err := json.Unmarshal([]byte(out), &rows); err != nil || len(rows) != 1 || rows[0].Intent != "candidate-A" {
		t.Fatalf("queue JSON: %s %v", out, err)
	}
	if _, _, err = runClaim(context.Background(), "queue", "cancel", id); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "waiters.queue"), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = s.acquire(acquireSpec{req: Request{Ref: ParseRef("test-host"), Holder: owner}}); err == nil {
		t.Fatal("corrupt queue failed open")
	}
}

func TestQueueRefusalIsGuardConflict(t *testing.T) {
	dir := ledger(t)
	s := store{dir}
	owner := queueOwner("waiting")
	_, err := s.enqueue(acquireSpec{req: Request{Ref: ParseRef("host"), Holder: owner}}, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.acquire(acquireSpec{req: Request{Ref: ParseRef("host"), Holder: queueOwner("newcomer")}})
	var conflict *Conflict
	if !errors.As(err, &conflict) {
		t.Fatalf("guard would fail open: %v", err)
	}
	if strings.Contains(conflict.Error(), "already holds") || !strings.Contains(conflict.Error(), "waiting ahead") {
		t.Fatal(conflict.Error())
	}
	b, err := conflict.JSON()
	if err != nil || !strings.Contains(string(b), `"state": "queued"`) {
		t.Fatalf("misleading refusal: %s %v", b, err)
	}
}

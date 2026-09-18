package weave

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/qiangli/coreutils/pkg/lockfile"
)

// LOCK DISCIPLINE — why this file exists.
//
// weave used to hold ONE coarse exclusive flock on <queueDir>/queue.lock across
// whole operations, including the multi-minute ones: `weave pull
// --review-agent` ran an adversarial-review agent subprocess and a suite gate
// INSIDE the lock. Everything else in the repo — `weave list`, `weave show`,
// `weave add`, another `weave pull` — then blocked for the entire cycle. A
// steward could not read the board or file an issue while the autopilot ran,
// and the only escape found in practice was killing the autopilot. That is a
// governance failure, not a performance nit.
//
// The discipline now:
//
//   - READS ARE LOCK-FREE. saveWeaveQueue writes a temp file and renames it,
//     so every reader either sees the whole old queue or the whole new one —
//     never a torn one. A read therefore never needs to wait for a writer.
//     readWeaveQueue is that path, and it is what `weave list`/`show` use.
//   - WRITES HOLD THE LOCK ONLY AROUND THE MUTATION. load → mutate → save,
//     measured in milliseconds. Any long-running work (agent subprocess, suite
//     gate, git merge) happens OUTSIDE the lock and re-acquires it briefly to
//     record its outcome onto the freshly re-read queue.
//   - MAINTENANCE PASSES ARE OPPORTUNISTIC. The reaper runs on read paths; if
//     the lock is momentarily held it skips this round rather than blocking a
//     `weave list`. It is idempotent, so the next read reaps.
//   - LONG EXCLUSIVE OPERATIONS TAKE THEIR OWN NON-BLOCKING LOCK. `weave pull`
//     merges into the shared checkout, so two of them must not overlap — but a
//     second caller gets an immediate "busy, retry" instead of an indefinite
//     block. That is pull.lock, not queue.lock.

// errWeaveQueueBusy is the SENTINEL every bounded weave lock returns when its
// wait expires. Callers either report it (write paths) or degrade to a
// lock-free read (maintenance paths).
//
// Its message is deliberately lock-NEUTRAL. weaveFlock is now shared by
// queue.lock, pull.lock and cooldown.lock, so a message naming the queue would
// be printed verbatim to an operator who actually lost a race for cooldown.lock
// — sending them to inspect a lock nobody holds. Callers get the specific name
// via weaveLockBusy; the sentinel exists for errors.Is, not for reading.
var errWeaveQueueBusy = errors.New("weave: lock busy — another weave command holds it; retry")

// weaveLockBusy reports which lock was actually contended while staying
// errors.Is-compatible with errWeaveQueueBusy, so every existing check keeps
// working unchanged.
type weaveLockBusy struct {
	lock  string
	cause error
}

func (e weaveLockBusy) Error() string {
	return fmt.Sprintf("weave: %s is held by another weave command; retry", e.lock)
}

func (e weaveLockBusy) Unwrap() []error { return []error{errWeaveQueueBusy, e.cause} }

// errWeavePullBusy is returned when another merge/pull already owns this
// repo's pull lock. A pull mutates the shared live checkout, so it is
// genuinely exclusive; reporting busy immediately is the try-lock contract.
var errWeavePullBusy = errors.New("weave: a merge is already in progress")

var errWeavePullStale = errors.New("weave: merge target moved while review/gate ran")

var weavePullNow = time.Now
var weaveTestObservePullLockHeld func(time.Duration)

type weavePullHeld struct {
	message string
	cause   error
}

func (e weavePullHeld) Error() string   { return e.message }
func (e weavePullHeld) Unwrap() []error { return []error{errWeavePullBusy, e.cause} }

func weavePullBusyError(err error) error {
	info, ok := lockfile.HeldBy(err)
	if !ok || info.Since.IsZero() || (info.Name == "" && info.PID <= 0) {
		return weavePullHeld{message: errWeavePullBusy.Error() + " — retry when it finishes", cause: err}
	}
	holder := strings.TrimSpace(info.Name)
	if holder == "" {
		holder = fmt.Sprintf("pid %d", info.PID)
	}
	since := info.Since.Local().Format("15:04")
	age := weavePullNow().Sub(info.Since)
	if age < 0 {
		age = 0
	}
	return weavePullHeld{
		message: fmt.Sprintf("%s (%s, since %s, %s ago) — retry when it finishes",
			errWeavePullBusy, holder, since, age.Round(time.Second)),
		cause: err,
	}
}

// withWeaveNamedPullLock adds human-readable ownership to the shared lock.
// The holder record remains diagnostic: it is written only after the kernel
// grants the lock and is never consulted to grant entry.
func withWeaveNamedPullLock(dir, holder string, fn func() error) error {
	return withWeavePullLockHolder(dir, holder, func() error {
		started := time.Now()
		defer func() {
			if weaveTestObservePullLockHeld != nil {
				weaveTestObservePullLockHeld(time.Since(started))
			}
		}()
		return fn()
	})
}

// weaveQueueLockWait bounds how long an ordinary write waits for the lock. It
// is a safety valve against a crashed-but-not-yet-reaped holder, not a tuning
// knob: with long operations moved out of the lock, real contention is
// milliseconds. A var so tests can shorten it.
var weaveQueueLockWait = 120 * time.Second

// weaveReapLockWait is the reaper's patience on read paths. Short by design:
// `weave list` must return promptly even mid-merge, and a skipped reap costs
// nothing because the pass is idempotent.
var weaveReapLockWait = 250 * time.Millisecond

// withWeaveQueueLockWait takes the exclusive queue lock (waiting at most
// wait), loads the queue, hands it to fn for mutation, saves it back, then
// releases. This is the ONLY sanctioned read-modify-write path.
//
// fn must be short. Anything that shells out to an agent, runs a suite gate or
// merges must happen outside this call and re-enter it to record the outcome —
// see the lock-discipline note above.
func withWeaveQueueLockWait(dir string, wait time.Duration, fn func(*weaveQueue) error) error {
	l, err := lockfile.AcquireWithin(filepath.Join(dir, "queue.lock"), wait, lockfile.Holder{
		Name: "weave-queue", PID: os.Getpid(), Intent: "update queue", Since: time.Now(),
	})
	if err != nil {
		if errors.Is(err, lockfile.ErrHeld) {
			return weaveLockBusy{lock: "queue.lock", cause: err}
		}
		return fmt.Errorf("queue %w", err)
	}
	defer l.Release()

	q, err := loadWeaveQueue(dir)
	if err != nil {
		return fmt.Errorf("queue lock: load: %w", err)
	}
	if err := fn(q); err != nil {
		return err
	}
	if err := saveWeaveQueue(dir, q); err != nil {
		return fmt.Errorf("queue lock: save: %w", err)
	}
	return nil
}

// withWeaveQueueLock is the ordinary write path: bounded wait, then
// load/mutate/save. See withWeaveQueueLockWait.
func withWeaveQueueLock(dir string, fn func(*weaveQueue) error) error {
	return withWeaveQueueLockWait(dir, weaveQueueLockWait, fn)
}

// withWeavePullLock guards the genuinely exclusive part of a pull: merging
// into the ONE shared live checkout. Non-blocking on purpose — a second caller
// is told to retry instead of overlapping the short live merge commit.
// It does NOT hold queue.lock, so `weave list`/`add`/`comment` stay live for
// the whole merge cycle.
func withWeavePullLock(dir string, fn func() error) error {
	return withWeavePullLockHolder(dir, "weave-pull", fn)
}

func withWeavePullLockHolder(dir, holder string, fn func() error) error {
	l, err := lockfile.TryAcquire(filepath.Join(dir, "pull.lock"), lockfile.Holder{
		Name: holder, PID: os.Getpid(), Intent: "merge", Since: weavePullNow(),
	})
	if err != nil {
		if errors.Is(err, lockfile.ErrHeld) {
			return weavePullBusyError(err)
		}
		return fmt.Errorf("pull %w", err)
	}
	defer l.Release()
	return fn()
}

// withWeaveCooldownLock serialises cooldown read-modify-write on its own
// bounded lock, separate from queue.lock so a best-effort cooldown update never
// contends with a queue state transition.
func withWeaveCooldownLock(dir string, fn func(*toolCooldowns) error) error {
	l, err := lockfile.AcquireWithin(filepath.Join(dir, "cooldown.lock"), weaveQueueLockWait, lockfile.Holder{
		Name: "weave-cooldown", PID: os.Getpid(), Intent: "update cooldown", Since: time.Now(),
	})
	if err != nil {
		if errors.Is(err, lockfile.ErrHeld) {
			err = weaveLockBusy{lock: "cooldown.lock", cause: err}
		}
		return fmt.Errorf("cooldown %w", err)
	}
	defer l.Release()

	tc := loadToolCooldowns(dir)
	if err := fn(&tc); err != nil {
		return err
	}
	return saveToolCooldowns(dir, tc)
}

// readWeaveQueue is the LOCK-FREE read path onto the queue. It exists to make
// the contract explicit at call sites: reads never take queue.lock, because
// saveWeaveQueue's write-temp-then-rename makes every visible queue.json a
// complete one. Never pair it with a write — that is a lost-update race; use
// withWeaveQueueLock for read-modify-write.
func readWeaveQueue(dir string) (*weaveQueue, error) { return loadWeaveQueue(dir) }

// weaveIsBusy reports whether err is a lock-contention refusal rather than a
// real failure. Callers surface it as a retryable state conflict.
func weaveIsBusy(err error) bool {
	return errors.Is(err, errWeaveQueueBusy) || errors.Is(err, errWeavePullBusy)
}

// weaveItemFingerprints records each item's serialized form, keyed by ID.
// Paired with weaveWriteBackChangedItems it lets a long operation work on a
// private copy of the queue and then persist ONLY what it actually changed —
// the alternative, writing the whole stale copy back, silently drops every
// `weave add` / `weave comment` that landed while the operation ran.
func weaveItemFingerprints(q *weaveQueue) map[int64]string {
	fp := make(map[int64]string, len(q.Items))
	if q == nil {
		return fp
	}
	for _, it := range q.Items {
		if b, err := json.Marshal(it); err == nil {
			fp[it.ID] = string(b)
		}
	}
	return fp
}

// weaveWriteBackChangedItems re-acquires the queue lock briefly and copies the
// items whose content differs from their fingerprint onto the queue as it
// stands on disk NOW. Items the caller never touched are left alone, and items
// that appeared meanwhile are preserved.
//
// An item the caller changed but that has since vanished from disk (abandoned,
// reset) is not resurrected: it is gone by a deliberate act, and re-adding it
// would undo that.
func weaveWriteBackChangedItems(dir string, work *weaveQueue, before map[int64]string) error {
	if work == nil {
		return nil
	}
	changed := make(map[int64]*weaveItem)
	for _, it := range work.Items {
		b, err := json.Marshal(it)
		if err != nil {
			continue
		}
		if prev, ok := before[it.ID]; !ok || prev != string(b) {
			cp := *it
			changed[it.ID] = &cp
		}
	}
	if len(changed) == 0 {
		return nil
	}
	return withWeaveQueueLock(dir, func(fresh *weaveQueue) error {
		for i, it := range fresh.Items {
			if upd, ok := changed[it.ID]; ok {
				cp := *upd
				fresh.Items[i] = &cp
			}
		}
		return nil
	})
}

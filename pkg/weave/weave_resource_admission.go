package weave

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/qiangli/coreutils/pkg/lockfile"
	"github.com/qiangli/yoke/pkg/llmbudget"
	"github.com/spf13/cobra"
)

// WeaveResourceDemand describes one impending wrapper launch. An external
// harness cannot promise a finite token/spend total; those estimates are unknown.
type WeaveResourceDemand struct {
	Run, Queue, Model, Agent, Workspace string
	MemoryBytes                         uint64
}

// WeaveResourceHooks keeps native host observation outside weave (resources
// already imports weave). CheckHost must use the existing shared cache.
type WeaveResourceHooks struct {
	LookupIdentity   func(context.Context, int) (string, error)
	CheckHost        func(context.Context, WeaveResourceDemand) error
	RequireHostCheck bool
	Budget           *llmbudget.Gate
}
type weaveResourceKey struct{}

func WithWeaveResources(cmd *cobra.Command, hooks WeaveResourceHooks) {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	cmd.SetContext(context.WithValue(ctx, weaveResourceKey{}, hooks))
}
func weaveResourceHooks(ctx context.Context) WeaveResourceHooks {
	hooks, _ := ctx.Value(weaveResourceKey{}).(WeaveResourceHooks)
	if os.Getenv("BASHY_HOST_ADMISSION_POLICY") != "" {
		hooks.RequireHostCheck = true
	}
	return hooks
}
func weaveRunLifecycleLockPath(dir string, id int64) string {
	return filepath.Join(dir, "run-"+strconv.FormatInt(id, 10)+".lifecycle.lock")
}

func weaveRunLifecycleLock(dir string, id int64) (*lockfile.Lock, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	return lockfile.TryAcquire(weaveRunLifecycleLockPath(dir, id), lockfile.Holder{Intent: "weave run lifecycle"})
}

type weaveAdmission struct {
	ctx     context.Context
	cancel  context.CancelFunc
	gate    *llmbudget.Gate
	owner   *llmbudget.OwnerLease
	request llmbudget.Request
	startID string
	done    chan struct{}
}

func beginWeaveAdmission(ctx context.Context, hooks WeaveResourceHooks, demand WeaveResourceDemand) (*weaveAdmission, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if hooks.RequireHostCheck && hooks.CheckHost == nil {
		return nil, errors.New("resource admission queued: host pressure policy requires an available host observer")
	}
	if hooks.CheckHost != nil {
		checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := hooks.CheckHost(checkCtx, demand)
		cancel()
		if err != nil {
			return nil, fmt.Errorf("resource admission queued: %w", err)
		}
	}
	startID := ""
	if hooks.LookupIdentity != nil {
		lookupCtx, cancel := context.WithTimeout(ctx, time.Second)
		id, err := hooks.LookupIdentity(lookupCtx, os.Getpid())
		cancel()
		if err != nil || id == "" {
			return nil, fmt.Errorf("resource admission queued: wrapper birth identity unavailable: %v", err)
		}
		startID = id
	}
	gate := hooks.Budget
	if gate == nil {
		gate = llmbudget.DefaultFromEnv()
	}
	owner, err := gate.NewOwner(ctx, "weave:"+demand.Run)
	if err != nil {
		return nil, err
	}
	host, err := os.Hostname()
	if err != nil {
		owner.Close()
		return nil, err
	}
	req := llmbudget.Request{ID: "weave-" + owner.ID(), Owner: owner.ID(), Run: demand.Run, Host: host, Model: demand.Model, Agent: demand.Agent, HostSlots: 1, MemoryBytes: demand.MemoryBytes, TTL: 2 * time.Minute, UnknownMemory: demand.MemoryBytes == 0}
	if demand.Model != "" {
		req.Concurrency = 1
		req.UnknownTokens = true
	} // Opaque harness totals are unknown; hard token budgets fail closed
	admission, err := gate.Reserve(ctx, req)
	if err != nil || admission.Reservation == nil || admission.Decision.Action != llmbudget.Allow {
		owner.Close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("resource admission %s: %s", admission.Decision.Action, admission.Decision.Reason)
	}
	workCtx, cancel := context.WithCancel(ctx)
	a := &weaveAdmission{ctx: workCtx, cancel: cancel, gate: gate, owner: owner, request: admission.Reservation.Request, startID: startID, done: make(chan struct{})}
	go func() {
		defer close(a.done)
		timer := time.NewTicker(30 * time.Second)
		defer timer.Stop()
		for {
			select {
			case <-workCtx.Done():
				return
			case <-timer.C:
				renewCtx, cancel := context.WithTimeout(workCtx, 5*time.Second)
				err := gate.Renew(renewCtx, req.ID, req.Owner, 2*time.Minute)
				cancel()
				if err != nil {
					a.cancel()
					return
				}
			}
		}
	}()
	return a, nil
}

// finish never infers termination from TTL or owner death. Cancellation and
// uncertain child survival retain the reservation for verified reconciliation.
func (a *weaveAdmission) finish(terminated bool, launched bool) error {
	if a == nil {
		return nil
	}
	a.cancel()
	<-a.done
	defer a.owner.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if !launched {
		return a.gate.Release(ctx, a.request.ID, a.request.Owner)
	}
	if !terminated {
		return nil
	}
	return a.gate.ReconcileTerminated(ctx, a.request.ID, llmbudget.TerminationProof{Owner: a.request.Owner, Run: a.request.Run, Host: a.request.Host, VerifiedAt: time.Now().UTC(), Evidence: "weave wrapper synchronously waited for its owned child"})
}
func weaveRecordAdmission(it *weaveItem, a *weaveAdmission) {
	it.ResourceTerminated = false
	it.ResourceVerifiedAt = time.Time{}
	it.ChildPID, it.ChildGroup, it.ChildStartID = 0, 0, ""
	it.PauseRequestedBy = ""
	it.PauseReason = ""
	it.WrapperStartID = a.startID
	it.ResourceReservationID = a.request.ID
	it.ResourceReservationOwner = a.request.Owner
}
func weaveRecordOwnedChild(ctx context.Context, dir string, run int64, cmd *exec.Cmd, a *weaveAdmission) error {
	if cmd.Process == nil {
		return errors.New("child process did not start")
	}
	hooks := weaveResourceHooks(ctx)
	if hooks.LookupIdentity == nil {
		// Direct yoke callers without the native observer can still launch.
		// They cannot later reconcile an orphan from a missing birth record.
		return nil
	}
	group, err := weaveOwnedChildGroup(cmd.Process.Pid)
	if err != nil {
		// A very short child can exit before inspection. Its synchronous Wait
		// still governs normal settlement, but an orphan cannot be reconciled.
		return nil
	}
	if group == 0 {
		return nil
	} // Windows has no portable group lifetime proof.
	lookupCtx, cancel := context.WithTimeout(ctx, time.Second)
	start, err := hooks.LookupIdentity(lookupCtx, cmd.Process.Pid)
	cancel()
	if err != nil || start == "" {
		return nil
	}
	return withWeaveQueueLock(dir, func(q *weaveQueue) error {
		it := findWeaveItem(q, run)
		if it == nil || it.ResourceReservationID != a.request.ID || it.State != "working" {
			return errors.New("run ownership changed before child registration")
		}
		it.ChildPID, it.ChildStartID, it.ChildGroup = cmd.Process.Pid, start, group
		return nil
	})
}

// Reconciliation is supported only for a recorded isolated child group. A
// vanished wrapper, expired lease, or an empty process listing is no proof.
func weaveReconcileOwnedTree(ctx context.Context, dir string, run int64) error {
	q, err := loadWeaveQueue(dir)
	if err != nil {
		return err
	}
	it := findWeaveItem(q, run)
	if it == nil {
		return fmt.Errorf("run #%d not found", run)
	}
	if it.ResourceReservationID == "" || it.ResourceTerminated {
		return nil
	}
	if it.WrapperPid > 0 && pidAlive(it.WrapperPid) {
		return errors.New("wrapper may still be active; retry after it exits")
	}
	if err := weaveRecordedGroupStopped(it); err != nil {
		return fmt.Errorf("run #%d reservation retained: %w", run, err)
	}
	verifiedAt := it.ResourceVerifiedAt
	if verifiedAt.IsZero() {
		verifiedAt = time.Now().UTC()
		if err := withWeaveQueueLock(dir, func(q *weaveQueue) error {
			cur := findWeaveItem(q, run)
			if cur == nil || cur.ResourceReservationID != it.ResourceReservationID || cur.ChildPID != it.ChildPID || cur.ChildStartID != it.ChildStartID || cur.ChildGroup != it.ChildGroup {
				return errors.New("run identity changed during reconciliation")
			}
			if cur.ResourceVerifiedAt.IsZero() {
				cur.ResourceVerifiedAt = verifiedAt
			} else {
				verifiedAt = cur.ResourceVerifiedAt
			}
			return nil
		}); err != nil {
			return err
		}
	}
	gate := weaveResourceHooks(ctx).Budget
	if gate == nil {
		gate = llmbudget.DefaultFromEnv()
	}
	host, err := os.Hostname()
	if err != nil {
		return err
	}
	proof := llmbudget.TerminationProof{Owner: it.ResourceReservationOwner, Run: filepath.Join(dir, strconv.FormatInt(run, 10)), Host: host, VerifiedAt: verifiedAt, Evidence: fmt.Sprintf("recorded child pid %d birth %s isolated group %d absent", it.ChildPID, it.ChildStartID, it.ChildGroup)}
	if err := gate.ReconcileTerminated(ctx, it.ResourceReservationID, proof); err != nil {
		return fmt.Errorf("run #%d reservation retained: %w", run, err)
	}
	return withWeaveQueueLock(dir, func(q *weaveQueue) error {
		cur := findWeaveItem(q, run)
		if cur == nil || cur.ResourceReservationID != it.ResourceReservationID || cur.ChildPID != it.ChildPID || cur.ChildStartID != it.ChildStartID || cur.ChildGroup != it.ChildGroup {
			return errors.New("run identity changed during reconciliation; inspect queue before retry")
		}
		cur.ResourceTerminated = true
		return nil
	})
}
func weaveVerifiedWrapper(ctx context.Context, it *weaveItem) error {
	if it.WrapperPid <= 0 || it.WrapperStartID == "" {
		return errors.New("wrapper identity unknown; cannot control legacy or unverified work")
	}
	hooks := weaveResourceHooks(ctx)
	if hooks.LookupIdentity == nil {
		return errors.New("native wrapper identity lookup unavailable")
	}
	id, err := hooks.LookupIdentity(ctx, it.WrapperPid)
	if err != nil {
		return err
	}
	if id != it.WrapperStartID {
		return errors.New("wrapper PID was reused; refusing control")
	}
	return nil
}

func weaveControlOwned(dir string, q *weaveQueue, it *weaveItem) bool {
	actor, known := weaveConductorIdentity("")
	return known && actor != "" && weaveOwnerFor(dir, q, it) == actor
}

func weaveWaitOwnedChildTerminated(cmd *exec.Cmd) bool {
	deadline := time.Now().Add(time.Second)
	for {
		if weaveOwnedChildTerminated(cmd) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

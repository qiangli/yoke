package dag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/qiangli/coreutils/pkg/lockfile"
	"github.com/qiangli/yoke/pkg/llmbudget"
	"os"
	"path/filepath"
	"time"
)

type capacityReceipt struct {
	Version        int                         `json:"version"`
	Run            string                      `json:"run"`
	Owner          string                      `json:"owner"`
	Worker         string                      `json:"worker"`
	BootID         string                      `json:"boot_id"`
	PID            int                         `json:"pid"`
	StartID        string                      `json:"start_id"`
	BoundedBody    bool                        `json:"bounded_body"`
	ReleasePending bool                        `json:"release_pending"`
	Released       bool                        `json:"released"`
	Proof          *llmbudget.TerminationProof `json:"proof,omitempty"`
}

func capacityStateDir(worker string) string {
	return filepath.Join(filepath.Dir(CapacityPolicyPath()), "remote-capacity", worker)
}
func capacityReceiptPath(worker, run string) string {
	return filepath.Join(capacityStateDir(worker), "runs", capacityOutputDigest(run)+".json")
}
func capacityGate(t CapacityTarget) *llmbudget.Gate {
	return llmbudget.New(llmbudget.Config{StatePath: filepath.Join(capacityStateDir(t.Worker), "meter.json"), Policy: &llmbudget.Policy{Version: 1, Constraints: []llmbudget.Constraint{{Host: t.Worker, HostSlots: &t.Slots, MemoryBytes: &t.MemoryBytes}}}})
}
func writeCapacityReceipt(r capacityReceipt) error {
	path := capacityReceiptPath(r.Worker, r.Run)
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return e
	}
	b, e := json.Marshal(r)
	if e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(path), ".receipt-*")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	if _, e = f.Write(b); e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e != nil {
		return e
	}
	if ce != nil {
		return ce
	}
	return os.Rename(f.Name(), path)
}
func reconcileCapacity(ctx context.Context, t CapacityTarget, run string, s CapacityServices) (*CapacityReply, error) {
	reply := &CapacityReply{Version: 1, Worker: t.Worker, Decision: "retained", Reason: "verify receiver boot changed, then retry dag capacity reconcile --target " + t.Name + " --run " + run}
	if !t.Dispatch || run == "" {
		return nil, errors.New("reconciliation requires explicit dispatch authority and run")
	}
	path := capacityReceiptPath(t.Worker, run)
	lock, e := lockfile.TryAcquire(path+".lock", lockfile.Holder{Name: "capacity-reconcile"})
	if e != nil {
		return nil, e
	}
	defer lock.Release()
	f, e := os.Open(path)
	if e != nil {
		return nil, e
	}
	var receipt capacityReceipt
	e = decodeCapacity(f, &receipt)
	f.Close()
	if e != nil {
		return nil, e
	}
	if receipt.Version != 1 || receipt.Run != run || receipt.Worker != t.Worker {
		return nil, errors.New("retained capacity identity mismatch")
	}
	if receipt.Released {
		reply.Decision = "reconciled"
		reply.Reason = "capacity already released"
		return reply, nil
	}
	gate := capacityGate(t)
	if receipt.ReleasePending {
		e = gate.Release(ctx, run, receipt.Owner)
	} else {
		if receipt.Proof == nil {
			if s.Identity == nil {
				return reply, nil
			}
			boot, e := s.Identity(ctx, 1)
			if e != nil || boot == "" || receipt.BootID == "" {
				return reply, nil
			}
			evidence := "verified receiver reboot"
			if boot == receipt.BootID {
				if !receipt.BoundedBody || receipt.PID <= 0 || !capacityGroupGone(receipt.PID) {
					return reply, nil
				}
				evidence = "verified bounded task process group exited"
			}
			receipt.Proof = &llmbudget.TerminationProof{Owner: receipt.Owner, Run: run, Host: t.Worker, VerifiedAt: time.Now().UTC(), Evidence: evidence}
			if e = writeCapacityReceipt(receipt); e != nil {
				return nil, e
			}
		}
		e = gate.ReconcileTerminated(ctx, run, *receipt.Proof)
	}
	if e != nil {
		return nil, e
	}
	receipt.Released = true
	if e = writeCapacityReceipt(receipt); e != nil {
		return nil, e
	}
	reply.Decision = "reconciled"
	reply.Reason = "verified terminated work released target capacity"
	return reply, nil
}
func (c *CapacityClient) Reconcile(ctx context.Context, name, run string) (*CapacityReply, error) {
	t, e := c.target(name)
	if e != nil {
		return nil, e
	}
	if !t.Dispatch {
		return nil, errors.New("reconciliation requires dispatch permission")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return c.exchange(ctx, t, capacityWire{Operation: "reconcile", Worker: t.Worker, Request: CapacityRequest{ID: run}})
}
func capacityRetentionReason(t CapacityTarget, run string) string {
	return fmt.Sprintf("capacity retained; verify termination with dag capacity reconcile --target %s --run %s (external descendants require verified receiver reboot)", t.Name, run)
}

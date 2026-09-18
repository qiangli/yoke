package llmbudget

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/qiangli/coreutils/pkg/schedule"
)

func init() { schedule.DefaultAdmission = ScheduleAdmission(nil) }

// ScheduleAdmission is the LLM-budget JobAdmission for pkg/schedule: a
// WorkBudget-marked job reserves host/model capacity through the meter before
// it launches and releases or reconciles it after. Nil uses the process policy
// authority; an isolated Gate can be embedded. It never applies to an unmarked
// job. It is installed as schedule.DefaultAdmission at init, so linking this
// package is what turns the budget on — the certified coreutils never does.
func ScheduleAdmission(g *Gate) schedule.JobAdmission {
	if g == nil {
		g = DefaultGate()
	}
	return func(ctx context.Context, j *schedule.Job) (func(error) error, error) {
		if j.WorkBudget == nil {
			return func(error) error { return nil }, nil
		}
		newOwner := NewOwner
		reserve := Reserve
		renew := Renew
		release := Release
		reconcile := ReconcileTerminated
		if g != nil {
			newOwner = g.NewOwner
			reserve = g.Reserve
			renew = g.Renew
			release = g.Release
			reconcile = g.ReconcileTerminated
		}
		o, e := newOwner(ctx, "schedule "+j.ID)
		if e != nil {
			return nil, e
		}
		host, _ := os.Hostname()
		r := Request{ID: "schedule-" + o.ID(), Owner: o.ID(), Run: j.ID, Host: host, Model: j.WorkBudget.Model, Agent: j.WorkBudget.Agent, HostSlots: 1, UnknownMemory: j.WorkBudget.MemoryBytes == nil, TTL: 2 * time.Minute}
		if j.WorkBudget.MemoryBytes != nil {
			r.MemoryBytes = *j.WorkBudget.MemoryBytes
		}
		if r.Model != "" {
			r.Concurrency = 1
			r.UnknownTokens = true
		}
		a, e := reserve(ctx, r)
		if e != nil || a.Reservation == nil || a.Decision.Action != Allow {
			o.Close()
			if e != nil {
				return nil, e
			}
			return nil, fmt.Errorf("schedule: budget %s: %s", a.Decision.Action, a.Decision.Reason)
		}
		stop := make(chan struct{})
		go func() {
			t := time.NewTicker(30 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-stop:
					return
				case <-t.C:
					c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					_ = renew(c, r.ID, r.Owner, 2*time.Minute)
					cancel()
				}
			}
		}()
		return func(runErr error) error {
			close(stop)
			defer o.Close()
			if errors.Is(runErr, schedule.ErrBudgetJobNotStarted) {
				c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				return release(c, r.ID, r.Owner)
			}
			if runErr != nil {
				return nil
			} // uncertain failed work remains reserved for lifecycle reconciliation
			c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if r.Model == "" {
				return release(c, r.ID, r.Owner)
			}
			return reconcile(c, r.ID, TerminationProof{Owner: r.Owner, Run: r.Run, Host: r.Host, VerifiedAt: time.Now().UTC(), Evidence: "owned scheduled command returned successfully; usage remains unknown"})
		}, nil
	}
}

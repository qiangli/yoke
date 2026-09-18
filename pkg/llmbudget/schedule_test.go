package llmbudget

import (
	"context"
	"io"
	"path/filepath"
	"testing"

	"github.com/qiangli/coreutils/pkg/schedule"
)

func TestExpensiveJobAdmissionRefusesBeforeExecution(t *testing.T) {
	zero := 0
	g := New(Config{StatePath: filepath.Join(t.TempDir(), "meter.json"), Policy: &Policy{Version: 1, Constraints: []Constraint{{HostSlots: &zero}}}})
	j := &schedule.Job{ID: "expensive", Command: []string{"a-command-that-must-never-launch"}, WorkBudget: &schedule.WorkBudget{}}
	if e := schedule.FireJobWithAdmission(j, io.Discard, nil, ScheduleAdmission(g)); e == nil {
		t.Fatal("capacity refusal missing")
	}
	called := false
	e := schedule.FireJobWithAdmission(j, io.Discard, nil, func(ctx context.Context, j *schedule.Job) (func(error) error, error) {
		called = true
		return nil, context.Canceled
	})
	if !called || e != context.Canceled {
		t.Fatal("execution bypassed admission", e)
	}
}
func TestOrdinaryJobDoesNotConsumeWorkBudget(t *testing.T) {
	g := New(Config{StatePath: filepath.Join(t.TempDir(), "meter.json"), Policy: &Policy{Version: 1}})
	finish, e := ScheduleAdmission(g)(context.Background(), &schedule.Job{ID: "posix", Kind: "at", POSIXCron: true})
	if e != nil {
		t.Fatal(e)
	}
	if e = finish(nil); e != nil {
		t.Fatal(e)
	}
	// Reporting a new gate must still find no reserved or metered rows.
	r, e := g.CollectReport(context.Background(), ReportOptions{Roster: []Binding{}})
	if e != nil || len(r.Accounts) != 0 {
		t.Fatal(r, e)
	}
}

func TestExpensiveJobHoldsCapacityUntilFinish(t *testing.T) {
	one := 1
	g := New(Config{StatePath: filepath.Join(t.TempDir(), "meter.json"), Policy: &Policy{Version: 1, Constraints: []Constraint{{HostSlots: &one}}}})
	j := &schedule.Job{ID: "running", WorkBudget: &schedule.WorkBudget{}}
	admit := ScheduleAdmission(g)
	finish, e := admit(context.Background(), j)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = admit(context.Background(), &schedule.Job{ID: "second", WorkBudget: &schedule.WorkBudget{}}); e == nil {
		t.Fatal("running job capacity was free")
	}
	if e = finish(nil); e != nil {
		t.Fatal(e)
	}
	next, e := admit(context.Background(), &schedule.Job{ID: "next", WorkBudget: &schedule.WorkBudget{}})
	if e != nil {
		t.Fatal("ended job did not free capacity", e)
	}
	if e = next(nil); e != nil {
		t.Fatal(e)
	}
}

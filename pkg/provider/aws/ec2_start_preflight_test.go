package aws_test

// OpStart money preflights (S546.4 yoke half): every on-demand wake of
// a stopped GPU worker runs the same hourly session model as a create
// — dhnt-monthly-cap, dhnt-gpu-daily and the Running On-Demand G quota
// — before StartInstances, and a refused wake issues none. An
// already-running worker is a no-op start with no preflight, and a
// stop never preflights. Zero cloud spend: every endpoint is an
// httptest fake (fakeec2 + fakelightsail).

import (
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/provider"
	"github.com/qiangli/yoke/pkg/provider/aws"
)

// seedWorkerAs puts a managed, owned worker of one instance type in the
// EC2 fake without counting a launch. AddInstance only knows g6.xlarge,
// so the type is set on the stored record through the returned pointer.
func seedWorkerAs(r rig, name, state, key, typ string) string {
	tags := append(provider.LeaseTags(testOwner, key, nil), aws.ManagedTag)
	in := r.ec2.AddInstance(name, state, key, tags...)
	in.Type = typ
	return in.ID
}

func startAs(t *testing.T, a provider.Adapter, id, key string) (provider.Result, error) {
	t.Helper()
	return do(t, a, provider.Request{Op: provider.OpStart, IdempotencyKey: key, ResourceID: id})
}

// A stopped worker past the monthly cap is refused with budget_exceeded
// — naming the budget, its limit, the month-to-date spend and the
// session estimate — and no StartInstances goes out.
func TestEC2StartRefusesOverMonthlyCap(t *testing.T) {
	r := newRig(t)
	seedMoney(r.ls, 395, 400, 0, 50)
	a := openEC2(t, r, nil)
	id := seedWorker(r, "over-cap", "stopped", "over-cap-key", nil)
	_, err := startAs(t, a, id, "over-cap-start")
	if codeOf(err) != provider.ErrBudgetExceeded {
		t.Fatalf("over-cap start: err = %v, want budget_exceeded", err)
	}
	for _, want := range []string{
		aws.DefaultBudgetName, "monthly cap", "$395.00", "$400.00", "$6.44",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal does not name %q: %v", want, err)
		}
	}
	if n := r.ec2.Calls("StartInstances"); n != 0 {
		t.Fatalf("StartInstances called %d times past the cap, want 0", n)
	}
	if in, _ := r.ec2.Instance(id); in.State != "stopped" {
		t.Fatalf("worker state = %q after refused start, want stopped", in.State)
	}
	mustEndEC2(t, r)
}

// A daily GPU budget at 100% refuses the wake: today's spend plus any
// session is past the limit. The daily budget is alert-only in AWS, so
// this preflight is what stops a GPU being woken all day.
func TestEC2StartRefusesAtDailyLimit(t *testing.T) {
	r := newRig(t)
	seedMoney(r.ls, 10, 400, 50, 50)
	a := openEC2(t, r, nil)
	id := seedWorker(r, "at-daily", "stopped", "at-daily-key", nil)
	_, err := startAs(t, a, id, "at-daily-start")
	if codeOf(err) != provider.ErrBudgetExceeded {
		t.Fatalf("at-limit start: err = %v, want budget_exceeded", err)
	}
	for _, want := range []string{
		aws.DailyBudgetName, "daily", "$50.00", "$6.44",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal does not name %q: %v", want, err)
		}
	}
	if n := r.ec2.Calls("StartInstances"); n != 0 {
		t.Fatalf("StartInstances called %d times at the daily limit, want 0", n)
	}
	mustEndEC2(t, r)
}

// Zero quota refuses the wake with quota_exceeded and launches nothing.
func TestEC2StartRefusesZeroQuota(t *testing.T) {
	r := newRig(t)
	seedMoney(r.ls, 10, 400, 5, 50)
	r.ec2.SetQuota(aws.QuotaRunningOnDemandG, 0)
	a := openEC2(t, r, nil)
	id := seedWorker(r, "no-quota", "stopped", "no-quota-key", nil)
	_, err := startAs(t, a, id, "no-quota-start")
	if codeOf(err) != provider.ErrQuotaExceeded {
		t.Fatalf("no-quota start: err = %v, want quota_exceeded", err)
	}
	if !strings.Contains(err.Error(), aws.QuotaRunningOnDemandG) {
		t.Fatalf("refusal does not name quota %s: %v", aws.QuotaRunningOnDemandG, err)
	}
	if n := r.ec2.Calls("StartInstances"); n != 0 {
		t.Fatalf("StartInstances called %d times without quota, want 0", n)
	}
	mustEndEC2(t, r)
}

// The start preflight prices the instance's OWN type: at 4 vCPUs of
// quota a g6.xlarge (4 vCPUs) wakes while a g6.2xlarge (8 vCPUs) is
// refused.
func TestEC2StartQuotaIsPerType(t *testing.T) {
	r := newRig(t)
	seedMoney(r.ls, 10, 400, 5, 50)
	r.ec2.SetQuota(aws.QuotaRunningOnDemandG, 4)
	a := openEC2(t, r, nil)
	small := seedWorkerAs(r, "q-small", "stopped", "q-small-key", "g6.xlarge")
	if _, err := startAs(t, a, small, "q-small-start"); err != nil {
		t.Fatalf("g6.xlarge start at quota 4: %v", err)
	}
	big := seedWorkerAs(r, "q-big", "stopped", "q-big-key", "g6.2xlarge")
	_, err := startAs(t, a, big, "q-big-start")
	if codeOf(err) != provider.ErrQuotaExceeded {
		t.Fatalf("g6.2xlarge start at quota 4: err = %v, want quota_exceeded", err)
	}
	if n := r.ec2.Calls("StartInstances"); n != 1 {
		t.Fatalf("StartInstances = %d, want exactly 1 (the allowed wake)", n)
	}
	mustEndEC2(t, r)
}

// Under budget and quota the wake goes out exactly once and the worker
// runs.
func TestEC2StartAllowedUnderBudget(t *testing.T) {
	r := newRig(t)
	seedMoney(r.ls, 10, 400, 5, 50)
	a := openEC2(t, r, nil)
	id := seedWorker(r, "wake", "stopped", "wake-key", nil)
	res, err := startAs(t, a, id, "wake-start")
	if err != nil {
		t.Fatalf("start under budget: %v", err)
	}
	if res.Resource == nil || res.Resource.State != provider.StateRunning {
		t.Fatalf("start resource = %+v, want running", res.Resource)
	}
	if n := r.ec2.Calls("StartInstances"); n != 1 {
		t.Fatalf("StartInstances = %d, want exactly 1", n)
	}
	mustEndEC2(t, r)
}

// An already-running worker is a no-op start: it succeeds even when the
// money and quota would refuse a wake, and issues no StartInstances.
func TestEC2RunningStartNeedsNoPreflight(t *testing.T) {
	r := newRig(t)
	seedMoney(r.ls, 395, 400, 50, 50)
	r.ec2.SetQuota(aws.QuotaRunningOnDemandG, 0)
	a := openEC2(t, r, nil)
	id := seedWorker(r, "already", "running", "already-key", nil)
	res, err := startAs(t, a, id, "already-start")
	if err != nil {
		t.Fatalf("start of a running worker with refusing budgets: %v", err)
	}
	if res.Resource.State != provider.StateRunning {
		t.Fatalf("start state = %q, want running", res.Resource.State)
	}
	if n := r.ec2.Calls("StartInstances"); n != 0 {
		t.Fatalf("StartInstances called %d times for a running worker, want 0", n)
	}
	mustEndEC2(t, r)
}

// A stop never preflights: it succeeds even when the money and quota
// would refuse a wake.
func TestEC2StopNeverPreflights(t *testing.T) {
	r := newRig(t)
	seedMoney(r.ls, 395, 400, 50, 50)
	r.ec2.SetQuota(aws.QuotaRunningOnDemandG, 0)
	a := openEC2(t, r, nil)
	id := seedWorker(r, "halt", "running", "halt-key", nil)
	res, err := do(t, a, provider.Request{Op: provider.OpStop, IdempotencyKey: "halt-stop", ResourceID: id})
	if err != nil {
		t.Fatalf("stop with refusing budgets: %v", err)
	}
	if res.Resource.State != provider.StateStopped {
		t.Fatalf("stop state = %q, want stopped", res.Resource.State)
	}
	if n := r.ec2.Calls("StopInstances"); n != 1 {
		t.Fatalf("StopInstances = %d, want exactly 1", n)
	}
	mustEndEC2(t, r)
}

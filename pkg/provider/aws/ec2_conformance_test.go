package aws_test

// Dispatcher conformance for the EC2 GPU worker adapter (S546.2):
// the same dimensions the shared suite checks — states and
// transitions, idempotency and reconciliation, normalized errors,
// unsupported operations, stop-versus-destroy billing and the A6
// destroy authorization — against fakeec2 through provider.Open the
// way a host opens it, plus the allocating live lifecycle. Fixture
// seeding bypasses the adapter (no provider call), so the
// fixture-only run must allocate nothing. Zero cloud spend.
import (
	"context"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/provider"
	"github.com/qiangli/yoke/pkg/provider/aws"
	"github.com/qiangli/yoke/pkg/provider/aws/fakeec2"
	"github.com/qiangli/yoke/pkg/provider/aws/fakelightsail"
)

const ec2ConfOwner = "ec2conformance"

type ec2Seeds struct {
	running, stopped, temp string
}

// openEC2Dispatcher opens the GPU adapter through the dispatcher the
// way a host does, with spend and quota answering yes.
func openEC2Dispatcher(t *testing.T, r rig) *provider.Dispatcher {
	t.Helper()
	seedMoney(r.ls, 10, 400, 5, 50)
	d, err := provider.Open(context.Background(), aws.EC2Manifest(), provider.Options{
		Owner:       ec2ConfOwner,
		Exec:        provider.ExecDisabled,
		Journal:     provider.NewMemoryJournal(),
		Credentials: provider.StaticCredential(fakelightsail.TestSecretAccessKey),
		Builtins: map[string]provider.BuiltinFactory{
			aws.EC2Name: func(_ context.Context, _ provider.Manifest, credential string) (provider.Adapter, error) {
				return aws.NewEC2(aws.EC2Config{
					Config: aws.Config{
						Credentials: aws.Credentials{
							AccessKeyID:     fakelightsail.TestAccessKeyID,
							SecretAccessKey: credential,
						},
						Region:          fakeec2.TestRegion,
						Owner:           ec2ConfOwner,
						BudgetsURL:      r.ls.URL,
						CostExplorerURL: r.ls.URL,
						STSURL:          r.ls.URL,
						S3URL:           r.ec2.URL,
						PollInterval:    time.Millisecond,
						WaitTimeout:     5 * time.Second,
						Sprint:          "546",
						Story:           "2988",
					},
					EC2URL:           r.ec2.URL,
					ServiceQuotasURL: r.ec2.URL,
				})
			},
		},
		Timeout: 10 * time.Second,
		Retries: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// seedEC2Fake provisions deterministic fixtures: a running worker,
// one parked stopped, and a temporary one. Each carries what a real
// create writes — the lease tags AND dhnt:managed=true.
func seedEC2Fake(r rig) ec2Seeds {
	mk := func(name, state, key string, auth *provider.Authorization) string {
		tags := append(provider.LeaseTags(ec2ConfOwner, key, auth), aws.ManagedTag, "dhnt:sprint=546")
		return r.ec2.AddInstance(name, state, key, tags...).ID
	}
	return ec2Seeds{
		running: mk("fx-running", "running", "seed-running", nil),
		stopped: mk("fx-stopped", "running", "seed-stopped", nil),
		temp: mk("fx-temp", "running", "seed-temp", &provider.Authorization{
			Kind: provider.AuthTemporary, ExpiresAt: time.Now().Add(time.Hour),
		}),
	}
}

func ec2do(t *testing.T, d *provider.Dispatcher, req provider.Request) (provider.Result, error) {
	t.Helper()
	if req.ProtocolVersion == "" {
		req.ProtocolVersion = provider.ProtocolVersion
	}
	return d.Do(context.Background(), req)
}

func ec2code(err error) provider.ErrorCode { return codeOf(err) }

// TestEC2ConformanceFixtureOnly runs the non-allocating dimensions:
// validate, capacity, plan, inspect, list, stop/start, unsupported
// ops, normalized errors and A6 — then proves the fixtures allocated
// nothing.
func TestEC2ConformanceFixtureOnly(t *testing.T) {
	r := newRig(t)
	s := seedEC2Fake(r)
	d := openEC2Dispatcher(t, r)
	ctx := context.Background()

	if res, err := ec2do(t, d, provider.Request{Op: provider.OpValidate}); err != nil {
		t.Fatalf("validate: %v", err)
	} else if len(res.Data) == 0 {
		t.Fatal("validate: empty payload")
	}
	if _, err := ec2do(t, d, provider.Request{Op: provider.OpCapacity}); err != nil {
		t.Fatalf("capacity: %v", err)
	}
	pres, err := ec2do(t, d, provider.Request{Op: provider.OpPlan, IdempotencyKey: "conf-plan-1",
		Spec: &provider.Spec{Name: "would-be", Region: fakeec2.TestRegion, Size: "g6.xlarge", Image: fakeec2.TestAMI}})
	if err != nil {
		t.Fatalf("plan: %v", err)
	} else if len(pres.Data) == 0 {
		t.Fatal("plan: empty plan payload")
	}
	res, err := ec2do(t, d, provider.Request{Op: provider.OpInspect, ResourceID: s.running})
	if err != nil {
		t.Fatalf("inspect: %v", err)
	} else if res.Resource == nil || res.Resource.State != provider.StateRunning {
		t.Fatalf("inspect: state = %v, want running", res.Resource)
	}
	res, err = ec2do(t, d, provider.Request{Op: provider.OpList})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	seen := map[string]bool{}
	for _, x := range res.Resources {
		seen[x.ID] = true
	}
	for _, id := range []string{s.running, s.stopped, s.temp} {
		if !seen[id] {
			t.Fatalf("list: missing fixture %s", id)
		}
	}
	// Stop is not destroy: the worker stays, keeps its identity and
	// keeps billing the volume.
	before, err := ec2do(t, d, provider.Request{Op: provider.OpInspect, ResourceID: s.running})
	if err != nil {
		t.Fatalf("pre-stop inspect: %v", err)
	}
	res, err = ec2do(t, d, provider.Request{Op: provider.OpStop, IdempotencyKey: "conf-stop-1", ResourceID: s.running})
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	if res.Resource == nil || res.Resource.State != provider.StateStopped {
		t.Fatalf("stop: state = %v, want stopped", res.Resource)
	}
	if res.Resource.Name != before.Resource.Name {
		t.Fatalf("stop: name changed %q -> %q", before.Resource.Name, res.Resource.Name)
	}
	if res.Cost == nil || !res.Cost.Ongoing {
		t.Fatalf("stop: cost = %+v, want ongoing (the volume keeps billing)", res.Cost)
	}
	if !r.ec2.Live(s.running) {
		t.Fatal("stop deleted the worker: a stop must not delete")
	}
	res, err = ec2do(t, d, provider.Request{Op: provider.OpStart, IdempotencyKey: "conf-start-1", ResourceID: s.running})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if res.Resource == nil || res.Resource.State != provider.StateRunning {
		t.Fatalf("start: state = %v, want running", res.Resource)
	}
	// Unsupported ops never reach the provider.
	for _, op := range []provider.Op{provider.Op("hibernate"), provider.OpReboot, provider.OpResize} {
		if _, err := ec2do(t, d, provider.Request{Op: op, IdempotencyKey: "conf-un-" + string(op), ResourceID: s.running,
			Spec: &provider.Spec{Size: "g6.xlarge"}}); ec2code(err) != provider.ErrUnsupportedCapability {
			t.Fatalf("%s: err = %v, want unsupported_capability", op, err)
		}
	}
	// Normalized errors.
	if _, err := ec2do(t, d, provider.Request{Op: provider.OpInspect, ResourceID: "999999"}); ec2code(err) != provider.ErrNotFound {
		t.Fatalf("inspect missing: err = %v, want not_found", err)
	}
	if _, err := ec2do(t, d, provider.Request{Op: provider.OpCreate,
		Spec: &provider.Spec{Name: "nokey"}}); ec2code(err) != provider.ErrInvalidRequest {
		t.Fatalf("create without idempotency key: err = %v, want invalid_request", err)
	}
	if _, err := ec2do(t, d, provider.Request{Op: provider.OpValidate,
		ProtocolVersion: "provider/v0"}); ec2code(err) != provider.ErrProtocolMismatch {
		t.Fatalf("wrong protocol: err = %v, want protocol_mismatch", err)
	}
	// A6: a naked destroy is refused before any provider call, the
	// worker stays live, and the refusal reports ongoing cost.
	res, err = ec2do(t, d, provider.Request{Op: provider.OpDestroy, IdempotencyKey: "conf-destroy-naked", ResourceID: s.running})
	if ec2code(err) != provider.ErrConfirmationRequired {
		t.Fatalf("unconfirmed destroy: err = %v, want confirmation_required", err)
	}
	if res.Cost == nil || !res.Cost.Ongoing {
		t.Fatalf("refused destroy must report ongoing cost: %+v", res.Cost)
	}
	if !r.ec2.Live(s.running) {
		t.Fatal("refused destroy removed the worker provider-side")
	}
	view, err := d.View(ctx)
	if err != nil {
		t.Fatalf("journal view: %v", err)
	}
	if jr, ok := view.Resource(s.running); !ok || jr.Destroyed {
		t.Fatalf("journal after refused destroy: %+v", jr)
	}
	// A6: a temporary authorization destroys a temporary worker but
	// not a persistent one.
	if _, err := ec2do(t, d, provider.Request{Op: provider.OpDestroy, IdempotencyKey: "conf-temp-no",
		ResourceID: s.running, Authorization: &provider.Authorization{Kind: provider.AuthTemporary}}); ec2code(err) != provider.ErrConfirmationRequired {
		t.Fatalf("temporary destroy of a persistent worker: err = %v, want confirmation_required", err)
	}
	if _, err := ec2do(t, d, provider.Request{Op: provider.OpDestroy, IdempotencyKey: "conf-temp-yes",
		ResourceID: s.temp, Authorization: &provider.Authorization{Kind: provider.AuthTemporary}}); err != nil {
		t.Fatalf("temporary destroy of a temporary worker: %v", err)
	}
	if r.ec2.Live(s.temp) {
		t.Fatal("temporary destroy left the worker live")
	}
	// A6: a confirmed destroy ends the stopped fixture.
	auth := &provider.Authorization{Kind: provider.AuthConfirmation, ResourceID: s.stopped, IdempotencyKey: "conf-del-yes"}
	if _, err := ec2do(t, d, provider.Request{Op: provider.OpDestroy, IdempotencyKey: "conf-del-yes",
		ResourceID: s.stopped, Authorization: auth}); err != nil {
		t.Fatalf("confirmed destroy: %v", err)
	}
	if r.ec2.Live(s.stopped) {
		t.Fatal("confirmed destroy left the worker live")
	}

	if n := r.ec2.CreateCount(); n != 0 {
		t.Fatalf("CreateCount after the fixture-only suite = %d, want 0", n)
	}
	mustEndEC2(t, r)
}

// TestEC2ConformanceLiveLifecycle runs the allocating checks: a live
// create, an ambiguous create that must reconcile to one worker, and
// the stop/start/destroy teardown — ending with no live workers.
func TestEC2ConformanceLiveLifecycle(t *testing.T) {
	r := newRig(t)
	seedEC2Fake(r)
	d := openEC2Dispatcher(t, r)
	before := r.ec2.CreateCount()

	tempAuth := &provider.Authorization{Kind: provider.AuthTemporary, ExpiresAt: time.Now().Add(time.Hour)}
	res, err := ec2do(t, d, provider.Request{Op: provider.OpCreate, IdempotencyKey: "conf-live-1",
		Spec:          &provider.Spec{Name: "live", Region: fakeec2.TestRegion, Size: "g6.xlarge", Image: fakeec2.TestAMI},
		Authorization: tempAuth})
	if err != nil {
		t.Fatalf("live create: %v", err)
	}
	live := res.Resource.ID
	if res.Resource.State != provider.StateRunning {
		t.Fatalf("live create: state = %q, want running", res.Resource.State)
	}
	// Ambiguous create: the answer is lost, the worker exists, and
	// the adapter must find it rather than launch a second one.
	r.ec2.Inject("RunInstances", fakeec2.FaultLoseResponse, 1)
	res, err = ec2do(t, d, provider.Request{Op: provider.OpCreate, IdempotencyKey: "conf-live-2",
		Spec:          &provider.Spec{Name: "live-ambiguous", Region: fakeec2.TestRegion, Size: "g6.xlarge", Image: fakeec2.TestAMI},
		Authorization: tempAuth})
	if err != nil {
		t.Fatalf("ambiguous create: %v", err)
	}
	ambiguous := res.Resource.ID
	if got := r.ec2.CreateCount() - before; got != 2 {
		t.Fatalf("CreateCount delta = %d, want 2 (one per key, none duplicated)", got)
	}
	for _, id := range []string{live, ambiguous} {
		if _, err := ec2do(t, d, provider.Request{Op: provider.OpStop, IdempotencyKey: "conf-live-stop-" + id, ResourceID: id}); err != nil {
			t.Fatalf("live stop %s: %v", id, err)
		}
		if _, err := ec2do(t, d, provider.Request{Op: provider.OpStart, IdempotencyKey: "conf-live-start-" + id, ResourceID: id}); err != nil {
			t.Fatalf("live start %s: %v", id, err)
		}
		if _, err := ec2do(t, d, provider.Request{Op: provider.OpDestroy, IdempotencyKey: "conf-live-del-" + id,
			ResourceID: id, Authorization: tempAuth}); err != nil {
			t.Fatalf("live destroy %s: %v", id, err)
		}
		if r.ec2.Live(id) {
			t.Fatalf("live worker %s still live after destroy", id)
		}
	}
	mustEndEC2(t, r)
}

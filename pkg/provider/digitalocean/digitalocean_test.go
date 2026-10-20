package digitalocean_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/provider"
	"github.com/qiangli/yoke/pkg/provider/digitalocean"
	"github.com/qiangli/yoke/pkg/provider/digitalocean/fakedo"
)

func newAdapter(t *testing.T, fake *fakedo.Server) provider.Adapter {
	t.Helper()
	a, err := digitalocean.New(digitalocean.Config{BaseURL: fake.URL, Token: fakedo.TestToken, Owner: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func testSpec() *provider.Spec {
	return &provider.Spec{Name: "sat-1", Region: "r1", Size: "s-1vcpu-1gb", Image: "ubuntu-24-04-x64"}
}

func TestCreateIdempotencyKeyReplay(t *testing.T) {
	fake := fakedo.New(t)
	a := newAdapter(t, fake)
	req := provider.Request{Op: provider.OpCreate, IdempotencyKey: "k-1", Spec: testSpec()}
	r1, err := a.Do(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := a.Do(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if r1.Resource == nil || r2.Resource == nil || r1.Resource.ID == "" || r1.Resource.ID != r2.Resource.ID {
		t.Fatalf("replay returned a different resource: %+v vs %+v", r1.Resource, r2.Resource)
	}
	if n := fake.CreateCount(); n != 1 {
		t.Fatalf("CreateCount = %d, want 1", n)
	}
}

func newFastAdapter(t *testing.T, fake *fakedo.Server, timeout time.Duration) provider.Adapter {
	t.Helper()
	a, err := digitalocean.New(digitalocean.Config{
		BaseURL: fake.URL, Token: fakedo.TestToken, Owner: "t1",
		PollInterval: time.Millisecond, HTTPClient: &http.Client{Timeout: timeout},
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func code(err error) provider.ErrorCode {
	var e *provider.Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

func mustCreate(t *testing.T, a provider.Adapter, key string, auth *provider.Authorization) *provider.Resource {
	t.Helper()
	r, err := a.Do(context.Background(), provider.Request{Op: provider.OpCreate, IdempotencyKey: key, Spec: testSpec(), Authorization: auth})
	if err != nil {
		t.Fatal(err)
	}
	return r.Resource
}

func confirm(id, key string) *provider.Authorization {
	return &provider.Authorization{Kind: provider.AuthConfirmation, ResourceID: id, IdempotencyKey: key}
}

func TestCreateTagsAndUserDataPassThrough(t *testing.T) {
	fake := fakedo.New(t)
	a := newAdapter(t, fake)
	spec := testSpec()
	spec.Tags = []string{"role-sat"}
	spec.UserData = []byte("#!opaque enrollment bundle\n")
	exp := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	r, err := a.Do(context.Background(), provider.Request{Op: provider.OpCreate, IdempotencyKey: "k", Spec: spec,
		Authorization: &provider.Authorization{Kind: provider.AuthTemporary, ExpiresAt: exp}})
	if err != nil {
		t.Fatal(err)
	}
	id, _ := strconv.Atoi(r.Resource.ID)
	d, ok := fake.Droplet(id)
	if !ok {
		t.Fatal("droplet missing")
	}
	if d.UserData != string(spec.UserData) {
		t.Fatalf("user data altered: %q", d.UserData)
	}
	for _, want := range []string{provider.TagLease, provider.OwnerTag("t1"), provider.IdempotencyTag("k"), provider.TagTemporary, provider.DeadlineTag(exp), "role-sat"} {
		if !slices.Contains(d.Tags, want) {
			t.Errorf("missing tag %q in %v", want, d.Tags)
		}
	}
	if r.Resource.RawStatus == "" || len(r.Resource.PublicIPs) == 0 {
		t.Errorf("resource not normalized: %+v", r.Resource)
	}
}

func TestDestroyRefusesForeignResource(t *testing.T) {
	fake := fakedo.New(t)
	a := newAdapter(t, fake)
	untagged := fake.AddDroplet("pet")
	foreign := fake.AddDroplet("other", provider.TagLease, provider.OwnerTag("someone-else"))
	ownerOnly := fake.AddDroplet("half", provider.OwnerTag("t1")) // no lease tag
	for _, id := range []int{untagged, foreign, ownerOnly} {
		sid := strconv.Itoa(id)
		_, err := a.Do(context.Background(), provider.Request{Op: provider.OpDestroy, IdempotencyKey: "d-" + sid, ResourceID: sid, Authorization: confirm(sid, "d-"+sid)})
		if code(err) != provider.ErrNotOwned {
			t.Fatalf("droplet %d: got %v, want not_owned", id, err)
		}
		if _, ok := fake.Droplet(id); !ok {
			t.Fatalf("droplet %d was deleted", id)
		}
	}
	if n := fake.DeleteCount(); n != 0 {
		t.Fatalf("DeleteCount = %d, want 0", n)
	}
	// Stop/inspect refuse them too.
	sid := strconv.Itoa(untagged)
	if _, err := a.Do(context.Background(), provider.Request{Op: provider.OpStop, IdempotencyKey: "s", ResourceID: sid}); code(err) != provider.ErrNotOwned {
		t.Fatalf("stop foreign: %v", err)
	}
	if d, _ := fake.Droplet(untagged); d.Status != "active" {
		t.Fatalf("foreign droplet touched: %s", d.Status)
	}
}

func TestStopNeverDeletes(t *testing.T) {
	fake := fakedo.New(t)
	fake.SetAsyncPolls(2)
	a := newFastAdapter(t, fake, 5*time.Second)
	res := mustCreate(t, a, "k", nil)
	id, _ := strconv.Atoi(res.ID)
	fake.AttachVolume(id, "vol-1")
	for i := range 2 { // replay: second stop finds it already off
		r, err := a.Do(context.Background(), provider.Request{Op: provider.OpStop, IdempotencyKey: "stop-1", ResourceID: res.ID})
		if err != nil {
			t.Fatalf("stop %d: %v", i, err)
		}
		if r.Op != provider.OpStop || r.Resource.State != provider.StateStopped || r.Resource.RawStatus != "off" {
			t.Fatalf("stop %d result: %+v", i, r.Resource)
		}
		if r.Cost == nil || !r.Cost.Ongoing || !slices.Contains(r.Cost.Retained, "volume:vol-1") {
			t.Fatalf("stop must report ongoing charges: %+v", r.Cost)
		}
	}
	if _, ok := fake.Droplet(id); !ok || fake.DeleteCount() != 0 {
		t.Fatalf("stop deleted the droplet (deletes=%d)", fake.DeleteCount())
	}
	r, err := a.Do(context.Background(), provider.Request{Op: provider.OpStart, IdempotencyKey: "start-1", ResourceID: res.ID})
	if err != nil || r.Resource.State != provider.StateRunning {
		t.Fatalf("start: %v %+v", err, r.Resource)
	}
}

func TestCreateResponseLostLeavesNoOrphan(t *testing.T) {
	for _, kind := range []fakedo.Failure{fakedo.LoseResponse, fakedo.StallAfterSideEffect} {
		fake := fakedo.New(t)
		a := newFastAdapter(t, fake, 300*time.Millisecond)
		fake.Inject(http.MethodPost, "/v2/droplets", kind, 1)
		req := provider.Request{Op: provider.OpCreate, IdempotencyKey: "k", Spec: testSpec()}
		r1, err := a.Do(context.Background(), req)
		if err != nil {
			t.Fatalf("fault %d: create whose response was lost should recover the droplet: %v", kind, err)
		}
		r2, err := a.Do(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		if r1.Resource.ID != r2.Resource.ID || fake.CreateCount() != 1 {
			t.Fatalf("fault %d: ids %s/%s creates=%d", kind, r1.Resource.ID, r2.Resource.ID, fake.CreateCount())
		}
	}
}

func TestCreate5xxThenReplay(t *testing.T) {
	fake := fakedo.New(t)
	a := newAdapter(t, fake)
	fake.Inject(http.MethodPost, "/v2/droplets", fakedo.Fail5xx, 1)
	req := provider.Request{Op: provider.OpCreate, IdempotencyKey: "k", Spec: testSpec()}
	r, err := a.Do(context.Background(), req)
	if code(err) != provider.ErrProvider || r.Error == nil || !r.Error.Retryable {
		t.Fatalf("got %v %+v, want retryable provider_error", err, r.Error)
	}
	if fake.CreateCount() != 0 {
		t.Fatal("503 before side effect created a droplet")
	}
	if _, err := a.Do(context.Background(), req); err != nil || fake.CreateCount() != 1 {
		t.Fatalf("replay: %v creates=%d", err, fake.CreateCount())
	}
}

func TestAsyncCreateThenInspect(t *testing.T) {
	fake := fakedo.New(t)
	fake.SetAsyncPolls(2)
	a := newFastAdapter(t, fake, 5*time.Second)
	r, err := a.Do(context.Background(), provider.Request{Op: provider.OpCreate, IdempotencyKey: "k", Spec: testSpec()})
	if err != nil || r.Resource.State != provider.StatePending || r.Resource.RawStatus != "new" {
		t.Fatalf("create: %v %+v", err, r.Resource)
	}
	var st provider.State
	for range 5 {
		r, err = a.Do(context.Background(), provider.Request{Op: provider.OpInspect, ResourceID: r.Resource.ID})
		if err != nil {
			t.Fatal(err)
		}
		if st = r.Resource.State; st == provider.StateRunning {
			break
		}
	}
	if st != provider.StateRunning {
		t.Fatalf("never became running: %s", st)
	}
}

func TestDestroyReportsRetainedVolumes(t *testing.T) {
	fake := fakedo.New(t)
	a := newAdapter(t, fake)
	res := mustCreate(t, a, "k", nil)
	id, _ := strconv.Atoi(res.ID)
	fake.AttachVolume(id, "vol-9")
	r, err := a.Do(context.Background(), provider.Request{Op: provider.OpDestroy, IdempotencyKey: "d", ResourceID: res.ID, Authorization: confirm(res.ID, "d")})
	if err != nil {
		t.Fatal(err)
	}
	if r.Cost == nil || !r.Cost.Ongoing || !slices.Contains(r.Cost.Retained, "volume:vol-9") {
		t.Fatalf("cost: %+v", r.Cost)
	}
	// Replay after success: the droplet is gone.
	if _, err := a.Do(context.Background(), provider.Request{Op: provider.OpDestroy, IdempotencyKey: "d", ResourceID: res.ID, Authorization: confirm(res.ID, "d")}); code(err) != provider.ErrNotFound {
		t.Fatalf("replayed destroy: %v", err)
	}
	if fake.DeleteCount() != 1 {
		t.Fatalf("deletes = %d", fake.DeleteCount())
	}
}

func TestListOnlyOwnedAcrossPages(t *testing.T) {
	fake := fakedo.New(t)
	fake.SetMaxPerPage(1)
	a := newAdapter(t, fake)
	fake.AddDroplet("pet")
	fake.AddDroplet("theirs", provider.TagLease, provider.OwnerTag("other"))
	fake.AddDroplet("spoof", provider.OwnerTag("t1")) // owner tag without lease
	var want []string
	for i := range 3 {
		want = append(want, mustCreate(t, a, "k"+strconv.Itoa(i), nil).ID)
	}
	before := fake.RequestCount()
	r, err := a.Do(context.Background(), provider.Request{Op: provider.OpList})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, res := range r.Resources {
		got = append(got, res.ID)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("list = %v, want %v", got, want)
	}
	if n := fake.RequestCount() - before; n < len(want) {
		t.Fatalf("list made %d requests; pagination not followed", n)
	}
}

func TestReadOnlyOps(t *testing.T) {
	fake := fakedo.New(t)
	a := newAdapter(t, fake)
	for _, op := range []provider.Op{provider.OpValidate, provider.OpCapacity} {
		r, err := a.Do(context.Background(), provider.Request{Op: op})
		if err != nil || len(r.Data) == 0 {
			t.Fatalf("%s: %v %s", op, err, r.Data)
		}
	}
	r, err := a.Do(context.Background(), provider.Request{Op: provider.OpPlan, IdempotencyKey: "k", Spec: testSpec()})
	if err != nil {
		t.Fatal(err)
	}
	var p digitalocean.Plan
	if err := json.Unmarshal(r.Data, &p); err != nil || p.PriceMonthly <= 0 || !slices.Contains(p.Tags, provider.IdempotencyTag("k")) {
		t.Fatalf("plan: %v %+v", err, p)
	}
	if fake.CreateCount() != 0 {
		t.Fatal("plan created something")
	}
	bad := testSpec()
	bad.Region = "r2"
	if _, err := a.Do(context.Background(), provider.Request{Op: provider.OpPlan, Spec: bad}); code(err) != provider.ErrInvalidRequest {
		t.Fatalf("plan in unavailable region: %v", err)
	}
}

func TestResizeAndReboot(t *testing.T) {
	fake := fakedo.New(t)
	fake.SetAsyncPolls(1)
	a := newFastAdapter(t, fake, 5*time.Second)
	res := mustCreate(t, a, "k", nil)
	r, err := a.Do(context.Background(), provider.Request{Op: provider.OpResize, IdempotencyKey: "rz", ResourceID: res.ID, Spec: &provider.Spec{Size: "s-2vcpu-4gb"}})
	if err != nil || r.Resource.Size != "s-2vcpu-4gb" {
		t.Fatalf("resize: %v %+v", err, r.Resource)
	}
	if _, err := a.Do(context.Background(), provider.Request{Op: provider.OpReboot, IdempotencyKey: "rb", ResourceID: res.ID}); err != nil {
		t.Fatal(err)
	}
}

func TestInvalidCredentials(t *testing.T) {
	fake := fakedo.New(t)
	a, err := digitalocean.New(digitalocean.Config{BaseURL: fake.URL, Token: "wrong", Owner: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Do(context.Background(), provider.Request{Op: provider.OpValidate}); code(err) != provider.ErrInvalidCredentials {
		t.Fatalf("got %v", err)
	}
	if _, err := digitalocean.New(digitalocean.Config{BaseURL: fake.URL, Token: "x", Owner: "bad owner"}); err == nil {
		t.Fatal("unsafe owner accepted")
	}
}

// TestDigitalOceanWorker_CreateRetryNoDuplicateAllocation exercises the
// role=worker path specifically (S451.4): worker tags, no satellite
// enrollment bundle, and the bootstrap handoff to S451.9. It is not the
// generic idempotency replay covered by TestCreateIdempotencyKeyReplay and
// TestCreateResponseLostLeavesNoOrphan above — those exercise a plain
// create with no role and no handoff.
func TestDigitalOceanWorker_CreateRetryNoDuplicateAllocation(t *testing.T) {
	fake := fakedo.New(t)
	a := newFastAdapter(t, fake, 300*time.Millisecond)
	fake.Inject(http.MethodPost, "/v2/droplets", fakedo.LoseResponse, 1)

	spec := provider.Spec{Name: "wk-1", Region: "r1", Size: "s-1vcpu-1gb", Image: "ubuntu-24-04-x64"}
	res1, handoff1, err := digitalocean.CreateWorker(context.Background(), a, "worker-k", spec, nil)
	if err != nil {
		t.Fatalf("create whose response was lost should adopt the existing droplet: %v", err)
	}
	res2, handoff2, err := digitalocean.CreateWorker(context.Background(), a, "worker-k", spec, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res1.Resource == nil || res2.Resource == nil || res1.Resource.ID == "" || res1.Resource.ID != res2.Resource.ID {
		t.Fatalf("retry allocated a duplicate: %+v vs %+v", res1.Resource, res2.Resource)
	}
	if n := fake.CreateCount(); n != 1 {
		t.Fatalf("CreateCount = %d, want exactly one allocation", n)
	}
	if handoff1 != handoff2 || handoff1.ResourceID != res1.Resource.ID || handoff1.IdempotencyKey != "worker-k" {
		t.Fatalf("bootstrap handoff must stay stable across the retry: %+v vs %+v", handoff1, handoff2)
	}
	for _, want := range []string{provider.TagLease, provider.OwnerTag("t1"), provider.IdempotencyTag("worker-k"), digitalocean.TagRoleWorker} {
		if !slices.Contains(res1.Resource.Tags, want) {
			t.Errorf("missing worker tag %q in %v", want, res1.Resource.Tags)
		}
	}
	id, _ := strconv.Atoi(res1.Resource.ID)
	d, ok := fake.Droplet(id)
	if !ok || d.UserData != "" {
		t.Fatalf("a role=worker create must never carry a satellite enrollment bundle as user data: %q", d.UserData)
	}
}

func TestManifestIsValid(t *testing.T) {
	if err := provider.ValidateManifest(digitalocean.Manifest()); err != nil {
		t.Fatal(err)
	}
}

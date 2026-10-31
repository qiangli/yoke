package aws_test

// Unit tests for the EC2 GPU worker adapter (S546.2) over fakeec2,
// with spend sources from fakelightsail: the allowlist, the launch
// wire (tags on instance AND volume, shutdown behavior, gp3 volume,
// no SG/spot), the hourly/daily/quota preflights, capacity errors,
// and destroy semantics. Zero cloud spend: every endpoint is an
// httptest server.

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/rand"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/provider"
	"github.com/qiangli/yoke/pkg/provider/aws"
	"github.com/qiangli/yoke/pkg/provider/aws/fakeec2"
	"github.com/qiangli/yoke/pkg/provider/aws/fakelightsail"
)

// rig is one EC2 test world: the EC2/quotas/S3 fake plus the
// budgets/CE/STS fake.
type rig struct {
	ec2 *fakeec2.Server
	ls  *fakelightsail.Server
}

func newRig(t *testing.T) rig {
	t.Helper()
	return rig{ec2: fakeec2.New(t), ls: fakelightsail.New(t)}
}

// openEC2 builds a GPU adapter against the rig: EC2, Service Quotas
// and S3 staging go to fakeec2, while Budgets, Cost Explorer and STS
// go to fakelightsail.
func openEC2(t *testing.T, r rig, tweak func(*aws.EC2Config)) provider.Adapter {
	t.Helper()
	cfg := aws.EC2Config{
		Config: aws.Config{
			Credentials:     fakelightsail.TestCredentials(),
			Region:          fakeec2.TestRegion,
			Owner:           testOwner,
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
	}
	if tweak != nil {
		tweak(&cfg)
	}
	a, err := aws.NewEC2(cfg)
	if err != nil {
		t.Fatalf("aws.NewEC2: %v", err)
	}
	return a
}

func gpuSpec(name, size string) *provider.Spec {
	if size == "" {
		size = "g6.xlarge"
	}
	return &provider.Spec{
		Name: name, Region: fakeec2.TestRegion, Size: size, Image: fakeec2.TestAMI,
	}
}

// seedMoney sets the two budgets the GPU preflight reads: a monthly
// cap with spend and a daily budget with spend.
func seedMoney(ls *fakelightsail.Server, monthlySpend, monthlyLimit, dailySpend, dailyLimit float64) {
	ls.SetBudget(fakelightsail.Budget{
		Name: aws.DefaultBudgetName, LimitUSD: monthlyLimit, ActualSpendUSD: monthlySpend,
	})
	ls.SetBudget(fakelightsail.Budget{
		Name: aws.DailyBudgetName, LimitUSD: dailyLimit, ActualSpendUSD: dailySpend, TimeUnit: "DAILY",
	})
}

// seedWorker puts a managed, owned worker in the EC2 fake without
// counting a launch, the way a previously provisioned worker looks.
func seedWorker(r rig, name, state, key string, auth *provider.Authorization) string {
	tags := append(provider.LeaseTags(testOwner, key, auth), aws.ManagedTag)
	in := r.ec2.AddInstance(name, state, key, tags...)
	return in.ID
}

func mustEndEC2(t *testing.T, r rig) {
	t.Helper()
	if n := r.ec2.BadSignatures(); n != 0 {
		t.Fatalf("%d EC2 request(s) failed SigV4 verification at fakeec2", n)
	}
	if n := r.ls.BadSignatures(); n != 0 {
		t.Fatalf("%d request(s) failed SigV4 verification at fakelightsail", n)
	}
}

func gpuPlan(t *testing.T, a provider.Adapter, name, size string, scripts ...[]byte) aws.GPUPlan {
	t.Helper()
	s := gpuSpec(name, size)
	if len(scripts) > 0 {
		s.UserData = scripts[0]
	}
	res, err := do(t, a, provider.Request{Op: provider.OpPlan, IdempotencyKey: "plan-" + name, Spec: s})
	if err != nil {
		t.Fatalf("plan: %v (%s)", err, res.Error)
	}
	var p aws.GPUPlan
	if err := json.Unmarshal(res.Data, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// Every request the adapter sends must verify against a SigV4
// signature recomputed server-side, across all three faked services.
func TestEC2EveryRequestIsCorrectlySigned(t *testing.T) {
	r := newRig(t)
	seedMoney(r.ls, 10, 400, 5, 50)
	a := openEC2(t, r, nil)
	if _, err := do(t, a, provider.Request{Op: provider.OpValidate}); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if _, err := do(t, a, provider.Request{Op: provider.OpCapacity}); err != nil {
		t.Fatalf("capacity: %v", err)
	}
	_ = gpuPlan(t, a, "sig", "")
	if r.ec2.RequestCount() == 0 {
		t.Fatal("fakeec2 saw no requests")
	}
	mustEndEC2(t, r)
}

// Each service must be signed for its own scope: EC2 and Service
// Quotas for us-west-2, Budgets for us-east-1. Signing Service Quotas
// with the wrong region is the easy mistake, and it would only
// surface as a live SignatureDoesNotMatch.
func TestEC2ServicesAreSignedForTheirOwnScopes(t *testing.T) {
	r := newRig(t)
	seedMoney(r.ls, 10, 400, 5, 50)
	a := openEC2(t, r, nil)
	if _, err := do(t, a, provider.Request{Op: provider.OpValidate}); err != nil {
		t.Fatalf("validate: %v", err)
	}
	_ = gpuPlan(t, a, "scope", "")
	if got := r.ec2.Scope("GetServiceQuota"); got != "servicequotas/us-west-2" {
		t.Fatalf("service quotas signed for %q, want servicequotas/us-west-2", got)
	}
	if got := r.ec2.Scope("DescribeInstances"); got != "ec2/us-west-2" {
		t.Fatalf("ec2 signed for %q, want ec2/us-west-2", got)
	}
	if got := r.ls.Scope("DescribeBudget"); got != "budgets/us-east-1" {
		t.Fatalf("budgets signed for %q, want budgets/us-east-1", got)
	}
	mustEndEC2(t, r)
}

// The plan prints the hourly price: g6.xlarge at $0.805/h over an 8h
// session plus a 100 GB gp3 month, within both budgets and quota.
func TestEC2PlanPricesTheHourlyModel(t *testing.T) {
	r := newRig(t)
	seedMoney(r.ls, 10, 400, 5, 50)
	a := openEC2(t, r, nil)
	p := gpuPlan(t, a, "price", "g6.xlarge")
	if p.PriceHourlyUSD != 0.805 {
		t.Fatalf("price_hourly_usd = %v, want 0.805", p.PriceHourlyUSD)
	}
	if p.SessionHours != 8 || p.SessionCostUSD != 0.805*8 {
		t.Fatalf("session = %.1fh $%.2f, want 8h $6.44", p.SessionHours, p.SessionCostUSD)
	}
	if p.VolumeGB != 100 || p.EBSMonthlyUSD != 8.0 {
		t.Fatalf("ebs = %dGB $%.2f/mo, want 100GB $8.00/mo", p.VolumeGB, p.EBSMonthlyUSD)
	}
	if p.ShutdownBehavior != "stop" {
		t.Fatalf("shutdown_behavior = %q, want stop", p.ShutdownBehavior)
	}
	v := p.Preflight
	if !v.Checked || !v.OK {
		t.Fatalf("preflight = %+v, want a passing checked verdict", v)
	}
	if v.QuotaCode != aws.QuotaRunningOnDemandG || v.QuotaNeedVCPU != 4 {
		t.Fatalf("quota = %+v, want %s needing 4 vCPUs", v, aws.QuotaRunningOnDemandG)
	}
	if v.DailyBudget != aws.DailyBudgetName {
		t.Fatalf("daily budget = %q, want %q", v.DailyBudget, aws.DailyBudgetName)
	}
	// A plan stages nothing: no launch, no S3 object.
	if n := r.ec2.CreateCount(); n != 0 {
		t.Fatalf("CreateCount after plan = %d, want 0", n)
	}
	if keys := r.ec2.S3Keys(); len(keys) != 0 {
		t.Fatalf("plan staged objects: %v", keys)
	}
	mustEndEC2(t, r)
}

// Provisioned throughput above 125 MB/s is priced even while the
// worker is stopped: 500 MB/s adds ~$15/mo, 1,000 ~ +$35/mo.
func TestEC2PlanPricesProvisionedThroughput(t *testing.T) {
	r := newRig(t)
	seedMoney(r.ls, 10, 400, 5, 200)
	for _, tc := range []struct {
		thr  int
		want float64
	}{
		{125, 8.0},
		{500, 8.0 + 375*0.04},
		{1000, 8.0 + 875*0.04},
	} {
		a := openEC2(t, r, func(c *aws.EC2Config) { c.ThroughputMBs = tc.thr })
		p := gpuPlan(t, a, "thr", "")
		if p.EBSMonthlyUSD != tc.want {
			t.Fatalf("throughput %d: ebs $%.2f/mo, want $%.2f/mo", tc.thr, p.EBSMonthlyUSD, tc.want)
		}
		if !p.Preflight.OK {
			t.Fatalf("throughput %d: preflight refused: %s", tc.thr, p.Preflight.Reason)
		}
	}
	mustEndEC2(t, r)
}

// A type off the allowlist is refused before any API call.
func TestEC2RejectsNonAllowlistedType(t *testing.T) {
	r := newRig(t)
	seedMoney(r.ls, 10, 400, 5, 50)
	a := openEC2(t, r, nil)
	s := gpuSpec("bad-type", "g6.4xlarge")
	_, err := do(t, a, provider.Request{Op: provider.OpPlan, IdempotencyKey: "bad-type", Spec: s})
	if codeOf(err) != provider.ErrInvalidRequest {
		t.Fatalf("plan off-allowlist: err = %v, want invalid_request", err)
	}
	if !strings.Contains(err.Error(), "g6.xlarge") {
		t.Fatalf("refusal does not name the allowlist: %v", err)
	}
	if n := r.ec2.Calls("RunInstances"); n != 0 {
		t.Fatalf("RunInstances called %d times for a refused type", n)
	}
	if n := r.ec2.Calls("GetServiceQuota"); n != 0 {
		t.Fatalf("quota consulted %d times for a refused type", n)
	}
	mustEndEC2(t, r)
}

// The adapter is pinned to us-west-2: any other spec region is
// refused before any API call.
func TestEC2RejectsOtherRegions(t *testing.T) {
	r := newRig(t)
	a := openEC2(t, r, nil)
	s := gpuSpec("bad-region", "")
	s.Region = "us-east-1"
	_, err := do(t, a, provider.Request{Op: provider.OpPlan, IdempotencyKey: "bad-region", Spec: s})
	if codeOf(err) != provider.ErrInvalidRequest {
		t.Fatalf("plan other-region: err = %v, want invalid_request", err)
	}
	if n := r.ec2.RequestCount(); n != 0 {
		t.Fatalf("fakeec2 saw %d requests for a refused region", n)
	}
	mustEndEC2(t, r)
}

// The image must be an AMI id.
func TestEC2RejectsNonAMIImage(t *testing.T) {
	r := newRig(t)
	a := openEC2(t, r, nil)
	s := gpuSpec("bad-image", "")
	s.Image = "ubuntu_24_04"
	_, err := do(t, a, provider.Request{Op: provider.OpPlan, IdempotencyKey: "bad-image", Spec: s})
	if codeOf(err) != provider.ErrInvalidRequest {
		t.Fatalf("plan non-ami: err = %v, want invalid_request", err)
	}
	mustEndEC2(t, r)
}

// A volume over the 200 GB IAM ceiling is refused at construction,
// before it can become a denied RunInstances.
func TestEC2RejectsOversizeVolume(t *testing.T) {
	_, err := aws.NewEC2(aws.EC2Config{
		Config:       aws.Config{Credentials: fakelightsail.TestCredentials(), Owner: testOwner},
		VolumeSizeGB: 201,
	})
	if err == nil {
		t.Fatal("NewEC2 with a 201 GB volume: want an error")
	}
	var pe *provider.Error
	if !errors.As(err, &pe) || pe.Code != provider.ErrInvalidRequest {
		t.Fatalf("oversize volume: err = %v, want invalid_request", err)
	}
}

// The launch wire: managed tags on BOTH instance and volume,
// shutdown behavior stop, the gp3 volume, a public address for the
// dial-out — and no security-group or spot parameters at all.
func TestEC2LaunchWire(t *testing.T) {
	r := newRig(t)
	seedMoney(r.ls, 10, 400, 5, 50)
	a := openEC2(t, r, nil)
	script := []byte("#!/bin/sh\necho hello\n")
	s := gpuSpec("wire", "")
	s.UserData = script
	res, err := do(t, a, provider.Request{Op: provider.OpCreate, IdempotencyKey: "wire-key", Spec: s})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if res.Resource == nil || res.Resource.State != provider.StateRunning {
		t.Fatalf("create resource = %+v, want running", res.Resource)
	}
	if !strings.HasPrefix(res.Resource.ID, "i-") {
		t.Fatalf("resource id = %q, want an instance id", res.Resource.ID)
	}
	form := r.ec2.LastRun()
	if form == nil {
		t.Fatal("no RunInstances reached the fake")
	}
	if got := form.Get("InstanceType"); got != "g6.xlarge" {
		t.Fatalf("InstanceType = %q", got)
	}
	if got := form.Get("ImageId"); got != fakeec2.TestAMI {
		t.Fatalf("ImageId = %q", got)
	}
	if got := form.Get("MinCount"); got != "1" {
		t.Fatalf("MinCount = %q, want 1", got)
	}
	if got := form.Get("InstanceInitiatedShutdownBehavior"); got != "stop" {
		t.Fatalf("shutdown behavior = %q, want stop", got)
	}
	if got := form.Get("ClientToken"); got != "wire-key" {
		t.Fatalf("ClientToken = %q, want the idempotency key", got)
	}
	if got := form.Get("TagSpecification.1.ResourceType"); got != "instance" {
		t.Fatalf("spec 1 = %q, want instance", got)
	}
	if !tagSpecHas(form, 1, "dhnt:managed", "true") {
		t.Fatal("instance TagSpecification lacks dhnt:managed=true")
	}
	if got := form.Get("TagSpecification.2.ResourceType"); got != "volume" {
		t.Fatalf("spec 2 = %q, want volume", got)
	}
	if !tagSpecHas(form, 2, "dhnt:managed", "true") {
		t.Fatal("volume TagSpecification lacks dhnt:managed=true")
	}
	if !tagSpecHas(form, 1, "Name", "wire") {
		t.Fatal("instance TagSpecification lacks the Name tag")
	}
	if got := form.Get("BlockDeviceMapping.1.Ebs.VolumeSize"); got != "100" {
		t.Fatalf("volume size = %q, want 100", got)
	}
	if got := form.Get("BlockDeviceMapping.1.Ebs.VolumeType"); got != "gp3" {
		t.Fatalf("volume type = %q, want gp3", got)
	}
	if got := form.Get("BlockDeviceMapping.1.Ebs.DeleteOnTermination"); got != "true" {
		t.Fatalf("DeleteOnTermination = %q, want true", got)
	}
	if got := form.Get("NetworkInterface.1.AssociatePublicIpAddress"); got != "true" {
		t.Fatalf("public address = %q, want true for the dial-out", got)
	}
	for key := range form {
		if strings.Contains(key, "SecurityGroup") || strings.Contains(key, "Spot") ||
			strings.Contains(key, "MarketOptions") || strings.Contains(key, "ModifyInstance") {
			t.Fatalf("launch sends forbidden parameter %q (no SG writes, no spot, no modification)", key)
		}
	}
	raw, err := base64.StdEncoding.DecodeString(form.Get("UserData"))
	if err != nil {
		t.Fatalf("UserData is not base64: %v", err)
	}
	if string(raw) != string(script) {
		t.Fatal("small user data did not pass through byte-identical")
	}
	mustEndEC2(t, r)
}

// Create is idempotent on the key: the second create returns the
// same worker instead of launching another.
func TestEC2CreateIsIdempotent(t *testing.T) {
	r := newRig(t)
	seedMoney(r.ls, 10, 400, 5, 50)
	a := openEC2(t, r, nil)
	mk := func() provider.Result {
		res, err := do(t, a, provider.Request{Op: provider.OpCreate, IdempotencyKey: "idem-1", Spec: gpuSpec("idem", "")})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		return res
	}
	first, second := mk(), mk()
	if first.Resource.ID != second.Resource.ID {
		t.Fatalf("idempotent create returned %s then %s", first.Resource.ID, second.Resource.ID)
	}
	if n := r.ec2.CreateCount(); n != 1 {
		t.Fatalf("CreateCount = %d, want 1", n)
	}
	mustEndEC2(t, r)
}

// A create whose answer was lost reconciles by tag instead of
// launching a second worker.
func TestEC2LostCreateReconciles(t *testing.T) {
	r := newRig(t)
	seedMoney(r.ls, 10, 400, 5, 50)
	a := openEC2(t, r, nil)
	r.ec2.Inject("RunInstances", fakeec2.FaultLoseResponse, 1)
	res, err := do(t, a, provider.Request{Op: provider.OpCreate, IdempotencyKey: "lost-1", Spec: gpuSpec("lost", "")})
	if err != nil {
		t.Fatalf("lost create: %v", err)
	}
	if res.Resource == nil || res.Resource.State != provider.StateRunning {
		t.Fatalf("reconciled resource = %+v, want running", res.Resource)
	}
	if n := r.ec2.CreateCount(); n != 1 {
		t.Fatalf("CreateCount = %d, want exactly 1", n)
	}
	mustEndEC2(t, r)
}

// A script past the 16 KiB limit is staged in S3 behind the S522
// presigned bootstrap, and destroy deletes the staged object.
func TestEC2StagesLargeUserData(t *testing.T) {
	r := newRig(t)
	seedMoney(r.ls, 10, 400, 5, 50)
	a := openEC2(t, r, nil)
	// Incompressible payload: shell text compresses 10x or better,
	// so the fixture is fixed-seed random bytes past the wrapper's
	// ceiling, the way keys and blobs push the live script over it.
	rng := rand.New(rand.NewSource(1))
	script := make([]byte, 24<<10)
	if _, err := rng.Read(script); err != nil {
		t.Fatal(err)
	}
	script = append([]byte("#!/bin/bash\n"), script...)
	p := gpuPlan(t, a, "staged", "g6.xlarge", script)
	if !p.UserDataStaged {
		t.Fatal("large script was not staged in the plan")
	}
	if p.UserDataBucket == "" || !strings.HasPrefix(p.UserDataBucket, "dhnt-userdata-") {
		t.Fatalf("staging bucket = %q, want dhnt-userdata-<acct>-<region>", p.UserDataBucket)
	}
	if p.UserDataSHA256 == "" {
		t.Fatal("staged plan has no script digest")
	}
	res, err := do(t, a, provider.Request{Op: provider.OpCreate, IdempotencyKey: "staged-1", Spec: func() *provider.Spec {
		s := gpuSpec("staged", "")
		s.UserData = script
		return s
	}()})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	keys := r.ec2.S3Keys()
	if len(keys) != 1 {
		t.Fatalf("staged objects = %v, want exactly one", keys)
	}
	raw, err := base64.StdEncoding.DecodeString(r.ec2.LastRun().Get("UserData"))
	if err != nil {
		t.Fatalf("UserData is not base64: %v", err)
	}
	if !strings.Contains(string(raw), "bashy-userdata-s3-v1") {
		t.Fatal("launched user data is not the S3 fetch bootstrap")
	}
	if !tagSpecHas(r.ec2.LastRun(), 1, "dhnt:userdata-key", strings.SplitN(keys[0], "/", 2)[1]) {
		t.Fatalf("instance lacks the dhnt:userdata-key tag for %v", keys)
	}
	obj, ok := r.ec2.S3Object(strings.SplitN(keys[0], "/", 2)[0], strings.SplitN(keys[0], "/", 2)[1])
	if !ok || string(obj.Body) != string(script) {
		t.Fatal("staged object is not the script byte-identical")
	}
	if obj.SSE != "AES256" {
		t.Fatalf("staged object SSE = %q, want AES256", obj.SSE)
	}
	// Destroy deletes the staged object with the worker.
	if _, err := do(t, a, provider.Request{Op: provider.OpDestroy, IdempotencyKey: "staged-del",
		ResourceID: res.Resource.ID,
		Authorization: &provider.Authorization{Kind: provider.AuthConfirmation,
			ResourceID: res.Resource.ID, IdempotencyKey: "staged-del"}}); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if keys := r.ec2.S3Keys(); len(keys) != 0 {
		t.Fatalf("staged objects after destroy = %v, want none", keys)
	}
	mustEndEC2(t, r)
}

// The monthly cap refuses: spend plus session plus EBS past $400.
func TestEC2MonthlyCapRefuses(t *testing.T) {
	r := newRig(t)
	seedMoney(r.ls, 395, 400, 0, 50)
	a := openEC2(t, r, nil)
	s := gpuSpec("over-cap", "")
	_, err := do(t, a, provider.Request{Op: provider.OpCreate, IdempotencyKey: "over-cap", Spec: s})
	if codeOf(err) != provider.ErrInvalidRequest {
		t.Fatalf("over-cap create: err = %v, want invalid_request", err)
	}
	if !strings.Contains(err.Error(), "monthly cap") {
		t.Fatalf("refusal does not name the monthly cap: %v", err)
	}
	if n := r.ec2.Calls("RunInstances"); n != 0 {
		t.Fatalf("RunInstances called %d times past the cap", n)
	}
	mustEndEC2(t, r)
}

// The daily budget refuses: today's spend plus the session past its
// limit.
func TestEC2DailyBudgetRefuses(t *testing.T) {
	r := newRig(t)
	seedMoney(r.ls, 10, 400, 48, 50)
	a := openEC2(t, r, nil)
	_, err := do(t, a, provider.Request{Op: provider.OpCreate, IdempotencyKey: "over-daily", Spec: gpuSpec("over-daily", "")})
	if codeOf(err) != provider.ErrInvalidRequest {
		t.Fatalf("over-daily create: err = %v, want invalid_request", err)
	}
	if !strings.Contains(err.Error(), aws.DailyBudgetName) {
		t.Fatalf("refusal does not name %s: %v", aws.DailyBudgetName, err)
	}
	if n := r.ec2.Calls("RunInstances"); n != 0 {
		t.Fatalf("RunInstances called %d times past the daily budget", n)
	}
	mustEndEC2(t, r)
}

// A missing daily budget fails closed: unknown daily spend refuses
// the launch.
func TestEC2MissingDailyBudgetRefuses(t *testing.T) {
	r := newRig(t)
	r.ls.SetBudget(fakelightsail.Budget{Name: aws.DefaultBudgetName, LimitUSD: 400, ActualSpendUSD: 10})
	a := openEC2(t, r, nil)
	_, err := do(t, a, provider.Request{Op: provider.OpCreate, IdempotencyKey: "no-daily", Spec: gpuSpec("no-daily", "")})
	if codeOf(err) != provider.ErrInvalidRequest {
		t.Fatalf("no-daily create: err = %v, want invalid_request", err)
	}
	mustEndEC2(t, r)
}

// The PENDING-quota state (0 vCPUs) refuses with the quota named, and
// launches nothing.
func TestEC2ZeroQuotaRefuses(t *testing.T) {
	r := newRig(t)
	seedMoney(r.ls, 10, 400, 5, 50)
	r.ec2.SetQuota(aws.QuotaRunningOnDemandG, 0)
	a := openEC2(t, r, nil)
	_, err := do(t, a, provider.Request{Op: provider.OpCreate, IdempotencyKey: "no-quota", Spec: gpuSpec("no-quota", "")})
	if codeOf(err) != provider.ErrInvalidRequest {
		t.Fatalf("no-quota create: err = %v, want invalid_request", err)
	}
	if !strings.Contains(err.Error(), aws.QuotaRunningOnDemandG) {
		t.Fatalf("refusal does not name quota %s: %v", aws.QuotaRunningOnDemandG, err)
	}
	if n := r.ec2.Calls("RunInstances"); n != 0 {
		t.Fatalf("RunInstances called %d times without quota", n)
	}
	mustEndEC2(t, r)
}

// An 8-vCPU type needs 8 vCPUs of quota: 4 is not enough.
func TestEC2QuotaIsPerType(t *testing.T) {
	r := newRig(t)
	seedMoney(r.ls, 10, 400, 5, 50)
	r.ec2.SetQuota(aws.QuotaRunningOnDemandG, 4)
	a := openEC2(t, r, nil)
	if _, err := do(t, a, provider.Request{Op: provider.OpCreate, IdempotencyKey: "q4", Spec: gpuSpec("q4", "g6.xlarge")}); err != nil {
		t.Fatalf("g6.xlarge at quota 4: %v", err)
	}
	_, err := do(t, a, provider.Request{Op: provider.OpCreate, IdempotencyKey: "q8", Spec: gpuSpec("q8", "g6.2xlarge")})
	if codeOf(err) != provider.ErrInvalidRequest {
		t.Fatalf("g6.2xlarge at quota 4: err = %v, want invalid_request", err)
	}
	mustEndEC2(t, r)
}

// InsufficientInstanceCapacity on start is reported with its AZ: the
// volume is AZ-bound, so nothing is recreated elsewhere.
func TestEC2StartCapacityReportsAZ(t *testing.T) {
	r := newRig(t)
	seedMoney(r.ls, 10, 400, 5, 50)
	a := openEC2(t, r, nil)
	id := seedWorker(r, "cap", "stopped", "cap-key", nil)
	r.ec2.Inject("StartInstances", fakeec2.FaultInsufficientCapacity, 1)
	_, err := do(t, a, provider.Request{Op: provider.OpStart, IdempotencyKey: "cap-start", ResourceID: id})
	if err == nil {
		t.Fatal("start without capacity: want an error")
	}
	if !strings.Contains(err.Error(), "InsufficientInstanceCapacity") {
		t.Fatalf("start error does not name capacity: %v", err)
	}
	if !strings.Contains(err.Error(), fakeec2.TestAZ) {
		t.Fatalf("start error does not name the AZ: %v", err)
	}
	if strings.Contains(err.Error(), "recreate the worker") {
		t.Fatalf("start error recreates the worker elsewhere: %v", err)
	}
	if !strings.Contains(err.Error(), "AZ-bound") {
		t.Fatalf("start error does not say the volume is AZ-bound: %v", err)
	}
	if n := r.ec2.CreateCount(); n != 0 {
		t.Fatalf("CreateCount = %d after a failed start: nothing may be recreated", n)
	}
	in, _ := r.ec2.Instance(id)
	if in.State != "stopped" {
		t.Fatalf("worker state = %q after failed start, want stopped", in.State)
	}
	mustEndEC2(t, r)
}

// Stop keeps billing the volume (and says the address is gone);
// start brings the worker back.
func TestEC2StopStartRoundTrip(t *testing.T) {
	r := newRig(t)
	seedMoney(r.ls, 10, 400, 5, 50)
	a := openEC2(t, r, nil)
	id := seedWorker(r, "cycle", "running", "cycle-key", nil)
	res, err := do(t, a, provider.Request{Op: provider.OpStop, IdempotencyKey: "cycle-stop", ResourceID: id})
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	if res.Resource.State != provider.StateStopped {
		t.Fatalf("stop state = %q, want stopped", res.Resource.State)
	}
	if res.Cost == nil || !res.Cost.Ongoing {
		t.Fatalf("stop cost = %+v, want ongoing (the volume keeps billing)", res.Cost)
	}
	if !strings.Contains(res.Cost.Note, "no GPU compute") {
		t.Fatalf("stop note does not say compute stopped billing: %q", res.Cost.Note)
	}
	if !r.ec2.Live(id) {
		t.Fatal("stop deleted the worker: a stop must not delete")
	}
	res, err = do(t, a, provider.Request{Op: provider.OpStart, IdempotencyKey: "cycle-start", ResourceID: id})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if res.Resource.State != provider.StateRunning {
		t.Fatalf("start state = %q, want running", res.Resource.State)
	}
	mustEndEC2(t, r)
}

// Destroy terminates the worker and reports both steps; a repeat
// destroy of the gone worker succeeds.
func TestEC2DestroyTerminates(t *testing.T) {
	r := newRig(t)
	seedMoney(r.ls, 10, 400, 5, 50)
	a := openEC2(t, r, nil)
	id := seedWorker(r, "gone", "running", "gone-key", nil)
	auth := &provider.Authorization{Kind: provider.AuthConfirmation, ResourceID: id, IdempotencyKey: "gone-del"}
	res, err := do(t, a, provider.Request{Op: provider.OpDestroy, IdempotencyKey: "gone-del", ResourceID: id, Authorization: auth})
	if err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if res.Resource.State != provider.StateDeleted {
		t.Fatalf("destroy state = %q, want deleted", res.Resource.State)
	}
	rep, ok := aws.DecodeDestroyReport(res.Data)
	if !ok {
		t.Fatal("destroy carried no step report")
	}
	steps := map[string]string{}
	for _, s := range rep.Steps {
		steps[s.Step] = s.Status
	}
	if steps[aws.StepTerminateInstance] != aws.StepDone {
		t.Fatalf("terminate step = %v", rep.Steps)
	}
	in, found := r.ec2.Instance(id)
	if !found || in.State != "terminated" {
		t.Fatalf("worker after destroy = %+v, want terminated", in)
	}
	// Repeat: the gone worker is a success, not an error.
	res, err = do(t, a, provider.Request{Op: provider.OpDestroy, IdempotencyKey: "gone-del2", ResourceID: id,
		Authorization: &provider.Authorization{Kind: provider.AuthConfirmation, ResourceID: id, IdempotencyKey: "gone-del2"}})
	if err != nil {
		t.Fatalf("repeat destroy: %v", err)
	}
	if res.Resource.State != provider.StateDeleted {
		t.Fatalf("repeat destroy state = %q, want deleted", res.Resource.State)
	}
	mustEndEC2(t, r)
}

// Destroy of an untagged worker is refused by the managed gate before
// any terminate call.
func TestEC2DestroyRefusesUntagged(t *testing.T) {
	r := newRig(t)
	a := openEC2(t, r, nil)
	in := r.ec2.AddInstance("wild", "running", "wild-key",
		append(provider.LeaseTags(testOwner, "wild-key", nil), "dhnt:sprint=546")...)
	auth := &provider.Authorization{Kind: provider.AuthConfirmation, ResourceID: in.ID, IdempotencyKey: "wild-del"}
	_, err := do(t, a, provider.Request{Op: provider.OpDestroy, IdempotencyKey: "wild-del", ResourceID: in.ID, Authorization: auth})
	if codeOf(err) != provider.ErrNotOwned {
		t.Fatalf("untagged destroy: err = %v, want not_owned", err)
	}
	if n := r.ec2.Calls("TerminateInstances"); n != 0 {
		t.Fatalf("TerminateInstances called %d times for an untagged worker", n)
	}
	mustEndEC2(t, r)
}

// Reboot and resize are outside the manifest: Guard refuses them
// before any API call.
func TestEC2RebootAndResizeAreUnsupported(t *testing.T) {
	r := newRig(t)
	a := openEC2(t, r, nil)
	id := seedWorker(r, "nor", "running", "nor-key", nil)
	for _, op := range []provider.Op{provider.OpReboot, provider.OpResize} {
		_, err := do(t, a, provider.Request{Op: op, IdempotencyKey: "nor-" + string(op), ResourceID: id,
			Spec: &provider.Spec{Size: "g6.xlarge"}})
		if codeOf(err) != provider.ErrUnsupportedCapability {
			t.Fatalf("%s: err = %v, want unsupported_capability", op, err)
		}
	}
	if n := r.ec2.RequestCount(); n != 0 {
		t.Fatalf("fakeec2 saw %d requests for refused ops", n)
	}
	mustEndEC2(t, r)
}

// A well-formed but absent id answers not_found.
func TestEC2InspectMissingIsNotFound(t *testing.T) {
	r := newRig(t)
	a := openEC2(t, r, nil)
	_, err := do(t, a, provider.Request{Op: provider.OpInspect, ResourceID: "999999"})
	if codeOf(err) != provider.ErrNotFound {
		t.Fatalf("inspect missing: err = %v, want not_found", err)
	}
	_, err = do(t, a, provider.Request{Op: provider.OpInspect, ResourceID: "i-00000000000000000"})
	if codeOf(err) != provider.ErrNotFound {
		t.Fatalf("inspect unknown id: err = %v, want not_found", err)
	}
	mustEndEC2(t, r)
}

// Capacity lists the four allowlisted offers with hourly prices, and
// marks what the quota does not cover unavailable.
func TestEC2CapacityListsGPUOffers(t *testing.T) {
	r := newRig(t)
	seedMoney(r.ls, 10, 400, 5, 50)
	r.ec2.SetQuota(aws.QuotaRunningOnDemandG, 4)
	a := openEC2(t, r, nil)
	res, err := do(t, a, provider.Request{Op: provider.OpCapacity})
	if err != nil {
		t.Fatalf("capacity: %v", err)
	}
	var out struct {
		Region string `json:"region"`
		Offers []struct {
			SKU        string  `json:"sku"`
			USDPerHour float64 `json:"-"`
			Price      struct {
				USDPerHour float64 `json:"usd_per_hour"`
			} `json:"price"`
			Accelerators []struct {
				Model string `json:"model"`
			} `json:"accelerators"`
			Availability struct {
				State string `json:"state"`
			} `json:"availability"`
		} `json:"offers"`
		QuotaCode  string  `json:"quota_code"`
		QuotaValue float64 `json:"quota_value"`
	}
	if err := json.Unmarshal(res.Data, &out); err != nil {
		t.Fatal(err)
	}
	if out.Region != fakeec2.TestRegion {
		t.Fatalf("region = %q", out.Region)
	}
	if len(out.Offers) != 4 {
		t.Fatalf("offers = %d, want the 4 allowlisted types", len(out.Offers))
	}
	bySKU := map[string]float64{}
	avail := map[string]string{}
	for _, o := range out.Offers {
		bySKU[o.SKU] = o.Price.USDPerHour
		avail[o.SKU] = o.Availability.State
		if len(o.Accelerators) != 1 {
			t.Fatalf("offer %s has no accelerator", o.SKU)
		}
	}
	for sku, want := range map[string]float64{"g6.xlarge": 0.805, "g5.xlarge": 1.006, "g6e.xlarge": 1.861} {
		if bySKU[sku] != want {
			t.Fatalf("offer %s = $%.3f/h, want $%.3f/h", sku, bySKU[sku], want)
		}
	}
	if avail["g6.2xlarge"] != "unavailable" {
		t.Fatalf("g6.2xlarge at quota 4 is %q, want unavailable", avail["g6.2xlarge"])
	}
	if avail["g6.xlarge"] != "available" {
		t.Fatalf("g6.xlarge at quota 4 is %q, want available", avail["g6.xlarge"])
	}
	mustEndEC2(t, r)
}

// A worker that never reaches running is a failed create.
func TestEC2StuckCreateTimesOut(t *testing.T) {
	r := newRig(t)
	seedMoney(r.ls, 10, 400, 5, 50)
	a := openEC2(t, r, func(c *aws.EC2Config) { c.Config.WaitTimeout = 50 * time.Millisecond })
	r.ec2.SetStuck(true)
	_, err := do(t, a, provider.Request{Op: provider.OpCreate, IdempotencyKey: "stuck-1", Spec: gpuSpec("stuck", "")})
	if codeOf(err) != provider.ErrTimeout {
		t.Fatalf("stuck create: err = %v, want timeout", err)
	}
	mustEndEC2(t, r)
}

func TestEC2ValidateReportsRegionAndRedactsKey(t *testing.T) {
	r := newRig(t)
	a := openEC2(t, r, nil)
	res, err := do(t, a, provider.Request{Op: provider.OpValidate})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Region      string `json:"region"`
		AZ          string `json:"availability_zone"`
		AccessKeyID string `json:"access_key_id"`
	}
	if err := json.Unmarshal(res.Data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Region != fakeec2.TestRegion {
		t.Fatalf("region = %q", got.Region)
	}
	if got.AZ != fakeec2.TestAZ {
		t.Fatalf("availability_zone = %q, want %q", got.AZ, fakeec2.TestAZ)
	}
	if strings.Contains(string(res.Data), fakelightsail.TestSecretAccessKey) {
		t.Fatal("validate payload leaked the secret access key")
	}
	mustEndEC2(t, r)
}

// tagSpecHas reports whether TagSpecification n carries key=value.
func tagSpecHas(form url.Values, n int, key, val string) bool {
	pre := "TagSpecification." + strconv.Itoa(n) + "."
	if form.Get(pre+"ResourceType") == "" {
		return false
	}
	for i := 1; ; i++ {
		k := form.Get(pre + "Tag." + strconv.Itoa(i) + ".Key")
		v := form.Get(pre + "Tag." + strconv.Itoa(i) + ".Value")
		if k == "" && v == "" {
			return false
		}
		if k == key && v == val {
			return true
		}
	}
}

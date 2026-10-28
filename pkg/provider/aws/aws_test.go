package aws_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/provider"
	"github.com/qiangli/yoke/pkg/provider/aws"
	"github.com/qiangli/yoke/pkg/provider/aws/fakelightsail"
)

const testOwner = "t1"

// open builds an adapter against the fake, with every endpoint it talks to
// (Lightsail, Cost Explorer, Budgets, STS, S3) pointed at it and the poll
// interval short enough that wait loops do not dominate the test runtime.
func open(t *testing.T, fake *fakelightsail.Server, tweak func(*aws.Config)) provider.Adapter {
	t.Helper()
	cfg := aws.Config{
		Credentials:     fakelightsail.TestCredentials(),
		Region:          fakelightsail.TestRegion,
		Owner:           testOwner,
		BaseURL:         fake.URL,
		CostExplorerURL: fake.URL,
		BudgetsURL:      fake.URL,
		STSURL:          fake.URL,
		S3URL:           fake.URL,
		PollInterval:    time.Millisecond,
		WaitTimeout:     5 * time.Second,
		Sprint:          "522",
		Story:           "2816",
	}
	if tweak != nil {
		tweak(&cfg)
	}
	a, err := aws.New(cfg)
	if err != nil {
		t.Fatalf("aws.New: %v", err)
	}
	return a
}

func spec(name string) *provider.Spec {
	return &provider.Spec{
		Name: name, Region: fakelightsail.TestRegion,
		Size: fakelightsail.TestBundleID, Image: fakelightsail.TestBlueprintID,
	}
}

func do(t *testing.T, a provider.Adapter, req provider.Request) (provider.Result, error) {
	t.Helper()
	if req.ProtocolVersion == "" {
		req.ProtocolVersion = provider.ProtocolVersion
	}
	return a.Do(t.Context(), req)
}

func codeOf(err error) provider.ErrorCode {
	var e *provider.Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// seed puts a managed, owned instance in the fake without counting a
// create, the way a previously provisioned host would look.
func seed(fake *fakelightsail.Server, name, state, key string, auth *provider.Authorization) {
	tags := append(provider.LeaseTags(testOwner, key, auth), aws.ManagedTag)
	fake.AddInstance(name, state, tags...)
}

func mustEnd(t *testing.T, fake *fakelightsail.Server) {
	t.Helper()
	if n := fake.BadSignatures(); n != 0 {
		t.Fatalf("%d request(s) failed SigV4 verification at the fake", n)
	}
}

// Every request the adapter sends must verify against a SigV4 signature
// recomputed server-side, so the signer and the client agree about the
// canonical request, the headers and the payload hash.
func TestEveryRequestIsCorrectlySigned(t *testing.T) {
	fake := fakelightsail.New(t)
	a := open(t, fake, nil)
	if _, err := do(t, a, provider.Request{Op: provider.OpValidate}); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if _, err := do(t, a, provider.Request{Op: provider.OpCapacity}); err != nil {
		t.Fatalf("capacity: %v", err)
	}
	if fake.RequestCount() == 0 {
		t.Fatal("the fake saw no requests")
	}
	mustEnd(t, fake)
}

// Cost Explorer is a GLOBAL service: its endpoint and signing region are
// us-east-1 no matter where the instance lands. Signing it with the
// instance's region is the easy mistake, and it would only surface as a
// live SignatureDoesNotMatch.
func TestCostExplorerIsSignedForUSEast1NotTheInstanceRegion(t *testing.T) {
	fake := fakelightsail.New(t)
	a := open(t, fake, nil)
	if _, err := do(t, a, provider.Request{Op: provider.OpPlan, IdempotencyKey: "scope", Spec: spec("scope")}); err != nil {
		t.Fatal(err)
	}
	if got := fake.Scope("GetCostAndUsage"); got != "ce/us-east-1" {
		t.Fatalf("cost explorer signed for %q, want ce/us-east-1", got)
	}
	if got := fake.Scope("GetBundles"); got != "lightsail/"+fakelightsail.TestRegion {
		t.Fatalf("lightsail signed for %q, want lightsail/%s", got, fakelightsail.TestRegion)
	}
	mustEnd(t, fake)
}

func TestValidateReportsRegionAndRedactsKey(t *testing.T) {
	fake := fakelightsail.New(t)
	a := open(t, fake, nil)
	res, err := do(t, a, provider.Request{Op: provider.OpValidate})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Region      string   `json:"region"`
		AZ          string   `json:"availability_zone"`
		Regions     []string `json:"regions"`
		AccessKeyID string   `json:"access_key_id"`
	}
	if err := json.Unmarshal(res.Data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Region != fakelightsail.TestRegion {
		t.Fatalf("region = %q", got.Region)
	}
	if got.AZ != fakelightsail.TestRegion+"a" {
		t.Fatalf("availability_zone = %q, want the region's first AZ", got.AZ)
	}
	if !slices.Contains(got.Regions, fakelightsail.TestRegion) {
		t.Fatalf("regions = %v", got.Regions)
	}
	// The key id is reported only as a tail, and the secret appears nowhere.
	if strings.Contains(string(res.Data), fakelightsail.TestSecretAccessKey) {
		t.Fatal("validate payload leaked the secret access key")
	}
	if got.AccessKeyID != "****MPLE" {
		t.Fatalf("access_key_id = %q, want a redacted tail", got.AccessKeyID)
	}
	mustEnd(t, fake)
}

// An AccessDeniedException — the shape the Sprint 522 automation principal
// produces outside us-west-2 or on a forbidden action — must normalize to
// invalid_credentials and keep AWS's own message.
func TestAccessDeniedNormalizes(t *testing.T) {
	fake := fakelightsail.New(t)
	a := open(t, fake, nil)
	fake.Inject("GetRegions", fakelightsail.FaultAccessDenied, 1)
	res, err := do(t, a, provider.Request{Op: provider.OpValidate})
	if codeOf(err) != provider.ErrInvalidCredentials {
		t.Fatalf("err = %v, want %s", err, provider.ErrInvalidCredentials)
	}
	if res.Error == nil || !strings.Contains(res.Error.Message, "dhnt-automation") {
		t.Fatalf("result error lost AWS's message: %+v", res.Error)
	}
	mustEnd(t, fake)
}

func TestErrorNormalizationTable(t *testing.T) {
	for _, tc := range []struct {
		fault     fakelightsail.Fault
		want      provider.ErrorCode
		retryable bool
	}{
		{fakelightsail.FaultAccessDenied, provider.ErrInvalidCredentials, false},
		{fakelightsail.FaultThrottle, provider.ErrRateLimited, true},
		{fakelightsail.FaultServiceError, provider.ErrProvider, true},
	} {
		fake := fakelightsail.New(t)
		a := open(t, fake, nil)
		fake.Inject("GetRegions", tc.fault, 1)
		_, err := do(t, a, provider.Request{Op: provider.OpValidate})
		if codeOf(err) != tc.want {
			t.Fatalf("fault %d: err = %v, want %s", tc.fault, err, tc.want)
		}
		var e *provider.Error
		if errors.As(err, &e) && e.Retryable != tc.retryable {
			t.Fatalf("fault %d: retryable = %t, want %t", tc.fault, e.Retryable, tc.retryable)
		}
		mustEnd(t, fake)
	}
}

func TestCapacityReportsOffersWithX8664AndMonthlyLifecycle(t *testing.T) {
	fake := fakelightsail.New(t)
	a := open(t, fake, nil)
	offers, err := provider.DiscoverOffers(t.Context(), a)
	if err != nil {
		t.Fatal(err)
	}
	if len(offers) == 0 {
		t.Fatal("capacity reported no offers")
	}
	var seenGPU, seenTiny bool
	for _, o := range offers {
		if o.CPU.Arch != "x86_64" {
			t.Fatalf("offer %s arch = %q, want x86_64 (Lightsail has no other)", o.SKU, o.CPU.Arch)
		}
		if !o.Lifecycle.StopKeepsBilling {
			t.Fatalf("offer %s must report that a stopped Lightsail plan keeps billing", o.SKU)
		}
		if strings.Contains(o.SKU, "gpu") {
			seenGPU = true
			if o.Qualification.Qualified {
				t.Fatalf("GPU offer %s must not be qualified", o.SKU)
			}
		}
		if o.SKU == fakelightsail.TestBundleID {
			seenTiny = true
			if !o.Price.Known || o.Price.USDPerMonth != 5 {
				t.Fatalf("offer %s price = %+v, want $5/mo", o.SKU, o.Price)
			}
		}
		if o.SKU == "xlarge_3_0" && o.Qualification.Qualified {
			t.Fatal("an $80/mo bundle is over the per-instance ceiling and must not qualify")
		}
	}
	if !seenGPU || !seenTiny {
		t.Fatalf("catalog incomplete: gpu=%t tiny=%t", seenGPU, seenTiny)
	}
	mustEnd(t, fake)
}

func TestPlanReportsPriceBudgetAndFirewall(t *testing.T) {
	fake := fakelightsail.New(t)
	a := open(t, fake, nil)
	res, err := do(t, a, provider.Request{
		Op: provider.OpPlan, IdempotencyKey: "plan-1", Spec: spec("would-be"),
	})
	if err != nil {
		t.Fatal(err)
	}
	var p aws.Plan
	if err := json.Unmarshal(res.Data, &p); err != nil {
		t.Fatal(err)
	}
	if p.PriceMonthly != 5 {
		t.Fatalf("price_monthly = %v, want 5", p.PriceMonthly)
	}
	if p.Arch != "x86_64" {
		t.Fatalf("arch = %q", p.Arch)
	}
	if p.StaticIPName != "would-be-ip" {
		t.Fatalf("static_ip_name = %q", p.StaticIPName)
	}
	if !p.Budget.Checked || !p.Budget.OK {
		t.Fatalf("budget verdict = %+v, want a passing checked verdict", p.Budget)
	}
	if !p.Budget.MonthToDateKnown || p.Budget.MonthToDateUSD != 3.5 {
		t.Fatalf("month to date = %v (known %t), want 3.5", p.Budget.MonthToDateUSD, p.Budget.MonthToDateKnown)
	}
	if p.Budget.ProjectedMonthlyUSD != 8.5 {
		t.Fatalf("projected = %v, want 8.5", p.Budget.ProjectedMonthlyUSD)
	}
	want := []string{"80/tcp from 0.0.0.0/0,::/0", "443/tcp from 0.0.0.0/0,::/0",
		"6443/tcp from 0.0.0.0/0,::/0", "10250/tcp from 0.0.0.0/0,::/0",
		"8475-8476/udp from 0.0.0.0/0,::/0", "22/tcp from lightsail-connect"}
	if !slices.Equal(p.OpenPorts, want) {
		t.Fatalf("open_ports =\n %v\nwant\n %v", p.OpenPorts, want)
	}
	// A plan allocates nothing.
	if fake.CreateCount() != 0 {
		t.Fatalf("plan allocated %d instances", fake.CreateCount())
	}
	mustEnd(t, fake)
}

func TestPlanRejectsUnknownBundleBlueprintAndRegion(t *testing.T) {
	fake := fakelightsail.New(t)
	a := open(t, fake, nil)
	for name, s := range map[string]*provider.Spec{
		"unknown bundle":    {Name: "x", Region: fakelightsail.TestRegion, Size: "nope_9_9", Image: fakelightsail.TestBlueprintID},
		"unknown blueprint": {Name: "x", Region: fakelightsail.TestRegion, Size: fakelightsail.TestBundleID, Image: "nope"},
		"inactive bundle":   {Name: "x", Region: fakelightsail.TestRegion, Size: "retired_1_0", Image: fakelightsail.TestBlueprintID},
		"inactive image":    {Name: "x", Region: fakelightsail.TestRegion, Size: fakelightsail.TestBundleID, Image: "ubuntu_16_04"},
		"other region":      {Name: "x", Region: "eu-west-1", Size: fakelightsail.TestBundleID, Image: fakelightsail.TestBlueprintID},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := do(t, a, provider.Request{Op: provider.OpPlan, IdempotencyKey: "p", Spec: s})
			if codeOf(err) != provider.ErrInvalidRequest {
				t.Fatalf("err = %v, want %s", err, provider.ErrInvalidRequest)
			}
		})
	}
	mustEnd(t, fake)
}

// ---- budget preflight ----

// The budget refusal must happen BEFORE CreateInstances, so an over-cap
// request costs nothing.
func TestBudgetRefusesOverMonthlyCapWithoutCreating(t *testing.T) {
	fake := fakelightsail.New(t)
	fake.SetMonthToDate(47.0) // 47 + 5 = 52 > the $50 cap
	a := open(t, fake, nil)
	res, err := do(t, a, provider.Request{
		Op: provider.OpCreate, IdempotencyKey: "over-cap", Spec: spec("over-cap"),
	})
	if codeOf(err) != provider.ErrInvalidRequest {
		t.Fatalf("err = %v, want %s", err, provider.ErrInvalidRequest)
	}
	for _, want := range []string{"budget preflight refused create", "$47.00", "$52.00", "$50.00"} {
		if res.Error == nil || !strings.Contains(res.Error.Message, want) {
			t.Fatalf("refusal message %q does not name %q", res.Error, want)
		}
	}
	if fake.CreateCount() != 0 {
		t.Fatalf("a refused create still allocated %d instances", fake.CreateCount())
	}
	mustEnd(t, fake)
}

func TestBudgetRefusesOverPerInstanceCeiling(t *testing.T) {
	fake := fakelightsail.New(t)
	a := open(t, fake, nil)
	s := spec("too-big")
	s.Size = "xlarge_3_0" // $80/mo, over the $24 ceiling (and under the cap alone)
	res, err := do(t, a, provider.Request{Op: provider.OpCreate, IdempotencyKey: "too-big", Spec: s})
	if codeOf(err) != provider.ErrInvalidRequest {
		t.Fatalf("err = %v, want %s", err, provider.ErrInvalidRequest)
	}
	if res.Error == nil || !strings.Contains(res.Error.Message, "per-instance ceiling") {
		t.Fatalf("refusal = %v, want the per-instance ceiling named", res.Error)
	}
	if fake.CreateCount() != 0 {
		t.Fatal("a refused create allocated an instance")
	}
	mustEnd(t, fake)
}

// No GPU, ever — even a GPU bundle priced under both caps.
func TestBudgetRefusesGPUBundleRegardlessOfPrice(t *testing.T) {
	fake := fakelightsail.New(t)
	a := open(t, fake, nil)
	s := spec("gpu-box")
	s.Size = "gpu_nvidia_xl_1_0" // $20/mo: under the $24 ceiling and the $50 cap
	res, err := do(t, a, provider.Request{Op: provider.OpCreate, IdempotencyKey: "gpu", Spec: s})
	if codeOf(err) != provider.ErrInvalidRequest {
		t.Fatalf("err = %v, want %s", err, provider.ErrInvalidRequest)
	}
	if res.Error == nil || !strings.Contains(res.Error.Message, "GPU") {
		t.Fatalf("refusal = %v, want GPU named", res.Error)
	}
	if fake.CreateCount() != 0 {
		t.Fatal("a refused GPU create allocated an instance")
	}
	mustEnd(t, fake)
}

// The guard FAILS CLOSED: Cost Explorer denied means spend is unknown, and
// unknown spend refuses the create.
func TestBudgetFailsClosedWhenCostExplorerIsDenied(t *testing.T) {
	fake := fakelightsail.New(t)
	a := open(t, fake, nil)
	fake.Inject("GetCostAndUsage", fakelightsail.FaultAccessDenied, 1)
	res, err := do(t, a, provider.Request{Op: provider.OpCreate, IdempotencyKey: "blind", Spec: spec("blind")})
	if codeOf(err) != provider.ErrInvalidRequest {
		t.Fatalf("err = %v, want %s", err, provider.ErrInvalidRequest)
	}
	if res.Error == nil || !strings.Contains(res.Error.Message, "month-to-date spend is unknown") {
		t.Fatalf("refusal = %v, want unknown spend named", res.Error)
	}
	if fake.CreateCount() != 0 {
		t.Fatal("a blind create allocated an instance")
	}
	mustEnd(t, fake)
}

// ...unless the operator says otherwise, explicitly.
func TestBudgetAllowUnknownSpendProceeds(t *testing.T) {
	fake := fakelightsail.New(t)
	a := open(t, fake, func(c *aws.Config) { c.Budget.AllowUnknownSpend = true })
	fake.Inject("GetCostAndUsage", fakelightsail.FaultAccessDenied, 1)
	if _, err := do(t, a, provider.Request{Op: provider.OpCreate, IdempotencyKey: "blind-ok", Spec: spec("blind-ok")}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if fake.CreateCount() != 1 {
		t.Fatalf("CreateCount = %d, want 1", fake.CreateCount())
	}
	mustEnd(t, fake)
}

// The Cost Explorer request itself: unblended cost, monthly granularity, a
// Not/RECORD_TYPE=Credit filter (so the figure is BEFORE credits) and a
// window whose exclusive End includes today.
func TestCostExplorerRequestExcludesCredits(t *testing.T) {
	fake := fakelightsail.New(t)
	var body map[string]any
	fake.Observe(func(target string, in map[string]any) {
		if target == "GetCostAndUsage" && body == nil {
			body = in
		}
	})
	a := open(t, fake, nil)
	if _, err := do(t, a, provider.Request{Op: provider.OpPlan, IdempotencyKey: "ce", Spec: spec("ce")}); err != nil {
		t.Fatal(err)
	}
	if body == nil {
		t.Fatal("no GetCostAndUsage request was sent")
	}
	if got := body["Granularity"]; got != "MONTHLY" {
		t.Fatalf("Granularity = %v", got)
	}
	metrics, _ := body["Metrics"].([]any)
	if len(metrics) != 1 || metrics[0] != "UnblendedCost" {
		t.Fatalf("Metrics = %v, want [UnblendedCost]", metrics)
	}
	raw, _ := json.Marshal(body["Filter"])
	want := `{"Not":{"Dimensions":{"Key":"RECORD_TYPE","Values":["Credit"]}}}`
	if string(raw) != want {
		t.Fatalf("Filter = %s, want %s", raw, want)
	}
	period, _ := body["TimePeriod"].(map[string]any)
	start, _ := period["Start"].(string)
	end, _ := period["End"].(string)
	now := time.Now().UTC()
	if start != now.Format("2006-01")+"-01" {
		t.Fatalf("Start = %q, want the first of this month", start)
	}
	// End is exclusive, so it must be after today or today's usage is lost.
	if end <= now.Format("2006-01-02") {
		t.Fatalf("End = %q is not after today %q; today's usage would be excluded", end, now.Format("2006-01-02"))
	}
	mustEnd(t, fake)
}

// ---- create ----

func TestCreateTagsFirewallsAndAttachesStaticIP(t *testing.T) {
	fake := fakelightsail.New(t)
	a := open(t, fake, func(c *aws.Config) { c.TTL = "2026-10-29T00:00:00Z" })
	res, err := do(t, a, provider.Request{
		Op: provider.OpCreate, IdempotencyKey: "boot-1", Spec: spec("zoo-aws"),
	})
	if err != nil {
		t.Fatal(err)
	}
	r := res.Resource
	if r == nil || r.ID != "zoo-aws" {
		t.Fatalf("resource = %+v, want the instance NAME as its id", r)
	}
	if r.State != provider.StateRunning {
		t.Fatalf("state = %q, want running (create must wait for it)", r.State)
	}
	// Contract + dhnt tags, both present.
	for _, want := range []string{
		provider.TagLease, provider.OwnerTag(testOwner),
		provider.IdempotencyTag("boot-1"), aws.ManagedTag,
		"dhnt:sprint=522", "dhnt:story=2816", "dhnt:ttl=2026-10-29T00:00:00Z",
	} {
		if !slices.Contains(r.Tags, want) {
			t.Fatalf("tags %v missing %q", r.Tags, want)
		}
	}
	if !provider.Owned(r.Tags, testOwner) {
		t.Fatal("created instance is not recognized as owned")
	}
	// The static IP exists, is named after the instance, is attached, and
	// is the address reported first.
	sip, ok := fake.StaticIP("zoo-aws-ip")
	if !ok || sip.AttachedTo != "zoo-aws" {
		t.Fatalf("static ip = %+v, want zoo-aws-ip attached to zoo-aws", sip)
	}
	if len(r.PublicIPs) == 0 || r.PublicIPs[0] != sip.IPAddress {
		t.Fatalf("public_ips = %v, want the static IP %s first", r.PublicIPs, sip.IPAddress)
	}
	// The firewall is the satellite set, with SSH on the console alias only.
	ports, _ := fake.LastPorts()["portInfos"].([]any)
	if len(ports) != 6 {
		t.Fatalf("portInfos has %d rules, want 6", len(ports))
	}
	var sshChecked bool
	for _, p := range ports {
		m := p.(map[string]any)
		if m["fromPort"] == float64(22) {
			sshChecked = true
			if _, open := m["cidrs"]; open {
				t.Fatalf("ssh rule is world-open: %v", m)
			}
			aliases, _ := m["cidrListAliases"].([]any)
			if len(aliases) != 1 || aliases[0] != "lightsail-connect" {
				t.Fatalf("ssh cidrListAliases = %v, want [lightsail-connect]", aliases)
			}
		}
	}
	if !sshChecked {
		t.Fatal("no tcp/22 rule was sent")
	}
	// The create request carried the AZ Lightsail requires.
	if got := fake.LastCreate()["availabilityZone"]; got != fakelightsail.TestRegion+"a" {
		t.Fatalf("availabilityZone = %v", got)
	}
	mustEnd(t, fake)
}

// Spec.UserData must reach Lightsail byte-identical: a VM given anything
// else never installs or enrolls.
func TestCreatePassesUserDataThroughByteIdentical(t *testing.T) {
	fake := fakelightsail.New(t)
	a := open(t, fake, nil)
	script := "#!/bin/bash\nset -euo pipefail\n# enrollment bundle\necho 'tökén \"quoted\" \\ $x'\n"
	s := spec("ud-box")
	s.UserData = []byte(script)
	if _, err := do(t, a, provider.Request{Op: provider.OpCreate, IdempotencyKey: "ud", Spec: s}); err != nil {
		t.Fatal(err)
	}
	in, ok := fake.Instance("ud-box")
	if !ok {
		t.Fatal("no instance")
	}
	if in.UserData != script {
		t.Fatalf("userData was altered:\n got %q\nwant %q", in.UserData, script)
	}
	mustEnd(t, fake)
}

// A master-rendered first-boot script bigger than EC2's 16 KiB launch
// limit is wrapped losslessly, and what arrives decodes back to the exact
// original.
func TestCreateWrapsOversizeUserDataLosslessly(t *testing.T) {
	fake := fakelightsail.New(t)
	a := open(t, fake, nil)
	script := bigScript(30 * 1024)
	s := spec("big-box")
	s.UserData = script
	if _, err := do(t, a, provider.Request{Op: provider.OpCreate, IdempotencyKey: "big", Spec: s}); err != nil {
		t.Fatal(err)
	}
	in, ok := fake.Instance("big-box")
	if !ok {
		t.Fatal("no instance")
	}
	sent := []byte(in.UserData)
	if len(sent) > aws.UserDataLimit {
		t.Fatalf("sent %d bytes of userData, over the %d byte limit", len(sent), aws.UserDataLimit)
	}
	if !aws.Wrapped(sent) {
		t.Fatal("an oversize script must be wrapped, not truncated")
	}
	back, err := aws.Unwrap(sent)
	if err != nil {
		t.Fatal(err)
	}
	if string(back) != string(script) {
		t.Fatalf("round trip lost the script: %d bytes back, %d sent", len(back), len(script))
	}
	mustEnd(t, fake)
}

// Replaying a create's idempotency key must find the instance by tag, not
// allocate a second one.
func TestCreateIsIdempotentOnKey(t *testing.T) {
	fake := fakelightsail.New(t)
	a := open(t, fake, nil)
	first, err := do(t, a, provider.Request{Op: provider.OpCreate, IdempotencyKey: "same", Spec: spec("idem-box")})
	if err != nil {
		t.Fatal(err)
	}
	second, err := do(t, a, provider.Request{Op: provider.OpCreate, IdempotencyKey: "same", Spec: spec("idem-box")})
	if err != nil {
		t.Fatal(err)
	}
	if first.Resource.ID != second.Resource.ID {
		t.Fatalf("replay answered %s, first create was %s", second.Resource.ID, first.Resource.ID)
	}
	if fake.CreateCount() != 1 {
		t.Fatalf("CreateCount = %d, want exactly 1", fake.CreateCount())
	}
	if fake.StaticIPCount() != 1 {
		t.Fatalf("StaticIPCount = %d, want exactly 1", fake.StaticIPCount())
	}
	mustEnd(t, fake)
}

// A create whose answer is lost after the side effect must be reconciled
// by idempotency tag and reported as the success it was.
func TestCreateReconcilesLostAnswer(t *testing.T) {
	fake := fakelightsail.New(t)
	a := open(t, fake, nil)
	fake.Inject("CreateInstances", fakelightsail.FaultLoseResponse, 1)
	res, err := do(t, a, provider.Request{Op: provider.OpCreate, IdempotencyKey: "lost", Spec: spec("lost-box")})
	if err != nil {
		t.Fatalf("create: %v (a lost answer must be reconciled, not reported as a failure)", err)
	}
	if res.Resource == nil || res.Resource.ID != "lost-box" {
		t.Fatalf("resource = %+v", res.Resource)
	}
	if fake.Losses() != 1 {
		t.Fatalf("injected loss fired %d times, want 1", fake.Losses())
	}
	if fake.CreateCount() != 1 {
		t.Fatalf("CreateCount = %d, want exactly 1 (no orphan)", fake.CreateCount())
	}
	mustEnd(t, fake)
}

// A create replay against an instance the operator has since stopped must
// answer promptly with that instance, not hang out the wait budget waiting
// for a "running" nobody is going to produce.
func TestCreateReplayOnAStoppedInstanceReturnsPromptly(t *testing.T) {
	fake := fakelightsail.New(t)
	a := open(t, fake, func(c *aws.Config) { c.WaitTimeout = 150 * time.Millisecond })
	if _, err := do(t, a, provider.Request{Op: provider.OpCreate, IdempotencyKey: "park", Spec: spec("park-box")}); err != nil {
		t.Fatal(err)
	}
	if _, err := do(t, a, provider.Request{Op: provider.OpStop, IdempotencyKey: "park-stop", ResourceID: "park-box"}); err != nil {
		t.Fatal(err)
	}
	res, err := do(t, a, provider.Request{Op: provider.OpCreate, IdempotencyKey: "park", Spec: spec("park-box")})
	if err != nil {
		t.Fatalf("create replay on a stopped instance: %v", err)
	}
	if res.Resource == nil || res.Resource.ID != "park-box" {
		t.Fatalf("resource = %+v", res.Resource)
	}
	if res.Resource.State != provider.StateStopped {
		t.Fatalf("state = %q, want stopped: create reports what is there and does not start it", res.Resource.State)
	}
	if fake.CreateCount() != 1 {
		t.Fatalf("CreateCount = %d, want 1", fake.CreateCount())
	}
	mustEnd(t, fake)
}

// Lightsail answers CreateInstances with an operation record, not an
// instance, and a GetInstance right after it can still answer NotFound.
// A create must wait that out rather than fail on an instance that exists.
func TestCreateToleratesNotFoundRightAfterCreateInstances(t *testing.T) {
	fake := fakelightsail.New(t)
	a := open(t, fake, nil)
	fake.Inject("GetInstance", fakelightsail.FaultNotFound, 2)
	res, err := do(t, a, provider.Request{Op: provider.OpCreate, IdempotencyKey: "slow", Spec: spec("slow-box")})
	if err != nil {
		t.Fatalf("create: %v (a not-yet-propagated instance must be waited out)", err)
	}
	if res.Resource == nil || res.Resource.State != provider.StateRunning {
		t.Fatalf("resource = %+v, want running", res.Resource)
	}
	if fake.CreateCount() != 1 {
		t.Fatalf("CreateCount = %d, want 1", fake.CreateCount())
	}
	mustEnd(t, fake)
}

// Inspect, by contrast, must NOT wait a missing instance out: a read of
// something that does not exist is not_found, promptly.
func TestInspectDoesNotWaitOutANotFound(t *testing.T) {
	fake := fakelightsail.New(t)
	seed(fake, "there", "running", "seed-there", nil)
	a := open(t, fake, nil)
	fake.Inject("GetInstance", fakelightsail.FaultNotFound, 1)
	_, err := do(t, a, provider.Request{Op: provider.OpInspect, ResourceID: "there"})
	if codeOf(err) != provider.ErrNotFound {
		t.Fatalf("err = %v, want %s", err, provider.ErrNotFound)
	}
	mustEnd(t, fake)
}

// ---- inspect / list / power ----

func TestInspectMissingInstanceIsNotFound(t *testing.T) {
	fake := fakelightsail.New(t)
	a := open(t, fake, nil)
	_, err := do(t, a, provider.Request{Op: provider.OpInspect, ResourceID: "no-such-box"})
	if codeOf(err) != provider.ErrNotFound {
		t.Fatalf("err = %v, want %s", err, provider.ErrNotFound)
	}
	mustEnd(t, fake)
}

// A foreign or untagged instance is never acted on, even by inspect.
func TestForeignInstanceIsNotOwned(t *testing.T) {
	fake := fakelightsail.New(t)
	fake.AddInstance("someone-elses", "running", "Name=theirs")
	a := open(t, fake, nil)
	for _, op := range []provider.Op{provider.OpInspect, provider.OpStop, provider.OpStart} {
		_, err := do(t, a, provider.Request{Op: op, IdempotencyKey: "k-" + string(op), ResourceID: "someone-elses"})
		if codeOf(err) != provider.ErrNotOwned {
			t.Fatalf("%s: err = %v, want %s", op, err, provider.ErrNotOwned)
		}
	}
	mustEnd(t, fake)
}

func TestListReturnsOnlyThisOwnersInstances(t *testing.T) {
	fake := fakelightsail.New(t)
	seed(fake, "mine-a", "running", "seed-a", nil)
	seed(fake, "mine-b", "stopped", "seed-b", nil)
	fake.AddInstance("theirs", "running", provider.TagLease, provider.OwnerTag("other"))
	a := open(t, fake, nil)
	res, err := do(t, a, provider.Request{Op: provider.OpList})
	if err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, r := range res.Resources {
		got = append(got, r.ID)
	}
	slices.Sort(got)
	if !slices.Equal(got, []string{"mine-a", "mine-b"}) {
		t.Fatalf("list = %v, want only this owner's instances", got)
	}
	mustEnd(t, fake)
}

// Stop is not destroy: the instance stays, keeps its data, and keeps
// billing — a Lightsail plan is charged monthly whether it runs or not.
func TestStopRetainsAndReportsOngoingCost(t *testing.T) {
	fake := fakelightsail.New(t)
	a := open(t, fake, nil)
	if _, err := do(t, a, provider.Request{Op: provider.OpCreate, IdempotencyKey: "pw", Spec: spec("pw-box")}); err != nil {
		t.Fatal(err)
	}
	res, err := do(t, a, provider.Request{Op: provider.OpStop, IdempotencyKey: "pw-stop", ResourceID: "pw-box"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Resource.State != provider.StateStopped {
		t.Fatalf("state = %q, want stopped", res.Resource.State)
	}
	if !fake.Exists("pw-box") {
		t.Fatal("stop deleted the instance")
	}
	if res.Cost == nil || !res.Cost.Ongoing {
		t.Fatalf("cost = %+v, want ongoing", res.Cost)
	}
	if !slices.Contains(res.Cost.Retained, "instance:pw-box") ||
		!slices.Contains(res.Cost.Retained, "static-ip:pw-box-ip") {
		t.Fatalf("retained = %v, want the instance and its static IP", res.Cost.Retained)
	}
	// And start brings it back.
	res, err = do(t, a, provider.Request{Op: provider.OpStart, IdempotencyKey: "pw-start", ResourceID: "pw-box"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Resource.State != provider.StateRunning {
		t.Fatalf("state after start = %q", res.Resource.State)
	}
	// The static IP survived the stop/start, which is the whole point.
	sip, _ := fake.StaticIP("pw-box-ip")
	if !slices.Contains(res.Resource.PublicIPs, sip.IPAddress) {
		t.Fatalf("public_ips = %v, want the static IP %s to survive stop/start", res.Resource.PublicIPs, sip.IPAddress)
	}
	mustEnd(t, fake)
}

func TestRebootReturnsRunning(t *testing.T) {
	fake := fakelightsail.New(t)
	seed(fake, "rb-box", "running", "seed-rb", nil)
	a := open(t, fake, nil)
	res, err := do(t, a, provider.Request{Op: provider.OpReboot, IdempotencyKey: "rb", ResourceID: "rb-box"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Resource.State != provider.StateRunning {
		t.Fatalf("state = %q, want running", res.Resource.State)
	}
	mustEnd(t, fake)
}

// Lightsail has no in-place bundle change, so resize is not in the
// manifest and Guard refuses it before any API call.
func TestResizeIsUnsupportedAndNeverReachesAWS(t *testing.T) {
	fake := fakelightsail.New(t)
	seed(fake, "rz-box", "running", "seed-rz", nil)
	a := open(t, fake, nil)
	before := fake.RequestCount()
	_, err := do(t, a, provider.Request{
		Op: provider.OpResize, IdempotencyKey: "rz", ResourceID: "rz-box",
		Spec: &provider.Spec{Size: "small_3_0"},
	})
	if codeOf(err) != provider.ErrUnsupportedCapability {
		t.Fatalf("err = %v, want %s", err, provider.ErrUnsupportedCapability)
	}
	if fake.RequestCount() != before {
		t.Fatal("an undeclared operation reached AWS")
	}
	if aws.Manifest().Supports(provider.OpResize) {
		t.Fatal("the manifest must not declare resize")
	}
	mustEnd(t, fake)
}

// ---- destroy ----

func confirm(id, key string) *provider.Authorization {
	return &provider.Authorization{Kind: provider.AuthConfirmation, ResourceID: id, IdempotencyKey: key}
}

func TestDestroyReleasesStaticIPAndReportsNoOngoingCost(t *testing.T) {
	fake := fakelightsail.New(t)
	a := open(t, fake, nil)
	if _, err := do(t, a, provider.Request{Op: provider.OpCreate, IdempotencyKey: "d", Spec: spec("d-box")}); err != nil {
		t.Fatal(err)
	}
	if fake.StaticIPCount() != 1 {
		t.Fatalf("StaticIPCount before destroy = %d", fake.StaticIPCount())
	}
	res, err := do(t, a, provider.Request{
		Op: provider.OpDestroy, IdempotencyKey: "d-kill", ResourceID: "d-box",
		Authorization: confirm("d-box", "d-kill"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Resource.State != provider.StateDeleted {
		t.Fatalf("state = %q, want deleted", res.Resource.State)
	}
	if fake.Exists("d-box") {
		t.Fatal("destroy left the instance")
	}
	if fake.StaticIPCount() != 0 {
		t.Fatal("destroy left the static IP allocated; an unattached Lightsail static IP is billed")
	}
	if res.Cost != nil && res.Cost.Ongoing {
		t.Fatalf("cost = %+v, want nothing ongoing after a clean destroy", res.Cost)
	}
	mustEnd(t, fake)
}

// A static IP that cannot be released is REPORTED, not swallowed: the
// instance is gone but the address is still billed.
func TestDestroyReportsAnUnreleasedStaticIP(t *testing.T) {
	fake := fakelightsail.New(t)
	a := open(t, fake, nil)
	if _, err := do(t, a, provider.Request{Op: provider.OpCreate, IdempotencyKey: "dl", Spec: spec("dl-box")}); err != nil {
		t.Fatal(err)
	}
	fake.Inject("ReleaseStaticIp", fakelightsail.FaultAccessDenied, 1)
	res, err := do(t, a, provider.Request{
		Op: provider.OpDestroy, IdempotencyKey: "dl-kill", ResourceID: "dl-box",
		Authorization: confirm("dl-box", "dl-kill"),
	})
	if err != nil {
		t.Fatalf("destroy: %v (the instance was deleted; this must succeed with a cost report)", err)
	}
	if res.Cost == nil || !res.Cost.Ongoing {
		t.Fatalf("cost = %+v, want ongoing: the static IP is still allocated", res.Cost)
	}
	if !slices.Contains(res.Cost.Retained, "static-ip:dl-box-ip") {
		t.Fatalf("retained = %v, want the static IP", res.Cost.Retained)
	}
	// The note must name a remedy that exists for AWS, not DigitalOcean's.
	if !strings.Contains(res.Cost.Note, "release-static-ip --static-ip-name dl-box-ip") {
		t.Fatalf("cost note gives no usable remedy: %s", res.Cost.Note)
	}
	mustEnd(t, fake)
}

// Deleting an instance already detaches its static IP, so the detach the
// release path issues first is expected to be rejected. That rejection is
// not a leak: the address is still released and nothing keeps billing.
func TestDestroySucceedsWhenTheDetachIsRejected(t *testing.T) {
	fake := fakelightsail.New(t)
	a := open(t, fake, nil)
	if _, err := do(t, a, provider.Request{Op: provider.OpCreate, IdempotencyKey: "dd", Spec: spec("dd-box")}); err != nil {
		t.Fatal(err)
	}
	fake.Inject("DetachStaticIp", fakelightsail.FaultServiceError, 1)
	res, err := do(t, a, provider.Request{
		Op: provider.OpDestroy, IdempotencyKey: "dd-kill", ResourceID: "dd-box",
		Authorization: confirm("dd-box", "dd-kill"),
	})
	if err != nil {
		t.Fatalf("destroy: %v (a rejected detach is expected, not a failure)", err)
	}
	if fake.StaticIPCount() != 0 {
		t.Fatal("the static IP was not released")
	}
	if res.Cost != nil && res.Cost.Ongoing {
		t.Fatalf("cost = %+v, want nothing ongoing", res.Cost)
	}
	mustEnd(t, fake)
}

// An instance without dhnt:managed=true cannot be deleted by the
// automation principal, so the adapter refuses up front with a message
// that says WHY, instead of letting AWS answer AccessDenied.
func TestDestroyRefusesAnUntaggedInstanceBeforeCallingAWS(t *testing.T) {
	fake := fakelightsail.New(t)
	// Owned by us (lease tags) but missing the managed marker.
	fake.AddInstance("legacy-box", "running", provider.LeaseTags(testOwner, "legacy", nil)...)
	a := open(t, fake, nil)
	before := fake.DeleteCount()
	res, err := do(t, a, provider.Request{
		Op: provider.OpDestroy, IdempotencyKey: "legacy-kill", ResourceID: "legacy-box",
		Authorization: confirm("legacy-box", "legacy-kill"),
	})
	if codeOf(err) != provider.ErrNotOwned {
		t.Fatalf("err = %v, want %s", err, provider.ErrNotOwned)
	}
	if res.Error == nil || !strings.Contains(res.Error.Message, aws.ManagedTag) {
		t.Fatalf("refusal = %v, want %s named", res.Error, aws.ManagedTag)
	}
	if res.Cost == nil || !res.Cost.Ongoing {
		t.Fatalf("a refused destroy must report ongoing cost: %+v", res.Cost)
	}
	if fake.DeleteCount() != before {
		t.Fatal("a refused destroy reached DeleteInstance")
	}
	if !fake.Exists("legacy-box") {
		t.Fatal("a refused destroy removed the instance")
	}
	mustEnd(t, fake)
}

// A6: an unconfirmed destroy of a persistent instance is refused before
// any provider call.
func TestDestroyWithoutConfirmationIsRefused(t *testing.T) {
	fake := fakelightsail.New(t)
	seed(fake, "a6-box", "running", "seed-a6", nil)
	a := open(t, fake, nil)
	before := fake.RequestCount()
	res, err := do(t, a, provider.Request{Op: provider.OpDestroy, IdempotencyKey: "naked", ResourceID: "a6-box"})
	if codeOf(err) != provider.ErrConfirmationRequired {
		t.Fatalf("err = %v, want %s", err, provider.ErrConfirmationRequired)
	}
	if res.Cost == nil || !res.Cost.Ongoing {
		t.Fatalf("refused destroy cost = %+v, want ongoing", res.Cost)
	}
	if fake.RequestCount() != before {
		t.Fatal("a refused destroy reached AWS")
	}
	mustEnd(t, fake)
}

// A temporary (TTL) authorization is honored only on an instance created
// as temporary.
func TestDestroyTemporaryAuthorization(t *testing.T) {
	fake := fakelightsail.New(t)
	temp := &provider.Authorization{Kind: provider.AuthTemporary, ExpiresAt: time.Now().Add(time.Hour)}
	seed(fake, "temp-box", "running", "seed-temp", temp)
	seed(fake, "perm-box", "running", "seed-perm", nil)
	a := open(t, fake, nil)

	_, err := do(t, a, provider.Request{
		Op: provider.OpDestroy, IdempotencyKey: "t-wrong", ResourceID: "perm-box",
		Authorization: &provider.Authorization{Kind: provider.AuthTemporary, ExpiresAt: time.Now().Add(time.Hour)},
	})
	if codeOf(err) != provider.ErrConfirmationRequired {
		t.Fatalf("temporary auth on a persistent instance: err = %v, want %s", err, provider.ErrConfirmationRequired)
	}
	if !fake.Exists("perm-box") {
		t.Fatal("a refused temporary destroy removed the instance")
	}
	res, err := do(t, a, provider.Request{
		Op: provider.OpDestroy, IdempotencyKey: "t-ok", ResourceID: "temp-box",
		Authorization: &provider.Authorization{Kind: provider.AuthTemporary, ExpiresAt: time.Now().Add(time.Hour)},
	})
	if err != nil {
		t.Fatalf("temporary destroy of a temporary instance: %v", err)
	}
	if res.Resource.State != provider.StateDeleted || fake.Exists("temp-box") {
		t.Fatalf("temporary destroy did not delete: %+v", res.Resource)
	}
	mustEnd(t, fake)
}

// ---- construction ----

func TestNewValidatesCredentialsOwnerAndRegion(t *testing.T) {
	good := fakelightsail.TestCredentials()
	if _, err := aws.New(aws.Config{Owner: "t1"}); codeOf(err) != provider.ErrInvalidCredentials {
		t.Fatalf("missing credentials: err = %v, want %s", err, provider.ErrInvalidCredentials)
	}
	if _, err := aws.New(aws.Config{Credentials: good, Owner: "bad owner"}); codeOf(err) != provider.ErrInvalidRequest {
		t.Fatalf("bad owner: err = %v, want %s", err, provider.ErrInvalidRequest)
	}
	if _, err := aws.New(aws.Config{Credentials: good, Owner: "t1", BaseURL: "ftp://nope"}); codeOf(err) != provider.ErrInvalidRequest {
		t.Fatalf("bad endpoint: err = %v, want %s", err, provider.ErrInvalidRequest)
	}
	if _, err := aws.New(aws.Config{Credentials: good, Owner: "t1"}); err != nil {
		t.Fatalf("default config: %v", err)
	}
}

func TestCredentialsFromEnv(t *testing.T) {
	c, e := aws.CredentialsFromEnv([]string{"AWS_ACCESS_KEY_ID=AKIA1", "AWS_SECRET_ACCESS_KEY=s3cr3t", "AWS_SESSION_TOKEN=tok"})
	if e != nil {
		t.Fatal(e)
	}
	if c.AccessKeyID != "AKIA1" || c.SecretAccessKey != "s3cr3t" || c.SessionToken != "tok" {
		t.Fatalf("credentials = %+v", c)
	}
	_, e = aws.CredentialsFromEnv([]string{"AWS_ACCESS_KEY_ID=AKIA1"})
	if e == nil || e.Code != provider.ErrInvalidCredentials {
		t.Fatalf("missing secret: %v", e)
	}
	// The error names what is missing and nothing else: no values.
	if !strings.Contains(e.Message, "AWS_SECRET_ACCESS_KEY") || strings.Contains(e.Message, "AKIA1") {
		t.Fatalf("error message leaks or misses: %q", e.Message)
	}
	if got := aws.RegionFromEnv([]string{"AWS_DEFAULT_REGION=eu-west-1"}); got != "eu-west-1" {
		t.Fatalf("RegionFromEnv = %q", got)
	}
	if got := aws.RegionFromEnv([]string{"AWS_DEFAULT_REGION=eu-west-1", "AWS_REGION=us-west-2"}); got != "us-west-2" {
		t.Fatalf("AWS_REGION must win: %q", got)
	}
	if got := aws.RegionFromEnv(nil); got != "" {
		t.Fatalf("RegionFromEnv(nil) = %q", got)
	}
}

func TestManifestIsAWellFormedBuiltin(t *testing.T) {
	m := aws.Manifest()
	if err := provider.ValidateManifest(m); err != nil {
		t.Fatal(err)
	}
	if m.Name != "aws" || m.Form != provider.FormBuiltin || m.Kind != provider.KindInfra {
		t.Fatalf("manifest = %+v", m)
	}
	for _, op := range []provider.Op{
		provider.OpValidate, provider.OpCapacity, provider.OpPlan, provider.OpCreate,
		provider.OpInspect, provider.OpList, provider.OpStart, provider.OpStop,
		provider.OpDestroy, provider.OpReboot,
	} {
		if !m.Supports(op) {
			t.Fatalf("manifest does not declare %s", op)
		}
	}
}

// ---- helpers ----

// bigScript renders a plausible first-boot script of at least n bytes.
func bigScript(n int) []byte {
	var b strings.Builder
	b.WriteString("#!/bin/bash\nset -euo pipefail\n")
	for i := 0; b.Len() < n; i++ {
		fmt.Fprintf(&b, "echo 'satellite enrollment step %d of the master-rendered first-boot script'\n", i)
	}
	return []byte(b.String())
}

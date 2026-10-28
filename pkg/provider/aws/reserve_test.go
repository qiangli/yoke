package aws_test

// Static-IP-first reservation and the destroy step report (S522.2
// rework 3). fakelightsail only: no AWS call leaves the process.

import (
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/provider"
	"github.com/qiangli/yoke/pkg/provider/aws"
	"github.com/qiangli/yoke/pkg/provider/aws/fakelightsail"
)

// reserve runs the optional reservation capability, failing the test when
// the adapter does not carry it through its guard.
func reserve(t *testing.T, a provider.Adapter, name string) provider.Reservation {
	t.Helper()
	r, err := provider.ReserveEndpoint(t.Context(), a, provider.Spec{Name: name})
	if err != nil {
		t.Fatalf("reserve %s: %v", name, err)
	}
	return r
}

// The address is allocated BEFORE any instance exists, and the endpoint
// reported for it is the DASHED sslip.io name — the only form a wildcard
// certificate covers.
func TestReserveEndpointAllocatesAheadOfTheInstance(t *testing.T) {
	fake := fakelightsail.New(t)
	a := open(t, fake, nil)

	got := reserve(t, a, "zoo-aws")

	if got.Name != "zoo-aws-ip" {
		t.Fatalf("reservation name = %q, want zoo-aws-ip", got.Name)
	}
	if got.Reused {
		t.Fatal("a freshly allocated address must not be reported as reused")
	}
	if got.Address == "" {
		t.Fatal("reservation carries no address")
	}
	want := "https://" + strings.ReplaceAll(got.Address, ".", "-") + ".sslip.io"
	if got.Endpoint != want {
		t.Fatalf("endpoint = %q, want %q", got.Endpoint, want)
	}
	if n := fake.StaticIPCount(); n != 1 {
		t.Fatalf("static ips = %d, want 1", n)
	}
	// The instance itself does not exist yet: that is the whole point.
	if fake.Exists("zoo-aws") {
		t.Fatal("reserving an address created an instance")
	}
	if n := fake.CreateCount(); n != 0 {
		t.Fatalf("CreateInstances calls = %d, want 0", n)
	}
	mustEnd(t, fake)
}

// A second provision of the same satellite REUSES the address it already
// has. Allocating a second one would both change the endpoint the master
// baked into the script and leave the first address billing unattached.
func TestReserveEndpointReusesAnExistingUnattachedAddress(t *testing.T) {
	fake := fakelightsail.New(t)
	a := open(t, fake, nil)

	first := reserve(t, a, "zoo-aws")
	second := reserve(t, a, "zoo-aws")

	if second.Address != first.Address || second.Endpoint != first.Endpoint {
		t.Fatalf("second reservation = %+v, want the first one %+v", second, first)
	}
	if !second.Reused {
		t.Fatal("the second reservation must report that it reused the address")
	}
	if n := fake.StaticIPCount(); n != 1 {
		t.Fatalf("static ips = %d, want 1 (a second allocation would bill)", n)
	}
	if n := fake.Calls("AllocateStaticIp"); n != 1 {
		t.Fatalf("AllocateStaticIp calls = %d, want 1", n)
	}
	mustEnd(t, fake)
}

// The address the create then attaches is the reserved one, and the
// create allocates nothing further.
func TestCreateAttachesThePreReservedAddress(t *testing.T) {
	fake := fakelightsail.New(t)
	a := open(t, fake, nil)

	held := reserve(t, a, "zoo-aws")
	res, err := do(t, a, provider.Request{
		Op: provider.OpCreate, IdempotencyKey: "zoo", Spec: spec("zoo-aws"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := fake.Calls("AllocateStaticIp"); n != 1 {
		t.Fatalf("AllocateStaticIp calls = %d, want 1 (the create must reuse the reservation)", n)
	}
	if n := fake.StaticIPCount(); n != 1 {
		t.Fatalf("static ips = %d, want 1", n)
	}
	sip, ok := fake.StaticIP("zoo-aws-ip")
	if !ok || sip.AttachedTo != "zoo-aws" {
		t.Fatalf("static ip after create = %+v, want attached to zoo-aws", sip)
	}
	if len(res.Resource.PublicIPs) == 0 || res.Resource.PublicIPs[0] != held.Address {
		t.Fatalf("public ips = %v, want the reserved %s first", res.Resource.PublicIPs, held.Address)
	}
	mustEnd(t, fake)
}

// Reserving the name of an address attached to ANOTHER instance is
// refused: moving it would take the address off a host that is working.
func TestReserveEndpointRefusesAnAddressHeldByAnotherInstance(t *testing.T) {
	fake := fakelightsail.New(t)
	a := open(t, fake, nil)
	fake.AddInstance("other-box", "running")
	// The address zoo-aws would reserve is already attached elsewhere —
	// a name collision, or a satellite renamed around an existing host.
	fake.AddStaticIP("zoo-aws-ip", "other-box")

	_, err := provider.ReserveEndpoint(t.Context(), a, provider.Spec{Name: "zoo-aws"})

	if codeOf(err) != provider.ErrNotOwned {
		t.Fatalf("err = %v, want %s", err, provider.ErrNotOwned)
	}
	if !strings.Contains(err.Error(), "other-box") {
		t.Fatalf("the refusal must name who holds it: %v", err)
	}
	if sip, _ := fake.StaticIP("zoo-aws-ip"); sip.AttachedTo != "other-box" {
		t.Fatalf("the address moved: %+v", sip)
	}
	mustEnd(t, fake)
}

// An adapter opened without static IPs reserves nothing, and that is not
// an error: the provisioning sequence simply has no hint to send.
func TestReserveEndpointIsSilentWhenStaticIPsAreOff(t *testing.T) {
	fake := fakelightsail.New(t)
	a := open(t, fake, func(c *aws.Config) { c.SkipStaticIP = true })

	got := reserve(t, a, "zoo-aws")

	if got.Held() {
		t.Fatalf("reservation = %+v, want nothing held", got)
	}
	if n := fake.StaticIPCount(); n != 0 {
		t.Fatalf("static ips = %d, want 0", n)
	}
	mustEnd(t, fake)
}

// A name Lightsail would reject is refused locally, before any API call.
func TestReserveEndpointRefusesAnInvalidName(t *testing.T) {
	fake := fakelightsail.New(t)
	a := open(t, fake, nil)

	_, err := provider.ReserveEndpoint(t.Context(), a, provider.Spec{Name: "not a name"})
	if codeOf(err) != provider.ErrInvalidRequest {
		t.Fatalf("err = %v, want %s", err, provider.ErrInvalidRequest)
	}
	if n := fake.RequestCount(); n != 0 {
		t.Fatalf("%d request(s) reached AWS for a name that cannot exist", n)
	}
}

// ---- destroy step report ----

// steps decodes the per-step destroy report a result carries.
func steps(t *testing.T, res provider.Result) map[string]aws.DestroyStep {
	t.Helper()
	rep, ok := aws.DecodeDestroyReport(res.Data)
	if !ok {
		t.Fatalf("destroy result carries no step report (data = %s)", res.Data)
	}
	out := map[string]aws.DestroyStep{}
	for _, s := range rep.Steps {
		out[s.Step] = s
	}
	return out
}

// Destroy accounts for every billable thing it touched, including the
// ones it had nothing to do about.
func TestDestroyReportsEveryStep(t *testing.T) {
	fake := fakelightsail.New(t)
	a := open(t, fake, nil)
	if _, err := do(t, a, provider.Request{
		Op: provider.OpCreate, IdempotencyKey: "rep", Spec: spec("rep-box"),
	}); err != nil {
		t.Fatal(err)
	}
	res, err := do(t, a, provider.Request{
		Op: provider.OpDestroy, IdempotencyKey: "rep-kill", ResourceID: "rep-box",
		Authorization: confirm("rep-box", "rep-kill"),
	})
	if err != nil {
		t.Fatal(err)
	}
	got := steps(t, res)
	if s := got[aws.StepDeleteInstance]; s.Status != aws.StepDone || s.Target != "rep-box" {
		t.Fatalf("delete-instance step = %+v", s)
	}
	if s := got[aws.StepReleaseStaticIP]; s.Status != aws.StepDone || s.Target != "rep-box-ip" {
		t.Fatalf("release-static-ip step = %+v", s)
	}
	// This instance's first-boot script went inline, so there is nothing
	// staged — and the report says so rather than staying silent.
	if s := got[aws.StepDeleteStagedUserData]; s.Status != aws.StepSkipped {
		t.Fatalf("delete-staged-userdata step = %+v, want skipped", s)
	}
	mustEnd(t, fake)
}

// A failed release is reported as a FAILED step as well as ongoing cost,
// so an operator reading the steps sees the one thing still charging.
func TestDestroyReportsAFailedReleaseAsAStep(t *testing.T) {
	fake := fakelightsail.New(t)
	a := open(t, fake, nil)
	if _, err := do(t, a, provider.Request{
		Op: provider.OpCreate, IdempotencyKey: "fr", Spec: spec("fr-box"),
	}); err != nil {
		t.Fatal(err)
	}
	fake.Inject("ReleaseStaticIp", fakelightsail.FaultAccessDenied, 1)
	res, err := do(t, a, provider.Request{
		Op: provider.OpDestroy, IdempotencyKey: "fr-kill", ResourceID: "fr-box",
		Authorization: confirm("fr-box", "fr-kill"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if s := steps(t, res)[aws.StepReleaseStaticIP]; s.Status != aws.StepFailed {
		t.Fatalf("release-static-ip step = %+v, want failed", s)
	}
	if res.Cost == nil || !res.Cost.Ongoing {
		t.Fatalf("cost = %+v, want ongoing", res.Cost)
	}
	mustEnd(t, fake)
}

// Destroying an instance that is already gone SUCCEEDS and releases the
// address it left behind. A destroy that cannot be repeated is not much
// of a cleanup verb, and an orphaned Lightsail static IP bills forever.
func TestDestroyOfAGoneInstanceReleasesTheOrphanedAddress(t *testing.T) {
	fake := fakelightsail.New(t)
	a := open(t, fake, nil)
	// A provision that reserved the address and then failed: the address
	// exists, the instance never did.
	held := reserve(t, a, "ghost")

	res, err := do(t, a, provider.Request{
		Op: provider.OpDestroy, IdempotencyKey: "ghost-kill", ResourceID: "ghost",
		Authorization: confirm("ghost", "ghost-kill"),
	})
	if err != nil {
		t.Fatalf("destroy of a gone instance: %v", err)
	}
	if res.Resource == nil || res.Resource.State != provider.StateDeleted {
		t.Fatalf("resource = %+v, want deleted", res.Resource)
	}
	if n := fake.StaticIPCount(); n != 0 {
		t.Fatalf("static ips = %d, want 0; %s was left billing", n, held.Name)
	}
	got := steps(t, res)
	if s := got[aws.StepDeleteInstance]; s.Status != aws.StepSkipped {
		t.Fatalf("delete-instance step = %+v, want skipped", s)
	}
	if s := got[aws.StepReleaseStaticIP]; s.Status != aws.StepDone {
		t.Fatalf("release-static-ip step = %+v, want done", s)
	}
	if res.Cost != nil && res.Cost.Ongoing {
		t.Fatalf("cost = %+v, want nothing ongoing", res.Cost)
	}
	mustEnd(t, fake)
}

// Repeating a destroy all the way to a clean account is a no-op that
// still succeeds and still reports what it looked at.
func TestDestroyOfAGoneInstanceWithNoAddressIsANoOp(t *testing.T) {
	fake := fakelightsail.New(t)
	a := open(t, fake, nil)

	res, err := do(t, a, provider.Request{
		Op: provider.OpDestroy, IdempotencyKey: "nothing-kill", ResourceID: "nothing",
		Authorization: confirm("nothing", "nothing-kill"),
	})
	if err != nil {
		t.Fatalf("destroy of an unknown instance: %v", err)
	}
	for _, s := range steps(t, res) {
		if s.Status != aws.StepSkipped {
			t.Fatalf("step %+v: nothing existed, so nothing can have been done", s)
		}
	}
	if n := fake.DeleteCount(); n != 0 {
		t.Fatalf("DeleteInstance calls = %d, want 0", n)
	}
	mustEnd(t, fake)
}

// The idempotent path is still an A6 mutation: releasing an address
// without the per-operation confirmation is refused, and nothing is
// touched.
func TestDestroyOfAGoneInstanceStillNeedsConfirmation(t *testing.T) {
	fake := fakelightsail.New(t)
	a := open(t, fake, nil)
	reserve(t, a, "ghost")

	_, err := do(t, a, provider.Request{
		Op: provider.OpDestroy, IdempotencyKey: "ghost-kill", ResourceID: "ghost",
	})
	if codeOf(err) != provider.ErrConfirmationRequired {
		t.Fatalf("err = %v, want %s", err, provider.ErrConfirmationRequired)
	}
	if n := fake.StaticIPCount(); n != 1 {
		t.Fatalf("static ips = %d; a refused destroy must release nothing", n)
	}
	mustEnd(t, fake)
}

// An address attached to a DIFFERENT instance is left alone when the
// named instance is gone: it belongs to a host that still exists.
func TestDestroyOfAGoneInstanceLeavesSomeoneElsesAddress(t *testing.T) {
	fake := fakelightsail.New(t)
	a := open(t, fake, nil)
	if _, err := do(t, a, provider.Request{
		Op: provider.OpCreate, IdempotencyKey: "live", Spec: spec("live-box"),
	}); err != nil {
		t.Fatal(err)
	}
	// An address named for a gone instance, but attached elsewhere.
	fake.AddStaticIP("ghost-ip", "live-box")

	res, err := do(t, a, provider.Request{
		Op: provider.OpDestroy, IdempotencyKey: "ghost-kill", ResourceID: "ghost",
		Authorization: confirm("ghost", "ghost-kill"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if s := steps(t, res)[aws.StepReleaseStaticIP]; s.Status != aws.StepSkipped {
		t.Fatalf("release-static-ip step = %+v, want skipped", s)
	}
	if _, ok := fake.StaticIP("ghost-ip"); !ok {
		t.Fatal("released an address attached to another instance")
	}
	mustEnd(t, fake)
}

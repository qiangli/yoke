package fleet

// `infra inspect|list|destroy` and the static-IP-first provision order
// (S522.2 rework 3). fakelightsail and an httptest master only: no AWS
// call and no master call leaves the process.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/provider"
	awsprovider "github.com/qiangli/yoke/pkg/provider/aws"
	"github.com/qiangli/yoke/pkg/provider/aws/fakelightsail"
)

// seedAWSInstance puts a managed, owned instance in the fake, the way a
// previous `infra provision` would have left it. owner "local" is the
// lifecycle verbs' default --owner.
func seedAWSInstance(fake *fakelightsail.Server, name string, tags ...string) {
	all := append(provider.LeaseTags("local", "seed-"+name, nil), awsprovider.ManagedTag)
	fake.AddInstance(name, "running", append(all, tags...)...)
}

// enrollmentMaster is a master that issues an enrollment bundle and records the
// endpoint hint it was asked to render the first-boot script for.
func enrollmentMaster(t *testing.T, seen *[]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			SatelliteID string `json:"satellite_id"`
			Endpoint    string `json:"endpoint"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		*seen = append(*seen, body.Endpoint)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"satellite_id":      body.SatelliteID,
			"enrollment_token":  "tok-1",
			"master_public_key": "ed25519:k",
			"master_url":        "https://master.example",
			"artifact_url":      "https://artifacts.example/sat",
			"user_data":         "#!/bin/sh\nENDPOINT=" + body.Endpoint + "\n",
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// ---- provision: the address first, the bundle second ----

// The live failure this rework exists for: the satellite was created
// before anyone knew what address it would answer on, so the first-boot
// script was rendered for the wrong endpoint. The address is now
// allocated BEFORE the bundle is requested, and the dashed sslip.io name
// for it is the hint the master renders with.
func TestProvisionAWSReservesTheAddressBeforeAskingTheMaster(t *testing.T) {
	isolatedFleetRoot(t)
	fake := useFakeLightsail(t)
	clearAWSCredentialEnv(t)
	awsCredentialEnv(t)
	var hints []string
	master := enrollmentMaster(t, &hints)

	out, err := runInfra(t, "provision", "aws", "--satellite", "zoo-aws",
		"--region", fakelightsail.TestRegion, "--size", "small_3_0", "--image", "ubuntu_24_04",
		"--master", master.URL, "--token", "sess")
	if err != nil {
		t.Fatalf("provision: %v\n%s", err, out)
	}

	sip, ok := fake.StaticIP("zoo-aws-ip")
	if !ok {
		t.Fatal("no static ip was reserved for zoo-aws")
	}
	want := provider.SslipEndpoint(sip.IPAddress)
	if len(hints) != 1 || hints[0] != want {
		t.Fatalf("endpoint hints = %v, want [%s]", hints, want)
	}
	// Reserved before the create, attached after it.
	if sip.AttachedTo != "zoo-aws" {
		t.Fatalf("static ip = %+v, want attached to zoo-aws", sip)
	}
	// And the operator is told the endpoint, which is what they have to
	// hand to anything that talks to this satellite.
	if !strings.Contains(out, "endpoint: "+want) {
		t.Fatalf("output does not print the endpoint %q:\n%s", want, out)
	}
	if n := fake.BadSignatures(); n != 0 {
		t.Fatalf("%d request(s) failed SigV4 verification", n)
	}
}

// The reservation is reported in --json too, next to the address it
// belongs to, so a script does not have to parse prose.
func TestProvisionAWSJSONCarriesTheEndpoint(t *testing.T) {
	isolatedFleetRoot(t)
	fake := useFakeLightsail(t)
	clearAWSCredentialEnv(t)
	awsCredentialEnv(t)
	var hints []string
	master := enrollmentMaster(t, &hints)

	out, err := runInfra(t, "provision", "aws", "--satellite", "zoo-aws",
		"--region", fakelightsail.TestRegion, "--size", "small_3_0", "--image", "ubuntu_24_04",
		"--master", master.URL, "--token", "sess", "--json")
	if err != nil {
		t.Fatalf("provision: %v\n%s", err, out)
	}
	// The progress lines precede the JSON record; the record is the last line.
	lines := strings.Split(strings.TrimSpace(out), "\n")
	var row map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &row); err != nil {
		t.Fatalf("last line is not JSON: %v\n%s", err, out)
	}
	sip, _ := fake.StaticIP("zoo-aws-ip")
	if row["endpoint"] != provider.SslipEndpoint(sip.IPAddress) {
		t.Fatalf("endpoint = %v, want %s", row["endpoint"], provider.SslipEndpoint(sip.IPAddress))
	}
	if row["reserved_address"] != sip.IPAddress || row["reserved_address_name"] != "zoo-aws-ip" {
		t.Fatalf("reservation fields = %v / %v", row["reserved_address"], row["reserved_address_name"])
	}
	if row["id"] != "zoo-aws" || row["state"] != "running" {
		t.Fatalf("row = %v", row)
	}
}

// A caller who names --endpoint has decided; nothing is reserved ahead of
// the bundle and the master is asked for exactly that endpoint.
func TestProvisionAWSExplicitEndpointWins(t *testing.T) {
	isolatedFleetRoot(t)
	fake := useFakeLightsail(t)
	clearAWSCredentialEnv(t)
	awsCredentialEnv(t)
	var hints []string
	master := enrollmentMaster(t, &hints)

	out, err := runInfra(t, "provision", "aws", "--satellite", "zoo-aws",
		"--region", fakelightsail.TestRegion, "--size", "small_3_0", "--image", "ubuntu_24_04",
		"--endpoint", "https://sat.example.com",
		"--master", master.URL, "--token", "sess")
	if err != nil {
		t.Fatalf("provision: %v\n%s", err, out)
	}
	if len(hints) != 1 || hints[0] != "https://sat.example.com" {
		t.Fatalf("endpoint hints = %v", hints)
	}
	if n := fake.Calls("AllocateStaticIp"); n != 1 {
		// The create still gives the host a stable address; what must
		// NOT happen is a reservation ahead of the bundle overriding the
		// endpoint the caller named.
		t.Fatalf("AllocateStaticIp calls = %d, want 1 (from the create)", n)
	}
	if !strings.Contains(out, "endpoint: https://sat.example.com") {
		t.Fatalf("output does not report the named endpoint:\n%s", out)
	}
}

// ---- inspect ----

func TestInfraInspectAWSReportsTheStableAddress(t *testing.T) {
	isolatedFleetRoot(t)
	fake := useFakeLightsail(t)
	clearAWSCredentialEnv(t)
	awsCredentialEnv(t)
	seedAWSInstance(fake, "zoo-aws")
	fake.AddStaticIP("zoo-aws-ip", "zoo-aws")

	out, err := runInfra(t, "inspect", "aws", "--name", "zoo-aws", "--region", fakelightsail.TestRegion)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	sip, _ := fake.StaticIP("zoo-aws-ip")
	for _, want := range []string{
		"name: zoo-aws",
		"state: running",
		"address: " + sip.IPAddress,
		"endpoint: " + provider.SslipEndpoint(sip.IPAddress),
		awsprovider.ManagedTag,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("inspect output missing %q:\n%s", want, out)
		}
	}
	if n := fake.BadSignatures(); n != 0 {
		t.Fatalf("%d request(s) failed SigV4 verification", n)
	}
}

func TestInfraInspectNeedsANameAndAProvider(t *testing.T) {
	isolatedFleetRoot(t)
	clearAWSCredentialEnv(t)

	if _, err := runInfra(t, "inspect", "aws"); err == nil || !strings.Contains(err.Error(), "needs --name") {
		t.Fatalf("err = %v, want a --name refusal", err)
	}
	if _, err := runInfra(t, "inspect", "--name", "zoo-aws"); err == nil ||
		!strings.Contains(err.Error(), "infra inspect needs a provider") {
		t.Fatalf("err = %v, want a provider refusal", err)
	}
}

func TestInfraInspectMissingInstanceIsNotFound(t *testing.T) {
	isolatedFleetRoot(t)
	useFakeLightsail(t)
	clearAWSCredentialEnv(t)
	awsCredentialEnv(t)

	_, err := runInfra(t, "inspect", "aws", "--name", "no-such-box", "--region", fakelightsail.TestRegion)
	if err == nil || !strings.Contains(err.Error(), string(provider.ErrNotFound)) {
		t.Fatalf("err = %v, want %s", err, provider.ErrNotFound)
	}
	if !strings.Contains(err.Error(), "no-such-box") {
		t.Fatalf("the error must name what was looked for: %v", err)
	}
}

// ---- list ----

// With --provider, `infra list` lists what that provider is running.
// Without it, it still lists the installed provider PACKAGES.
func TestInfraListProviderResources(t *testing.T) {
	isolatedFleetRoot(t)
	fake := useFakeLightsail(t)
	clearAWSCredentialEnv(t)
	awsCredentialEnv(t)
	seedAWSInstance(fake, "zoo-aws")
	seedAWSInstance(fake, "zoo-aws-2")
	// Not this owner's: it must not be listed.
	fake.AddInstance("someone-elses", "running", awsprovider.ManagedTag)

	out, err := runInfra(t, "list", "--provider", "aws", "--region", fakelightsail.TestRegion)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, want := range []string{"zoo-aws", "zoo-aws-2", "running"} {
		if !strings.Contains(out, want) {
			t.Fatalf("list output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "someone-elses") {
		t.Fatalf("list showed an instance this owner does not hold:\n%s", out)
	}

	// The package listing is unchanged.
	pkgs, err := runInfra(t, "list")
	if err != nil {
		t.Fatalf("list packages: %v", err)
	}
	if !strings.Contains(pkgs, "aws") || !strings.Contains(pkgs, "digitalocean") {
		t.Fatalf("package listing changed:\n%s", pkgs)
	}
	if strings.Contains(pkgs, "zoo-aws") {
		t.Fatalf("the package listing leaked resources:\n%s", pkgs)
	}
}

func TestInfraListProviderResourcesJSON(t *testing.T) {
	isolatedFleetRoot(t)
	fake := useFakeLightsail(t)
	clearAWSCredentialEnv(t)
	awsCredentialEnv(t)
	seedAWSInstance(fake, "zoo-aws")

	out, err := runInfra(t, "list", "--provider", "aws", "--region", fakelightsail.TestRegion, "--json")
	if err != nil {
		t.Fatalf("list --json: %v", err)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if len(rows) != 1 || rows[0]["id"] != "zoo-aws" || rows[0]["state"] != "running" {
		t.Fatalf("rows = %v", rows)
	}
}

// ---- destroy ----

func TestInfraDestroyAWSReportsEveryStep(t *testing.T) {
	isolatedFleetRoot(t)
	fake := useFakeLightsail(t)
	clearAWSCredentialEnv(t)
	awsCredentialEnv(t)
	seedAWSInstance(fake, "zoo-aws", awsprovider.TagUserDataKey+"=zoo-aws/abc123.sh")
	fake.AddStaticIP("zoo-aws-ip", "zoo-aws")

	out, err := runInfra(t, "destroy", "aws", "--name", "zoo-aws",
		"--region", fakelightsail.TestRegion, "--yes")
	if err != nil {
		t.Fatalf("destroy: %v\n%s", err, out)
	}
	for _, want := range []string{
		"[done] delete-instance zoo-aws",
		"[done] release-static-ip zoo-aws-ip",
		"delete-staged-userdata zoo-aws/abc123.sh",
		"zoo-aws: deleted",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("destroy output missing %q:\n%s", want, out)
		}
	}
	if fake.Exists("zoo-aws") {
		t.Fatal("destroy left the instance")
	}
	if n := fake.StaticIPCount(); n != 0 {
		t.Fatalf("static ips = %d; an unattached Lightsail static IP is billed", n)
	}
	if n := fake.BadSignatures(); n != 0 {
		t.Fatalf("%d request(s) failed SigV4 verification", n)
	}
}

// --yes IS the confirmation. Without it the provider is never called and
// the instance is untouched.
func TestInfraDestroyWithoutYesCallsNothing(t *testing.T) {
	isolatedFleetRoot(t)
	fake := useFakeLightsail(t)
	clearAWSCredentialEnv(t)
	awsCredentialEnv(t)
	seedAWSInstance(fake, "zoo-aws")

	_, err := runInfra(t, "destroy", "aws", "--name", "zoo-aws", "--region", fakelightsail.TestRegion)
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("err = %v, want a --yes refusal", err)
	}
	if !fake.Exists("zoo-aws") {
		t.Fatal("a refused destroy deleted the instance")
	}
	if n := fake.RequestCount(); n != 0 {
		t.Fatalf("%d request(s) reached AWS for a refused destroy", n)
	}
}

// An instance without dhnt:managed=true is refused with the reason, not
// with the AccessDeniedException AWS would answer.
func TestInfraDestroyRefusesAnUntaggedInstance(t *testing.T) {
	isolatedFleetRoot(t)
	fake := useFakeLightsail(t)
	clearAWSCredentialEnv(t)
	awsCredentialEnv(t)
	// Owned by this owner, but never tagged managed.
	fake.AddInstance("wild-box", "running", provider.LeaseTags("local", "k", nil)...)

	_, err := runInfra(t, "destroy", "aws", "--name", "wild-box",
		"--region", fakelightsail.TestRegion, "--yes")
	if err == nil {
		t.Fatal("an untagged instance must not be destroyed")
	}
	if !strings.Contains(err.Error(), awsprovider.TagManagedKey) {
		t.Fatalf("the refusal must name the tag: %v", err)
	}
	if !fake.Exists("wild-box") {
		t.Fatal("the refused destroy deleted the instance anyway")
	}
	if n := fake.DeleteCount(); n != 0 {
		t.Fatalf("DeleteInstance calls = %d, want 0", n)
	}
}

// Destroying what is already gone succeeds and still releases the address
// the host left behind — the one thing that keeps billing on its own.
func TestInfraDestroyIsIdempotent(t *testing.T) {
	isolatedFleetRoot(t)
	fake := useFakeLightsail(t)
	clearAWSCredentialEnv(t)
	awsCredentialEnv(t)
	seedAWSInstance(fake, "zoo-aws")
	fake.AddStaticIP("zoo-aws-ip", "zoo-aws")

	if _, err := runInfra(t, "destroy", "aws", "--name", "zoo-aws",
		"--region", fakelightsail.TestRegion, "--yes"); err != nil {
		t.Fatalf("first destroy: %v", err)
	}
	// A second run under a FRESH key: the journal cannot answer it, so
	// this reaches the adapter with nothing left to delete.
	out, err := runInfra(t, "destroy", "aws", "--name", "zoo-aws",
		"--region", fakelightsail.TestRegion, "--yes", "--key", "infra-destroy-zoo-aws-2")
	if err != nil {
		t.Fatalf("repeat destroy: %v\n%s", err, out)
	}
	if !strings.Contains(out, "[skipped] delete-instance") {
		t.Fatalf("output does not say the instance was already gone:\n%s", out)
	}
	if !strings.Contains(out, "zoo-aws: deleted") {
		t.Fatalf("repeat destroy did not report the resource as deleted:\n%s", out)
	}
}

// An orphaned address — a provision that reserved one and then failed —
// is what a repeat destroy is for.
func TestInfraDestroyReleasesAnOrphanedAddress(t *testing.T) {
	isolatedFleetRoot(t)
	fake := useFakeLightsail(t)
	clearAWSCredentialEnv(t)
	awsCredentialEnv(t)
	fake.AddStaticIP("ghost-ip", "")

	out, err := runInfra(t, "destroy", "aws", "--name", "ghost",
		"--region", fakelightsail.TestRegion, "--yes")
	if err != nil {
		t.Fatalf("destroy: %v\n%s", err, out)
	}
	if !strings.Contains(out, "[done] release-static-ip ghost-ip") {
		t.Fatalf("the orphaned address was not released:\n%s", out)
	}
	if n := fake.StaticIPCount(); n != 0 {
		t.Fatalf("static ips = %d, want 0", n)
	}
}

func TestInfraDestroyJSONCarriesTheSteps(t *testing.T) {
	isolatedFleetRoot(t)
	fake := useFakeLightsail(t)
	clearAWSCredentialEnv(t)
	awsCredentialEnv(t)
	seedAWSInstance(fake, "zoo-aws")
	fake.AddStaticIP("zoo-aws-ip", "zoo-aws")

	out, err := runInfra(t, "destroy", "aws", "--name", "zoo-aws",
		"--region", fakelightsail.TestRegion, "--yes", "--json")
	if err != nil {
		t.Fatalf("destroy --json: %v\n%s", err, out)
	}
	var row struct {
		ID    string `json:"id"`
		State string `json:"state"`
		Steps []struct {
			Step, Target, Status string
		} `json:"steps"`
	}
	if err := json.Unmarshal([]byte(out), &row); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if row.ID != "zoo-aws" || row.State != "deleted" {
		t.Fatalf("row = %+v", row)
	}
	if len(row.Steps) != 3 {
		t.Fatalf("steps = %+v, want one per billable thing", row.Steps)
	}
}

// The destroy flags mirror provision's: the provider is positional or
// --provider, and naming two different ones is an error.
func TestInfraLifecycleProviderNaming(t *testing.T) {
	isolatedFleetRoot(t)
	clearAWSCredentialEnv(t)

	_, err := runInfra(t, "destroy", "aws", "--provider", "digitalocean", "--name", "x", "--yes")
	if err == nil || !strings.Contains(err.Error(), "infra destroy names two providers") {
		t.Fatalf("err = %v, want a two-provider refusal", err)
	}
	_, err = runInfra(t, "inspect", "--name", "x")
	if err == nil || !strings.Contains(err.Error(), "aws, digitalocean") {
		t.Fatalf("err = %v, want the built-in list", err)
	}
}

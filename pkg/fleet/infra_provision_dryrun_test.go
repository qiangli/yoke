package fleet

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/provider/digitalocean/fakedo"
)

// Provision dry-run (S490 story a0e6f1ae): validate + print the exact
// request and run the size-in-region preflight through the provider's
// sizes API when a credential is present; create nothing. fakedo only:
// the public DO API is never touched (digitaloceanBaseURL points at the
// fake; without a credential the preflight is skipped before any call).

func clearDOCredentialEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"DIGITALOCEAN_API_KEY", "DIGITALOCEAN_TOKEN", "DIGITALOCEAN_KEY",
		"DIGITALOCEAN", "DIGITALOCEAN_ACCESS_TOKEN",
	} {
		t.Setenv(k, "")
	}
}

func runInfra(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := NewInfraCmd(WithoutCloudOverlay())
	cmd.SetArgs(args)
	var out strings.Builder
	cmd.SetOut(&out)
	err := cmd.Execute()
	return out.String(), err
}

func TestProvisionDryRunSkipsPreflightWithoutCredential(t *testing.T) {
	isolatedFleetRoot(t)
	clearDOCredentialEnv(t)

	out, err := runInfra(t, "provision", "digitalocean",
		"--satellite", "zoo", "--region", "sfo3", "--size", "s-4vcpu-1gb",
		"--image", "ubuntu-24-04-x64", "--profile", "standard", "--dry-run")
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	for _, want := range []string{
		"dry-run: creating nothing",
		"provider: digitalocean",
		"satellite: zoo",
		"region: sfo3",
		"size: s-4vcpu-1gb",
		"image: ubuntu-24-04-x64",
		"profile: standard",
		"idempotency_key: infra-provision-zoo",
		"preflight: skipped",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}

func TestProvisionDryRunCatchesSizeMismatch(t *testing.T) {
	isolatedFleetRoot(t)
	srv := fakedo.New(t)
	// s-1vcpu-1gb exists but not in sfo3: the live path would only learn
	// this as a 422 at create time; the dry-run must fail here instead.
	srv.SetCatalog(nil, []map[string]any{
		{"slug": "s-1vcpu-1gb", "price_monthly": 6.0, "price_hourly": 0.00893,
			"available": true, "regions": []string{"r1"}},
	})
	old := digitaloceanBaseURL
	digitaloceanBaseURL = srv.URL
	t.Cleanup(func() { digitaloceanBaseURL = old })
	clearDOCredentialEnv(t)
	t.Setenv("DIGITALOCEAN_ACCESS_TOKEN", fakedo.TestToken)

	_, err := runInfra(t, "provision", "digitalocean",
		"--satellite", "zoo", "--region", "sfo3", "--size", "s-1vcpu-1gb",
		"--image", "ubuntu-24-04-x64", "--dry-run")
	if err == nil {
		t.Fatal("dry-run succeeded for a size unavailable in the region")
	}
	if !strings.Contains(err.Error(), "sfo3") || !strings.Contains(err.Error(), "s-1vcpu-1gb") {
		t.Fatalf("error names neither size nor region: %v", err)
	}
	if n := srv.CreateCount(); n != 0 {
		t.Fatalf("dry-run created %d droplet(s)", n)
	}
}

func TestProvisionDryRunOKAndJSON(t *testing.T) {
	isolatedFleetRoot(t)
	srv := fakedo.New(t) // default catalog: s-1vcpu-1gb in r1
	old := digitaloceanBaseURL
	digitaloceanBaseURL = srv.URL
	t.Cleanup(func() { digitaloceanBaseURL = old })
	clearDOCredentialEnv(t)
	t.Setenv("DIGITALOCEAN_ACCESS_TOKEN", fakedo.TestToken)

	out, err := runInfra(t, "provision", "digitalocean",
		"--satellite", "zoo", "--region", "r1", "--size", "s-1vcpu-1gb",
		"--image", "ubuntu-24-04-x64", "--dry-run")
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if !strings.Contains(out, "available in region r1") {
		t.Fatalf("no availability verdict:\n%s", out)
	}
	if strings.Contains(out, fakedo.TestToken) {
		t.Fatal("credential printed in dry-run output")
	}

	jout, err := runInfra(t, "provision", "digitalocean",
		"--satellite", "zoo", "--region", "r1", "--size", "s-1vcpu-1gb",
		"--image", "ubuntu-24-04-x64", "--profile", "third_party",
		"--key", "test-key-1", "--dry-run", "--json")
	if err != nil {
		t.Fatalf("dry-run --json: %v", err)
	}
	var doc struct {
		DryRun  bool `json:"dry_run"`
		Request struct {
			Provider       string `json:"provider"`
			Region         string `json:"region"`
			Size           string `json:"size"`
			Image          string `json:"image"`
			Profile        string `json:"profile"`
			IdempotencyKey string `json:"idempotency_key"`
		} `json:"request"`
		Preflight struct {
			Checked bool `json:"checked"`
			OK      bool `json:"ok"`
		} `json:"preflight"`
	}
	if err := json.Unmarshal([]byte(jout), &doc); err != nil {
		t.Fatalf("dry-run --json is not JSON: %v:\n%s", err, jout)
	}
	if !doc.DryRun || !doc.Preflight.Checked || !doc.Preflight.OK {
		t.Fatalf("unexpected verdict: %+v", doc)
	}
	if doc.Request.Provider != "digitalocean" || doc.Request.Region != "r1" ||
		doc.Request.Size != "s-1vcpu-1gb" || doc.Request.Profile != "third_party" ||
		doc.Request.IdempotencyKey != "test-key-1" {
		t.Fatalf("unexpected request: %+v", doc.Request)
	}
	if strings.Contains(jout, fakedo.TestToken) {
		t.Fatal("credential printed in --json output")
	}
	if n := srv.CreateCount(); n != 0 {
		t.Fatalf("dry-run created %d droplet(s)", n)
	}
}

func TestProvisionDryRunRejectsBadProfileAndKey(t *testing.T) {
	isolatedFleetRoot(t)
	clearDOCredentialEnv(t)

	if _, err := runInfra(t, "provision", "digitalocean",
		"--satellite", "zoo", "--profile", "bogus", "--dry-run"); err == nil ||
		!strings.Contains(err.Error(), "unknown profile") {
		t.Fatalf("bad profile: err = %v", err)
	}
	if _, err := runInfra(t, "provision", "digitalocean",
		"--satellite", "zoo", "--key", "has space", "--dry-run"); err == nil ||
		!strings.Contains(err.Error(), "idempotency key") {
		t.Fatalf("bad key: err = %v", err)
	}
}

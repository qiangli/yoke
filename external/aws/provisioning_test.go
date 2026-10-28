package aws

import (
	"strings"
	"testing"

	awsprovider "github.com/qiangli/yoke/pkg/provider/aws"
	"github.com/qiangli/yoke/pkg/provider/aws/fakelightsail"
)

// ProvisioningAllowed (Sprint 522 story ed3ef42edf96): the LIVE
// authorization check, wired to the Lightsail adapter's budget preflight.
// fakelightsail only — the real AWS API is never touched.

// The two aws packages share env NAMES, not code: pkg/provider is a
// nested stdlib-only module and cannot import this one, so each declares
// its own constants. This pins them together, because a rename on either
// side would silently split the credential ladder in two.
func TestEnvNamesMatchTheProviderAdapter(t *testing.T) {
	for _, pair := range [][2]string{
		{EnvAccessKeyID, awsprovider.EnvAccessKeyID},
		{EnvSecretAccessKey, awsprovider.EnvSecretAccessKey},
		{EnvRegion, awsprovider.EnvRegion},
		{EnvDefaultRegion, awsprovider.EnvDefaultRegion},
	} {
		if pair[0] != pair[1] {
			t.Errorf("env name drift: external/aws has %q, pkg/provider/aws has %q", pair[0], pair[1])
		}
	}
	// Known asymmetry, stated rather than left to be discovered: only the
	// provider adapter reads a session token, because only it signs
	// requests. external/aws hands credential SELECTION to the aws child,
	// which resolves the token itself.
	if awsprovider.EnvSessionToken != "AWS_SESSION_TOKEN" {
		t.Errorf("provider session-token env name = %q", awsprovider.EnvSessionToken)
	}
}

func awsPreflightEnv(t *testing.T) {
	t.Helper()
	clearCredEnv(t)
	t.Setenv(EnvProfile, "fixture")
	t.Setenv(EnvAccessKeyID, fakelightsail.TestAccessKeyID)
	t.Setenv(EnvSecretAccessKey, fakelightsail.TestSecretAccessKey)
}

func fixtureCredentials(t *testing.T) Credentials {
	t.Helper()
	c, err := ResolveCredentials()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func intent(srv *fakelightsail.Server, bundle string) ProvisionIntent {
	return ProvisionIntent{
		Region:          fakelightsail.TestRegion,
		BundleID:        bundle,
		BlueprintID:     "ubuntu_24_04",
		LightsailURL:    srv.URL,
		CostExplorerURL: srv.URL,
	}
}

// A bundle within both caps is permitted, and the verdict carries the
// numbers it was computed from.
func TestProvisioningAllowedWithinBudget(t *testing.T) {
	srv := fakelightsail.New(t)
	awsPreflightEnv(t)
	v, perr := ProvisioningAllowed(t.Context(), fixtureCredentials(t), intent(srv, "small_3_0"))
	if perr != nil {
		t.Fatalf("ProvisioningAllowed: %v", perr)
	}
	if !v.Checked || !v.OK || !v.MonthToDateKnown {
		t.Fatalf("verdict = %+v", v)
	}
	if v.BundleMonthlyUSD != 12 || v.MonthToDateUSD != 3.5 || v.ProjectedMonthlyUSD != 15.5 {
		t.Fatalf("verdict numbers = %+v", v)
	}
	if v.MonthlyCapUSD != 50 || v.PerInstanceCapUSD != 24 {
		t.Fatalf("caps = %v/%v, want the operator's $50/$24", v.MonthlyCapUSD, v.PerInstanceCapUSD)
	}
	if srv.CreateCount() != 0 {
		t.Fatal("the authorization check created something")
	}
	if srv.BadSignatures() != 0 {
		t.Fatal("a request failed SigV4 verification")
	}
}

// Over the monthly cap is a permission denial, with the verdict still
// returned so the operator sees the arithmetic.
func TestProvisioningRefusedOverMonthlyCap(t *testing.T) {
	srv := fakelightsail.New(t)
	srv.SetMonthToDate(45.0) // 45 + 12 = 57 > 50
	awsPreflightEnv(t)
	v, perr := ProvisioningAllowed(t.Context(), fixtureCredentials(t), intent(srv, "small_3_0"))
	if perr == nil {
		t.Fatal("a create over the monthly cap must be refused")
	}
	if perr.Code != CodePermissionDenied {
		t.Fatalf("code = %q, want %q", perr.Code, CodePermissionDenied)
	}
	for _, want := range []string{"$45.00", "$57.00", "$50.00"} {
		if !strings.Contains(perr.Message, want) {
			t.Fatalf("denial does not name %q: %s", want, perr.Message)
		}
	}
	if v.OK || v.ProjectedMonthlyUSD != 57 {
		t.Fatalf("verdict = %+v, want the refused arithmetic", v)
	}
}

func TestProvisioningRefusedOverPerInstanceCeiling(t *testing.T) {
	srv := fakelightsail.New(t)
	awsPreflightEnv(t)
	_, perr := ProvisioningAllowed(t.Context(), fixtureCredentials(t), intent(srv, "xlarge_3_0"))
	if perr == nil || perr.Code != CodePermissionDenied {
		t.Fatalf("an $80/mo bundle must be refused: %v", perr)
	}
	if !strings.Contains(perr.Message, "per-instance ceiling") {
		t.Fatalf("denial = %s", perr.Message)
	}
}

// No GPU, ever — even priced under both caps.
func TestProvisioningRefusesGPU(t *testing.T) {
	srv := fakelightsail.New(t)
	awsPreflightEnv(t)
	_, perr := ProvisioningAllowed(t.Context(), fixtureCredentials(t), intent(srv, "gpu_nvidia_xl_1_0"))
	if perr == nil || perr.Code != CodePermissionDenied {
		t.Fatalf("a GPU bundle must be refused: %v", perr)
	}
	if !strings.Contains(perr.Message, "GPU") {
		t.Fatalf("denial = %s", perr.Message)
	}
}

// It fails CLOSED: Cost Explorer denied means spend is unknown, and
// unknown spend is not an authorization.
func TestProvisioningFailsClosedWhenSpendIsUnknown(t *testing.T) {
	srv := fakelightsail.New(t)
	srv.Inject("GetCostAndUsage", fakelightsail.FaultAccessDenied, 1)
	awsPreflightEnv(t)
	_, perr := ProvisioningAllowed(t.Context(), fixtureCredentials(t), intent(srv, "small_3_0"))
	if perr == nil || perr.Code != CodePermissionDenied {
		t.Fatalf("unknown spend must refuse: %v", perr)
	}
	if !strings.Contains(perr.Message, "month-to-date spend is unknown") {
		t.Fatalf("denial = %s", perr.Message)
	}
}

// An IAM refusal from Lightsail itself is a permission denial, and the
// live secret never reaches the message.
func TestProvisioningReportsAccessDeniedWithoutLeakingTheSecret(t *testing.T) {
	srv := fakelightsail.New(t)
	srv.Inject("GetBlueprints", fakelightsail.FaultAccessDenied, 1)
	awsPreflightEnv(t)
	_, perr := ProvisioningAllowed(t.Context(), fixtureCredentials(t), intent(srv, "small_3_0"))
	if perr == nil || perr.Code != CodePermissionDenied {
		t.Fatalf("AccessDenied must refuse: %v", perr)
	}
	if strings.Contains(perr.Message, fakelightsail.TestSecretAccessKey) {
		t.Fatal("the denial leaked the secret access key")
	}
	if perr.ProviderCode != "invalid_credentials" {
		t.Fatalf("provider code = %q", perr.ProviderCode)
	}
}

// Provider text that happens to carry the live secret is redacted, with a
// canary proving it.
func TestProvisioningRedactsACanarySecret(t *testing.T) {
	const canary = "s522-canary-secret-value"
	srv := fakelightsail.New(t)
	clearCredEnv(t)
	t.Setenv(EnvProfile, "fixture")
	t.Setenv(EnvAccessKeyID, fakelightsail.TestAccessKeyID)
	t.Setenv(EnvSecretAccessKey, canary)
	// The wrong secret makes every signature invalid, so the adapter's
	// error carries provider text; none of it may carry the canary.
	_, perr := ProvisioningAllowed(t.Context(), fixtureCredentials(t), intent(srv, "small_3_0"))
	if perr == nil {
		t.Fatal("a bad secret must refuse")
	}
	if strings.Contains(perr.Message, canary) {
		t.Fatalf("denial leaks the canary: %s", perr.Message)
	}
	if srv.BadSignatures() == 0 {
		t.Fatal("the fake should have rejected the bad signature")
	}
}

// An intent with no bundle is an input error, not a refusal: there is no
// "may I provision in general".
func TestProvisioningNeedsABundle(t *testing.T) {
	srv := fakelightsail.New(t)
	awsPreflightEnv(t)
	_, perr := ProvisioningAllowed(t.Context(), fixtureCredentials(t), intent(srv, ""))
	if perr == nil || perr.Code != CodeInvalidInput {
		t.Fatalf("missing bundle: %v", perr)
	}
	if !strings.Contains(perr.Message, "bundle id") {
		t.Fatalf("error = %s", perr.Message)
	}
}

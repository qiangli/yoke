package fleet

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/provider"
	awsprovider "github.com/qiangli/yoke/pkg/provider/aws"
	"github.com/qiangli/yoke/pkg/provider/aws/fakelightsail"
	"github.com/qiangli/yoke/pkg/provider/digitalocean"
)

// `infra provision --provider aws` (S522.2): provider selection, the
// per-provider flag defaults, and the dry-run's size + budget preflight.
// fakelightsail only — the real AWS API is never touched, and no
// environment variable can point the adapter at it by accident.

func clearAWSCredentialEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN",
		"AWS_REGION", "AWS_DEFAULT_REGION",
		"AWS_API_KEY", "AWS_TOKEN", "AWS_KEY", "AWS",
	} {
		t.Setenv(k, "")
	}
}

// useFakeLightsail points both built-in AWS endpoints at the fake for the
// duration of the test.
func useFakeLightsail(t *testing.T) *fakelightsail.Server {
	t.Helper()
	srv := fakelightsail.New(t)
	oldLS, oldCE := awsLightsailBaseURL, awsCostExplorerURL
	awsLightsailBaseURL, awsCostExplorerURL = srv.URL, srv.URL
	t.Cleanup(func() { awsLightsailBaseURL, awsCostExplorerURL = oldLS, oldCE })
	return srv
}

func awsCredentialEnv(t *testing.T) {
	t.Helper()
	t.Setenv("AWS_ACCESS_KEY_ID", fakelightsail.TestAccessKeyID)
	t.Setenv("AWS_SECRET_ACCESS_KEY", fakelightsail.TestSecretAccessKey)
}

// The embedded aws baseline loads, is marked built-in, and declares the
// Lightsail capability set (no resize: Lightsail cannot change a bundle in
// place).
func TestEmbeddedAWSProviderIsABuiltin(t *testing.T) {
	isolatedFleetRoot(t)
	p, ok := New(WithoutCloudOverlay()).InfraProvider("aws")
	if !ok {
		t.Fatal("embedded aws provider missing")
	}
	if !p.Builtin {
		t.Fatal("the aws entry must carry the loader-level builtin marker")
	}
	if p.Manifest.ProtocolVersion != provider.ProtocolVersion {
		t.Fatalf("protocol = %q", p.Manifest.ProtocolVersion)
	}
	for _, op := range []provider.Op{
		provider.OpValidate, provider.OpCapacity, provider.OpPlan, provider.OpCreate,
		provider.OpInspect, provider.OpList, provider.OpStart, provider.OpStop,
		provider.OpReboot, provider.OpDestroy,
	} {
		if !p.Manifest.Supports(op) {
			t.Fatalf("the aws manifest does not declare %s", op)
		}
	}
	if p.Manifest.Supports(provider.OpResize) {
		t.Fatal("Lightsail has no in-place bundle change; resize must not be declared")
	}
	b, isBuiltin := builtinFor(p)
	if !isBuiltin {
		t.Fatal("the aws entry does not resolve to a linked-in adapter")
	}
	if b.manifest().Name != awsprovider.Name {
		t.Fatalf("linked-in manifest = %q", b.manifest().Name)
	}
}

// Both compiled-in providers are registered, and each keeps its own
// vocabulary of defaults.
func TestBuiltinProvidersAndTheirDefaults(t *testing.T) {
	got := BuiltinInfraProviders()
	want := []string{"aws", "digitalocean"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("built-in providers = %v, want %v", got, want)
	}
	aws := infraDefaults(awsprovider.Name)
	if aws.Region != "us-west-2" || aws.Size != "small_3_0" || aws.Image != "ubuntu_24_04" {
		t.Fatalf("aws defaults = %+v, want us-west-2 / small_3_0 / ubuntu_24_04", aws)
	}
	do := infraDefaults(digitalocean.Name)
	if do.Region != "r1" || do.Size != "s-1vcpu-1gb" || do.Image != "img" {
		t.Fatalf("digitalocean defaults = %+v, want the historical values", do)
	}
	// An unknown name keeps the historical defaults so a declarative or
	// executable provider behaves exactly as before.
	if unknown := infraDefaults("someone-elses-cloud"); unknown != do {
		t.Fatalf("unknown provider defaults = %+v, want %+v", unknown, do)
	}
}

// The per-provider defaults are generated into the help text from the
// same registry RunE reads, so the help cannot claim one default while the
// command applies another. --region/--size/--image carry no cobra default
// at all, because the right one depends on the provider.
func TestProvisionHelpListsEveryProvidersDefaults(t *testing.T) {
	isolatedFleetRoot(t)
	out, err := runInfra(t, "provision", "--help")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"provider region (default: aws us-west-2, digitalocean r1)",
		"(default: aws small_3_0, digitalocean s-1vcpu-1gb)",
		"(default: aws ubuntu_24_04, digitalocean img)",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("help missing %q:\n%s", want, out)
		}
	}
	// Cobra prints its own `(default "x")` for a flag that has one; these
	// three must not, or the help would name two different defaults.
	for _, nope := range []string{`(default "r1")`, `(default "s-1vcpu-1gb")`, `(default "img")`} {
		if strings.Contains(out, nope) {
			t.Fatalf("help still claims a single cobra default %s:\n%s", nope, out)
		}
	}
}

// The provider may be named positionally or with --provider; runbooks
// write it both ways. Naming two DIFFERENT providers is an error, and
// naming none says what to do.
func TestProvisionAcceptsProviderPositionallyAndAsAFlag(t *testing.T) {
	isolatedFleetRoot(t)
	clearAWSCredentialEnv(t)

	positional, err := runInfra(t, "provision", "aws", "--satellite", "zoo-aws", "--dry-run")
	if err != nil {
		t.Fatalf("positional: %v", err)
	}
	flagged, err := runInfra(t, "provision", "--provider", "aws", "--satellite", "zoo-aws", "--dry-run")
	if err != nil {
		t.Fatalf("--provider: %v", err)
	}
	if positional != flagged {
		t.Fatalf("the two spellings disagree:\n%s\n---\n%s", positional, flagged)
	}
	// The same provider named twice is unambiguous, not an error.
	if _, err := runInfra(t, "provision", "aws", "--provider", "aws", "--satellite", "zoo-aws", "--dry-run"); err != nil {
		t.Fatalf("the same provider named twice: %v", err)
	}
	// Two different providers is a mistake worth stopping on.
	_, err = runInfra(t, "provision", "aws", "--provider", "digitalocean", "--satellite", "zoo", "--dry-run")
	if err == nil || !strings.Contains(err.Error(), "names two providers") {
		t.Fatalf("conflicting providers: err = %v", err)
	}
	// None at all says what to do.
	_, err = runInfra(t, "provision", "--satellite", "zoo", "--dry-run")
	if err == nil || !strings.Contains(err.Error(), "needs a provider") {
		t.Fatalf("no provider: err = %v", err)
	}
	if err != nil && !strings.Contains(err.Error(), "aws, digitalocean") {
		t.Fatalf("the error should list the built-ins: %v", err)
	}
}

// With no credential the dry-run still prints the request, and it prints
// the AWS defaults — not DigitalOcean's.
func TestProvisionDryRunAWSDefaultsWithoutCredential(t *testing.T) {
	isolatedFleetRoot(t)
	clearAWSCredentialEnv(t)

	out, err := runInfra(t, "provision", "aws", "--satellite", "zoo-aws", "--dry-run")
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	for _, want := range []string{
		"dry-run: creating nothing",
		"provider: aws",
		"satellite: zoo-aws",
		"region: us-west-2",
		"size: small_3_0",
		"image: ubuntu_24_04",
		"idempotency_key: infra-provision-zoo-aws",
		"preflight: skipped",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
	// The flag defaults must not leak across providers.
	if strings.Contains(out, "s-1vcpu-1gb") {
		t.Fatalf("aws provision used the DigitalOcean size default:\n%s", out)
	}
}

// An explicit flag always beats the provider default.
func TestProvisionDryRunAWSExplicitFlagsWin(t *testing.T) {
	isolatedFleetRoot(t)
	clearAWSCredentialEnv(t)

	out, err := runInfra(t, "provision", "aws", "--satellite", "zoo-aws",
		"--region", "us-east-1", "--size", "medium_3_0", "--image", "amazon_linux_2023", "--dry-run")
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	for _, want := range []string{"region: us-east-1", "size: medium_3_0", "image: amazon_linux_2023"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}

// With a credential the dry-run runs the real plan against Lightsail: the
// bundle price, the budget verdict and the full provider request, with
// nothing created and no key printed.
func TestProvisionDryRunAWSPreflightAndBudget(t *testing.T) {
	isolatedFleetRoot(t)
	srv := useFakeLightsail(t)
	clearAWSCredentialEnv(t)
	awsCredentialEnv(t)

	out, err := runInfra(t, "provision", "aws", "--satellite", "zoo-aws",
		"--region", fakelightsail.TestRegion, "--size", "small_3_0",
		"--image", "ubuntu_24_04", "--sprint", "522", "--story", "2816", "--dry-run")
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	for _, want := range []string{
		"($12.00/mo)",
		"budget: month-to-date spend $3.50",
		"within the $50.00 monthly cap",
		"\"bundle_id\":\"small_3_0\"",
		"\"arch\":\"x86_64\"",
		"\"static_ip_name\":\"zoo-aws-ip\"",
		"lightsail-connect",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, fakelightsail.TestSecretAccessKey) {
		t.Fatal("credential printed in dry-run output")
	}
	if n := srv.CreateCount(); n != 0 {
		t.Fatalf("dry-run created %d instance(s)", n)
	}
	if n := srv.BadSignatures(); n != 0 {
		t.Fatalf("%d request(s) failed SigV4 verification", n)
	}
}

// --json carries the numbers the verdict was computed from, so a caller
// does not have to parse prose.
func TestProvisionDryRunAWSJSON(t *testing.T) {
	isolatedFleetRoot(t)
	srv := useFakeLightsail(t)
	clearAWSCredentialEnv(t)
	awsCredentialEnv(t)

	jout, err := runInfra(t, "provision", "aws", "--satellite", "zoo-aws",
		"--region", fakelightsail.TestRegion, "--size", "small_3_0",
		"--image", "ubuntu_24_04", "--key", "aws-key-1", "--dry-run", "--json")
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
			IdempotencyKey string `json:"idempotency_key"`
		} `json:"request"`
		Preflight struct {
			Checked      bool                `json:"checked"`
			OK           bool                `json:"ok"`
			PriceMonthly float64             `json:"price_monthly"`
			Budget       awsprovider.Verdict `json:"budget"`
			Request      awsprovider.Plan    `json:"provider_request"`
		} `json:"preflight"`
	}
	if err := json.Unmarshal([]byte(jout), &doc); err != nil {
		t.Fatalf("dry-run --json is not JSON: %v:\n%s", err, jout)
	}
	if !doc.DryRun || !doc.Preflight.Checked || !doc.Preflight.OK {
		t.Fatalf("unexpected verdict: %+v", doc.Preflight)
	}
	if doc.Request.Provider != "aws" || doc.Request.Size != "small_3_0" ||
		doc.Request.Image != "ubuntu_24_04" || doc.Request.IdempotencyKey != "aws-key-1" {
		t.Fatalf("unexpected request: %+v", doc.Request)
	}
	if doc.Preflight.PriceMonthly != 12 {
		t.Fatalf("price_monthly = %v, want 12", doc.Preflight.PriceMonthly)
	}
	b := doc.Preflight.Budget
	if !b.Checked || !b.OK || !b.MonthToDateKnown {
		t.Fatalf("budget = %+v", b)
	}
	if b.MonthToDateUSD != 3.5 || b.ProjectedMonthlyUSD != 15.5 ||
		b.MonthlyCapUSD != 50 || b.PerInstanceCapUSD != 24 {
		t.Fatalf("budget numbers = %+v", b)
	}
	if p := doc.Preflight.Request; p.Arch != "x86_64" || p.StaticIPName != "zoo-aws-ip" || len(p.OpenPorts) != 6 {
		t.Fatalf("provider_request = %+v", p)
	}
	if strings.Contains(jout, fakelightsail.TestSecretAccessKey) {
		t.Fatal("credential printed in --json output")
	}
	if n := srv.CreateCount(); n != 0 {
		t.Fatalf("dry-run created %d instance(s)", n)
	}
	if n := srv.BadSignatures(); n != 0 {
		t.Fatalf("%d request(s) failed SigV4 verification", n)
	}
}

// A spend that would breach the cap fails the DRY RUN, so the operator
// learns before anything is created.
func TestProvisionDryRunAWSBudgetRefusalFails(t *testing.T) {
	isolatedFleetRoot(t)
	srv := useFakeLightsail(t)
	srv.SetMonthToDate(45.0) // 45 + 12 = 57 > the $50 cap
	clearAWSCredentialEnv(t)
	awsCredentialEnv(t)

	_, err := runInfra(t, "provision", "aws", "--satellite", "zoo-aws",
		"--region", fakelightsail.TestRegion, "--size", "small_3_0",
		"--image", "ubuntu_24_04", "--dry-run")
	if err == nil {
		t.Fatal("a dry-run over the budget cap must fail")
	}
	for _, want := range []string{"budget preflight refused create", "$45.00", "$57.00", "$50.00"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error does not name %q: %v", want, err)
		}
	}
	if n := srv.CreateCount(); n != 0 {
		t.Fatalf("dry-run created %d instance(s)", n)
	}
}

// The caps are overridable, so an operator can tighten (or, with an
// explicit decision, raise) them per run.
func TestProvisionDryRunAWSBudgetFlagsOverrideTheCaps(t *testing.T) {
	isolatedFleetRoot(t)
	srv := useFakeLightsail(t)
	clearAWSCredentialEnv(t)
	awsCredentialEnv(t)

	// A $10 per-instance ceiling refuses the $12/mo bundle the defaults accept.
	_, err := runInfra(t, "provision", "aws", "--satellite", "zoo-aws",
		"--region", fakelightsail.TestRegion, "--size", "small_3_0",
		"--image", "ubuntu_24_04", "--budget-per-instance", "10", "--dry-run")
	if err == nil || !strings.Contains(err.Error(), "per-instance ceiling") {
		t.Fatalf("tightened ceiling: err = %v", err)
	}
	// And a $14 cap refuses it on the monthly total ($3.50 + $12).
	_, err = runInfra(t, "provision", "aws", "--satellite", "zoo-aws",
		"--region", fakelightsail.TestRegion, "--size", "small_3_0",
		"--image", "ubuntu_24_04", "--budget-cap", "14", "--dry-run")
	if err == nil || !strings.Contains(err.Error(), "$14.00 monthly cap") {
		t.Fatalf("tightened cap: err = %v", err)
	}
	if n := srv.CreateCount(); n != 0 {
		t.Fatalf("dry-run created %d instance(s)", n)
	}
}

// A size that does not exist in the region fails the dry-run rather than
// becoming a 400 at create time.
func TestProvisionDryRunAWSCatchesUnknownBundle(t *testing.T) {
	isolatedFleetRoot(t)
	srv := useFakeLightsail(t)
	clearAWSCredentialEnv(t)
	awsCredentialEnv(t)

	_, err := runInfra(t, "provision", "aws", "--satellite", "zoo-aws",
		"--region", fakelightsail.TestRegion, "--size", "nano_99_9",
		"--image", "ubuntu_24_04", "--dry-run")
	if err == nil {
		t.Fatal("dry-run succeeded for an unknown bundle")
	}
	if !strings.Contains(err.Error(), "nano_99_9") {
		t.Fatalf("error does not name the bundle: %v", err)
	}
	if n := srv.CreateCount(); n != 0 {
		t.Fatalf("dry-run created %d instance(s)", n)
	}
}

// AWS needs a key PAIR, and the dispatcher resolves only one credential
// string. A secret with no key id alongside it must say exactly which
// variable is missing, not fail as a signature mismatch against AWS.
func TestProvisionAWSSecretWithoutKeyIDSaysWhatIsMissing(t *testing.T) {
	isolatedFleetRoot(t)
	useFakeLightsail(t)
	clearAWSCredentialEnv(t)
	t.Setenv("AWS_SECRET_ACCESS_KEY", fakelightsail.TestSecretAccessKey)

	_, err := runInfra(t, "provision", "aws", "--satellite", "zoo-aws",
		"--region", fakelightsail.TestRegion, "--size", "small_3_0",
		"--image", "ubuntu_24_04", "--dry-run")
	if err == nil {
		t.Fatal("a secret with no key id must not be accepted")
	}
	if !strings.Contains(err.Error(), "AWS_ACCESS_KEY_ID") {
		t.Fatalf("error does not name the missing variable: %v", err)
	}
	if strings.Contains(err.Error(), fakelightsail.TestSecretAccessKey) {
		t.Fatalf("error leaked the secret: %v", err)
	}
}

// The AWS credential resolver wants the key pair the vault projects, and
// its refusal names the variables without echoing any value.
func TestAWSCredentialResolution(t *testing.T) {
	clearAWSCredentialEnv(t)
	if _, err := resolveProviderCredential("aws"); err == nil {
		t.Fatal("no credential must be an error")
	} else if !strings.Contains(err.Error(), "AWS_SECRET_ACCESS_KEY") {
		t.Fatalf("error does not name the secret variable: %v", err)
	}
	t.Setenv("AWS_SECRET_ACCESS_KEY", "s3cr3t-value")
	got, err := resolveProviderCredential("aws")
	if err != nil {
		t.Fatal(err)
	}
	if got != "s3cr3t-value" {
		t.Fatalf("resolved credential = %q", got)
	}
}

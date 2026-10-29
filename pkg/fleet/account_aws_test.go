package fleet

// `--account` for the AWS family (S524.7, slice D).
//
// In-package, because the proof needs the provider's base-URL seams
// (awsLightsailBaseURL and friends) — the only way to run a provision against
// a fake instead of a live account.
//
// The end-to-end assertion is a SIGNATURE: fakelightsail verifies SigV4
// against one key pair, so a dry run that succeeds proves the account's keys
// signed the requests, and a dry run with a different account's keys fails
// verification. That is stronger than inspecting what we passed along —
// it is read off the wire.

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/qiangli/yoke/pkg/accounts/accountstest"
	awsprovider "github.com/qiangli/yoke/pkg/provider/aws"
	"github.com/qiangli/yoke/pkg/provider/aws/fakelightsail"
	"github.com/qiangli/yoke/pkg/provider/digitalocean"
)

// twoProfileResolver is two AWS accounts, alice and bob, each holding its own
// key pair. Only alice's pair is the one the fake accepts.
func twoProfileResolver(t *testing.T) {
	t.Helper()
	keys := map[string]awsprovider.Credentials{
		"alice": {AccessKeyID: fakelightsail.TestAccessKeyID, SecretAccessKey: fakelightsail.TestSecretAccessKey},
		"bob":   {AccessKeyID: "AKIDOTTER", SecretAccessKey: "otter-secret"},
	}
	prev := AccountResolver
	AccountResolver = func(_ context.Context, ref, _ string) (AccountSelection, error) {
		k, ok := keys[ref]
		if !ok {
			return AccountSelection{}, accountFail(accountCodeUnresolved, "no such account")
		}
		return AccountSelection{Ref: ref, AccountID: "acc_" + ref, Category: AccountAWS, Profile: ref, AWSKeys: k}, nil
	}
	t.Cleanup(func() { AccountResolver = prev })
}

// Two AWS profiles stay separate: the account named on the command line is the
// identity that signs, and the ambient environment supplies nothing.
func TestProvisionAWSAccountSignsWithItsOwnProfileKeys(t *testing.T) {
	isolatedFleetRoot(t)
	srv := useFakeLightsail(t)
	clearAWSCredentialEnv(t)
	twoProfileResolver(t)

	out, err := runInfra(t, "provision", "aws", "--satellite", "zoo-aws",
		"--region", fakelightsail.TestRegion, "--size", "small_3_0",
		"--image", "ubuntu_24_04", "--dry-run", "--account", "alice")
	if err != nil {
		t.Fatalf("dry-run --account alice: %v", err)
	}
	if !strings.Contains(out, "($12.00/mo)") {
		t.Fatalf("preflight did not run under the account's keys:\n%s", out)
	}
	if n := srv.BadSignatures(); n != 0 {
		t.Fatalf("%d request(s) failed SigV4 under alice's keys", n)
	}
	if n := srv.CreateCount(); n != 0 {
		t.Fatalf("dry-run created %d instance(s)", n)
	}

	// bob's keys are a different identity, and they are the ones presented:
	// the fake rejects them. If the account were ignored (or alice's keys
	// leaked across), this would pass instead.
	before := srv.BadSignatures()
	_, err = runInfra(t, "provision", "aws", "--satellite", "zoo-aws",
		"--region", fakelightsail.TestRegion, "--size", "small_3_0",
		"--image", "ubuntu_24_04", "--dry-run", "--account", "bob")
	if err == nil {
		t.Fatal("bob's key pair must not be accepted by a fake that only knows alice's")
	}
	if srv.BadSignatures() <= before {
		t.Fatal("bob's request never reached signature verification — the account was not used")
	}
	if strings.Contains(err.Error(), "otter-secret") {
		t.Fatal("a provider error carried credential material")
	}
}

// An account that cannot supply this provider's credential refuses; it never
// slides onto the ambient environment pair.
func TestAWSAccountWithoutKeysRefuses(t *testing.T) {
	prev := AccountResolver
	AccountResolver = func(context.Context, string, string) (AccountSelection, error) {
		return AccountSelection{Ref: "alice", AccountID: "acc_alice", Category: AccountAWS, Profile: "alice"}, nil
	}
	t.Cleanup(func() { AccountResolver = prev })

	_, err := resolveProviderCredentialFor("aws", &AccountSelection{
		Ref: "alice", Category: AccountAWS, Profile: "alice",
	})
	if code := AccountErrorCode(err); code != accountCodeCredential {
		t.Fatalf("code = %q (%v), want %s", code, err, accountCodeCredential)
	}
	if _, err := awsAccountCredentials(builtinOpenOptions{Account: &AccountSelection{
		Ref: "alice", Category: AccountAWS,
	}}); AccountErrorCode(err) != accountCodeCredential {
		t.Fatalf("awsAccountCredentials: %v", err)
	}
}

// The dispatcher credential is per invocation and lives on the stack, so two
// goroutines resolving two accounts cannot observe each other's.
func TestProviderCredentialPerInvocationNeverCrosses(t *testing.T) {
	do := &AccountSelection{Ref: "owl", Category: AccountDigitalOcean, Token: "token-owl"}
	aws := &AccountSelection{Ref: "alice", Category: AccountAWS,
		AWSKeys: awsprovider.Credentials{AccessKeyID: "AKIDOWL", SecretAccessKey: "alice-secret"}}

	var wg sync.WaitGroup
	fail := make(chan string, 64)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				got, err := resolveProviderCredentialFor(digitalocean.Name, do)
				if err != nil || got != "token-owl" {
					fail <- "digitalocean got " + got
				}
				return
			}
			got, err := resolveProviderCredentialFor("aws", aws)
			if err != nil || got != "alice-secret" {
				fail <- "aws got " + got
			}
		}(i)
	}
	wg.Wait()
	close(fail)
	for msg := range fail {
		t.Fatal("credentials crossed between accounts: " + msg)
	}
}

// Without an account the provider credential resolution is the historical
// environment ladder, untouched.
func TestProviderCredentialWithoutAccountIsUnchanged(t *testing.T) {
	clearAWSCredentialEnv(t)
	t.Setenv("DIGITALOCEAN_ACCESS_TOKEN", "ambient-do")
	got, err := resolveProviderCredentialFor(digitalocean.Name, nil)
	if err != nil || got != "ambient-do" {
		t.Fatalf("got (%q, %v), want the ambient environment token", got, err)
	}
}

// A non-provider category cannot supply a provider credential, and the refusal
// carries no secret.
func TestProviderCredentialRejectsForeignCategory(t *testing.T) {
	_, err := resolveProviderCredentialFor("aws", &AccountSelection{
		Ref: "owl-box", Category: AccountCloudbox, Token: accountstest.CanarySecret,
	})
	if code := AccountErrorCode(err); code != accountCodeUnsupported {
		t.Fatalf("code = %q (%v), want %s", code, err, accountCodeUnsupported)
	}
	accountstest.AssertNoSecret(t, err.Error())
}

// A provider with no account category refuses --account by name rather than
// ignoring it.
func TestInfraAccountOnProviderWithoutACategory(t *testing.T) {
	flag := &accountFlag{ref: "owl"}
	_, err := resolveInfraAccount(context.Background(), flag, "some-declarative-provider")
	if code := AccountErrorCode(err); code != accountCodeUnsupported {
		t.Fatalf("code = %q (%v), want %s", code, err, accountCodeUnsupported)
	}
	if _, ok := accountCategoryForProvider(digitalocean.Name); !ok {
		t.Fatal("digitalocean must have an account category")
	}
	if _, ok := accountCategoryForProvider(awsprovider.Name); !ok {
		t.Fatal("aws must have an account category")
	}
}

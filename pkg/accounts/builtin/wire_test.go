package builtin

// End-to-end `--account` through the WIRED resolvers (S524.7, slice D): a real
// host-local store, the real credential materialization, and the real
// pkg/fleet / pkg/secrets seams. This package is the only one that can see all
// three, which is why the join is tested here.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/qiangli/yoke/pkg/accounts"
	"github.com/qiangli/yoke/pkg/accounts/accountstest"
	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/secrets"
)

// Linking this package must install both seams; without them `--account`
// refuses everywhere rather than guessing an identity.
func TestAccountResolversAreWired(t *testing.T) {
	if fleet.AccountResolver == nil {
		t.Fatal("fleet.AccountResolver is not wired")
	}
	if secrets.AccountResolver == nil {
		t.Fatal("secrets.AccountResolver is not wired")
	}
}

func isolatedStore(t *testing.T) *accounts.Store {
	t.Helper()
	t.Setenv("BASHY_HOME", t.TempDir())
	return accounts.Open(accounts.DefaultPath())
}

// observe records one resolvable account and returns its local id.
func observe(t *testing.T, st *accounts.Store, adapter string, a accounts.Account) string {
	t.Helper()
	if err := st.Observe(adapter, []accounts.Observation{{Account: a, SourceKey: a.ProviderAccountID}}); err != nil {
		t.Fatalf("Observe: %v", err)
	}
	list, err := st.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, acct := range list {
		if acct.ProviderAccountID == a.ProviderAccountID {
			if acct.Status != accounts.Resolved {
				t.Fatalf("account %s is %s, want resolved", acct.ID, acct.Status)
			}
			return acct.ID
		}
	}
	t.Fatalf("observed account %q not in the store", a.ProviderAccountID)
	return ""
}

// Two DigitalOcean teams on one host: each id resolves to its OWN token, and
// neither sees the other's.
func TestFleetResolverKeepsTwoDigitalOceanTeamsApart(t *testing.T) {
	st := isolatedStore(t)
	t.Setenv("DIGITALOCEAN_ACCESS_TOKEN", "token-owl")
	t.Setenv("DIGITALOCEAN_TOKEN", "token-otter")

	owl := observe(t, st, "digitalocean", accounts.Account{
		Category: accounts.DigitalOcean, Provider: "digitalocean", ProviderAccountID: "team-owl",
		Team: "owl", Connections: []accounts.Connection{{ID: "owl", CredentialRef: "env:DIGITALOCEAN_ACCESS_TOKEN"}},
	})
	otter := observe(t, st, "digitalocean", accounts.Account{
		Category: accounts.DigitalOcean, Provider: "digitalocean", ProviderAccountID: "team-otter",
		Team: "otter", Connections: []accounts.Connection{{ID: "otter", CredentialRef: "env:DIGITALOCEAN_TOKEN"}},
	})

	for ref, want := range map[string]string{owl: "token-owl", otter: "token-otter"} {
		sel, err := fleet.AccountResolver(context.Background(), ref, "infra")
		if err != nil {
			t.Fatalf("resolve %s: %v", ref, err)
		}
		if sel.Category != fleet.AccountDigitalOcean {
			t.Fatalf("category = %q", sel.Category)
		}
		if sel.Token != want {
			t.Fatalf("account %s resolved token %q, want %q", ref, sel.Token, want)
		}
		if sel.AWSKeys.AccessKeyID != "" {
			t.Fatal("a DigitalOcean selection carried AWS keys")
		}
	}
}

// Two AWS profiles: each account reads only its own section of the shared
// credentials file, and the pair never collapses into a single token.
func TestFleetResolverKeepsTwoAWSProfilesApart(t *testing.T) {
	st := isolatedStore(t)
	for _, k := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AWS_PROFILE"} {
		t.Setenv(k, "")
	}
	path := filepath.Join(t.TempDir(), "credentials")
	body := "[alice]\naws_access_key_id = AKIDALICE\naws_secret_access_key = alice-secret\n" +
		"[bob]\naws_access_key_id = AKIDBOB\naws_secret_access_key = bob-secret\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", path)

	alice := observe(t, st, "aws", accounts.Account{
		Category: accounts.AWS, Provider: "aws", ProviderAccountID: "111111111111",
		Connections: []accounts.Connection{{ID: "alice", Profile: "alice", CredentialRef: "profile:alice"}},
	})
	bob := observe(t, st, "aws", accounts.Account{
		Category: accounts.AWS, Provider: "aws", ProviderAccountID: "222222222222",
		Connections: []accounts.Connection{{ID: "bob", Profile: "bob", CredentialRef: "profile:bob"}},
	})

	for ref, want := range map[string][2]string{
		alice: {"AKIDALICE", "alice-secret"},
		bob:   {"AKIDBOB", "bob-secret"},
	} {
		sel, err := fleet.AccountResolver(context.Background(), ref, "infra")
		if err != nil {
			t.Fatalf("resolve %s: %v", ref, err)
		}
		if sel.Category != fleet.AccountAWS {
			t.Fatalf("category = %q", sel.Category)
		}
		if sel.AWSKeys.AccessKeyID != want[0] || sel.AWSKeys.SecretAccessKey != want[1] {
			t.Fatalf("account %s resolved %q/%q, want %q/%q", ref,
				sel.AWSKeys.AccessKeyID, sel.AWSKeys.SecretAccessKey, want[0], want[1])
		}
		if sel.Token != "" {
			t.Fatal("an AWS key pair also appeared as a single token")
		}
		if sel.Profile == "" {
			t.Fatal("the selection lost the profile it came from")
		}
	}
}

func writeCloudboxProfile(t *testing.T, root, name, server, token string) {
	t.Helper()
	dir := filepath.Join(root, "profiles", name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"server_addr":"` + server + `","access_token":"` + token + `"}`
	if err := os.WriteFile(filepath.Join(dir, "agent.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Two cloudbox endpoints: `bashy secret --account` must talk to the account's
// own vault with the account's own bearer, and nothing from the ambient ladder.
func TestSecretsResolverKeepsTwoCloudboxEndpointsApart(t *testing.T) {
	st := isolatedStore(t)
	cfgHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfgHome)
	root := filepath.Join(cfgHome, "matrix")
	writeCloudboxProfile(t, root, "owl", "https://owl.example.com", "bearer-owl")
	writeCloudboxProfile(t, root, "otter", "https://otter.example.com", "bearer-otter")
	// An ambient credential that must never be chosen once --account is given.
	t.Setenv("BASHY_CLOUDBOX_URL", "https://ambient.example.com")
	t.Setenv("BASHY_SECRETS_TOKEN", "ambient-token")

	owl := observe(t, st, "cloudbox", accounts.Account{
		Category: accounts.Cloudbox, Provider: "cloudbox", ProviderAccountID: "owl@owl.example.com",
		Username: "owl",
		Connections: []accounts.Connection{{ID: "owl", Endpoint: "https://owl.example.com",
			Profile: "owl", CredentialRef: "profile:owl"}},
	})
	otter := observe(t, st, "cloudbox", accounts.Account{
		Category: accounts.Cloudbox, Provider: "cloudbox", ProviderAccountID: "otter@otter.example.com",
		Username: "otter",
		Connections: []accounts.Connection{{ID: "otter", Endpoint: "https://otter.example.com",
			Profile: "otter", CredentialRef: "profile:otter"}},
	})

	for ref, want := range map[string][2]string{
		owl:   {"https://owl.example.com", "bearer-owl"},
		otter: {"https://otter.example.com", "bearer-otter"},
	} {
		client, err := (secrets.Config{Account: ref}).Resolve()
		if err != nil {
			t.Fatalf("secret --account %s: %v", ref, err)
		}
		if client.BaseURL != want[0] || client.Token != want[1] {
			t.Fatalf("account %s resolved (%q, %q), want (%q, %q)",
				ref, client.BaseURL, client.Token, want[0], want[1])
		}
	}

	// The same accounts drive the cloudbox-facing fleet verbs.
	for ref, want := range map[string][2]string{
		owl:   {"https://owl.example.com", "bearer-owl"},
		otter: {"https://otter.example.com", "bearer-otter"},
	} {
		sel, err := fleet.AccountResolver(context.Background(), ref, "cloudbox")
		if err != nil {
			t.Fatalf("resolve %s: %v", ref, err)
		}
		base, token, err := fleet.ResolveCloudAccount(&sel, "", "")
		if err != nil {
			t.Fatalf("ResolveCloudAccount %s: %v", ref, err)
		}
		if base != want[0] || token != want[1] {
			t.Fatalf("account %s resolved (%q, %q), want (%q, %q)", ref, base, token, want[0], want[1])
		}
	}
}

// Without --account the secrets ladder is exactly what it was.
func TestSecretsWithoutAccountIsUnchanged(t *testing.T) {
	isolatedStore(t)
	t.Setenv("BASHY_CLOUDBOX_URL", "https://ambient.example.com")
	t.Setenv("BASHY_SECRETS_TOKEN", "ambient-token")
	client, err := (secrets.Config{}).Resolve()
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if client.BaseURL != "https://ambient.example.com" || client.Token != "ambient-token" {
		t.Fatalf("got (%q, %q), want the ambient ladder", client.BaseURL, client.Token)
	}
}

// The refusals, all of which must happen before a request could be made: a
// conflicting explicit target, a category that cannot drive the verb, and an
// account that does not exist.
func TestSecretsAccountRefusals(t *testing.T) {
	st := isolatedStore(t)
	t.Setenv("DIGITALOCEAN_ACCESS_TOKEN", accountstest.CanarySecret)
	do := observe(t, st, "digitalocean", accounts.Account{
		Category: accounts.DigitalOcean, Provider: "digitalocean", ProviderAccountID: "team-owl",
		Connections: []accounts.Connection{{ID: "owl", CredentialRef: "env:DIGITALOCEAN_ACCESS_TOKEN"}},
	})

	for _, tc := range []struct {
		name string
		cfg  secrets.Config
		want string
	}{
		{"conflicting url", secrets.Config{Account: do, URL: "https://other.example.com"}, "account_conflict"},
		{"conflicting token", secrets.Config{Account: do, Token: "other"}, "account_conflict"},
		{"wrong category", secrets.Config{Account: do}, "account_unsupported"},
		{"no such account", secrets.Config{Account: "nobody"}, "account_unresolved"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.cfg.Resolve()
			if err == nil {
				t.Fatal("want a refusal")
			}
			var code string
			if c, ok := err.(interface{ ErrorCode() string }); ok {
				code = c.ErrorCode()
			}
			if code != tc.want {
				t.Fatalf("code = %q (%v), want %q", code, err, tc.want)
			}
			accountstest.AssertNoSecret(t, err.Error())
		})
	}
}

// Two accounts resolved in parallel must never cross, including through the
// real store and the real credential materialization.
func TestWiredResolverConcurrentAccountsNeverCross(t *testing.T) {
	st := isolatedStore(t)
	t.Setenv("DIGITALOCEAN_ACCESS_TOKEN", "token-owl")
	t.Setenv("DIGITALOCEAN_TOKEN", "token-otter")
	owl := observe(t, st, "digitalocean", accounts.Account{
		Category: accounts.DigitalOcean, Provider: "digitalocean", ProviderAccountID: "team-owl",
		Connections: []accounts.Connection{{ID: "owl", CredentialRef: "env:DIGITALOCEAN_ACCESS_TOKEN"}},
	})
	otter := observe(t, st, "digitalocean", accounts.Account{
		Category: accounts.DigitalOcean, Provider: "digitalocean", ProviderAccountID: "team-otter",
		Connections: []accounts.Connection{{ID: "otter", CredentialRef: "env:DIGITALOCEAN_TOKEN"}},
	})
	want := map[string]string{owl: "token-owl", otter: "token-otter"}

	var wg sync.WaitGroup
	bad := make(chan string, 64)
	for i := 0; i < 32; i++ {
		ref := owl
		if i%2 == 1 {
			ref = otter
		}
		wg.Add(1)
		go func(ref string) {
			defer wg.Done()
			sel, err := fleet.AccountResolver(context.Background(), ref, "infra")
			if err != nil || sel.Token != want[ref] {
				bad <- ref + " got " + sel.Token
			}
		}(ref)
	}
	wg.Wait()
	close(bad)
	for msg := range bad {
		t.Fatal("identities crossed between concurrent resolutions: " + msg)
	}
}

// A selection's safe metadata and every refusal are free of credential
// material — the sentinel never appears, in any field or any message.
func TestWiredResolverLeaksNoSecret(t *testing.T) {
	st := isolatedStore(t)
	t.Setenv("DIGITALOCEAN_ACCESS_TOKEN", "")
	t.Setenv("DIGITALOCEAN_TOKEN", "")
	ref := observe(t, st, "digitalocean", accounts.Account{
		Category: accounts.DigitalOcean, Provider: "digitalocean", ProviderAccountID: "team-owl",
		Connections: []accounts.Connection{{ID: "owl", CredentialRef: "env:DIGITALOCEAN_ACCESS_TOKEN"}},
	})
	_, err := fleet.AccountResolver(context.Background(), ref, "infra")
	if err == nil {
		t.Fatal("an unset credential variable must refuse")
	}
	if !strings.Contains(err.Error(), "credential_unavailable") {
		t.Fatalf("error = %v, want credential_unavailable", err)
	}

	t.Setenv("DIGITALOCEAN_ACCESS_TOKEN", accountstest.CanarySecret)
	sel, err := fleet.AccountResolver(context.Background(), ref, "infra")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if sel.Token != accountstest.CanarySecret {
		t.Fatal("the resolver did not return the account's credential")
	}
	// Everything EXCEPT the credential field itself must be clean: that is
	// what makes it safe to print a selection's metadata.
	sel.Token = ""
	accountstest.AssertNoSecret(t, sel)
	list, err := st.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	accountstest.AssertNoSecret(t, list)
}

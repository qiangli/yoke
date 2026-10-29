package secrets

// `bashy secret --account` at the seam (S524.7, slice D). A fake resolver
// stands in for the account service; the store-to-credential half lives in
// pkg/accounts and is joined to this in pkg/accounts/builtin.

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/qiangli/yoke/pkg/accounts/accountstest"
)

func withSecretsResolver(t *testing.T, fn func(context.Context, string, string) (AccountSelection, error)) {
	t.Helper()
	prev := AccountResolver
	AccountResolver = fn
	t.Cleanup(func() { AccountResolver = prev })
}

// twoVaults is the acceptance fixture: two cloudbox endpoints on one host.
func twoVaults(t *testing.T) {
	t.Helper()
	withSecretsResolver(t, func(_ context.Context, ref, operation string) (AccountSelection, error) {
		if operation != "secret" {
			t.Fatalf("unexpected operation %q", operation)
		}
		switch ref {
		case "owl":
			return AccountSelection{Ref: ref, AccountID: "acc_owl", Category: "cloudbox",
				Endpoint: "https://owl.example.com/", Token: "bearer-owl"}, nil
		case "otter":
			return AccountSelection{Ref: ref, AccountID: "acc_otter", Category: "satellite",
				Endpoint: "https://otter.example.com", Token: "bearer-otter"}, nil
		}
		return AccountSelection{}, accountFail(accountCodeUnresolved, "account not found")
	})
}

// Each account reaches ITS OWN vault with its own bearer, and the ambient
// ladder answers for neither.
func TestSecretAccountSelectsItsOwnVault(t *testing.T) {
	twoVaults(t)
	t.Setenv("BASHY_CLOUDBOX_URL", "https://ambient.example.com")
	t.Setenv("BASHY_SECRETS_TOKEN", "ambient-token")

	for ref, want := range map[string][2]string{
		"owl":   {"https://owl.example.com", "bearer-owl"},
		"otter": {"https://otter.example.com", "bearer-otter"},
	} {
		c, err := (Config{Account: ref}).Resolve()
		if err != nil {
			t.Fatalf("--account %s: %v", ref, err)
		}
		if c.BaseURL != want[0] || c.Token != want[1] {
			t.Fatalf("--account %s resolved (%q, %q), want (%q, %q)", ref, c.BaseURL, c.Token, want[0], want[1])
		}
	}
}

// Two selections in flight at once cannot cross: the credential lives on the
// stack of one Resolve, not in any shared place.
func TestSecretAccountConcurrentSelectionsNeverCross(t *testing.T) {
	twoVaults(t)
	want := map[string]string{"owl": "bearer-owl", "otter": "bearer-otter"}
	var wg sync.WaitGroup
	bad := make(chan string, 64)
	for i := 0; i < 32; i++ {
		ref := "owl"
		if i%2 == 1 {
			ref = "otter"
		}
		wg.Add(1)
		go func(ref string) {
			defer wg.Done()
			c, err := (Config{Account: ref}).Resolve()
			if err != nil || c.Token != want[ref] {
				bad <- ref + " got " + c.Token
			}
		}(ref)
	}
	wg.Wait()
	close(bad)
	for msg := range bad {
		t.Fatal("tokens crossed between accounts: " + msg)
	}
}

// Every refusal lands before a request could be made, and none of them carries
// credential material.
func TestSecretAccountRefusalsPrecedeAnyRequest(t *testing.T) {
	var requests int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		requests++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	t.Setenv("BASHY_CLOUDBOX_URL", srv.URL)
	t.Setenv("BASHY_SECRETS_TOKEN", "ambient-token")

	withSecretsResolver(t, func(_ context.Context, ref, _ string) (AccountSelection, error) {
		switch ref {
		case "owl":
			return AccountSelection{Ref: ref, Category: "cloudbox", Endpoint: "https://owl.example.com", Token: "bearer-owl"}, nil
		case "do-team":
			return AccountSelection{Ref: ref, Category: "digitalocean", Token: accountstest.CanarySecret}, nil
		case "no-endpoint":
			return AccountSelection{Ref: ref, Category: "cloudbox", Token: "bearer"}, nil
		case "no-token":
			return AccountSelection{Ref: ref, Category: "cloudbox", Endpoint: "https://owl.example.com"}, nil
		}
		return AccountSelection{}, accountFail(accountCodeUnresolved, "account not found")
	})

	for _, tc := range []struct {
		name string
		cfg  Config
		want string
	}{
		{"url conflict", Config{Account: "owl", URL: "https://other.example.com"}, accountCodeConflict},
		{"token conflict", Config{Account: "owl", Token: "other"}, accountCodeConflict},
		{"wrong category", Config{Account: "do-team"}, accountCodeUnsupported},
		{"no endpoint", Config{Account: "no-endpoint"}, accountCodeUnresolved},
		{"no token", Config{Account: "no-token"}, accountCodeCredential},
		{"missing account", Config{Account: "nobody"}, accountCodeUnresolved},
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

	mu.Lock()
	defer mu.Unlock()
	if requests != 0 {
		t.Fatalf("%d request(s) were made before a refusal", requests)
	}
}

// A binary with no account service refuses rather than using the ambient vault.
func TestSecretAccountWithoutWiredService(t *testing.T) {
	withSecretsResolver(t, nil)
	t.Setenv("BASHY_SECRETS_TOKEN", "ambient-token")
	_, err := (Config{Account: "owl"}).Resolve()
	if err == nil {
		t.Fatal("--account without a wired account service must fail")
	}
	if !strings.Contains(err.Error(), accountCodeUnresolved) {
		t.Fatalf("error = %v, want %s", err, accountCodeUnresolved)
	}
}

// Without --account the ladder is byte-identical, and the account service is
// never consulted.
func TestSecretWithoutAccountIsUnchanged(t *testing.T) {
	consulted := 0
	withSecretsResolver(t, func(context.Context, string, string) (AccountSelection, error) {
		consulted++
		return AccountSelection{}, nil
	})
	t.Setenv("BASHY_CLOUDBOX_URL", "https://ambient.example.com")
	t.Setenv("BASHY_SECRETS_TOKEN", "ambient-token")
	c, err := (Config{}).Resolve()
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if c.BaseURL != "https://ambient.example.com" || c.Token != "ambient-token" {
		t.Fatalf("got (%q, %q), want the ambient ladder", c.BaseURL, c.Token)
	}
	if consulted != 0 {
		t.Fatal("a run without --account consulted the account service")
	}
}

// The flag is on the verb, which is what puts it in bashy's help and MCP tool
// schema (both reflected off the cobra tree).
func TestSecretAccountFlagIsRegistered(t *testing.T) {
	cmd := NewSecretsCmd()
	if cmd.PersistentFlags().Lookup("account") == nil {
		t.Fatal("secret has no --account flag")
	}
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("--help: %v", err)
	}
	if !strings.Contains(out.String(), "--account") {
		t.Fatal("--account is missing from the help output")
	}
}

// `secret env --account` must neither read nor write the shared cache file:
// it is not keyed by account, so one account's rendered values sitting there
// would be served to the next invocation — a different identity, or none.
func TestSecretEnvAccountNeitherReadsNorWritesTheSharedCache(t *testing.T) {
	cacheHome := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cacheHome)
	cache := filepath.Join(cacheHome, "bashy", "secrets-env.sh")
	if err := os.MkdirAll(filepath.Dir(cache), 0o700); err != nil {
		t.Fatal(err)
	}
	// A cache left behind by some other identity.
	if err := os.WriteFile(cache, []byte("export OWL_TOKEN='"+accountstest.CanarySecret+"'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stale, err := os.ReadFile(cache)
	if err != nil {
		t.Fatal(err)
	}

	cfgHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfgHome)
	if err := os.MkdirAll(filepath.Join(cfgHome, "bashy"), 0o700); err != nil {
		t.Fatal(err)
	}
	tmpl := filepath.Join(cfgHome, "bashy", "secrets.map")
	if err := os.WriteFile(tmpl, []byte("OWL_TOKEN=@owl-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// The account's vault is unreachable, which is precisely when the cache
	// would otherwise be consulted.
	withSecretsResolver(t, func(_ context.Context, ref, _ string) (AccountSelection, error) {
		return AccountSelection{Ref: ref, Category: "cloudbox",
			Endpoint: "http://127.0.0.1:1", Token: "bearer-otter"}, nil
	})

	var out, errb bytes.Buffer
	if err := runEnv(&out, &errb, Config{Account: "otter"}, "", false); err != nil {
		t.Fatalf("secret env --account otter: %v", err)
	}
	accountstest.AssertNoSecret(t, out.String(), errb.String())
	after, err := os.ReadFile(cache)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(stale) {
		t.Fatal("secret env --account wrote another identity's values into the shared cache")
	}
}

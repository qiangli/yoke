package accounts

// The shared `--account` half: selecting one connection and materializing its
// credential for exactly one call (S524.7, slice D).
//
// Every reach outside the process here is driven through the real resolvers
// wherever an environment variable or a temporary directory can do it, so
// these tests exercise the code a live invocation runs rather than a stub.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/qiangli/yoke/pkg/accounts/accountstest"
)

func isolatedAccountsHome(t *testing.T) *Store {
	t.Helper()
	t.Setenv("BASHY_HOME", t.TempDir())
	return Open(DefaultPath())
}

// clearAWSEnv keeps external/aws' explicit-environment branch out of the way,
// so a profile: reference reads the shared credentials file.
func clearAWSEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AWS_PROFILE"} {
		t.Setenv(k, "")
	}
}

func TestMaterializeSecretReference(t *testing.T) {
	// ValidCredentialRef (S524.1) does not admit "/" in a secret name, so a
	// vault reference is spelled flat.
	m := Materializer{SecretGet: func(name string) (string, error) {
		if name != "do-owl-agent" {
			t.Fatalf("fetched %q", name)
		}
		return "token-owl", nil
	}}
	got, err := m.Materialize(context.Background(), Selection{CredentialRef: "secret:do-owl-agent"}, DigitalOcean)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	if got.Token != "token-owl" || got.Kind != "secret" || got.Ref != "secret:do-owl-agent" {
		t.Fatalf("got %+v", got)
	}
}

// The two DigitalOcean spellings are interchangeable in BOTH directions: the
// account adapter recorded one name and pkg/fleet's provider path reads the
// other, and a host that set either must work.
func TestMaterializeAcceptsEitherDigitalOceanSpelling(t *testing.T) {
	for _, tc := range []struct{ set, ref string }{
		{"DIGITALOCEAN_ACCESS_TOKEN", "env:DIGITALOCEAN_TOKEN"},
		{"DIGITALOCEAN_TOKEN", "env:DIGITALOCEAN_ACCESS_TOKEN"},
		{"DIGITALOCEAN_ACCESS_TOKEN", "env:DIGITALOCEAN_ACCESS_TOKEN"},
		{"DIGITALOCEAN_TOKEN", "env:DIGITALOCEAN_TOKEN"},
	} {
		t.Run(tc.set+"/"+tc.ref, func(t *testing.T) {
			t.Setenv("DIGITALOCEAN_ACCESS_TOKEN", "")
			t.Setenv("DIGITALOCEAN_TOKEN", "")
			t.Setenv(tc.set, "token-owl")
			got, err := Materializer{}.Materialize(context.Background(),
				Selection{CredentialRef: tc.ref}, DigitalOcean)
			if err != nil {
				t.Fatalf("Materialize: %v", err)
			}
			if got.Token != "token-owl" {
				t.Fatalf("token = %q", got.Token)
			}
		})
	}
}

// Discovery must see the spelling pkg/fleet actually uses; before this story a
// host that set only $DIGITALOCEAN_ACCESS_TOKEN had no DigitalOcean account at
// all.
func TestDigitalOceanDiscoveryAcceptsEitherSpelling(t *testing.T) {
	for _, name := range DigitalOceanEnvNames() {
		t.Run(name, func(t *testing.T) {
			t.Setenv("DIGITALOCEAN_ACCESS_TOKEN", "")
			t.Setenv("DIGITALOCEAN_TOKEN", "")
			t.Setenv(name, "token-owl")
			obs, err := NewDigitalOceanAdapter().Discover(context.Background())
			if err != nil {
				t.Fatalf("Discover: %v", err)
			}
			if len(obs) != 1 {
				t.Fatalf("got %d observations, want 1", len(obs))
			}
			if ref := obs[0].Account.Connections[0].CredentialRef; ref != "env:"+name {
				t.Fatalf("credential ref = %q, want env:%s", ref, name)
			}
			accountstest.AssertNoSecret(t, obs)
			if strings.Contains(obs[0].Account.Connections[0].CredentialRef, "token-owl") {
				t.Fatal("a token value reached an inventory record")
			}
		})
	}
}

// Two AWS profiles stay separate: each reference reads only its own section,
// and neither falls back to [default].
func TestMaterializeAWSProfileKeysStayPerProfile(t *testing.T) {
	clearAWSEnv(t)
	path := filepath.Join(t.TempDir(), "credentials")
	body := strings.Join([]string{
		"[default]",
		"aws_access_key_id = AKIDDEFAULT",
		"aws_secret_access_key = default-secret",
		"[alice]",
		"aws_access_key_id = AKIDALICE",
		"aws_secret_access_key = alice-secret",
		"[bob]",
		"aws_access_key_id = AKIDBOB",
		"aws_secret_access_key = bob-secret",
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", path)

	for profile, wantID := range map[string]string{"alice": "AKIDALICE", "bob": "AKIDBOB"} {
		got, err := Materializer{}.Materialize(context.Background(),
			Selection{Profile: profile, CredentialRef: "profile:" + profile}, AWS)
		if err != nil {
			t.Fatalf("profile %s: %v", profile, err)
		}
		if got.AWS.AccessKeyID != wantID {
			t.Fatalf("profile %s resolved key id %q, want %q", profile, got.AWS.AccessKeyID, wantID)
		}
		if got.Token != "" {
			t.Fatal("an AWS key pair must not also appear as a single token")
		}
	}

	// A profile that is not in the file refuses; it does NOT become [default].
	_, err := Materializer{}.Materialize(context.Background(),
		Selection{Profile: "carol", CredentialRef: "profile:carol"}, AWS)
	if ErrorCode(err) != CodeCredentialUnavailable {
		t.Fatalf("unknown profile: code = %q (%v)", ErrorCode(err), err)
	}
}

// writeCloudboxProfile lays out an outpost profile directory the way the
// discovery adapter reads it.
func writeCloudboxProfile(t *testing.T, root, name, server, token string) {
	t.Helper()
	dir := filepath.Join(root, "profiles", name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"server_addr":"` + server + `","access_token":"` + token + `","user":"` + name + `"}`
	if err := os.WriteFile(filepath.Join(dir, "agent.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Two cloudbox endpoints on one host keep separate tokens, and reading one
// must not touch the outpost profile directory in any way.
func TestMaterializeCloudboxProfileTokenIsPerProfileAndReadOnly(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	root := filepath.Join(cfg, "matrix")
	writeCloudboxProfile(t, root, "owl", "https://owl.example.com", "bearer-owl")
	writeCloudboxProfile(t, root, "otter", "https://otter.example.com", "bearer-otter")

	before := snapshotTree(t, root)
	for profile, want := range map[string]string{"owl": "bearer-owl", "otter": "bearer-otter"} {
		got, err := Materializer{}.Materialize(context.Background(),
			Selection{Profile: profile, CredentialRef: "profile:" + profile}, Cloudbox)
		if err != nil {
			t.Fatalf("profile %s: %v", profile, err)
		}
		if got.Token != want {
			t.Fatalf("profile %s resolved token %q, want %q", profile, got.Token, want)
		}
	}
	if after := snapshotTree(t, root); after != before {
		t.Fatal("materializing a cloudbox token wrote to the outpost profile directory")
	}
	if _, err := os.Stat(filepath.Join(root, ".active")); !os.IsNotExist(err) {
		t.Fatal("materializing a cloudbox token activated a profile")
	}
}

// snapshotTree is every path and byte under root, so a read-only claim is
// checked rather than asserted.
func snapshotTree(t *testing.T, root string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		b.WriteString(path)
		if info.IsDir() {
			b.WriteString("/\n")
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		b.WriteString("=" + string(data) + "\n")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// The active profile lives at the root of the profile directory, named by
// .active — a selection for it must find it there.
func TestMaterializeCloudboxActiveProfile(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	root := filepath.Join(cfg, "matrix")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "agent.json"),
		[]byte(`{"server_addr":"https://owl.example.com","access_token":"bearer-active"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".active"), []byte("owl\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Materializer{}.Materialize(context.Background(),
		Selection{Profile: "owl", CredentialRef: "profile:owl"}, Cloudbox)
	if err != nil || got.Token != "bearer-active" {
		t.Fatalf("got (%q, %v), want the active profile's token", got.Token, err)
	}
}

// Every refusal carries a stable code and no credential text.
func TestMaterializeRefusals(t *testing.T) {
	t.Setenv("DIGITALOCEAN_ACCESS_TOKEN", "")
	t.Setenv("DIGITALOCEAN_TOKEN", "")
	for _, tc := range []struct {
		name     string
		sel      Selection
		category Category
		want     string
	}{
		{"no reference", Selection{}, DigitalOcean, CodeCredentialMissing},
		{"ssh key", Selection{CredentialRef: "ssh-key:SHA256:abc"}, DigitalOcean, CodeCredentialUnsupported},
		{"profile for an LLM seat", Selection{CredentialRef: "profile:owl"}, LLM, CodeCredentialUnsupported},
		{"unset variable", Selection{CredentialRef: "env:OWL_TOKEN"}, DigitalOcean, CodeCredentialUnavailable},
		{"malformed reference", Selection{CredentialRef: "not-a-reference"}, DigitalOcean, CodeCredentialUnsupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Materializer{}.Materialize(context.Background(), tc.sel, tc.category)
			if ErrorCode(err) != tc.want {
				t.Fatalf("code = %q (%v), want %q", ErrorCode(err), err, tc.want)
			}
			accountstest.AssertNoSecret(t, err.Error())
		})
	}

	// A vault that cannot answer is unavailable, and its error text never
	// reaches the caller.
	m := Materializer{SecretGet: func(string) (string, error) {
		return "", errCloudRead
	}}
	_, err := m.Materialize(context.Background(), Selection{CredentialRef: "secret:owl"}, Cloudbox)
	if ErrorCode(err) != CodeCredentialUnavailable {
		t.Fatalf("code = %q (%v)", ErrorCode(err), err)
	}
}

// Two accounts materialized in parallel must never see each other's value.
func TestMaterializeConcurrentAccountsNeverCross(t *testing.T) {
	m := Materializer{SecretGet: func(name string) (string, error) { return "token-" + name, nil }}
	var wg sync.WaitGroup
	bad := make(chan string, 64)
	for i := 0; i < 32; i++ {
		name := "owl"
		if i%2 == 1 {
			name = "otter"
		}
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			got, err := m.Materialize(context.Background(), Selection{CredentialRef: "secret:" + name}, DigitalOcean)
			if err != nil || got.Token != "token-"+name {
				bad <- name + " got " + got.Token
			}
		}(name)
	}
	wg.Wait()
	close(bad)
	for msg := range bad {
		t.Fatal("credentials crossed between accounts: " + msg)
	}
}

// SelectFor maps the store's own sentinels onto the stable codes, and resolves
// through the owning adapter when that adapter declares Select.
func TestSelectForCodes(t *testing.T) {
	st := isolatedAccountsHome(t)
	if _, _, err := SelectFor(context.Background(), st, DefaultRegistry(), "nobody", OpInfra); ErrorCode(err) != CodeUnresolved {
		t.Fatalf("missing account: code = %q (%v)", ErrorCode(err), err)
	}

	// An identity-bearing observation is resolvable; two connections are not.
	err := st.Observe("digitalocean", []Observation{{Account: Account{
		Category: DigitalOcean, Provider: "digitalocean", ProviderAccountID: "team-owl",
		Connections: []Connection{
			{ID: "a", CredentialRef: "env:DIGITALOCEAN_TOKEN"},
			{ID: "b", CredentialRef: "env:DIGITALOCEAN_ACCESS_TOKEN"},
		},
	}}})
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	accts, err := st.List()
	if err != nil || len(accts) != 1 {
		t.Fatalf("List: %v %d", err, len(accts))
	}
	if _, _, err := SelectFor(context.Background(), st, DefaultRegistry(), accts[0].ID, OpInfra); ErrorCode(err) != CodeAmbiguous {
		t.Fatalf("two connections: code = %q (%v)", ErrorCode(err), err)
	}
}

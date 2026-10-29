package accounts

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/accounts/accountstest"
	"github.com/qiangli/yoke/pkg/llmbudget"
	awsprovider "github.com/qiangli/yoke/pkg/provider/aws"
	do "github.com/qiangli/yoke/pkg/provider/digitalocean"
	"github.com/qiangli/yoke/pkg/provider/digitalocean/fakedo"
)

// fence points the package-level hook at a test inventory, so a wiring test
// can see what an operation recorded without any test ever reaching the
// operator's real store.
func fence(t *testing.T, s *Store) {
	t.Helper()
	SetHookStore(s)
	t.Cleanup(func() { SetHookStore(nil) })
}

func hostFacts() Facts {
	return Facts{Adapter: "ssh", Provider: "ssh", Username: "alice", Endpoint: "example.com:22", AuthMethod: "publickey", CredentialRef: "ssh-key:aa:bb", Source: "ssh/connect"}
}

// A host login added by hand and the same login proven by a later connect are
// ONE account: the manual alias is what the operator types, so it survives as
// an override while the adapter owns the observation.
func TestManualAddAndObservedConnectConverge(t *testing.T) {
	s := Open(filepath.Join(t.TempDir(), "accounts.json"))
	added, err := s.Add(Account{Alias: "owl", Name: "my box", Category: HostLogin, Provider: "ssh", Username: "alice", Connections: []Connection{{ID: "main", Endpoint: "example.com:22", AuthMethod: "password", CredentialRef: "secret:owl"}}})
	if err != nil {
		t.Fatal(err)
	}
	if added.Alias != "owl" || added.Name != "my box" || added.Status != Resolved {
		t.Fatalf("added: %+v", added)
	}
	if err = s.record(context.Background(), HostLogin, hostFacts(), Resolved, ""); err != nil {
		t.Fatal(err)
	}
	list, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("manual add and observation split into %d records: %+v", len(list), list)
	}
	got := list[0]
	if got.Alias != "owl" {
		t.Fatalf("alias lost: %+v", got)
	}
	if got.Name != "my box" {
		t.Fatalf("manual label lost: %+v", got)
	}
	if got.ID != added.ID {
		t.Fatal("observation changed the account ID")
	}
	if got.Provenance != "ssh" || got.Status != Resolved {
		t.Fatalf("observation not applied: %+v", got)
	}
	// The connect proved an auth method; the manually entered connection is
	// updated in place rather than duplicated.
	if len(got.Connections) != 1 || got.Connections[0].ID != "main" {
		t.Fatalf("connections: %+v", got.Connections)
	}
	if got.Connections[0].AuthMethod != "publickey" || got.Connections[0].CredentialRef != "ssh-key:aa:bb" {
		t.Fatalf("proven connection detail not applied: %+v", got.Connections[0])
	}
	if _, err = s.Get("owl"); err != nil {
		t.Fatal(err)
	}
}

// A manual record that names no reachable identity keeps the alias key: there
// is nothing for an adapter to converge on.
func TestManualAddWithoutIdentityKeepsAliasKey(t *testing.T) {
	s := Open(filepath.Join(t.TempDir(), "accounts.json"))
	for _, alias := range []string{"owl", "otter"} {
		if _, err := s.Add(Account{Alias: alias, Category: Other, Provider: "example"}); err != nil {
			t.Fatal(err)
		}
	}
	list, _ := s.List()
	if len(list) != 2 {
		t.Fatalf("unidentifiable manual records merged: %+v", list)
	}
	if _, err := s.Add(Account{Alias: "owl", Category: Other, Provider: "example"}); err == nil {
		t.Fatal("duplicate alias accepted")
	}
	// Adding the same proven identity twice is a duplicate, not a second row.
	host := Account{Alias: "first", Category: HostLogin, Provider: "ssh", Username: "alice", Connections: []Connection{{Endpoint: "example.com:22"}}}
	if _, err := s.Add(host); err != nil {
		t.Fatal(err)
	}
	host.Alias = "second"
	if _, err := s.Add(host); err == nil {
		t.Fatal("same identity added twice")
	}
}

// Retried and repeated operations are idempotent down to the bytes: the
// timestamp moves only when something else did, so a hook on a hot path does
// not rewrite the inventory on every call.
func TestHookIdempotentAndRespectsSuppression(t *testing.T) {
	s := Open(filepath.Join(t.TempDir(), "accounts.json"))
	f := hostFacts()
	f.At = time.Now()
	if err := s.record(context.Background(), HostLogin, f, Resolved, ""); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(s.Path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		f.At = time.Now().Add(time.Duration(i+1) * time.Hour)
		if err = s.record(context.Background(), HostLogin, f, Resolved, ""); err != nil {
			t.Fatal(err)
		}
	}
	again, _ := os.ReadFile(s.Path)
	if string(first) != string(again) {
		t.Fatalf("repeat observation rewrote the store:\n%s\n%s", first, again)
	}
	list, _ := s.List()
	if len(list) != 1 {
		t.Fatalf("duplicates: %+v", list)
	}
	// A removed account stays removed: collection never undoes an operator.
	if err = s.Remove(list[0].ID); err != nil {
		t.Fatal(err)
	}
	if err = s.record(context.Background(), HostLogin, f, Resolved, ""); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(list[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != Suppressed {
		t.Fatalf("suppressed account was re-added: %+v", got)
	}
	if list, _ = s.List(); len(list) != 1 {
		t.Fatalf("suppressed account duplicated: %+v", list)
	}
}

// A logout or an expired credential changes the STATUS of an account; it does
// not erase what the account is. The next success clears the error code.
func TestHookTransitionKeepsLastGood(t *testing.T) {
	s := Open(filepath.Join(t.TempDir(), "accounts.json"))
	ctx := context.Background()
	rich := Facts{
		Adapter: "digitalocean", Provider: "digitalocean", ProviderAccountID: "team-1",
		Team: "owls", Name: "owls", Region: "nyc1", ConnectionID: "env",
		CredentialRef: "env:DIGITALOCEAN_TOKEN", Source: "digitalocean/account",
		Resources: []ResourceRef{{ID: "1", Kind: "droplet", Name: "web"}},
		Links:     Links{Hosts: []string{"web"}},
	}
	if err := s.record(ctx, DigitalOcean, rich, Resolved, ""); err != nil {
		t.Fatal(err)
	}
	// The expiry path knows the identity and nothing else.
	sparse := Facts{Adapter: "digitalocean", Provider: "digitalocean", ProviderAccountID: "team-1"}
	if err := s.record(ctx, DigitalOcean, sparse, Error, "credential_expired"); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get("acc_" + strings.TrimPrefix(localID(mustKey(t, rich, DigitalOcean)), "acc_"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != Error || got.LastError != "credential_expired" {
		t.Fatalf("transition not recorded: %+v", got)
	}
	if got.Team != "owls" || got.Region != "nyc1" || len(got.Resources) != 1 || len(got.Links.Hosts) != 1 {
		t.Fatalf("last good erased by a lost credential: %+v", got)
	}
	if len(got.Connections) != 1 || got.Connections[0].CredentialRef != "env:DIGITALOCEAN_TOKEN" {
		t.Fatalf("connections: %+v", got.Connections)
	}
	// Logging back in resolves the same record again.
	if err = s.record(ctx, DigitalOcean, rich, Resolved, ""); err != nil {
		t.Fatal(err)
	}
	if got, err = s.Get(got.ID); err != nil {
		t.Fatal(err)
	}
	if got.Status != Resolved || got.LastError != "" {
		t.Fatalf("re-login did not clear the error: %+v", got)
	}
	// Suppression is an operator act, never an observed status.
	if err = s.record(ctx, DigitalOcean, rich, Suppressed, ""); err == nil {
		t.Fatal("observation claimed suppression")
	}
	if err = s.record(ctx, DigitalOcean, rich, Error, "Token Expired: abc"); err == nil {
		t.Fatal("unsafe error text accepted")
	}
}

func mustKey(t *testing.T, f Facts, category Category) string {
	t.Helper()
	key, ok := identityKey(f.account(category))
	if !ok {
		t.Fatal("facts name no identity")
	}
	return key
}

// A store the hook cannot write is not the caller's problem: the operation
// that produced the fact still succeeds, and nothing panics.
func TestHookFailureLeavesCallerResultIntact(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := filepath.Join(t.TempDir(), "ro")
	if err := os.MkdirAll(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	broken := Open(filepath.Join(dir, "inventory", "accounts.json"))
	if err := broken.record(context.Background(), HostLogin, hostFacts(), Resolved, ""); err == nil {
		t.Fatal("unwritable store reported success")
	}
	// The public entry points swallow it: no error, no panic, no output.
	broken.ObserveSuccess(context.Background(), HostLogin, hostFacts())
	broken.ObserveTransition(context.Background(), HostLogin, hostFacts(), Error, "credential_expired")
	fence(t, broken)
	ObserveSuccess(context.Background(), HostLogin, hostFacts())

	// The same, through a real caller: a provider call keeps working.
	srv := fakedo.New(t)
	srv.SetAccount(map[string]any{"email": "alice@example.com", "uuid": "user-1", "team": map[string]string{"uuid": "team-1", "name": "owls"}, "droplet_limit": 5, "status": "active"})
	client, err := do.NewClient(do.ClientConfig{BaseURL: srv.URL, Token: fakedo.TestToken, HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	a := NewDigitalOceanAdapter()
	a.Client = func(context.Context, Connection) (*do.Client, error) { return client, nil }
	acct := Account{Category: DigitalOcean, Provider: "digitalocean", Connections: []Connection{{ID: "env", CredentialRef: "env:DIGITALOCEAN_TOKEN"}}}
	metrics, err := a.Usage(context.Background(), acct, "day")
	if err != nil || len(metrics) == 0 {
		t.Fatalf("provider call broken by the hook: %v", err)
	}
	if _, err = broken.List(); err != nil {
		t.Fatal(err)
	}
}

// A successful DigitalOcean call records the team it proved, without an
// explicit refresh.
func TestHookWiringDigitalOcean(t *testing.T) {
	srv := fakedo.New(t)
	srv.SetAccount(map[string]any{"email": "alice@example.com", "uuid": "user-1", "team": map[string]string{"uuid": "team-1", "name": "owls"}, "droplet_limit": 5, "status": "active"})
	client, err := do.NewClient(do.ClientConfig{BaseURL: srv.URL, Token: accountstest.CanarySecret, HTTPClient: &http.Client{Transport: cloudTransport(func(r *http.Request) (*http.Response, error) {
		clone := r.Clone(r.Context())
		clone.Header.Set("Authorization", "Bearer "+fakedo.TestToken)
		return srv.Client().Transport.RoundTrip(clone)
	})}})
	if err != nil {
		t.Fatal(err)
	}
	s := Open(filepath.Join(t.TempDir(), "accounts.json"))
	fence(t, s)
	a := NewDigitalOceanAdapter()
	a.Client = func(context.Context, Connection) (*do.Client, error) { return client, nil }
	acct := Account{Category: DigitalOcean, Provider: "digitalocean", Connections: []Connection{{ID: "env", CredentialRef: "env:DIGITALOCEAN_TOKEN"}}}
	if _, err = a.Usage(context.Background(), acct, "day"); err != nil {
		t.Fatal(err)
	}
	list, _ := s.List()
	if len(list) != 1 {
		t.Fatalf("records: %+v", list)
	}
	got := list[0]
	if got.ProviderAccountID != "team-1" || got.Team != "owls" || got.Status != Resolved || got.Provenance != "digitalocean" {
		t.Fatalf("identity not recorded: %+v", got)
	}
	if got.Source != "digitalocean/account" || got.ObservedAt == nil {
		t.Fatalf("provenance: %+v", got)
	}
	b, _ := os.ReadFile(s.Path)
	accountstest.AssertNoSecret(t, string(b))
}

// A successful STS lookup records the account number the profile reached.
func TestHookWiringAWS(t *testing.T) {
	s := Open(filepath.Join(t.TempDir(), "accounts.json"))
	fence(t, s)
	a := NewAWSAdapter()
	a.Client = func(context.Context, Connection, string) (*awsprovider.InventoryClient, error) {
		return awsprovider.NewInventoryClient(awsprovider.Config{Credentials: awsprovider.Credentials{AccessKeyID: "owl", SecretAccessKey: accountstest.CanarySecret}, Region: "us-east-1", HTTPClient: &http.Client{Transport: cloudTransport(func(r *http.Request) (*http.Response, error) {
			if r.Header.Get("X-Amz-Target") == "" {
				return cloudReply(200, `<GetCallerIdentityResponse><GetCallerIdentityResult><Account>123456789012</Account><Arn>arn:aws:iam::123456789012:role/owl</Arn></GetCallerIdentityResult></GetCallerIdentityResponse>`), nil
			}
			return cloudReply(200, `{"instances":[{"name":"owl","arn":"arn:aws:lightsail:us-east-1:123456789012:Instance/owl","location":{"regionName":"us-east-1"}}]}`), nil
		})}})
	}
	acct := Account{Category: AWS, Provider: "aws", Region: "us-east-1", Connections: []Connection{{ID: "owl", Profile: "owl", CredentialRef: "profile:owl"}}}
	if _, err := a.Resources(context.Background(), acct); err != nil {
		t.Fatal(err)
	}
	list, _ := s.List()
	if len(list) != 1 {
		t.Fatalf("records: %+v", list)
	}
	got := list[0]
	if got.ProviderAccountID != "123456789012" || got.Region != "us-east-1" || got.Status != Resolved {
		t.Fatalf("identity not recorded: %+v", got)
	}
	if len(got.Connections) != 1 || got.Connections[0].Profile != "owl" || got.Connections[0].CredentialRef != "profile:owl" {
		t.Fatalf("connection: %+v", got.Connections)
	}
	b, _ := os.ReadFile(s.Path)
	accountstest.AssertNoSecret(t, string(b))
}

// The llmbudget seam is installed by this package and records the seat a run
// proved, with the model and agent as links.
func TestHookWiringLLMRun(t *testing.T) {
	if llmbudget.AccountObserver == nil {
		t.Fatal("llmbudget observation seam not installed")
	}
	s := Open(filepath.Join(t.TempDir(), "accounts.json"))
	fence(t, s)
	llmbudget.AccountObserver(context.Background(), "anthropic", "max-seat", "claude-opus-5", "claude")
	llmbudget.AccountObserver(context.Background(), "anthropic", "max-seat", "claude-sonnet-5", "codex")
	list, _ := s.List()
	if len(list) != 1 {
		t.Fatalf("one seat expected: %+v", list)
	}
	got := list[0]
	if got.Category != LLM || got.Provider != "anthropic" || got.ProviderAccountID != "max-seat" {
		t.Fatalf("seat: %+v", got)
	}
	// Each run proves one relationship; the set accumulates.
	if strings.Join(got.Links.Models, ",") != "claude-opus-5,claude-sonnet-5" {
		t.Fatalf("models: %+v", got.Links.Models)
	}
	if strings.Join(got.Links.Agents, ",") != "claude,codex" {
		t.Fatalf("agents: %+v", got.Links.Agents)
	}
}

// Nothing a caller passes can put a credential VALUE in the inventory, and an
// unidentifiable observation is dropped rather than keyed on a guess.
func TestHookRefusesSecretsAndGuesses(t *testing.T) {
	s := Open(filepath.Join(t.TempDir(), "accounts.json"))
	ctx := context.Background()
	f := hostFacts()
	f.CredentialRef = accountstest.CanarySecret
	f.SSHKeys = []SSHKeyRef{{Fingerprint: "aa:bb", CredentialRef: accountstest.CanarySecret}}
	if err := s.record(ctx, HostLogin, f, Resolved, ""); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(s.Path)
	if err != nil {
		t.Fatal(err)
	}
	accountstest.AssertNoSecret(t, string(b))
	list, _ := s.List()
	if len(list) != 1 || list[0].Username != "alice" {
		t.Fatalf("identity lost with the bad reference: %+v", list)
	}
	if list[0].Connections[0].CredentialRef != "" || list[0].SSHKeys[0].CredentialRef != "" {
		t.Fatalf("unvalidated reference stored: %+v", list[0])
	}
	// Control characters and oversized values are metadata, not facts.
	noisy := Facts{Adapter: "ssh", Provider: "ssh", Username: "bob", Endpoint: "other.example:22", Name: "line\nbreak", Team: strings.Repeat("x", maxFactLength+1)}
	if err = s.record(ctx, HostLogin, noisy, Resolved, ""); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(localID(mustKey(t, noisy, HostLogin)))
	if got.Name != "" || got.Team != "" {
		t.Fatalf("unsafe field stored: %+v", got)
	}
	// Neither an identity nor a source key: there is nothing to record.
	if err = s.record(ctx, LLM, Facts{Provider: "anthropic"}, Resolved, ""); !errors.Is(err, errHookUnidentified) {
		t.Fatal(err)
	}
	if err = s.record(ctx, LLM, Facts{Provider: "anthropic", SourceKey: "pool/standard"}, Resolved, ""); err != nil {
		t.Fatal(err)
	}
	if list, _ = s.List(); len(list) != 3 {
		t.Fatalf("records: %d", len(list))
	}
}

// Concurrent operations reporting their facts must not lose or duplicate a
// record, and must leave the inventory private.
func TestHookConcurrentObservations(t *testing.T) {
	s := Open(filepath.Join(t.TempDir(), "accounts", "accounts.json"))
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx := context.Background()
			for j := 0; j < 5; j++ {
				f := hostFacts()
				f.Username = "user" + strconv.Itoa(i)
				if err := s.record(ctx, HostLogin, f, Resolved, ""); err != nil {
					t.Error(err)
				}
				shared := Facts{Adapter: "llm", Provider: "anthropic", ProviderAccountID: "max-seat", Links: Links{Agents: []string{"agent" + strconv.Itoa(i)}}}
				if err := s.record(ctx, LLM, shared, Resolved, ""); err != nil {
					t.Error(err)
				}
			}
		}(i)
	}
	wg.Wait()
	list, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 9 {
		t.Fatalf("got %d records: %+v", len(list), list)
	}
	for _, a := range list {
		if a.Category == LLM && len(a.Links.Agents) != 8 {
			t.Fatalf("lost a concurrent relationship: %+v", a.Links.Agents)
		}
	}
	st, err := os.Stat(s.Path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatal(st.Mode())
	}
}

// The hook is bounded: a caller whose context is already done records nothing
// instead of waiting on the inventory lock.
func TestHookBoundedByContext(t *testing.T) {
	s := Open(filepath.Join(t.TempDir(), "accounts.json"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.record(ctx, HostLogin, hostFacts(), Resolved, ""); err == nil {
		t.Fatal("expired context recorded an observation")
	}
	if _, err := os.Stat(s.Path); !os.IsNotExist(err) {
		t.Fatal("store written past the deadline")
	}
}

// Under go test the default hook target refuses the operator's live inventory:
// an unfenced test records nothing rather than polluting it.
func TestHookNeverTouchesRealHome(t *testing.T) {
	if realHomeErr != nil || realHome == "" {
		t.Skip("no home directory to guard")
	}
	t.Setenv("BASHY_HOME", filepath.Join(realHome, ".bashy", "nested"))
	if got := hookTarget(); got.Path != "" {
		t.Fatalf("hook aimed at the real inventory: %q", got.Path)
	}
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	if got := hookTarget(); got.Path != filepath.Join(home, "accounts", "accounts.json") {
		t.Fatalf("fenced home not used: %q", got.Path)
	}
}

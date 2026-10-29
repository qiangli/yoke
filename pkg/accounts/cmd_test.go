package accounts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/accounts/accountstest"
)

func invokeAccount(t *testing.T, store *Store, registry *Registry, args ...string) (string, string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	cmd := NewAccountCmd(WithStore(store), WithRegistry(registry))
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs(args)
	err := cmd.Execute()
	accountstest.AssertNoSecret(t, stdout.String(), stderr.String(), err)
	return stdout.String(), stderr.String(), err
}

func decodeEnvelope(t *testing.T, raw string) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		t.Fatalf("decode envelope: %v\n%s", err, raw)
	}
	if value["schema_version"] != CommandSchemaVersion {
		t.Fatalf("schema_version = %v", value["schema_version"])
	}
	return value
}

func TestAccountCommandCRUDAndSchema(t *testing.T) {
	store := Open(t.TempDir() + "/accounts.json")
	registry := NewRegistry()

	out, _, err := invokeAccount(t, store, registry, "add", "owl", "--kind", "host-login", "--user", "alice", "--endpoint", "example.com:22", "--credential", "secret:owl", "--json")
	if err != nil {
		t.Fatal(err)
	}
	envelope := decodeEnvelope(t, out)
	account := envelope["account"].(map[string]any)
	id := account["id"].(string)
	if account["status"] != string(Resolved) {
		t.Fatalf("add status = %v", account["status"])
	}

	out, _, err = invokeAccount(t, store, registry, "list", "--category", "host-login", "--provider", "host-login", "--status", "resolved", "--fields", "id,alias,team", "--json")
	if err != nil {
		t.Fatal(err)
	}
	rows := decodeEnvelope(t, out)["accounts"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["alias"] != "owl" {
		t.Fatalf("filtered list = %#v", rows)
	}

	out, _, err = invokeAccount(t, store, registry, "show", id[:10], "--fields", "id,alias", "--json")
	if err != nil || decodeEnvelope(t, out)["account"].(map[string]any)["id"] != id {
		t.Fatalf("prefix show: out=%s err=%v", out, err)
	}

	out, _, err = invokeAccount(t, store, registry, "set", "owl", "--set", "alias=otter", "--set", "region=west", "--json")
	if err != nil || decodeEnvelope(t, out)["account"].(map[string]any)["alias"] != "otter" {
		t.Fatalf("set: out=%s err=%v", out, err)
	}
	if _, _, err = invokeAccount(t, store, registry, "set", "otter", "--unset", "region"); err != nil {
		t.Fatal(err)
	}
	_, stderr, err := invokeAccount(t, store, registry, "set", "otter", "--set", "provider=bad", "--json")
	if err == nil || decodeEnvelope(t, stderr)["error"].(map[string]any)["code"] != "invalid_field" {
		t.Fatalf("invalid set: stderr=%s err=%v", stderr, err)
	}

	if err := store.SetBindings("otter", []string{"connect"}); err != nil {
		t.Fatal(err)
	}
	_, stderr, err = invokeAccount(t, store, registry, "rm", "otter", "--json")
	if err == nil || decodeEnvelope(t, stderr)["error"].(map[string]any)["code"] != "referenced" {
		t.Fatalf("guarded rm: stderr=%s err=%v", stderr, err)
	}
	if err := store.SetBindings("otter", nil); err != nil {
		t.Fatal(err)
	}
	out, _, err = invokeAccount(t, store, registry, "rm", "otter", "--json")
	if err != nil || decodeEnvelope(t, out)["account"].(map[string]any)["status"] != string(Suppressed) {
		t.Fatalf("rm: out=%s err=%v", out, err)
	}
	out, _, err = invokeAccount(t, store, registry, "restore", "otter", "--json")
	if err != nil || decodeEnvelope(t, out)["account"].(map[string]any)["status"] != string(Resolved) {
		t.Fatalf("restore: out=%s err=%v", out, err)
	}

	out, _, err = invokeAccount(t, store, registry, "schema", "--json")
	if err != nil || decodeEnvelope(t, out)["schema"] == nil {
		t.Fatalf("schema: out=%s err=%v", out, err)
	}

	_, stderr, err = invokeAccount(t, store, registry, "add", "raw", "--kind", "host-login", "--user", "bob", "--endpoint", "example.com:22", "--credential", accountstest.CanarySecret, "--json")
	if err == nil || !strings.Contains(stderr, "invalid_credential_ref") {
		t.Fatalf("raw credential accepted: stderr=%s err=%v", stderr, err)
	}
}

type fixtureAdapter struct {
	fail bool
	now  time.Time
}

func (a *fixtureAdapter) Name() string { return "fixture" }
func (a *fixtureAdapter) Capabilities() Capability {
	return Discover | Verify | Resources | Usage | Billing | Select
}
func (a *fixtureAdapter) observation() Observation {
	return Observation{Account: Account{
		Alias: "bob", Category: DigitalOcean, Provider: "fixture", ProviderAccountID: "acct-bob",
		Team: "team-b", Status: Resolved, Connections: []Connection{{ID: "primary", Profile: "bob", CredentialRef: "profile:bob"}},
	}}
}
func (a *fixtureAdapter) Discover(context.Context) ([]Observation, error) {
	if a.fail {
		return nil, errors.New(accountstest.CanarySecret)
	}
	return []Observation{a.observation()}, nil
}
func (a *fixtureAdapter) Verify(context.Context, Account) (Observation, error) {
	return a.observation(), nil
}
func (a *fixtureAdapter) Resources(context.Context, Account) ([]ResourceRef, error) {
	return []ResourceRef{{ID: "r-2", Kind: "server", Name: "otter"}, {ID: "r-1", Kind: "server", Name: "owl"}}, nil
}
func (a *fixtureAdapter) Usage(context.Context, Account, string) ([]Metric, error) {
	v := 2.0
	return []Metric{{Name: "requests", Value: &v, Unit: "count", Source: "fixture/usage", Classification: ProviderReported, ObservedAt: a.now, Period: "month", Coverage: Complete}}, nil
}
func (a *fixtureAdapter) Billing(context.Context, Account, string) ([]Metric, error) {
	v := 3.0
	return []Metric{{Name: "cost", Value: &v, Unit: "currency", Currency: "USD", Source: "fixture/billing", Classification: ProviderReported, ObservedAt: a.now, Period: "month", Coverage: Complete}}, nil
}
func (a *fixtureAdapter) Select(_ context.Context, account Account, _ Operation) (Selection, error) {
	return Selection{AccountID: account.ID, ConnectionID: "primary", Profile: "bob", CredentialRef: "profile:bob"}, nil
}

func TestAccountCommandAdapterVerbs(t *testing.T) {
	store := Open(t.TempDir() + "/accounts.json")
	registry := NewRegistry()
	adapter := &fixtureAdapter{now: time.Date(2026, 10, 29, 8, 0, 0, 0, time.UTC)}
	if err := registry.Register(adapter); err != nil {
		t.Fatal(err)
	}

	out, _, err := invokeAccount(t, store, registry, "discover", "--dry-run", "--json")
	if err != nil || decodeEnvelope(t, out)["dry_run"] != true {
		t.Fatalf("discover dry-run: out=%s err=%v", out, err)
	}
	if items, err := store.List(); err != nil || len(items) != 0 {
		t.Fatalf("dry-run wrote store: items=%v err=%v", items, err)
	}
	out, _, err = invokeAccount(t, store, registry, "discover", "--json")
	if err != nil || len(decodeEnvelope(t, out)["accounts"].([]any)) != 1 {
		t.Fatalf("discover: out=%s err=%v", out, err)
	}

	for _, tc := range []struct {
		args []string
		key  string
	}{
		{[]string{"refresh", "bob", "--json"}, "accounts"},
		{[]string{"usage", "bob", "--period", "month", "--json"}, "metrics"},
		{[]string{"resources", "bob", "--json"}, "resources"},
		{[]string{"resolve", "bob", "--for", "server.list", "--json"}, "selection"},
	} {
		out, _, err = invokeAccount(t, store, registry, tc.args...)
		if err != nil || decodeEnvelope(t, out)[tc.key] == nil {
			t.Fatalf("%v: out=%s err=%v", tc.args, out, err)
		}
	}
	out, _, err = invokeAccount(t, store, registry, "refresh", "--all", "--json")
	if err != nil || len(decodeEnvelope(t, out)["accounts"].([]any)) != 1 {
		t.Fatalf("refresh all: out=%s err=%v", out, err)
	}
}

// cacheProofAdapter dies loudly if any adapter method runs. Cached reads
// (list, show, usage, resources) must succeed against it: only explicit
// refresh may call adapters.
type cacheProofAdapter struct {
	t   *testing.T
	now time.Time
}

func (a *cacheProofAdapter) Name() string { return "snap" }
func (a *cacheProofAdapter) Capabilities() Capability {
	return Discover | Verify | Resources | Usage | Billing | Select
}
func (a *cacheProofAdapter) observation() Observation {
	return Observation{Account: Account{
		Alias: "snap", Category: DigitalOcean, Provider: "snap", ProviderAccountID: "acct-snap",
		Status: Resolved, Connections: []Connection{{ID: "primary", Profile: "snap", CredentialRef: "profile:snap"}},
	}}
}
func (a *cacheProofAdapter) Discover(context.Context) ([]Observation, error) {
	a.t.Fatal("cache-only read called Discover")
	return nil, nil
}
func (a *cacheProofAdapter) Verify(context.Context, Account) (Observation, error) {
	a.t.Fatal("cache-only read called Verify")
	return Observation{}, nil
}
func (a *cacheProofAdapter) Resources(context.Context, Account) ([]ResourceRef, error) {
	a.t.Fatal("cache-only read called Resources")
	return nil, nil
}
func (a *cacheProofAdapter) Usage(context.Context, Account, string) ([]Metric, error) {
	a.t.Fatal("cache-only read called Usage")
	return nil, nil
}
func (a *cacheProofAdapter) Billing(context.Context, Account, string) ([]Metric, error) {
	a.t.Fatal("cache-only read called Billing")
	return nil, nil
}
func (a *cacheProofAdapter) Select(_ context.Context, account Account, _ Operation) (Selection, error) {
	a.t.Fatal("cache-only read called Select")
	return Selection{}, nil
}

func seedSnapAccount(t *testing.T, store *Store) string {
	t.Helper()
	if err := store.Observe("snap", []Observation{(&cacheProofAdapter{}).observation()}); err != nil {
		t.Fatal(err)
	}
	items, err := store.List()
	if err != nil || len(items) != 1 {
		t.Fatalf("seeded accounts = %v, %v", items, err)
	}
	return items[0].ID
}

func snapMetric(name string, v float64) Metric {
	return Metric{Name: name, Value: &v, Unit: "count", Source: "snap/usage", Classification: ProviderReported, ObservedAt: time.Date(2026, 10, 29, 8, 0, 0, 0, time.UTC), Period: "month", Coverage: Complete}
}

func TestAccountCacheOnlyReadsNeverCallAdapter(t *testing.T) {
	store := Open(t.TempDir() + "/accounts.json")
	id := seedSnapAccount(t, store)
	observed := time.Date(2026, 10, 29, 9, 30, 0, 0, time.UTC)
	if err := store.SaveSnapshot(Snapshot{
		AccountID:  id,
		Resources:  []ResourceRef{{ID: "r-1", Kind: "server", Name: "owl"}},
		Metrics:    map[string][]Metric{"day": {snapMetric("requests", 1)}, "week": {snapMetric("requests", 2)}, "month": {snapMetric("requests", 3)}},
		ObservedAt: observed,
	}); err != nil {
		t.Fatal(err)
	}
	registry := NewRegistry()
	if err := registry.Register(&cacheProofAdapter{t: t}); err != nil {
		t.Fatal(err)
	}

	if _, _, err := invokeAccount(t, store, registry, "list", "--json"); err != nil {
		t.Fatalf("cached list: %v", err)
	}
	out, _, err := invokeAccount(t, store, registry, "show", id[:10], "--json")
	if err != nil || decodeEnvelope(t, out)["account"] == nil {
		t.Fatalf("cached show: out=%s err=%v", out, err)
	}
	for _, period := range []string{"day", "week", "month"} {
		out, _, err = invokeAccount(t, store, registry, "usage", "snap", "--period", period, "--json")
		if err != nil {
			t.Fatalf("cached usage %s: %v", period, err)
		}
		envelope := decodeEnvelope(t, out)
		rows := envelope["metrics"].([]any)
		if len(rows) != 1 || rows[0].(map[string]any)["name"] != "requests" {
			t.Fatalf("cached usage %s = %#v", period, rows)
		}
		if envelope["observed_at"] != observed.UTC().Format(time.RFC3339) {
			t.Fatalf("usage %s hides snapshot age: %#v", period, envelope["observed_at"])
		}
		if _, ok := envelope["last_error"]; ok {
			t.Fatalf("usage %s reports an error on a good snapshot: %v", period, envelope)
		}
	}
	out, _, err = invokeAccount(t, store, registry, "resources", "snap", "--json")
	if err != nil {
		t.Fatalf("cached resources: %v", err)
	}
	envelope := decodeEnvelope(t, out)
	rows := envelope["resources"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["id"] != "r-1" {
		t.Fatalf("cached resources = %#v", rows)
	}
	if envelope["observed_at"] != observed.UTC().Format(time.RFC3339) {
		t.Fatalf("resources hides snapshot age: %#v", envelope["observed_at"])
	}
	out, _, err = invokeAccount(t, store, registry, "resources", "snap")
	if err != nil || !strings.Contains(out, "observed_at") {
		t.Fatalf("table resources hides snapshot age: out=%s err=%v", out, err)
	}
}

// flakySnapAdapter serves one good refresh and then fails mid-refresh with an
// error carrying the secret canary. The failure must surface as a safe code
// while the previous resources and metrics survive.
type flakySnapAdapter struct {
	now time.Time
	bad bool
}

func (a *flakySnapAdapter) Name() string { return "snap" }
func (a *flakySnapAdapter) Capabilities() Capability {
	return Discover | Verify | Resources | Usage | Billing | Select
}
func (a *flakySnapAdapter) observation() Observation {
	return Observation{Account: Account{
		Alias: "snap", Category: DigitalOcean, Provider: "snap", ProviderAccountID: "acct-snap",
		Status: Resolved, Connections: []Connection{{ID: "primary", Profile: "snap", CredentialRef: "profile:snap"}},
	}}
}
func (a *flakySnapAdapter) Discover(context.Context) ([]Observation, error) {
	return []Observation{a.observation()}, nil
}
func (a *flakySnapAdapter) Verify(context.Context, Account) (Observation, error) {
	return a.observation(), nil
}
func (a *flakySnapAdapter) Resources(context.Context, Account) ([]ResourceRef, error) {
	return []ResourceRef{{ID: "r-1", Kind: "server", Name: "owl"}}, nil
}
func (a *flakySnapAdapter) Usage(context.Context, Account, string) ([]Metric, error) {
	if a.bad {
		return nil, errors.New("adapter boom " + accountstest.CanarySecret)
	}
	v := 3.0
	return []Metric{{Name: "requests", Value: &v, Unit: "count", Source: "snap/usage", Classification: ProviderReported, ObservedAt: a.now, Period: "month", Coverage: Complete}}, nil
}
func (a *flakySnapAdapter) Billing(context.Context, Account, string) ([]Metric, error) {
	v := 4.0
	return []Metric{{Name: "cost", Value: &v, Unit: "usd", Currency: "USD", Source: "snap/billing", Classification: ProviderReported, ObservedAt: a.now, Period: "month", Coverage: Complete}}, nil
}
func (a *flakySnapAdapter) Select(_ context.Context, account Account, _ Operation) (Selection, error) {
	return Selection{AccountID: account.ID, ConnectionID: "primary", Profile: "snap", CredentialRef: "profile:snap"}, nil
}

func TestAccountRefreshKeepsLastGoodOnError(t *testing.T) {
	path := t.TempDir() + "/accounts.json"
	store := Open(path)
	good := &flakySnapAdapter{now: time.Date(2026, 10, 29, 8, 0, 0, 0, time.UTC)}
	registry := NewRegistry()
	if err := registry.Register(good); err != nil {
		t.Fatal(err)
	}
	if _, _, err := invokeAccount(t, store, registry, "discover", "--json"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := invokeAccount(t, store, registry, "refresh", "snap", "--json"); err != nil {
		t.Fatalf("good refresh: %v", err)
	}
	snap, err := store.Snapshot(mustAccountID(t, store, "snap"))
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Resources) != 1 || len(snap.Metrics["month"]) != 2 || snap.LastError != "" {
		t.Fatalf("good snapshot = %+v", snap)
	}

	bad := &flakySnapAdapter{bad: true}
	registry = NewRegistry()
	if err := registry.Register(bad); err != nil {
		t.Fatal(err)
	}
	_, stderr, err := invokeAccount(t, store, registry, "refresh", "snap", "--json")
	if err == nil {
		t.Fatal("failing refresh unexpectedly succeeded")
	}
	if code := decodeEnvelope(t, stderr)["error"].(map[string]any)["code"]; code != "adapter_failed" {
		t.Fatalf("refresh error code = %v", code)
	}
	after, err := store.Snapshot(mustAccountID(t, store, "snap"))
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Resources) != 1 || after.Resources[0].ID != "r-1" {
		t.Fatalf("last-good resources lost: %+v", after.Resources)
	}
	if len(after.Metrics["month"]) != 2 {
		t.Fatalf("last-good metrics lost: %+v", after.Metrics)
	}
	if after.LastError != refreshFailed {
		t.Fatalf("last_error = %q", after.LastError)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	accountstest.AssertNoSecret(t, string(raw))

	out, _, err := invokeAccount(t, store, registry, "usage", "snap", "--period", "month", "--json")
	if err != nil {
		t.Fatalf("usage after failed refresh: %v", err)
	}
	envelope := decodeEnvelope(t, out)
	if len(envelope["metrics"].([]any)) != 2 {
		t.Fatalf("usage lost last-good metrics: %v", envelope["metrics"])
	}
	if envelope["last_error"] != refreshFailed || envelope["observed_at"] == nil {
		t.Fatalf("usage hides refresh failure: %v", envelope)
	}
}

func mustAccountID(t *testing.T, store *Store, alias string) string {
	t.Helper()
	items, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range items {
		if a.Alias == alias {
			return a.ID
		}
	}
	t.Fatalf("account %q not found", alias)
	return ""
}

func TestAccountCommandStructuredErrorsAndFilters(t *testing.T) {
	store := Open(t.TempDir() + "/accounts.json")
	registry := NewRegistry()
	if err := registry.Register(&fixtureAdapter{fail: true}); err != nil {
		t.Fatal(err)
	}
	// Cached reads must not invoke even a registered adapter that would fail.
	if _, _, err := invokeAccount(t, store, registry, "list", "--json"); err != nil {
		t.Fatalf("cached list reached adapter: %v", err)
	}

	for _, args := range [][]string{
		{"show", "--json"},
		{"show", "missing", "--json"},
		{"list", "--fields", "id,unknown", "--json"},
		{"list", "--category", "invalid", "--json"},
		{"usage", "missing", "--period", "year", "--json"},
		{"discover", "--json"},
	} {
		_, stderr, err := invokeAccount(t, store, registry, args...)
		if err == nil {
			t.Fatalf("%v unexpectedly succeeded", args)
		}
		envelope := decodeEnvelope(t, stderr)
		if envelope["error"] == nil {
			t.Fatalf("%v did not emit structured error: %s", args, stderr)
		}
	}
}

func TestAccountCommandTextModeErrors(t *testing.T) {
	store := Open(t.TempDir() + "/accounts.json")
	registry := NewRegistry()
	if err := registry.Register(&fixtureAdapter{fail: true}); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name     string
		args     []string
		wantCode string
	}{
		{
			name:     "add missing args",
			args:     []string{"add"},
			wantCode: "usage",
		},
		{
			name:     "add invalid kind",
			args:     []string{"add", "bad", "--kind", "nonexistent"},
			wantCode: "invalid_category",
		},
		{
			name:     "add invalid credential",
			args:     []string{"add", "bad", "--kind", "host-login", "--user", "alice", "--endpoint", "example.com:22", "--credential", "raw-secret"},
			wantCode: "invalid_credential_ref",
		},
		{
			name:     "add missing host-login details",
			args:     []string{"add", "bad", "--kind", "host-login", "--user", "alice"},
			wantCode: "invalid_account",
		},
		{
			name:     "set missing flags",
			args:     []string{"set", "owl"},
			wantCode: "usage",
		},
		{
			name:     "rm missing account",
			args:     []string{"rm", "missing-id"},
			wantCode: "not_found",
		},
		{
			name:     "restore missing account",
			args:     []string{"restore", "missing-id"},
			wantCode: "not_found",
		},
		{
			name:     "refresh missing account",
			args:     []string{"refresh", "missing-id"},
			wantCode: "not_found",
		},
		{
			name:     "discover adapter failure",
			args:     []string{"discover"},
			wantCode: "adapter_failed",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, err := invokeAccount(t, store, registry, tc.args...)
			if err == nil {
				t.Fatalf("%s unexpectedly succeeded (stdout: %s, stderr: %s)", tc.name, stdout, stderr)
			}
			if !strings.Contains(stderr, "account: "+tc.wantCode+":") {
				t.Fatalf("%s: stderr = %q, want prefix 'account: %s:'", tc.name, stderr, tc.wantCode)
			}
		})
	}
}

func TestAccountCommandDuplicateIdentity(t *testing.T) {
	store := Open(t.TempDir() + "/accounts.json")
	registry := NewRegistry()

	// Initial account creation with OpenSSH fingerprint credential
	out, _, err := invokeAccount(t, store, registry, "add", "owl", "--kind", "host-login", "--user", "alice", "--endpoint", "example.com:22", "--credential", "ssh-key:SHA256:oFTyb/wLPkeVpfBjq8i4XoY9zs4+AoWome8V4/m5jxg", "--json")
	if err != nil {
		t.Fatal(err)
	}
	env := decodeEnvelope(t, out)
	acc := env["account"].(map[string]any)
	id := acc["id"].(string)

	// Attempt duplicate identity in text mode: must name alias and id, exit 1, no secrets
	stdout, stderr, err := invokeAccount(t, store, registry, "add", "owl2", "--kind", "host-login", "--user", "alice", "--endpoint", "example.com:22", "--credential", "secret:owl2")
	if err == nil {
		t.Fatal("duplicate add unexpectedly succeeded in text mode")
	}
	wantTextErr := "account: duplicate_identity: identity already recorded as owl (" + id + ")\n"
	if stderr != wantTextErr {
		t.Fatalf("duplicate add text stderr = %q, want %q", stderr, wantTextErr)
	}
	if stdout != "" {
		t.Fatalf("duplicate add text stdout = %q, want empty", stdout)
	}

	// Attempt duplicate identity in JSON mode: structured error with code + message naming alias and id
	stdout, stderr, err = invokeAccount(t, store, registry, "add", "owl3", "--kind", "host-login", "--user", "alice", "--endpoint", "example.com:22", "--credential", "secret:owl3", "--json")
	if err == nil {
		t.Fatal("duplicate add unexpectedly succeeded in JSON mode")
	}
	jsonEnv := decodeEnvelope(t, stderr)
	errObj := jsonEnv["error"].(map[string]any)
	if errObj["code"] != "duplicate_identity" {
		t.Fatalf("error code = %v, want duplicate_identity", errObj["code"])
	}
	wantMsg := "identity already recorded as owl (" + id + ")"
	if errObj["message"] != wantMsg {
		t.Fatalf("error message = %v, want %q", errObj["message"], wantMsg)
	}

	// Duplicate identity when existing account has no alias (e.g. from adapter observation)
	obs := Observation{
		Account: Account{
			Category: HostLogin,
			Provider: "host-login",
			Username: "bob",
			Connections: []Connection{{
				ID:            "main",
				Endpoint:      "box.example.com:22",
				CredentialRef: "ssh-key:MD5:aa:bb:cc:dd:ee:ff:00:11:22:33:44:55:66:77:88:99",
			}},
		},
	}
	if err := store.Observe("fake", []Observation{obs}); err != nil {
		t.Fatal(err)
	}
	list, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	var bobID string
	for _, a := range list {
		if a.Username == "bob" {
			bobID = a.ID
			break
		}
	}
	if bobID == "" {
		t.Fatal("bob account not found in store")
	}

	_, stderr, err = invokeAccount(t, store, registry, "add", "bob-box", "--kind", "host-login", "--user", "bob", "--endpoint", "box.example.com:22", "--credential", "secret:bob")
	if err == nil {
		t.Fatal("duplicate add without alias unexpectedly succeeded")
	}
	wantNoAliasErr := "account: duplicate_identity: identity already recorded as " + bobID + "\n"
	if stderr != wantNoAliasErr {
		t.Fatalf("stderr = %q, want %q", stderr, wantNoAliasErr)
	}
}

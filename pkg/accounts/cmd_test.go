package accounts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

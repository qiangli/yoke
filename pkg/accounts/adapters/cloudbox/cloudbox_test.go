package cloudbox

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/accounts"
	"github.com/qiangli/yoke/pkg/accounts/accountstest"
)

func write(t *testing.T, p, s string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

func fixture(t *testing.T) string {
	root := t.TempDir()
	write(t, filepath.Join(root, ".active"), "p1\n")
	mk := func(name, server, user, host, extra string) {
		d := filepath.Join(root, "profiles", name)
		write(t, filepath.Join(d, "agent.json"), `{"agent_name":"`+name+`","user":"`+user+`","token":"`+accountstest.CanarySecret+`"`+extra+`}`)
		write(t, filepath.Join(d, "server"), server+"\n")
		write(t, filepath.Join(d, "host"), host+"\n")
	}
	mk("p1", "https://one.example.com", "alice", "owl", "")
	mk("p2", "https://one.example.com", "bob", "owl", "")
	mk("p3", "https://two.example.com", "alice", "otter", "")
	mk("p4", "https://two.example.com", "bob", "otter", `,"expires_at":"2020-01-01T00:00:00Z"`)
	write(t, filepath.Join(root, "agent.json"), `{"agent_name":"p1","user":"alice","token":"`+accountstest.CanarySecret+`"}`)
	write(t, filepath.Join(root, "agent.json"), `{"agent_name":"p1","user":"alice","server":"https://one.example.com","token":"`+accountstest.CanarySecret+`"}`)
	return root
}

const remotes = `[{"name":"sat-a","server":"https://one.example.com","user":"alice"},
{"name":"sat-b","server":"https://one.example.com","user":"bob"},
{"name":"sat-c","server":"https://two.example.com","user":"alice"},
{"name":"sat-d","server":"https://two.example.com","user":"alice"}]`

func okExec(_ context.Context, args ...string) ([]byte, error) {
	if args[0] == "status" {
		return []byte(`{"agent_name":"p1"}`), nil
	}
	return []byte(remotes), nil
}

func find(obs []accounts.Observation, id string) accounts.Account {
	for _, o := range obs {
		if o.Account.ProviderAccountID == id {
			return o.Account
		}
	}
	return accounts.Account{}
}

func TestDiscoverSeparatesEndpointsAndUsers(t *testing.T) {
	a := &Adapter{ConfigDir: fixture(t), Exec: okExec, Now: func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }}
	obs, err := a.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// p1 appears via the active top-level agent.json and profiles/p1: same account.
	if len(obs) != 4 {
		t.Fatalf("want 4 accounts, got %d", len(obs))
	}
	a1 := find(obs, "https://one.example.com|alice")
	if len(a1.Connections) != 2 { // top-level active + profiles/p1
		t.Fatalf("%+v", a1.Connections)
	}
	for _, c := range a1.Connections {
		if c.CredentialRef != "profile:p1" || c.Endpoint != "https://one.example.com" {
			t.Fatalf("crossover: %+v", c)
		}
	}
	if s := find(obs, "https://one.example.com|bob").Links.Satellites; len(s) != 1 || s[0] != "sat-b" {
		t.Fatalf("%v", s)
	}
	if s := find(obs, "https://two.example.com|alice").Links.Satellites; len(s) != 2 {
		t.Fatalf("%v", s)
	}
	exp := find(obs, "https://two.example.com|bob")
	if exp.LastError != "credential_expired" || exp.Status != accounts.Error {
		t.Fatalf("%+v", exp)
	}
	st := accounts.Open(filepath.Join(t.TempDir(), "a.json"))
	if err := st.Observe(Name, obs); err != nil {
		t.Fatal(err)
	}
	list, _ := st.List()
	b, _ := json.Marshal(list)
	raw, _ := os.ReadFile(st.Path)
	accountstest.AssertNoSecret(t, obs, string(b), string(raw))
}

func TestCachedWhenUnreachableOrForbidden(t *testing.T) {
	for msg, code := range map[string]string{"dial tcp: refused": "unreachable", "HTTP 403 " + accountstest.CanarySecret: "forbidden", "token expired": "credential_expired"} {
		root := fixture(t)
		cache := filepath.Join(t.TempDir(), "cache.json")
		a := &Adapter{ConfigDir: root, CachePath: cache, Exec: okExec}
		// drop expired profile so the good run is fully clean
		os.RemoveAll(filepath.Join(root, "profiles", "p4"))
		if _, err := a.Discover(context.Background()); err != nil {
			t.Fatal(err)
		}
		a2 := &Adapter{ConfigDir: t.TempDir(), CachePath: cache, Exec: func(context.Context, ...string) ([]byte, error) {
			return nil, errors.New(msg)
		}}
		obs, err := a2.Discover(context.Background())
		if err != nil || len(obs) != 3 {
			t.Fatalf("%v %d", err, len(obs))
		}
		for _, o := range obs {
			if o.Account.LastError != code || len(o.Account.Connections) == 0 {
				t.Fatalf("%s: %+v", code, o.Account)
			}
		}
		if len(find(obs, "https://two.example.com|alice").Links.Satellites) != 2 {
			t.Fatal("satellites lost from cache")
		}
		accountstest.AssertNoSecret(t, obs)
		raw, _ := os.ReadFile(cache)
		accountstest.AssertNoSecret(t, string(raw))
	}
}

func TestDaemonDownKeepsProfiles(t *testing.T) {
	a := &Adapter{ConfigDir: fixture(t), Exec: func(context.Context, ...string) ([]byte, error) { return nil, errors.New("connection refused") }}
	obs, err := a.Discover(context.Background())
	if err != nil || len(obs) != 4 {
		t.Fatal(err, len(obs))
	}
	if find(obs, "https://one.example.com|bob").LastError != "unreachable" {
		t.Fatal("expected unreachable")
	}
	_, err = (&Adapter{ConfigDir: t.TempDir(), Exec: func(context.Context, ...string) ([]byte, error) {
		return nil, errors.New("HTTP 403 " + accountstest.CanarySecret)
	}}).Discover(context.Background())
	if err == nil || strings.Contains(err.Error(), accountstest.CanarySecret) {
		t.Fatal(err)
	}
}

// A host with no outpost installed, no profiles and nothing cached has no
// cloudbox account; that must not fail the whole discover.
func TestDiscoverNotInstalledNoProfilesIsEmpty(t *testing.T) {
	a := &Adapter{ConfigDir: t.TempDir(), CachePath: filepath.Join(t.TempDir(), "cache.json"), Exec: func(context.Context, ...string) ([]byte, error) {
		return nil, errors.New("outpost not installed")
	}}
	obs, err := a.Discover(context.Background())
	if err != nil || len(obs) != 0 {
		t.Fatalf("want no observations and no error, got %v %+v", err, obs)
	}
}

func TestRegisterAndSelect(t *testing.T) {
	r := accounts.NewRegistry()
	if err := Register(r); err != nil {
		t.Fatal(err)
	}
	ad, err := r.Adapter(Name, accounts.Discover)
	if err != nil {
		t.Fatal(err)
	}
	two := accounts.Account{Connections: []accounts.Connection{{ID: "a"}, {ID: "b"}}}
	if _, err := ad.Select(context.Background(), two, "x"); !errors.Is(err, accounts.ErrAmbiguous) {
		t.Fatal(err)
	}
}

func TestDiscoverRealShapeFixtures(t *testing.T) {
	root := t.TempDir()

	// Top-level agent.json without sidecar files (no "server" or "host" sidecar)
	write(t, filepath.Join(root, "agent.json"), `{
		"agent_name": "owl",
		"server_addr": "one.example.com",
		"server_port": 443,
		"protocol": "wss",
		"token": "`+accountstest.CanarySecret+`",
		"access_token": "`+accountstest.CanarySecret+`",
		"admin_session_key": "`+accountstest.CanarySecret+`",
		"mcp_bearer_token": "`+accountstest.CanarySecret+`",
		"files_signing_key": "`+accountstest.CanarySecret+`",
		"cloudbox_ticket_pubkey": "`+accountstest.CanarySecret+`",
		"cluster_node_token": "`+accountstest.CanarySecret+`",
		"custom_key": "`+accountstest.CanarySecret+`",
		"cookie": "`+accountstest.CanarySecret+`",
		"password": "`+accountstest.CanarySecret+`"
	}`)

	// Profile 1: with sidecar "server" file, same agent_name "owl"
	p1Dir := filepath.Join(root, "profiles", "sat")
	write(t, filepath.Join(p1Dir, "server"), "https://two.example.com\n")
	write(t, filepath.Join(p1Dir, "agent.json"), `{
		"agent_name": "owl",
		"server_addr": "two.example.com",
		"server_port": 443,
		"protocol": "wss",
		"token": "`+accountstest.CanarySecret+`",
		"access_token": "`+accountstest.CanarySecret+`",
		"admin_session_key": "`+accountstest.CanarySecret+`",
		"mcp_bearer_token": "`+accountstest.CanarySecret+`",
		"files_signing_key": "`+accountstest.CanarySecret+`"
	}`)

	// Profile 2: with sidecar "server" file, different agent_name "otter"
	p2Dir := filepath.Join(root, "profiles", "other")
	write(t, filepath.Join(p2Dir, "server"), "https://three.example.com\n")
	write(t, filepath.Join(p2Dir, "agent.json"), `{
		"agent_name": "otter",
		"server_addr": "three.example.com",
		"server_port": 443,
		"protocol": "wss",
		"token": "`+accountstest.CanarySecret+`",
		"access_token": "`+accountstest.CanarySecret+`"
	}`)

	// Profile 3: empty config (neither endpoint nor agent_name known)
	p3Dir := filepath.Join(root, "profiles", "unresolved")
	write(t, filepath.Join(p3Dir, "agent.json"), `{}`)

	mockExec := func(_ context.Context, args ...string) ([]byte, error) {
		if args[0] == "status" {
			return []byte(`{
				"config": {
					"agent_name": "owl",
					"server_addr": "one.example.com",
					"server_port": 443,
					"protocol": "wss",
					"cloudbox_url": "https://one.example.com"
				},
				"state": "active (paired as owl)",
				"status": {
					"configured": true,
					"agent_name": "owl",
					"server_addr": "one.example.com",
					"cloudbox_url": "https://one.example.com",
					"current_os_user": "tester"
				}
			}`), nil
		}
		return []byte(`[]`), nil
	}

	a := &Adapter{ConfigDir: root, Exec: mockExec, Now: func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }}
	obs, err := a.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	// Active endpoint: resolved, username empty, name = agent_name, links satellite "sat"
	activeAcc := find(obs, "https://one.example.com|owl")
	if activeAcc.Status != accounts.Resolved {
		t.Fatalf("expected active account to be Resolved, got %v", activeAcc.Status)
	}
	if activeAcc.Username != "" {
		t.Fatalf("expected Username to be empty, got %q", activeAcc.Username)
	}
	if activeAcc.Name != "owl" {
		t.Fatalf("expected Name to be 'owl', got %q", activeAcc.Name)
	}
	if len(activeAcc.Connections) == 0 || activeAcc.Connections[0].Endpoint != "https://one.example.com" {
		t.Fatalf("expected endpoint 'https://one.example.com', got %+v", activeAcc.Connections)
	}
	if len(activeAcc.Links.Satellites) != 1 || activeAcc.Links.Satellites[0] != "sat" {
		t.Fatalf("expected satellite 'sat' linked, got %v", activeAcc.Links.Satellites)
	}

	// Satellite endpoint: separate resolved account, username empty, name = agent_name
	satAcc := find(obs, "https://two.example.com|owl")
	if satAcc.Status != accounts.Resolved {
		t.Fatalf("expected sat account to be Resolved, got %v", satAcc.Status)
	}
	if satAcc.Username != "" {
		t.Fatalf("expected sat Username to be empty, got %q", satAcc.Username)
	}
	if satAcc.Name != "owl" {
		t.Fatalf("expected sat Name to be 'owl', got %q", satAcc.Name)
	}
	if len(satAcc.Connections) == 0 || satAcc.Connections[0].Endpoint != "https://two.example.com" {
		t.Fatalf("expected endpoint 'https://two.example.com', got %+v", satAcc.Connections)
	}
	// Two endpoints stay separate; sat does not link itself
	if len(satAcc.Links.Satellites) != 0 {
		t.Fatalf("expected sat satellites to be empty, got %v", satAcc.Links.Satellites)
	}

	// Other endpoint with different agent_name: separate account, not linked to owl
	otherAcc := find(obs, "https://three.example.com|otter")
	if otherAcc.Status != accounts.Resolved {
		t.Fatalf("expected other account to be Resolved, got %v", otherAcc.Status)
	}
	if otherAcc.Username != "" || otherAcc.Name != "otter" {
		t.Fatalf("expected otter account, got Username=%q Name=%q", otherAcc.Username, otherAcc.Name)
	}

	// Unresolved account when neither endpoint nor agent_name is known
	var unresAcc accounts.Account
	for _, o := range obs {
		if o.Account.Status == accounts.Unresolved {
			unresAcc = o.Account
			break
		}
	}
	if unresAcc.Status != accounts.Unresolved {
		t.Fatalf("expected an Unresolved account for empty profile, got none")
	}

	// Sentinel test: verify secrets are never present in records, JSON, or store
	st := accounts.Open(filepath.Join(t.TempDir(), "store.json"))
	if err := st.Observe(Name, obs); err != nil {
		t.Fatal(err)
	}
	list, _ := st.List()
	b, _ := json.Marshal(list)
	raw, _ := os.ReadFile(st.Path)
	accountstest.AssertNoSecret(t, obs, string(b), string(raw))
}

// A completed `outpost status` is the login check for the active profile, so
// discovery reports what it proved to the inventory without waiting for a
// refresh — and an expired profile reports the transition instead.
func TestDiscoverRecordsProvenProfiles(t *testing.T) {
	store := accounts.Open(filepath.Join(t.TempDir(), "accounts.json"))
	accounts.SetHookStore(store)
	t.Cleanup(func() { accounts.SetHookStore(nil) })
	a := &Adapter{ConfigDir: fixture(t), Exec: okExec, Now: func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }}
	if _, err := a.Discover(context.Background()); err != nil {
		t.Fatal(err)
	}
	list, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 4 {
		t.Fatalf("recorded %d accounts: %+v", len(list), list)
	}
	var expired, good int
	for _, acc := range list {
		if acc.Category != accounts.Cloudbox || acc.Provenance != Name || acc.Source != "outpost-profile" {
			t.Fatalf("provenance: %+v", acc)
		}
		switch acc.ProviderAccountID {
		case "https://two.example.com|bob":
			expired++
			if acc.Status != accounts.Error || acc.LastError != "credential_expired" {
				t.Fatalf("expiry not recorded: %+v", acc)
			}
		default:
			good++
			if acc.Status != accounts.Resolved || acc.LastError != "" {
				t.Fatalf("login not recorded: %+v", acc)
			}
			if len(acc.Connections) != 1 || !strings.HasPrefix(acc.Connections[0].CredentialRef, "profile:") {
				t.Fatalf("connection: %+v", acc.Connections)
			}
		}
	}
	if expired != 1 || good != 3 {
		t.Fatalf("expired=%d good=%d", expired, good)
	}
	// A repeated discovery is the same four accounts, not eight.
	if _, err = a.Discover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if list, err = store.List(); err != nil || len(list) != 4 {
		t.Fatalf("repeat discovery: %d %v", len(list), err)
	}
	raw, _ := os.ReadFile(store.Path)
	accountstest.AssertNoSecret(t, string(raw))
}

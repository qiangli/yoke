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
	_, err = (&Adapter{ConfigDir: t.TempDir(), Exec: func(context.Context, ...string) ([]byte, error) { return nil, errors.New("x " + accountstest.CanarySecret) }}).Discover(context.Background())
	if err == nil || strings.Contains(err.Error(), accountstest.CanarySecret) {
		t.Fatal(err)
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

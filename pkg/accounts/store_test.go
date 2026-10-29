package accounts

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/accounts/accountstest"
)

func fixture(id string) Observation {
	return Observation{Account: Account{Category: DigitalOcean, Provider: "example", ProviderAccountID: id, Name: "owl", Email: "alice@example.com", Connections: []Connection{{ID: "main", CredentialRef: "secret:owl"}}}}
}
func TestObserveAndOverrides(t *testing.T) {
	s := Open(filepath.Join(t.TempDir(), "accounts", "accounts.json"))
	a := fixture("team-a")
	if err := s.Observe("fake", []Observation{a}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(s.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Observe("fake", []Observation{a}); err != nil {
		t.Fatal(err)
	}
	b2, _ := os.ReadFile(s.Path)
	if string(b) != string(b2) {
		t.Fatal("repeat changed bytes")
	}
	list, _ := s.List()
	id := list[0].ID
	alias := "otter"
	if err = s.Override(id, Override{Alias: &alias}); err != nil {
		t.Fatal(err)
	}
	a.Account.Name = "otter"
	if err = s.Observe("fake", []Observation{a, fixture("team-b")}); err != nil {
		t.Fatal(err)
	}
	list, _ = s.List()
	if len(list) != 2 || list[1].ID != id || list[1].Alias != alias {
		t.Fatalf("bad reconciliation: %+v", list)
	}
	if err = s.SetBindings(id, []string{"agent"}); err != nil {
		t.Fatal(err)
	}
	if err = s.Remove(id); !errors.Is(err, ErrReferenced) {
		t.Fatal(err)
	}
	if err = s.SetBindings(id, nil); err != nil {
		t.Fatal(err)
	}
	if err = s.Remove(id); err != nil {
		t.Fatal(err)
	}
	if err = s.Observe("fake", []Observation{a}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(id)
	if got.Status != Suppressed {
		t.Fatal(got.Status)
	}
	if err = s.Restore(id); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Get(id)
	if got.Status != Resolved {
		t.Fatal(got.Status)
	}
}
func TestUnresolvedAndHostUsers(t *testing.T) {
	s := Open(filepath.Join(t.TempDir(), "a.json"))
	u := fixture("")
	u.SourceKey = "owl"
	v := u
	v.SourceKey = "otter"
	if err := s.Observe("fake", []Observation{u, v}); err != nil {
		t.Fatal(err)
	}
	list, _ := s.List()
	if len(list) != 2 || list[0].Status != Unresolved || list[0].ID == list[1].ID {
		t.Fatal(list)
	}
	h := func(user string) Observation {
		return Observation{Account: Account{Category: HostLogin, Provider: "ssh", Username: user, Connections: []Connection{{Endpoint: "example.com:22", CredentialRef: "ssh-key:aa:bb"}}}}
	}
	if err := s.Observe("fake", []Observation{h("alice"), h("bob")}); err != nil {
		t.Fatal(err)
	}
	list, _ = s.List()
	if len(list) != 4 {
		t.Fatal(len(list))
	}
}
func TestConcurrentProcesses(t *testing.T) {
	if os.Getenv("ACCOUNTS_HELPER") == "1" {
		s := Open(os.Getenv("ACCOUNTS_PATH"))
		n, _ := strconv.Atoi(os.Getenv("ACCOUNTS_START"))
		for i := 0; i < 20; i++ {
			if err := s.Observe("fake", []Observation{fixture(strconv.Itoa(n + i))}); err != nil {
				t.Fatal(err)
			}
		}
		return
	}
	path := filepath.Join(t.TempDir(), "accounts", "accounts.json")
	var wg sync.WaitGroup
	for p := 0; p < 2; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			cmd := exec.Command(os.Args[0], "-test.run=^TestConcurrentProcesses$")
			cmd.Env = append(os.Environ(), "ACCOUNTS_HELPER=1", "ACCOUNTS_PATH="+path, "ACCOUNTS_START="+strconv.Itoa(p*100))
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("helper: %v %s", err, out)
			}
		}(p)
	}
	for p := 2; p < 4; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			s := Open(path)
			for i := 0; i < 20; i++ {
				if err := s.Observe("fake", []Observation{fixture(strconv.Itoa(p*100 + i))}); err != nil {
					t.Error(err)
				}
			}
		}(p)
	}
	wg.Wait()
	list, err := Open(path).List()
	if err != nil || len(list) != 80 {
		t.Fatalf("got %d accounts: %v", len(list), err)
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0o600 {
		t.Fatal(st.Mode())
	}
	st, _ = os.Stat(filepath.Dir(path))
	if st.Mode().Perm() != 0o700 {
		t.Fatal(st.Mode())
	}
}
func TestSecretAndSchema(t *testing.T) {
	s := Open(filepath.Join(t.TempDir(), "accounts.json"))
	a := fixture("one")
	a.Account.Connections[0].CredentialRef = accountstest.CanarySecret
	if err := s.Observe("fake", []Observation{a}); err == nil {
		t.Fatal("raw secret accepted")
	} else {
		accountstest.AssertNoSecret(t, err)
	}
	a.Account.Connections[0].CredentialRef = "env:OWL_TOKEN"
	if err := s.Observe("fake", []Observation{a}); err != nil {
		t.Fatal(err)
	}
	list, _ := s.List()
	b, _ := json.Marshal(list)
	accountstest.AssertNoSecret(t, string(b))
	sel, err := s.Resolve(list[0].ID, "connect")
	if err != nil {
		t.Fatal(err)
	}
	accountstest.AssertNoSecret(t, sel)
	if !strings.HasPrefix(sel.CredentialRef, "env:") {
		t.Fatal(sel)
	}
	if err = os.WriteFile(s.Path, []byte(`{"schema_version":2,"records":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = s.List(); !errors.Is(err, ErrNewerSchema) {
		t.Fatal(err)
	}
}
func TestMetricRefusals(t *testing.T) {
	v := 1.0
	start := time.Now()
	end := start.Add(time.Hour)
	m := Metric{Name: "tokens", Value: &v, Unit: "tokens", Currency: "", Source: "provider:one", WindowStart: &start, WindowEnd: &end}
	n := m
	n.Currency = "USD"
	if _, err := SumMetrics(m, n); !errors.Is(err, ErrIncompatibleMetric) {
		t.Fatal(err)
	}
	n = m
	n.Unit = "requests"
	if _, err := SumMetrics(m, n); !errors.Is(err, ErrIncompatibleMetric) {
		t.Fatal(err)
	}
	n = m
	n.Source = "provider:two"
	if _, err := SumMetrics(m, n); !errors.Is(err, ErrOverlappingMetric) {
		t.Fatal(err)
	}
	n = m
	n.Name = "current-context-tokens"
	if _, err := SumMetrics(n); !errors.Is(err, ErrCurrentContext) {
		t.Fatal(err)
	}
	n = m
	n.Value = nil
	if _, err := SumMetrics(n); !errors.Is(err, ErrIncompatibleMetric) {
		t.Fatal(err)
	}
}

func TestManualHostAndDistinctProfiles(t *testing.T) {
	s := Open(filepath.Join(t.TempDir(), "accounts.json"))
	host := Account{Alias: "owl", Category: HostLogin, Provider: "ssh", Username: "alice", Connections: []Connection{{ID: "main", Endpoint: "example.com:22", AuthMethod: "password", CredentialRef: "secret:owl"}}}
	a, err := s.Add(host)
	if err != nil {
		t.Fatal(err)
	}
	sel, err := s.Resolve("owl", "connect")
	if err != nil || sel.Username != "alice" || sel.Endpoint != "example.com:22" {
		t.Fatalf("selection: %+v %v", sel, err)
	}
	alias := "otter"
	if err = s.Override(a.ID, Override{Alias: &alias}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(alias)
	if err != nil || got.ID != a.ID {
		t.Fatal("editable alias changed ID")
	}
	for _, profile := range []string{"owl", "otter"} {
		o := Observation{Account: Account{Category: AWS, Provider: "aws", ProviderAccountID: "123456", Connections: []Connection{{ID: profile, Profile: profile, CredentialRef: "profile:" + profile}}}}
		if err = s.Observe("fake", []Observation{o}); err != nil {
			t.Fatal(err)
		}
	}
	list, err := s.List()
	if err != nil || len(list) != 3 {
		t.Fatalf("profiles merged: %d %v", len(list), err)
	}
}

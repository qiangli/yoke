package steward

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/bus"
)

func TestRolesForBoardDoesNotCreateSeatOrRegistry(t *testing.T) {
	seatHome := t.TempDir()
	accountHome := t.TempDir()
	t.Setenv("HOME", seatHome)
	oldHome := accountHomeFn
	accountHomeFn = func() (string, string, error) { return accountHome, "account:test", nil }
	t.Cleanup(func() { accountHomeFn = oldHome })

	roles := rolesForBoard()
	if len(roles) != 1 || roles[0].Label != "steward" {
		t.Fatalf("unexpected vacant role list: %+v", roles)
	}
	for _, path := range []string{filepath.Join(seatHome, ".bashy", "steward"), filepath.Join(accountHome, ".bashy", "steward")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("read-only role lookup created %s: %v", path, err)
		}
	}
}

func TestRolesForBoard_HolderUUIDAndLegacyName(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("BASHY_HOME", t.TempDir())
	home := t.TempDir()
	oldHome := accountHomeFn
	accountHomeFn = func() (string, string, error) { return home, "account:test", nil }
	t.Cleanup(func() { accountHomeFn = oldHome })
	s, err := Open("", WithVerifier(verified()))
	if err != nil {
		t.Fatal(err)
	}
	holder := agent("Esme-2")
	holder.Episode = "11111111-1111-4111-8111-111111111111"
	mustClaim(t, s, holder, time.Now())
	prev := bus.HostRoles
	bus.HostRoles = rolesForBoard
	t.Cleanup(func() { bus.HostRoles = prev })
	roles := rolesForBoard()
	if len(roles) == 0 {
		t.Fatal("missing steward role")
	}
	if roles[0].Holder != holder.Name || roles[0].HolderInstance != holder.Episode {
		t.Fatalf("role omitted holder identity: %+v", roles[0])
	}
	p := bus.Post{To: roles[0].Topic}
	if !p.Directed(holder.Episode) {
		t.Error("holder UUID cannot read steward mail")
	}
	if !p.Directed(holder.Name) {
		t.Error("legacy holder name cannot read steward mail")
	}
	if p.Directed("intruder") {
		t.Error("non-holder reads steward mail")
	}
	if p.Directed("22222222-2222-4222-8222-222222222222") {
		t.Error("different instance reads steward mail")
	}
}

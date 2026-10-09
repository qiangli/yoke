package meet

import (
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/bus"
	"github.com/qiangli/yoke/pkg/fleet"
)

func meetInstances(t *testing.T) *fleet.InstanceStore {
	t.Helper()
	t.Setenv("BASHY_PRINCIPAL", "")
	t.Setenv("BASHY_AGENT", "")
	t.Setenv("BASHY_AGENT_ID", "")
	s := fleet.NewInstanceStore(t.TempDir())
	t.Setenv(fleet.InstanceCapEnv, "5")
	prev := bus.InstanceStoreFn
	bus.InstanceStoreFn = func() *fleet.InstanceStore { return s }
	t.Cleanup(func() { bus.InstanceStoreFn = prev })
	return s
}

func openInst(t *testing.T, s *fleet.InstanceStore, label string) fleet.Instance {
	t.Helper()
	i, err := s.Open(fleet.Family{Name: "esme", Display: "Esme", Policy: "single", Bindings: []string{"claude:opus5"}},
		fleet.OpenOptions{Label: label})
	if err != nil {
		t.Fatal(err)
	}
	return i
}

func boardWith(t *testing.T, seats ...string) *State {
	t.Helper()
	st := newRoom(t)
	st.Board = true
	st.Participants = seats
	if err := st.save(); err != nil {
		t.Fatal(err)
	}
	return st
}

func transcriptLen(t *testing.T, st *State) int {
	t.Helper()
	ev, _ := readTranscript(st.ID)
	return len(ev)
}

func TestPostAs_RefusalsWriteNothing(t *testing.T) {
	s := meetInstances(t)
	a, b := openInst(t, s, "Ada"), openInst(t, s, "Bea")
	gone := openInst(t, s, "Cy")
	if _, err := s.Retire(gone.UUID, func(fleet.Instance) ([]string, error) { return []string{`{}`}, nil }); err != nil {
		t.Fatal(err)
	}
	st := boardWith(t, "codex", strings.ToLower(a.UUID), strings.ToLower(b.UUID))

	for name, to := range map[string]string{
		"ambiguous family": "esme",
		"retired label":    "Cy",
		"retired uuid":     gone.UUID,
		"invalid role":     "conductor:404",
	} {
		before := transcriptLen(t, st)
		if _, err := PostAs(st.ID, "codex", to, "x"); err == nil {
			t.Errorf("%s: PostAs accepted %q", name, to)
		}
		if transcriptLen(t, st) != before {
			t.Errorf("%s: a refused post wrote to the transcript", name)
		}
	}
}

func TestPostAs_InstanceTargetStoresUUIDAndSnapshot(t *testing.T) {
	s := meetInstances(t)
	a := openInst(t, s, "Ada")
	uid := strings.ToLower(a.UUID)
	st := boardWith(t, "codex", uid)

	ev, err := PostAs(st.ID, "codex", "Ada", "hello")
	if err != nil {
		t.Fatal(err)
	}
	if ev.To != uid || ev.ToParty == nil || ev.ToParty.UUID != a.UUID || ev.ToParty.Label != "Ada" {
		t.Fatalf("event = %+v", ev)
	}

	// Membership is still enforced: an instance without a seat is refused.
	b := openInst(t, s, "Bea")
	before := transcriptLen(t, st)
	if _, err := PostAs(st.ID, "codex", b.UUID, "x"); err == nil || !strings.Contains(err.Error(), "bashy meet invite") {
		t.Fatalf("unseated instance error = %v", err)
	}
	if transcriptLen(t, st) != before {
		t.Error("unseated post wrote")
	}
}

func TestPostAs_RoleStaysARoleAcrossHandoff(t *testing.T) {
	meetInstances(t)
	holder := "codex"
	prev := bus.HostRoles
	bus.HostRoles = func() []bus.HostRole {
		return []bus.HostRole{{Label: "conductor:22", Topic: "conductor.22", Holder: holder}}
	}
	t.Cleanup(func() { bus.HostRoles = prev })
	st := boardWith(t, "codex", "opencode")

	ev, err := PostAs(st.ID, "opencode", "conductor:22", "status?")
	if err != nil {
		t.Fatal(err)
	}
	if ev.To != "conductor:22" {
		t.Fatalf("role stored as %q — must be the seat, not %q", ev.To, holder)
	}
	holder = "opencode" // handover: the stored address is unchanged
	if got, _ := readTranscript(st.ID); got[0].To != "conductor:22" {
		t.Errorf("handover rewrote the address: %q", got[0].To)
	}
}

func TestRoutableSeat_ActiveInstanceYesRetiredNo(t *testing.T) {
	s := meetInstances(t)
	a := openInst(t, s, "Ada")
	if err := routableSeat(a.UUID); err != nil {
		t.Errorf("active instance: %v", err)
	}
	if _, err := s.Retire(a.UUID, func(fleet.Instance) ([]string, error) { return []string{`{}`}, nil }); err != nil {
		t.Fatal(err)
	}
	if err := routableSeat(a.UUID); err == nil || !strings.Contains(err.Error(), "retired") {
		t.Errorf("retired instance: %v", err)
	}
}

func TestDM_ColdInstanceQueuesByUUIDWithWarning(t *testing.T) {
	s := meetInstances(t)
	t.Setenv("BASHY_MEET_DIR", t.TempDir())
	a := openInst(t, s, "Ada")
	prev := dmPeerLive
	dmPeerLive = func(string) (bool, error) { return false, nil }
	t.Cleanup(func() { dmPeerLive = prev })
	cmd := newDMCmd()
	cmd.SetArgs([]string{"--as", "codex", a.UUID})
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"no read evidence", "not proof", "unverified"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in %s", want, out.String())
		}
	}
	name, err := directMessageRoomName("codex", a.UUID)
	if err != nil {
		t.Fatal(err)
	}
	id, err := resolveMeeting("@" + name)
	if err != nil {
		t.Fatal(err)
	}
	st, err := loadState(id)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Board {
		t.Fatal("cold instance DM must queue on a board")
	}
	ev, err := PostAs(id, "codex", a.UUID, "queued for Ada")
	if err != nil {
		t.Fatal(err)
	}
	if ev.To != a.UUID || ev.ToParty == nil || ev.ToParty.UUID != a.UUID {
		t.Fatalf("queued event: %+v", ev)
	}
	if _, err := s.Retire(a.UUID, func(fleet.Instance) ([]string, error) { return []string{`{}`}, nil }); err != nil {
		t.Fatal(err)
	}
	fresh := openInst(t, s, "Ada")
	events, err := readTranscript(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].To != a.UUID || events[0].To == fresh.UUID {
		t.Fatalf("queued mail followed reused label: %+v", events)
	}
}

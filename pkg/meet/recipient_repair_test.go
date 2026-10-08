package meet

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/bus"
	"github.com/qiangli/yoke/pkg/fleet"
)

func TestPostAs_UnknownUUIDFailsClosedEvenWhenSeated(t *testing.T) {
	meetInstances(t)
	ghost := "22222222-2222-4222-8222-222222222222"
	st := boardWith(t, "codex", ghost)
	before := transcriptLen(t, st)
	if _, err := PostAs(st.ID, "codex", ghost, "x"); err == nil || !strings.Contains(err.Error(), "bashy instance list") {
		t.Fatalf("unknown UUID with a matching roster seat was accepted: %v", err)
	}
	if transcriptLen(t, st) != before {
		t.Error("a refused post wrote to the transcript")
	}
	if err := routableSeat(ghost); err == nil {
		t.Error("an unknown UUID is routable")
	}
}

func TestPostAs_UnreadableStoreFailsClosed(t *testing.T) {
	meetInstances(t)
	bad := filepath.Join(t.TempDir(), "instances")
	if err := os.WriteFile(bad, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	bus.InstanceStoreFn = func() *fleet.InstanceStore { return fleet.NewInstanceStore(bad) }
	st := boardWith(t, "codex", "opencode")
	before := transcriptLen(t, st)
	// "opencode" is a genuine legacy seat, but validation could not run: no
	// compatibility fallback may turn that into an append.
	if _, err := PostAs(st.ID, "codex", "opencode", "x"); err == nil || !strings.Contains(err.Error(), "unreadable") {
		t.Fatalf("store failure did not fail closed: %v", err)
	}
	if transcriptLen(t, st) != before {
		t.Error("a refused post wrote to the transcript")
	}
}

func TestPostAs_GenuineLegacySeatStillWorks(t *testing.T) {
	meetInstances(t)
	st := boardWith(t, "codex", "opencode")
	ev, err := PostAs(st.ID, "codex", "opencode", "hi")
	if err != nil || ev.To != "opencode" || ev.ToParty != nil {
		t.Fatalf("legacy seat: %+v %v", ev, err)
	}
}

func TestPostAs_DefaultToHandleResolvesOnceToUUID(t *testing.T) {
	s := meetInstances(t)
	old := openInst(t, s, "Ada")
	st := newRoom(t)
	st.DefaultTo = "Ada"
	if err := st.save(); err != nil {
		t.Fatal(err)
	}
	ev, err := PostAs(st.ID, "qiangli", "", "queued for Ada")
	if err != nil {
		t.Fatal(err)
	}
	if ev.To != strings.ToLower(old.UUID) || ev.ToParty == nil || ev.ToParty.UUID != old.UUID {
		t.Fatalf("default addressee stored as %+v — must be the resolved UUID plus snapshot", ev)
	}
	stored, _ := readTranscript(st.ID)
	if got := stored[len(stored)-1]; got.To != strings.ToLower(old.UUID) || got.ToParty == nil {
		t.Errorf("persisted event = %+v", got)
	}

	// Reuse: the retired Ada's queued event stays with its UUID.
	if _, err := s.Retire(old.UUID, func(fleet.Instance) ([]string, error) { return []string{`{}`}, nil }); err != nil {
		t.Fatal(err)
	}
	fresh := openInst(t, s, "Ada")
	stored, _ = readTranscript(st.ID)
	if stored[len(stored)-1].To == strings.ToLower(fresh.UUID) {
		t.Fatal("queued mail followed the reused label")
	}
	// A new unaddressed post resolves the NEW occupant.
	ev, err = PostAs(st.ID, "qiangli", "", "second")
	if err != nil || ev.To != strings.ToLower(fresh.UUID) {
		t.Fatalf("second default post: %+v %v", ev, err)
	}
}

func TestPostAs_DefaultToRetiredOrInvalidFailsBeforeAppend(t *testing.T) {
	s := meetInstances(t)
	gone := openInst(t, s, "Cy")
	if _, err := s.Retire(gone.UUID, func(fleet.Instance) ([]string, error) { return []string{`{}`}, nil }); err != nil {
		t.Fatal(err)
	}
	for _, def := range []string{"Cy", "conductor:404"} {
		st := newRoom(t)
		st.DefaultTo = def
		if err := st.save(); err != nil {
			t.Fatal(err)
		}
		before := transcriptLen(t, st)
		if _, err := PostAs(st.ID, "qiangli", "", "x"); err == nil {
			t.Errorf("default %q accepted", def)
		}
		if transcriptLen(t, st) != before {
			t.Errorf("default %q wrote before failing", def)
		}
	}
}

func TestPostAs_BoardVacantRoleRetainedWithWarning(t *testing.T) {
	meetInstances(t)
	holder := ""
	prev := bus.HostRoles
	bus.HostRoles = func() []bus.HostRole {
		return []bus.HostRole{{Label: "deputy:sprint-1", Topic: "deputy.sprint-1", Holder: holder}}
	}
	t.Cleanup(func() { bus.HostRoles = prev })
	st := boardWith(t, "codex", "opencode")

	ev, err := PostAs(st.ID, "codex", "deputy:sprint-1", "pending for whoever holds it")
	if err != nil {
		t.Fatalf("vacant role mail must be retained, as mb send retains it: %v", err)
	}
	if ev.To != "deputy:sprint-1" || !strings.Contains(ev.Warning, "vacant") || !strings.Contains(ev.Warning, "nobody has read") {
		t.Fatalf("event = %+v", ev)
	}

	// Occupied by a NON-member: membership still refuses.
	holder = "stranger"
	before := transcriptLen(t, st)
	if _, err := PostAs(st.ID, "codex", "deputy:sprint-1", "x"); err == nil || !strings.Contains(err.Error(), "no seat") {
		t.Fatalf("non-member holder: %v", err)
	}
	if transcriptLen(t, st) != before {
		t.Error("refused post wrote")
	}
	// Handoff to a member: same stored address, now delivered to the holder.
	holder = "opencode"
	if _, err := PostAs(st.ID, "codex", "deputy:sprint-1", "now held"); err != nil {
		t.Fatal(err)
	}
	directed, _, _, _, err := UnreadRecords(st.ID, "opencode", 0)
	if err != nil || len(directed) != 2 {
		t.Fatalf("holder sees %d directed records (%v), want the retained one plus the new one", len(directed), err)
	}
	if other, _, _, _, _ := UnreadRecords(st.ID, "codex", 0); len(other) != 0 {
		t.Errorf("a non-holder was handed role mail: %+v", other)
	}
}

func TestPostAs_SenderSnapshotFromAmbientInstance(t *testing.T) {
	s := meetInstances(t)
	me := openInst(t, s, "Ada")
	t.Setenv("BASHY_INSTANCE", me.UUID)
	st := boardWith(t, "codex", "Ada")
	_ = st
	// Non-board room, speaking by the label the TUI passes.
	room := newRoom(t)
	ev, err := PostAs(room.ID, "Ada", "", "hello")
	if err != nil {
		t.Fatal(err)
	}
	if ev.FromParty == nil || ev.FromParty.UUID != me.UUID || ev.FromParty.Family != "esme" || len(ev.FromParty.Bindings) != 1 {
		t.Fatalf("from_party = %+v", ev.FromParty)
	}
	stored, _ := readTranscript(room.ID)
	if got := stored[len(stored)-1]; got.FromParty == nil || got.FromParty.UUID != me.UUID {
		t.Errorf("persisted from_party = %+v", got.FromParty)
	}
}

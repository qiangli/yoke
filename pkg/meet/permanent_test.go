package meet

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/qiangli/yoke/pkg/chat"
	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/fleet/fleettest"
)

func permanentTestStore(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("BASHY_MEET_DIR", dir)
	t.Setenv("BASHY_MEET_ROOMS_FILE", "")
	t.Setenv("USER", "tester")
	return dir
}

func TestConfiguredPermanentRoomsIncludeStewardByDefault(t *testing.T) {
	permanentTestStore(t)
	rooms, err := ConfiguredPermanentRooms()
	if err != nil {
		t.Fatal(err)
	}
	if len(rooms) != 1 || rooms[0].Name != "steward" || rooms[0].Topic != "Steward" {
		t.Fatalf("default rooms = %+v, want steward", rooms)
	}
	if rooms[0].Band != 4 || rooms[0].AutoStart == nil || !*rooms[0].AutoStart {
		t.Fatalf("default steward activation = %+v, want automatic L4", rooms[0])
	}
}

func TestAddressLazyStartsConfiguredPermanentSteward(t *testing.T) {
	dir := permanentTestStore(t)
	seatEverything(t)
	auto := true
	b, _ := json.Marshal(permanentRoomsFile{
		Schema: permanentRoomsSchema,
		Rooms: []PermanentRoomConfig{{
			Name: "steward", Topic: "Steward", Agent: "configured-agent", Band: 3, AutoStart: &auto,
		}},
	})
	if err := os.WriteFile(filepath.Join(dir, "rooms.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureConfiguredPermanentRooms(); err != nil {
		t.Fatal(err)
	}

	oldStarter, oldRunner := StartPermanentRole, apiRunner
	var got PermanentRoleStartRequest
	StartPermanentRole = func(_ context.Context, req PermanentRoleStartRequest) error {
		got = req
		_, err := EnsurePermanentRoleRoom(req.Room, req.Role, "configured-agent", CreateOptions{
			Participants: []string{"configured-agent"}, Out: OutStore,
		})
		return err
	}
	apiRunner = func() chat.Runner { return fakeRunner{reply: "invited"} }
	t.Cleanup(func() { StartPermanentRole, apiRunner = oldStarter, oldRunner })

	event, err := Address(context.Background(), "steward", "@steward", "invite Rufus")
	if err != nil {
		t.Fatal(err)
	}
	if got.Room != "steward" || got.Role != "steward" || got.Agent != "configured-agent" || got.Band != 3 {
		t.Fatalf("start request = %+v", got)
	}
	if event.Speaker != "configured-agent" {
		t.Fatalf("reply speaker = %q", event.Speaker)
	}
}

func TestAddressDoesNotAutoStartDisabledPermanentSteward(t *testing.T) {
	dir := permanentTestStore(t)
	auto := false
	b, _ := json.Marshal(permanentRoomsFile{
		Schema: permanentRoomsSchema,
		Rooms:  []PermanentRoomConfig{{Name: "steward", AutoStart: &auto}},
	})
	if err := os.WriteFile(filepath.Join(dir, "rooms.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureConfiguredPermanentRooms(); err != nil {
		t.Fatal(err)
	}
	called := false
	old := StartPermanentRole
	StartPermanentRole = func(context.Context, PermanentRoleStartRequest) error { called = true; return nil }
	t.Cleanup(func() { StartPermanentRole = old })
	if _, err := Address(context.Background(), "steward", "steward", "hello"); err == nil {
		t.Fatal("disabled steward auto-start succeeded")
	}
	if called {
		t.Fatal("disabled auto-start invoked host starter")
	}
}

func TestFirstRoomContributionActivatesOneFleetSecretary(t *testing.T) {
	permanentTestStore(t)
	fleettest.Ring(t)
	agents, _ := fleet.New().Agents()
	if len(agents) == 0 {
		t.Fatal("test ring has no agent for secretary test")
	}
	want := agents[0].Name
	old := StartRoomSecretary
	calls := 0
	StartRoomSecretary = func(_ context.Context, req RoomSecretaryStartRequest) (string, error) {
		calls++
		if req.Band != 2 || req.Room == "" {
			t.Fatalf("secretary request = %+v", req)
		}
		return want, nil
	}
	t.Cleanup(func() { StartRoomSecretary = old })
	st, err := Create(CreateOptions{Topic: "lazy secretary", Human: "tester", Out: OutStore})
	if err != nil {
		t.Fatal(err)
	}
	if !st.SecretaryPending || st.Secretary != "" {
		t.Fatalf("new room secretary state = %+v", st)
	}
	if _, err := Post(st.ID, "tester", "hello"); err != nil {
		t.Fatal(err)
	}
	if _, err := Post(st.ID, "tester", "again"); err != nil {
		t.Fatal(err)
	}
	fresh, _, err := Room(st.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Secretary != want || fresh.SecretaryPending || calls != 1 {
		t.Fatalf("activated secretary = %q pending=%v calls=%d", fresh.Secretary, fresh.SecretaryPending, calls)
	}
}

func TestConfiguredPermanentRoomsMergeHostConfig(t *testing.T) {
	dir := permanentTestStore(t)
	p := filepath.Join(dir, "rooms.json")
	b, _ := json.Marshal(permanentRoomsFile{
		Schema: permanentRoomsSchema,
		Rooms: []PermanentRoomConfig{
			{Name: "steward", Topic: "Dragon steward"},
			{Name: "release", Topic: "Release room", Agenda: []string{"release gate"}},
		},
	})
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	rooms, err := ConfiguredPermanentRooms()
	if err != nil {
		t.Fatal(err)
	}
	if len(rooms) != 2 || rooms[0].Name != "release" || rooms[1].Topic != "Dragon steward" {
		t.Fatalf("merged rooms = %+v", rooms)
	}
}

func TestEnsurePermanentRoomIsStableNamedAndCanCloseAndReopen(t *testing.T) {
	permanentTestStore(t)
	first, err := EnsurePermanentRoom("steward", CreateOptions{Topic: "Steward", Out: OutStore})
	if err != nil {
		t.Fatal(err)
	}
	second, err := EnsurePermanentRoom("@steward", CreateOptions{Topic: "Host steward", Out: OutStore})
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Fatalf("ensure created two identities: %s and %s", first.ID, second.ID)
	}
	if !second.Permanent || second.Name != "steward" || second.Topic != "Host steward" {
		t.Fatalf("permanent state = %+v", second)
	}
	for _, ref := range []string{"steward", "@steward"} {
		got, _, err := Room(ref)
		if err != nil || got.ID != first.ID {
			t.Fatalf("Room(%q) = %+v, %v", ref, got, err)
		}
	}
	if _, err := record(second, "note", "tester", "human", "first session evidence"); err != nil {
		t.Fatal(err)
	}
	if err := Close("steward", "tester"); err != nil {
		t.Fatalf("Close(steward) = %v", err)
	}
	closed, _, err := Room("steward")
	if err != nil || closed.Status != "closed" || closed.Room != 0 {
		t.Fatalf("permanent room did not close and release its number: %+v, %v", closed, err)
	}
	reopened, err := Open("steward", "tester")
	if err != nil {
		t.Fatal(err)
	}
	if reopened.ID != first.ID || reopened.Status != "open" || reopened.Room < 1 {
		t.Fatalf("reopened permanent room = %+v", reopened)
	}
	if events, err := readTranscript(reopened.ID); err != nil || len(events) != 0 {
		t.Fatalf("reopened permanent room reused its prior transcript: %+v, %v", events, err)
	}
	dir, _ := storeDir(reopened.ID)
	archives, err := filepath.Glob(filepath.Join(dir, "archive", "*", "transcript.jsonl"))
	if err != nil || len(archives) != 1 {
		t.Fatalf("archived transcript = %v, %v", archives, err)
	}
}

func TestEnsureConfiguredPermanentRoomsMaterializesAllOnce(t *testing.T) {
	dir := permanentTestStore(t)
	b, _ := json.Marshal(permanentRoomsFile{
		Schema: permanentRoomsSchema,
		Rooms:  []PermanentRoomConfig{{Name: "operations", Topic: "Operations"}},
	})
	if err := os.WriteFile(filepath.Join(dir, "rooms.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if rooms, err := EnsureConfiguredPermanentRooms(); err != nil || len(rooms) != 2 {
			t.Fatalf("ensure %d = %+v, %v", i, rooms, err)
		}
	}
	sessions, err := listSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 {
		t.Fatalf("sessions = %d, want one identity per configured room", len(sessions))
	}
}

func TestPermanentStewardAliasRoutesAndAuthorizesCurrentHolder(t *testing.T) {
	permanentTestStore(t)
	seatEverything(t)
	st, err := EnsurePermanentRoleRoom("steward", "steward", "codex", CreateOptions{
		Topic: "Steward", Participants: []string{"codex"}, Initiator: "codex", Out: OutStore,
	})
	if err != nil {
		t.Fatal(err)
	}
	if st.RoleHolders["steward"] != "codex" {
		t.Fatalf("role holders = %v", st.RoleHolders)
	}

	oldRunner := apiRunner
	apiRunner = func() chat.Runner { return fakeRunner{reply: "done"} }
	t.Cleanup(func() { apiRunner = oldRunner })
	event, err := Address(context.Background(), "steward", "steward", "invite the release agent")
	if err != nil {
		t.Fatal(err)
	}
	if event.Speaker != "codex" {
		t.Fatalf("@steward routed to %q, want codex", event.Speaker)
	}
	if err := Invite("steward", "codex", "release-agent"); err != nil {
		t.Fatalf("current steward could not invite: %v", err)
	}
	if err := Invite("steward", "intruder", "other-agent"); !errors.Is(err, ErrNotOrganizer) {
		t.Fatalf("intruder invite = %v, want ErrNotOrganizer", err)
	}

	if err := ClearPermanentRoleHolder("steward", "steward", "predecessor"); err != nil {
		t.Fatal(err)
	}
	still, _, _ := Room("steward")
	if still.RoleHolders["steward"] != "codex" {
		t.Fatal("a stale predecessor cleared the current holder")
	}
	if err := ClearPermanentRoleHolder("steward", "steward", "codex"); err != nil {
		t.Fatal(err)
	}
	if _, err := Address(context.Background(), "steward", "steward", "hello"); err == nil {
		t.Fatal("@steward remained routable after its holder released the seat")
	}
}

func TestPermanentRoomConfigFailsClosed(t *testing.T) {
	dir := permanentTestStore(t)
	if err := os.WriteFile(filepath.Join(dir, "rooms.json"), []byte(`{"schema":"wrong"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureConfiguredPermanentRooms(); err == nil {
		t.Fatal("wrong schema was accepted")
	}
}

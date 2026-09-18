package meet

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/qiangli/yoke/pkg/coopauth"
	"github.com/qiangli/yoke/pkg/fleet"
)

// The HTTP + WebSocket surface. Every test here drives the real mux through
// httptest — no network, no agent CLIs, and $BASHY_MEET_DIR pointed at a temp
// dir by newRoom/newTestSession, so a developer's own meetings are never touched.
//
// The routes that run agents (round, poll, ask, address, converge) are exercised
// only as far as their 202/409 contract: actually running one would spawn a real
// CLI, which is the one thing a hermetic suite must never do.

// serveTest starts the surface over whatever room store the caller has already
// isolated.
func serveTest(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(newServeHandler(context.Background(), MountOptions{}))
	t.Cleanup(srv.Close)
	return srv
}

func doJSON(t *testing.T, srv *httptest.Server, method, path string, body any) (*http.Response, []byte) {
	t.Helper()
	var rdr *strings.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rdr = strings.NewReader(string(b))
	} else {
		rdr = strings.NewReader("")
	}
	req, err := http.NewRequest(method, srv.URL+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			break
		}
	}
	return resp, buf
}

// apiError pulls the {"error":"…"} the contract says every failure carries.
func apiErrorOf(t *testing.T, body []byte) string {
	t.Helper()
	var e struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("an error response must be {\"error\":…}, got %q", body)
	}
	if e.Error == "" {
		t.Fatalf("empty error message in %q", body)
	}
	return e.Error
}

func TestAPIRoomList(t *testing.T) {
	st := newRoom(t)
	// History can reuse both the active room's door and topic. Relay's channel
	// navigation must not render those terminal sessions as duplicate entries.
	closed := saveMeeting(t, "closed-copy", st.Room, "closed")
	closed.Topic = st.Topic
	if err := closed.save(); err != nil {
		t.Fatal(err)
	}
	abandoned := saveMeeting(t, "abandoned-copy", st.Room, "abandoned")
	abandoned.Topic = st.Topic
	if err := abandoned.save(); err != nil {
		t.Fatal(err)
	}
	srv := serveTest(t)

	resp, body := doJSON(t, srv, "GET", "/api/rooms", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body %s", resp.StatusCode, body)
	}
	var rooms []RoomSummary
	if err := json.Unmarshal(body, &rooms); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if len(rooms) != 1 {
		t.Fatalf("rooms = %+v, want the one room on this host", rooms)
	}
	got := rooms[0]
	if got.ID != st.ID || got.Topic != st.Topic || got.Status != "open" {
		t.Errorf("summary = %+v, want id/topic/status of %+v", got, st)
	}
	// Members is the room's people, not just the agent seats: a chat room's
	// member list without the human in it is missing whoever is typing.
	if !contains(got.Members, "codex") || !contains(got.Members, "qiangli") {
		t.Errorf("members = %v, want the participant and the human", got.Members)
	}

	// And the room is reachable by every reference resolveMeeting accepts, which
	// is the point of routing :ref through it rather than through a second
	// spelling of the lookup.
	for _, ref := range []string{st.ID, st.ID[:12], "1"} {
		resp, body := doJSON(t, srv, "GET", "/api/rooms/"+url.PathEscape(ref), nil)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET by %q = %d, body %s", ref, resp.StatusCode, body)
			continue
		}
		var got struct {
			State     *State     `json:"state"`
			Synthesis *Synthesis `json:"synthesis"`
		}
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("decode: %v (%s)", err, body)
		}
		if got.State == nil || got.State.ID != st.ID {
			t.Errorf("GET by %q returned %+v", ref, got.State)
		}
		if got.Synthesis != nil {
			t.Errorf("a room with no secretary has no synthesis: %+v", got.Synthesis)
		}
	}
}

func TestAPIAgentListIsTheRegisteredFleet(t *testing.T) {
	pinFleet(t)
	old := operableFn
	operableFn = func(tool string) (bool, string) { return tool != "", "test availability" }
	t.Cleanup(func() { operableFn = old })
	srv := serveTest(t)

	resp, body := doJSON(t, srv, "GET", "/api/agents", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body %s", resp.StatusCode, body)
	}
	var agents []AgentOption
	if err := json.Unmarshal(body, &agents); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if len(agents) == 0 {
		t.Fatal("the shipped registered fleet must be visible to the room picker")
	}
	for _, agent := range agents {
		if _, ok := fleet.New().Agent(agent.Name); !ok {
			t.Fatalf("API leaked an unregistered identity: %+v", agent)
		}
	}
}

// A post is the chat primitive: it lands in the transcript, takes no lease, and
// reads back identically to one the CLI made.
func TestAPIPost(t *testing.T) {
	st := newRoom(t)
	srv := serveTest(t)

	resp, body := doJSON(t, srv, "POST", "/api/rooms/"+st.ID+"/post",
		map[string]string{"author": "qiangli", "text": "morning"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body %s", resp.StatusCode, body)
	}
	var ev Event
	if err := json.Unmarshal(body, &ev); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if ev.Kind != "human" || ev.Speaker != "qiangli" || ev.Text != "morning" {
		t.Errorf("event = %+v", ev)
	}

	events, err := readTranscript(st.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Text != "morning" {
		t.Fatalf("transcript = %+v", events)
	}

	// An empty message is not a contribution, and saying so is a 400 rather than
	// a recorded blank the room has to render.
	resp, body = doJSON(t, srv, "POST", "/api/rooms/"+st.ID+"/post",
		map[string]string{"author": "qiangli", "text": "   "})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty post status = %d, body %s", resp.StatusCode, body)
	}
	apiErrorOf(t, body)
}

// A held lease answers 409 ON THE REQUEST. The long verbs return 202 and run in a
// goroutine, so if the lease were only taken in there, a busy room would answer
// 202 and then fail where the caller is not looking. See startJob.
func TestAPILongVerbIsBusyWhileTheLeaseIsHeld(t *testing.T) {
	st := newRoom(t)
	srv := serveTest(t)

	lease, err := acquireRunLease(st.ID)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	for _, verb := range []string{"round", "converge"} {
		resp, body := doJSON(t, srv, "POST", "/api/rooms/"+st.ID+"/"+verb, nil)
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("POST /%s while the lease is held = %d, want 409 (body %s)", verb, resp.StatusCode, body)
		}
		if msg := apiErrorOf(t, body); !strings.Contains(msg, "already being run") {
			t.Errorf("the 409 must name the reason: %q", msg)
		}
	}

	// A post is deliberately NOT lease-guarded — that is what lets a room feel
	// like chat while an agent holds the floor.
	resp, body := doJSON(t, srv, "POST", "/api/rooms/"+st.ID+"/post",
		map[string]string{"text": "still typing"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a post must not need the lease: %d %s", resp.StatusCode, body)
	}

	lease.Release()
}

// Any member may post; only the organizer changes the roster. Over HTTP that is a
// 403, classified from the sentinel rather than from the message text.
func TestAPINonOrganizerInviteIsForbidden(t *testing.T) {
	seatEverything(t)
	st := newRoom(t) // convened by qiangli
	srv := serveTest(t)

	resp, body := doJSON(t, srv, "POST", "/api/rooms/"+st.ID+"/invite",
		map[string]string{"agent": "opencode"})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("the organizer's invite = %d, body %s", resp.StatusCode, body)
	}
	reloaded, _ := loadState(st.ID)
	if len(reloaded.Participants) != 2 {
		t.Fatalf("participants = %v", reloaded.Participants)
	}

	// The sentinel the 403 is classified from — matched with errors.Is, never on
	// the prose, which is written for humans and will be reworded.
	if err := Invite(st.ID, "codex", "claude-opus5"); !errors.Is(err, ErrNotOrganizer) {
		t.Fatalf("a member's invite must be ErrNotOrganizer, got %v", err)
	}

	// Over HTTP: a room convened by somebody else refuses this host's human. The
	// ungated loopback caller is the machine OWNER, which is not the same thing as
	// being every room's organizer.
	other := newRoomOwnedBy(t, "someone-else")
	resp3, body3 := doJSON(t, srv, "POST", "/api/rooms/"+other.ID+"/invite",
		map[string]string{"agent": "opencode"})
	if resp3.StatusCode != http.StatusForbidden {
		t.Fatalf("a non-organizer invite = %d, want 403 (body %s)", resp3.StatusCode, body3)
	}
	if msg := apiErrorOf(t, body3); !strings.Contains(msg, "organizer") {
		t.Errorf("the 403 must say why: %q", msg)
	}
	after, _ := loadState(other.ID)
	if len(after.Participants) != 1 {
		t.Errorf("a refused invite must not mutate the roster: %v", after.Participants)
	}
}

// newRoomOwnedBy opens a second room in the SAME store, convened by somebody who
// is not this host's human.
func newRoomOwnedBy(t *testing.T, organizer string) *State {
	t.Helper()
	st := &State{
		ID: newID("Someone else's room", fixedNow()), Topic: "Someone else's room",
		Participants: []string{"codex"}, Human: organizer, Initiator: organizer,
		Status: "open", Cwd: t.TempDir(), Created: fixedNow(),
	}
	if err := st.save(); err != nil {
		t.Fatal(err)
	}
	return st
}

// An unknown reference is a 404 on every route that takes one — including the
// roster routes, whose verbs live in roster.go and know nothing about HTTP.
func TestAPIUnknownRoomIs404(t *testing.T) {
	newRoom(t) // a store with exactly one room in it
	srv := serveTest(t)

	cases := []struct {
		method, path string
		body         any
	}{
		{"GET", "/api/rooms/nope-not-a-room", nil},
		{"POST", "/api/rooms/nope-not-a-room/post", map[string]string{"text": "hi"}},
		{"POST", "/api/rooms/nope-not-a-room/invite", map[string]string{"agent": "codex"}},
		{"POST", "/api/rooms/nope-not-a-room/kick", map[string]string{"agent": "codex"}},
		{"POST", "/api/rooms/nope-not-a-room/mark", map[string]string{"kind": "note", "text": "hi"}},
		{"POST", "/api/rooms/nope-not-a-room/round", nil},
		{"POST", "/api/rooms/nope-not-a-room/close", nil},
		{"POST", "/api/rooms/99/round", nil}, // a room number nobody is in
	}
	for _, c := range cases {
		resp, body := doJSON(t, srv, c.method, c.path, c.body)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 404 (body %s)", c.method, c.path, resp.StatusCode, body)
			continue
		}
		apiErrorOf(t, body)
	}
}

// The marker routes write the same events the REPL's /decision and /agenda do.
func TestAPIMark(t *testing.T) {
	st := newRoom(t)
	srv := serveTest(t)

	resp, body := doJSON(t, srv, "POST", "/api/rooms/"+st.ID+"/mark",
		map[string]string{"kind": "decision", "text": "ship it"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body %s", resp.StatusCode, body)
	}
	resp, body = doJSON(t, srv, "POST", "/api/rooms/"+st.ID+"/mark",
		map[string]string{"kind": "agenda", "text": "the cache question"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body %s", resp.StatusCode, body)
	}
	// An agenda item is header state too — currentAgenda reads it to decide what
	// the next round is about, so recording the event alone would be half the act.
	reloaded, _ := loadState(st.ID)
	if len(reloaded.Agenda) != 1 || reloaded.Agenda[0] != "the cache question" {
		t.Errorf("agenda = %v", reloaded.Agenda)
	}

	// An invented kind is refused: kind is what every reader switches on, so an
	// unknown one would be recorded and then displayed by nothing.
	resp, body = doJSON(t, srv, "POST", "/api/rooms/"+st.ID+"/mark",
		map[string]string{"kind": "proclamation", "text": "hear ye"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("an unknown marker kind = %d, body %s", resp.StatusCode, body)
	}

	events, _ := readTranscript(st.ID)
	if countKind(events, "decision") != 1 || countKind(events, "agenda") != 1 {
		t.Errorf("transcript = %+v", events)
	}
}

// The room closes over HTTP, and the organizer check applies — a close is an
// organizer's act, like a roster change.
func TestAPIClose(t *testing.T) {
	st := newRoom(t)
	srv := serveTest(t)

	other := newRoomOwnedBy(t, "someone-else")
	resp, body := doJSON(t, srv, "POST", "/api/rooms/"+other.ID+"/close", nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a non-organizer close = %d, want 403 (body %s)", resp.StatusCode, body)
	}

	resp, body = doJSON(t, srv, "POST", "/api/rooms/"+st.ID+"/close", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("close = %d, body %s", resp.StatusCode, body)
	}
	reloaded, _ := loadState(st.ID)
	if reloaded.Status != "closed" {
		t.Errorf("status = %q, want closed", reloaded.Status)
	}
	// Yes:true is recorded, not silent — the confirm event is what says the room
	// was concluded deliberately rather than abandoned.
	events, _ := readTranscript(st.ID)
	if countKind(events, "confirm") != 1 {
		t.Errorf("want one confirm event: %+v", events)
	}
}

func TestAPIRoomCreate(t *testing.T) {
	newRoom(t) // isolate the store
	pinFleet(t)
	srv := serveTest(t)

	resp, body := doJSON(t, srv, "POST", "/api/rooms", map[string]any{
		"Topic": "Plan together", "Owner": "codex", "Participants": []string{"claude-fable5"},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body %s", resp.StatusCode, body)
	}
	var st State
	if err := json.Unmarshal(body, &st); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	// The browser must preserve the facilitator the human selected and must not
	// silently acquire a note-taker.
	if st.recorded() || st.Chair != "codex" {
		t.Errorf("a created meeting needs its explicit owner and no secretary: %+v", st)
	}
	if st.Initiator == "" {
		t.Error("a room opened through the API must name its organizer, or the privilege check is disabled forever")
	}
	if _, err := loadState(st.ID); err != nil {
		t.Fatalf("the created room must be on disk: %v", err)
	}

	// A room with no topic is refused by Validate, not by a second check here.
	resp, body = doJSON(t, srv, "POST", "/api/rooms", map[string]any{
		"Owner": "codex", "Participants": []string{"claude-fable5"},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a topicless room = %d, body %s", resp.StatusCode, body)
	}
}

// doVouchedJSON is doJSON arriving THROUGH THE TUNNEL as a named cloud account —
// the outpost wire contract: X-Forwarded-Prefix marks it cloud-arrived, Remote-User
// carries the identity cloudbox vouched for. With no BASHY_MEET_SSO_SECRET set
// there is no HMAC to satisfy, which is the same shape a paired host without a
// configured secret presents.
func doVouchedJSON(t *testing.T, srv *httptest.Server, user, method, path string, body any) (*http.Response, []byte) {
	t.Helper()
	var rdr *strings.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rdr = strings.NewReader(string(b))
	} else {
		rdr = strings.NewReader("")
	}
	req, err := http.NewRequest(method, srv.URL+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(coopauth.HdrForwardedPrefix, "/matrix/h/box/app/meet")
	req.Header.Set(coopauth.HdrRemoteUser, user)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, buf
}

// The reported defect: the FIRST thing a non-technical user does through Tessaro —
// create a room — failed, because the initiator resolved to this host's OS user
// while the caller was a cloudbox email, and Validate refused an initiator who was
// not at the table.
//
// Nothing was wrong with that check. It was being applied at the one moment there
// is no table to check against. The caller is now SEATED as the room's human, so
// it passes for the honest reason rather than by being waived.
func TestAPIRoomCreateSeatsTheCloudIdentity(t *testing.T) {
	// Create stamps the SERVER's cwd onto the room, and the close below files the
	// minutes relative to it. Without this the suite writes into the repo's docs/.
	t.Chdir(t.TempDir())
	newRoom(t) // isolate the store; its human is the OS user "qiangli"
	pinFleet(t)
	srv := serveTest(t)

	const cloudUser = "qiangli@example.com"
	resp, body := doVouchedJSON(t, srv, cloudUser, "POST", "/api/rooms",
		map[string]any{"Topic": "tunnel verification", "Owner": "codex", "Participants": []string{"claude-fable5"}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("creating a room as a cloud identity = %d, body %s", resp.StatusCode, body)
	}
	var st State
	if err := json.Unmarshal(body, &st); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	// The identity that called is the identity recorded — still NOT an OS-shaped
	// approximation. What changed is WHICH form of it: the collision-free handle
	// derived from the verified email, which is what the rest of the console
	// signs with (pkg/webconsole userOf -> bus.BoardIdentity).
	//
	// Recording the raw email here instead made one cloud human TWO principals on
	// a single page load — the handle in Messages and inbox, the email in this
	// room — which fragments their mail and breaks attribution. The email remains
	// on Identity.User/Email for audit; addressing keys on the handle because
	// that is what bus and inbox key on.
	want := coopauth.Username(cloudUser)
	if st.Human != want {
		t.Errorf("human = %q, want the caller's canonical handle %q", st.Human, want)
	}
	if st.Initiator != want {
		t.Errorf("initiator = %q, want the caller's canonical handle %q", st.Initiator, want)
	}
	// Derived, never stored: an initiator who IS the human is a human.
	if st.initiatorKind() != "human" {
		t.Errorf("initiatorKind = %q, want human", st.initiatorKind())
	}

	// And it is durable — the check that matters is what a LATER request loads,
	// since requireOrganizer has nothing to compare against but the recorded name.
	saved, err := loadState(st.ID)
	if err != nil {
		t.Fatalf("the created room must be on disk: %v", err)
	}
	if saved.Human != want || saved.Initiator != want {
		t.Fatalf("saved header = human %q initiator %q, want %q", saved.Human, saved.Initiator, want)
	}

	// The whole point of recording it: the creator can now act on their own room.
	if resp, body := doVouchedJSON(t, srv, cloudUser, "POST", "/api/rooms/"+st.ID+"/close", nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("the creator's close = %d, want 204 (body %s)", resp.StatusCode, body)
	}
}

// A vouched-but-unnamed caller opens nothing. Falling back to this host's user
// would file the room under the machine owner and hand THEM the organizer
// privilege — a room the actual caller could never close.
func TestAPIRoomCreateRefusesAnUnnamedVouch(t *testing.T) {
	newRoom(t)
	srv := serveTest(t)

	req, err := http.NewRequest("POST", srv.URL+"/api/rooms", strings.NewReader(`{"Topic":"who am i"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(coopauth.HdrForwardedPrefix, "/matrix/h/box/app/meet")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	// The gate refuses it before the handler is even reached — an unauthenticated
	// cloud arrival is not a caller. Either way it must not open a room.
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("an unnamed cloud arrival = %d, want 403", resp.StatusCode)
	}
	rooms, _ := Rooms()
	if len(rooms) != 1 {
		t.Errorf("a refused create must open no room: %+v", rooms)
	}
}

// The loopback control from the defect report: no cloud identity, so the room is
// this host's — the local-first path must be untouched by the fix.
func TestAPIRoomCreateOnLoopbackIsTheOSUser(t *testing.T) {
	newRoom(t) // sets $USER=qiangli
	pinFleet(t)
	srv := serveTest(t)

	resp, body := doJSON(t, srv, "POST", "/api/rooms", map[string]any{
		"Topic": "loopback control", "Owner": "codex", "Participants": []string{"claude-fable5"},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body %s", resp.StatusCode, body)
	}
	var st State
	if err := json.Unmarshal(body, &st); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if st.Human != "qiangli" || st.Initiator != "qiangli" {
		t.Errorf("human = %q initiator = %q, want the OS user", st.Human, st.Initiator)
	}
	if st.Status != "open" {
		t.Errorf("status = %q", st.Status)
	}
}

// #155's organizer check is unchanged for every verb that acts on an EXISTING
// room. Seating the creator gave the check something true to compare against; it
// did not relax it, and a second cloud account is still a stranger to the room.
func TestAPIOrganizerCheckStillRefusesAnotherCloudIdentity(t *testing.T) {
	seatEverything(t)
	newRoom(t)
	pinFleet(t)
	srv := serveTest(t)

	const owner, intruder = "owner@example.com", "intruder@example.com"
	resp, body := doVouchedJSON(t, srv, owner, "POST", "/api/rooms",
		map[string]any{"Topic": "whose room is this", "Owner": "codex", "Participants": []string{"claude-fable5"}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create = %d, body %s", resp.StatusCode, body)
	}
	var st State
	if err := json.Unmarshal(body, &st); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}

	// Every organizer-gated verb, refused for the intruder — and the body cannot
	// buy the privilege back, because a vouched caller's stated actor is ignored.
	for _, c := range []struct {
		verb string
		body any
	}{
		{"invite", map[string]string{"agent": "opencode", "actor": owner}},
		{"kick", map[string]string{"agent": "claude-fable5", "actor": owner}},
		{"close", map[string]string{"actor": owner}},
	} {
		resp, body := doVouchedJSON(t, srv, intruder, "POST", "/api/rooms/"+st.ID+"/"+c.verb, c.body)
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s by a non-organizer = %d, want 403 (body %s)", c.verb, resp.StatusCode, body)
			continue
		}
		if msg := apiErrorOf(t, body); !strings.Contains(msg, "organizer") {
			t.Errorf("the %s 403 must say why: %q", c.verb, msg)
		}
	}
	// Nothing moved.
	after, _ := loadState(st.ID)
	if len(after.Participants) != 1 || after.Participants[0] != "claude-fable5" || after.Status != "open" {
		t.Fatalf("a refused act must not mutate the room: %+v", after)
	}

	// The organizer's own invite still lands, so the 403s above are the check
	// working rather than the route being broken.
	if resp, body := doVouchedJSON(t, srv, owner, "POST", "/api/rooms/"+st.ID+"/invite",
		map[string]string{"agent": "opencode"}); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("the organizer's invite = %d, body %s", resp.StatusCode, body)
	}
}

// --- The WebSocket -----------------------------------------------------------

// The info frame arrives FIRST, before the backlog. Without it a client has a
// stream of turns and no idea what room it is in — no topic, no roster, no
// status, no round.
func TestObserveWSSendsInfoFirst(t *testing.T) {
	st := newRoom(t)
	if _, err := record(st, "human", st.Human, "human", "morning"); err != nil {
		t.Fatal(err)
	}
	srv := serveTest(t)

	conn := dialObserve(t, srv, st.ID)
	defer conn.Close()

	first := readFrame(t, conn)
	if first.Kind != "info" {
		t.Fatalf("first frame kind = %q, want info (note %q)", first.Kind, first.Note)
	}
	var got State
	if err := json.Unmarshal(first.Data, &got); err != nil {
		t.Fatalf("the info frame must carry a State: %v (%s)", err, first.Data)
	}
	if got.ID != st.ID || got.Topic != st.Topic || got.Status != "open" {
		t.Errorf("info = %+v, want the header of %s", got, st.ID)
	}
	if len(got.Participants) != 1 || got.Participants[0] != "codex" {
		t.Errorf("the info frame must carry the roster: %v", got.Participants)
	}

	// Then the backlog, then the marker — the order observe prints in.
	second := readFrame(t, conn)
	if second.Kind != "event" {
		t.Fatalf("second frame kind = %q, want the backlog event", second.Kind)
	}
	third := readFrame(t, conn)
	if third.Kind != "history-end" {
		t.Fatalf("third frame kind = %q, want history-end", third.Kind)
	}
}

// The stream ends when the room does. It used to run forever, so a browser tab
// left open on a finished meeting pinned a goroutine and two file tails until the
// process died.
func TestObserveWSStopsWhenTheRoomCloses(t *testing.T) {
	st := newRoom(t)
	srv := serveTest(t)

	conn := dialObserve(t, srv, st.ID)
	defer conn.Close()
	if f := readFrame(t, conn); f.Kind != "info" {
		t.Fatalf("first frame = %q", f.Kind)
	}
	if f := readFrame(t, conn); f.Kind != "history-end" {
		t.Fatalf("second frame = %q", f.Kind)
	}

	// The last word is written BEFORE the status flips, which is exactly the
	// ordering a stop-on-the-flag implementation truncates.
	if _, err := record(st, "decision", st.Human, "", "we ship on friday"); err != nil {
		t.Fatal(err)
	}
	st.Status = "closed"
	if err := st.save(); err != nil {
		t.Fatal(err)
	}

	var sawDecision, sawClosedInfo bool
	deadline := time.Now().Add(10 * time.Second)
	_ = conn.SetReadDeadline(deadline)
	for {
		var f rawFrame
		if err := conn.ReadJSON(&f); err != nil {
			break // the server hung up, which is the point of the test
		}
		switch f.Kind {
		case "event":
			var e Event
			if json.Unmarshal(f.Data, &e) == nil && e.Text == "we ship on friday" {
				sawDecision = true
			}
		case "info":
			var s State
			if json.Unmarshal(f.Data, &s) == nil && s.Status == "closed" {
				sawClosedInfo = true
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("the socket did not terminate when the room closed")
		}
	}
	if !sawDecision {
		t.Error("the closing turn must be drained before the socket stops — that is the word that says what was decided")
	}
	if !sawClosedInfo {
		t.Error("a client must be told the room closed, not left to infer it from a hangup")
	}
}

// CheckOrigin used to return true unconditionally, so any page on the web could
// open this socket on a visitor's behalf and read a whole transcript.
func TestObserveWSOriginPolicy(t *testing.T) {
	cases := []struct {
		name, origin, forwardedHost, host string
		want                              bool
	}{
		{"no origin at all is not a browser", "", "", "127.0.0.1:8637", true},
		{"loopback by ip", "http://127.0.0.1:5173", "", "127.0.0.1:8637", true},
		{"loopback by name", "http://localhost:5173", "", "127.0.0.1:8637", true},
		{"ipv6 loopback", "http://[::1]:5173", "", "127.0.0.1:8637", true},
		{"same host as the request", "https://box.example", "", "box.example", true},
		{"the tunnelled host", "https://cloud.example", "cloud.example", "127.0.0.1:8637", true},
		{"a proxy chain names the browser's host first", "https://cloud.example", "cloud.example, inner.local", "127.0.0.1:8637", true},
		{"a stranger", "https://evil.example", "cloud.example", "127.0.0.1:8637", false},
		{"a lookalike suffix", "https://evilcloud.example", "cloud.example", "127.0.0.1:8637", false},
		{"unparseable", "://", "", "127.0.0.1:8637", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "http://"+c.host+"/observe?room=1", nil)
			r.Host = c.host
			if c.origin != "" {
				r.Header.Set("Origin", c.origin)
			}
			if c.forwardedHost != "" {
				r.Header.Set("X-Forwarded-Host", c.forwardedHost)
			}
			if got := originAllowed(r); got != c.want {
				t.Errorf("originAllowed(%q via %q) = %v, want %v", c.origin, c.forwardedHost, got, c.want)
			}
		})
	}
}

// The two entrances: ungated on loopback (the machine owner, and the whole
// local-first development path), vouched through outpost, refused otherwise.
//
// The refused case is the one that matters — a naive "skip the gate when there is
// no X-Forwarded-Prefix" waves through exactly the caller this must stop.
func TestGateEntrances(t *testing.T) {
	guard := serveGuard()
	var reached bool
	h := gate(guard, func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	})

	cases := []struct {
		name       string
		remoteAddr string
		headers    map[string]string
		want       int
	}{
		{"loopback owner", "127.0.0.1:51234", nil, http.StatusOK},
		{"ipv6 loopback owner", "[::1]:51234", nil, http.StatusOK},
		{"off-host with no vouch", "10.0.0.7:51234", nil, http.StatusForbidden},
		{"cloud-arrived with no identity", "127.0.0.1:51234",
			map[string]string{"X-Forwarded-Prefix": "/matrix/h/box/app/meet"}, http.StatusForbidden},
		{"cloud-arrived and vouched", "127.0.0.1:51234", map[string]string{
			"X-Forwarded-Prefix": "/matrix/h/box/app/meet",
			"Remote-User":        "someone@example.com",
		}, http.StatusOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reached = false
			r := httptest.NewRequest("GET", "/api/rooms", nil)
			r.RemoteAddr = c.remoteAddr
			for k, v := range c.headers {
				r.Header.Set(k, v)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != c.want {
				t.Fatalf("status = %d, want %d (body %s)", w.Code, c.want, w.Body)
			}
			if reached != (c.want == http.StatusOK) {
				t.Errorf("handler reached = %v for a %d", reached, w.Code)
			}
		})
	}
}

// The actor is named by the REQUEST, never by the body — on every path, not
// just the vouched one. /meet/ is a panel of the same console whose message
// board already refuses a body-supplied sender, so a page that could sign one
// way in Messages and another way here would let one human be two principals.
//
// The vouched name is the collision-free Username, exactly as
// pkg/webconsole/api.go userOf derives it — not the raw email, which is what
// made one cloud human two names on a single page load.
func TestActorOfIsDerivedFromTheRequestNotTheBody(t *testing.T) {
	t.Setenv("USER", "owner")

	// Ungated loopback: the host's human, and no way to state somebody else.
	plain := httptest.NewRequest("POST", "/api/rooms/1/invite", nil)
	if got := actorOf(plain); got != "owner" {
		t.Errorf("actorOf(loopback) = %q, want the host's human", got)
	}

	// Vouched: the verified identity, canonicalized the same way the console
	// canonicalizes it.
	var seen string
	gate(serveGuard(), func(w http.ResponseWriter, r *http.Request) {
		seen = actorOf(r)
	}).ServeHTTP(httptest.NewRecorder(), func() *http.Request {
		r := httptest.NewRequest("POST", "/api/rooms/1/invite", nil)
		r.RemoteAddr = "127.0.0.1:51234"
		r.Header.Set("X-Forwarded-Prefix", "/app/meet")
		r.Header.Set("Remote-User", "vouched@example.com")
		return r
	}())
	if want := coopauth.Username("vouched@example.com"); seen != want {
		t.Errorf("actorOf(vouched) = %q, want %q — the same handle the console signs with", seen, want)
	}
}

// rawFrame is wsFrame with the payload left undecoded, so a test can assert on
// `kind` before choosing how to read `data`.
type rawFrame struct {
	Kind string          `json:"kind"`
	Data json.RawMessage `json:"data"`
	Note string          `json:"note"`
}

func dialObserve(t *testing.T, srv *httptest.Server, ref string) *websocket.Conn {
	t.Helper()
	u := "ws" + strings.TrimPrefix(srv.URL, "http") + "/observe?room=" + url.QueryEscape(ref)
	conn, resp, err := websocket.DefaultDialer.Dial(u, nil)
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("dial %s: %v (status %d)", u, err, status)
	}
	return conn
}

func readFrame(t *testing.T, conn *websocket.Conn) rawFrame {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	var f rawFrame
	if err := conn.ReadJSON(&f); err != nil {
		t.Fatalf("read frame: %v", err)
	}
	return f
}

// --- The SPA -----------------------------------------------------------------

// The trailing slash in <base href> is the whole mechanism by which a prefixed
// mount resolves its assets. Without it `src="assets/x.js"` resolves against the
// PARENT of the mount and 404s.
func TestInjectBaseHref(t *testing.T) {
	const doc = `<!doctype html><html><head><meta charset="utf-8"><title>meet</title></head><body></body></html>`

	got := string(injectBase([]byte(doc), "/matrix/h/box/app/meet/"))
	if !strings.Contains(got, `<base href="/matrix/h/box/app/meet/">`) {
		t.Fatalf("no base element injected:\n%s", got)
	}
	// Immediately after <head>: a base element governs only the URLs that follow
	// it, so one placed after the first <script> or <link> would miss them.
	head := strings.Index(got, "<head>")
	base := strings.Index(got, "<base")
	meta := strings.Index(got, "<meta")
	if !(head < base && base < meta) {
		t.Errorf("base must sit directly after <head>, before any other element:\n%s", got)
	}

	// An existing base is REPLACED. Two base elements are not an error in HTML —
	// the first one wins — so appending ours would silently do nothing.
	const withBase = `<!doctype html><html><head><base href="/"><title>meet</title></head></html>`
	got = string(injectBase([]byte(withBase), "/app/meet/"))
	if strings.Count(got, "<base") != 1 {
		t.Fatalf("want exactly one base element:\n%s", got)
	}
	if !strings.Contains(got, `<base href="/app/meet/">`) {
		t.Errorf("the existing base was not replaced:\n%s", got)
	}

	// Reached directly, BasePrefix is "" and the href is bare "/" — still
	// trailing-slashed, still correct.
	r := httptest.NewRequest("GET", "/", nil)
	got = string(injectBase([]byte(doc), coopauth.BaseHref(r)))
	if !strings.Contains(got, `<base href="/">`) {
		t.Errorf("loopback base:\n%s", got)
	}
}

// A binary built without the SPA explains itself instead of 404-ing blankly: the
// API and the socket are working, and the missing piece is a build flag.
func TestSPAMissingExplainsItself(t *testing.T) {
	if spaFS != nil {
		t.Skip("built with -tags meetspa; the SPA is present")
	}
	newRoom(t)
	srv := serveTest(t)

	resp, body := doJSON(t, srv, "GET", "/", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if !strings.Contains(string(body), "meetspa") {
		t.Errorf("the page must name the build flag that fixes it:\n%s", body)
	}

	// And the rest of the surface is unaffected — that is the claim it makes.
	resp, _ = doJSON(t, srv, "GET", "/healthz", nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/healthz = %d", resp.StatusCode)
	}
}

func contains(list []string, want string) bool { return slices.Contains(list, want) }

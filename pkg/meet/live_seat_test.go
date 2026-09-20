// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package meet

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/bus"
	"github.com/qiangli/yoke/pkg/chat"
	"github.com/qiangli/yoke/pkg/room"
)

// These are the end-to-end regressions for the live defect on the Apps
// console (2026-09-19): a 1:1 with the sprint manager, and `@manager …` in the
// sprint room, each ran a FRESH one-shot under the manager's name while the
// manager's live seat sat in `inbox --watch` with the whole sprint in its
// context. The name pointed at two agents. The message must reach the live
// seat and nothing else; only an unheld identity gets a one-shot.

// claimSeat registers a live seat for agent in this test's room registry, the
// way `bashy inbox --watch --as <agent>` (log-only) or a bashy-launched session
// (with a control socket) does.
func claimSeat(t *testing.T, agent, mode, sock string) room.Card {
	t.Helper()
	card := room.Card{
		ID: room.AgentClaimID(agent), Binding: agent, Nick: agent,
		Mode: mode, PID: os.Getpid(), CtlSock: sock,
	}
	if err := room.Join(card); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { room.Leave(card.ID) })
	return card
}

// refuseOneShotDM fails the test if the DM path launches an agent.
func refuseOneShotDM(t *testing.T) {
	t.Helper()
	old := invokeRelayDM
	invokeRelayDM = func(context.Context, chat.Options, chat.Runner) (chat.Result, error) {
		t.Error("a one-shot was launched for an identity that has a live seat")
		return chat.Result{}, nil
	}
	t.Cleanup(func() { invokeRelayDM = old })
}

// captureSteer stubs the control-socket transport and records what was pushed.
func captureSteer(t *testing.T) *[]string {
	t.Helper()
	var pushed []string
	old := bus.SteerFrame
	bus.SteerFrame = func(_ string, text string) error {
		pushed = append(pushed, text)
		return nil
	}
	t.Cleanup(func() { bus.SteerFrame = old })
	return &pushed
}

func dmEnv(t *testing.T) {
	t.Helper()
	t.Setenv("BASHY_MEET_DIR", t.TempDir())
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	t.Setenv("BASHY_CAPABILITY_DIR", t.TempDir())
	t.Setenv("USER", "tester")
	pinFleet(t)
}

func TestSprintManagerDMReachesTheLiveSeatNotAOneShot(t *testing.T) {
	dmEnv(t)
	// The manager is live the way an operator-driven seat is: `inbox --watch
	// --as codex` — a claim with no control socket.
	claimSeat(t, "codex", "inbox", "")
	refuseOneShotDM(t)
	pushed := captureSteer(t)

	h := newServeHandler(context.Background(), MountOptions{})
	w := meetAPIRequest(t, h, http.MethodPost, "/api/dms/codex/messages", `{"text":"what is the state of sprint 217?"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Status   string       `json:"status"`
		Note     string       `json:"note"`
		Delivery seatDelivery `json:"delivery"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Status != "queued" || resp.Delivery.Session != "inbox" || resp.Delivery.Steered {
		t.Fatalf("delivery = %+v", resp)
	}
	if !strings.Contains(resp.Note, "reads its mail at its next turn") {
		t.Fatalf("the sender is not told how the seat will answer: %q", resp.Note)
	}
	if len(*pushed) != 0 {
		t.Fatalf("a seat with no control socket was pushed: %v", *pushed)
	}
	// The durable copy is the seat's directed mail, and it stays unread until
	// the seat reads it — that is how the live manager finds it.
	st, err := dmRoomFor("codex", "tester")
	if err != nil {
		t.Fatal(err)
	}
	directed, _, _, _, err := UnreadRecords(st.ID, "codex", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(directed) != 1 || directed[0].Event.Text != "what is the state of sprint 217?" {
		t.Fatalf("directed mail = %+v", directed)
	}
}

func TestSprintManagerDMIsPushedIntoASteerableLiveSeat(t *testing.T) {
	dmEnv(t)
	claimSeat(t, "codex", "interactive", "/tmp/fake-codex.sock")
	refuseOneShotDM(t)
	pushed := captureSteer(t)

	h := newServeHandler(context.Background(), MountOptions{})
	w := meetAPIRequest(t, h, http.MethodPost, "/api/dms/codex/messages", `{"text":"status please"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Delivery seatDelivery `json:"delivery"`
		Note     string       `json:"note"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Delivery.Steered || len(*pushed) != 1 || !strings.Contains((*pushed)[0], "status please") {
		t.Fatalf("steer did not reach the live session: delivery=%+v pushed=%v", resp.Delivery, *pushed)
	}
	if !strings.Contains((*pushed)[0], "tester") {
		t.Fatalf("the pushed line does not say who is asking: %q", (*pushed)[0])
	}
	if !strings.Contains(resp.Note, "answering now") {
		t.Fatalf("note = %q", resp.Note)
	}
}

// An identity nobody holds is the only case that still runs a one-shot — the
// path that always existed.
func TestSprintManagerDMFallsBackToAOneShotWhenNobodyHoldsTheName(t *testing.T) {
	dmEnv(t)
	invoked := make(chan chat.Options, 1)
	old := invokeRelayDM
	invokeRelayDM = func(_ context.Context, opt chat.Options, _ chat.Runner) (chat.Result, error) {
		invoked <- opt
		return chat.Result{Output: "one-shot reply"}, nil
	}
	t.Cleanup(func() { invokeRelayDM = old })

	h := newServeHandler(context.Background(), MountOptions{})
	w := meetAPIRequest(t, h, http.MethodPost, "/api/dms/codex/messages", `{"text":"anyone there?"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	opt := <-invoked
	if opt.Agent != "codex" || !strings.Contains(opt.Instruction, "anyone there?") {
		t.Fatalf("one-shot options = %+v", opt)
	}
}

func TestSprintRoomAddressReachesTheLiveSeatNotAOneShot(t *testing.T) {
	st := newRoom(t)
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	seatEverything(t)
	claimSeat(t, "codex", "sprint-inbox", "")
	old := apiRunner
	apiRunner = func() chat.Runner {
		t.Error("a one-shot runner was built for an identity that has a live seat")
		return fakeRunner{reply: "wrong agent"}
	}
	t.Cleanup(func() { apiRunner = old })
	pushed := captureSteer(t)

	note, err := Address(t.Context(), st.ID, "codex", "please report on your sprint")
	if err != nil {
		t.Fatalf("Address: %v", err)
	}
	if note.Kind != "note" || !strings.Contains(note.Text, "live sprint-inbox session") {
		t.Fatalf("the room was not told where the question went: %+v", note)
	}
	if len(*pushed) != 0 {
		t.Fatalf("a seat with no control socket was pushed: %v", *pushed)
	}
	// The question is the seat's directed mail and stays UNREAD: nothing here
	// answered it, so `bashy inbox` / `meet dispatch` must still hand it over.
	directed, _, _, err := Unread(st.ID, "codex", 0)
	if err != nil {
		t.Fatalf("Unread: %v", err)
	}
	if len(directed) != 1 || directed[0].Text != "please report on your sprint" {
		t.Fatalf("directed mail = %+v", directed)
	}
	events, _ := readTranscript(st.ID)
	for _, ev := range events {
		if ev.Speaker == "codex" && ev.Kind == "turn" {
			t.Fatalf("a turn was recorded for the live seat's name without the seat: %+v", ev)
		}
	}
}

func TestSprintRoomAddressIsPushedIntoASteerableLiveSeat(t *testing.T) {
	st := newRoom(t)
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	seatEverything(t)
	claimSeat(t, "codex", "weave", "/tmp/fake-codex.sock")
	old := apiRunner
	apiRunner = func() chat.Runner {
		t.Error("a one-shot runner was built for an identity that has a live seat")
		return fakeRunner{reply: "wrong agent"}
	}
	t.Cleanup(func() { apiRunner = old })
	pushed := captureSteer(t)

	note, err := Address(t.Context(), st.ID, "codex", "merge is blocked, look")
	if err != nil {
		t.Fatalf("Address: %v", err)
	}
	if !strings.Contains(note.Text, "answering now") {
		t.Fatalf("note = %+v", note)
	}
	if len(*pushed) != 1 || !strings.Contains((*pushed)[0], "merge is blocked, look") || !strings.Contains((*pushed)[0], st.ID) {
		t.Fatalf("the live seat was not told the room and the question: %v", *pushed)
	}
}

// The reply half: the seat answering in its own room must not address itself.
func TestTheSeatsOwnReplyInItsRoomIsNotMailToItself(t *testing.T) {
	st := newRoom(t)
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	seatEverything(t)
	old := bus.HostRoles
	bus.HostRoles = func() []bus.HostRole {
		return []bus.HostRole{{Label: "conductor:7", Holder: "codex"}}
	}
	t.Cleanup(func() { bus.HostRoles = old })
	if err := SetDefaultTo(st.ID, "conductor:7"); err != nil {
		t.Fatal(err)
	}
	// The human's unaddressed question goes to the seat.
	asked, err := PostAs(st.ID, st.Human, "", "how is it going?")
	if err != nil {
		t.Fatal(err)
	}
	if asked.To != "conductor:7" {
		t.Fatalf("the human's post was not addressed to the seat: %+v", asked)
	}
	// The seat's unaddressed answer goes to the room, not back to itself.
	answer, err := PostAs(st.ID, "codex", "", "going fine")
	if err != nil {
		t.Fatal(err)
	}
	if answer.To != "" {
		t.Fatalf("the seat's own reply was addressed to %q — mail to itself", answer.To)
	}
	directed, _, _, err := Unread(st.ID, "codex", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range directed {
		if d.Text == "going fine" {
			t.Fatalf("the seat's own reply is in its unread mail: %+v", directed)
		}
	}
}

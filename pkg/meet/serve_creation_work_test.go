package meet

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/agentlaunch"
	"github.com/qiangli/yoke/pkg/chat"
	"github.com/qiangli/yoke/pkg/coopauth"
	"github.com/qiangli/yoke/pkg/room"
)

func meetAPIRequest(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestWebMeetingCreateRequiresExplicitRegisteredOwnerAndParticipant(t *testing.T) {
	t.Setenv("BASHY_MEET_DIR", t.TempDir())
	t.Setenv("USER", "tester")
	pinFleet(t)
	h := newServeHandler(context.Background(), MountOptions{})

	for name, body := range map[string]string{
		"no owner":        `{"topic":"plan","participants":["claude-fable5"]}`,
		"unknown owner":   `{"topic":"plan","owner":"made-up-agent","participants":["claude-fable5"]}`,
		"no participants": `{"topic":"plan","owner":"codex"}`,
	} {
		t.Run(name, func(t *testing.T) {
			w := meetAPIRequest(t, h, http.MethodPost, "/api/rooms", body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}

	w := meetAPIRequest(t, h, http.MethodPost, "/api/rooms",
		`{"topic":"plan","owner":"codex","participants":["claude-fable5"]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var st State
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st.Chair != "codex" || len(st.Participants) != 1 || st.Participants[0] != "claude-fable5" {
		t.Fatalf("explicit roster was not preserved canonically: %+v", st)
	}
	if st.Round != 0 || st.SecretaryPending || st.recorded() {
		t.Fatalf("creation must not launch turns or silently add a secretary: %+v", st)
	}
	events, err := readTranscript(st.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("creation launched or recorded work: %+v", events)
	}
}

func TestWebStartWorkRefusesUncontainedHostBeforeLaunch(t *testing.T) {
	t.Setenv("BASHY_MEET_DIR", t.TempDir())
	t.Setenv("USER", "tester")
	pinFleet(t)
	oldContained, oldStart := trustedBashyWorkContainment, startRelayDMWork
	trustedBashyWorkContainment = func() (bool, string) { return false, "no trusted test provenance" }
	called := false
	startRelayDMWork = func(context.Context, string, chat.SessionOptions) (*chat.Session, error) {
		called = true
		return &chat.Session{}, nil
	}
	t.Cleanup(func() { trustedBashyWorkContainment, startRelayDMWork = oldContained, oldStart })

	h := newServeHandler(context.Background(), MountOptions{})
	// Extra client fields are deliberately powerless: the server derives every
	// launch option and refuses before invoking the launcher.
	w := meetAPIRequest(t, h, http.MethodPost, "/api/dms/codex/work",
		`{"text":"edit and test","allowUnsafe":true,"attended":true}`)
	if w.Code != http.StatusForbidden || called {
		t.Fatalf("status=%d launched=%v body=%s", w.Code, called, w.Body.String())
	}
}

func TestWebStartWorkDoesNotTrustAGenericContainerMarker(t *testing.T) {
	t.Setenv("BASHY_MEET_DIR", t.TempDir())
	t.Setenv("USER", "tester")
	pinFleet(t)
	oldProbe, oldStart := agentlaunch.Containerized, startRelayDMWork
	agentlaunch.Containerized = func() bool { return true }
	called := false
	startRelayDMWork = func(context.Context, string, chat.SessionOptions) (*chat.Session, error) {
		called = true
		return &chat.Session{}, nil
	}
	t.Cleanup(func() { agentlaunch.Containerized, startRelayDMWork = oldProbe, oldStart })

	h := newServeHandler(context.Background(), MountOptions{})
	w := meetAPIRequest(t, h, http.MethodPost, "/api/dms/codex/work", `{"text":"edit"}`)
	if w.Code != http.StatusForbidden || called {
		t.Fatalf("a generic container marker authorized work: status=%d launched=%v body=%s", w.Code, called, w.Body.String())
	}
}

func TestWebStartWorkRefusesUnnamedCloudActorBeforeLaunch(t *testing.T) {
	oldContained, oldStart := trustedBashyWorkContainment, startRelayDMWork
	trustedBashyWorkContainment = func() (bool, string) { return true, "trusted test runner" }
	called := false
	startRelayDMWork = func(context.Context, string, chat.SessionOptions) (*chat.Session, error) {
		called = true
		return &chat.Session{}, nil
	}
	t.Cleanup(func() { trustedBashyWorkContainment, startRelayDMWork = oldContained, oldStart })

	r := httptest.NewRequest(http.MethodPost, "/api/dms/codex/work", strings.NewReader(`{"text":"edit"}`))
	r.SetPathValue("agent", "codex")
	r.Header.Set(coopauth.HdrForwardedPrefix, "/matrix/h/box/app/meet")
	w := httptest.NewRecorder()
	handleRelayDMWork(context.Background()).ServeHTTP(w, r)
	if w.Code != http.StatusForbidden || called {
		t.Fatalf("unnamed cloud actor status=%d launched=%v body=%s", w.Code, called, w.Body.String())
	}
}

func TestWebStartWorkUsesManagedContainedSession(t *testing.T) {
	t.Setenv("BASHY_MEET_DIR", t.TempDir())
	t.Setenv("USER", "tester")
	pinFleet(t)
	oldContained, oldStart := trustedBashyWorkContainment, startRelayDMWork
	trustedBashyWorkContainment = func() (bool, string) { return true, "trusted test runner" }
	var gotAgent string
	var got chat.SessionOptions
	startRelayDMWork = func(_ context.Context, agent string, opts chat.SessionOptions) (*chat.Session, error) {
		gotAgent, got = agent, opts
		return &chat.Session{Agent: agent}, nil
	}
	t.Cleanup(func() { trustedBashyWorkContainment, startRelayDMWork = oldContained, oldStart })

	h := newServeHandler(context.Background(), MountOptions{})
	w := meetAPIRequest(t, h, http.MethodPost, "/api/dms/codex/work", `{"text":"edit and test"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if gotAgent != "codex" || got.Prompt != "edit and test" || got.ReadOnly || got.Attended || !got.AllowUnsafe || got.Mode != "meet-work" {
		t.Fatalf("managed session options = agent %q %+v", gotAgent, got)
	}
	events, err := relayDMEvents("codex", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Text != "edit and test" || events[0].Status != "work-started" {
		t.Fatalf("work request transcript = %+v", events)
	}
}

func TestManagedWorkDMQueuesExactlyOneInboxEventWithoutOneShot(t *testing.T) {
	t.Setenv("BASHY_MEET_DIR", t.TempDir())
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	t.Setenv("BASHY_CAPABILITY_DIR", t.TempDir())
	t.Setenv("USER", "tester")
	pinFleet(t)
	card := room.Card{
		ID: room.AgentClaimID("codex"), Binding: "codex", Nick: "codex",
		Mode: "meet-work", PID: os.Getpid(), Caps: []string{room.CapInboxDelivery},
	}
	if err := room.Join(card); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { room.Leave(card.ID) })
	oldInvoke := invokeRelayDM
	invoked := false
	invokeRelayDM = func(context.Context, chat.Options, chat.Runner) (chat.Result, error) {
		invoked = true
		return chat.Result{}, nil
	}
	t.Cleanup(func() { invokeRelayDM = oldInvoke })

	h := newServeHandler(context.Background(), MountOptions{})
	w := meetAPIRequest(t, h, http.MethodPost, "/api/dms/codex/messages", `{"text":"continue with tests"}`)
	if w.Code != http.StatusAccepted || invoked {
		t.Fatalf("status=%d one-shot=%v body=%s", w.Code, invoked, w.Body.String())
	}
	var response map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response["status"] != "queued" {
		t.Fatalf("status claim=%q, want queued", response["status"])
	}
	st, err := dmRoomFor("codex", "tester")
	if err != nil {
		t.Fatal(err)
	}
	directed, other, _, _, err := UnreadRecords(st.ID, "codex", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(directed) != 1 || len(other) != 0 || directed[0].Event.Text != "continue with tests" {
		t.Fatalf("inbox snapshot directed=%+v other=%+v", directed, other)
	}
}

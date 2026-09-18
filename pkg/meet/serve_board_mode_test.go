package meet

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// A board's floor is never run for it, and the HTTP surface must say so in the
// right vocabulary: 409, the well-formed-request-that-the-room's-state-forbids
// class ErrMeetingBusy already occupies — not 400, which blames the request,
// and not 202, which promises work that then dies where nobody is looking.
// The refusal happens in handleAsync BEFORE startJob for exactly that reason.
func TestBoardChairVerbsRefusedWith409(t *testing.T) {
	newRoom(t) // isolates the store and pins $USER/nowFn
	st, err := Create(CreateOptions{Topic: "board room", Participants: []string{"codex"}, Board: true})
	if err != nil {
		t.Fatalf("Create board: %v", err)
	}
	srv := serveTest(t)

	for _, tc := range []struct {
		verb string
		body any
	}{
		{"round", nil},
		{"poll", map[string]any{"question": "ship it?"}},
		{"ask", map[string]any{"question": "thoughts?"}},
		{"converge", nil},
	} {
		resp, body := doJSON(t, srv, "POST", "/api/rooms/"+url.PathEscape(st.ID)+"/"+tc.verb, tc.body)
		if resp.StatusCode != http.StatusConflict {
			t.Errorf("%s on a board = %d, want 409; body %s", tc.verb, resp.StatusCode, body)
			continue
		}
		// The message is the engine's own refusal: it names the mode and the
		// recovery, so a browser surfacing it verbatim tells the user what to do.
		if msg := apiErrorOf(t, body); !strings.Contains(msg, "board") {
			t.Errorf("%s refusal %q does not name the board mode", tc.verb, msg)
		}
	}

	// Address and post are how a board is USED, so the mode gate must not have
	// swallowed them. Post succeeds outright; address gets past the mode check
	// and fails only on routing the agent (anything but 409 wrong-mode).
	if _, err := PostAs(st.ID, "codex", "", "posting still works"); err != nil {
		t.Fatalf("PostAs on a board: %v", err)
	}
}

// The sentinel classifies without losing the engine's message, mirroring
// meetingBusyError: errors.Is sees ErrWrongMode AND the wrapped cause.
func TestWrongModeErrorClassifies(t *testing.T) {
	cause := errors.New("meet: room 7 is a board")
	err := wrongModeError{cause}
	if !errors.Is(err, ErrWrongMode) {
		t.Error("wrongModeError must classify as ErrWrongMode")
	}
	if !errors.Is(err, cause) {
		t.Error("wrongModeError must preserve its cause for errors.Is")
	}
	if err.Error() != cause.Error() {
		t.Errorf("Error() = %q, want the cause's message %q", err.Error(), cause.Error())
	}
}

// An ordinary meeting is untouched by the mode gate: the same verbs still reach
// startJob and answer 202/409 by the lease, exactly as before.
func TestMeetingChairVerbsStillReachStartJob(t *testing.T) {
	st := newRoom(t)
	srv := serveTest(t)

	// Hold the lease so startJob's probe answers 409 ErrMeetingBusy — proof the
	// request got PAST the mode gate and into the lease path.
	lease, err := acquireRunLease(st.ID)
	if err != nil {
		t.Fatalf("acquireRunLease: %v", err)
	}
	defer lease.Release()

	resp, body := doJSON(t, srv, "POST", "/api/rooms/"+url.PathEscape(st.ID)+"/round", nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("round on a busy meeting = %d, want 409; body %s", resp.StatusCode, body)
	}
	if msg := apiErrorOf(t, body); !strings.Contains(msg, "already being run") {
		t.Errorf("busy refusal %q should come from the lease, not the mode gate", msg)
	}
}

package ask

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/ctty"
)

// fakeDialog injects the tty/GUI dialog runner for one test. No test may pop a
// real dialog or open a real terminal; every case here fakes the rung and lets
// the ladder's own behavior be the thing under test.
func fakeDialog(t *testing.T, fn func(ctty.Channel, ctty.Request) ([]byte, error)) {
	t.Helper()
	prev := askDialog
	askDialog = fn
	t.Cleanup(func() { askDialog = prev })
}

// runObtain drives the ladder in the background and answers through the real
// rendezvous store as soon as a channel is open, so what is asserted is what
// production would see: the status prose, the answer command, the delivered
// value. Cases that never open a channel return on their own.
func runObtain(t *testing.T, r Request, probe ctty.Probe) (status string, value []byte, err error) {
	t.Helper()
	var buf bytes.Buffer
	type result struct {
		v   []byte
		err error
	}
	done := make(chan result, 1)
	go func() {
		v, err := obtain(r, probe, &buf)
		done <- result{v, err}
	}()

	answered := false
	tick := time.NewTicker(2 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case res := <-done:
			return buf.String(), res.v, res.err
		case <-tick.C:
			if !answered && channelReady(r.ID) {
				answered = true
				if err := Answer(r, []byte("rendezvous-answer")); err != nil {
					t.Errorf("delivering the rendezvous answer: %v", err)
				}
			}
		}
	}
}

// Story f2c250c5f038, the required shape: in a headless session (no tty, no
// usable GUI) the ladder goes straight to the rendezvous and PRINTS THE ANSWER
// COMMAND. No dialog rung may even be attempted — the rung's popup is the
// thing that auto-cancels.
func TestHeadlessSelectionPrintsTheAnswerCommand(t *testing.T) {
	isolate(t)
	r := newTestRequest(t)
	fakeDialog(t, func(ch ctty.Channel, _ ctty.Request) ([]byte, error) {
		t.Errorf("channel %q was attempted in a headless session — no dialog may be tried", ch)
		return nil, ctty.ErrNoGUI
	})

	status, v, err := runObtain(t, r, ctty.Probe{Requested: ctty.ChannelAuto})
	if err != nil {
		t.Fatalf("obtain: %v", err)
	}
	if string(v) != "rendezvous-answer" {
		t.Errorf("value = %q, want the rendezvous answer", v)
	}
	if !strings.Contains(status, "bashy ask claim "+shortID(r.ID)) {
		t.Errorf("the status never printed the exact answer command:\n%s", status)
	}
}

// A GUI rung that fails outright — helper missing, dialog cannot be shown —
// falls through to the rendezvous and prints the answer command. It must never
// surface as a cancel.
func TestGuiErrorFallsBackToRendezvous(t *testing.T) {
	isolate(t)
	r := newTestRequest(t)
	fakeDialog(t, func(ch ctty.Channel, _ ctty.Request) ([]byte, error) {
		if ch != ctty.ChannelGUI {
			t.Errorf("unexpected channel %q", ch)
		}
		return nil, fmt.Errorf("%w (osascript: exec: not found)", ctty.ErrNoGUI)
	})

	status, v, err := runObtain(t, r, ctty.Probe{Requested: ctty.ChannelAuto, GUI: true})
	if err != nil {
		t.Fatalf("obtain: %v", err)
	}
	if string(v) != "rendezvous-answer" {
		t.Errorf("value = %q, want the rendezvous answer", v)
	}
	if !strings.Contains(status, "bashy ask claim "+shortID(r.ID)) {
		t.Errorf("the fallback never printed the answer command:\n%s", status)
	}
}

// The incident's shape on the way back up: the dialog is dismissed by the
// SYSTEM (osascript -128 from a session with no reachable window server), not
// by an operator. Auto selection falls back to the rendezvous, loudly.
func TestSystemDismissedGuiFallsBackToRendezvous(t *testing.T) {
	isolate(t)
	r := newTestRequest(t)
	fakeDialog(t, func(ch ctty.Channel, _ ctty.Request) ([]byte, error) {
		if ch != ctty.ChannelGUI {
			t.Errorf("unexpected channel %q", ch)
		}
		return nil, ctty.ErrDismissed
	})

	status, v, err := runObtain(t, r, ctty.Probe{Requested: ctty.ChannelAuto, GUI: true})
	if err != nil {
		t.Fatalf("obtain: %v — a system dismissal must not end the request", err)
	}
	if string(v) != "rendezvous-answer" {
		t.Errorf("value = %q, want the rendezvous answer", v)
	}
	for _, want := range []string{"dismissed", "bashy ask claim " + shortID(r.ID)} {
		if !strings.Contains(status, want) {
			t.Errorf("status is missing %q:\n%s", want, status)
		}
	}
}

// ...and even a forced --channel gui, where no next rung exists, must fail
// LOUDLY with the answer command rather than reporting a silent cancel: the
// operator forced the channel, not the cancel.
func TestForcedGuiDismissalFailsLoudlyWithTheAnswerCommand(t *testing.T) {
	isolate(t)
	r := newTestRequest(t)
	fakeDialog(t, func(ch ctty.Channel, _ ctty.Request) ([]byte, error) {
		if ch != ctty.ChannelGUI {
			t.Errorf("unexpected channel %q", ch)
		}
		return nil, ctty.ErrDismissed
	})

	status, v, err := runObtain(t, r, ctty.Probe{Requested: ctty.ChannelGUI, GUI: true})
	if err != nil {
		t.Fatalf("obtain: %v — a forced channel may not end in a silent cancel", err)
	}
	if string(v) != "rendezvous-answer" {
		t.Errorf("value = %q, want the rendezvous answer", v)
	}
	if !strings.Contains(status, "bashy ask claim "+shortID(r.ID)) {
		t.Errorf("a forced-channel dismissal never printed the answer command:\n%s", status)
	}
}

// The line that must not move: a decline that WAS an operator action — the
// dialog's Cancel button — stays a cancel. No rendezvous command is printed,
// no second prompt is opened.
func TestExplicitOperatorCancelStaysACancel(t *testing.T) {
	isolate(t)
	r := newTestRequest(t)
	fakeDialog(t, func(_ ctty.Channel, _ ctty.Request) ([]byte, error) {
		return nil, ctty.ErrDeclined
	})

	status, _, err := runObtain(t, r, ctty.Probe{Requested: ctty.ChannelAuto, GUI: true})
	if err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("error = %v, want ask: cancelled", err)
	}
	if strings.Contains(status, "bashy ask claim") {
		t.Errorf("a cancelled ask opened a rendezvous — re-prompting after a human said no:\n%s", status)
	}
}

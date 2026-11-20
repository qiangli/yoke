package agentpty

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

const fakeTrustDialog = "Quick safety check: Do you trust the files in this folder?\r\n  1. Yes, I trust this folder\r\n❯ 2. No, exit\r\n"

func TestDialogGuardRequiresVisibleTrustSelection(t *testing.T) {
	var keys bytes.Buffer
	var confirmed string
	guard := NewDialogGuard(func(label string) { confirmed = label })
	guard.pty = &keys
	guard.observe(fakeTrustDialog)
	if keys.String() != "\x1b[A" || confirmed != "" {
		t.Fatalf("before redraw keys=%q confirmed=%q", &keys, confirmed)
	}
	guard.observe(fakeTrustDialog)
	if keys.String() != "\x1b[A" || confirmed != "" {
		t.Fatalf("after negative redraw keys=%q confirmed=%q", &keys, confirmed)
	}
	guard.observe("\x1b[2J❯ 1. Yes, I trust this folder\r\n  2. No, exit\r\n")
	if keys.String() != "\x1b[A\r" || confirmed != "Yes, I trust this folder" {
		t.Fatalf("after positive redraw keys=%q confirmed=%q", &keys, confirmed)
	}
	if guard.Clear() {
		t.Fatal("answering the dialog did not establish that it has closed")
	}
	guard.observe("\x1b[2J❯ \r\nstatus footer\r\n")
	if !guard.Clear() {
		t.Fatal("prompt redraw did not release delivery")
	}
}

func TestDialogGuardLeavesUnknownTrustOptionAndConsentAlone(t *testing.T) {
	for _, screen := range []string{
		"Do you trust the contents of this folder?\r\n❯ 1. Continue\r\n  2. No, exit",
		"Authentication required\r\n❯ 1. Sign in",
	} {
		var keys bytes.Buffer
		guard := NewDialogGuard(func(label string) {
			t.Errorf("confirmed %q", label)
		})
		guard.pty = &keys
		guard.observe(screen)
		if err := guard.Send(TextFrame("steer")); !errors.Is(err, ErrDialogActive) {
			t.Errorf("send error=%v, want active dialog", err)
		}
		if keys.Len() != 0 {
			t.Errorf("input reached dialog: %q", &keys)
		}
	}
}

type dialogAfterSubmit struct {
	got    strings.Builder
	submit chan struct{}
}

func (w *dialogAfterSubmit) Write(p []byte) (int, error) {
	n, err := w.got.Write(p)
	if string(p) == "\r" && !strings.HasSuffix(w.got.String(), "\r\r") {
		close(w.submit)
	}
	return n, err
}

func TestDialogGuardChecksDelayedEnter(t *testing.T) {
	prev := steerResubmitDelay
	steerResubmitDelay = 100 * time.Millisecond
	t.Cleanup(func() { steerResubmitDelay = prev })
	terminal := &dialogAfterSubmit{submit: make(chan struct{})}
	guard := NewDialogGuard(nil)
	guard.pty = terminal
	sent := make(chan error, 1)
	go func() { sent <- guard.Send(TextFrame("instruction")) }()
	select {
	case <-terminal.submit:
	case <-time.After(time.Second):
		t.Fatal("first Enter did not arrive")
	}
	guard.observe(fakeTrustDialog)
	if err := <-sent; err != nil {
		t.Fatalf("first submission was successful: %v", err)
	}
	if got := terminal.got.String(); got != "instruction\r" {
		t.Fatalf("terminal received %q; second Enter would select No, exit", got)
	}
}

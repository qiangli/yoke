package chat

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/agentpty"
)

func testDeliveryTiming() interactiveDeliveryTiming {
	return interactiveDeliveryTiming{
		poll: 2 * time.Millisecond, socketTimeout: 200 * time.Millisecond,
		settle: 15 * time.Millisecond, readyTimeout: 200 * time.Millisecond,
	}
}

func TestInteractivePromptWaitsForTUIReadiness(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "ctl")
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ready := &interactiveReady{}
	var calls atomic.Int32
	done := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		result <- deliverInteractivePromptWithTiming(context.Background(), "test", sock, "  hello  ", ready, done,
			func(text string) error {
				calls.Add(1)
				if text != "  hello  " {
					t.Errorf("instruction changed during delivery: %q", text)
				}
				return nil
			}, testDeliveryTiming())
	}()
	time.Sleep(25 * time.Millisecond)
	if got := calls.Load(); got != 0 {
		t.Fatalf("instruction sent before TUI readiness: %d calls", got)
	}
	_, _ = ready.Write([]byte("TUI"))
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("send calls = %d, want exactly 1", got)
	}
}

func TestInteractivePromptCancellationBeforeReadiness(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "ctl")
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var calls atomic.Int32
	err := deliverInteractivePromptWithTiming(ctx, "test", sock, "hello", &interactiveReady{}, make(chan struct{}),
		func(text string) error { calls.Add(1); return nil }, testDeliveryTiming())
	if err == nil {
		t.Fatal("expected cancellation error")
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("instruction sent after cancellation: %d calls", got)
	}
}

func TestInteractivePromptNeverDuplicatesSend(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "ctl")
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ready := &interactiveReady{}
	_, _ = ready.Write([]byte("ready"))
	time.Sleep(20 * time.Millisecond)
	var calls atomic.Int32
	err := deliverInteractivePromptWithTiming(context.Background(), "test", sock, "hello", ready, make(chan struct{}),
		func(text string) error { calls.Add(1); return nil }, testDeliveryTiming())
	if err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("send calls = %d, want exactly 1", got)
	}
}

// A trust dialog is drawn and then sits still — exactly what "settled" used to
// mean. codex quit when the instruction was typed into it. The instruction must
// wait until the dialog is answered and the TUI has drawn its real prompt.
func TestInteractivePromptWaitsOutATrustDialog(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "ctl")
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ready := &interactiveReady{}
	_, _ = ready.Write([]byte("\x1b[5;3HTrust\x1b[5;9Hthis\x1b[5;14Hfolder?\x1b[9;1H› 1. Trust and continue\x1b[10;3H2. Quit"))
	var calls atomic.Int32
	result := make(chan error, 1)
	go func() {
		result <- deliverInteractivePromptWithTiming(context.Background(), "test", sock, "hello", ready,
			make(chan struct{}), func(string) error { calls.Add(1); return nil }, testDeliveryTiming())
	}()
	time.Sleep(60 * time.Millisecond) // four settle windows with the dialog still up
	if got := calls.Load(); got != 0 {
		t.Fatalf("instruction typed into the trust dialog: %d calls", got)
	}
	ready.gateAnswered(agentpty.GateVerdict{Kind: agentpty.GateTrust}, "say_trust")
	_, _ = ready.Write([]byte("› Ask Codex to do anything"))
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("send calls = %d, want exactly 1", got)
	}
}

// Without an instruction the inbox relay used to be released at once — before
// the TUI drew anything. Its readiness is now the same screen check.
func TestInboxReadinessWaitsForAQuietGateFreeScreen(t *testing.T) {
	ready := &interactiveReady{}
	settle := 15 * time.Millisecond
	if ready.settled(settle) {
		t.Fatal("ready before the TUI drew anything")
	}
	_, _ = ready.Write([]byte("Do you trust the contents of this folder?\n 1. Yes  2. No"))
	time.Sleep(2 * settle)
	if ready.settled(settle) {
		t.Fatal("ready while a trust dialog is on screen")
	}
	ready.gateAnswered(agentpty.GateVerdict{Kind: agentpty.GateTrust}, "say_trust")
	_, _ = ready.Write([]byte("> "))
	if ready.settled(settle) {
		t.Fatal("ready before the redraw settled")
	}
	time.Sleep(2 * settle)
	if !ready.settled(settle) {
		t.Fatal("not ready on a quiet, gate-free prompt")
	}
}

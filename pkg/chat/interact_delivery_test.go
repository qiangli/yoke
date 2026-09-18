package chat

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
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

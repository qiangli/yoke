//go:build unix

package ollama

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A cancelled serve must stop the system ollama it spawned gracefully (SIGTERM),
// so ollama can stop its own runner children; a SIGKILL would orphan them.
func TestRunSystemServe_CancelStopsChildGracefully(t *testing.T) {
	dir := t.TempDir()
	ready := filepath.Join(dir, "ready")
	termed := filepath.Join(dir, "termed")
	fake := filepath.Join(dir, "ollama")
	script := "#!/bin/sh\n" +
		"trap 'echo x > " + termed + "; exit 0' TERM\n" +
		"echo x > " + ready + "\n" +
		"while :; do sleep 0.05; done\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runSystemServe(ctx, fake, os.Environ()) }()

	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fake ollama never started")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("runSystemServe did not return after cancel")
	}
	if _, err := os.Stat(termed); err != nil {
		t.Fatalf("child was not sent SIGTERM on cancel (killed hard instead): %v", err)
	}
}

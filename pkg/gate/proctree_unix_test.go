//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package gate

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRunLocalTimeoutReapsGateDescendants(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "leaked")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	out := RunLocal(ctx, dir, "(sleep 0.3; echo leaked > leaked) & wait", "")
	if out.Passed {
		t.Fatal("timed-out gate must not pass")
	}
	time.Sleep(500 * time.Millisecond)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("gate descendant survived cancellation: %v", err)
	}
}

// TestRunTimeoutReapsGateDescendants guards the OTHER launch path — the
// project-gate Run/runCommands path behind `bashy gate` and pkg/pair. Before
// Sprint 194 it built its command with a bare exec.CommandContext, so a timeout
// SIGKILLed only `sh` and the backgrounded descendant (the stand-in for a leaked
// go-test/interp.test child) was reparented to init and ran on to write the
// marker. With the command's process group prepared, the whole group dies and
// the marker never appears.
func TestRunTimeoutReapsGateDescendants(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "leaked")
	def := &Definition{
		Root:     dir,
		Source:   "test",
		Commands: []string{"(sleep 0.3; echo leaked > leaked) & wait"},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	res, err := Run(ctx, def, "/bin/sh")
	if err == nil && res != nil && res.Passed {
		t.Fatal("timed-out gate must not pass")
	}
	time.Sleep(500 * time.Millisecond)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("gate descendant survived cancellation on the Run path: %v", err)
	}
}

//go:build !windows

package agentpty

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// End to end through Run: the tool is given its instruction over the control
// socket, and only THEN draws a trust-shaped screen. The next thing it reads
// must be the caller's next line, not a "1" from the tap.
func TestRunDoesNotAnswerTrustTextAfterFirstInput(t *testing.T) {
	// Not t.TempDir(): its path is too long for a unix socket on macOS, and the
	// socket is the control channel a real session uses.
	dir, err := os.MkdirTemp("", "ap")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	script := filepath.Join(dir, "fake-tool")
	out := filepath.Join(dir, "read")
	// The empty-line skip is for the second Enter every steer gets.
	body := "#!/bin/sh\nIFS= read -r first\nprintf 'Do you trust this directory?\\n1. Yes\\n'\n" +
		"second=\nwhile [ -z \"$second\" ]; do IFS= read -r second || break; done\n" +
		"printf '%s|%s' \"$first\" \"$second\" > \"$1\"\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "c.sock")
	go func() {
		for _, line := range []string{"go", "done"} {
			for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
				if BrokerSay(sock, line) == nil {
					break
				}
			}
			// Long enough for a stray "1" to be typed ahead of "done".
			time.Sleep(time.Second)
		}
	}()
	exit, reason, err := Run(exec.Command(script, out), io.Discard, Options{
		CtlSock: sock, Capture: true, MaxRuntime: 20 * time.Second,
	})
	if err != nil || exit != 0 || reason != "" {
		t.Fatalf("Run: exit=%d reason=%q err=%v", exit, reason, err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "go|done" {
		t.Fatalf("tool read %q, want %q — the tap typed at a working session", got, "go|done")
	}
}

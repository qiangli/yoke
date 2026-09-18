//go:build !windows

package agentpty

import (
	"os/exec"
	"testing"
)

func TestRunAcceptsCommandWithoutContext(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exit 0")
	exit, reason, err := Run(cmd, nil, Options{Capture: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if exit != 0 || reason != "" {
		t.Fatalf("exit = %d, reason = %q; want 0 and empty", exit, reason)
	}
}

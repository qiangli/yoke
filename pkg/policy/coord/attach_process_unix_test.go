//go:build !windows

package coord

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestAttachedClaimHelper(t *testing.T) {
	if os.Getenv("GO_WANT_ATTACHED_CLAIM_HELPER") != "1" {
		return
	}
	cmd := NewClaimCmd(func() []string { return []string{"/w/bashy"} })
	script := fmt.Sprintf("echo $$ > %q; exec sleep 30", os.Getenv("CLAIM_CHILD_MARKER"))
	cmd.SetArgs([]string{"do1", "--", "sh", "-c", script})
	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

func TestAttachedClaimDiesWithParentAndDoesNotLeakFD(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "child.pid")
	cmd := exec.Command(os.Args[0], "-test.run=^TestAttachedClaimHelper$")
	cmd.Env = append(os.Environ(),
		"GO_WANT_ATTACHED_CLAIM_HELPER=1",
		"CLAIM_CHILD_MARKER="+marker,
		"BASHY_COORD_DIR="+dir,
		"BASHY_AGENT_ID=claim-parent",
		EpisodeEnv+"=ep-parent",
	)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	// Wait for a complete pid, not just the file: `echo $$ > file` creates
	// the file before writing it, and under load a read in between saw ""
	// (strconv.Atoi: parsing "": invalid syntax).
	deadline := time.Now().Add(10 * time.Second)
	var childPID int
	for {
		if b, err := os.ReadFile(marker); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 0 {
				childPID = pid
				break
			}
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			t.Fatal("child command did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Cleanup(func() { _ = syscall.Kill(childPID, syscall.SIGKILL) })

	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()

	// The command child is still alive. Acquisition succeeds only if the
	// lock descriptor was close-on-exec and the killed parent released it.
	c, l, err := AcquireAttached(dir, "do1", agentA(), "next holder", 200*time.Millisecond)
	if err != nil {
		t.Fatalf("resource remained held after parent SIGKILL: %v", err)
	}
	if err := ReleaseAttached(dir, c, l); err != nil {
		t.Fatal(err)
	}
}

//go:build windows

package chat

import (
	"os"
	"os/exec"
	"testing"
)

// A Windows run that was started and has been waited on must count as ended,
// or every tool run keeps its reservation unsettled and is never metered.
func TestBudgetOwnedGroupGoneAfterWait(t *testing.T) {
	cmd := exec.Command(os.Getenv("ComSpec"), "/c", "exit 0")
	setProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if budgetOwnedGroupGone(cmd) {
		t.Fatal("running process reported gone")
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if !budgetOwnedGroupGone(cmd) {
		t.Fatal("waited process reported unverified: its reservation is never settled")
	}
}

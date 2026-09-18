//go:build !windows

package chat

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestBudgetProcessGroupHelper(t *testing.T) {
	mode := os.Getenv("CHAT_BUDGET_GROUP_HELPER")
	if mode == "" {
		return
	}
	if mode == "child" {
		for {
			time.Sleep(time.Second)
		}
	}
	if mode == "parent" {
		c := exec.Command(os.Args[0], "-test.run=^TestBudgetProcessGroupHelper$")
		c.Env = append(os.Environ(), "CHAT_BUDGET_GROUP_HELPER=child")
		f, e := os.OpenFile(os.DevNull, os.O_WRONLY, 0600)
		if e != nil {
			os.Exit(2)
		}
		c.Stdout = f
		c.Stderr = f
		if e = c.Start(); e != nil {
			os.Exit(3)
		}
		if e = os.WriteFile(os.Getenv("CHAT_BUDGET_CHILD_PID"), []byte(strconv.Itoa(c.Process.Pid)), 0600); e != nil {
			os.Exit(4)
		}
		os.Exit(0)
	}
	os.Exit(0)
}
func TestBudgetProcessProofRetainsLiveDescendant(t *testing.T) {
	pidPath := filepath.Join(t.TempDir(), "child.pid")
	cmd := exec.Command(os.Args[0], "-test.run=^TestBudgetProcessGroupHelper$")
	cmd.Env = append(os.Environ(), "CHAT_BUDGET_GROUP_HELPER=parent", "CHAT_BUDGET_CHILD_PID="+pidPath)
	setProcessGroup(cmd)
	if e := cmd.Run(); e != nil {
		t.Fatal(e)
	}
	defer syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	raw, e := os.ReadFile(pidPath)
	if e != nil {
		t.Fatal(e)
	}
	pid, e := strconv.Atoi(strings.TrimSpace(string(raw)))
	if e != nil {
		t.Fatal(e)
	}
	defer syscall.Kill(pid, syscall.SIGKILL)
	if budgetOwnedGroupGone(cmd) {
		t.Fatal("direct parent exit falsely proved inherited child termination")
	}
	if e = budgetCompletionError(cmd); e == nil {
		t.Fatal("unknown descendant lifetime released")
	}
}
func TestBudgetProcessProofAcceptsVerifiedEmptyGroup(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestBudgetProcessGroupHelper$")
	cmd.Env = append(os.Environ(), "CHAT_BUDGET_GROUP_HELPER=empty")
	setProcessGroup(cmd)
	if e := cmd.Run(); e != nil {
		t.Fatal(e)
	}
	if !budgetOwnedGroupGone(cmd) {
		t.Fatal("verified empty owned group retained")
	}
	cmd.SysProcAttr = nil
	if budgetOwnedGroupGone(cmd) {
		t.Fatal("unowned process group treated as proof")
	}
}

//go:build !windows

package weave

import (
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// Both processes belong to this test. In particular, never discover a wrapper
// from the host queue or use the test runner's inherited process group.
func TestWeaveAbandonDoesNotSignalSiblingWrapper(t *testing.T) {
	root := setupIsolationFixture(t)
	t.Chdir(root)
	start := func(group int) (*exec.Cmd, <-chan struct{}) {
		c := exec.Command("sleep", "60")
		c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: group}
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() { _ = c.Wait(); close(done) }()
		t.Cleanup(func() { _ = c.Process.Kill(); <-done })
		return c, done
	}
	target, targetDone := start(0)
	sibling, siblingDone := start(target.Process.Pid)
	dir, err := weaveQueueDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := saveWeaveQueue(dir, &weaveQueue{Root: root, Items: []*weaveItem{
		{ID: 1, State: "working", WrapperPid: target.Process.Pid},
		{ID: 2, State: "working", WrapperPid: sibling.Process.Pid},
	}}); err != nil {
		t.Fatal(err)
	}
	if out, code := runWeave(t, "abandon", "1", "--yes", "--disposition", "superseded"); code != 0 {
		t.Fatalf("abandon: %d %s", code, out)
	}
	select {
	case <-targetDone:
	case <-time.After(time.Second):
		t.Fatal("target wrapper survived")
	}
	select {
	case <-siblingDone:
		t.Fatal("abandon signalled sibling wrapper in the same process group")
	case <-time.After(100 * time.Millisecond):
	}
	q, err := loadWeaveQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	if it := findWeaveItem(q, 2); it.State != "working" || it.WrapperPid != sibling.Process.Pid {
		t.Fatalf("sibling changed: %+v", it)
	}
}

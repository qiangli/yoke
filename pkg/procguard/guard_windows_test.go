//go:build windows

package procguard

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

const roleEnv = "BASHY_PROCGUARD_WINDOWS_ROLE"

// TestMain lets the test binary play the parent, child and grandchild roles.
func TestMain(m *testing.M) {
	switch os.Getenv(roleEnv) {
	case "print":
		fmt.Fprint(os.Stdout, os.Args[0])
		os.Exit(0)
	case "parent":
		runParent()
	case "child":
		runChild()
	case "grandchild":
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func self() string {
	exe, err := os.Executable()
	if err != nil {
		panic(err)
	}
	return exe
}

func roleCmd(role string, extra ...string) *exec.Cmd {
	cmd := exec.Command(self())
	cmd.Env = append(append(os.Environ(), roleEnv+"="+role), extra...)
	return cmd
}

// runParent arms a guarded child, then waits to be killed.
func runParent() {
	cmd := roleCmd("child")
	guard, err := Arm(cmd)
	if err != nil {
		os.Exit(2)
	}
	startErr := cmd.Start()
	guard.Started(startErr)
	if startErr != nil {
		os.Exit(3)
	}
	_ = cmd.Wait()
	guard.Disarm()
	os.Exit(0)
}

// runChild starts an unguarded grandchild, publishes its pid, and waits.
func runChild() {
	gc := roleCmd("grandchild")
	if err := gc.Start(); err != nil {
		os.Exit(4)
	}
	dir := os.Getenv("BASHY_PROCGUARD_WINDOWS_DIR")
	_ = os.WriteFile(filepath.Join(dir, "grandchild.pid.tmp"), []byte(strconv.Itoa(gc.Process.Pid)), 0o600)
	_ = os.Rename(filepath.Join(dir, "grandchild.pid.tmp"), filepath.Join(dir, "grandchild.pid"))
	_ = gc.Wait()
	os.Exit(0)
}

func TestGuardNormalRun(t *testing.T) {
	cmd := roleCmd("print")
	cmd.Args[0] = "deliberate-custom-argv0"
	var out bytes.Buffer
	cmd.Stdout = &out
	guard, err := Arm(cmd)
	if err != nil {
		t.Fatal(err)
	}
	startErr := cmd.Start()
	guard.Started(startErr)
	if startErr != nil {
		t.Fatal(startErr)
	}
	waitErr := cmd.Wait()
	guard.Disarm()
	if waitErr != nil {
		t.Fatal(waitErr)
	}
	if out.String() != "deliberate-custom-argv0" {
		t.Fatalf("argv[0] = %q", out.String())
	}
}

func TestJobObjectContainsSessionEscapes(t *testing.T) {
	if !ContainsSessionEscapes() {
		t.Fatal("the job-object guard contains every descendant and must say so")
	}
}

func TestGrandchildDiesWithParentJob(t *testing.T) {
	dir := t.TempDir()
	parent := roleCmd("parent", "BASHY_PROCGUARD_WINDOWS_DIR="+dir)
	if err := parent.Start(); err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(dir, "grandchild.pid")
	deadline := time.Now().Add(10 * time.Second)
	var raw []byte
	for {
		var err error
		if raw, err = os.ReadFile(pidFile); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = parent.Process.Kill()
			t.Fatal("grandchild never published its pid")
		}
		time.Sleep(10 * time.Millisecond)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		t.Fatalf("open grandchild %d: %v", pid, err)
	}
	defer windows.CloseHandle(h)
	if ev, _ := windows.WaitForSingleObject(h, 0); ev == windows.WAIT_OBJECT_0 {
		t.Fatal("grandchild exited before the parent was killed")
	}
	if err := parent.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = parent.Wait()
	if ev, _ := windows.WaitForSingleObject(h, 5000); ev != windows.WAIT_OBJECT_0 {
		_ = windows.TerminateProcess(h, 1)
		t.Fatalf("grandchild %d survived its parent's job closing", pid)
	}
}

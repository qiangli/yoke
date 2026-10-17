package fleet

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// shortTempDir is t.TempDir(), but short: a unix socket path under it must
// fit sun_path (~104 bytes on macOS/BSD), and t.TempDir() nests the test's
// own (sometimes long) name into the path.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ysd")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func TestSupervisordSocketPath(t *testing.T) {
	t.Setenv("BASHY_SUPERVISORD_SOCKET", "")
	t.Setenv("BASHY_HOME", "/h")
	if got, want := SupervisordSocketPath(), filepath.Join("/h", "service", "supervisord.sock"); got != want {
		t.Fatalf("SupervisordSocketPath = %q, want %q", got, want)
	}
	t.Setenv("BASHY_SUPERVISORD_SOCKET", "/s.sock")
	if got, want := SupervisordSocketPath(), "/s.sock"; got != want {
		t.Fatalf("SupervisordSocketPath (explicit) = %q, want %q", got, want)
	}
}

// TestEnsureSupervisordRunningNoopWhenAlreadyUp proves EnsureSupervisordRunning
// never touches argv when a monitor already answers the control socket: an
// empty argv would otherwise fail outright (see EnsureSupervisordRunning's
// "no supervisord command to start" error), so a nil-error result here can
// only mean the short-circuit fired.
func TestEnsureSupervisordRunningNoopWhenAlreadyUp(t *testing.T) {
	sock := filepath.Join(shortTempDir(t), "supervisord.sock")
	t.Setenv("BASHY_SUPERVISORD_SOCKET", sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	if err := EnsureSupervisordRunning(context.Background(), nil); err != nil {
		t.Fatalf("EnsureSupervisordRunning: %v", err)
	}
}

func TestEnsureSupervisordRunningRefusesWithNoArgvWhenDown(t *testing.T) {
	t.Setenv("BASHY_SUPERVISORD_SOCKET", filepath.Join(shortTempDir(t), "supervisord.sock"))
	if err := EnsureSupervisordRunning(context.Background(), nil); err == nil {
		t.Fatal("EnsureSupervisordRunning with no argv and no monitor: want an error")
	}
}

// supervisordHelperEnv marks the child half of
// TestEnsureSupervisordRunningStartsIt: a process that stands in for bashy
// supervisord by listening on the socket EnsureSupervisordRunning is
// polling, then exiting on its own after a few seconds (nothing in this
// test package holds its pid to reap it, since EnsureSupervisordRunning's
// whole point is to start-and-release a long-lived daemon it does not own).
const supervisordHelperEnv = "BASHY_FLEET_SUPERVISORD_HELPER"

// TestSupervisordHelperProcess is not a test; see supervisordHelperEnv.
func TestSupervisordHelperProcess(t *testing.T) {
	if os.Getenv(supervisordHelperEnv) == "" {
		t.Skip("child helper for TestEnsureSupervisordRunningStartsIt")
	}
	ln, err := net.Listen("unix", SupervisordSocketPath())
	if err != nil {
		os.Exit(1)
	}
	defer ln.Close()
	time.Sleep(5 * time.Second)
}

func TestEnsureSupervisordRunningStartsIt(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a real child process")
	}
	// Prove the child can actually start here before asking
	// EnsureSupervisordRunning to do it detached — a failure inside that
	// call is otherwise indistinguishable from "cannot spawn a child
	// process in this sandbox", which other tests in this repo skip rather
	// than fail on.
	probe := exec.Command(os.Args[0], "-test.run=^TestSupervisordHelperProcess$", "-test.v=false")
	if err := probe.Start(); err != nil {
		t.Skipf("cannot spawn a child process here: %v", err)
	}
	_ = probe.Process.Kill()
	_ = probe.Wait()

	sock := filepath.Join(shortTempDir(t), "supervisord.sock")
	t.Setenv("BASHY_SUPERVISORD_SOCKET", sock)
	t.Setenv(supervisordHelperEnv, "1")

	argv := []string{os.Args[0], "-test.run=^TestSupervisordHelperProcess$", "-test.v=false"}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := EnsureSupervisordRunning(ctx, argv); err != nil {
		t.Fatalf("EnsureSupervisordRunning: %v", err)
	}
	if !SupervisordRunning() {
		t.Fatal("socket still not answering after EnsureSupervisordRunning returned")
	}
}

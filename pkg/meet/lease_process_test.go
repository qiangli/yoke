package meet

import (
	"bufio"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The kernel-lock design rests on one claim that no in-process test can fully
// prove: when the owning PROCESS dies, the lock is gone, with no cleanup by
// anybody. So this test runs a real second process.
//
// It re-execs the test binary into leaseHolderProcess below, which takes the
// lease and then exits WITHOUT releasing it — the crash case. Along the way it
// also pins the live case: while that process holds the lease, this one is
// refused.
const leaseHolderEnv = "MEET_TEST_LEASE_HOLDER"

func TestOwnerProcessDeathReleasesLease(t *testing.T) {
	st := newTestSession(t)
	dir := os.Getenv("BASHY_MEET_DIR")

	cmd := exec.Command(os.Args[0], "-test.run=TestLeaseHolderProcess", "-test.v=false")
	cmd.Env = append(os.Environ(), leaseHolderEnv+"=1", "BASHY_MEET_DIR="+dir, "MEET_TEST_LEASE_ID="+st.ID)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start holder: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	// Wait until the child actually holds the lease, so the refusal below is a
	// real contention rather than a race with the child's startup.
	sc := bufio.NewScanner(stdout)
	if !sc.Scan() || !strings.HasPrefix(sc.Text(), "locked") {
		t.Fatalf("holder never reported the lease (got %q, err %v)", sc.Text(), sc.Err())
	}

	if _, err := acquireRunLease(st.ID); !errors.Is(err, ErrMeetingBusy) {
		t.Fatalf("a lease held by another process must refuse us; err = %v", err)
	}

	// Tell it to die, and wait for the process to be truly gone.
	_, _ = io.WriteString(stdin, "die\n")
	_ = stdin.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("holder exited badly: %v", err)
	}

	path, _ := leasePath(st.ID)
	if _, err := os.Stat(path); err != nil {
		t.Errorf("run.lock must outlive its owner: %v", err)
	}
	l, err := acquireRunLease(st.ID)
	if err != nil {
		t.Fatalf("the lease must be free once its owner is gone, got %v", err)
	}
	l.Release()
}

// TestLeaseHolderProcess is not a test. It is the child half of the test above,
// and does nothing unless re-exec'd with leaseHolderEnv set.
func TestLeaseHolderProcess(t *testing.T) {
	if os.Getenv(leaseHolderEnv) == "" {
		t.Skip("child-process helper for TestOwnerProcessDeathReleasesLease")
	}
	l, err := acquireRunLease(os.Getenv("MEET_TEST_LEASE_ID"))
	if err != nil {
		os.Stdout.WriteString("failed: " + err.Error() + "\n")
		os.Exit(3)
	}
	os.Stdout.WriteString("locked\n")
	_ = l // deliberately never released

	// Block until the parent says so, then leave abruptly. os.Exit skips every
	// deferred cleanup and every test-framework teardown, which is the point:
	// only the kernel can be releasing this lock.
	bufio.NewScanner(os.Stdin).Scan()
	os.Exit(0)
}

// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package dag

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
)

// writeFakeSSH drops an executable stand-in for the ssh client so every test
// here is hermetic: no network, no real ssh, ever.
func writeFakeSSH(t *testing.T, script string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake ssh client is a #!/bin/sh script — unix-only test harness")
	}
	fake := filepath.Join(t.TempDir(), "fakessh")
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return fake
}

func TestSSHTransportCommandArgs(t *testing.T) {
	x := NewSSHTransport(fleet.Host{Name: "bigbox"})
	if n, a, sh := x.commandArgs(); n != "ssh" || !sh ||
		!reflect.DeepEqual(a, []string{"bigbox", "bash", "-s"}) {
		t.Fatalf("plain = %q %v shell=%v", n, a, sh)
	}

	x = NewSSHTransport(fleet.Host{Name: "big", Address: "203.0.113.7", SSHUser: "alice", SSHPort: 2222})
	if n, a, _ := x.commandArgs(); n != "ssh" ||
		!reflect.DeepEqual(a, []string{"-l", "alice", "-p", "2222", "203.0.113.7", "bash", "-s"}) {
		t.Fatalf("reach flags = %q %v", n, a)
	}

	x = NewSSHTransport(fleet.Host{Name: "h"})
	x.Command = "ssh -i key"
	x.Shell = "none"
	if n, a, sh := x.commandArgs(); n != "ssh" || sh ||
		!reflect.DeepEqual(a, []string{"-i", "key", "h"}) {
		t.Fatalf("none = %q %v shell=%v", n, a, sh)
	}
}

func TestSSHTransportRunsBodyOnWorker(t *testing.T) {
	// The fake drops the target arg and execs the rest (`bash -s`), so the body
	// — fed on stdin — runs here, exercising the full dispatch path.
	fake := writeFakeSSH(t, "#!/bin/sh\nshift\nexec \"$@\"\n")
	x := NewSSHTransport(fleet.Host{Name: "bigbox"})
	x.Command = fake

	out := new(bytes.Buffer)
	res := x.Exec(context.Background(),
		&Worker{ID: "ssh-1", Venues: []string{VenueUserland}},
		&Task{Name: "remote", Host: "bigbox", Body: "echo ran-over-ssh worker=$DAG_FLEET_WORKER venue=$DAG_FLEET_VENUE\n"},
		TaskIO{Dir: t.TempDir(), Env: os.Environ(), Stdout: out, Stderr: new(bytes.Buffer)})
	if res.Status != StatusDone || res.Err != nil {
		t.Fatalf("status = %s (%v)", res.Status, res.Err)
	}
	// The fleet tag travels inside the body (ssh forwards no client env), and
	// carries the LOGICAL identity only.
	if got := out.String(); got != "ran-over-ssh worker=ssh-1 venue=userland\n" {
		t.Fatalf("out = %q", got)
	}
	if res.Host != "bigbox" {
		t.Errorf("placement alias not recorded: %q", res.Host)
	}
}

func TestSSHTransportDialsWithHostReach(t *testing.T) {
	// Reach from fleet.Host must surface as OpenSSH -l/-p flags plus the dial
	// target, with the body arriving on stdin — and nowhere else.
	fake := writeFakeSSH(t, "#!/bin/sh\nprintf 'argv=%s\\n' \"$*\"\ncat >/dev/null\n")
	x := NewSSHTransport(fleet.Host{Name: "big", Address: "203.0.113.7", SSHUser: "alice", SSHPort: 2222})
	x.Command = fake

	out := new(bytes.Buffer)
	res := x.Exec(context.Background(), &Worker{ID: "ssh-1"},
		&Task{Name: "remote", Body: "true\n"},
		TaskIO{Env: os.Environ(), Stdout: out, Stderr: new(bytes.Buffer)})
	if res.Status != StatusDone || res.Err != nil {
		t.Fatalf("status = %s (%v)", res.Status, res.Err)
	}
	if got := out.String(); got != "argv=-l alice -p 2222 203.0.113.7 bash -s\n" {
		t.Fatalf("client argv = %q", got)
	}
}

func TestSSHTransportRemoteExitIsAConformanceVerdict(t *testing.T) {
	// A body that RAN and lost is the code's own failure: exit code passthrough,
	// no error, and RecordAttempt seals a RunFailed verdict.
	fake := writeFakeSSH(t, "#!/bin/sh\nshift\nexec \"$@\"\n")
	x := NewSSHTransport(fleet.Host{Name: "bigbox"})
	x.Command = fake

	task := &Task{Name: "remote", Body: "exit 3\n"}
	w := &Worker{ID: "ssh-1"}
	res := x.Exec(context.Background(), w, task,
		TaskIO{Env: os.Environ(), Stdout: new(bytes.Buffer), Stderr: new(bytes.Buffer)})
	if res.Status != StatusFailed || res.ExitCode != 3 || res.Err != nil {
		t.Fatalf("res = %s exit=%d err=%v", res.Status, res.ExitCode, res.Err)
	}
	rec := RecordAttempt(task, w, 1, res)
	if rec.Status != RunFailed || rec.Failure == nil || rec.Failure.Code != FailExitNonzero {
		t.Fatalf("record = %+v", rec)
	}
	if err := rec.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestSSHTransportExit255IsUnreachableNotAVerdict(t *testing.T) {
	// OpenSSH reserves 255 for client/connection failure: no verdict was
	// earned, so the attempt must classify as infra, never conformance.
	fake := writeFakeSSH(t, "#!/bin/sh\ncat >/dev/null\nexit 255\n")
	x := NewSSHTransport(fleet.Host{Name: "big", Address: "203.0.113.7", SSHUser: "alice", SSHPort: 2222})
	x.Command = fake

	task := &Task{Name: "remote", Body: "true\n"}
	w := &Worker{ID: "ssh-1"}
	res := x.Exec(context.Background(), w, task,
		TaskIO{Env: os.Environ(), Stdout: new(bytes.Buffer), Stderr: new(bytes.Buffer)})
	if !errors.Is(res.Err, ErrWorkerUnreachable) {
		t.Fatalf("err = %v, want ErrWorkerUnreachable", res.Err)
	}
	assertNoReach(t, res, RecordAttempt(task, w, 1, res), FailUnreachable)
}

func TestSSHTransportClientMissingIsUnreachable(t *testing.T) {
	x := NewSSHTransport(fleet.Host{Name: "big", Address: "203.0.113.7", SSHUser: "alice", SSHPort: 2222})
	x.Command = filepath.Join(t.TempDir(), "no-such-client")

	task := &Task{Name: "remote", Body: "true\n"}
	w := &Worker{ID: "ssh-1"}
	res := x.Exec(context.Background(), w, task,
		TaskIO{Env: os.Environ(), Stdout: new(bytes.Buffer), Stderr: new(bytes.Buffer)})
	if !errors.Is(res.Err, ErrWorkerUnreachable) {
		t.Fatalf("err = %v, want ErrWorkerUnreachable", res.Err)
	}
	assertNoReach(t, res, RecordAttempt(task, w, 1, res), FailUnreachable)
}

func TestSSHTransportNoDialTargetIsUnreachable(t *testing.T) {
	x := NewSSHTransport(fleet.Host{})
	task := &Task{Name: "remote", Body: "true\n"}
	w := &Worker{ID: "ssh-1"}
	res := x.Exec(context.Background(), w, task,
		TaskIO{Stdout: new(bytes.Buffer), Stderr: new(bytes.Buffer)})
	if !errors.Is(res.Err, ErrWorkerUnreachable) {
		t.Fatalf("err = %v, want ErrWorkerUnreachable", res.Err)
	}
	assertNoReach(t, res, RecordAttempt(task, w, 1, res), FailUnreachable)
}

func TestSSHTransportCancelTerminatesRemoteAndReportsCanceled(t *testing.T) {
	// Cancellation must (a) tear the client down promptly — SIGTERM, so the
	// connection closes and the remote side is reaped, not orphaned — and (b)
	// report context.Canceled, which the contract records as the distinct
	// FailCanceled infra status rather than a conformance failure.
	dir := t.TempDir()
	started := filepath.Join(dir, "started")
	mark := filepath.Join(dir, "terminated")
	fake := writeFakeSSH(t,
		"#!/bin/sh\n"+
			"trap 'echo yes >\"$SSH_TEST_MARK\"; exit 143' TERM\n"+
			"echo yes >\"$SSH_TEST_STARTED\"\n"+
			"i=0; while [ $i -lt 200 ]; do sleep 0.05; i=$((i+1)); done\n")
	x := NewSSHTransport(fleet.Host{Name: "bigbox"})
	x.Command = fake

	env := append(os.Environ(), "SSH_TEST_STARTED="+started, "SSH_TEST_MARK="+mark)
	task := &Task{Name: "remote", Body: "true\n"}
	w := &Worker{ID: "ssh-1"}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan TaskResult, 1)
	go func() {
		done <- x.Exec(ctx, w, task,
			TaskIO{Env: env, Stdout: new(bytes.Buffer), Stderr: new(bytes.Buffer)})
	}()

	waitForFile(t, started, 5*time.Second, "fake client never started")
	cancel()

	var res TaskResult
	select {
	case res = <-done:
	case <-time.After(4 * time.Second): // must beat sshWaitDelay: graceful, not kill
		t.Fatal("Exec did not return after cancellation")
	}
	if !errors.Is(res.Err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", res.Err)
	}
	waitForFile(t, mark, 5*time.Second, "remote process was orphaned: no TERM delivered")

	rec := RecordAttempt(task, w, 1, res)
	if rec.Status != RunInfraFailed || rec.Failure == nil || rec.Failure.Code != FailCanceled {
		t.Fatalf("record = %+v, want infra-failed/%s", rec, FailCanceled)
	}
	if rec.Status.HasVerdict() {
		t.Fatal("a cancelled attempt must not carry a verdict")
	}
	if err := rec.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestSSHTransportCloseIsIdempotentAndRefusesExec(t *testing.T) {
	x := NewSSHTransport(fleet.Host{Name: "big", Address: "203.0.113.7", SSHUser: "alice", SSHPort: 2222})
	if err := x.Close(); err != nil {
		t.Fatal(err)
	}
	if err := x.Close(); err != nil { // once per worker sharing it — must stay nil
		t.Fatal(err)
	}
	task := &Task{Name: "remote", Body: "true\n"}
	w := &Worker{ID: "ssh-1"}
	res := x.Exec(context.Background(), w, task,
		TaskIO{Stdout: new(bytes.Buffer), Stderr: new(bytes.Buffer)})
	if !errors.Is(res.Err, ErrWorkerUnreachable) {
		t.Fatalf("exec after close: err = %v, want ErrWorkerUnreachable", res.Err)
	}
	assertNoReach(t, res, RecordAttempt(task, w, 1, res), FailUnreachable)
}

func TestPoolClosesSharedSSHTransportOncePerWorker(t *testing.T) {
	// One transport behind several workers: Pool.Close dedups comparable
	// transports, and Close stays nil however many times it lands.
	x := NewSSHTransport(fleet.Host{Name: "bigbox"})
	p := NewPool(nil,
		&Worker{ID: "ssh-1", Venues: []string{VenueUserland}, CPU: 2, Transport: x},
		&Worker{ID: "ssh-2", Venues: []string{VenueUserland}, CPU: 2, Transport: x},
	)
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	res := x.Exec(context.Background(), &Worker{ID: "ssh-1"},
		&Task{Name: "remote", Body: "true\n"},
		TaskIO{Stdout: new(bytes.Buffer), Stderr: new(bytes.Buffer)})
	if !errors.Is(res.Err, ErrWorkerUnreachable) {
		t.Fatalf("transport not closed by pool: err = %v", res.Err)
	}
}

// assertNoReach pins the privacy rule for this transport's failures: the
// TaskResult error and the sealed RunRecord must carry the expected infra code
// and no address, user, or port — worker identity stays logical.
func assertNoReach(t *testing.T, res TaskResult, rec RunRecord, wantCode string) {
	t.Helper()
	if rec.Status != RunInfraFailed || rec.Failure == nil || rec.Failure.Code != wantCode {
		t.Fatalf("record = %+v, want infra-failed/%s", rec, wantCode)
	}
	if rec.Status.HasVerdict() {
		t.Fatal("an undelivered attempt must not carry a verdict")
	}
	if err := rec.Validate(); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	for _, reach := range []string{"203.0.113.7", "alice", "2222"} {
		if strings.Contains(string(data), reach) {
			t.Errorf("run record leaks reach detail %q: %s", reach, data)
		}
		if res.Err != nil && strings.Contains(res.Err.Error(), reach) {
			t.Errorf("task result error leaks reach detail %q: %v", reach, res.Err)
		}
	}
}

func waitForFile(t *testing.T, path string, timeout time.Duration, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(msg)
}

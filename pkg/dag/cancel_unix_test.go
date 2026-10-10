//go:build !windows

package dag

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	_ "github.com/qiangli/coreutils/cmds/all"
)

// TestDagCancelHelper is not a test: it is the body of the child processes the
// real-process cancellation test spawns from this test binary.
func TestDagCancelHelper(t *testing.T) {
	switch os.Getenv("DAG_CANCEL_HELPER") {
	case "dag":
		cmd := newDagCmd()
		cmd.SetArgs([]string{"--no-journal", "--cache-dir", os.Getenv("DAG_CANCEL_DIR"), "-f", os.Getenv("DAG_CANCEL_FILE"), "fence"})
		os.Exit(ExitCodeOf(cmd.Execute()))
	case "sleeper":
		_ = os.WriteFile(filepath.Join(os.Getenv("DAG_CANCEL_DIR"), "grandchild.pid"), []byte(strconv.Itoa(os.Getpid())), 0o644)
		time.Sleep(time.Minute)
		os.Exit(0)
	}
}

func pidAlive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestDagSignalCancelsFenceProcessGroup(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGINT} {
		t.Run(sig.String(), func(t *testing.T) {
			dir := t.TempDir()
			self, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			// The fence loop: each iteration runs a subshell whose backgrounded
			// grandchild sleeps; a marker follows each iteration.
			body := `for n in 1 2 3; do
  /bin/sh -c 'DAG_CANCEL_HELPER=sleeper "$SELF" -test.run=^TestDagCancelHelper$ & wait'
  touch "$DAG_CANCEL_DIR/next-$n"
done`
			file := filepath.Join(dir, "dag.md")
			md := "---\nname: cancel\ndescription: cancel fixture\ntype: dag\n---\n\n## Tasks\n\n### fence\n" + block("bash", body)
			if err := os.WriteFile(file, []byte(md), 0o644); err != nil {
				t.Fatal(err)
			}

			// An unrelated process in its own group must survive the cancel.
			bystander := exec.Command("sleep", "60")
			bystander.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			if err := bystander.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = bystander.Process.Kill(); _ = bystander.Wait() }()

			errFile, err := os.Create(filepath.Join(dir, "stderr.log"))
			if err != nil {
				t.Fatal(err)
			}
			defer errFile.Close()
			stderrText := func() string { b, _ := os.ReadFile(errFile.Name()); return string(b) }
			run := exec.Command(self, "-test.run=^TestDagCancelHelper$")
			run.Env = append(os.Environ(), "DAG_CANCEL_HELPER=dag", "DAG_CANCEL_DIR="+dir, "DAG_CANCEL_FILE="+file, "SELF="+self)
			run.Stderr = errFile
			run.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			if err := run.Start(); err != nil {
				t.Fatal(err)
			}
			waited := make(chan error, 1)
			go func() { waited <- run.Wait() }()
			defer func() { _ = syscall.Kill(-run.Process.Pid, syscall.SIGKILL) }()

			pidFile := filepath.Join(dir, "grandchild.pid")
			var gpid int
			waitFor(t, "grandchild to start", func() bool {
				b, err := os.ReadFile(pidFile)
				if err != nil {
					return false
				}
				gpid, err = strconv.Atoi(strings.TrimSpace(string(b)))
				return err == nil && gpid > 0
			})

			if err := run.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			var waitErr error
			select {
			case waitErr = <-waited:
			case <-time.After(15 * time.Second):
				t.Fatalf("dag did not exit after %s; stderr:\n%s", sig, stderrText())
			}

			want := 128 + int(sig)
			ee, ok := waitErr.(*exec.ExitError)
			if !ok || ee.ExitCode() != want {
				t.Fatalf("exit = %v, want code %d; stderr:\n%s", waitErr, want, stderrText())
			}
			if !strings.Contains(stderrText(), "cancelled") {
				t.Errorf("stderr lacks cancellation status:\n%s", stderrText())
			}
			waitFor(t, "grandchild reaped", func() bool { return !pidAlive(gpid) })
			time.Sleep(300 * time.Millisecond) // a wrongly continuing loop would have launched by now
			matches, _ := filepath.Glob(filepath.Join(dir, "next-*"))
			if len(matches) != 0 {
				t.Errorf("fence loop continued after cancel: %v", matches)
			}
			if !pidAlive(bystander.Process.Pid) {
				t.Errorf("unrelated process %d was killed", bystander.Process.Pid)
			}
		})
	}
}

package jobs

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

func TestJobRegistryRecordList(t *testing.T) {
	dir := t.TempDir()
	r := NewJobRegistry(dir)

	myPID := os.Getpid()
	if err := r.Record(myPID, "(detached)"); err != nil {
		t.Fatalf("Record: %v", err)
	}

	rows, err := r.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows=%d, want 1", len(rows))
	}
	if rows[0].PID != myPID {
		t.Errorf("PID=%d, want %d", rows[0].PID, myPID)
	}
	if rows[0].Cmd != "(detached)" {
		t.Errorf("Cmd=%q, want %q", rows[0].Cmd, "(detached)")
	}
	if rows[0].User == "" {
		t.Errorf("User should be populated")
	}
	if rows[0].StartedAt.IsZero() {
		t.Errorf("StartedAt should be set")
	}
}

func TestJobRegistryGet(t *testing.T) {
	dir := t.TempDir()
	r := NewJobRegistry(dir)

	myPID := os.Getpid()
	if err := r.Record(myPID, "x"); err != nil {
		t.Fatalf("Record: %v", err)
	}
	rec, err := r.Get(myPID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if rec.PID != myPID || rec.Cmd != "x" {
		t.Fatalf("Get = %+v, want pid %d cmd x", rec, myPID)
	}
}

func TestJobRegistryPrunesDeadPid(t *testing.T) {
	dir := t.TempDir()
	r := NewJobRegistry(dir)

	cmd := exec.Command(os.Args[0], "-test.run=TestJobRegistryHelperProcess", "--")
	cmd.Env = append(os.Environ(), "JOBS_HELPER_PROCESS=1")
	if err := cmd.Run(); err != nil {
		t.Fatalf("run helper process: %v", err)
	}
	deadPID := cmd.ProcessState.Pid()
	if err := r.Record(deadPID, "(was here)"); err != nil {
		t.Fatalf("Record: %v", err)
	}

	rows, err := r.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("dead pid not pruned, got rows=%+v", rows)
	}
	if _, err := os.Stat(filepath.Join(dir, strconv.Itoa(deadPID)+".json")); !os.IsNotExist(err) {
		t.Fatalf("record for dead pid still exists: %v", err)
	}
}

func TestJobRegistryHelperProcess(t *testing.T) {
	if os.Getenv("JOBS_HELPER_PROCESS") != "1" {
		return
	}
	os.Exit(0)
}

func TestJobRegistryDeleteIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	r := NewJobRegistry(dir)

	myPID := os.Getpid()
	if err := r.Record(myPID, "x"); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := r.Delete(myPID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := r.Delete(myPID); err != nil {
		t.Errorf("Delete (idempotent): %v", err)
	}
}

func TestJobRegistryGetMissing(t *testing.T) {
	dir := t.TempDir()
	r := NewJobRegistry(dir)
	if _, err := r.Get(99999); err == nil {
		t.Error("Get on missing pid should error")
	}
}

func TestPidAlive(t *testing.T) {
	if !pidAlive(os.Getpid()) {
		t.Error("pidAlive(self) should be true")
	}
	if pidAlive(0) {
		t.Error("pidAlive(0) should be false")
	}
}

func TestNoopRegistry(t *testing.T) {
	r := NewJobRegistry("")
	if err := r.Record(1234, "x"); err == nil {
		t.Error("no-op Record should error")
	}
	rows, err := r.List()
	if err != nil {
		t.Errorf("no-op List should not error: %v", err)
	}
	if rows != nil {
		t.Errorf("no-op List should return nil, got %+v", rows)
	}
	if err := r.Delete(1234); err != nil {
		t.Errorf("no-op Delete should not error: %v", err)
	}
}

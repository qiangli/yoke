package telemetry

// The KILLED-ROTOR control: a real child process is terminated mid-compaction
// (before the truncate, after the copy has started writing), and the parent
// then proves the two properties the incident report demanded:
//
//   - total storage after the kill is bounded (no scratch file exists to
//     orphan; the file is no larger than what the child had written);
//   - recovery preserves the newest valid records and heals the file.
//
// This is a portable test: on Windows it runs natively (Process.Kill is
// TerminateProcess), which is exactly the platform whose interrupted
// rotations filled the disk.

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const killChildEnv = "TEST_SPOOL_ROTATE_KILLED_CHILD=1"

// TestRotateKilledChildProcessLeavesBoundedRecoverableSpool spawns the child
// half below, waits until the child is provably inside the copy (its ready
// marker appears only from inside the pre-truncate hook), kills it, and
// asserts the aftermath.
func TestRotateKilledChildProcessLeavesBoundedRecoverableSpool(t *testing.T) {
	withSmallSpoolCap(t, 32<<10)
	dir := t.TempDir()
	path := filepath.Join(dir, "spans.jsonl")
	ready := filepath.Join(dir, "child-inside-rotation")

	cmd := exec.Command(os.Args[0], "-test.run=^TestRotateKilledChildProcess$", "-test.v")
	cmd.Env = append(os.Environ(), killChildEnv, "BASHY_OTEL_SPOOL="+path, "TEST_SPOOL_ROTATE_READY="+ready)
	var childLog strings.Builder
	cmd.Stdout, cmd.Stderr = &childLog, &childLog
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill() }()

	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			t.Fatalf("child never entered rotation; log:\n%s", childLog.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill mid-rotation: %v", err)
	}
	_ = cmd.Wait()

	if names := scratchFiles(t, dir); len(names) != 0 {
		t.Errorf("killed rotation left scratch files: %v (the 1.3 TB failure mode)", names)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// The child wrote ~96 KiB and never truncated: bounded at exactly what it
	// wrote — not at soft bound, and never growing after death.
	if fi.Size() > 100<<10 {
		t.Errorf("spool after killed rotation = %d bytes; a kill must not add storage", fi.Size())
	}

	// Recovery: a fresh exporter sweeps (nothing to sweep), compacts, and the
	// newest records survive.
	e, err := newSpoolExporter(path)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Shutdown(context.Background())
	e.mu.Lock()
	e.rotateIfLarge()
	e.mu.Unlock()

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(b)) >= maxSpoolBytes {
		t.Errorf("recovery compaction did not bound the file: %d >= %d", len(b), maxSpoolBytes)
	}
	invalid, newest := 0, 0
	for _, ln := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		var v map[string]any
		if json.Unmarshal([]byte(ln), &v) != nil {
			invalid++
			continue
		}
		if seq, ok := v["seq"].(float64); ok && seq >= 90 {
			newest++
		}
	}
	if invalid != 0 {
		t.Errorf("%d invalid lines after kill+recovery; the copy filter must converge to a fully valid file", invalid)
	}
	if newest == 0 {
		t.Error("no newest records (seq>=90) survived the killed rotation and recovery")
	}
}

// The kill-DURING-copy control: the child is terminated after the first copy
// chunk has landed but long before the truncate, with a multi-chunk file so
// the copy genuinely cannot have finished. Recovery must bound the file,
// converge to fully valid JSON, and keep the newest records.
func TestRotateKilledDuringCopyLeavesBoundedRecoverableSpool(t *testing.T) {
	withSmallSpoolCap(t, 32<<10)
	dir := t.TempDir()
	path := filepath.Join(dir, "spans.jsonl")
	ready := filepath.Join(dir, "child-inside-copy")

	cmd := exec.Command(os.Args[0], "-test.run=^TestRotateKilledDuringCopyChild$", "-test.v")
	cmd.Env = append(os.Environ(), killChildEnv, "BASHY_OTEL_SPOOL="+path, "TEST_SPOOL_ROTATE_READY="+ready)
	var childLog strings.Builder
	cmd.Stdout, cmd.Stderr = &childLog, &childLog
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill() }()

	deadline := time.Now().Add(60 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			t.Fatalf("child never entered the copy; log:\n%s", childLog.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill mid-copy: %v", err)
	}
	_ = cmd.Wait()

	if names := scratchFiles(t, dir); len(names) != 0 {
		t.Errorf("killed copy left scratch files: %v (the 1.3 TB failure mode)", names)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// The child wrote ~2.4 MiB and never truncated: bounded at exactly what it
	// wrote — a kill must not add storage.
	if fi.Size() > 3<<20 {
		t.Errorf("spool after killed copy = %d bytes; a kill must not add storage", fi.Size())
	}

	e, err := newSpoolExporter(path)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Shutdown(context.Background())
	e.mu.Lock()
	e.rotateIfLarge()
	e.mu.Unlock()

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(b)) >= maxSpoolBytes {
		t.Errorf("recovery compaction did not bound the file: %d >= %d", len(b), maxSpoolBytes)
	}
	invalid, newest := 0, 0
	for _, ln := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		var v map[string]any
		if json.Unmarshal([]byte(ln), &v) != nil {
			invalid++
			continue
		}
		if seq, ok := v["seq"].(float64); ok && seq >= 3990 {
			newest++
		}
	}
	if invalid != 0 {
		t.Errorf("%d invalid lines after kill-during-copy + recovery; torn lines must be dropped, never preserved", invalid)
	}
	if newest == 0 {
		t.Error("no newest records (seq>=3990) survived the killed copy and recovery")
	}
}

// TestRotateKilledDuringCopyChild is the child half of the test above: pad
// past several copy chunks, install the rotation hook that announces itself
// and blocks inside the copy, and start the rotation. The parent kills this
// process between two copy chunks — the genuine interrupted rotation.
func TestRotateKilledDuringCopyChild(t *testing.T) {
	if os.Getenv("TEST_SPOOL_ROTATE_KILLED_CHILD") != "1" {
		t.Skip("parent-spawned child only")
	}
	maxSpoolBytes = 32 << 10
	path := os.Getenv("BASHY_OTEL_SPOOL")
	ready := os.Getenv("TEST_SPOOL_ROTATE_READY")

	e, err := newSpoolExporter(path)
	if err != nil {
		t.Fatal(err)
	}
	// Several copy chunks' worth: with rotateChunk at 1 MiB this file needs
	// more than 2 MiB for the first "copy" hook to fire mid-copy.
	for _, ln := range padLines(4000, 512) {
		if _, err := e.f.WriteString(ln); err != nil {
			t.Fatal(err)
		}
	}
	announced := false
	spoolRotateHook = func(stage string) {
		if stage == "copy" && !announced {
			announced = true
			if err := os.WriteFile(ready, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
				t.Fatal(err)
			}
			<-make(chan struct{}) // block forever; the parent kills us here
		}
	}
	e.mu.Lock()
	e.rotateIfLarge()
	e.mu.Unlock()
	// Unreachable in practice: the parent kills us inside the hook. Reaching
	// this line means the hook contract broke — fail the child so the parent
	// sees a non-zero exit.
	t.Fatal("rotation completed without the parent killing it; hook contract broken")
}

// TestRotateKilledChildProcess is the child half: pad past the bound, install
// the rotation hook that announces itself and blocks before the truncate, and
// start the rotation. The parent kills this process inside that hook.
func TestRotateKilledChildProcess(t *testing.T) {
	if os.Getenv("TEST_SPOOL_ROTATE_KILLED_CHILD") != "1" {
		t.Skip("parent-spawned child only")
	}
	maxSpoolBytes = 32 << 10
	path := os.Getenv("BASHY_OTEL_SPOOL")
	ready := os.Getenv("TEST_SPOOL_ROTATE_READY")

	e, err := newSpoolExporter(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, ln := range padLines(160, 512) {
		if _, err := e.f.WriteString(ln); err != nil {
			t.Fatal(err)
		}
	}
	// Announce from inside the rotation, then BLOCK: the parent kills this
	// process between the copy and the truncate — the exact interrupted
	// rotation the incident report demands be controlled.
	spoolRotateHook = func(stage string) {
		if stage == "pre-truncate" {
			if err := os.WriteFile(ready, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
				t.Fatal(err)
			}
			<-make(chan struct{}) // block forever; the parent kills us here
		}
	}
	e.mu.Lock()
	e.rotateIfLarge()
	e.mu.Unlock()
	// Unreachable in practice: the parent kills us inside the hook. Reaching
	// this line means the hook contract broke — fail the child so the parent
	// sees a non-zero exit.
	t.Fatal("rotation completed without the parent killing it; hook contract broken")
}

package weave

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/coreutils/pkg/lockfile"
	todopkg "github.com/qiangli/yoke/pkg/todo"
)

// The sprint-aware binary wires the board writability probe into todo's edit
// path, so `todo edit --sprint` fails before any store is written when the
// session sandbox denies the board.
func TestSprintPreflightIsWiredIntoTodo(t *testing.T) {
	if todopkg.SprintPreflight == nil {
		t.Fatal("todopkg.SprintPreflight is nil: the board probe is not wired, confined sessions half-save story moves")
	}
}

func TestSprintBoardPreflightPassesOnAWritableBoard(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BASHY_SPRINT_DIR", dir)
	if err := sprintBoardPreflight(); err != nil {
		t.Fatalf("preflight on a writable board: %v", err)
	}
	// The probe leaves the board it proved: no queue.json minted.
	if _, err := os.Stat(filepath.Join(dir, "queue.json")); !os.IsNotExist(err) {
		t.Fatalf("preflight wrote queue.json (stat err = %v); a probe must not mint board state", err)
	}
}

func TestSprintBoardPreflightPassesThroughContention(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BASHY_SPRINT_DIR", dir)
	held, err := lockfile.TryAcquire(filepath.Join(dir, "queue.lock"), lockfile.Holder{
		Name: "test-holder", PID: os.Getpid(), Intent: "hold the lock", Since: time.Now(),
	})
	if err != nil {
		t.Fatalf("holding queue.lock: %v", err)
	}
	defer held.Release()
	// A held lock is contention, not denial: the later reconcile waits its
	// turn, so the probe must not refuse the edit.
	if err := sprintBoardPreflight(); err != nil {
		t.Fatalf("preflight under contention: %v", err)
	}
}

// A board path that can never take a lock fails deterministically on every
// OS (no permission bits involved): the probe must refuse with the denial
// and name the unwritable board.
func TestSprintBoardPreflightRefusesAnUnusableBoardDir(t *testing.T) {
	blocker, err := os.CreateTemp(t.TempDir(), "not-a-dir")
	if err != nil {
		t.Fatal(err)
	}
	blocker.Close()
	t.Setenv("BASHY_SPRINT_DIR", blocker.Name())
	err = sprintBoardPreflight()
	if err == nil {
		t.Fatal("preflight over a file board dir succeeded, want refusal")
	}
	if !strings.Contains(err.Error(), blocker.Name()) {
		t.Fatalf("preflight error = %v, want it to name the board %s", err, blocker.Name())
	}
}

// The incident shape: the directory exists but the session may not create
// queue.lock inside it (sandbox EPERM). chmod is a unix permission story,
// so this runs there; the file-as-dir case above covers Windows.
func TestSprintBoardPreflightRefusesAnUnwritableBoardDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod permission bits do not deny writes on Windows")
	}
	dir := t.TempDir()
	board := filepath.Join(dir, "sprint")
	if err := os.MkdirAll(board, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(board, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(board, 0o755) })
	t.Setenv("BASHY_SPRINT_DIR", board)
	if err := sprintBoardPreflight(); err == nil {
		t.Skip("lock creation succeeded despite 0555 (elevated privileges ignore permission bits here)")
	} else if !strings.Contains(err.Error(), board) {
		t.Fatalf("preflight error = %v, want it to name the board %s", err, board)
	}
}

// The sharper incident shape: a lock file left over from an earlier run
// opens fine in a directory the session may no longer create files in, so
// a lock-only probe passes exactly when the save's tmp+rename then fails.
// The probe must cover the save path too and refuse before any other store
// is written.
func TestSprintBoardPreflightRefusesABoardItCannotSaveTo(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod permission bits do not deny writes on Windows")
	}
	dir := t.TempDir()
	board := filepath.Join(dir, "sprint")
	if err := saveWeaveQueue(board, &weaveQueue{NextID: 1}); err != nil {
		t.Fatal(err)
	}
	held, err := lockfile.TryAcquire(filepath.Join(board, "queue.lock"), lockfile.Holder{Name: "seed"})
	if err != nil {
		t.Fatal(err)
	}
	if err := held.Release(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(board, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(board, 0o755) })
	t.Setenv("BASHY_SPRINT_DIR", board)
	if err := sprintBoardPreflight(); err == nil {
		t.Skip("temp creation succeeded despite 0555 (elevated privileges ignore permission bits here)")
	} else if !strings.Contains(err.Error(), board) {
		t.Fatalf("preflight error = %v, want it to name the board %s", err, board)
	}
}

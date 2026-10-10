package stateaccess

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/qiangli/coreutils/pkg/lockfile"
)

// Sprint: #413; Story: #1829; Story-ID: ce5985c75042

func denyDir(t *testing.T) string {
	t.Helper()
	if os.Geteuid() == 0 || runtime.GOOS == "windows" {
		t.Skip("directory permission bits do not deny root or Windows")
	}
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	return dir
}

func TestCheckLockDeniedIsDiagnosedAndNotContention(t *testing.T) {
	path := filepath.Join(denyDir(t), "claims.lock")
	err := CheckLock("coord claim ledger", path)
	if !IsDenied(err) {
		t.Fatalf("want DeniedError, got %v", err)
	}
	if errors.Is(err, lockfile.ErrHeld) {
		t.Fatal("a denial must never read as contention")
	}
	msg := err.Error()
	for _, want := range []string{"DENIED", "not lock contention", "--writable-root " + filepath.Dir(path), "Do not delete or force"} {
		if !strings.Contains(msg, want) {
			t.Errorf("diagnosis missing %q: %s", want, msg)
		}
	}
	if _, serr := os.Stat(path); !errors.Is(serr, os.ErrNotExist) {
		t.Fatalf("a denied preflight created %s (%v)", path, serr)
	}
}

func TestCheckLockGrantedCreatesOnlyTheSentinel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fresh", "claims.lock")
	if err := CheckLock("coord claim ledger", path); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Size() != 0 {
		t.Fatalf("sentinel: %v %v", fi, err)
	}
}

// Contention stays contention: preflight takes no lock, so a held lock neither
// fails it nor blocks it, and the holder keeps its lock.
func TestCheckLockIgnoresAHeldLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.lock")
	held, err := lockfile.Acquire(path, lockfile.Holder{Name: "holder"})
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	if err := CheckLock("weave/sprint queue", path); err != nil {
		t.Fatalf("preflight against a held lock: %v", err)
	}
	if _, err := lockfile.TryAcquire(path, lockfile.Holder{Name: "rival"}); !errors.Is(err, lockfile.ErrHeld) {
		t.Fatalf("holder lost its lock after preflight: %v", err)
	}
}

func TestDiagnosePassesNonPermissionErrorsThrough(t *testing.T) {
	held := lockfile.ErrHeld
	if got := Diagnose("s", "/x", held); got != held {
		t.Fatalf("ErrHeld rewritten: %v", got)
	}
	other := errors.New("disk on fire")
	if got := Diagnose("s", "/x", other); got != other {
		t.Fatalf("unrelated error rewritten: %v", got)
	}
	if Diagnose("s", "/x", nil) != nil {
		t.Fatal("nil rewritten")
	}
	denied := &os.PathError{Op: "open", Path: "/x", Err: os.ErrPermission}
	if got := Diagnose("s", "/x", denied); !IsDenied(got) || !errors.Is(got, os.ErrPermission) {
		t.Fatalf("permission error not diagnosed or unwrap lost: %v", got)
	}
}

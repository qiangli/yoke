package telemetry

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// secureSpoolDir documents itself as hardening "the dedicated spool directory".
// SpoolPath()'s own fallback — no resolvable home — is os.TempDir(), which on
// Linux is the shared /tmp. There the chmod either fails (non-root: EPERM, so
// telemetry is disabled and an error is printed on every start) or SUCCEEDS as
// root and rewrites /tmp to 0700, locking every other user out of it. Either
// way the fallback path the package chose for itself is one it cannot open
// without side effects. Emulate a shared temp dir (1777, owned by us so the
// chmod can succeed) and require the exporter to leave it alone.
func TestSpoolFallbackDoesNotChmodSharedTempDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission semantics")
	}
	shared := filepath.Join(t.TempDir(), "tmp")
	if err := os.Mkdir(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	const want = os.ModeSticky | 0o777
	if err := os.Chmod(shared, want); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", "")
	t.Setenv("BASHY_OTEL_SPOOL", "")
	t.Setenv("TMPDIR", shared)

	path := SpoolPath()
	if filepath.Dir(path) != shared {
		t.Fatalf("SpoolPath()=%q, want it under the shared temp dir %q", path, shared)
	}
	e, err := newSpoolExporter(path)
	if err != nil {
		t.Fatalf("fallback spool unusable: %v", err)
	}
	defer e.Shutdown(context.Background())

	fi, err := os.Stat(shared)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode() & (os.ModePerm | os.ModeSticky); got != want {
		t.Errorf("shared temp dir mode = %v, want %v untouched (spool must not rewrite a directory it does not own the purpose of)", got, want)
	}
}

// BASHY_OTEL_SPOOL is an operator override for the FILE. A relative value —
// `BASHY_OTEL_SPOOL=spans.jsonl bashy …` — resolves against the working
// directory, and secureSpoolDir then chmods the working directory (a project
// checkout) to 0700. The override selects where spans go; it must not change
// the permissions of the directory it lands in.
func TestSpoolOverrideDoesNotChmodWorkingDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission semantics")
	}
	project := filepath.Join(t.TempDir(), "project")
	if err := os.Mkdir(project, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(project, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(project)
	t.Setenv("BASHY_OTEL_SPOOL", "spans.jsonl")

	e, err := newSpoolExporter(SpoolPath())
	if err != nil {
		t.Fatalf("override spool unusable: %v", err)
	}
	defer e.Shutdown(context.Background())

	fi, err := os.Stat(project)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o755 {
		t.Errorf("working directory permissions = %04o, want 0755 untouched", got)
	}
}

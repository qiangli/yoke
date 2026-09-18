package telemetry

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// secureSpoolDir Lstat()s the spool's parent and refuses it when that LAST
// path component is a symlink, reporting "not a directory" for something that
// is one. Symlinked directories are how operators relocate data (a bigger
// volume, a dotfiles checkout), and on macOS /tmp itself is a symlink
// (/tmp -> private/tmp): `BASHY_OTEL_SPOOL=/tmp/spans.jsonl`, accepted by every
// earlier release, now prints "bashy: telemetry disabled — spool: spool dir:
// not a directory" on every start. The check also buys nothing: only the final
// component is inspected, so a symlink one level up passes unchallenged (the
// second case below), and the FILE is already protected by openPrivateSpool's
// own Lstat + IsRegular check. A symlink to a directory is a directory.
func TestSpoolAcceptsSymlinkedDirectory(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable on this platform: %v", err)
	}
	for _, tc := range []struct {
		name string
		path string
		want string // where the spool must actually land
	}{
		{"parent is a symlink", filepath.Join(link, "spans.jsonl"), filepath.Join(real, "spans.jsonl")},
		{"grandparent is a symlink", filepath.Join(link, "sub", "spans.jsonl"), filepath.Join(real, "sub", "spans.jsonl")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, err := newSpoolExporter(tc.path)
			if err != nil {
				t.Fatalf("newSpoolExporter(%q) = %v; a symlink to a directory is a directory", tc.path, err)
			}
			defer e.Shutdown(context.Background())
			if _, err := os.Stat(tc.want); err != nil {
				t.Errorf("spool did not land in the link target: %v", err)
			}
		})
	}

	// The real-world instance: macOS ships /tmp as a symlink. Skip elsewhere.
	if fi, err := os.Lstat("/tmp"); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Skip("/tmp is not a symlink on this host; the t.TempDir cases above cover the defect")
	}
	f, err := os.CreateTemp("/tmp", "bashy-otel-spool-test-*.jsonl")
	if err != nil {
		t.Skipf("/tmp not writable: %v", err)
	}
	path := f.Name()
	_ = f.Close()
	t.Cleanup(func() { _ = os.Remove(path) })
	e, err := newSpoolExporter(path)
	if err != nil {
		t.Fatalf("BASHY_OTEL_SPOOL=%s is unusable on macOS: %v", path, err)
	}
	_ = e.Shutdown(context.Background())
}

// telemetryAutomation documents itself as recognizing POSITIVE evidence of a
// harness. `CI=false` is the opposite: the established convention (ci-info,
// is-ci, and every tool that honours `CI=false <cmd>` to leave CI mode) treats
// the literal "false" as "not CI". A person who exported it once — the
// create-react-app idiom — has it in every interactive shell for good, and the
// notice that exists so "is telemetry actually running?" can be answered at a
// glance is then hidden from exactly the human it is for.
func TestAutomationDetectionCIFalseIsNotAutomation(t *testing.T) {
	for _, name := range []string{"CI", "CODEX_CI", "WEAVE_AGENT", "BASHY_AGENT_ID", "BASHY_PRINCIPAL", "TERM"} {
		t.Setenv(name, "")
	}
	t.Setenv("CI", "false")
	if telemetryAutomation() {
		t.Error(`telemetryAutomation() = true with CI=false: "false" is not positive evidence of a harness`)
	}
	if !shouldReportTelemetryStatus(true) {
		t.Error("interactive stderr with CI=false must still receive the startup notice")
	}
}

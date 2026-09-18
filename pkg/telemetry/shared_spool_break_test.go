//go:build !windows

package telemetry

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// The exporter's own contract (spool.go): "a single write() of a line under
// the pipe-buffer size is atomic on POSIX, so several processes can share one
// spool without coordinating." A host-wide shared spool — a file created by
// the operator (or root) and left group/world-writable so every user's bashy
// appends to it — is that contract used as documented, and it worked on every
// release before this run: O_APPEND on a writable file succeeds regardless of
// who owns it.
//
// openPrivateSpool now follows the successful open with f.Chmod(0o600) and
// treats ITS failure as fatal. chmod(2) needs ownership, not write permission,
// so every user who is not the file's owner gets EPERM, telemetry is disabled,
// and — because init failures are "always visible" — every bashy invocation
// prints "bashy: telemetry disabled — spool: open spool: operation not
// permitted" into stderr, in every mode, including the captured agent logs
// this run set out to keep quiet. Hardening a file the caller can already
// write must be best-effort: a chmod we are not entitled to make is not a
// reason to throw the spool away.
//
// The fixture is any regular file on the host that we can append to but do
// not own (a world-writable log is the usual instance); the test opens it and
// writes nothing.
func TestSharedSpoolWritableButNotOwnedIsAccepted(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root owns every chmod; the failure needs an unprivileged caller")
	}
	fixture := findWritableForeignFile(t)
	if fixture == "" {
		t.Skip("no regular file on this host is writable by, but not owned by, the test user")
	}
	t.Setenv("BASHY_OTEL_SPOOL", fixture)
	e, err := newSpoolExporter(SpoolPath())
	if err != nil {
		t.Fatalf("BASHY_OTEL_SPOOL=%s (writable, owned by another user) rejected: %v — accepted by every release before this run", fixture, err)
	}
	_ = e.Shutdown(context.Background())
}

func findWritableForeignFile(t *testing.T) string {
	t.Helper()
	uid := uint32(os.Getuid())
	for _, dir := range []string{"/Library/Logs", "/var/log", "/var/tmp", "/tmp", "/private/var/tmp", "/private/tmp"} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, ent := range entries {
			if !ent.Type().IsRegular() {
				continue
			}
			p := filepath.Join(dir, ent.Name())
			st, err := os.Lstat(p)
			if err != nil || !st.Mode().IsRegular() {
				continue
			}
			sys, ok := st.Sys().(*syscall.Stat_t)
			if !ok || sys.Uid == uid {
				continue
			}
			f, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0)
			if err != nil {
				continue
			}
			_ = f.Close()
			return p
		}
	}
	return ""
}

// The startup notice exists so "where are my spans?" has a one-glance answer.
// newSpoolExporter now anchors a relative override to an absolute path
// (rightly: the shell will cd), but Init prints the RAW SpoolPath() string, so
// `BASHY_OTEL_SPOOL=spans.jsonl bashy` reports
//
//	bashy: telemetry on → spans.jsonl (service=bashy)
//
// and after the first cd that name points at nothing — or at someone else's
// spans.jsonl. The notice must name the file the exporter actually opened.
func TestStartupNoticeNamesAnchoredSpoolPath(t *testing.T) {
	if os.Getenv("TEST_TELEMETRY_RELNOTICE_CHILD") == "1" {
		shutdown := Init(context.Background())
		_ = shutdown(context.Background())
		os.Exit(0)
	}
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestStartupNoticeNamesAnchoredSpoolPath$")
	cmd.Dir = dir
	cmd.Env = append(scrubbedEnv(),
		"TEST_TELEMETRY_RELNOTICE_CHILD=1",
		"BASHY_TELEMETRY_NOTICE=1",
		"OTEL_TRACES_EXPORTER=file",
		"BASHY_OTEL_SPOOL=spans.jsonl",
	)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("child: %v\n%s", err, stderr.String())
	}
	out := stderr.String()
	_, rest, ok := strings.Cut(out, "telemetry on → ")
	if !ok {
		t.Fatalf("no startup notice on stderr with BASHY_TELEMETRY_NOTICE=1:\n%s", out)
	}
	reported, _, _ := strings.Cut(rest, " (service=")
	if !filepath.IsAbs(reported) {
		t.Fatalf("notice names %q; the exporter anchored the relative override to an absolute path and the notice must say which one", reported)
	}
	want, err := filepath.EvalSymlinks(filepath.Join(dir, "spans.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := filepath.EvalSymlinks(reported)
	if err != nil {
		t.Fatalf("notice names %q, which does not exist: %v", reported, err)
	}
	if got != want {
		t.Errorf("notice names %q, spool is %q", got, want)
	}
}

func TestDisplayPathAbbreviatesOnlyCurrentHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	inside := filepath.Join(home, ".agents", "otel", "spool", "spans.jsonl")
	wantInside := filepath.Join("$HOME", ".agents", "otel", "spool", "spans.jsonl")
	if got := displayPath(inside); got != wantInside {
		t.Errorf("displayPath(%q)=%q, want %q", inside, got, wantInside)
	}

	outside := filepath.Join(filepath.Dir(home), "someone-else", "spans.jsonl")
	if got := displayPath(outside); got != outside {
		t.Errorf("displayPath(%q)=%q, want the exact non-home path", outside, got)
	}
}

func TestStartupNoticeAbbreviatesHomeSpool(t *testing.T) {
	if os.Getenv("TEST_TELEMETRY_HOME_NOTICE_CHILD") == "1" {
		shutdown := Init(context.Background())
		_ = shutdown(context.Background())
		os.Exit(0)
	}
	home := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestStartupNoticeAbbreviatesHomeSpool$")
	cmd.Env = append(scrubbedEnv(),
		"TEST_TELEMETRY_HOME_NOTICE_CHILD=1",
		"HOME="+home,
		"BASHY_TELEMETRY_NOTICE=1",
		"OTEL_TRACES_EXPORTER=file",
	)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("child: %v\n%s", err, stderr.String())
	}
	out := stderr.String()
	want := "bashy: telemetry on → " + filepath.Join("$HOME", ".agents", "otel", "spool", "spans.jsonl") + " (service=bashy)"
	if !strings.Contains(out, want) {
		t.Errorf("startup notice %q does not contain %q", out, want)
	}
	if strings.Contains(out, home) {
		t.Errorf("startup notice leaks literal home path %q: %q", home, out)
	}
}

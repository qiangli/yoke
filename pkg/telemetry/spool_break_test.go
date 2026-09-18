//go:build !windows

package telemetry

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/creack/pty/v2"
)

// Commit f6aab1b6 ("avoid chmod on operator-owned directories") keeps the
// chmod for "Bashy's canonical .../spool/spans.jsonl directory" — but it
// identifies that directory by BASENAME, not by being the canonical path under
// $HOME/.agents/otel. Any operator override whose directory happens to be
// called "spool" is misidentified as Bashy-owned and rewritten to 0700:
// `BASHY_OTEL_SPOOL=/var/spool/spans.jsonl` as root chmods /var/spool (0755,
// not sticky, serves cron/mail/lp) and a project's group-shared spool/ loses its
// group bits AND its setgid bit, because os.Chmod(dir, 0o700) clears setgid.
// The exemption for sticky directories does not cover either.
func TestSpoolOverrideDoesNotChmodOperatorDirNamedSpool(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "spool")
	if err := os.Mkdir(dir, 0o775); err != nil {
		t.Fatal(err)
	}
	const want = os.ModeSetgid | 0o775 // a group-shared spool: rwxrwsr-x
	if err := os.Chmod(dir, want); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode() & (os.ModePerm | os.ModeSetgid); got != want {
		t.Skipf("filesystem cannot represent %v (got %v)", want, got)
	}
	if err := os.WriteFile(filepath.Join(dir, "other.txt"), []byte("not ours\n"), 0o664); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BASHY_OTEL_SPOOL", filepath.Join(dir, "spans.jsonl"))

	e, err := newSpoolExporter(SpoolPath())
	if err != nil {
		t.Fatalf("override spool unusable: %v", err)
	}
	defer e.Shutdown(context.Background())

	fi, err = os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode() & (os.ModePerm | os.ModeSetgid); got != want {
		t.Errorf("operator directory mode = %v, want %v untouched: a directory is not Bashy's because its basename is \"spool\"", got, want)
	}
}

// The proposer's own TestSpoolOverrideDoesNotChmodWorkingDirectory blesses a
// RELATIVE override (`BASHY_OTEL_SPOOL=spans.jsonl bashy …`). The exporter
// keeps that relative string as e.path and every later filesystem operation in
// rotateIfLarge — ReadFile, CreateTemp, Rename, reopen — resolves it against the
// CURRENT working directory. bashy is a shell; `cd` is its most common command.
// After one, rotation reads whatever spans.jsonl sits in the NEW directory,
// renames a half of IT over itself, reopens IT as the spool, and abandons the
// real spool at 64 MiB. A foreign file is truncated and the spool changes
// directory without anyone asking.
func TestRelativeSpoolOverrideSurvivesChdir(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	elsewhere := filepath.Join(root, "elsewhere")
	for _, d := range []string{home, elsewhere} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	foreign := filepath.Join(elsewhere, "spans.jsonl")
	foreignBody := []byte(`{"_msg":"someone else's spool"}` + "\n")
	if err := os.WriteFile(foreign, foreignBody, 0o644); err != nil {
		t.Fatal(err)
	}

	t.Chdir(home)
	t.Setenv("BASHY_OTEL_SPOOL", "spans.jsonl")
	e, err := newSpoolExporter(SpoolPath())
	if err != nil {
		t.Fatalf("relative override unusable: %v", err)
	}
	defer e.Shutdown(context.Background())
	spool := filepath.Join(home, "spans.jsonl")

	// The shell moves on. The spool must not.
	t.Chdir(elsewhere)

	line := `{"_msg":"pad","filler":"` + strings.Repeat("x", 512) + `"}` + "\n"
	var buf strings.Builder
	for buf.Len() < maxSpoolBytes+(1<<20) {
		buf.WriteString(line)
	}
	if _, err := e.f.WriteString(buf.String()); err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	e.rotateIfLarge()
	e.mu.Unlock()

	if got, err := os.ReadFile(foreign); err != nil || !bytes.Equal(got, foreignBody) {
		t.Errorf("rotation touched a foreign spans.jsonl in the new cwd: err=%v, %d bytes (want %d, untouched)", err, len(got), len(foreignBody))
	}
	fi, err := os.Stat(spool)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() >= maxSpoolBytes {
		t.Errorf("real spool %s not rotated: size=%d, bound=%d — the 64 MiB bound is void once the shell changes directory", spool, fi.Size(), maxSpoolBytes)
	}
	if _, err := e.f.WriteString(`{"_msg":"after"}` + "\n"); err != nil {
		t.Errorf("spool not writable after rotation: %v", err)
	}
	if b, _ := os.ReadFile(spool); !strings.Contains(string(b), `"after"`) {
		t.Errorf("post-rotation span did not land in the real spool %s (the exporter now writes somewhere else)", spool)
	}
}

// "Automation-quiet" is decided by term.IsTerminal(stderr). But this repo's own
// automation substrate runs children under a PTY precisely so they behave like
// interactive CLIs. Such a harness must stamp positive automation evidence;
// Weave does so with BASHY_AGENT_ID/BASHY_PRINCIPAL.
func TestStartupNoticeStaysOutOfPTYDrivenAutomation(t *testing.T) {
	if os.Getenv("TEST_TELEMETRY_PTY_CHILD") == "1" {
		shutdown := Init(context.Background())
		_ = shutdown(context.Background())
		os.Exit(0)
	}
	spool := filepath.Join(t.TempDir(), "spans.jsonl")
	cmd := exec.Command(os.Args[0], "-test.run=^TestStartupNoticeStaysOutOfPTYDrivenAutomation$")
	// Commit 280573ba answered this test with an env-marker allowlist (CI,
	// CODEX_CI, WEAVE_AGENT, BASHY_AGENT_ID, BASHY_PRINCIPAL, TERM=dumb) and the
	// test went green — but only because `go test` was itself running under
	// weave, whose markers the child inherited. On a clean clone (`git clone;
	// go test ./pkg/telemetry` from an xterm with no CI) the same test is red.
	// A test whose verdict is decided by the invoker's environment asserts
	// nothing; scrub the markers so the child is exactly what the comment above
	// describes: a PTY allocated by a harness that did not stamp it.
	env := []string{}
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		switch name {
		case "CI", "CODEX_CI", "WEAVE_AGENT", "BASHY_AGENT_ID", "BASHY_PRINCIPAL", "BASHY_AGENT", "TERM":
			continue
		}
		if strings.HasPrefix(kv, "BASHY_TELEMETRY_") || strings.HasPrefix(kv, "OTEL_") {
			continue
		}
		env = append(env, kv)
	}
	cmd.Env = append(env,
		"TERM=xterm",
		"BASHY_AGENT_ID=test-harness",
		"TEST_TELEMETRY_PTY_CHILD=1",
		"OTEL_TRACES_EXPORTER=file",
		"BASHY_OTEL_SPOOL="+spool,
	)
	f, err := pty.Start(cmd)
	if err != nil {
		t.Skipf("no pty available: %v", err)
	}
	defer f.Close()
	var out bytes.Buffer
	_, _ = io.Copy(&out, f) // EIO at child exit is the normal EOF on a pty
	if err := cmd.Wait(); err != nil {
		t.Fatalf("child: %v\n%s", err, out.String())
	}
	if strings.Contains(out.String(), "telemetry on") {
		t.Errorf("routine startup notice leaked into a PTY-driven, unattended child:\n%s", out.String())
	}
}

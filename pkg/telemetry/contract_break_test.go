package telemetry

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// cleanChildEnv returns the environment with every telemetry switch, OTEL_*
// variable and automation marker removed, so a child observes only what the
// test sets. (attended_break_test.go has a unix-only twin; this file must also
// compile for the windows leg.)
func cleanChildEnv() []string {
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
	return env
}

// The run's docs (telemetry.go package comment, agent-run-observability-otel.md)
// state: "Network export remains opt-in: set an OTLP endpoint or exporter
// explicitly." Init does not honour the second half of that sentence, and the
// failure is SILENT — the one failure mode this package exists to eliminate:
//
//   - OTEL_TRACES_EXPORTER=otlp with no endpoint: isFileExporter() says "not
//     file", the endpoint gate says "no-op", and Init returns without a span
//     exporter, without Enabled(), and without a word on stderr — even with
//     BASHY_TELEMETRY_NOTICE=1. The OTel spec gives the OTLP exporter a default
//     endpoint (http://localhost:4318); an operator who set the standard
//     exporter knob gets nothing, and nothing says so.
//   - OTEL_TRACES_EXPORTER=zipkin (or any value this package does not
//     implement): same silent no-op. The agent contract says an unsupported
//     option fails loudly naming what is unsupported; a shell that quietly
//     ignores the operator's exporter choice is the opposite.
//   - OTEL_EXPORTER_OTLP_TRACES_ENDPOINT — the standard signal-specific
//     endpoint — is an endpoint, but hasOTLPEndpoint() reads only the generic
//     variable, so the operator's collector is bypassed and the startup notice
//     announces a spool FILE as the destination.
//
// Each case must either turn telemetry on (and say where) or say why it did not.
func TestExporterSelectionIsNeverSilent(t *testing.T) {
	if os.Getenv("TEST_TELEMETRY_SELECT_CHILD") == "1" {
		shutdown := Init(context.Background())
		_ = shutdown(context.Background())
		if Enabled() {
			os.Stdout.WriteString("enabled\n")
		}
		os.Exit(0)
	}
	for _, tc := range []struct {
		name string
		env  []string
		// wantEnabled: telemetry must be exporting somewhere (stdout "enabled").
		wantEnabled bool
		// wantStderr: a substring that must appear on stderr.
		wantStderr string
		// rejectStderr: a substring that must NOT appear on stderr.
		rejectStderr string
	}{
		{
			name:        "explicit otlp exporter, no endpoint",
			env:         []string{"OTEL_TRACES_EXPORTER=otlp"},
			wantEnabled: true,
			wantStderr:  "telemetry",
		},
		{
			name:       "unsupported exporter name is refused by name",
			env:        []string{"OTEL_TRACES_EXPORTER=zipkin"},
			wantStderr: "zipkin",
		},
		{
			name:         "signal-specific OTLP endpoint selects the network",
			env:          []string{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=http://127.0.0.1:1/v1/traces"},
			wantEnabled:  true,
			wantStderr:   "127.0.0.1:1",
			rejectStderr: "spans.jsonl",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spool := filepath.Join(t.TempDir(), "spans.jsonl")
			cmd := exec.Command(os.Args[0], "-test.run=^TestExporterSelectionIsNeverSilent$")
			cmd.Env = append(cleanChildEnv(),
				"TEST_TELEMETRY_SELECT_CHILD=1",
				"BASHY_TELEMETRY_NOTICE=1",
				"BASHY_OTEL_SPOOL="+spool,
			)
			cmd.Env = append(cmd.Env, tc.env...)
			var stdout, stderr strings.Builder
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			if err := cmd.Run(); err != nil {
				t.Fatalf("child: %v\n%s", err, stderr.String())
			}
			enabled := strings.Contains(stdout.String(), "enabled")
			if enabled != tc.wantEnabled {
				t.Errorf("%v: Enabled() = %v, want %v", tc.env, enabled, tc.wantEnabled)
			}
			if tc.wantStderr != "" && !strings.Contains(stderr.String(), tc.wantStderr) {
				t.Errorf("%v: stderr %q does not mention %q — telemetry silently %s", tc.env, stderr.String(), tc.wantStderr, map[bool]string{true: "went somewhere else", false: "did nothing"}[enabled])
			}
			if tc.rejectStderr != "" && strings.Contains(stderr.String(), tc.rejectStderr) {
				t.Errorf("%v: stderr %q names %q; the operator configured a collector, not a file", tc.env, stderr.String(), tc.rejectStderr)
			}
		})
	}
}

// The run documents the default sink as "the bounded owner-only local spool".
// defaultSpoolPath()'s no-home fallback is a FIXED NAME in the shared temp
// directory — /tmp/bashy-otel-spool.jsonl — and commit 7814b246 made an
// existing file at a non-canonical path an "explicit shared spool" that is
// opened as-is and never hardened. The fallback is not explicit and not the
// operator's: it is the package's own default, chosen when HOME is unset
// (cron, systemd units, containers, `env -i`). Anyone on the host can
// pre-create that name world-writable and every homeless bashy then appends
// its command telemetry into a file the attacker owns and reads. The classic
// predictable-name-in-/tmp mistake, and the exact opposite of owner-only.
//
// Either the exporter must refuse a pre-existing file at its own default
// fallback, or the file it uses must end up 0600. Today it does neither.
func TestDefaultTempFallbackSpoolIsOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission semantics")
	}
	shared := filepath.Join(t.TempDir(), "tmp")
	if err := os.Mkdir(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(shared, os.ModeSticky|0o777); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", "")
	t.Setenv("BASHY_OTEL_SPOOL", "")
	t.Setenv("TMPDIR", shared)

	path := SpoolPath()
	if filepath.Dir(path) != shared {
		t.Fatalf("SpoolPath()=%q, want the fallback under %q", path, shared)
	}
	// The squatter got there first: same name, wide open.
	if err := os.WriteFile(path, []byte(`{"_msg":"planted"}`+"\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatal(err)
	}

	e, err := newSpoolExporter(path)
	if err != nil {
		return // refusing the squatted default is an acceptable outcome
	}
	defer e.Shutdown(context.Background())
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("default fallback spool %s accepted with mode %04o; the documented default sink is owner-only (0600), and this path was chosen by the package, not by an operator", path, got)
	}
}

// Commit 7814b246 ("preserve explicit shared spool semantics") makes an
// existing operator override — say a 0664 group-shared file that several
// users' shells append to, per spool.go's own "several processes can share one
// spool without coordinating" — open without changing its mode. That holds
// until the first rotation: rotateIfLarge writes the survivor to a 0600 temp
// file and renames it over the shared spool, so the moment the bound binds the
// shared file becomes the rotating user's private one. Every other sharer's
// next bashy then fails openPrivateSpool with EACCES and — because init
// failures are always visible — prints "telemetry disabled" on every start.
// A property preserved at open and destroyed at 64 MiB is not preserved.
func TestRotationKeepsExplicitSharedSpoolShared(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission semantics")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "shared.jsonl")
	const want = os.FileMode(0o664)
	if err := os.WriteFile(path, []byte(`{"_msg":"first"}`+"\n"), want); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, want); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BASHY_OTEL_SPOOL", path)
	e, err := newSpoolExporter(SpoolPath())
	if err != nil {
		t.Fatalf("explicit shared spool rejected at open: %v", err)
	}
	defer e.Shutdown(context.Background())
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != want {
		t.Fatalf("open changed the shared spool's mode to %04o (want %04o); the rotation half of this test needs the open half to hold", got, want)
	}

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

	fi, err = os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() >= maxSpoolBytes {
		t.Fatalf("spool not rotated: size=%d", fi.Size())
	}
	if got := fi.Mode().Perm(); got != want {
		t.Errorf("rotation rewrote the explicit shared spool to %04o (want %04o): the other sharers can no longer append, and their next start prints \"telemetry disabled\"", got, want)
	}
}

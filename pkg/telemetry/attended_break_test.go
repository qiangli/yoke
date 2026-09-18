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

// scrubbedEnv returns the current environment with every automation marker,
// every BASHY_TELEMETRY_* switch and every OTEL_* variable removed, so a child
// sees exactly the environment the test describes and not the invoker's.
func scrubbedEnv() []string {
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

// Commit d626b9a8 ("distinguish harness PTYs from attended shells") decides
// "harness" from isolatedTerminalSession(): getsid(0) == getpid(). But that is
// not a property of harnesses — it is how EVERY terminal emulator on Linux
// (xterm, gnome-terminal/vte, alacritty, kitty), every tmux/screen pane and
// every `ssh -t` session starts the user's login shell: forkpty()/setsid(),
// then exec $SHELL. bashy is a shell. Run as the user's shell in any of those,
// bashy IS the session leader, stderr IS the human's terminal, no marker is
// set, and the "interactive stderr only" notice the run documents is
// suppressed for precisely the attended human it exists for. The same bashy
// under macOS Terminal.app is a child of login(1) (login is the leader — verified
// on this host: every ttys* shell has sid == login's pid, not its own), so the
// notice shows there and not on Linux: the answer to "is telemetry on?" now
// depends on which terminal program the person happens to use.
//
// The process configuration below — Setsid + Setctty on a fresh PTY, TERM=xterm,
// no markers — is byte-for-byte what tmux and sshd do for a human. It is also
// byte-for-byte what TestStartupNoticeStaysOutOfPTYDrivenAutomation constructs
// and demands be silent. Both cannot pass: session leadership carries no
// information about attendance, so a heuristic built on it can only choose
// which population to get wrong. This repo owns its harness (pkg/agentpty); a
// harness that wants quiet children can say so in their environment. A human
// at a terminal has no such lever.
func TestStartupNoticeReachesShellStartedByTerminalEmulator(t *testing.T) {
	if os.Getenv("TEST_TELEMETRY_LEADER_CHILD") == "1" {
		shutdown := Init(context.Background())
		_ = shutdown(context.Background())
		os.Exit(0)
	}
	spool := filepath.Join(t.TempDir(), "spans.jsonl")
	cmd := exec.Command(os.Args[0], "-test.run=^TestStartupNoticeReachesShellStartedByTerminalEmulator$")
	cmd.Env = append(scrubbedEnv(),
		"TERM=xterm",
		"TEST_TELEMETRY_LEADER_CHILD=1",
		"OTEL_TRACES_EXPORTER=file",
		"BASHY_OTEL_SPOOL="+spool,
	)
	// pty.Start == forkpty(): new session, new controlling terminal — the
	// login-shell shape.
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
	if !strings.Contains(out.String(), "telemetry on") {
		t.Errorf("a human's login shell (session leader on its own PTY, no automation marker) did not receive the startup notice; got:\n%q", out.String())
	}
}

// BASHY_OTEL_SPOOL=/dev/null is the universal "keep the feature, discard the
// output" idiom, and it worked on every release before this run: MkdirAll of
// /dev succeeded, O_APPEND on the null device succeeded, rotation saw size 0.
// openPrivateSpool now rejects anything that is not a regular file, and the
// rejection is an INIT FAILURE, which the run documents as "always visible".
// An operator who put that line in their profile therefore gets
//
//	bashy: telemetry disabled — spool: open spool: not a regular file
//
// on stderr from every bashy invocation, in every mode, including the captured
// agent logs this run set out to keep quiet — for a configuration that was
// valid yesterday. The symlink check that motivated the regular-file test is
// satisfied by Lstat alone; a character device the operator named on purpose
// is not a symlink attack.
func TestSpoolAcceptsNullDeviceOverride(t *testing.T) {
	if fi, err := os.Stat(os.DevNull); err != nil || fi.Mode().IsRegular() {
		t.Skipf("no null device on this host: %v", err)
	}
	t.Setenv("BASHY_OTEL_SPOOL", os.DevNull)
	e, err := newSpoolExporter(SpoolPath())
	if err != nil {
		t.Fatalf("BASHY_OTEL_SPOOL=%s rejected: %v (accepted by every release before this run)", os.DevNull, err)
	}
	_ = e.Shutdown(context.Background())
}

//go:build !windows

package agentpty

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Is a tool STEERABLE? The registry has been asserting an answer for months with
// nothing behind it — `supports_say: false` on codex, agy, aider and opencode was
// written down, never tested. That is the same class of unchecked claim as every
// other bug this fleet has found.
//
// This is the instrument that settles it, and it is deliberately the PRODUCTION
// path: agentpty.Run with a real control socket, and a real BrokerSay. If steering
// works here it works in `meet say`, because it IS `meet say`.
//
// Opt-in — it launches a real agent and spends real tokens:
//
//	BASHY_STEER_LIVE=1 go test ./pkg/agentpty -run TestSteerLive -v -timeout 10m
func TestSteerLive(t *testing.T) {
	if os.Getenv("BASHY_STEER_LIVE") == "" {
		t.Skip("set BASHY_STEER_LIVE=1 to launch a real agent")
	}
	tool := os.Getenv("STEER_TOOL")
	if tool == "" {
		t.Skip("set STEER_TOOL=agy|codex|claude")
	}

	var argv []string
	switch tool {
	case "agy":
		// -i: "Run an initial prompt interactively and CONTINUE THE SESSION."
		argv = []string{"agy", "-i", "--dangerously-skip-permissions",
			"--model", os.Getenv("STEER_MODEL"),
			"What is 2+2? Reply with only the number."}
	case "claude":
		argv = []string{"claude", "--dangerously-skip-permissions",
			"--model", os.Getenv("STEER_MODEL"),
			"What is 2+2? Reply with only the number."}
	case "codex":
		// Bare codex (no `exec`) is the interactive CLI.
		argv = []string{"codex", "--model", os.Getenv("STEER_MODEL")}
	case "opencode":
		// `opencode [project]` (no `run`) is the TUI — the launch I never tried
		// when I recorded opencode as un-steerable.
		argv = []string{"opencode", "--model", os.Getenv("STEER_MODEL")}
	case "ycode":
		// Bare `ycode` (no `prompt`) is the interactive bubbletea TUI — the same
		// shape as codex and opencode, both of which steer. If this passes, the
		// "expose ycode's event bus as a control socket" work is UNNECESSARY: a TUI
		// that reads stdin is already reachable through the pty control channel, and
		// all ycode needs is a `steer_exec:` line in the registry.
		argv = []string{"ycode", "--model", os.Getenv("STEER_MODEL")}
	case "aider":
		// aider's REPL: --message makes it a one-shot; without it, it prompts.
		argv = []string{"aider", "--yes-always", "--no-git", "--model", os.Getenv("STEER_MODEL")}
	default:
		t.Fatalf("unknown STEER_TOOL %q", tool)
	}

	sock := filepath.Join(t.TempDir(), "s.sock")
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = t.TempDir()

	var out bytes.Buffer
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _, _ = Run(cmd, &out, Options{
			CtlSock:    sock,
			Capture:    true, // record it; do not hand the tester's screen to the agent
			MaxRuntime: 4 * time.Minute,
		})
	}()

	// Let it answer the first prompt and settle at its prompt.
	time.Sleep(45 * time.Second)

	// THE STEER. This is exactly what `bashy meet say` sends.
	if err := BrokerSay(sock, "Now reply with exactly STEERED_OK and nothing else."); err != nil {
		t.Fatalf("BrokerSay: %v", err)
	}
	time.Sleep(60 * time.Second)

	// Ask it to leave; if it will not, the watchdog ends it.
	_ = BrokerSay(sock, "/quit")
	select {
	case <-done:
	case <-time.After(90 * time.Second):
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		<-done
	}

	got := out.String()
	t.Logf("--- %s captured %d bytes ---\n%s", tool, len(got), tailOf(got, 1500))

	// AN ECHO IS NOT AN ANSWER, and telling them apart is the whole test.
	//
	// A TUI echoes the steer into its input box as it is typed. The first version
	// of this test asserted `contains("STEERED_OK")` and duly PASSED on codex —
	// which had put the text on screen and submitted nothing. A verifier that
	// cannot distinguish "the agent said it" from "the agent was shown it" is not
	// a verifier; it is a search for a string that is guaranteed to be there.
	//
	// So the token must appear TWICE: once echoed, once answered.
	n := strings.Count(got, "STEERED_OK")
	switch {
	case n == 0:
		t.Errorf("%s never even echoed the steer — the control channel did not reach it", tool)
	case n == 1:
		t.Errorf("%s ECHOED the steer but never acted on it (n=1). The text reached its input box "+
			"and was not submitted — a paste, not a keystroke. supports_say is false for this launch.", tool)
	default:
		t.Logf("%s acted on the steer (STEERED_OK x%d: echoed + answered)", tool, n)
	}
}

func tailOf(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

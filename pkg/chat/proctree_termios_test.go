//go:build darwin

package chat

import (
	"context"
	"os"
	"os/exec"
	"testing"

	"github.com/creack/pty/v2"
	"golang.org/x/sys/unix"
)

const pipeTermiosHelperEnv = "COREUTILS_CHAT_PIPE_TERMIOS_HELPER"

// A pipe-run agent inherits no stdio terminal, but before Setsid it could still
// open the caller's controlling /dev/tty and alter it. Run the helper under a
// real PTY to exercise that case rather than relying on go test's usual pipes.
func TestPipeRunCannotMutateCallerTermios(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=TestPipeTermiosHelper")
	cmd.Env = append(os.Environ(), pipeTermiosHelperEnv+"=1")
	parent, err := pty.Start(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	setPipeExpectedTermios(t, parent)
	if _, err := parent.Write([]byte("x\n")); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("helper: %v", err)
	}
	assertPipeExpectedTermios(t, parent)
}

func TestPipeTermiosHelper(t *testing.T) {
	if os.Getenv(pipeTermiosHelperEnv) != "1" {
		return
	}
	var ready [1]byte
	if _, err := os.Stdin.Read(ready[:]); err != nil {
		os.Exit(2)
	}
	_, _, _ = execRunner{}.Run(context.Background(), "sh", []string{
		"-c", "stty -icanon -echo -opost < /dev/tty",
	}, "")
}

func setPipeExpectedTermios(t *testing.T, f *os.File) {
	t.Helper()
	state, err := unix.IoctlGetTermios(int(f.Fd()), unix.TIOCGETA)
	if err != nil {
		t.Fatal(err)
	}
	state.Oflag |= unix.OPOST | unix.ONLCR
	state.Lflag |= unix.ICANON | unix.ECHO
	if err := unix.IoctlSetTermios(int(f.Fd()), unix.TIOCSETA, state); err != nil {
		t.Fatal(err)
	}
}

func assertPipeExpectedTermios(t *testing.T, f *os.File) {
	t.Helper()
	state, err := unix.IoctlGetTermios(int(f.Fd()), unix.TIOCGETA)
	if err != nil {
		t.Fatal(err)
	}
	if state.Oflag&(unix.OPOST|unix.ONLCR) != unix.OPOST|unix.ONLCR {
		t.Fatalf("output flags = %#x, want OPOST|ONLCR", state.Oflag)
	}
	if state.Lflag&(unix.ICANON|unix.ECHO) != unix.ICANON|unix.ECHO {
		t.Fatalf("local flags = %#x, want ICANON|ECHO", state.Lflag)
	}
}

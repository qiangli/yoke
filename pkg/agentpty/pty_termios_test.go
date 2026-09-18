//go:build darwin

package agentpty

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/creack/pty/v2"
	"golang.org/x/sys/unix"
)

const ptyTermiosHelperEnv = "COREUTILS_AGENTPTY_TERMIOS_HELPER"

func TestPTYCancellationRestoresParentTermios(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=TestPTYTermiosHelper")
	cmd.Env = append(os.Environ(), ptyTermiosHelperEnv+"=1")
	parent, err := pty.Start(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	setExpectedTermios(t, parent)
	if _, err := parent.Write([]byte("x\n")); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("helper: %v", err)
	}
	assertExpectedTermios(t, parent)
}

func TestPTYTermiosHelper(t *testing.T) {
	if os.Getenv(ptyTermiosHelperEnv) != "1" {
		return
	}
	var ready [1]byte
	if _, err := os.Stdin.Read(ready[:]); err != nil {
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", "stty -icanon -echo -opost < /dev/tty; sleep 30")
	_, _, _ = Run(cmd, nil, Options{})
}

func setExpectedTermios(t *testing.T, f *os.File) {
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

func assertExpectedTermios(t *testing.T, f *os.File) {
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

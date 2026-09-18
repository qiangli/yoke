//go:build windows

package ctty

import (
	"os"
	"syscall"

	"golang.org/x/term"
)

// Open returns a handle on the console, bypassing stdio.
//
// Windows has no /dev/tty. The equivalent is the pair of console pseudo-files
// CONIN$ and CONOUT$, which resolve to the process's console regardless of how
// its standard handles were redirected — the same property that makes /dev/tty
// useful here. They are separate objects (input and output are distinct console
// buffers), so unlike unix this really is two opens.
//
// FILE_SHARE_READ|FILE_SHARE_WRITE is required: the console is already open in
// the parent, and an exclusive open would fail.
func Open() (*Terminal, error) {
	in, err := openConsole("CONIN$", syscall.GENERIC_READ|syscall.GENERIC_WRITE)
	if err != nil {
		return nil, ErrNoTTY
	}
	out, err := openConsole("CONOUT$", syscall.GENERIC_WRITE)
	if err != nil {
		in.Close()
		return nil, ErrNoTTY
	}
	if !term.IsTerminal(int(in.Fd())) {
		in.Close()
		out.Close()
		return nil, ErrNoTTY
	}
	// There is no TIOCGPGRP analogue: Windows has no process groups attached to a
	// console in the unix sense, so the foreground test that guards the unix path
	// has nothing to check here. A console we can open is one we may read.
	return &Terminal{in: in, out: out}, nil
}

func openConsole(name string, access uint32) (*os.File, error) {
	p, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	h, err := syscall.CreateFile(
		p,
		access,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE,
		nil,
		syscall.OPEN_EXISTING,
		0,
		0,
	)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(h), name), nil
}

// interruptSignals: Windows delivers Ctrl-C as os.Interrupt and supports SIGTERM
// as a synthetic signal. SIGHUP/SIGQUIT do not exist.
func interruptSignals() []os.Signal {
	return []os.Signal{os.Interrupt, syscall.SIGTERM}
}

// reRaise has no faithful Windows equivalent — there is no kill(getpid(), sig)
// that re-runs the default disposition. Exiting with the conventional 128+SIGINT
// status keeps the exit code meaningful to a shell that inspects it, which is the
// property the unix path is protecting.
func reRaise(os.Signal) {
	os.Exit(130)
}

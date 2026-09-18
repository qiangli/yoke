//go:build !windows

package ctty

import (
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// Open returns a handle on the controlling terminal, bypassing stdio entirely.
//
// Two guards, and both have bitten real programs:
//
//   - O_NOCTTY. Opening a terminal can ACQUIRE it as this process's controlling
//     terminal when it has none. A daemon that asks one question would then own a
//     terminal for the rest of its life and take its SIGHUP when that terminal
//     goes away. We want to talk to a terminal, not adopt one.
//
//   - The foreground process-group check below. Being able to OPEN /dev/tty says
//     nothing about being allowed to READ it.
func Open() (*Terminal, error) {
	f, err := os.OpenFile("/dev/tty", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		// ENXIO is the interesting case and the common one under an agentic
		// harness: the process was setsid'd, so it has no controlling terminal at
		// all. Every failure here means the same thing to a caller — try the next
		// channel — so they are not distinguished.
		return nil, ErrNoTTY
	}
	fd := int(f.Fd())
	if !term.IsTerminal(fd) {
		f.Close()
		return nil, ErrNoTTY
	}
	if !isForeground(fd) {
		f.Close()
		return nil, ErrNotForeground
	}
	return &Terminal{in: f, out: f}, nil
}

// isForeground reports whether we are the foreground process group of fd's
// terminal.
//
// This is the check that separates "a terminal exists" from "the human's
// keystrokes will come to me", and skipping it produces a genuinely nasty bug
// rather than a clean failure. Under an agent TUI the harness is sitting on that
// same terminal running its own read loop; if we read too, keystrokes go to
// whichever process wins the race and the harness repaints over our prompt. The
// user sees a prompt flicker and their password go somewhere unknown.
//
// TIOCGPGRP is the kernel's own answer to the question, and it is exactly the
// condition under which a read would otherwise raise SIGTTIN and stop us.
func isForeground(fd int) bool {
	pgrp, err := unix.IoctlGetInt(fd, unix.TIOCGPGRP)
	if err != nil {
		// Cannot establish it → assume not ours. Fail toward the rendezvous,
		// which is always safe, rather than toward a racing read that is not.
		return false
	}
	return pgrp == syscall.Getpgrp()
}

// interruptSignals are the ones that would otherwise leave the terminal in
// no-echo mode.
//
// SIGTSTP is deliberately absent. Handling suspend correctly means restoring the
// terminal on stop AND re-disabling echo on SIGCONT, and a half-implementation
// (restore, then resume reading with echo back on) would print the secret the user
// types after resuming. A known gap is safer than a partial fix; it is recorded in
// the design doc rather than papered over here.
func interruptSignals() []os.Signal {
	return []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT}
}

// reRaise re-delivers a signal to ourselves with the default disposition, so the
// process dies the way the signal says it should and the exit status tells the
// truth (130 for Ctrl-C, not a fabricated 1).
func reRaise(sig os.Signal) {
	s, ok := sig.(syscall.Signal)
	if !ok {
		os.Exit(1)
	}
	signal.Reset(s)
	_ = syscall.Kill(syscall.Getpid(), s)
	// Unreachable for the default dispositions above; a belt-and-braces exit in
	// case the signal was somehow ignored, so we never fall back into the read.
	os.Exit(128 + int(s))
}

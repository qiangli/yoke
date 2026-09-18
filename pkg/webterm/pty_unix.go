// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

//go:build !windows

package webterm

import (
	"os"
	"os/exec"
	"syscall"

	"github.com/creack/pty/v2"
)

// Supported reports whether this host can allocate a pseudo-terminal.
func Supported() bool { return true }

type session struct {
	pty *os.File
	cmd *exec.Cmd
}

func (s *session) Read(b []byte) (int, error)  { return s.pty.Read(b) }
func (s *session) Write(b []byte) (int, error) { return s.pty.Write(b) }

func start(opts Options, cols, rows uint16) (*session, error) {
	argv := opts.argv()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = opts.Dir
	cmd.Env = append(os.Environ(),
		"TERM=xterm-256color",
		// So the shell — and any agent running inside it — can tell it is being
		// driven from the browser console rather than a real tty.
		"BASHY_CONSOLE=1",
		// The telemetry banner is deliberate ("a feature that is silently on is as
		// hard to debug as one that is silently off") — but its audience is
		// someone wondering why a collector is empty, not someone who just opened
		// a terminal. Greeting every new shell with a diagnostic line trains the
		// reader to ignore the top of the screen, which is where real startup
		// errors appear. Telemetry still runs; it just stops announcing itself.
		"BASHY_TELEMETRY_QUIET=1",
	)
	cmd.Env = append(cmd.Env, opts.Env...)

	f, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: cols, Rows: rows})
	if err != nil {
		return nil, err
	}
	return &session{pty: f, cmd: cmd}, nil
}

func (s *session) resize(cols, rows uint16) error {
	return pty.Setsize(s.pty, &pty.Winsize{Cols: cols, Rows: rows})
}

func (s *session) wait() error { return s.cmd.Wait() }

// Close hangs the child up before killing it, so a shell with a chance to run
// its EXIT trap gets one, and a wedged one still goes away.
func (s *session) Close() error {
	if s.cmd.Process != nil {
		_ = s.cmd.Process.Signal(syscall.SIGHUP)
	}
	err := s.pty.Close()
	if s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
	return err
}

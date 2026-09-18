// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package webterm

import (
	"os"

	xpty "github.com/aymanbagabas/go-pty"
)

// Supported reports true: Windows gets a real pseudo-console.
//
// creack/pty (the unix backend) returns ErrUnsupported on Windows — its
// start_windows.go is a stub — and outpost's pipe-backed vPTY does not
// substitute, because it emulates a TTY for an IN-PROCESS runner and cannot give
// a child process a console. The working primitive is ConPTY
// (CreatePseudoConsole / UpdateProcThreadAttribute / ResizePseudoConsole), which
// github.com/aymanbagabas/go-pty wraps: MIT, and thin enough to read.
//
// It is a separate backend rather than a replacement for creack on every
// platform: the unix path is tested and working, and swapping it for a
// cross-platform abstraction would risk the case that works to tidy the case
// that did not exist.
func Supported() bool { return true }

type session struct {
	pty xpty.Pty
	cmd *xpty.Cmd
}

func start(opts Options, cols, rows uint16) (*session, error) {
	p, err := xpty.New()
	if err != nil {
		return nil, err
	}
	if err := p.Resize(int(cols), int(rows)); err != nil {
		_ = p.Close()
		return nil, err
	}

	argv := opts.argv()
	c := p.Command(argv[0], argv[1:]...)
	c.Dir = opts.Dir
	c.Env = append(os.Environ(),
		"TERM=xterm-256color",
		"BASHY_CONSOLE=1",
		// The telemetry banner is deliberate ("a feature that is silently on is as
		// hard to debug as one that is silently off") — but its audience is
		// someone wondering why a collector is empty, not someone who just opened
		// a terminal. Greeting every new shell with a diagnostic line trains the
		// reader to ignore the top of the screen, which is where real startup
		// errors appear. Telemetry still runs; it just stops announcing itself.
		"BASHY_TELEMETRY_QUIET=1",
	)
	c.Env = append(c.Env, opts.Env...)

	if err := c.Start(); err != nil {
		_ = p.Close()
		return nil, err
	}
	return &session{pty: p, cmd: c}, nil
}

func (s *session) Read(b []byte) (int, error)  { return s.pty.Read(b) }
func (s *session) Write(b []byte) (int, error) { return s.pty.Write(b) }

func (s *session) resize(cols, rows uint16) error {
	return s.pty.Resize(int(cols), int(rows))
}

func (s *session) wait() error { return s.cmd.Wait() }

// Close drops the console. There is no SIGHUP on Windows, so closing the
// pseudo-console IS the hangup — the attached process sees its console go away.
func (s *session) Close() error { return s.pty.Close() }

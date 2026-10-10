// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package dag

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"golang.org/x/term"
	"mvdan.cc/sh/v3/interp"
)

// signalCause is the context.Cause of a run cancelled by SIGINT/SIGTERM.
type signalCause struct {
	sig os.Signal
	at  time.Time
}

func (c signalCause) Error() string { return fmt.Sprintf("interrupted by %s", c.sig) }

// ExitCode follows the shell convention 128+signal (130 INT, 143 TERM).
func (c signalCause) ExitCode() int {
	if s, ok := c.sig.(syscall.Signal); ok {
		return 128 + int(s)
	}
	return 130
}

// runSignalContext cancels the returned context, with a signalCause, on the
// first SIGINT/SIGTERM. A bare cobra context ignores signals, so without this
// a TERM killed the dag process outright and left its bodies' process trees
// running.
func runSignalContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancelCause(parent)
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		select {
		case s := <-ch:
			cancel(signalCause{sig: s, at: time.Now()})
		case <-ctx.Done():
		}
	}()
	return ctx, func() {
		signal.Stop(ch)
		cancel(context.Canceled)
	}
}

func cancelCause(ctx context.Context) (signalCause, bool) {
	if ctx.Err() == nil {
		return signalCause{}, false
	}
	c, ok := context.Cause(ctx).(signalCause)
	return c, ok
}

// ownProcessGroups reports whether external commands in a body should each
// lead their own process group, so a cancel signals the whole tree. With a
// terminal attached they stay in the shared group: the terminal delivers ^C to
// all of it, and a prompt on /dev/tty needs a foreground group.
func ownProcessGroups(tio TaskIO) bool {
	for _, f := range []any{tio.Stdin, tio.Stdout, tio.Stderr} {
		if u, ok := f.(interface{ Unwrap() io.Writer }); ok {
			f = u.Unwrap()
		}
		if file, ok := f.(*os.File); ok && term.IsTerminal(int(file.Fd())) {
			return false
		}
	}
	return true
}

// execKillGrace is the interpreter's cancel-to-SIGKILL delay for an external
// command (the sh fork's default). The cancel first sends only SIGINT, which a
// shell's async children ignore, and the SIGKILL that reaps the group fires on
// a timer inside this process.
const execKillGrace = 2 * time.Second

// settleCancelledExec keeps the process alive until that SIGKILL has fired.
// Without it a shell that exits on the first signal lets dag exit at once and
// the timer dies with it, orphaning the rest of the group (which then carries
// on, e.g. a fence loop launching its next task). It must sit innermost in the
// ExecHandlers chain so it only wraps real external commands.
func settleCancelledExec(on bool) func(interp.ExecHandlerFunc) interp.ExecHandlerFunc {
	return func(next interp.ExecHandlerFunc) interp.ExecHandlerFunc {
		if !on || runtime.GOOS == "windows" {
			return next
		}
		return func(ctx context.Context, args []string) error {
			err := next(ctx, args)
			if c, ok := cancelCause(ctx); ok {
				time.Sleep(time.Until(c.at.Add(execKillGrace + 100*time.Millisecond)))
			}
			return err
		}
	}
}

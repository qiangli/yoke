//go:build windows

// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package agentpty

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	xpty "github.com/aymanbagabas/go-pty"
	"golang.org/x/sys/windows"
	"golang.org/x/term"
)

// enableVT turns on virtual-terminal processing for the wrapper's own console
// output, so the escape sequences the pseudo-console emits render instead of
// printing as text. Returns the restore func (no-op when nothing changed).
func enableVT(f *os.File) func() {
	h := windows.Handle(f.Fd())
	var mode uint32
	if err := windows.GetConsoleMode(h, &mode); err != nil {
		return func() {}
	}
	want := mode | windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING | windows.DISABLE_NEWLINE_AUTO_RETURN
	if want == mode || windows.SetConsoleMode(h, want) != nil {
		return func() {}
	}
	return func() { _ = windows.SetConsoleMode(h, mode) }
}

// Supported reports true: Windows gets a real pseudo-console.
//
// Until sprint 220 (story ad5b0de9) this was false and every caller fell
// back to a plain exec — so `bashy chat --agent codex` on a Windows host
// refused ("an interactive session needs a pty, which this platform has no
// support for") and no bashy-launched seat could exist there at all. The
// working primitive is ConPTY, which github.com/aymanbagabas/go-pty wraps;
// pkg/webterm already drives the apps console's terminal with it.
func Supported() bool { return true }

// Run is the ConPTY twin of the unix Run: the same control-line protocol
// (control.go), the same watchdogs, the same stdin/stdout routing. What
// differs is the substrate — a pseudo-console instead of a pty pair, Job
// Object-free tree termination (the console's process tree is killed through
// taskkill), and no signal forwarding (Windows has no SIGTERM to forward; ^C
// at the wrapper reaches the child through the shared console).
func Run(cmd *exec.Cmd, logSink io.Writer, opts Options) (int, string, error) {
	rows, cols := ptySize()
	p, err := xpty.New()
	if err != nil {
		return 127, "", fmt.Errorf("conpty: %w", err)
	}
	if err := p.Resize(int(cols), int(rows)); err != nil {
		_ = p.Close()
		return 127, "", fmt.Errorf("conpty resize: %w", err)
	}
	if opts.OnResize != nil {
		opts.OnResize(rows, cols)
	}
	if len(cmd.Args) == 0 {
		_ = p.Close()
		return 127, "", fmt.Errorf("agentpty: no command")
	}
	xc := p.Command(cmd.Path, cmd.Args[1:]...)
	xc.Dir = cmd.Dir
	xc.Env = cmd.Env
	if xc.Env == nil {
		xc.Env = os.Environ()
	}

	var killOnce sync.Once
	var killReason atomic.Value
	killTree := func(reason string, grace time.Duration) {
		killOnce.Do(func() {
			killReason.Store(reason)
			if xc.Process == nil {
				return
			}
			pid := xc.Process.Pid
			slog.Warn("agentpty: terminating subagent tree", "pid", pid, "reason", reason)
			if logSink != nil {
				fmt.Fprintf(logSink, "\r\n[agent] terminating subagent: %s\r\n", reason)
			}
			// taskkill /T walks the tree the console started; /F because a TUI
			// ignores the polite close. The grace window is for the child to
			// flush; Windows has no TERM→KILL ladder to honour.
			go func() {
				time.Sleep(grace)
				_ = exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid)).Run()
			}()
		})
	}
	if cmd.Cancel != nil {
		cmd.Cancel = func() error {
			killTree("context cancelled", 2*time.Second)
			return nil
		}
	}

	if err := xc.Start(); err != nil {
		_ = p.Close()
		return 127, "", fmt.Errorf("conpty start: %w", err)
	}
	// Callers read cmd.Process.Pid (room cards, kill paths); hand them the
	// real child.
	cmd.Process = xc.Process
	if opts.OnStart != nil {
		if err := opts.OnStart(p.Name()); err != nil {
			killTree("pty registration failed", 2*time.Second)
			_ = p.Close()
			_ = xc.Wait()
			return 127, "pty registration failed", err
		}
	}
	defer p.Close()

	// ^C at the wrapper: forward as a tree kill, as the unix side does for
	// SIGINT/SIGTERM. (A console ^C also reaches the child directly; the
	// forward is the guarantee for a backgrounded wrapper.)
	intSigs := make(chan os.Signal, 1)
	signal.Notify(intSigs, os.Interrupt)
	go func() {
		for s := range intSigs {
			killTree(fmt.Sprintf("signal %v forwarded from wrapper", s), 2*time.Second)
		}
	}()
	defer func() {
		signal.Stop(intSigs)
		close(intSigs)
	}()

	var lastWriteUnixNs atomic.Int64
	lastWriteUnixNs.Store(time.Now().UnixNano())
	watchdogStop := make(chan struct{})
	defer close(watchdogStop)
	if opts.Activity != nil {
		go func() {
			for {
				select {
				case <-watchdogStop:
					return
				case _, ok := <-opts.Activity:
					if !ok {
						return
					}
					lastWriteUnixNs.Store(time.Now().UnixNano())
				}
			}
		}()
	}
	if opts.IdleTimeout > 0 {
		go func() {
			ticker := time.NewTicker(opts.IdleTimeout / 4)
			defer ticker.Stop()
			for {
				select {
				case <-watchdogStop:
					return
				case <-ticker.C:
					last := time.Unix(0, lastWriteUnixNs.Load())
					if time.Since(last) >= opts.IdleTimeout {
						killTree(fmt.Sprintf("idle %s exceeds --idle-timeout %s",
							time.Since(last).Round(time.Second), opts.IdleTimeout), 10*time.Second)
						return
					}
				}
			}
		}()
	}
	if opts.MaxRuntime > 0 {
		go func() {
			deadlineUnix := time.Now().Add(opts.MaxRuntime).Unix()
			interval := opts.MaxRuntime / 10
			if interval <= 0 || interval > 30*time.Second {
				interval = 30 * time.Second
			}
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-watchdogStop:
					return
				case <-ticker.C:
					if time.Now().Unix() >= deadlineUnix {
						killTree(fmt.Sprintf("runtime exceeds --max-runtime %s (wall-clock)", opts.MaxRuntime), 10*time.Second)
						return
					}
				}
			}
		}()
	}
	if opts.MemLimitBytes > 0 {
		// The unix backend sums the tree's RSS from `ps`; there is no ps here
		// and the Win32 walk is not written yet. Say so once rather than
		// pretend a limit is enforced.
		slog.Warn("agentpty: --mem-limit is not enforced on Windows yet", "limit_mb", opts.MemLimitBytes>>20)
	}

	// Control socket: Windows 10 1803+ speaks AF_UNIX, and Go's net package
	// dials it; the file-tail fallback covers an older host.
	if opts.CtlSock != "" {
		_ = os.Remove(opts.CtlSock)
		if ln, lnErr := net.Listen("unix", opts.CtlSock); lnErr == nil {
			defer func() {
				_ = ln.Close()
				_ = os.Remove(opts.CtlSock)
			}()
			go func() {
				for {
					conn, acceptErr := ln.Accept()
					if acceptErr != nil {
						return
					}
					go func(c net.Conn) {
						defer c.Close()
						sc := newPTYControlScanner(c)
						for sc.Scan() {
							writePTYControlLine(p, sc.Text())
						}
					}(conn)
				}
			}()
		} else if f, err := os.OpenFile(opts.CtlSock, os.O_CREATE|os.O_RDONLY, 0o600); err == nil {
			_ = f.Close()
			defer func() { _ = os.Remove(opts.CtlSock) }()
			go tailPTYControlFile(opts.CtlSock, p)
			slog.Warn("agentpty: control socket unavailable; using file control fallback", "path", opts.CtlSock, "err", lnErr)
		} else {
			slog.Warn("agentpty: control socket unavailable; steering disabled for this run", "path", opts.CtlSock, "err", lnErr)
		}
	}

	parentTTY := term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
	bump := func(n int) {
		if n > 0 {
			lastWriteUnixNs.Store(time.Now().UnixNano())
		}
	}
	tap := func(w io.Writer) io.Writer { return &activityTap{w: w, bump: bump} }
	trustTap := func(w io.Writer) io.Writer {
		if opts.CtlSock == "" {
			return w
		}
		return newTrustClearTap(w, opts.CtlSock)
	}

	if parentTTY && !opts.Capture {
		// Raw mode on the INPUT handle (on Windows the console modes live on
		// stdin); the pseudo-console renders to stdout through the copy below.
		oldState, rawErr := term.MakeRaw(int(os.Stdin.Fd()))
		if rawErr != nil {
			fmt.Fprintf(os.Stderr, "agentpty: term.MakeRaw: %v\n", rawErr)
		} else {
			defer func() { _ = term.Restore(int(os.Stdin.Fd()), oldState) }()
		}
		defer enableVT(os.Stdout)()
		dst := io.Writer(os.Stdout)
		if logSink != nil {
			dst = io.MultiWriter(os.Stdout, logSink)
		}
		go func() { _, _ = io.Copy(p, os.Stdin) }()
		// Resize follows the console: there is no SIGWINCH, so poll.
		go func() {
			ticker := time.NewTicker(500 * time.Millisecond)
			defer ticker.Stop()
			lastW, lastH := int(cols), int(rows)
			for {
				select {
				case <-watchdogStop:
					return
				case <-ticker.C:
					if w, h, err := term.GetSize(int(os.Stdout.Fd())); err == nil && (w != lastW || h != lastH) {
						lastW, lastH = w, h
						_ = p.Resize(w, h)
					}
				}
			}
		}()
		_, _ = io.Copy(tap(trustTap(dst)), p)
	} else {
		if logSink == nil {
			logSink = io.Discard
		}
		sink, flush := opts.filter(logSink)
		_, _ = io.Copy(tap(trustTap(sink)), p)
		if flush != nil {
			_ = flush()
		}
	}

	waitErr := xc.Wait()
	reason, _ := killReason.Load().(string)
	if xc.ProcessState != nil {
		if code := xc.ProcessState.ExitCode(); code >= 0 {
			return code, reason, nil
		}
	}
	if waitErr != nil {
		return 1, reason, waitErr
	}
	return 0, reason, nil
}

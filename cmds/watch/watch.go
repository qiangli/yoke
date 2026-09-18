// Package watchcmd implements watch(1): run a command periodically.
package watchcmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/qiangli/coreutils/tool"
)

var cmd = &tool.Tool{
	Name:     "watch",
	Synopsis: "Execute a program periodically, showing output fullscreen.",
	Usage:    "watch [OPTION]... CMD [ARGS...]",
}

func init() { cmd.Run = run; tool.Register(cmd) }

func run(rc *tool.RunContext, args []string) int {
	fs := tool.NewFlags(cmd.Name)
	interval := fs.Float64P("interval", "n", 2.0, "seconds to wait between updates")
	noTitle := fs.BoolP("no-title", "t", false, "turn off the header")
	diff := fs.BoolP("differences", "d", false, "highlight changes between updates")
	errexit := fs.BoolP("errexit", "e", false, "exit if CMD exits with a non-zero status")
	chgexit := fs.BoolP("chgexit", "g", false, "exit when CMD output changes")
	execDirect := fs.BoolP("exec", "x", false, "pass CMD directly to exec instead of a shell")
	operands, code := tool.Parse(rc, cmd, fs, args)
	if code >= 0 {
		return code
	}
	if len(operands) == 0 {
		return tool.UsageError(rc, cmd, "missing command")
	}
	if *interval <= 0 {
		return tool.UsageError(rc, cmd, "interval must be greater than zero")
	}

	opts := watchOptions{
		interval:   time.Duration(*interval * float64(time.Second)),
		noTitle:    *noTitle,
		diff:       *diff,
		errexit:    *errexit,
		chgexit:    *chgexit,
		execDirect: *execDirect,
		argv:       operands,
		tty:        stdoutIsTTY(rc.Out),
		now:        time.Now,
		runCommand: execWatchCommand,
	}
	return watchLoop(rc, opts)
}

type watchOptions struct {
	interval   time.Duration
	noTitle    bool
	diff       bool
	errexit    bool
	chgexit    bool
	execDirect bool
	argv       []string
	tty        bool
	maxCycles  int
	now        func() time.Time
	runCommand func(context.Context, *tool.RunContext, watchOptions) ([]byte, int, error)
	sleep      func(context.Context, time.Duration) bool
}

func watchLoop(rc *tool.RunContext, opts watchOptions) int {
	if opts.now == nil {
		opts.now = time.Now
	}
	if opts.runCommand == nil {
		opts.runCommand = execWatchCommand
	}
	if opts.sleep == nil {
		opts.sleep = sleepContext
	}
	ctx := rc.Ctx
	if ctx == nil {
		ctx = context.Background()
	}

	var prev []byte
	for cycle := 0; opts.maxCycles <= 0 || cycle < opts.maxCycles; cycle++ {
		out, exitCode, err := opts.runCommand(ctx, rc, opts)
		if err != nil && errors.Is(ctx.Err(), context.Canceled) {
			return 130
		}
		if opts.tty {
			fmt.Fprint(rc.Out, "\x1b[H\x1b[2J")
		}
		renderWatch(rc, opts, out, prev)
		if opts.errexit && exitCode != 0 {
			return 8
		}
		if opts.chgexit && cycle > 0 && !bytes.Equal(out, prev) {
			return 0
		}
		prev = append(prev[:0], out...)
		if opts.maxCycles > 0 && cycle == opts.maxCycles-1 {
			break
		}
		if !opts.sleep(ctx, opts.interval) {
			return 130
		}
	}
	return 0
}

func renderWatch(rc *tool.RunContext, opts watchOptions, out, prev []byte) {
	if !opts.noTitle {
		host := rc.Getenv("HOSTNAME")
		if host == "" {
			host, _ = os.Hostname()
		}
		fmt.Fprintf(rc.Out, "Every %s: %s    %s: %s\n\n", formatInterval(opts.interval), commandString(opts.argv), host, opts.now().Format("Mon Jan _2 15:04:05 2006"))
	}
	if opts.diff && prev != nil {
		rc.Out.Write(highlightChanges(out, prev))
		return
	}
	rc.Out.Write(out)
}

func execWatchCommand(ctx context.Context, rc *tool.RunContext, opts watchOptions) ([]byte, int, error) {
	var c *exec.Cmd
	if opts.execDirect {
		path := rc.ResolveExecutable(opts.argv[0])
		c = exec.CommandContext(ctx, path, opts.argv[1:]...)
	} else if runtime.GOOS == "windows" {
		c = exec.CommandContext(ctx, "cmd", "/c", commandString(opts.argv))
	} else {
		c = exec.CommandContext(ctx, "sh", "-c", commandString(opts.argv))
	}
	c.Dir = rc.Dir
	c.Env = rc.Env
	out, err := c.CombinedOutput()
	if err == nil {
		return out, 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return out, exitErr.ExitCode(), err
	}
	return out, 1, err
}

func sleepContext(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func commandString(argv []string) string {
	return strings.Join(argv, " ")
}

func formatInterval(d time.Duration) string {
	if d%time.Second == 0 {
		return strconv.FormatInt(int64(d/time.Second), 10) + "s"
	}
	s := strconv.FormatFloat(d.Seconds(), 'f', -1, 64)
	return s + "s"
}

func highlightChanges(out, prev []byte) []byte {
	var b bytes.Buffer
	for i, c := range out {
		changed := i >= len(prev) || prev[i] != c
		if changed {
			b.WriteString("\x1b[7m")
			b.WriteByte(c)
			b.WriteString("\x1b[0m")
		} else {
			b.WriteByte(c)
		}
	}
	return b.Bytes()
}

func stdoutIsTTY(w any) bool {
	f, ok := w.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

//go:build !windows

package agentpty

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/creack/pty/v2"
	"github.com/qiangli/yoke/pkg/procguard"
	"golang.org/x/term"
)

// wrapperKillSignals are the signals the wrapper forwards to the subagent tree
// as a deliberate termination — the `weave kill`/`abandon` path (SIGTERM) and an
// interactive ^C (SIGINT). SIGHUP is intentionally absent: see wrapperIgnoreSignals.
var wrapperKillSignals = []os.Signal{syscall.SIGTERM, syscall.SIGINT}

// wrapperIgnoreSignals are the signals the wrapper intercepts and DROPS so they
// neither kill the subagent nor terminate the wrapper by default disposition.
// SIGHUP lives here because it means "the launcher/terminal that started me went
// away" — an orchestrator exit/restart/idle-timeout — which must not kill a
// worker that is actively producing.
var wrapperIgnoreSignals = []os.Signal{syscall.SIGHUP}

// Supported reports whether this host can run an agent under a PTY. The Windows
// build returns false and callers fall back to a plain exec.
func Supported() bool { return true }

// Run launches cmd attached to a freshly-allocated PTY.
//
// Stdin/stdout routing depends on the parent's terminal:
//   - parent stdin IS a TTY: switch to raw mode, bidirectionally
//     copy stdin↔PTY so the user types into the subagent's TUI and
//     sees it render normally. SIGWINCH propagates terminal resizes.
//   - parent stdin is NOT a TTY (orchestrator pipe, backgrounded
//     by shell &): logSink receives the PTY output verbatim and
//     subagent stdin is fed from /dev/null. The orchestrator's
//     pipes are not held open by us — the subagent thinks it has
//     a controlling terminal even though no human is attached.
//
// logSink is only used in the non-TTY path; pass nil for the TTY
// pass-through case. guards carries the three watchdog tripwires
// (idle, wall-clock, memory) — see Options. Returns the
// subagent's exit code (or 128+N when killed by signal N, matching
// the wrap helper), the first wrapper-initiated kill reason, if any.
func Run(cmd *exec.Cmd, logSink io.Writer, opts Options) (int, string, error) {
	rows, cols := ptySize()

	// killTree terminates the subagent and every transitive child.
	// pty.Start put the subagent in its own session (it must be a
	// session leader to own the PTY), so the wrapper's process
	// group does NOT contain it — and the subagent's descendants
	// (claude's MCP servers, shell shims) may setpgid themselves
	// out of the subagent's group. Signalling only -pid therefore
	// strands grandchildren; the dogfood OOM left an orphaned
	// claude tree running for 15+ minutes after `weave abandon`.
	// We signal the group AND each descendant from a `ps` snapshot,
	// then escalate to SIGKILL after a grace window.
	var killOnce sync.Once
	var killReason atomic.Value
	killTree := func(reason string, grace time.Duration) {
		killOnce.Do(func() {
			killReason.Store(reason)
			if cmd.Process == nil {
				return
			}
			pid := cmd.Process.Pid
			slog.Warn("agentpty: terminating subagent tree", "pid", pid, "reason", reason)
			if logSink != nil {
				fmt.Fprintf(logSink, "\r\n[agent] terminating subagent: %s\r\n", reason)
				// Forensic snapshot while `ps` still works: the
				// 2026-06 OOM post-mortems had no record of which
				// process actually held the memory. Tree-local AND
				// system-wide, so a culprit outside the subagent
				// tree is still named.
				if tree, system := forensicSnapshot(pid); tree != "" {
					fmt.Fprintf(logSink, "[agent] top tree procs:   %s\r\n", tree)
					fmt.Fprintf(logSink, "[agent] top system procs: %s\r\n", system)
				}
			}
			pids := procTreePids(pid)
			_ = syscall.Kill(-pid, syscall.SIGTERM)
			for _, p := range pids {
				_ = syscall.Kill(p, syscall.SIGTERM)
			}
			go func() {
				time.Sleep(grace)
				if syscall.Kill(pid, 0) != nil {
					return // leader reaped; Wait() unblocks
				}
				_ = syscall.Kill(-pid, syscall.SIGKILL)
				for _, p := range procTreePids(pid) {
					_ = syscall.Kill(p, syscall.SIGKILL)
				}
			}()
		})
	}
	// CommandContext otherwise kills only the immediate child. The PTY child is
	// a session leader, so that can leave descendants holding its slave open;
	// io.Copy then cannot return and a foreground caller stays in raw mode.
	// Install this before Start: os/exec captures Cancel during Start when it
	// arms the context watcher.
	if cmd.Cancel != nil {
		cmd.Cancel = func() error {
			killTree("context cancelled", 2*time.Second)
			return nil
		}
	}

	ptmx, tty, err := pty.Open()
	if err != nil {
		return 127, "", fmt.Errorf("pty.Open: %w", err)
	}
	ttyName := tty.Name()
	if err := pty.Setsize(ptmx, &pty.Winsize{Rows: rows, Cols: cols}); err != nil {
		_ = tty.Close()
		_ = ptmx.Close()
		return 127, "", fmt.Errorf("pty.Setsize: %w", err)
	}
	if cmd.Stdout == nil {
		cmd.Stdout = tty
	}
	if cmd.Stderr == nil {
		cmd.Stderr = tty
	}
	if cmd.Stdin == nil {
		cmd.Stdin = tty
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
	cmd.SysProcAttr.Setctty = true
	var parentGuard *procguard.Guard
	if opts.KillOnParentExit {
		parentGuard, err = procguard.Arm(cmd)
		if err != nil {
			_ = tty.Close()
			_ = ptmx.Close()
			return 127, "", err
		}
	}
	startErr := cmd.Start()
	if parentGuard != nil {
		parentGuard.Started(startErr)
	}
	if startErr != nil {
		_ = tty.Close()
		_ = ptmx.Close()
		return 127, "", fmt.Errorf("pty.Start: %w", startErr)
	}
	_ = tty.Close()
	if opts.OnStart != nil {
		if err := opts.OnStart(ttyName); err != nil {
			killTree("pty registration failed", 2*time.Second)
			_ = ptmx.Close()
			_ = cmd.Wait()
			if parentGuard != nil {
				parentGuard.Disarm()
			}
			return 127, "pty registration failed", err
		}
	}
	defer ptmx.Close()

	// Forward EXPLICIT termination signals (SIGTERM/SIGINT) from the wrapper
	// to the subagent tree. Without this, SIGTERM kills the wrapper instantly
	// (default disposition) and the subagent — in its own session — survives
	// as an orphan that `weave kill`/`abandon` can never reach again. Short
	// grace: weaveStopWrapper SIGKILLs the wrapper 5s after its SIGTERM, and
	// our escalation goroutine must fire before we die.
	termSigs := make(chan os.Signal, 1)
	signal.Notify(termSigs, wrapperKillSignals...)
	go func() {
		for s := range termSigs {
			killTree(fmt.Sprintf("signal %v forwarded from wrapper", s), 2*time.Second)
		}
	}()

	// SIGHUP is deliberately NOT a kill signal. SIGHUP is the launcher /
	// controlling-terminal death signal: the orchestrator (weave autopilot,
	// itself spawned by weave heartbeat) that launched this worker exiting,
	// restarting, idle-timing-out, or its terminal closing delivers SIGHUP to
	// a still-attached worker. Forwarding it — OR letting the wrapper's default
	// SIGHUP disposition (terminate) stand — kills an actively-working subagent
	// whose only sin is that its launcher went away, producing the "killed, ~0
	// commits" orphan the fleet kept losing runs to. We intercept SIGHUP and
	// DROP it so an in-flight worker outlives the death of whatever launched it;
	// an explicit `weave kill`/`abandon` still reaches it via SIGTERM.
	hupSigs := make(chan os.Signal, 1)
	signal.Notify(hupSigs, wrapperIgnoreSignals...)
	go func() {
		for range hupSigs {
			slog.Info("agentpty: ignoring SIGHUP (launcher/terminal death); subagent keeps working", "pid", func() int {
				if cmd.Process != nil {
					return cmd.Process.Pid
				}
				return 0
			}())
		}
	}()
	defer func() {
		// Stop must precede close — the signal package does a
		// non-blocking send to registered channels, and a send on
		// a closed channel panics.
		signal.Stop(termSigs)
		close(termSigs)
		signal.Stop(hupSigs)
		close(hupSigs)
	}()

	// Idle-timeout watchdog: tracks the time of the most recent PTY
	// write. A background goroutine kills the subagent tree if that
	// timestamp stalls past idleTimeout. The watchdog reads the
	// timestamp via atomic load; the copy loop bumps it on every
	// write.
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

	// Wall-clock watchdog: --idle-timeout is useless against a
	// runaway TUI whose spinner keeps emitting output; this one
	// cannot be reset by activity.
	//
	// Uses a REAL wall-clock deadline (time.Now().Unix()), NOT
	// time.After/a monotonic timer. Monotonic timers PAUSE while the
	// host is asleep (laptop suspend), so a run that spans a sleep
	// would never hit its "hard wall-clock ceiling" in real elapsed
	// time — the dogfood hit exactly this: a 35m ceiling went
	// unenforced across an overnight suspend (the subagent sat
	// "working" for 40m+ of wall clock having accrued only minutes of
	// awake time). The poll ticker also pauses during sleep, but on
	// wake it re-checks within `interval` and catches the overrun, so
	// the ceiling fires within `interval` of the true deadline even
	// across a suspend.
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

	// Memory watchdog: sums RSS across the subagent's process tree
	// every poll and kills the tree over budget. This is the OOM
	// backstop — whatever leaks (orphan storms, runaway builds, a
	// buggy interpreter under test), the agent dies at its budget
	// instead of taking down the machine.
	if opts.MemLimitBytes > 0 {
		go func() {
			ticker := time.NewTicker(5 * time.Second)
			defer ticker.Stop()
			// Growth trail: log to the issue log each time the tree's
			// RSS doubles past 256MB. If a runaway dodges the limit
			// (or the machine dies before a kill lands), the log
			// still shows when the ballooning started and how fast
			// it grew.
			var lastLogged int64 = 256 << 20
			for {
				select {
				case <-watchdogStop:
					return
				case <-ticker.C:
					if cmd.Process == nil {
						continue
					}
					rss := procTreeRSSBytes(cmd.Process.Pid)
					if rss > opts.MemLimitBytes {
						killTree(fmt.Sprintf("process-tree RSS %dMB exceeds --mem-limit %dMB",
							rss>>20, opts.MemLimitBytes>>20), 10*time.Second)
						return
					}
					if rss >= lastLogged*2 {
						lastLogged = rss
						slog.Warn("agentpty: subagent tree RSS growing", "pid", cmd.Process.Pid, "rss_mb", rss>>20)
						if logSink != nil {
							tree, system := forensicSnapshot(cmd.Process.Pid)
							fmt.Fprintf(logSink, "\r\n[agent] tree RSS %dMB (limit %dMB) — top tree: %s | top system: %s\r\n",
								rss>>20, opts.MemLimitBytes>>20, tree, system)
						}
					}
				}
			}
		}()
	}

	// Control socket for `weave say`: every line received becomes
	// keystrokes on the PTY master (trailing \r = Enter, which the
	// TUI's line discipline reads as submit). Serving it in both
	// the captured and pass-through paths costs nothing; writes to
	// *os.File are serialized by the kernel for these small sizes.
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
						return // listener closed at tool exit
					}
					go func(c net.Conn) {
						defer c.Close()
						sc := newPTYControlScanner(c)
						for sc.Scan() {
							writePTYControlLine(ptmx, sc.Text())
						}
					}(conn)
				}
			}()
		} else {
			if f, err := os.OpenFile(opts.CtlSock, os.O_CREATE|os.O_RDONLY, 0o600); err == nil {
				_ = f.Close()
				defer func() { _ = os.Remove(opts.CtlSock) }()
				go tailPTYControlFile(opts.CtlSock, ptmx)
				slog.Warn("agentpty: control socket unavailable; using file control fallback", "path", opts.CtlSock, "err", lnErr)
			} else {
				slog.Warn("agentpty: control socket unavailable; `weave say` disabled for this run", "path", opts.CtlSock, "err", lnErr)
			}
		}
	}
	parentTTY := term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))

	// Forward SIGWINCH so terminal resizes propagate into the subagent's
	// PTY. Even in the non-TTY path we install the handler — it costs
	// nothing and means a manual SIGWINCH (rare) still works.
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	winchDone := make(chan struct{})
	go func() {
		defer close(winchDone)
		for range winch {
			if parentTTY {
				_ = pty.InheritSize(os.Stdout, ptmx)
			}
		}
	}()
	defer func() {
		signal.Stop(winch)
		close(winch)
		<-winchDone
	}()

	var (
		oldState    *term.State
		restoreOnce sync.Once
	)
	restore := func() {
		if oldState != nil {
			restoreOnce.Do(func() { _ = term.Restore(int(os.Stdout.Fd()), oldState) })
		}
	}
	defer restore()

	// activityTap wraps an io.Writer and bumps lastWriteUnixNs on
	// every successful write. The idle-timeout watchdog reads that
	// timestamp.
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
		// Raw mode so the user's keystrokes go straight to the
		// subagent's TTY. Goroutine for stdin→PTY (os.Stdin reads
		// block); PTY→stdout in the foreground (blocks until child
		// closes the slave).
		oldState, err = term.MakeRaw(int(os.Stdout.Fd()))
		if err != nil {
			fmt.Fprintf(os.Stderr, "agentpty: term.MakeRaw: %v\n", err)
		}
		// A human at the keyboard sees the tool's native TUI on os.Stdout.
		// When a logSink is also supplied, tee the raw PTY bytes to it too —
		// that capture is what makes a foreground, human-driven session
		// OBSERVABLE and ATTACHABLE (`chat attach`/`observe`) instead of a
		// black box. Without it, an interactive session steers (the ctlsock
		// works in both branches) but nobody else can watch it. The capture
		// is raw ANSI, so it follows and steers but is not a clean transcript
		// — headless capture (Capture:true) remains the path for that.
		dst := io.Writer(os.Stdout)
		if logSink != nil {
			dst = io.MultiWriter(os.Stdout, logSink)
		}
		go func() { _, _ = io.Copy(ptmx, os.Stdin) }()
		_, _ = io.Copy(tap(trustTap(dst)), ptmx)
	} else {
		// Non-TTY parent (orchestrator pipe / backgrounded by `cmd &`).
		// Subagent gets a PTY but stdin is closed; PTY output is
		// captured to logSink (typically a per-issue log file under
		// the queue dir). We deliberately do NOT copy to os.Stdout —
		// that would feed the subagent's TUI output back into the
		// orchestrator's pipe, the exact pattern that caused the
		// recent OOM incident.
		if logSink == nil {
			logSink = io.Discard
		}
		// The caller decides what the output IS — weave decodes stream-json into
		// a worker log, a meeting streams the raw lines to whoever is watching.
		sink, flush := opts.filter(logSink)
		_, _ = io.Copy(tap(trustTap(sink)), ptmx)
		if flush != nil {
			_ = flush()
		}
	}

	waitErr := cmd.Wait()
	if parentGuard != nil {
		parentGuard.Disarm()
	}
	restore()

	reason, _ := killReason.Load().(string)
	switch e := waitErr.(type) {
	case nil:
		return 0, reason, nil
	case *exec.ExitError:
		if status, ok := e.Sys().(syscall.WaitStatus); ok {
			if status.Signaled() {
				return 128 + int(status.Signal()), reason, nil
			}
			return status.ExitStatus(), reason, nil
		}
		return 1, reason, nil
	default:
		if errors.Is(waitErr, io.EOF) {
			return 0, reason, nil
		}
		return 1, reason, waitErr
	}
}

// procSnapshot returns the child-map, RSS (bytes), and command
// name per PID from one `ps` pass. Shelling out to ps is deliberate:
// it's portable across macOS and Linux (no /proc on darwin), and the
// watchdogs poll at multi-second intervals where a fork is noise.
func procSnapshot() (children map[int][]int, rss map[int]int64, comm map[int]string) {
	out, err := exec.Command("ps", "-axo", "pid=,ppid=,rss=,comm=").Output()
	if err != nil {
		return nil, nil, nil
	}
	children = make(map[int][]int)
	rss = make(map[int]int64)
	comm = make(map[int]string)
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		kb, err3 := strconv.ParseInt(f[2], 10, 64)
		if err1 != nil || err2 != nil || err3 != nil {
			continue
		}
		children[ppid] = append(children[ppid], pid)
		rss[pid] = kb << 10
		comm[pid] = strings.Join(f[3:], " ")
	}
	return children, rss, comm
}

// topProcs formats the n highest-RSS processes among pids as
// one log-friendly line: "pid:comm=rssMB pid:comm=rssMB ...".
func topProcs(pids []int, rss map[int]int64, comm map[int]string, n int) string {
	sorted := append([]int(nil), pids...)
	sort.Slice(sorted, func(i, j int) bool { return rss[sorted[i]] > rss[sorted[j]] })
	if len(sorted) > n {
		sorted = sorted[:n]
	}
	parts := make([]string, 0, len(sorted))
	for _, p := range sorted {
		name := comm[p]
		if i := strings.LastIndexByte(name, '/'); i >= 0 {
			name = name[i+1:]
		}
		parts = append(parts, fmt.Sprintf("%d:%s=%dMB", p, name, rss[p]>>20))
	}
	return strings.Join(parts, " ")
}

// forensicSnapshot returns two lines for the issue log: the
// top-RSS processes inside the subagent tree, and system-wide. The
// system-wide line is the one that catches a culprit OUTSIDE the
// tree (the 2026-06 OOMs were attributed to a VSCode process while
// per-process stats in Activity Monitor were already unreadable —
// this snapshot is taken while `ps` still works).
func forensicSnapshot(root int) (tree string, system string) {
	children, rss, comm := procSnapshot()
	if children == nil {
		return "", ""
	}
	all := make([]int, 0, len(rss))
	for p := range rss {
		all = append(all, p)
	}
	return topProcs(descend(root, children), rss, comm, 5),
		topProcs(all, rss, comm, 5)
}

// descend walks the snapshot from root, breadth-first, and
// returns root plus every transitive descendant.
func descend(root int, children map[int][]int) []int {
	pids := []int{root}
	seen := map[int]bool{root: true}
	for i := 0; i < len(pids); i++ {
		for _, c := range children[pids[i]] {
			if !seen[c] {
				seen[c] = true
				pids = append(pids, c)
			}
		}
	}
	return pids
}

// procTreePids returns root + all transitive children. Best
// effort: processes that double-fork and reparent to init escape
// the tree (they also escape the process group; nothing short of
// cgroups catches those, and macOS has none).
func procTreePids(root int) []int {
	children, _, _ := procSnapshot()
	if children == nil {
		return []int{root}
	}
	return descend(root, children)
}

// procTreeRSSBytes sums resident memory across the subagent's
// process tree.
func procTreeRSSBytes(root int) int64 {
	children, rss, _ := procSnapshot()
	if children == nil {
		return 0
	}
	var total int64
	for _, p := range descend(root, children) {
		total += rss[p]
	}
	return total
}

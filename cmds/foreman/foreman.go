package foremancmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/qiangli/coreutils/tool"
	"github.com/qiangli/yoke/pkg/chat"
	"github.com/qiangli/yoke/pkg/foreman"
)

var cmd = &tool.Tool{
	Name:     "foreman",
	Synopsis: "Drive a persistent, steerable agent session.",
	Usage:    "foreman start [--detach] [--yolo] [--max-runtime 30m|--no-max-runtime] --goal TEXT [--agent AGENT]\n   or: foreman tell <id> TEXT\n   or: foreman status [--wait DURATION] [--after SEQ] [--watch] [--json] <id>\n   or: foreman log <id> [-f]\n   or: foreman interrupt <id>   (ESC — breaks a tool loop)\n   or: foreman list\n   or: foreman --once --agent AGENT --instruction TEXT",
}

const defaultForemanMaxRuntime = 30 * time.Minute

var runner chat.Runner

func init() { cmd.Run = run; tool.Register(cmd) }

func run(rc *tool.RunContext, args []string) int {
	if rc.Ctx == nil {
		rc.Ctx = context.Background()
	}
	if len(args) == 0 {
		return runREPLWithFlags(rc, nil, nil)
	}
	// --help/-h and --version short-circuit before the manual flag parser,
	// which would otherwise treat "--help" as a value-taking flag and drop
	// into the interactive REPL (no rest args). The framework does not strip
	// these because this tool does its own arg dispatch.
	for _, a := range args {
		switch a {
		case "--help", "-h":
			fmt.Fprintln(rc.Out, cmd.Usage)
			return 0
		case "--version":
			fmt.Fprintf(rc.Out, "foreman %s\n", tool.Version)
			return 0
		}
	}
	global, rest := parseKVFlags(args)
	if global["once"] == "true" {
		return runOnce(rc, global)
	}
	if len(rest) == 0 {
		return runREPLWithFlags(rc, global, nil)
	}
	sub := rest[0]
	subFlags, subArgs := parseKVFlags(rest[1:])
	jsonOut := global["json"] == "true" || subFlags["json"] == "true"
	switch sub {
	case "start":
		return runStart(rc, subFlags, subArgs, jsonOut)
	case "serve":
		return runServe(rc, subArgs)
	case "tell":
		return runTell(rc, subArgs, jsonOut)
	case "status":
		return runStatus(rc, subFlags, subArgs, jsonOut)
	case "log":
		return runLog(rc, subFlags, subArgs)
	case "interrupt":
		return runKey(rc, subArgs, foreman.KeyEsc, jsonOut)
	case "key":
		return runKeyNamed(rc, subArgs, jsonOut)
	case "list":
		return runList(rc, jsonOut)
	case "pause", "resume", "skip", "stop":
		return runControl(rc, sub, subArgs, jsonOut)
	case "prio":
		return runPrio(rc, subArgs, jsonOut)
	case "run":
		if len(subArgs) > 0 && strings.HasSuffix(strings.ToLower(subArgs[0]), ".md") {
			return runDAG(rc, subFlags, subArgs, jsonOut)
		}
		return runREPLWithFlags(rc, subFlags, subArgs)
	default:
		return usage(rc, "unknown subcommand %q", sub)
	}
}

func runOnce(rc *tool.RunContext, flags map[string]string) int {
	res, err := chat.Invoke(rc.Ctx, chat.Options{
		Agent:       flags["agent"],
		Role:        flags["role"],
		Instruction: flags["instruction"],
		Cwd:         rc.Dir,
		JSON:        flags["json"] == "true",
		AllowUnsafe: flags["yolo"] == "true",
	}, runner)
	if flags["json"] == "true" {
		return emitJSON(rc, res)
	}
	if res.Output != "" {
		fmt.Fprint(rc.Out, res.Output)
		if !strings.HasSuffix(res.Output, "\n") {
			fmt.Fprintln(rc.Out)
		}
	}
	if err != nil {
		fmt.Fprintln(rc.Err, err)
	}
	return res.ExitCode
}

func runStart(rc *tool.RunContext, flags map[string]string, args []string, jsonOut bool) int {
	goal := flags["goal"]
	if goal == "" && len(args) > 0 {
		goal = strings.Join(args, " ")
	}
	maxRuntime, err := parseForemanMaxRuntime(flags)
	if err != nil {
		return fail(rc, jsonOut, err)
	}
	s, err := foreman.Start(rc.Ctx, foreman.Options{
		ID:              flags["id"],
		Goal:            goal,
		Agent:           flags["agent"],
		Role:            flags["role"],
		Cwd:             rc.Dir,
		MaxRuntime:      maxRuntime,
		Runner:          runner,
		OpeningSendOnce: flags["opening-send-once"] == "true",
		AllowUnsafe:     flags["yolo"] == "true",
	})
	if err != nil {
		return fail(rc, jsonOut, err)
	}
	if flags["detach"] == "true" {
		if err := spawnServe(s.State().ID); err != nil {
			return fail(rc, jsonOut, err)
		}
	}
	if jsonOut {
		return emitJSON(rc, s.State())
	}
	fmt.Fprintln(rc.Out, s.State().ID)
	if flags["detach"] != "true" {
		defer s.Close()
		ready := make(chan string, 1)
		go func() { <-ready }()
		if err := s.ServeControl(rc.Ctx, ready); err != nil {
			return fail(rc, jsonOut, err)
		}
	}
	return 0
}

func runServe(rc *tool.RunContext, args []string) int {
	if len(args) != 1 {
		return usage(rc, "serve requires id")
	}
	s, err := foreman.Open("", args[0], runner)
	if err != nil {
		return fail(rc, false, err)
	}
	defer s.Close()
	if err := s.ServeControl(rc.Ctx, nil); err != nil {
		return fail(rc, false, err)
	}
	return 0
}

func parseForemanMaxRuntime(flags map[string]string) (time.Duration, error) {
	if flags["no-max-runtime"] == "true" {
		if _, also := flags["max-runtime"]; also {
			return 0, fmt.Errorf("foreman: --no-max-runtime and --max-runtime cannot be used together")
		}
		return 0, nil
	}
	raw, ok := flags["max-runtime"]
	if !ok {
		return defaultForemanMaxRuntime, nil
	}
	d, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("foreman: --max-runtime must be a positive duration (for example 15m)")
	}
	return d, nil
}

func runTell(rc *tool.RunContext, args []string, jsonOut bool) int {
	if len(args) < 2 {
		return usage(rc, "tell requires id and message")
	}
	id, msg := args[0], strings.Join(args[1:], " ")
	ack, err := foreman.Tell("", id, msg)
	if err != nil {
		return fail(rc, jsonOut, err)
	}
	// SAY WHICH ONE HAPPENED. "steered" means the line landed on an agent that was
	// working; "accepted" means there was nobody there and it starts a fresh turn
	// instead. Those are different acts, and an operator who typed a correction
	// needs to know which one they got -- reporting both as a bare "ok" is the
	// precise lie this control plane was built to stop telling.
	delivery := "accepted (no live agent — this STARTS a turn)"
	if ack.Steered {
		delivery = "steered (delivered to the running agent, mid-turn)"
	}
	if !jsonOut {
		fmt.Fprintf(rc.Out, "→ %s: %s\n", id, delivery)
	}
	return ok(rc, jsonOut, map[string]any{
		"id": id, "sent": msg, "steered": ack.Steered, "accepted": ack.Accepted,
	})
}

// runLog shows what the agent is actually SAYING.
//
// Without it a detached foreman was a black box: `status` told you it was
// `working` and nothing at all about what it was working ON. That makes steering
// useless in practice, because you steer when you SEE the agent going wrong.
func runLog(rc *tool.RunContext, flags map[string]string, args []string) int {
	if len(args) != 1 {
		return usage(rc, "log requires id")
	}
	path := foreman.NewStore("", args[0]).LogPath()
	f, err := os.Open(path)
	if err != nil {
		return fail(rc, false, fmt.Errorf("no log for %s yet (the agent may not have started): %w", args[0], err))
	}
	defer f.Close()
	if _, err := io.Copy(rc.Out, f); err != nil {
		return fail(rc, false, err)
	}
	if flags["f"] != "true" && flags["follow"] != "true" {
		return 0
	}
	// Follow. A turn runs for minutes; an operator watching for the moment to
	// intervene needs the stream, not a snapshot.
	for {
		select {
		case <-rc.Ctx.Done():
			return 0
		case <-time.After(500 * time.Millisecond):
		}
		if _, err := io.Copy(rc.Out, f); err != nil {
			return 0
		}
	}
}

// runKey presses a key at the running agent.
//
// `foreman tell` says something to the agent; every agent TUI in this fleet queues
// it and reads it when the current turn ends. That is the right behaviour for a
// course correction and completely useless for an agent stuck in a tool loop,
// because that turn is never going to end. Escape is the only thing that reaches
// it. (Measured: an agy conductor made 224 tool calls of which 22 were distinct.)
func runKey(rc *tool.RunContext, args []string, key string, jsonOut bool) int {
	if len(args) != 1 {
		return usage(rc, "interrupt requires id")
	}
	if _, err := foreman.SendCommand("", args[0], foreman.Command{Verb: foreman.CommandKey, Message: key}); err != nil {
		return fail(rc, jsonOut, err)
	}
	if !jsonOut {
		fmt.Fprintf(rc.Out, "→ %s: pressed %s at the running agent\n", args[0], key)
	}
	return ok(rc, jsonOut, map[string]any{"id": args[0], "key": key})
}

func runKeyNamed(rc *tool.RunContext, args []string, jsonOut bool) int {
	if len(args) != 2 {
		return usage(rc, "key requires id and one of: esc, enter, ctrl-c")
	}
	return runKey(rc, args[:1], args[1], jsonOut)
}

// runStatus is the supervision read.
//
// Plain `status <id>` is one bounded snapshot: the state row, which now carries
// `seq` and `digest` in JSON so the caller has a cursor. The waiting forms are
// the state-change contract (foreman.Store.Changes / WaitChanges):
//
//	--after SEQ      every transition after the cursor, immediately — a
//	                 restarted supervisor catches up on what it missed instead
//	                 of reading the latest state and guessing
//	--wait DURATION  block until a transition lands after the cursor (default
//	                 cursor: the current seq, i.e. the NEXT change) or the bound
//	                 elapses; a timeout prints nothing and exits 0, the same
//	                 convention as `inbox --wait`, so an unchanged healthy
//	                 session is no payload at all
//	--watch          stream transitions until the context ends; with --json
//	                 that is NDJSON, one record per change
//
// Human rows are `id  seq  prev->status  step  blocker`; JSON is one
// bashy-foreman-transition-v1 object per line and nothing else on stdout.
func runStatus(rc *tool.RunContext, flags map[string]string, args []string, jsonOut bool) int {
	if len(args) != 1 {
		return usage(rc, "status requires id")
	}
	store := foreman.NewStore("", args[0])
	_, waiting := flags["wait"]
	_, hasAfter := flags["after"]
	watch := flags["watch"] == "true"
	if !waiting && !hasAfter && !watch {
		st, err := store.LoadState()
		if err != nil {
			return fail(rc, jsonOut, err)
		}
		if jsonOut {
			return emitJSON(rc, st)
		}
		fmt.Fprintf(rc.Out, "%s\t%s\t%s\n", st.ID, st.Status, st.Goal)
		return 0
	}
	var bound time.Duration
	if waiting {
		d, err := time.ParseDuration(strings.TrimSpace(flags["wait"]))
		if err != nil || d < 0 {
			return usage(rc, "--wait must be a non-negative duration (for example 30s)")
		}
		bound = d
	}
	var after int64
	if hasAfter {
		n, err := strconv.ParseInt(strings.TrimSpace(flags["after"]), 10, 64)
		if err != nil || n < 0 {
			return usage(rc, "--after must be a non-negative sequence number")
		}
		after = n
	} else {
		// No cursor: the caller wants the NEXT change, not a replay of the
		// session's whole life.
		st, err := store.LoadState()
		if err != nil {
			return fail(rc, jsonOut, err)
		}
		after = st.Seq
	}
	emit := func(trs []foreman.Transition) {
		for _, tr := range trs {
			if jsonOut {
				emitJSON(rc, tr)
				continue
			}
			prev := tr.PreviousStatus
			if prev == "" {
				prev = "-"
			}
			fmt.Fprintf(rc.Out, "%s\t%d\t%s->%s\t%s\t%s\n", tr.ID, tr.Seq, prev, tr.Status, tr.CurrentStep, tr.Blocker)
		}
	}
	if !watch {
		trs, err := store.WaitChanges(rc.Ctx, after, bound)
		if err != nil {
			if rc.Ctx.Err() != nil {
				return 0
			}
			return fail(rc, jsonOut, err)
		}
		emit(trs)
		return 0
	}
	// --watch: a stream. The bound, when given, is how long one quiet stretch
	// may last before we re-arm; without one, wait in long slices forever.
	slice := bound
	if slice <= 0 {
		slice = time.Hour
	}
	for {
		trs, err := store.WaitChanges(rc.Ctx, after, slice)
		if err != nil {
			if rc.Ctx.Err() != nil {
				return 0
			}
			return fail(rc, jsonOut, err)
		}
		emit(trs)
		if n := len(trs); n > 0 {
			after = trs[n-1].Seq
		}
		if rc.Ctx.Err() != nil {
			return 0
		}
	}
}

func runList(rc *tool.RunContext, jsonOut bool) int {
	items, err := foreman.List("")
	if err != nil {
		return fail(rc, jsonOut, err)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	if jsonOut {
		return emitJSON(rc, items)
	}
	for _, st := range items {
		fmt.Fprintf(rc.Out, "%s\t%s\t%s\n", st.ID, st.Status, st.Goal)
	}
	return 0
}

func runControl(rc *tool.RunContext, verb string, args []string, jsonOut bool) int {
	if len(args) < 1 || len(args) > 2 {
		return usage(rc, "%s requires id", verb)
	}
	cmd := foreman.Command{Verb: verb}
	if len(args) == 2 {
		cmd.Target = args[1]
	}
	if _, err := foreman.SendCommand("", args[0], cmd); err != nil {
		return fail(rc, jsonOut, err)
	}
	return ok(rc, jsonOut, map[string]any{"id": args[0], "verb": verb})
}

func runPrio(rc *tool.RunContext, args []string, jsonOut bool) int {
	if len(args) < 2 || len(args) > 3 {
		return usage(rc, "prio requires id [target] priority")
	}
	c := foreman.Command{Verb: foreman.CommandPrio, Priority: args[len(args)-1]}
	if len(args) == 3 {
		c.Target = args[1]
	}
	if _, err := foreman.SendCommand("", args[0], c); err != nil {
		return fail(rc, jsonOut, err)
	}
	return ok(rc, jsonOut, map[string]any{"id": args[0], "priority": args[len(args)-1]})
}

func runDAG(rc *tool.RunContext, flags map[string]string, args []string, jsonOut bool) int {
	path := rc.Path(args[0])
	targets := args[1:]
	goal := flags["goal"]
	if goal == "" {
		goal = "run " + args[0]
	}
	s, err := foreman.Start(rc.Ctx, foreman.Options{
		ID:     flags["id"],
		Goal:   goal,
		Agent:  flags["agent"],
		Role:   flags["role"],
		Cwd:    rc.Dir,
		Runner: runner,
	})
	if err != nil {
		return fail(rc, jsonOut, err)
	}
	report, err := s.RunDAG(rc.Ctx, foreman.DAGOptions{Path: path, Targets: targets})
	if err != nil {
		return fail(rc, jsonOut, err)
	}
	if jsonOut {
		return emitJSON(rc, report)
	}
	for _, name := range report.Targets {
		fmt.Fprintln(rc.Out, name)
	}
	return 0
}

func spawnServe(id string) error {
	if os.Getenv("BASHY_FOREMAN_NO_SPAWN") != "" {
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	base := strings.TrimSuffix(filepath.Base(exe), ".exe")
	args := []string{exe}
	// Any multicall HOST binary needs the `foreman` verb prepended; only a binary
	// that IS foreman takes `serve` directly.
	//
	// This used to check `base == "coreutils"`, which meant that under `bashy` —
	// the binary everyone actually runs — it spawned `bashy serve <id>`. That is a
	// real command, and a completely different one: the warm-session server. So
	// `foreman start --detach` cheerfully launched the wrong daemon, never created
	// a control socket, and every subsequent `foreman tell` died on
	// "dial unix …/ctl.sock: no such file or directory".
	//
	// Naming the ONE case that is right, instead of guessing at the set that is
	// wrong, is what keeps a third multicall host from re-breaking this.
	if base != "foreman" {
		args = append(args, "foreman")
	}
	args = append(args, "serve", id)
	serve := exec.Command(exe, args[1:]...)
	serve.Env = os.Environ()
	serve.SysProcAttr = foremanDetachedSysProcAttr()
	// Nil stdio maps to the null device. Inheriting the launcher's terminal
	// made the detached daemon die from SIGHUP as soon as a noninteractive
	// `bashy foreman start --detach` returned.
	if err := serve.Start(); err != nil {
		return err
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = serve.Wait()
	}()
	return nil
}

func parseKVFlags(args []string) (map[string]string, []string) {
	flags := map[string]string{}
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "--") || a == "--" {
			rest = append(rest, args[i:]...)
			break
		}
		name := strings.TrimPrefix(a, "--")
		if strings.Contains(name, "=") {
			parts := strings.SplitN(name, "=", 2)
			flags[parts[0]] = parts[1]
			continue
		}
		switch name {
		case "json", "once", "detach", "watch", "no-max-runtime", "opening-send-once", "yolo":
			flags[name] = "true"
		default:
			if i+1 >= len(args) {
				flags[name] = ""
				continue
			}
			flags[name] = args[i+1]
			i++
		}
	}
	return flags, rest
}

func usage(rc *tool.RunContext, format string, a ...any) int {
	fmt.Fprintf(rc.Err, "foreman: "+format+"\n", a...)
	fmt.Fprintln(rc.Err, cmd.Usage)
	return 2
}

func fail(rc *tool.RunContext, jsonOut bool, err error) int {
	if jsonOut {
		return emitJSON(rc, map[string]any{"ok": false, "error": err.Error()})
	}
	fmt.Fprintln(rc.Err, "foreman:", err)
	return 1
}

func ok(rc *tool.RunContext, jsonOut bool, v map[string]any) int {
	if jsonOut {
		v["ok"] = true
		return emitJSON(rc, v)
	}
	return 0
}

func emitJSON(rc *tool.RunContext, v any) int {
	data, _ := json.Marshal(v)
	fmt.Fprintln(rc.Out, string(data))
	return 0
}

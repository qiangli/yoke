package watchcmd

// AGENTIC MODE — the same `watch`, told what to notice.
//
// Classic watch answers "show me this command again every N seconds", and a
// human reads the screen. An agent cannot read a screen: it needs to be told
// WHICH CHANGE matters, and it needs to be able to block until that change
// happens. That is the whole difference, and it is why this is the same verb
// rather than a watch subcommand on every command — see docs/watch-design.md.
//
// Nothing here runs unless an agentic flag is given. With none, run() takes the
// classic path, byte for byte.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/spf13/pflag"

	"github.com/qiangli/coreutils/tool"

	"github.com/qiangli/yoke/pkg/watch"
	// hostsrc registers the mb and sprint-thread sources and the mb-reply join.
	"github.com/qiangli/yoke/pkg/watch/hostsrc"
)

// agenticFlags are the options that select agentic mode, and the one place that
// list lives: `run` decides which path to take by asking whether any of them
// was given.
var agenticFlags = []string{
	"json", "key", "locator", "match", "change", "idle", "heartbeat", "expect",
	"reply-within", "source", "backfill", "rules", "preset", "list-presets",
	"inbox", "from", "until", "timeout", "renotify", "allow-empty",
	"task", "quiet-window",
	"probe", "hook", "list-hooks", "hook-source",
}

type agenticOptions struct {
	jsonOut     bool
	keys        []string
	locator     string
	task        string
	quietWindow time.Duration
	match       []string
	change      []string
	idle        time.Duration
	heartbeat   time.Duration
	expect      []string
	replyWithin time.Duration
	sources     []string
	backfill    time.Duration
	rules       string
	preset      string
	listPresets bool
	probes      []string
	hooks       []string
	listHooks   bool
	hookSource  string
	inbox       []string
	from        string
	until       bool
	timeout     time.Duration
	renotify    time.Duration
	allowEmpty  bool
}

func registerAgenticFlags(fs *pflag.FlagSet, o *agenticOptions) {
	fs.BoolVar(&o.jsonOut, "json", false, "emit one "+watch.SchemaVersion+" alert per line on stdout (NDJSON)")
	fs.StringSliceVar(&o.keys, "key", nil, "record identity for the structured diff (default id/name/key/seq/path)")
	fs.StringVar(&o.locator, "locator", "", "name the watched resource (sprint:480, run:repo-7, @host, a path)")
	fs.StringVar(&o.task, "task", "", "declare the watched task's class: run, session, service, job, schedule, command (idle never flags a command)")
	fs.DurationVar(&o.quietWindow, "quiet-window", 0, "declare how long the watched task may legitimately be quiet (scales both idle rungs)")
	fs.StringArrayVar(&o.match, "match", nil, "alarm on events matching field=status,after=done,text=RE,attr.to=operator (repeatable)")
	fs.StringSliceVar(&o.change, "change", nil, "alarm on any change of these fields")
	fs.DurationVar(&o.idle, "idle", 0, "alarm when a resource shows no activity for this long (the inverse operator)")
	fs.DurationVar(&o.heartbeat, "heartbeat", 0, "alarm when an expected report does not arrive within this long")
	fs.StringSliceVar(&o.expect, "expect", nil, "locators that must report, armed from the start (use with --heartbeat)")
	fs.DurationVar(&o.replyWithin, "reply-within", 0, "alarm when a directed message is not answered within this long")
	fs.StringSliceVar(&o.sources, "source", nil, "subscribe to a journal: mb[:recipient], sprint-thread[:N]; with --preset/--rules it REPLACES that file's source(s) of the same kind")
	fs.DurationVar(&o.backfill, "backfill", 0, "report records already present that are younger than this (the window is announced on stderr at start)")
	fs.StringVar(&o.rules, "rules", "", "read rules from a "+watch.RulesSchemaVersion+" file")
	fs.StringVar(&o.preset, "preset", "", "start from a named rule file (see --list-presets)")
	fs.BoolVar(&o.listPresets, "list-presets", false, "list the available presets and exit")
	fs.StringArrayVar(&o.probes, "probe", nil, "sample a resource with a Bash# probe hook, by asset name or path (repeatable)")
	fs.StringArrayVar(&o.hooks, "hook", nil, "run a Bash# action hook on every alert, by asset name or path (repeatable); for a fixed argv, a when-filter or a destructive hook, use a rule file's sinks")
	fs.BoolVar(&o.listHooks, "list-hooks", false, "list the available hook scripts and exit")
	fs.StringVar(&o.hookSource, "hook-source", "", "print a hook script, so a shipped example can be copied into the local ring")
	fs.StringSliceVar(&o.inbox, "inbox", nil, "post every alert to these recipients or seats")
	fs.StringVar(&o.from, "from", "", "sender for --inbox (default: this agent's board identity)")
	fs.BoolVar(&o.until, "until", false, "exit 0 on the first alert; exit 1 if --timeout elapses first")
	fs.DurationVar(&o.timeout, "timeout", 0, "give up after this long (exit 1)")
	fs.DurationVar(&o.renotify, "renotify", 0, "restate a condition that persists this long")
	fs.BoolVar(&o.allowEmpty, "allow-empty", false, "read an empty snapshot literally, as zero records")
}

func agenticSelected(fs *pflag.FlagSet) bool {
	for _, name := range agenticFlags {
		if f := fs.Lookup(name); f != nil && f.Changed {
			return true
		}
	}
	return false
}

// runAgentic builds the plan from the flags and runs the engine.
func runAgentic(rc *tool.RunContext, opts watchOptions, o *agenticOptions) int {
	if o.listPresets {
		return listPresets(rc)
	}
	if o.listHooks {
		return listHooks(rc)
	}
	if o.hookSource != "" {
		return printHookSource(rc, o.hookSource)
	}

	plan, code := buildPlan(rc, opts, o)
	if code >= 0 {
		return code
	}
	if len(plan.Sources) == 0 {
		return tool.UsageError(rc, cmd, "nothing to watch: give a command after --, or a --source/--rules/--preset")
	}
	if len(plan.Rules) == 0 {
		return tool.UsageError(rc, cmd, "nothing to alarm on: give --match/--change/--idle/--heartbeat/--reply-within, or --rules/--preset")
	}

	var sinks []watch.Sink
	if o.jsonOut {
		// NDJSON goes to STDOUT because it is the result; the console line goes
		// to stderr because stdout belongs to the watched command. Emitting
		// both would make the stream neither.
		sinks = append(sinks, watch.JSONSink{W: rc.Out})
	} else {
		sinks = append(sinks, watch.ConsoleSink{W: rc.Err})
	}
	sinks = append(sinks, plan.Sinks...)
	if len(o.inbox) > 0 {
		from := o.from
		if from == "" {
			from = hostsrc.SenderIdentity()
		}
		if strings.TrimSpace(from) == "" {
			return tool.UsageError(rc, cmd, "--inbox needs a sender: pass --from")
		}
		sinks = append(sinks, hostsrc.NewInboxSink(from, o.inbox, "watch"))
	}

	// A backfilling watcher opens with the RECENT PAST, and unannounced those
	// first lines read exactly like a burst of live activity. Say the window
	// once, before the first cycle, so nobody acts on history as if it were news.
	if w := watch.BackfillWindow(plan.Sources); w > 0 {
		fmt.Fprintf(rc.Err, "watch: backfill: reporting entries younger than %s\n", w)
	}

	ctx := rc.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// -e in agentic mode: a command that exits non-zero ends the watch with the
	// classic code, rather than being reported as a source error forever.
	commandFailed := false
	// -n wins over a rule file's interval when it was given; otherwise the rule
	// file's own pacing applies, so a preset can say 60s without every caller
	// having to repeat it.
	interval := plan.Interval
	if interval <= 0 || opts.intervalSet {
		interval = opts.interval
	}
	renotify := plan.Renotify
	if o.renotify > 0 {
		renotify = o.renotify
	}
	// A condition that persists is RESTATED on the documented cadence. Without
	// this an --idle watch reports the stall once and then goes quiet, which
	// reads exactly like a watcher that stopped.
	if renotify <= 0 && (o.idle > 0 || o.heartbeat > 0) {
		renotify = watch.DefaultRenotify
	}
	timeout := plan.Timeout
	if o.timeout > 0 {
		timeout = o.timeout
	}

	engine := watch.New(watch.Config{
		Sources:  plan.Sources,
		Rules:    plan.Rules,
		Sinks:    sinks,
		Interval: interval,
		Renotify: renotify,
		Until:    o.until,
		Timeout:  timeout,
		OnError: func(source string, err error) {
			if opts.errexit && isCommandFailure(err) {
				commandFailed = true
				cancel()
				return
			}
			// Report it, never swallow it: a source that cannot read its store
			// is exactly the condition a watcher must not hide, and it is also
			// not a reason to stop watching everything else.
			fmt.Fprintf(rc.Err, "watch: %s: %v\n", source, err)
		},
	})
	// A failing hook is reported in the ALERT STREAM, not just as a diagnostic:
	// a hook that quietly did not run leaves the operator believing it did.
	// Routing it through the engine gets it the same dedupe as any condition.
	for _, s := range sinks {
		if hook, ok := s.(*watch.HookSink); ok {
			hook.Alarm = func(a watch.Alert) { engine.Raise(a) }
		}
	}
	for _, src := range plan.Sources {
		if hook, ok := src.(*watch.HookSource); ok {
			hook.Alarm = func(a watch.Alert) { engine.Raise(a) }
		}
	}

	code = engine.Run(ctx)
	if commandFailed {
		return watch.ExitCommandFailed
	}
	return code
}

func isCommandFailure(err error) bool {
	return err != nil && strings.Contains(err.Error(), "command exited")
}

func buildPlan(rc *tool.RunContext, opts watchOptions, o *agenticOptions) (watch.Plan, int) {
	var plan watch.Plan

	if o.preset != "" && o.rules != "" {
		return plan, tool.UsageError(rc, cmd, "--preset and --rules are two ways to say the same thing; pass one")
	}
	file, label, haveFile, code := ruleFile(rc, o)
	if code >= 0 {
		return plan, code
	}

	// The command line's --source selections NARROW the rule file rather than
	// joining it: naming a source of a kind replaces the file's own source(s)
	// of that kind. Otherwise `--preset sprint --source sprint-thread:472`
	// keeps the preset's unfiltered thread source and reports every card on
	// the host beside the one that was asked for.
	selections := make([]watch.SourceSpec, 0, len(o.sources))
	for _, spec := range o.sources {
		sel := namedSourceSpec(spec, o.backfill)
		// A typo on the command line is a usage error whether or not a rule
		// file is also in play, and it is refused BEFORE the narrowing: a
		// selection the host cannot build must not quietly drop the file's
		// source of the kind it misspelled.
		if !watch.KnownSourceType(sel.Type) {
			fmt.Fprintf(rc.Err, "%v\n", watch.UnknownSourceTypeError(sel.Type))
			return plan, 2
		}
		selections = append(selections, sel)
	}
	if haveFile {
		built, err := file.NarrowSources(selections).Build(buildOptions(rc, opts))
		if err != nil {
			fmt.Fprintf(rc.Err, "watch: %s: %v\n", label, err)
			return plan, 1
		}
		plan = built
		// Built with the file: they must not be added a second time below.
		selections = nil
	}

	// The command after -- is a poll source, always: it is the universal
	// fallback, and it is what makes every command watchable without any
	// per-command code.
	if len(opts.argv) > 0 {
		run, err := commandRunner(rc, opts)(opts.argv)
		if err != nil {
			fmt.Fprintf(rc.Err, "watch: %v\n", err)
			return plan, 1
		}
		src := watch.NewPollSource("poll", o.locator, run)
		src.Keys, src.AllowEmpty = o.keys, o.allowEmpty
		src.Class, src.Quiet = o.task, o.quietWindow
		plan.Sources = append(plan.Sources, src)
	}

	for _, spec := range o.probes {
		src, err := watch.NewHookSource(hookRunner(), watch.HookSpec{Script: spec, Locator: o.locator,
			Class: o.task, Quiet: watch.Duration(o.quietWindow)})
		if err != nil {
			fmt.Fprintf(rc.Err, "watch: %v\n", err)
			return plan, 2
		}
		plan.Sources = append(plan.Sources, src)
	}

	for _, spec := range o.hooks {
		sink, err := watch.NewHookSink(hookRunner(), watch.HookSpec{Script: spec})
		if err != nil {
			fmt.Fprintf(rc.Err, "watch: %v\n", err)
			return plan, 2
		}
		plan.Sinks = append(plan.Sinks, sink)
	}

	for _, spec := range selections {
		src, err := watch.BuildSource(spec, watch.BuildOptions{})
		if err != nil {
			fmt.Fprintf(rc.Err, "watch: %v\n", err)
			return plan, 2
		}
		plan.Sources = append(plan.Sources, src)
	}

	rules, err := buildFlagRules(o)
	if err != nil {
		fmt.Fprintf(rc.Err, "watch: %v\n", err)
		return plan, 2
	}
	plan.Rules = append(plan.Rules, rules...)
	return plan, -1
}

// ruleFile resolves --preset / --rules to the one rule file this invocation
// starts from. It is read, not built: the command line's own source selections
// have to reach it before it becomes a plan (see NarrowSources).
func ruleFile(rc *tool.RunContext, o *agenticOptions) (file watch.RuleFile, label string, ok bool, code int) {
	switch {
	case o.preset != "":
		preset, err := watch.LoadPreset(o.preset)
		if err != nil {
			fmt.Fprintf(rc.Err, "watch: %v\n", err)
			return file, "", false, 1
		}
		return preset.File, "preset " + o.preset, true, -1
	case o.rules != "":
		body, err := os.ReadFile(rc.Path(o.rules))
		if err != nil {
			fmt.Fprintf(rc.Err, "watch: %v\n", err)
			return file, "", false, 1
		}
		parsed, err := watch.ParseRuleFile(body)
		if err != nil {
			fmt.Fprintf(rc.Err, "watch: %v\n", err)
			return file, "", false, 1
		}
		return parsed, "rules " + o.rules, true, -1
	}
	return file, "", false, -1
}

// namedSourceSpec reads the flag spelling of a source: mb[:recipient],
// sprint-thread[:N]. "sprint" is the shorthand for the thread source, so the
// locator vocabulary and the source vocabulary agree.
func namedSourceSpec(spec string, backfill time.Duration) watch.SourceSpec {
	typ, target, _ := strings.Cut(spec, ":")
	typ = strings.TrimSpace(typ)
	if typ == "sprint" {
		typ = "sprint-thread"
	}
	return watch.SourceSpec{
		Type:     typ,
		Target:   strings.TrimSpace(target),
		Backfill: watch.Duration(backfill),
	}
}

func buildFlagRules(o *agenticOptions) ([]watch.Rule, error) {
	var out []watch.Rule
	for i, spec := range o.match {
		crit, err := parseCriteria(spec)
		if err != nil {
			return nil, err
		}
		rule, err := watch.NewMatch(fmt.Sprintf("match-%d", i+1), crit)
		if err != nil {
			return nil, err
		}
		out = append(out, rule)
	}
	if len(o.change) > 0 {
		rule, err := watch.NewChange("change", o.change, watch.Criteria{})
		if err != nil {
			return nil, err
		}
		out = append(out, rule)
	}
	if o.idle > 0 {
		out = append(out, watch.NewInverse(watch.Inverse{
			RuleName: "idle", Within: o.idle, Reason: watch.ReasonIdle, Expect: o.expect,
		}))
	}
	if o.heartbeat > 0 {
		out = append(out, watch.NewInverse(watch.Inverse{
			RuleName: "heartbeat", Within: o.heartbeat, Reason: watch.ReasonMissing, Expect: o.expect,
		}))
	}
	if o.replyWithin > 0 {
		trigger, err := watch.Criteria{Source: "mb", Kinds: []string{watch.KindAdded},
			Attrs: map[string]string{"directed": "true"}}.Compile()
		if err != nil {
			return nil, err
		}
		out = append(out, watch.NewCorrelation(watch.Correlation{
			RuleName: "agent-reply", Trigger: trigger, Within: o.replyWithin,
			Key:    func(ev watch.Event) string { return ev.Attr("conv") },
			Levels: hostsrc.MBReplyLevels(),
		}))
	}
	return out, nil
}

// parseCriteria reads the flag spelling of a predicate:
//
//	field=status,after=done        a field taking a value
//	text=stalled|failed            a regexp over the event's text
//	kind=added,kind=removed        event kinds
//	attr.to=operator               a source label
//	locator=sprint:*,not           everything but that locator
//
// An unknown key is an ERROR. A watch whose filter silently matched nothing is
// the first way the hand-written watcher failed, and it failed quietly.
func parseCriteria(spec string) (watch.Criteria, error) {
	var c watch.Criteria
	for _, token := range strings.Split(spec, ",") {
		token = strings.TrimSpace(token)
		if token == "" {
			continue
		}
		if token == "not" {
			c.Negate = true
			continue
		}
		key, value, ok := strings.Cut(token, "=")
		if !ok {
			return c, fmt.Errorf("--match %q: %q is not key=value", spec, token)
		}
		key = strings.TrimSpace(key)
		value = strings.TrimPrefix(strings.TrimSpace(value), "~")
		switch {
		case key == "source":
			c.Source = value
		case key == "kind" || key == "kinds":
			c.Kinds = append(c.Kinds, value)
		case key == "locator":
			c.Locator = value
		case key == "key":
			c.Key = value
		case key == "field":
			c.Field = value
		case key == "after":
			c.After = value
		case key == "before":
			c.Before = value
		case key == "text":
			c.Text = value
		case strings.HasPrefix(key, "attr."):
			if c.Attrs == nil {
				c.Attrs = map[string]string{}
			}
			c.Attrs[strings.TrimPrefix(key, "attr.")] = value
		default:
			return c, fmt.Errorf("--match %q: unknown selector %q (have source, kind, locator, key, field, after, before, text, attr.NAME, not)", spec, key)
		}
	}
	return c, nil
}

// hookRunner is the one hook runner for a watch invocation, so the
// concurrency-1 limit is shared across every hook in the plan rather than
// counted per sink.
var hookRunner = sync.OnceValue(watch.NewHooks)

// buildOptions is what a rule file is allowed to reach: the host's command
// runner and the host's hook runner, and nothing else.
func buildOptions(rc *tool.RunContext, opts watchOptions) watch.BuildOptions {
	return watch.BuildOptions{Command: commandRunner(rc, opts), Hooks: hookRunner()}
}

func listHooks(rc *tool.RunContext) int {
	rows, err := watch.HookCatalog().Rows()
	if err != nil {
		fmt.Fprintf(rc.Err, "watch: %v\n", err)
		return 1
	}
	if len(rows) == 0 {
		fmt.Fprintln(rc.Err, "watch: no hook scripts")
		return 1
	}
	for _, row := range rows {
		origin := row.Entry.Origin
		if origin == "" {
			origin = "(embedded)"
		}
		shadow := ""
		if row.Shadows {
			shadow = " (shadows the embedded example)"
		}
		fmt.Fprintf(rc.Out, "%-20s %-9s %s%s\n", row.Name, row.Entry.Ring, origin, shadow)
	}
	fmt.Fprintf(rc.Out, "\nlocal ring: %s\n", watch.HooksDir())
	return 0
}

// printHookSource is the copy mechanism: an embedded example has no path on
// disk, so `--hook-source gpu > ~/.bashy/watch/hooks/mine.bsh` is how a user
// starts from one.
func printHookSource(rc *tool.RunContext, name string) int {
	entry, _, ok := watch.HookCatalog().Get(name)
	if !ok {
		fmt.Fprintf(rc.Err, "watch: no hook %q (have: %s)\n", name, strings.Join(watch.HookNames(), ", "))
		return 1
	}
	rc.Out.Write(entry.Body)
	return 0
}

// commandRunner adapts the watched command to watch.Runner. The engine does not
// exec: which shell, which env and which directory are this invocation's
// business, and a rule file must not get to decide them.
func commandRunner(rc *tool.RunContext, opts watchOptions) func([]string) (watch.Runner, error) {
	return func(argv []string) (watch.Runner, error) {
		if len(argv) == 0 {
			return nil, fmt.Errorf("a poll source needs a command")
		}
		local := opts
		local.argv = argv
		return func(ctx context.Context) ([]byte, int, error) {
			return runSnapshot(ctx, rc, local)
		}, nil
	}
}

// runSnapshot captures one snapshot. Unlike the classic path it keeps stdout
// and stderr SEPARATE: a diagnostic on stderr would otherwise be mixed into the
// JSON the record diff is parsing, and the first malformed line would silently
// demote a structured watch to a line diff.
func runSnapshot(ctx context.Context, rc *tool.RunContext, opts watchOptions) ([]byte, int, error) {
	var c *exec.Cmd
	switch {
	case opts.execDirect:
		path := rc.ResolveExecutable(opts.argv[0])
		c = exec.CommandContext(ctx, path, opts.argv[1:]...)
	case runtime.GOOS == "windows":
		c = exec.CommandContext(ctx, "cmd", "/c", commandString(opts.argv))
	default:
		c = exec.CommandContext(ctx, "sh", "-c", commandString(opts.argv))
	}
	c.Dir = rc.Dir
	c.Env = rc.Env
	out, err := c.Output()
	if err == nil {
		return out, 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return out, exitErr.ExitCode(), nil
	}
	return out, 1, err
}

func listPresets(rc *tool.RunContext) int {
	names := watch.PresetNames()
	if len(names) == 0 {
		fmt.Fprintln(rc.Err, "watch: no presets")
		return 1
	}
	sort.Strings(names)
	for _, name := range names {
		preset, err := watch.LoadPreset(name)
		if err != nil {
			fmt.Fprintf(rc.Out, "%-12s (unreadable: %v)\n", name, err)
			continue
		}
		fmt.Fprintf(rc.Out, "%-12s %s\n", name, firstSentence(preset.File.Description))
	}
	fmt.Fprintf(rc.Out, "\nlocal ring: %s\n", watch.PresetsDir())
	return 0
}

func firstSentence(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, ". "); i > 0 {
		return s[:i+1]
	}
	return s
}

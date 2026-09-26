// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package dag

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/qiangli/coreutils/pkg/weavecli"
)

// newDagCmd builds the `dag` command. It is the agentic front door: a single
// command that lists targets (--list) or runs them as a dependency DAG. Output
// follows the weavecli envelope convention (BASHY_AGENTIC=1 forces --json).
func newDagCmd() *cobra.Command {
	var (
		listF, jsonF, plainF, quietF, keepGoing, forceF, explainF, dryRunF, outGroupF, checkF, watchF bool
		sandboxF, fleetF, meshF, timingsF, runsF, noJournalF, statusF, htmlF                          bool
		fileArg, showRunF, serveF                                                                     string
		cacheDir, cacheExport, cacheImport, chunksPath, remoteCmd, remoteShell                        string
		jobs, keepRuns                                                                                int
	)
	cmd := &cobra.Command{
		Use:   "dag [flags] [target ...]",
		Short: "Run markdown-defined targets as a dependency DAG",
		Long: `dag runs targets defined as headings in a markdown file (DAG.md) as a
real dependency graph — an agent-first replacement for make.

Each target is a heading with an optional description, metadata lines
(Requires:/Inputs:/Sources:/Generates:), an optional contract
(Require: precondition, checked before the body; Ensure: postcondition,
checked after it; Effects: declared effect cap — a command whose atlas
effects exceed the cap, or that the atlas does not know, is reported once
on stderr and runs anyway; a failed check exits 3 naming the clause), and a
fenced code block run through the in-process shell.
Targets execute in topological order; a target whose dependency failed is
skipped.

  dag --list                 # show targets (add --json for machine output)
  dag build                  # run "build" and its dependencies
  dag test lint              # run several targets and their closures
  dag build VERSION=v1.2     # make-style KEY=VALUE variable overrides
  dag pipeline.md            # run a dag file by path (no -f needed)
  dag pipeline.md ci         # ...with a target
  dag --file pipeline.md ci  # explicit --file is equivalent

With no target, dag runs the file's default goal — the frontmatter
"default:" key, or a target named "default" — and otherwise lists the
targets (like a Makefile whose .DEFAULT_GOAL is help).`,
		SilenceErrors: true,
		SilenceUsage:  true,
		// Targets are OPERANDS of the root command, and RunE validates them
		// against the DAG file. The contract must be declared: once a
		// subcommand is mounted on this root (bashy adds `capacity` through
		// AddCapacityCommands), cobra's legacy validator treats every
		// positional word as a subcommand lookup and rejects it with
		// `unknown command "build" for "dag"` — which SilenceErrors then
		// hides, so the verb died silently for every target in every repo
		// while this package's own tests (no subcommand mounted) stayed green.
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Changed("json") lets an explicit --json=false override BASHY_AGENTIC.
			mode := weavecli.ResolveOutputModeEx(cmd.Flags().Changed("json"), jsonF, plainF, quietF)
			out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()

			// --serve needs no DAG file: it serves the whole run journal, so it
			// short-circuits before discovery. A directory with no DAG.md can
			// still watch runs started from anywhere else on the machine.
			if serveF != "" {
				// --serve carries an optional value, which pflag only accepts
				// as --serve=ADDR. Written as `--serve ADDR` the address lands
				// here as a positional instead, and the server would silently
				// bind the default — so refuse rather than mis-bind.
				if len(args) > 0 {
					return emitErr(errOut, mode, errf(weavecli.ExitInvalidArg,
						"--serve takes its address attached: --serve=%s (got stray argument %q)",
						args[0], args[0]))
				}
				ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
				defer stop()
				if err := runServeCmd(ctx, out, serveF, cacheDir); err != nil {
					return emitErr(errOut, mode, err)
				}
				return nil
			}

			// make-style invocation: KEY=VALUE args are variable overrides
			// (injected into every target's environment), the rest are targets.
			targets, overrides := splitOverrides(args)

			// Convenience: `bashy dag path/to/file.md [target...]` — a leading
			// positional that names an existing file is treated as --file, so a
			// dag file can be run by path without -f. A leading positional that
			// names a DIRECTORY resolves to that folder's known entry (its
			// DAG.md), so a conventional deploy folder like `.bashy/deploy/`
			// works as the single entry point (`bashy dag .bashy/deploy deploy-qa`).
			// An explicit --file wins.
			// Bodies run in the INVOKING working directory (make parity):
			// `-f FILE` / a positional file only picks the file, exactly as
			// `make -f` does, so a task file kept elsewhere (a checked-in
			// example, a shared pipeline) drives THIS checkout. There is
			// deliberately no `-C DIR` flag — `awd DIR -- bashy dag …` is the
			// one "run it over there" mechanism, so verbs never grow their own.
			// The single exception is the positional DIRECTORY form below,
			// which names both the file and the place to run it.
			workDir, err := os.Getwd()
			if err != nil {
				return emitErr(errOut, mode, err)
			}
			path := fileArg
			if path == "" && len(targets) > 0 {
				if fi, statErr := os.Stat(targets[0]); statErr == nil {
					if fi.IsDir() {
						if p, derr := Discover(targets[0]); derr == nil {
							path, targets = p, targets[1:]
							if abs, aerr := filepath.Abs(filepath.Dir(p)); aerr == nil {
								workDir = abs
							}
						}
					} else {
						path, targets = targets[0], targets[1:]
					}
				}
			}
			if path == "" {
				p, err := Discover(".")
				if err != nil {
					return emitErr(errOut, mode, err)
				}
				path = p
			}
			doc, err := ParseFile(path)
			if err != nil {
				return emitErr(errOut, mode, err)
			}
			// P1 expansion passes (after parse, before BuildGraph):
			// ${NAME} metadata substitution (CLI KEY=VALUE wins), then the
			// matrix fan-out into one concrete node per combination.
			docVars := doc.expandVars(os.Environ(), overrides)
			doc.expandMatrix()

			// Chunk membership is a corpus property, so it is resolved here —
			// after the matrix fan-out that creates the shard targets, before
			// the graph is built, and entirely without reference to --fleet or
			// -j. Chunking pays off with no fleet configured at all (selective
			// re-run, cache granularity); the fleet only decides how many
			// chunks run at once.
			if err := applyChunkManifest(doc, chunksPath, path); err != nil {
				return emitErr(errOut, mode, err)
			}

			g, err := BuildGraph(doc)
			if err != nil {
				return emitErr(errOut, mode, err)
			}

			if checkF {
				return runCheck(out, errOut, mode, doc, g)
			}
			if listF {
				return runList(out, mode, doc)
			}
			// Read-only reporters (--timings, --runs, --show) report what is on
			// disk and run nothing, so they belong here with --check/--list:
			// BEFORE the default-goal resolution below, which falls back to
			// listing targets when a file has no default and no target was
			// named. Placed after it, `dag --timings` on such a file silently
			// printed the target list instead of the timings.
			absPath, _ := filepath.Abs(path)
			cache := LoadCache(absPath, cacheDir)
			if cacheImport != "" {
				if err := cache.ImportFromDir(cacheImport); err != nil {
					return emitErr(errOut, mode, errf(weavecli.ExitInvalidArg, "cache import: %v", err))
				}
			}
			// After --cache-import, so an imported baseline is what gets reported.
			if timingsF {
				return runTimings(out, mode, doc, cache)
			}
			if runsF {
				if err := runRuns(out, mode, doc, cacheDir, 0); err != nil {
					return emitErr(errOut, mode, err)
				}
				return nil
			}
			if showRunF != "" {
				if err := runShow(out, mode, doc, cacheDir, showRunF, htmlF); err != nil {
					return emitErr(errOut, mode, err)
				}
				return nil
			}
			if statusF {
				if err := runStatus(out, mode, doc, cacheDir); err != nil {
					// A failing last run is a verdict, not a usage error: report
					// the line already printed and carry the exit code out.
					if e, ok := err.(*Error); ok && e.Code == weavecli.ExitGenericFail {
						return err
					}
					return emitErr(errOut, mode, err)
				}
				return nil
			}

			if len(targets) == 0 {
				d := defaultTarget(doc)
				if d == "" {
					// No configured default goal — list targets, like a
					// Makefile whose .DEFAULT_GOAL is `help`.
					return runList(out, mode, doc)
				}
				targets = []string{d}
			}

			concurrency := jobs
			if concurrency < 1 {
				concurrency = 1
			}
			// CI log grouping: explicit --output-group, or auto-on under GitHub
			// Actions. Suppressed in JSON mode, which emits a single envelope.
			outputGroup := (outGroupF || os.Getenv("GITHUB_ACTIONS") == "true") &&
				mode != weavecli.OutputJSON
			// Body env: process env, then frontmatter vars (so ${HOST} etc. are
			// available to bodies, not just metadata), then CLI overrides (win).
			bodyEnv := os.Environ()
			bodyEnv = append(bodyEnv, bashySelfEnv()...)
			for k, v := range docVars {
				bodyEnv = append(bodyEnv, k+"="+v)
			}
			bodyEnv = append(bodyEnv, overrides...)
			eng := &Engine{
				Graph:       g,
				Dir:         workDir,
				Env:         bodyEnv,
				Concurrency: concurrency,
				FailFast:    !keepGoing,
				Force:       forceF,
				DryRun:      dryRunF,
				OutputGroup: outputGroup,
				Sandbox:     sandboxF,
				Fleet:       fleetF,
				Mesh:        meshF,
				RemoteCmd:   remoteCmd,
				RemoteShell: remoteShell,
				Cache:       cache,
				Verbose:     mode == weavecli.OutputAuto || mode == weavecli.OutputPlain,
				Capture:     mode == weavecli.OutputJSON,
				Stdin:       cmd.InOrStdin(),
				Stdout:      out,
				Stderr:      errOut,
			}

			if explainF {
				items, err := eng.Explain(targets...)
				if err != nil {
					return emitErr(errOut, mode, err)
				}
				return runExplain(out, mode, path, workDir, targets, items)
			}

			// Open the journal only once we know a run is actually happening —
			// --list/--check/--explain/--dryrun all return above, and opening it
			// earlier would litter the store with empty run directories.
			if !noJournalF {
				if j, jerr := OpenJournal(absPath, cacheDir, keepRuns); jerr == nil && j != nil {
					eng.Journal = j
					// Without this the journal records only the final report and
					// a live viewer sees a run in flight with every target stuck
					// on "pending" — the events ARE the live half.
					eng.Observer = j.Observer()
					defer j.Close()
				}
			}

			report, err := eng.Run(cmd.Context(), targets...)
			if err != nil {
				return emitErr(errOut, mode, err)
			}
			if cacheExport != "" {
				if err := cache.ExportToDir(cacheExport); err != nil {
					return emitErr(errOut, mode, errf(weavecli.ExitInvalidArg, "cache export: %v", err))
				}
			}
			if dryRunF {
				return runPlan(out, mode, path, targets, report.Plan)
			}
			if err := runReport(out, errOut, mode, path, targets, report); err != nil {
				return err
			}
			if watchF {
				ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
				defer stop()
				return runWatch(ctx, out, eng, targets)
			}
			return nil
		},
	}
	cmd.CompletionOptions.DisableDefaultCmd = true
	cmd.Flags().BoolVarP(&listF, "list", "l", false, "List targets and exit")
	cmd.Flags().BoolVar(&jsonF, "json", false, "Emit machine-readable envelope")
	cmd.Flags().BoolVar(&plainF, "plain", false, "Plain-text output, no banners")
	cmd.Flags().BoolVar(&quietF, "quiet", false, "Suppress banners; final result only")
	cmd.Flags().BoolVarP(&keepGoing, "keep-going", "k", false, "Continue past a failed target")
	cmd.Flags().IntVarP(&jobs, "jobs", "j", 1, "Run up to N targets in parallel (dependency-respecting)")
	cmd.Flags().BoolVarP(&forceF, "force", "B", false, "Ignore the fingerprint cache; run every target")
	cmd.Flags().BoolVar(&explainF, "explain", false, "Explain per target whether it would run or is up-to-date (runs nothing)")
	cmd.Flags().BoolVarP(&dryRunF, "dryrun", "n", false, "Print the ordered plan without running any target body")
	cmd.Flags().BoolVar(&outGroupF, "output-group", false, "Fold each target's output in GitHub ::group::/::endgroup:: markers (auto-on under GITHUB_ACTIONS)")
	cmd.Flags().BoolVar(&checkF, "check", false, "Validate the file (parse, deps, cycles, effects) and exit; runs nothing")
	cmd.Flags().BoolVar(&timingsF, "timings", false, "Report recorded per-target durations, total (T) and longest (L); runs nothing")
	cmd.Flags().BoolVar(&watchF, "watch", false, "Poll Sources/Inputs and re-run affected targets until interrupted")
	cmd.Flags().BoolVar(&runsF, "runs", false, "List recorded runs for this file, newest first; runs nothing")
	cmd.Flags().BoolVar(&statusF, "status", false, "One-line verdict for the most recent run (exit 1 if it failed); runs nothing")
	cmd.Flags().StringVar(&showRunF, "show", "", "Show one recorded run by id (or 'last'); runs nothing")
	cmd.Flags().BoolVar(&htmlF, "html", false, "Render --show as a standalone HTML page (graph + targets)")
	cmd.Flags().StringVar(&serveF, "serve", "", "Serve the run journal for live monitoring at ADDR (default "+DefaultServeAddr+"); runs nothing")
	// A bare --serve takes the loopback default; --serve host:port overrides it.
	cmd.Flags().Lookup("serve").NoOptDefVal = DefaultServeAddr
	cmd.Flags().IntVar(&keepRuns, "keep-runs", DefaultKeepRuns, "Recorded runs to retain per file (older ones are pruned)")
	cmd.Flags().BoolVar(&noJournalF, "no-journal", false, "Do not record this run's report, graph, or logs")
	cmd.Flags().BoolVar(&sandboxF, "sandbox", false, "Run target bodies through DAG_SANDBOX_CMD wrapper constraints")
	cmd.Flags().BoolVar(&fleetF, "fleet", false, "Run targets through the capacity-aware worker pool (local same-host workers only)")
	cmd.Flags().StringVar(&chunksPath, "chunks", "", "Committed chunk manifest pinning case→chunk membership (default: chunks.json next to the DAG file)")
	cmd.Flags().BoolVar(&meshF, "mesh", false, "Dispatch Host:-tagged targets to another machine (control plane only; body fetches its own code/data)")
	cmd.Flags().StringVar(&remoteCmd, "remote", "", "Remote-exec command for --mesh (default: ssh or DAG_REMOTE_EXEC)")
	cmd.Flags().StringVar(&remoteShell, "remote-shell", "", "Remote shell argv for --mesh (default: bash -s; use none to feed stdin directly)")
	cmd.Flags().StringVarP(&fileArg, "file", "f", "", "DAG markdown file (default: discover DAG.md)")
	cmd.Flags().StringVar(&cacheDir, "cache-dir", "", "Fingerprint cache directory (default: DAG_CACHE_DIR or user cache)")
	cmd.Flags().StringVar(&cacheExport, "cache-export", "", "Copy this DAG's fingerprint cache file to DIR after the run")
	cmd.Flags().StringVar(&cacheImport, "cache-import", "", "Copy this DAG's fingerprint cache file from DIR before the run")
	return cmd
}

func bashySelfEnv() []string {
	argv0 := os.Args[0]
	exe := resolveArgv0(argv0)
	if exe == "" {
		return nil
	}
	return []string{
		"BASHY=" + exe,
		"BASHY_EXE=" + exe,
		"BASHY_ARGV0=" + argv0,
	}
}

func resolveArgv0(argv0 string) string {
	if argv0 == "" {
		return ""
	}
	if strings.ContainsRune(argv0, os.PathSeparator) {
		if abs, err := filepath.Abs(argv0); err == nil {
			return abs
		}
		return argv0
	}
	if p, err := exec.LookPath(argv0); err == nil {
		if abs, err := filepath.Abs(p); err == nil {
			return abs
		}
		return p
	}
	return argv0
}

// checkResult is the --check envelope payload.
type checkResult struct {
	File     string   `json:"file"`
	Targets  int      `json:"targets"`
	Ok       bool     `json:"ok"`
	Warnings []string `json:"warnings,omitempty"`
}

// runCheck statically validates a parsed + built graph: parse, include
// resolution, var/matrix expansion, unknown/self deps, and unknown effects are
// already enforced by ParseFile/BuildGraph (errors surface before here); this
// adds a full-graph topological sort to catch cycles regardless of any target,
// plus non-fatal lint warnings. It runs no target bodies.
func runCheck(out, errOut io.Writer, mode weavecli.OutputMode, doc *Document, g *Graph) error {
	if _, err := g.TopoSort(); err != nil { // full-graph cycle detection
		return emitErr(errOut, mode, err)
	}
	res := checkResult{File: doc.Path, Targets: len(doc.Order), Ok: true}
	// Lint (non-fatal): targets whose Sources/Generates name a path that does
	// not exist yet are common and fine, so we only warn on empty bodies that
	// also declare no Requires (a no-op target that can never do anything).
	for _, name := range doc.Order {
		t, _ := doc.Lookup(name)
		if strings.TrimSpace(t.Body) == "" && len(t.Requires) == 0 {
			res.Warnings = append(res.Warnings, "target "+name+" has no body and no requires (no-op)")
		}
	}
	if mode == weavecli.OutputJSON {
		emitOK(out, res)
		return nil
	}
	fmt.Fprintf(out, "ok: %s — %d targets\n", doc.Path, res.Targets)
	for _, w := range res.Warnings {
		fmt.Fprintf(out, "warning: %s\n", w)
	}
	return nil
}

// defaultTarget resolves the goal for a no-argument invocation: the frontmatter
// `default:` key (make's .DEFAULT_GOAL) wins, then a target literally named
// "default". Empty means "no default" — the caller lists targets instead of
// guessing one to run.
func defaultTarget(doc *Document) string {
	if doc.Default != "" {
		return doc.Default
	}
	if _, ok := doc.Lookup("default"); ok {
		return "default"
	}
	return ""
}

// splitOverrides separates make-style `KEY=VALUE` variable overrides from target
// names. KEY must be a valid shell identifier; everything else is a target.
func splitOverrides(args []string) (targets, overrides []string) {
	for _, a := range args {
		if i := strings.IndexByte(a, '='); i > 0 && isVarName(a[:i]) {
			overrides = append(overrides, a)
		} else {
			targets = append(targets, a)
		}
	}
	return targets, overrides
}

func isVarName(s string) bool {
	for i, r := range s {
		switch {
		case r == '_':
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return s != ""
}

// applyChunkManifest binds committed chunk membership onto the document's shard
// targets.
//
// An explicit --chunks path must exist and must bind: a manifest the user named
// and that reached nothing is a silent truncation of the corpus. A manifest
// merely *discovered* beside the DAG file binds only when the document actually
// declares shard targets, so an unchunked `dag build` in a repo that happens to
// commit a chunks.json is unaffected.
func applyChunkManifest(doc *Document, chunksPath, dagPath string) error {
	explicit := chunksPath != ""

	p := chunksPath
	if p == "" {
		p = "chunks.json"
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(filepath.Dir(dagPath), p)
	}

	if !explicit {
		if !docHasShards(doc) {
			return nil
		}
		if _, err := os.Stat(p); err != nil {
			return nil // unchunked run of a sharded file; nothing pinned to apply
		}
	}

	m, err := LoadChunkManifest(p)
	if err != nil {
		return err
	}
	return BindChunks(doc, m)
}

// --- result shapes (envelope Result payloads) ---

type taskSummary struct {
	Name         string       `json:"name"`
	Desc         string       `json:"description,omitempty"`
	Requires     []string     `json:"requires,omitempty"`
	Inputs       []string     `json:"inputs,omitempty"`
	Sources      []string     `json:"sources,omitempty"`
	Generates    []string     `json:"generates,omitempty"`
	Effects      []string     `json:"effects,omitempty"`
	Lang         string       `json:"lang,omitempty"`
	Timeout      string       `json:"timeout,omitempty"`
	Retries      int          `json:"retries,omitempty"`
	Host         string       `json:"host,omitempty"`
	Venue        string       `json:"venue,omitempty"`
	Distribution Distribution `json:"distribution,omitempty"`
}

type listResult struct {
	File  string        `json:"file"`
	Tasks []taskSummary `json:"tasks"`
}

type runItem struct {
	Name        string       `json:"name"`
	Host        string       `json:"host"`
	Status      string       `json:"status"`
	ExitCode    int          `json:"exit_code"`
	DurationMS  int64        `json:"duration_ms"`
	Stdout      string       `json:"stdout,omitempty"`
	Stderr      string       `json:"stderr,omitempty"`
	Error       string       `json:"error,omitempty"`
	Attestation *Attestation `json:"attestation,omitempty"`
	Artifacts   []string     `json:"artifacts,omitempty"` // P1 #8 declared outputs recorded after success
}

type runResult struct {
	File    string    `json:"file"`
	Targets []string  `json:"targets"`
	Tasks   []runItem `json:"tasks"`
	Failed  bool      `json:"failed"`
}

type explainItem struct {
	Name         string       `json:"name"`
	Venue        string       `json:"venue,omitempty"`
	Distribution Distribution `json:"distribution,omitempty"`
	WouldRun     bool         `json:"would_run"`
	Reason       string       `json:"reason"`
}

type explainResult struct {
	File    string        `json:"file"`
	Dir     string        `json:"dir"`
	Targets []string      `json:"targets"`
	Plan    []explainItem `json:"plan"`
}

// runExplain prints the per-target run/skip decision computed by Engine.Explain
// without running anything. JSON mode emits a dag envelope carrying the plan.
// dir is the effective working directory bodies would run in — the invoking
// cwd, or the positional directory — so an operator can see where a task file
// kept elsewhere is about to act before it acts.
func runExplain(out io.Writer, mode weavecli.OutputMode, path, dir string, targets []string, items []ExplainItem) error {
	res := explainResult{File: path, Dir: dir, Targets: targets}
	for _, it := range items {
		res.Plan = append(res.Plan, explainItem{
			Name: it.Name, Venue: it.Venue, Distribution: it.Distribution,
			WouldRun: it.WouldRun, Reason: it.Reason,
		})
	}
	if mode == weavecli.OutputJSON {
		emitOK(out, res)
		return nil
	}
	fmt.Fprintf(out, "dir        %s\n", dir)
	for _, it := range items {
		decision := "up-to-date"
		if it.WouldRun {
			decision = "run"
		}
		fmt.Fprintf(out, "%-10s %-24s %s\n", decision, it.Name, it.Reason)
		if it.Venue != "" || it.Distribution != "" {
			fmt.Fprintf(out, "           %-24s venue=%s distribution=%s\n", "", it.Venue, it.Distribution)
		}
	}
	return nil
}

type planItem struct {
	Name     string   `json:"name"`
	WouldRun bool     `json:"would_run"`
	Reason   string   `json:"reason"`
	Effects  []string `json:"effects,omitempty"`
	Command  string   `json:"command,omitempty"`
}

type planResult struct {
	File    string     `json:"file"`
	Targets []string   `json:"targets"`
	Plan    []planItem `json:"plan"`
}

// runPlan prints the dry-run plan (ordered targets + decision + command) and
// runs nothing. JSON mode emits a dag envelope carrying the plan.
func runPlan(out io.Writer, mode weavecli.OutputMode, path string, targets []string, items []PlanItem) error {
	res := planResult{File: path, Targets: targets}
	for _, it := range items {
		res.Plan = append(res.Plan, planItem{
			Name: it.Name, WouldRun: it.WouldRun, Reason: it.Reason,
			Effects: it.Effects, Command: it.Command,
		})
	}
	if mode == weavecli.OutputJSON {
		emitOK(out, res)
		return nil
	}
	fmt.Fprintf(out, "dag: dry-run plan (%d target(s), running nothing)\n", len(items))
	for i, it := range items {
		decision := "up-to-date"
		if it.WouldRun {
			decision = "run"
		}
		fmt.Fprintf(out, "%2d. %-10s %-24s %s\n", i+1, decision, it.Name, it.Reason)
		if it.Command != "" {
			fmt.Fprintf(out, "    $ %s\n", it.Command)
		}
		if len(it.Effects) > 0 {
			fmt.Fprintf(out, "    effects: %s\n", strings.Join(it.Effects, ", "))
		}
	}
	return nil
}

func runList(out io.Writer, mode weavecli.OutputMode, doc *Document) error {
	res := listResult{File: doc.Path}
	for _, name := range doc.Order {
		t, _ := doc.Lookup(name)
		summary := taskSummary{
			Name: t.Name, Desc: t.Desc, Requires: t.Requires,
			Inputs: t.Inputs, Sources: t.Sources, Generates: t.Generates, Effects: t.Effects, Lang: t.Lang,
			Retries: t.Retries, Host: t.Host, Venue: t.Venue, Distribution: t.Distribution,
		}
		if t.Timeout > 0 {
			summary.Timeout = t.Timeout.String()
		}
		res.Tasks = append(res.Tasks, summary)
	}
	if mode == weavecli.OutputJSON {
		emitOK(out, res)
		return nil
	}
	for _, t := range res.Tasks {
		if t.Desc != "" {
			fmt.Fprintf(out, "%-24s %s\n", t.Name, firstLine(t.Desc))
		} else {
			fmt.Fprintln(out, t.Name)
		}
		if len(t.Requires) > 0 {
			fmt.Fprintf(out, "%-24s   requires: %s\n", "", strings.Join(t.Requires, ", "))
		}
	}
	return nil
}

func runReport(out, errOut io.Writer, mode weavecli.OutputMode, path string, targets []string, report RunReport) error {
	res := runResult{File: path, Targets: targets, Failed: report.Failed}
	for _, r := range report.Results {
		item := runItem{
			Name: r.Name, Host: r.Host, Status: r.Status.String(),
			ExitCode: r.ExitCode, DurationMS: r.Duration.Milliseconds(),
			Stdout: r.Stdout, Stderr: r.Stderr, Attestation: r.Attestation,
			Artifacts: r.Artifacts,
		}
		if r.Err != nil {
			item.Error = r.Err.Error()
		}
		res.Tasks = append(res.Tasks, item)
	}

	if report.Failed {
		if mode == weavecli.OutputJSON {
			res.Failed = true
			emitErrEnvelope(errOut, mode, weavecli.ExitGenericFail,
				errf(weavecli.ExitGenericFail, "one or more targets failed"), res)
		} else if mode != weavecli.OutputQuiet {
			fmt.Fprintf(errOut, "dag: %d/%d targets failed\n", countFailed(report), len(report.Results))
		}
		return &Error{Code: weavecli.ExitGenericFail, Msg: "one or more targets failed"}
	}

	if mode == weavecli.OutputJSON {
		emitOK(out, res)
	} else if mode != weavecli.OutputQuiet {
		fmt.Fprintf(out, "dag: %d target(s) ok\n", len(report.Results))
	}
	return nil
}

func countFailed(report RunReport) int {
	n := 0
	for _, r := range report.Results {
		if r.Status == StatusFailed || r.Status == StatusSkipped {
			n++
		}
	}
	return n
}

// emitErr writes the error envelope/line and returns the error so cobra's
// Execute() reports non-zero (ExitCodeOf recovers the code in the host).
func emitErr(errOut io.Writer, mode weavecli.OutputMode, err error) error {
	code := ExitCodeOf(err)
	emitErrEnvelope(errOut, mode, code, err, nil)
	return &Error{Code: code, Msg: err.Error()}
}

// emitOK encodes a status=ok dag envelope (schema "dag-v1") to w. Unlike
// weavecli.EmitOK it stamps the dag schema version, not weave's "loom-v2".
func emitOK(w io.Writer, result any) {
	encodeEnvelope(w, weavecli.Envelope{
		SchemaVersion: SchemaVersion,
		Command:       "dag",
		Status:        "ok",
		Result:        result,
	})
}

// emitErrEnvelope encodes a status=error dag envelope (or a plain "dag: msg"
// line outside JSON mode). result is attached when present (e.g. partial run
// state) so an agent sees which targets failed.
func emitErrEnvelope(w io.Writer, mode weavecli.OutputMode, code int, err error, result any) {
	if mode != weavecli.OutputJSON {
		fmt.Fprintf(w, "dag: %s\n", err.Error())
		return
	}
	encodeEnvelope(w, weavecli.Envelope{
		SchemaVersion: SchemaVersion,
		Command:       "dag",
		Status:        "error",
		Result:        result,
		Error:         &weavecli.EnvelopeError{Code: codeString(code), Message: err.Error()},
	})
}

func encodeEnvelope(w io.Writer, e weavecli.Envelope) {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(e)
}

func codeString(code int) string {
	switch code {
	case weavecli.ExitInvalidArg:
		return "invalid_arg"
	case weavecli.ExitPrecondFail:
		return "precondition_failed"
	case weavecli.ExitStateConflict:
		return "state_conflict"
	case weavecli.ExitDepUnhealthy:
		return "dependency_unhealthy"
	case weavecli.ExitOK:
		return "ok"
	default:
		return "generic_failure"
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

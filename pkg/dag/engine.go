// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package dag

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/qiangli/coreutils/pkg/weavecli"
)

// Engine executes a graph. Concurrency==1 runs targets in topological serial
// order (output streamed live); Concurrency>1 runs the dependency-respecting
// parallel scheduler (output captured per target, flushed in order). A Cache
// (when set, and unless Force) skips targets whose fingerprint is unchanged.
type Engine struct {
	Graph *Graph
	Dir   string   // working directory for every target body
	Env   []string // os.Environ() shape; passed to each interpreter

	Concurrency int    // 1 = serial; >1 = parallel worker pool (make -j N)
	FailFast    bool   // stop scheduling new targets after the first failure
	Verbose     bool   // print "==> target" banners
	Capture     bool   // buffer each target's output into its TaskResult (JSON)
	Force       bool   // ignore the fingerprint cache (make's unconditional run)
	DryRun      bool   // build the plan and short-circuit: run no bodies, mutate nothing
	OutputGroup bool   // wrap each target's captured output in CI ::group:: markers
	Sandbox     bool   // wrap target bodies in SandboxCmd/DAG_SANDBOX_CMD
	SandboxCmd  string // shell-split wrapper command; default is DAG_SANDBOX_CMD
	Fleet       bool   // run targets through a capacity-aware worker pool
	Pool        *Pool  // nil => LocalPool(Concurrency); a supplied pool owns its transports
	Mesh        bool   // dispatch Host:-tagged targets to another machine
	RemoteCmd   string // remote-exec command for mesh; default "ssh" / DAG_REMOTE_EXEC
	RemoteShell string // shell argv appended after host; default "bash -s"; "none" feeds stdin directly
	Executor    Executor
	Cache       *Cache   // nil = no incremental skip
	Journal     *Journal // nil = no run journal (history/logs are not recorded)

	// Observer, when set, is called as the run progresses. It exists so a run
	// can be WATCHED while it happens; the report only exists once it is over.
	// It is called from the scheduler's goroutines, so an implementation must
	// be safe for concurrent use, and it must not block: the engine calls it
	// on the path that runs targets.
	Observer func(Event)

	Stdout io.Writer
	Stderr io.Writer

	// pool is the run-scoped worker pool, bound at the top of Run and cleared
	// when it returns. Non-nil only under Fleet.
	pool     *Pool
	attempts *attemptLog
}

// RunReport aggregates per-target results. In dry-run mode Results is empty and
// Plan carries the ordered would-run/up-to-date plan instead.
type RunReport struct {
	Results []TaskResult
	Records []RunRecord
	Failed  bool
	Plan    []PlanItem // populated only in DryRun mode
}

// PlanItem is one target's entry in a dry-run plan: its topological position,
// the cache decision (would-run vs up-to-date) and why, declared effects, and
// the first body line that would execute.
type PlanItem struct {
	Name     string
	WouldRun bool
	Reason   string
	Effects  []string
	Command  string
}

// Run executes the transitive closure of targets (or the whole graph if none
// given) in dependency order. Returns a non-nil error only for graph-level
// failures (unknown target, cycle); per-target failures live in RunReport.
func (e *Engine) Run(ctx context.Context, targets ...string) (RunReport, error) {
	sub := e.Graph
	if len(targets) > 0 {
		var err error
		if sub, err = e.Graph.Subgraph(targets...); err != nil {
			return RunReport{}, err
		}
	}
	order, err := sub.TopoSort()
	if err != nil {
		return RunReport{}, err
	}

	// Precompute fingerprints in topological order so each node's fingerprint
	// can fold in its (already-computed) dependency fingerprints.
	fp := map[string]string{}
	if e.Cache != nil {
		for _, n := range order {
			fp[n.Task.Name] = e.Cache.Fingerprint(n, e.Dir, fp)
		}
	}

	// Dry-run: compute and return the plan without running any body or touching
	// the cache (no Save below — dry-run mutates nothing).
	if e.DryRun {
		return RunReport{Plan: e.planFor(order, fp)}, nil
	}

	e.attempts = newAttemptLog(order)
	defer func() { e.attempts = nil }()

	// The graph is recorded BEFORE anything runs, not at the end: a live viewer
	// needs the shape of the run while it is still happening, and a run that is
	// killed halfway should still leave behind what it was trying to do.
	if e.Journal != nil {
		_ = e.Journal.WriteGraph(order)
	}
	e.emitEvent(Event{Kind: EventRunStart, File: e.docPath(), Targets: targets})

	// Bind the worker pool for this run. It is built once here, not per task:
	// a pool constructed per dispatch would hand out a fresh set of slots every
	// time and enforce no capacity at all.
	if e.Fleet {
		pool, owned := e.bindPool()
		e.pool = pool
		defer func() {
			if owned {
				_ = pool.Close()
			}
			e.pool = nil
		}()
	}

	var report RunReport
	if e.Fleet || e.Concurrency > 1 {
		report = e.runParallel(ctx, order, fp)
	} else {
		report = e.runSerial(ctx, order, fp)
	}
	if e.Cache != nil {
		e.Cache.Save()
	}
	report.Records = e.attempts.all()
	e.emitEvent(Event{Kind: EventRunEnd, Failed: report.Failed})
	// The journal is best-effort by design: a run that produced results must not
	// be reported as failed because its history could not be written.
	if e.Journal != nil {
		_ = e.Journal.Finish(targets, report)
	}
	return report, nil
}

// docPath is the DAG document this engine is running, when known. The journal
// is keyed by it, so a viewer can name a live run before its report exists.
func (e *Engine) docPath() string {
	if e.Journal != nil {
		return e.Journal.docPath
	}
	return ""
}

// ExplainItem is one target's would-run/up-to-date decision and the reason,
// as produced by Explain without running any body.
type ExplainItem struct {
	Name         string
	Venue        string
	Distribution Distribution
	WouldRun     bool
	Reason       string
}

// Explain computes, for the transitive closure of targets in topological order,
// whether each target WOULD run or is already up-to-date and why — without
// executing any body. It reuses the exact fingerprint/up-to-date logic that
// Run does, so the explanation matches what a real run would decide.
func (e *Engine) Explain(targets ...string) ([]ExplainItem, error) {
	sub := e.Graph
	if len(targets) > 0 {
		var err error
		if sub, err = e.Graph.Subgraph(targets...); err != nil {
			return nil, err
		}
	}
	order, err := sub.TopoSort()
	if err != nil {
		return nil, err
	}
	fp := map[string]string{}
	if e.Cache != nil {
		for _, n := range order {
			fp[n.Task.Name] = e.Cache.Fingerprint(n, e.Dir, fp)
		}
	}
	items := make([]ExplainItem, 0, len(order))
	for _, n := range order {
		run, reason := e.explainOne(n, fp[n.Task.Name])
		spec := SpecFor(n.Task)
		items = append(items, ExplainItem{
			Name: n.Task.Name, Venue: spec.Venue, Distribution: spec.Distribution,
			WouldRun: run, Reason: reason,
		})
	}
	return items, nil
}

// explainOne decides, for a single node, whether it would run and the most
// specific reason available — mirroring Cache.UpToDate's checks so a "skip"
// here means upToDate would return true on a real run.
func (e *Engine) explainOne(n *Node, fp string) (wouldRun bool, reason string) {
	if e.Force {
		return true, "forced (--force ignores the cache)"
	}
	if e.Cache == nil {
		return true, "no cache configured"
	}
	if len(n.Task.Generates) == 0 {
		return true, "no declared outputs (phony target always runs)"
	}
	stored, ok := e.Cache.Hashes[n.Task.Name]
	if !ok {
		return true, "no cache entry (never recorded)"
	}
	for _, g := range n.Task.Generates {
		if _, err := os.Stat(filepath.Join(e.Dir, g)); err != nil {
			return true, "missing output: " + g
		}
	}
	if stored != fp {
		return true, "fingerprint changed (body or sources differ from cache)"
	}
	return false, "up to date"
}

// planFor builds the dry-run plan for an already-ordered, fingerprinted set of
// nodes, reusing the same decision logic the real run uses.
func (e *Engine) planFor(order []*Node, fp map[string]string) []PlanItem {
	plan := make([]PlanItem, 0, len(order))
	for _, n := range order {
		run, reason := e.explainOne(n, fp[n.Task.Name])
		plan = append(plan, PlanItem{
			Name:     n.Task.Name,
			WouldRun: run,
			Reason:   reason,
			Effects:  n.Task.Effects,
			Command:  firstBodyLine(n.Task.Body),
		})
	}
	return plan
}

// firstBodyLine returns the first non-blank, non-comment line of a body — the
// representative command shown in a plan listing.
func firstBodyLine(body string) string {
	for _, ln := range strings.Split(body, "\n") {
		s := strings.TrimSpace(ln)
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		return s
	}
	return ""
}

func (e *Engine) runSerial(ctx context.Context, order []*Node, fp map[string]string) RunReport {
	var report RunReport
	// Under --output-group, capture each target's output even in serial so it can
	// be flushed wrapped in ::group:: markers (mirrors the parallel scheduler).
	capture := e.Capture || e.OutputGroup
	for _, node := range order {
		if blocker, blocked := firstUnmetDep(node); blocked {
			res := e.markSkipped(node, blocker)
			if e.OutputGroup {
				e.flushGroup(node, res)
			}
			report.add(res)
			continue
		}
		if e.conditionFalse(ctx, node) {
			res := e.markConditionSkipped(node)
			if e.OutputGroup {
				e.flushGroup(node, res)
			}
			report.add(res)
			continue
		}
		if e.upToDate(node, fp) {
			node.Status = StatusUpToDate
			res := TaskResult{Name: node.Task.Name, Host: node.Task.Host, Status: StatusUpToDate, UpToDate: true}
			node.Result = &res
			e.noteResult(node.Task.Name, res)
			report.add(res)
			if e.OutputGroup {
				e.flushGroup(node, res)
			} else if e.Verbose {
				fmt.Fprintf(e.Stderr, "==> %s (up to date)\n", node.Task.Name)
			}
			continue
		}
		if e.Verbose && !e.OutputGroup {
			fmt.Fprintf(e.Stderr, "==> %s\n", node.Task.Name)
		}
		node.Status = StatusRunning
		res := e.runOne(ctx, node, capture, nil)
		node.Status = res.Status
		node.Result = &res
		report.add(res)
		if e.OutputGroup {
			e.flushGroup(node, res)
		}
		if res.Status == StatusFailed {
			if e.Verbose && !e.OutputGroup {
				fmt.Fprintf(e.Stderr, "==> %s FAILED (exit %d)\n", node.Task.Name, res.ExitCode)
				if res.Err != nil {
					fmt.Fprintf(e.Stderr, "    %s\n", res.Err)
				}
			}
			if e.FailFast {
				return report
			}
		} else if e.Cache != nil {
			e.Cache.Record(node.Task.Name, fp[node.Task.Name])
			// Guard on StatusDone rather than reusing the branch condition: this
			// else also catches StatusConditionSkipped, whose ~0s is not a cost.
			if res.Status == StatusDone {
				e.Cache.RecordDuration(node.Task.Name, res.Duration)
			}
		}
	}
	return report
}

// runParallel schedules ready nodes (all deps complete) across a worker pool of
// size Concurrency. Output is captured per node and flushed in topological order
// after completion, so concurrency never interleaves a target's output.
func (e *Engine) runParallel(ctx context.Context, order []*Node, fp map[string]string) RunReport {
	inSet := make(map[string]bool, len(order))
	remaining := make(map[string]int, len(order))
	for _, n := range order {
		inSet[n.Task.Name] = true
	}
	for _, n := range order {
		for _, d := range n.Deps {
			if inSet[d.Task.Name] {
				remaining[n.Task.Name]++
			}
		}
	}

	var (
		mu      sync.Mutex
		wg      sync.WaitGroup
		queued  = make(map[string]bool, len(order))
		results = make(map[string]*TaskResult, len(order))
		failed  bool
		stopped bool
		sem     = make(chan struct{}, max(1, e.Concurrency)) // unused under a fleet: the pool gates instead
		done    = make(chan *Node, len(order))
	)

	pushSkipped := func(n *Node, status Status) {
		queued[n.Task.Name] = true
		r := TaskResult{Name: n.Task.Name, Host: n.Task.Host, Status: status}
		n.Status, n.Result, results[n.Task.Name] = status, &r, &r
		e.noteResult(n.Task.Name, r)
		go func() { done <- n }()
	}

	var schedule func()
	schedule = func() { // caller holds mu
		for _, n := range order {
			if queued[n.Task.Name] || remaining[n.Task.Name] != 0 {
				continue
			}
			if _, blocked := firstUnmetDep(n); blocked {
				failed = true
				pushSkipped(n, StatusSkipped)
				continue
			}
			if stopped {
				continue // fail-fast: leave for the flush
			}
			queued[n.Task.Name] = true
			wg.Add(1)
			go func(node *Node) {
				defer wg.Done()
				// One gate, not two: a slot on a qualifying worker when a fleet
				// is configured, else the plain -j semaphore. Everything the
				// slot covers — the When: condition, the cache check, the body —
				// runs inside it, exactly as it did under the bare semaphore.
				worker, release, err := e.acquireSlot(ctx, node.Task, sem)
				var r TaskResult
				// runOne brackets the body with its own start/end events; the
				// other branches never run one, so they emit the end below.
				ran := false
				switch {
				case err != nil:
					// No worker can ever host this target: fail fast with the
					// reason instead of waiting for a slot that will never
					// qualify, mirroring the missing-tool failure.
					r = TaskResult{Name: node.Task.Name, Host: node.Task.Host, Status: StatusFailed, ExitCode: 1, Err: err}
					e.record(node.Task, worker, 1, r)
				case e.conditionFalse(ctx, node):
					r = TaskResult{Name: node.Task.Name, Host: node.Task.Host, Status: StatusConditionSkipped}
				case e.upToDate(node, fp):
					r = TaskResult{Name: node.Task.Name, Host: node.Task.Host, Status: StatusUpToDate, UpToDate: true}
				default:
					ran = true
					r = e.runOne(ctx, node, true, worker)
				}
				if !ran {
					e.noteResult(node.Task.Name, r)
				}
				if release != nil {
					release()
				}
				mu.Lock()
				node.Status, node.Result, results[node.Task.Name] = r.Status, &r, &r
				mu.Unlock()
				done <- node
			}(n)
		}
	}
	flushRemaining := func() { // caller holds mu; fail-fast: skip everything unstarted
		for _, n := range order {
			if !queued[n.Task.Name] {
				pushSkipped(n, StatusSkipped)
			}
		}
	}

	mu.Lock()
	schedule()
	mu.Unlock()

	for completed := 0; completed < len(order); completed++ {
		n := <-done
		mu.Lock()
		switch n.Result.Status {
		case StatusFailed:
			failed = true
			if e.FailFast && !stopped {
				stopped = true
				flushRemaining()
			}
		case StatusSkipped:
			failed = true
		case StatusDone, StatusUpToDate:
			if n.Result.Status == StatusDone && e.Cache != nil {
				e.Cache.Record(n.Task.Name, fp[n.Task.Name])
				e.Cache.RecordDuration(n.Task.Name, n.Result.Duration)
			}
		}
		for _, dep := range n.Dependents {
			if _, ok := remaining[dep.Task.Name]; ok && remaining[dep.Task.Name] > 0 {
				remaining[dep.Task.Name]--
			}
		}
		schedule()
		mu.Unlock()
	}
	wg.Wait()

	// Deterministic output + report assembly in topological order.
	var report RunReport
	report.Failed = failed
	for _, n := range order {
		r := results[n.Task.Name]
		if r == nil {
			continue
		}
		if e.OutputGroup {
			e.flushGroup(n, *r)
		} else if e.Verbose {
			e.flushBanner(n, *r)
		}
		report.Results = append(report.Results, *r)
	}
	return report
}

// acquireSlot blocks until this task may run, and reports where it may run.
//
// With a fleet, the pool IS the gate — it is the only thing that can express "4
// slots on this worker, 12 on that one", which a single global semaphore cannot.
// It returns the worker the task was placed on, so the body runs on the machine
// the scheduler chose rather than being re-placed further down.
//
// Without a fleet, the gate is the plain -j semaphore and there is no worker:
// byte-for-byte today's path.
func (e *Engine) acquireSlot(ctx context.Context, t *Task, sem chan struct{}) (*Worker, func(), error) {
	if e.pool != nil {
		return e.pool.Acquire(ctx, constraintsFor(t))
	}
	select {
	case sem <- struct{}{}:
		return nil, func() { <-sem }, nil
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
}

// flushGroup writes one target's captured output wrapped in GitHub Actions log
// folding markers (::group::<target> … ::endgroup::), emitted in topological
// order so that -j N output folds cleanly in CI logs instead of interleaving.
// stderr is folded into the same stream as stdout so the markers bracket the
// whole of a target's output.
func (e *Engine) flushGroup(n *Node, r TaskResult) {
	fmt.Fprintf(e.Stdout, "::group::%s\n", n.Task.Name)
	switch r.Status {
	case StatusUpToDate:
		fmt.Fprintln(e.Stdout, "(up to date)")
	case StatusSkipped:
		fmt.Fprintln(e.Stdout, "(skipped: dependency did not succeed)")
	case StatusConditionSkipped:
		fmt.Fprintln(e.Stdout, "(skipped: condition false)")
	}
	if r.Stdout != "" {
		io.WriteString(e.Stdout, r.Stdout)
	}
	if r.Stderr != "" {
		io.WriteString(e.Stdout, r.Stderr)
	}
	if r.Status == StatusFailed {
		fmt.Fprintf(e.Stdout, "==> %s FAILED (exit %d)\n", n.Task.Name, r.ExitCode)
		if r.Err != nil {
			fmt.Fprintf(e.Stdout, "    %s\n", r.Err)
		}
		fmt.Fprintf(e.Stderr, "==> %s FAILED (exit %d)\n", n.Task.Name, r.ExitCode)
		if r.Err != nil {
			fmt.Fprintf(e.Stderr, "    %s\n", r.Err)
		}
	}
	fmt.Fprintln(e.Stdout, "::endgroup::")
}

func (e *Engine) flushBanner(n *Node, r TaskResult) {
	switch r.Status {
	case StatusUpToDate:
		fmt.Fprintf(e.Stderr, "==> %s (up to date)\n", n.Task.Name)
		return
	case StatusSkipped:
		fmt.Fprintf(e.Stderr, "==> %s (skipped: dependency did not succeed)\n", n.Task.Name)
		return
	case StatusConditionSkipped:
		fmt.Fprintf(e.Stderr, "==> %s (skipped: condition false)\n", n.Task.Name)
		return
	}
	fmt.Fprintf(e.Stderr, "==> %s\n", n.Task.Name)
	if r.Stdout != "" {
		io.WriteString(e.Stdout, r.Stdout)
	}
	if r.Stderr != "" {
		io.WriteString(e.Stderr, r.Stderr)
	}
	if r.Status == StatusFailed {
		fmt.Fprintf(e.Stderr, "==> %s FAILED (exit %d)\n", n.Task.Name, r.ExitCode)
		if r.Err != nil {
			fmt.Fprintf(e.Stderr, "    %s\n", r.Err)
		}
	}
}

// runOne executes a single node's body through its interpreter, applying the
// target's P0 execution policy: a per-attempt Timeout (deadline hit =>
// StatusFailed, ExitCode 124, "timeout") and up to Retries extra attempts on
// failure (with an optional Backoff sleep between). When capture is set,
// stdout/stderr are buffered into the result instead of streamed.
// worker is the fleet worker the scheduler placed this target on, or nil when
// no fleet is configured (then the engine's own executor runs it in-process).
func (e *Engine) runOne(ctx context.Context, node *Node, capture bool, worker *Worker) TaskResult {
	// P1 #7 — resolve declared secret VALUES so they can be redacted from the
	// captured output. A target with secrets is always captured (even when the
	// engine isn't) so the values never reach the real stdout unredacted.
	secretVals := e.secretValues(node.Task)
	captureRun := capture || len(secretVals) > 0

	// Contract (Sprint 203): Require preconditions gate the body. A failed
	// precondition is a failed target with ExitPrecondFail and an invalid
	// attestation naming the check — the body is never run, and there is
	// nothing to retry.
	if att := requireHolds(ctx, node.Task, e.Dir, e.envFor(node)); att != nil {
		res := TaskResult{Name: node.Task.Name, Host: node.Task.Host, Status: StatusFailed,
			ExitCode: weavecli.ExitPrecondFail, Err: firstFailedCheck(att, "precondition"), Attestation: att}
		e.emitEvent(Event{Kind: EventTaskStart, Task: node.Task.Name, Attempt: 1,
			Log: filepath.ToSlash(attemptLogPath(node.Task.Name, 1))})
		e.emitEvent(Event{Kind: EventTaskEnd, Task: node.Task.Name, Attempt: 1,
			Status: res.Status.String(), ExitCode: res.ExitCode})
		e.record(node.Task, worker, 1, res)
		return res
	}

	attempts := node.Task.Retries + 1
	if attempts < 1 {
		attempts = 1
	}
	var res TaskResult
	results := make([]TaskResult, 0, attempts)
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 && node.Task.Backoff > 0 {
			select {
			case <-time.After(node.Task.Backoff):
			case <-ctx.Done():
			}
		}
		e.emitEvent(Event{
			Kind: EventTaskStart, Task: node.Task.Name, Attempt: attempt + 1,
			Log: filepath.ToSlash(attemptLogPath(node.Task.Name, attempt+1)),
		})
		res = e.runAttempt(ctx, node, captureRun, worker, attempt+1)
		res = applyExitContract(node.Task, res)
		e.emitEvent(Event{
			Kind: EventTaskEnd, Task: node.Task.Name, Attempt: attempt + 1,
			Status: res.Status.String(), ExitCode: res.ExitCode,
			DurationMS: res.Duration.Milliseconds(),
		})
		results = append(results, res)
		if res.Status == StatusDone {
			break
		}
		if res.Status == StatusConditionSkipped {
			break
		}
		if ctx.Err() != nil {
			break // parent cancelled — don't burn remaining attempts
		}
	}

	if len(secretVals) > 0 {
		// Journal the REDACTED output of every attempt. runAttempt deliberately
		// skipped the live tee for secret-declaring targets, so this is the only
		// place their logs can be written without putting the secret on disk.
		for i := range results {
			results[i].Stdout = redactSecrets(results[i].Stdout, secretVals)
			results[i].Stderr = redactSecrets(results[i].Stderr, secretVals)
			e.Journal.writeAttemptLog(node.Task.Name, i+1, results[i].Stdout, results[i].Stderr)
		}
		res.Stdout = redactSecrets(res.Stdout, secretVals)
		res.Stderr = redactSecrets(res.Stderr, secretVals)
		if res.Err != nil {
			res.Err = errors.New(redactSecrets(res.Err.Error(), secretVals))
		}
		// If the engine wasn't capturing, we captured only to redact: emit the
		// now-redacted output and drop it from the result so it is neither
		// streamed twice nor surfaced where a plain run would not show it.
		if !capture {
			io.WriteString(e.Stdout, res.Stdout)
			io.WriteString(e.Stderr, res.Stderr)
			res.Stdout, res.Stderr = "", ""
		}
	}

	// P1 #8 — record declared artifacts that exist after a successful body.
	if res.Status == StatusDone {
		res.Artifacts = e.collectArtifacts(node.Task)
	}

	// P2 contract: a clean exit is necessary but not sufficient — the target's
	// Ensure postconditions must hold. A failed postcondition fails the target
	// with ExitPrecondFail even though the body returned 0.
	if att := attest(ctx, node.Task, e.Dir, e.envFor(node), res.Status == StatusDone); att != nil {
		res.Attestation = att
		if !att.Valid && res.Status == StatusDone {
			res.Status = StatusFailed
			res.ExitCode = weavecli.ExitPrecondFail
			res.Err = firstFailedCheck(att, "postcondition")
		}
	}
	res.Host = node.Task.Host
	results[len(results)-1] = res
	for attempt, attemptResult := range results {
		e.record(node.Task, worker, attempt+1, attemptResult)
	}
	return res
}

func applyExitContract(t *Task, res TaskResult) TaskResult {
	if len(t.ExitCodes) == 0 {
		return res
	}
	action, ok := t.ExitCodes[res.ExitCode]
	if !ok {
		return res
	}
	switch action {
	case "ok":
		res.Status, res.Err = StatusDone, nil
	case "skip":
		res.Status, res.Err = StatusConditionSkipped, nil
	case "retry":
		res.Status = StatusFailed
	case "fail":
		res.Status = StatusFailed
		if res.Err == nil {
			res.Err = fmt.Errorf("exit %d classified as fail", res.ExitCode)
		}
	}
	return res
}

// runAttempt runs the body once, wrapping it in a context.WithTimeout when the
// target declares a Timeout. A deadline hit is reported as exit 124 "timeout".
func (e *Engine) runAttempt(ctx context.Context, node *Node, capture bool, worker *Worker, attempt int) TaskResult {
	runCtx := ctx
	if node.Task.Timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, node.Task.Timeout)
		defer cancel()
	}
	stdout, stderr := e.Stdout, e.Stderr
	var ob, eb *bytes.Buffer
	if capture {
		ob, eb = new(bytes.Buffer), new(bytes.Buffer)
		stdout, stderr = ob, eb
	}
	// Tee this attempt's output into the journal. Secret-declaring targets are
	// excluded HERE and journaled by runOne instead: their output is redacted
	// only after capture, so streaming raw bytes to a file would write exactly
	// the values redaction exists to remove. The journal writer comes last in
	// each MultiWriter, so the real stdout/stderr still sees its bytes first
	// and terminal ordering is unchanged.
	if e.Journal != nil && journalMayTee(node.Task) {
		if w := e.Journal.attemptLogWriter(node.Task.Name, attempt); w != nil {
			stdout = io.MultiWriter(stdout, w)
			stderr = io.MultiWriter(stderr, w)
		}
	}
	// Builtin Tools: preflight — prepend a presence + version check to the body
	// so it runs wherever the body runs (local, remote via --mesh, or sandbox).
	task := node.Task
	if len(task.Tools) > 0 {
		cp := *task
		cp.Body = toolPreamble(task.Tools) + task.Body
		task = &cp
	}
	res := e.executeTask(runCtx, task, worker, TaskIO{
		Dir:    e.Dir,
		Env:    e.envFor(node),
		Stdout: stdout,
		Stderr: stderr,
	})
	if capture {
		res.Stdout, res.Stderr = ob.String(), eb.String()
	}
	// A per-target deadline that fired (not a parent cancellation) is a timeout.
	if node.Task.Timeout > 0 && ctx.Err() == nil && runCtx.Err() == context.DeadlineExceeded {
		res.Status = StatusFailed
		res.ExitCode = 124
		res.Err = fmt.Errorf("timeout after %s", node.Task.Timeout)
	}
	return res
}

// executeTask is the placement seam. Without --fleet it is exactly today's path
// (the engine's executor, in-process). With --fleet the run-scoped pool picks a
// worker that satisfies the task's constraints and its transport runs the body
// there — which for P2 is always the local userland worker.
func (e *Engine) executeTask(ctx context.Context, task *Task, worker *Worker, tio TaskIO) TaskResult {
	if e.pool != nil && worker != nil {
		// The slot on this worker is already held by the scheduler; run the body
		// there through its transport (locally, for the userland venue).
		return e.pool.ExecOn(ctx, worker, task, tio)
	}
	return e.executor().Execute(ctx, task, tio)
}

// constraintsFor derives what a task demands of a worker, through the
// versioned TaskSpec contract so the in-memory scheduler and a serialized spec
// can never disagree. P2 specs always ask for the userland venue — which the
// local worker offers, making --fleet on one box behave exactly like -j N.
func constraintsFor(t *Task) Constraints {
	return SpecFor(t).Constraints()
}

func (e *Engine) sandboxEnabled() bool {
	return e.Sandbox || e.SandboxCmd != "" || os.Getenv("DAG_SANDBOX_CMD") != ""
}

// bindPool resolves the pool for one run and reports whether the engine owns it
// (and must therefore close it). `Pool == nil` degrades to LocalPool(Concurrency):
// one in-process worker with N slots, which is today's -j N by construction
// rather than by branching around the pool.
//
// The pool's default transport is built from the engine's executor, so --sandbox
// composes with --fleet instead of being silently dropped.
func (e *Engine) bindPool() (*Pool, bool) {
	if e.Pool != nil {
		e.Pool.SetDefaultTransport(localTransport{exec: e.executor()})
		return e.Pool, false
	}
	p := LocalPool(max(1, e.Concurrency))
	p.SetDefaultTransport(localTransport{exec: e.executor()})
	return p, true
}

func (e *Engine) executor() Executor {
	if e.sandboxEnabled() {
		return sandboxExecutor{Command: e.sandboxCommand()}
	}
	if e.Executor != nil {
		return e.Executor
	}
	if e.meshEnabled() {
		return meshExecutor{Remote: e.remoteCommand(), RemoteShell: e.RemoteShell}
	}
	return localExecutor{}
}

// meshEnabled requires an explicit opt-in (--mesh or --remote) so a stray
// DAG_REMOTE_EXEC in the environment never silently sends bodies off-box.
func (e *Engine) meshEnabled() bool {
	return e.Mesh || e.RemoteCmd != ""
}

func (e *Engine) remoteCommand() string {
	if e.RemoteCmd != "" {
		return e.RemoteCmd
	}
	if v := os.Getenv("DAG_REMOTE_EXEC"); v != "" {
		return v
	}
	return "ssh"
}

func (e *Engine) sandboxCommand() string {
	if e.SandboxCmd != "" {
		return e.SandboxCmd
	}
	if v := os.Getenv("DAG_SANDBOX_CMD"); v != "" {
		return v
	}
	return "bashy podman run"
}

type localExecutor struct{}

func (localExecutor) Execute(ctx context.Context, t *Task, tio TaskIO) TaskResult {
	ip, err := interpFor(t.Lang)
	if err != nil {
		return TaskResult{Name: t.Name, Host: t.Host, Status: StatusFailed, ExitCode: 2, Err: err}
	}
	return ip.Run(ctx, t, tio)
}

type sandboxExecutor struct {
	Command string
}

func (x sandboxExecutor) Execute(ctx context.Context, t *Task, tio TaskIO) TaskResult {
	start := time.Now()
	res := TaskResult{Name: t.Name, Host: t.Host}
	name, args := sandboxCommandArgs(x.Command, t.Effects, t.Body)
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = tio.Dir
	cmd.Env = tio.Env
	cmd.Stdout = tio.Stdout
	cmd.Stderr = tio.Stderr
	err := cmd.Run()
	res.Duration = time.Since(start)
	res.ExitCode, res.Err = exitCodeFromExecErr(err)
	if res.ExitCode == 0 {
		res.Status = StatusDone
	} else {
		res.Status = StatusFailed
	}
	return res
}

func sandboxCommandArgs(wrapper string, effects []string, body string) (string, []string) {
	parts := strings.Fields(wrapper)
	if len(parts) == 0 {
		parts = []string{"bashy", "podman", "run"}
	}
	args := append([]string{}, parts[1:]...)
	args = append(args, sandboxArgs(effects)...)
	// The container image is configurable + pinnable via DAG_SANDBOX_IMAGE
	// (default "bash"); avoid silently pulling an unpinned public :latest in CI.
	image := os.Getenv("DAG_SANDBOX_IMAGE")
	if image == "" {
		image = "bash"
	}
	args = append(args, image, "-c", body)
	return parts[0], args
}

func sandboxArgs(effects []string) []string {
	var args []string
	if !hasEffect(effects, "net") {
		args = append(args, "--network=none")
	}
	if !hasEffect(effects, "write") {
		args = append(args, "--read-only")
	}
	return args
}

func hasEffect(effects []string, want string) bool {
	for _, ef := range effects {
		if ef == want {
			return true
		}
	}
	return false
}

func exitCodeFromExecErr(err error) (int, error) {
	if err == nil {
		return 0, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), nil
	}
	return 1, err
}

// envFor builds the environment for a node: the base env, any per-target Env
// overrides (make's target-specific variables), and the resolved values of any
// declared Secrets (P1 #7), so the body sees $NAME for each secret.
func (e *Engine) envFor(node *Node) []string {
	secrets := e.secretEnv(node.Task)
	if len(node.Task.Env) == 0 && len(secrets) == 0 {
		return e.Env
	}
	out := append(append([]string{}, e.Env...), node.Task.Env...)
	return append(out, secrets...)
}

// secretEnv resolves each declared secret to a NAME=value entry. Resolution is
// process-env first (the base env, which already carries CLI overrides). A
// cloudbox-vault hook (`bashy secret get <name>`) is the documented future
// source; this runner never shells out, so an unresolved secret is simply
// absent (the body sees an empty $NAME).
func (e *Engine) secretEnv(t *Task) []string {
	if len(t.Secrets) == 0 {
		return nil
	}
	base := envMap(e.Env)
	var out []string
	for _, name := range t.Secrets {
		if v, ok := base[name]; ok {
			out = append(out, name+"="+v)
		}
	}
	return out
}

// secretValues returns the (non-empty) resolved values of a target's secrets,
// for redaction from its captured output.
func (e *Engine) secretValues(t *Task) []string {
	if len(t.Secrets) == 0 {
		return nil
	}
	base := envMap(e.Env)
	var out []string
	for _, name := range t.Secrets {
		if v := base[name]; v != "" {
			out = append(out, v)
		}
	}
	return out
}

// redactSecrets replaces every occurrence of each secret value in s with "***".
func redactSecrets(s string, values []string) string {
	for _, v := range values {
		if v != "" {
			s = strings.ReplaceAll(s, v, "***")
		}
	}
	return s
}

// collectArtifacts resolves a target's declared Artifacts (paths/globs, relative
// to the engine's Dir) to the relative paths that exist after the body ran, and
// — when $DAG_ARTIFACTS_DIR is set — copies each into that directory preserving
// its relative path. Returns the recorded relative paths in sorted order.
func (e *Engine) collectArtifacts(t *Task) []string {
	if len(t.Artifacts) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var rels []string
	for _, pat := range t.Artifacts {
		matches, err := filepath.Glob(filepath.Join(e.Dir, pat))
		if err != nil {
			continue
		}
		for _, m := range matches {
			rel, err := filepath.Rel(e.Dir, m)
			if err != nil {
				rel = m
			}
			if !seen[rel] {
				seen[rel] = true
				rels = append(rels, rel)
			}
		}
	}
	sort.Strings(rels)

	if adir := envMap(e.Env)["DAG_ARTIFACTS_DIR"]; adir != "" {
		for _, rel := range rels {
			copyArtifact(filepath.Join(e.Dir, rel), filepath.Join(adir, rel))
		}
	}
	return rels
}

// copyArtifact best-effort copies src to dst, creating parent directories. A
// failure is silent: artifact collection must never fail an otherwise-good run.
func copyArtifact(src, dst string) {
	fi, err := os.Stat(src)
	if err != nil || fi.IsDir() {
		return
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return
	}
	_ = os.WriteFile(dst, data, 0o644)
}

func (e *Engine) upToDate(node *Node, fp map[string]string) bool {
	return e.Cache != nil && !e.Force && e.Cache.UpToDate(node, e.Dir, fp[node.Task.Name])
}

func (e *Engine) markSkipped(node *Node, blocker string) TaskResult {
	node.Status = StatusSkipped
	res := TaskResult{Name: node.Task.Name, Host: node.Task.Host, Status: StatusSkipped}
	node.Result = &res
	e.noteResult(node.Task.Name, res)
	if e.Verbose && !e.OutputGroup {
		fmt.Fprintf(e.Stderr, "==> skip %s (dependency %s did not succeed)\n", node.Task.Name, blocker)
	}
	return res
}

// conditionFalse reports whether a target's `When:` condition (P1 #10) is
// present and evaluates false (non-zero exit) through the in-process shell. A
// target with no When always runs. A false condition is a clean skip — it does
// NOT fail the run and does NOT block dependents (unlike a dependency-failure
// skip); dependents see the target as a satisfied no-op and run normally.
func (e *Engine) conditionFalse(ctx context.Context, node *Node) bool {
	cond := strings.TrimSpace(node.Task.When)
	if cond == "" {
		return false
	}
	return !shellCheck(ctx, e.Dir, e.envFor(node), cond, cond).Pass
}

// markConditionSkipped records a `When:`-false skip — a non-failing status that
// keeps the run green.
func (e *Engine) markConditionSkipped(node *Node) TaskResult {
	node.Status = StatusConditionSkipped
	res := TaskResult{Name: node.Task.Name, Host: node.Task.Host, Status: StatusConditionSkipped}
	node.Result = &res
	e.noteResult(node.Task.Name, res)
	if e.Verbose && !e.OutputGroup {
		fmt.Fprintf(e.Stderr, "==> %s (skipped: condition false)\n", node.Task.Name)
	}
	return res
}

func (r *RunReport) add(res TaskResult) {
	r.Results = append(r.Results, res)
	if res.Status == StatusFailed || res.Status == StatusSkipped {
		r.Failed = true
	}
}

func firstUnmetDep(n *Node) (string, bool) {
	for _, d := range n.Deps {
		if d.Status == StatusFailed || d.Status == StatusSkipped {
			return d.Task.Name, true
		}
	}
	return "", false
}

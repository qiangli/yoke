package weave

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/coreutils/pkg/weavecli"
	"github.com/qiangli/yoke/pkg/role"
)

const weaveAutopilotLeaseFile = "orchestrator-lease.json"

type weaveAutopilotOptions struct {
	fleetCSV    string
	briefPath   string
	reviewAgent string
	standby     bool

	leaseTTL  time.Duration
	heartbeat time.Duration
	backoff   time.Duration
}

type weaveOrchestratorLease struct {
	Holder string `json:"holder"`
	// Tool stays the executable's registry name — the key everything else is
	// recorded under. Agent and Binding are additive, present when the roster
	// entry named an agent.
	Tool        string    `json:"tool"`
	Agent       string    `json:"agent,omitempty"`
	Binding     string    `json:"binding,omitempty"`
	PID         int       `json:"pid"`
	AcquiredAt  time.Time `json:"acquired_at"`
	HeartbeatAt time.Time `json:"heartbeat_at"`
	ExpiresAt   time.Time `json:"expires_at"`
	Generation  int64     `json:"generation"`
}

// weaveAutopilotRunner drives one orchestrator member.
//
// It takes a resolved member rather than a tool name because an agent and its
// bare tool exec the same binary — what differs is the model, and a runner
// handed only a name cannot know it.
type weaveAutopilotRunner interface {
	Run(ctx context.Context, m weaveMember, prompt, queueDir string, onOutput func(string)) (int, error)
	Healthy(ctx context.Context, m weaveMember) bool
}

type weaveExecAutopilotRunner struct{}

var weaveAutopilotRunnerDefault weaveAutopilotRunner = weaveExecAutopilotRunner{}

func runWeaveAutopilot(cmd *cobra.Command, opts weaveAutopilotOptions, flags *weaveOutputFlags) error {
	mode := flags.mode()
	if opts.leaseTTL <= 0 {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave autopilot",
			weavecli.ExitInvalidArg, fmt.Errorf("--lease-ttl must be positive")))
	}
	if opts.heartbeat <= 0 {
		opts.heartbeat = opts.leaseTTL / 3
		if opts.heartbeat <= 0 {
			opts.heartbeat = time.Second
		}
	}
	if opts.backoff <= 0 {
		opts.backoff = 10 * time.Second
	}
	names := parseWeaveAutopilotFleet(opts.fleetCSV)
	if len(names) == 0 {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave autopilot",
			weavecli.ExitInvalidArg, fmt.Errorf("--orchestrator-fleet is required")))
	}
	// Resolve the whole roster before taking a lease. Naming an agent asserts a
	// model, and a binding that cannot run is a configuration error the
	// operator should hear now — not at 3am, on failover, from a dead member.
	fleet, err := resolveWeaveRoster(names)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave autopilot",
			weavecli.ExitInvalidArg, err))
	}
	cwd, _ := os.Getwd()
	root, rerr := weaveRepoRoot(cwd)
	if rerr != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave autopilot",
			weavecli.ExitPrecondFail, rerr))
	}
	dir, err := weaveQueueDir(root)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave autopilot",
			weavecli.ExitGenericFail, err))
	}
	brief, err := readWeaveAutopilotBrief(opts.briefPath)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave autopilot",
			weavecli.ExitInvalidArg, err))
	}

	res, err := runWeaveAutopilotLoop(context.Background(), weaveAutopilotLoopOptions{
		queueDir:    dir,
		repoRoot:    root,
		fleet:       fleet,
		brief:       brief,
		standby:     opts.standby,
		leaseTTL:    opts.leaseTTL,
		heartbeat:   opts.heartbeat,
		backoff:     opts.backoff,
		reviewAgent: opts.reviewAgent,
		stdout:      cmd.OutOrStdout(),
		stderr:      cmd.ErrOrStderr(),
		runner:      weaveAutopilotRunnerDefault,
	})
	if err != nil {
		code := weavecli.ExitGenericFail
		if errors.Is(err, errWeaveAutopilotLeaseBusy) {
			code = weavecli.ExitStateConflict
		}
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave autopilot", code, err))
	}
	if mode == weavecli.OutputJSON {
		_ = emitOK(cmd.OutOrStdout(), mode, "weave autopilot", res)
		if res.PairExit != weavePairPassExit {
			return ec(res.PairExit)
		}
		return nil
	}
	if res.PairVerdict != "" {
		fmt.Fprintf(cmd.OutOrStdout(), "PAIR %s — %s\n", strings.ToUpper(res.PairVerdict), res.PairReason)
		if res.PairExit != weavePairPassExit {
			return ec(res.PairExit)
		}
	}
	label := res.Tool
	if res.Agent != "" {
		label = fmt.Sprintf("%s (%s)", res.Agent, res.Binding)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "weave autopilot: orchestrator %s exited cleanly\n", label)
	return nil
}

type weaveAutopilotLoopOptions struct {
	queueDir    string
	repoRoot    string
	fleet       []weaveMember
	brief       string
	standby     bool
	leaseTTL    time.Duration
	heartbeat   time.Duration
	backoff     time.Duration
	maxRuns     int
	stdout      io.Writer
	stderr      io.Writer
	runner      weaveAutopilotRunner
	now         func() time.Time
	sleep       func(time.Duration)
	holder      string
	reviewAgent string
}

type weaveAutopilotResult struct {
	Tool        string `json:"tool"`
	Agent       string `json:"agent,omitempty"`
	Binding     string `json:"binding,omitempty"`
	Runs        int    `json:"runs"`
	QueueDir    string `json:"queue_dir"`
	LastReason  string `json:"last_reason,omitempty"`
	PairVerdict string `json:"pair_verdict,omitempty"`
	PairReason  string `json:"pair_reason,omitempty"`
	PairExit    int    `json:"pair_exit,omitempty"`
}

var (
	weavePairJSONVerdict = regexp.MustCompile(`"pair_verdict"\s*:\s*"(pass|broken-before|refuted|harness-error)"`)
	weavePairJSONReason  = regexp.MustCompile(`"pair_reason"\s*:\s*"((?:\\.|[^"\\])*)"`)
)

func weavePairVerdictFromOutput(output string) (weavePairReviewResult, bool) {
	res := weavePairReviewResult{}
	if m := weavePairJSONVerdict.FindStringSubmatch(output); len(m) == 2 {
		res.Verdict = weavePairVerdict(m[1])
		if reason := weavePairJSONReason.FindStringSubmatch(output); len(reason) == 2 {
			if decoded, err := strconv.Unquote(`"` + reason[1] + `"`); err == nil {
				res.Reason = decoded
			}
		}
	} else {
		for _, line := range strings.Split(output, "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "PAIR ") {
				continue
			}
			head, reason, ok := strings.Cut(strings.TrimPrefix(line, "PAIR "), " — ")
			if !ok {
				continue
			}
			res.Verdict = weavePairVerdict(strings.ToLower(strings.TrimSpace(head)))
			res.Reason = strings.TrimSpace(reason)
			break
		}
	}
	res.ExitCode, _ = weavePairExitForVerdict(res.Verdict)
	if res.Verdict == "" || res.Reason == "" {
		return weavePairReviewResult{}, false
	}
	return res, true
}

// weaveAutopilotJudgePreflight is the autopilot-side twin of the pull merge
// loop's fail-closed pre-pass: it loads the durable queue and refuses to proceed
// when a submitted, verdict-required item cannot be matched to an eligible judge
// for the configured reviewer. A queue-load failure is itself fail-closed.
func weaveAutopilotJudgePreflight(queueDir, reviewAgent string) error {
	if strings.TrimSpace(reviewAgent) == "" {
		return nil
	}
	q, err := loadWeaveQueue(queueDir)
	if err != nil {
		return fmt.Errorf("weave autopilot: judge preflight could not read the queue: %w", err)
	}
	return weaveRequireEligibleJudge(q.Items, reviewAgent, 0, false)
}

func weaveRecordPairHarnessError(queueDir, reason string) {
	_ = withWeaveQueueLock(queueDir, func(q *weaveQueue) error {
		for _, it := range q.Items {
			if it.State != "submitted" {
				continue
			}
			it.PairVerdict = string(weavePairHarnessError)
			it.PairReason = reason
			it.PairExit = weavePairHarnessErrorExit
			it.NeedsSteward = true
			it.StewardReason = (weavePairReviewResult{Verdict: weavePairHarnessError, Reason: reason}).verdictLine()
		}
		return nil
	})
}

var errWeaveAutopilotLeaseBusy = errors.New("orchestrator lease is held")

func runWeaveAutopilotLoop(ctx context.Context, opts weaveAutopilotLoopOptions) (weaveAutopilotResult, error) {
	if opts.runner == nil {
		opts.runner = weaveAutopilotRunnerDefault
	}
	if opts.now == nil {
		opts.now = time.Now
	}
	if opts.sleep == nil {
		opts.sleep = time.Sleep
	}
	if opts.stdout == nil {
		opts.stdout = io.Discard
	}
	if opts.stderr == nil {
		opts.stderr = io.Discard
	}
	if opts.holder == "" {
		opts.holder = weaveAutopilotHolderID()
	}
	if opts.leaseTTL <= 0 {
		opts.leaseTTL = 30 * time.Second
	}
	if opts.heartbeat <= 0 {
		opts.heartbeat = 5 * time.Second
	}
	if opts.backoff <= 0 {
		opts.backoff = 10 * time.Second
	}

	index := 0
	runs := 0
	var lastReason string
	primaryProbeAfter := opts.now()
	for {
		if opts.maxRuns > 0 && runs >= opts.maxRuns {
			return weaveAutopilotResult{Runs: runs, QueueDir: opts.queueDir, LastReason: lastReason}, nil
		}
		// FAIL-CLOSED judge preflight. When review is in force, the autopilot must
		// not drive another merge run if any submitted, verdict-required item has no
		// eligible judge for the configured reviewer: it HALTS loudly and the
		// process exits non-zero rather than looping into a merge that can never
		// produce a verdict. Judge=="none" items are exempt (their probe suffices).
		if err := weaveAutopilotJudgePreflight(opts.queueDir, opts.reviewAgent); err != nil {
			return weaveAutopilotResult{Runs: runs, QueueDir: opts.queueDir, LastReason: lastReason}, err
		}
		member := opts.fleet[index]
		acquired, lease, err := acquireWeaveAutopilotLease(opts.queueDir, opts.holder, member, os.Getpid(), opts.leaseTTL, opts.now)
		if err != nil {
			return weaveAutopilotResult{}, err
		}
		if !acquired {
			if !opts.standby {
				return weaveAutopilotResult{}, fmt.Errorf("%w by %s until %s", errWeaveAutopilotLeaseBusy, lease.Holder, lease.ExpiresAt.Format(time.RFC3339))
			}
			wait := lease.ExpiresAt.Sub(opts.now())
			if wait <= 0 {
				wait = opts.heartbeat
			}
			if wait > opts.heartbeat {
				wait = opts.heartbeat
			}
			opts.sleep(wait)
			continue
		}

		leaseLog(opts.queueDir, "takeover", fmt.Sprintf("%s holder=%s generation=%d reason=%s", memberLogFields(member), opts.holder, lease.Generation, lastReasonOrInitial(lastReason)))
		prompt, err := buildWeaveAutopilotPrompt(opts.repoRoot, opts.queueDir, opts.brief, opts.reviewAgent)
		if err != nil {
			_ = releaseWeaveAutopilotLease(opts.queueDir, opts.holder)
			return weaveAutopilotResult{}, err
		}

		runCtx, cancel := context.WithCancel(ctx)
		hbDone := make(chan struct{})
		go func() {
			defer close(hbDone)
			ticker := time.NewTicker(opts.heartbeat)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					if err := renewWeaveAutopilotLease(opts.queueDir, opts.holder, member, opts.leaseTTL, opts.now); err != nil {
						fmt.Fprintf(opts.stderr, "weave autopilot: heartbeat failed: %v\n", err)
					}
				case <-runCtx.Done():
					return
				}
			}
		}()

		overload := false
		var runOutput strings.Builder
		exitCode, runErr := opts.runner.Run(runCtx, member, prompt, opts.queueDir, func(s string) {
			if s == "" {
				return
			}
			_, _ = io.WriteString(opts.stdout, s)
			runOutput.WriteString(s)
			if weaveAutopilotOverloaded(s) {
				overload = true
				cancel()
			}
		})
		cancel()
		<-hbDone
		runs++

		if opts.reviewAgent != "" {
			pairResult, hasPairVerdict := weavePairVerdictFromOutput(runOutput.String())
			if hasPairVerdict {
				if pairResult.Verdict != weavePairPass || (runErr == nil && exitCode == 0) {
					_ = releaseWeaveAutopilotLease(opts.queueDir, opts.holder)
					return weaveAutopilotResult{
						Tool: member.Tool, Agent: member.agentNick(), Binding: member.Binding(),
						Runs: runs, QueueDir: opts.queueDir, PairVerdict: string(pairResult.Verdict),
						PairReason: pairResult.Reason, PairExit: pairResult.ExitCode,
					}, nil
				}
			}
			if !overload && (runErr != nil || exitCode != 0) {
				reason := fmt.Sprintf("autopilot member %s exited without a pair verdict", member.Label())
				if hasPairVerdict {
					reason = fmt.Sprintf("autopilot member %s failed after pair PASS", member.Label())
				}
				if runErr != nil {
					reason += ": " + runErr.Error()
				} else {
					reason += fmt.Sprintf(" (exit %d)", exitCode)
				}
				weaveRecordPairHarnessError(opts.queueDir, reason)
				pairResult := weavePairReviewResult{Verdict: weavePairHarnessError, Reason: reason, ExitCode: weavePairHarnessErrorExit}
				fmt.Fprintln(opts.stdout, pairResult.verdictLine())
				_ = releaseWeaveAutopilotLease(opts.queueDir, opts.holder)
				return weaveAutopilotResult{
					Tool: member.Tool, Agent: member.agentNick(), Binding: member.Binding(),
					Runs: runs, QueueDir: opts.queueDir, PairVerdict: string(pairResult.Verdict),
					PairReason: pairResult.Reason, PairExit: pairResult.ExitCode,
				}, nil
			}
		}

		if overload {
			lastReason = "api-overload signature"
		} else if runErr != nil {
			lastReason = runErr.Error()
		} else if exitCode != 0 {
			lastReason = fmt.Sprintf("exit %d", exitCode)
		} else {
			_ = releaseWeaveAutopilotLease(opts.queueDir, opts.holder)
			return weaveAutopilotResult{
				Tool: member.Tool, Agent: member.agentNick(), Binding: member.Binding(),
				Runs: runs, QueueDir: opts.queueDir,
			}, nil
		}

		leaseLog(opts.queueDir, "failover", fmt.Sprintf("from=%s reason=%s", member.Label(), lastReason))
		_ = releaseWeaveAutopilotLease(opts.queueDir, opts.holder)

		if index == 0 {
			primaryProbeAfter = opts.now().Add(opts.backoff)
		}
		index = (index + 1) % len(opts.fleet)
		if index == 0 {
			opts.sleep(opts.backoff)
			continue
		}
		if opts.now().After(primaryProbeAfter) && opts.runner.Healthy(ctx, opts.fleet[0]) {
			leaseLog(opts.queueDir, "failback", fmt.Sprintf("from=%s to=%s boundary=between-runs", opts.fleet[index].Label(), opts.fleet[0].Label()))
			index = 0
		}
	}
}

func parseWeaveAutopilotFleet(s string) []string {
	var fleet []string
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			fleet = append(fleet, part)
		}
	}
	return fleet
}

func readWeaveAutopilotBrief(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read --brief: %w", err)
	}
	return string(b), nil
}

func weaveAutopilotHolderID() string {
	host, _ := os.Hostname()
	if host == "" {
		host = "unknown-host"
	}
	return host + ":" + strconv.Itoa(os.Getpid())
}

func lastReasonOrInitial(reason string) string {
	if reason == "" {
		return "initial"
	}
	return reason
}

func weaveAutopilotLeasePath(dir string) string {
	return filepath.Join(dir, weaveAutopilotLeaseFile)
}

func loadWeaveAutopilotLease(dir string) (weaveOrchestratorLease, bool, error) {
	b, err := os.ReadFile(weaveAutopilotLeasePath(dir))
	if errors.Is(err, os.ErrNotExist) {
		return weaveOrchestratorLease{}, false, nil
	}
	if err != nil {
		return weaveOrchestratorLease{}, false, err
	}
	var l weaveOrchestratorLease
	if err := json.Unmarshal(b, &l); err != nil {
		return weaveOrchestratorLease{}, false, fmt.Errorf("parse orchestrator lease: %w", err)
	}
	return l, true, nil
}

func saveWeaveAutopilotLease(dir string, l weaveOrchestratorLease) error {
	b, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	path := weaveAutopilotLeasePath(dir)
	tmp := path + ".tmp"
	if err := weaveWriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func acquireWeaveAutopilotLease(dir, holder string, m weaveMember, pid int, ttl time.Duration, now func() time.Time) (bool, weaveOrchestratorLease, error) {
	var out weaveOrchestratorLease
	acquired := false
	err := withWeaveQueueLock(dir, func(q *weaveQueue) error {
		_ = q
		cur, ok, err := loadWeaveAutopilotLease(dir)
		if err != nil {
			return err
		}
		t := now().UTC()
		if ok && cur.Holder != holder && cur.ExpiresAt.After(t) {
			out = cur
			return nil
		}
		gen := cur.Generation + 1
		if !ok {
			gen = 1
		}
		out = weaveOrchestratorLease{
			Holder:      holder,
			Tool:        m.Tool,
			Agent:       m.agentNick(),
			Binding:     m.Binding(),
			PID:         pid,
			AcquiredAt:  t,
			HeartbeatAt: t,
			ExpiresAt:   t.Add(ttl),
			Generation:  gen,
		}
		if err := saveWeaveAutopilotLease(dir, out); err != nil {
			return err
		}
		// A lease taken from an EXPIRED holder inherits their room. They are by
		// definition not coming back to close it, so the successor does —
		// otherwise the repo advertises a channel to somebody gone.
		if ok && cur.Holder != holder {
			_ = closeAutopilotRoom(dir, holder)
		}
		if cur.Holder != holder {
			_, _ = openAutopilotRoom(dir, holder)
		}
		acquired = true
		return nil
	})
	return acquired, out, err
}

func renewWeaveAutopilotLease(dir, holder string, m weaveMember, ttl time.Duration, now func() time.Time) error {
	return withWeaveQueueLock(dir, func(q *weaveQueue) error {
		_ = q
		cur, ok, err := loadWeaveAutopilotLease(dir)
		if err != nil {
			return err
		}
		if !ok || cur.Holder != holder {
			return fmt.Errorf("orchestrator lease not held by %s", holder)
		}
		t := now().UTC()
		cur.Tool = m.Tool
		cur.Agent, cur.Binding = m.agentNick(), m.Binding()
		cur.HeartbeatAt = t
		cur.ExpiresAt = t.Add(ttl)
		return saveWeaveAutopilotLease(dir, cur)
	})
}

func releaseWeaveAutopilotLease(dir, holder string) error {
	return withWeaveQueueLock(dir, func(q *weaveQueue) error {
		_ = q
		cur, ok, err := loadWeaveAutopilotLease(dir)
		if err != nil {
			return err
		}
		if !ok || cur.Holder != holder {
			return nil
		}
		// Releasing the lease closes the campaign room: a repo with no
		// driver and an open room is a channel nobody answers.
		_ = closeAutopilotRoom(dir, holder)
		return os.Remove(weaveAutopilotLeasePath(dir))
	})
}

func buildWeaveAutopilotPrompt(repoRoot, queueDir, brief string, reviewAgents ...string) (string, error) {
	reviewAgent := ""
	if len(reviewAgents) > 0 {
		reviewAgent = reviewAgents[0]
	}
	q, err := loadWeaveQueue(queueDir)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	if strings.TrimSpace(brief) != "" {
		b.WriteString(strings.TrimSpace(brief))
		b.WriteString("\n\n")
	}
	b.WriteString("You are the active bashy weave orchestrator.\n")
	b.WriteString("Resume from durable queue state. Do not assume prior in-memory context survived.\n\n")
	fmt.Fprintf(&b, "Repo root: %s\nQueue dir: %s\n\n", repoRoot, queueDir)
	b.WriteString("At safe top-of-loop boundaries, inspect the queue and run the normal weave gate/merge/launch flow. Never hand off mid-merge.\n\n")
	if reviewAgent != "" {
		fmt.Fprintf(&b, "Adversarial review was explicitly requested for this fleet: use `bashy weave pull <issue> --review-agent %s`. The pair writes evidence; configured deterministic gates decide.\n\n", reviewAgent)
	}
	b.WriteString("Current queue:\n")
	for _, it := range q.Items {
		fmt.Fprintf(&b, "- #%d [%s/%s] %s", it.ID, it.Priority, it.State, it.Title)
		if it.Tool != "" {
			fmt.Fprintf(&b, " tool=%s", it.Tool)
		}
		if it.Workspace != "" {
			fmt.Fprintf(&b, " workspace=%s", it.Workspace)
		}
		b.WriteByte('\n')
	}
	return b.String(), nil
}

func weaveAutopilotOverloaded(s string) bool {
	low := strings.ToLower(s)
	return strings.Contains(low, "529") ||
		strings.Contains(low, "overloaded") ||
		strings.Contains(low, "rate limit") ||
		strings.Contains(low, "rate_limit")
}

func leaseLog(dir, event, msg string) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	line := fmt.Sprintf("%s %s %s\n", time.Now().UTC().Format(time.RFC3339), event, msg)
	f, err := os.OpenFile(filepath.Join(dir, "autopilot.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(line)
}

// Healthy probes the member's EXECUTABLE. Its model was validated when the
// roster resolved; nothing about a model changes between runs, so failback
// asks only the question that can change: is the binary back on PATH.
func (weaveExecAutopilotRunner) Healthy(ctx context.Context, m weaveMember) bool {
	_, err := exec.LookPath(m.Bin)
	return err == nil
}

func (weaveExecAutopilotRunner) Run(ctx context.Context, m weaveMember, prompt, queueDir string, onOutput func(string)) (int, error) {
	args := m.headlessArgs()

	cmd := exec.CommandContext(ctx, m.Bin, args...)
	cmd.Dir = queueDir
	cmd.Stdin = strings.NewReader(prompt)
	env := make([]string, 0, len(os.Environ())+4)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "PWD=") || strings.HasPrefix(kv, "OLDPWD=") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "PWD="+queueDir, "WEAVE_ORCHESTRATOR=1", "WEAVE_QUEUE_DIR="+queueDir)
	// The orchestrator signs its own work too: it comments on issues and
	// commits. Stamp it with the principal it acts as, when it has one.
	cmd.Env = weaveAgentEnv(env, m.agent)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 1, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return 1, err
	}
	if err := cmd.Start(); err != nil {
		return 1, err
	}

	var wg sync.WaitGroup
	scan := func(r io.Reader) {
		defer wg.Done()
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			onOutput(sc.Text() + "\n")
		}
	}
	wg.Add(2)
	go scan(stdout)
	go scan(stderr)
	waitErr := cmd.Wait()
	wg.Wait()
	if waitErr != nil {
		if ee, ok := waitErr.(*exec.ExitError); ok {
			return ee.ExitCode(), nil
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			return 1, ctx.Err()
		}
		return 1, waitErr
	}
	return 0, nil
}

// seat expresses the orchestrator lease as the shared occupancy type.
//
// The stored record is unchanged — Generation, Tool, Agent and Binding are real
// and specific to a campaign, and a common type would have to drop them. What
// is shared is the VERDICT: given a holder, a heartbeat and a TTL, is this
// live, lapsed, or unanswerable.
//
// The TTL is derived from the record rather than taken from a constant, because
// this lease stores its own ExpiresAt: the holder chose the window, and reading
// it back is the only way to honour the one they actually chose.
func (l weaveOrchestratorLease) seat() role.Seat {
	ttl := time.Duration(0)
	if !l.ExpiresAt.IsZero() && !l.HeartbeatAt.IsZero() {
		ttl = l.ExpiresAt.Sub(l.HeartbeatAt)
	}
	return role.Seat{
		Holder:      l.Holder,
		AcquiredAt:  l.AcquiredAt,
		HeartbeatAt: l.HeartbeatAt,
		TTL:         ttl,
	}
}

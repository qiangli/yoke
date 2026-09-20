package weave

// DRAINING — stopping a sprint so the next one can START FROM WHERE IT LEFT OFF.
//
// The reason this exists is preemption: something more urgent arrives, the
// current work must yield, and it must yield in a state somebody can pick up
// cold. That is a different act from `abort` (kill everything, salvage nothing)
// and from a bare `stop` (close the clock and hope).
//
// A stop is not graceful because it was polite. It is graceful because three
// things are TRUE when it finishes:
//
//  1. No worker is still running. Wrappers are stopped the way `weave pause`
//     stops them — workspace and branch preserved — so the work is parked, not
//     destroyed.
//  2. The tree COMPILES AND ITS TESTS PASS. This is the minimum bar, and it is
//     the one that makes resumption possible at all: inheriting a broken tree
//     means the next sprint spends its first hour on damage from the last one,
//     without knowing which damage is theirs.
//  3. There is a continuity record. "Where it left off" is a written sentence,
//     not an inference from a diff.
//
// # A red gate REFUSES to close the sprint
//
// This is the hard rule, and it is deliberately inconvenient. If the gate
// fails, the sprint stays open in DRAINING: workers are already parked, so
// nothing is burning, and the conductor's remaining job is narrow and clear —
// fix the regression, run `sprint stop` again. Closing over a red gate would
// file the sprint as done and hand the next one a mess whose origin is no
// longer visible, which is the exact failure this feature was asked to prevent.
//
// # No gate is NOT a pass
//
// `gate.Outcome.Ran` exists for precisely this. A drain with no gate command
// cannot certify anything, so it does not: the box records that it closed
// unverified, and says so out loud. Absence of evidence is never success —
// the same rule the fleet evidence invariant states for runs.
//
// # WHY THE WORST CASE IS SAFE: sprint work goes through weave, never direct
//
// Every task a sprint plans is executed by `weave` — an isolated workspace on
// its own branch. No agent edits a repo's working tree directly. That single
// rule is what makes a hard stop survivable, and it is worth stating because
// the whole refusal above rests on it:
//
//   - Unmerged work CANNOT break the tree. A branch that was never merged has
//     no bearing on whether main builds, so parking it costs nothing and risks
//     nothing.
//   - Therefore a RED GATE MEANS SOMETHING ALREADY LANDED. The regression is
//     in merged code, which is exactly the case that must be fixed before the
//     sprint closes — it is the next sprint's inheritance either way.
//   - And the worst case has an exit that is not "close over a broken tree":
//     leave the weave unmerged (it is already parked), or `weave abandon` it
//     and lose only that branch. Main is untouched in both.
//
// So the escape hatch for a failing gate is never --force. It is to drop the
// offending work, which weave makes a local and reversible decision rather than
// a repo-wide one.
//
// # What draining honestly cannot promise
//
// Stopping a wrapper stops the PROCESS. Whether the agent inside had reached a
// tidy internal moment is not observable from here, and pretending otherwise
// would be the kind of claim this package exists to avoid making. What is
// guaranteed is the part that survives a process: the branch, the workspace,
// the gate verdict, and the continuity note.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/gate"
	"github.com/qiangli/yoke/pkg/kb"
	"github.com/qiangli/yoke/pkg/weave/memory"
)

// drainReport is what one drain attempt observed, per linked repo and overall.
type drainReport struct {
	Paused     []string // "repo (2 workers)" — what was parked
	GateRan    bool
	GatePassed bool
	GateCmd    string
	GateOutput string
	Failures   []string // repos whose gate failed
	Repos      []repoState
}

// Clean reports a drain that satisfies every condition for a good handoff: a
// gate ran, and it passed. Anything else is either unverified or broken, and
// neither may be recorded as a clean stop.
func (r *drainReport) Clean() bool { return r.GateRan && r.GatePassed }

// pauseLinkedRepos stops running workers in every repo this sprint links,
// reusing weave's own pause semantics so a drained worker is parked exactly the
// way a paused one is — same preserved workspace, same preserved branch.
//
// A repo whose queue cannot be found is REPORTED, not skipped silently: a
// sprint that thinks it parked three repos and parked two is worse than one
// that admits it, because the unparked worker keeps writing to a tree the next
// sprint believes is quiet.
func pauseLinkedRepos(s *weaveStory) (paused []string, problems []string) {
	type linkedQueue struct {
		repo string
		dir  string
		ids  map[int64]bool
	}
	queues := map[string]*linkedQueue{}
	var order []string
	for _, r := range s.Runs {
		if r.Repo == "" {
			continue
		}
		dir, err := weaveQueueDirForSprintRun(r)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s#%d: %v", r.Repo, r.ID, err))
			continue
		}
		key := filepath.Clean(dir)
		group := queues[key]
		if group == nil {
			group = &linkedQueue{repo: r.Repo, dir: dir, ids: map[int64]bool{}}
			queues[key] = group
			order = append(order, key)
		}
		group.ids[r.ID] = true
	}
	for _, key := range order {
		group := queues[key]
		// ONLY THIS SPRINT'S RUNS. Parking is per-queue at the weave layer, so
		// pausing a whole repo would stop ANOTHER running sprint's workers —
		// the exact cross-sprint damage the shared-repo exemption exists to
		// avoid one check later. A sprint may stop its own work and nobody
		// else's.
		n, err := pauseWorkersIn(group.dir, group.ids)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", group.repo, err))
			continue
		}
		paused = append(paused, fmt.Sprintf("%s (%d worker(s))", group.repo, n))
	}
	return paused, problems
}

// pauseWorkersIn stops the running wrappers for the GIVEN runs in one queue,
// leaving every other worker in that repo alone.
//
// The filter is the whole point. Two sprints can share a repo, and a drain that
// paused the queue would stop work its sprint does not own — silently, since a
// paused worker looks the same however it got there.
func pauseWorkersIn(dir string, only map[int64]bool) (int, error) {
	var stopped int
	q, err := loadWeaveQueue(dir)
	if err != nil {
		return 0, err
	}
	for _, it := range q.Items {
		if it.State != "working" || !only[it.ID] {
			continue
		}
		if it.WrapperPid > 0 && pidAlive(it.WrapperPid) {
			weaveStopWrapper(it.WrapperPid)
			stopped++
		}
	}
	// Persist the parked state after the processes have stopped. Merely killing
	// the wrappers while leaving queue.json at "working" makes a handoff look
	// live forever and gives the successor no truthful resume operation.
	if err := withWeaveQueueLock(dir, func(q *weaveQueue) error {
		found := make(map[int64]bool, len(only))
		for _, it := range q.Items {
			if !only[it.ID] {
				continue
			}
			found[it.ID] = true
			if it.State == "allocated" {
				return fmt.Errorf("run #%d is allocated but has no resumable worker state", it.ID)
			}
			if it.State != "working" {
				continue
			}
			if it.Workspace != "" {
				if out, err := exec.Command(gitBin(), "-C", it.Workspace, "rev-parse", "HEAD").Output(); err == nil {
					it.Head = strings.TrimSpace(string(out))
				}
			}
			it.State = "paused"
			it.WrapperPid = 0
			it.CtlSock = ""
		}
		for id := range only {
			if !found[id] {
				return fmt.Errorf("linked run #%d is absent from its weave queue", id)
			}
		}
		return nil
	}); err != nil {
		return stopped, err
	}
	return stopped, nil
}

// runDrainGate settles the minimum bar: does the tree still build and pass.
//
// The command is the caller's, because only they know what "compiles and
// tests" means for their tree. When none is given nothing is run and Ran stays
// false — which the caller must treat as UNVERIFIED, never as a pass.
type drainGateOutcome struct {
	gate.Outcome
	GateEventID string
}

func runDrainGate(ctx context.Context, dir, command string) drainGateOutcome {
	if strings.TrimSpace(command) == "" {
		return drainGateOutcome{Outcome: gate.Outcome{Ran: false}}
	}
	if dir == "" {
		dir, _ = os.Getwd()
	}
	out := gate.RunLocal(ctx, dir, command, "")
	if err := ctx.Err(); err != nil {
		out.Passed = false
		out.Output = strings.TrimSpace(out.Output + "\nsprint gate: " + err.Error())
	}
	result := drainGateOutcome{Outcome: out}
	eventID, err := observeDrainGate(out)
	if err != nil {
		result.Output = strings.TrimSpace(result.Output + "\nkb observe: " + err.Error())
		return result
	}
	result.GateEventID = eventID
	if err := rememberDrainGate(context.Background(), dir, out, eventID); err != nil {
		result.Output = strings.TrimSpace(result.Output + "\nweave memory: " + err.Error())
	}
	return result
}

// observeDrainGate goes through kb's command API so the C4 event writer remains
// the sole implementation of journal shape and event IDs.
func observeDrainGate(out gate.Outcome) (string, error) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	args := []string{
		"--dir", kb.DefaultDir(), "observe",
		"--episode", "weave-drain",
		"--kind", "gate",
		"--ref", "gate:" + now,
		"--summary", out.Summary(),
		"--at", now,
		"--command", out.Command,
		"--exit-code", strconv.Itoa(out.ExitCode),
		"--where", out.Where,
	}
	if out.Ran {
		args = append(args, "--ran")
	}
	if out.Passed {
		args = append(args, "--passed")
	}
	cmd := kb.NewKBCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		if detail := strings.TrimSpace(stderr.String()); detail != "" {
			return "", fmt.Errorf("%w: %s", err, detail)
		}
		return "", err
	}
	id := strings.TrimSpace(stdout.String())
	if id == "" {
		return "", fmt.Errorf("kb observe returned no event id")
	}
	return id, nil
}

func rememberDrainGate(ctx context.Context, repoRoot string, out gate.Outcome, eventID string) error {
	queueDir, err := weaveQueueDir(repoRoot)
	if err != nil {
		return err
	}
	st, _, err := memory.Open(queueDir, memory.Prefs{})
	if err != nil {
		return err
	}
	return st.Remember(ctx, memory.Observation{
		Outcome: "gate", GateExit: out.ExitCode, GateEventID: eventID,
		Summary: out.Summary(), CreatedAt: time.Now().UTC(),
	})
}

// drainSummary renders what happened, in the order a reader needs it: the
// blocking problem first, the evidence second.
func drainSummary(r *drainReport, elapsed, planned time.Duration) string {
	return drainEvidenceSummary(r) + fmt.Sprintf("; ran %s of %s", roundDur(elapsed), roundDur(planned))
}

// drainEvidenceSummary reports only evidence the drain actually observed. It
// is used by `sprint end` for legacy/unboxed work, where inventing elapsed and
// planned durations at close time would corrupt the cadence record.
func drainEvidenceSummary(r *drainReport) string {
	var b strings.Builder
	if len(r.Paused) > 0 {
		// The parked branches ARE the resumption pointer — "where it left off"
		// is a workspace and a branch per repo, not a feeling.
		fmt.Fprintf(&b, "parked %s (unmerged, resumable); ", strings.Join(r.Paused, ", "))
	}
	switch {
	case !r.GateRan:
		b.WriteString("NO GATE RAN — the stop is unverified")
	case r.GatePassed:
		b.WriteString("gate green")
	default:
		b.WriteString("GATE FAILED")
	}
	if n := len(r.Repos); n > 0 {
		clean := 0
		for i := range r.Repos {
			if r.Repos[i].OK() {
				clean++
			}
		}
		fmt.Fprintf(&b, "; %d/%d repo(s) wrapped up", clean, n)
	}
	return b.String()
}

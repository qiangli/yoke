package weave

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// THE REAPER — the pass that makes the lifecycle invariant true at RUNTIME.
//
// weave_lifecycle.go declares that every state has a determinate exit. This
// file is what actually fires those exits. It runs on every `weave list`,
// `weave status`, `weave doctor` and on each heartbeat tick, holds the queue
// lock once, and gives every limbo a determinate outcome:
//
//	allocated, launcher dead        -> failed  (durable provisioning evidence)
//	finalizing, lease expired       -> working (wrapper alive) or failed
//	working, wrapper pid dead       -> failed:wrapper-died
//	submitted, already merged       -> done
//	submitted, past threshold       -> FLAG needs-steward (state unchanged)
//	failed/killed with commits      -> FLAG salvageable   (state unchanged)
//
// Two rules constrain it absolutely:
//
//  1. IT NEVER DESTROYS COMMITTED WORK. The reaper writes state fields and
//     flags; it removes no workspace, no branch and no commit. Disposal stays
//     an explicit guarded step (`weave prune`, `weave abandon`).
//  2. IT NEVER INVENTS SUCCESS. A dead wrapper becomes `failed`, never
//     `submitted` — success still requires a clean exit AND measured commits.
//     Where work exists behind a failure, the reaper SURFACES it (salvageable)
//     rather than promoting it.
//
// It is idempotent by construction: every rule's precondition is falsified by
// the write it performs, so a second pass over a reaped queue returns no
// actions.

// weaveStewardThreshold is how long a `submitted` item may sit unmerged before
// the reaper declares it a pending steward decision. Generous on purpose: a
// conductor running a fleet legitimately batches pulls, and a flag that fires
// during normal operation is a flag nobody reads. A package var, not a const,
// so tests can compress it.
var weaveStewardThreshold = 30 * time.Minute

// weaveReapAction is one determinate outcome the reaper applied. `To` equals
// `From` for the flag-only rules — the state was already correct, what was
// missing was the visible decision it implies.
type weaveReapAction struct {
	Issue  int64  `json:"issue"`
	From   string `json:"from"`
	To     string `json:"to"`
	Reason string `json:"reason"`
	// Flag names the non-state determinacy the reaper attached, if any:
	// "salvageable" or "needs-steward".
	Flag string `json:"flag,omitempty"`
}

func (a weaveReapAction) String() string {
	if a.From == a.To {
		return fmt.Sprintf("#%d %s [%s] %s", a.Issue, a.From, a.Flag, a.Reason)
	}
	return fmt.Sprintf("#%d %s -> %s: %s", a.Issue, a.From, a.To, a.Reason)
}

// weaveReapPass applies every reaper rule to an in-memory queue and returns
// what it changed. Split out from the locked wrapper so it is directly
// testable and so callers that already hold the lock can reuse it.
//
// `root` is the user repo (may be "" — the merge check is then skipped) and
// `base` its base branch.
func weaveReapPass(q *weaveQueue, root, base string, now time.Time) []weaveReapAction {
	if q == nil {
		return nil
	}
	var actions []weaveReapAction
	// Order matters: finalization recovery may hand an item back to
	// "working", and the dead-wrapper rule must then judge that fresh state.
	actions = append(actions, weaveReapOrphanedAllocations(q, now)...)
	actions = append(actions, weaveReapAbandonedFinalizations(q, now)...)
	actions = append(actions, weaveReapDeadWrappers(q, now)...)
	actions = append(actions, weaveReapSubmissions(q, root, base, now)...)
	actions = append(actions, weaveReapSalvageable(q, root, base)...)
	return actions
}

// weaveReapQueue is the locked, persisting entry point. Safe to call on any
// read path: with nothing to reap it takes the lock, finds no change and
// writes the queue back unchanged.
//
// OPPORTUNISTIC BY DESIGN. This runs on the read paths (`weave list`, `weave
// show`, the heartbeat), and a read must never wait on a writer — that is what
// made the board unreadable while a merge ran. So it waits only
// weaveReapLockWait for the lock and, if a writer holds it, returns no actions
// and no error: the caller proceeds with a lock-free read of the queue. The
// pass is idempotent, so whatever needed reaping gets reaped on the next read.
// Callers that actually need the reap to have happened (weave doctor) use
// weaveReapQueueBlocking.
func weaveReapQueue(dir, root, base string) ([]weaveReapAction, error) {
	actions, err := weaveReapQueueWait(dir, root, base, weaveReapLockWait)
	if errors.Is(err, errWeaveQueueBusy) {
		return nil, nil
	}
	return actions, err
}

// weaveReapQueueBlocking waits the ordinary write patience for the lock. For
// callers whose whole purpose is the reap.
func weaveReapQueueBlocking(dir, root, base string) ([]weaveReapAction, error) {
	return weaveReapQueueWait(dir, root, base, weaveQueueLockWait)
}

func weaveReapQueueWait(dir, root, base string, wait time.Duration) ([]weaveReapAction, error) {
	// list/doctor/board call the reaper while rendering a snapshot. With no
	// queue there is nothing to reconcile, and taking queue.lock would create
	// the very per-repository state a read-only command is inspecting.
	if _, err := os.Stat(filepath.Join(dir, "queue.json")); os.IsNotExist(err) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var actions []weaveReapAction
	err := withWeaveQueueLockWait(dir, wait, func(q *weaveQueue) error {
		actions = weaveReapPass(q, root, base, time.Now().UTC())
		for _, a := range actions {
			if a.To == "failed" || a.To == "done" {
				weaveQueueOwnerNotice(dir, q, findWeaveItem(q, a.Issue), "run-reconciled")
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	weaveDeliverOwnerNotices(dir)
	return actions, nil
}

// weaveReapOrphanedAllocations turns a pre-agent allocation whose launcher died
// into durable terminal evidence. Unlike a working run, provisioning has no
// subagent to report failure. Legacy records did not persist the launcher PID,
// so an aged active provisioning phase is also an orphan; intentionally
// no-spawn/manual allocations have no start time and are preserved.
func weaveReapOrphanedAllocations(q *weaveQueue, now time.Time) []weaveReapAction {
	var actions []weaveReapAction
	for _, it := range q.Items {
		if !weaveAllocatedLaunchOrphaned(it, now) {
			continue
		}
		const reason = "provisioning launcher exited before agent launch"
		it.State = "failed"
		it.LaunchPhase = "failed: " + reason
		it.FinishedAt = now
		it.WrapperPid = 0
		it.CtlSock = ""
		weaveAppendComment(it, "reaper", "system", "reaped allocated -> failed: "+reason)
		actions = append(actions, weaveReapAction{Issue: it.ID, From: "allocated", To: "failed", Reason: reason})
	}
	return actions
}

// weaveReapAbandonedFinalizations releases the short finalizer lease when its
// conductor disappears. A surviving wrapper resumes ownership of the run; once
// both are gone, failed state preserves the workspace for explicit salvage
// rather than leaving phantom capacity in finalizing forever.
func weaveReapAbandonedFinalizations(q *weaveQueue, now time.Time) []weaveReapAction {
	var actions []weaveReapAction
	for _, it := range q.Items {
		if !weaveWrapperTerminalClaimed(it) {
			continue
		}
		expired := it.FinalizingAt.IsZero() || now.Sub(it.FinalizingAt) >= weaveFinalizationLease
		if !expired && it.FinalizerPID > 0 && pidAlive(it.FinalizerPID) {
			continue
		}
		it.FinalizerPID = 0
		it.FinalizingAt = time.Time{}
		if it.WrapperPid > 0 && pidAlive(it.WrapperPid) {
			it.State = "working"
			it.Completion = ""
			actions = append(actions, weaveReapAction{Issue: it.ID, From: "finalizing", To: "working",
				Reason: "finalizer lease expired; wrapper still alive"})
			continue
		}
		const reason = "finalizer exited before terminal evidence"
		it.State = "failed"
		it.Completion = "failed: " + reason
		it.FinishedAt = now
		it.WrapperPid = 0
		it.CtlSock = ""
		weaveAppendComment(it, "reaper", "system", "reaped finalizing -> failed: "+reason)
		actions = append(actions, weaveReapAction{Issue: it.ID, From: "finalizing", To: "failed", Reason: reason})
	}
	return actions
}

// weaveWorkspaceLive answers the only question that makes `weave start
// --resume` viable: is the run's workspace STILL A DIRECTORY ON DISK?
//
// It deliberately mirrors the precondition `weave start --resume` enforces
// (recorded path, and that path stats) so recovery advice and the command it
// advertises can never disagree. Everything that writes "workspace preserved"
// or derives a NEXT STEP must ask this first — asserting a preserved workspace
// without checking is how a run becomes unrecoverable-by-instruction.
func weaveWorkspaceLive(it *weaveItem) bool {
	if it == nil || it.Workspace == "" {
		return false
	}
	st, err := os.Stat(it.Workspace)
	return err == nil && st.IsDir()
}

// weaveFreshStartHint is the recovery command for a run with no workspace left:
// re-run the same issue into a newly provisioned one. `--resume` cannot serve
// here — it refuses without a workspace — so it must never be advertised.
func weaveFreshStartHint(id int64) string {
	return fmt.Sprintf("`weave start --run %d -- <agent>` to start fresh", id)
}

// weaveReapDeadWrappers is the rule the observed limbo needed most.
//
// A `working` item whose wrapper PID is gone has, by definition, no process
// left that could ever write its terminal state. Before this it stayed
// "working" forever — `weave list` printed "(stale — wrapper pid dead)" next to
// runs three and five hours dead, `weave wait --all` blocked on them, and the
// scheduler counted them as live capacity. The stale marker was honest and
// completely inert: it described the limbo instead of ending it.
//
// So end it. failed, with the cause named. The workspace, branch and commits
// are untouched by the reaper — where the workspace is still on disk,
// `weave start --resume` reattaches to the failed run; where it is NOT (the
// wrapper died mid-teardown, or the workspace was pruned), the comment says so
// and names the fresh-start command, because --resume would refuse. Either
// way weaveReapSalvageable will surface any work the dead wrapper never got to
// report.
func weaveReapDeadWrappers(q *weaveQueue, now time.Time) []weaveReapAction {
	var actions []weaveReapAction
	for _, it := range q.Items {
		// WrapperPid == 0 is a manual/no-spawn run weave never supervised:
		// there is no death to detect, and inventing one would terminalize
		// somebody's hand-driven session.
		if it.State != "working" || it.WrapperPid <= 0 || pidAlive(it.WrapperPid) {
			continue
		}
		const reason = "wrapper-died: wrapper process exited without recording terminal evidence"
		it.State = "failed"
		it.Completion = "failed: " + reason
		if it.FinishedAt.IsZero() {
			it.FinishedAt = now
		}
		it.WrapperPid = 0
		it.CtlSock = ""
		it.Stale = false
		// Only claim preservation the reaper actually verified. A wrapper that
		// died mid-teardown (or a workspace pruned since) leaves nothing to
		// reattach to, and `--resume` refuses — so in that case name the
		// fresh-start command instead of a dead end.
		recovery := "workspace preserved, `weave start --resume --issue " + fmt.Sprint(it.ID) + "` to retry"
		if !weaveWorkspaceLive(it) {
			recovery = "workspace lost (nothing to reattach), " + weaveFreshStartHint(it.ID)
		}
		weaveAppendComment(it, "reaper", "system", "reaped working -> failed ("+reason+"); "+recovery)
		actions = append(actions, weaveReapAction{Issue: it.ID, From: "working", To: "failed", Reason: reason})
	}
	return actions
}

// weaveReapSubmissions gives `submitted` — the one state that could hang
// indefinitely with NOTHING watching it — a determinate exit in both
// directions:
//
//   - the work already landed in base (a manual merge, a peer weave, an
//     earlier pull): git says so, so the item becomes done. This is
//     weaveReconcileMerged's rule, made durable instead of display-only.
//   - it did not land and the item is past the steward threshold: it is
//     flagged needs-steward with the reason it cannot auto-close. Merging is
//     NOT automated here on purpose — `weave pull` owns the verify/review/
//     isolation gates, and a reaper that merged around them would be a reaper
//     that ships unverified work.
func weaveReapSubmissions(q *weaveQueue, root, base string, now time.Time) []weaveReapAction {
	var actions []weaveReapAction
	for _, it := range q.Items {
		if it.State != "submitted" {
			continue
		}
		if root != "" && base != "" && weaveItemMerged(root, base, it) {
			it.State = "done"
			it.Disposition = weaveDispositionMerged
			it.NeedsSteward = false
			it.StewardReason = ""
			actions = append(actions, weaveReapAction{Issue: it.ID, From: "submitted", To: "done",
				Reason: "work is already contained in " + base})
			continue
		}
		age := now.Sub(weaveSubmittedSince(it))
		if age < weaveStewardThreshold {
			continue
		}
		reason := weaveSubmittedStewardReason(it, age)
		if it.NeedsSteward && it.StewardReason == reason {
			continue // already determinate — keep the pass idempotent
		}
		it.NeedsSteward = true
		it.StewardReason = reason
		actions = append(actions, weaveReapAction{Issue: it.ID, From: "submitted", To: "submitted",
			Flag: "needs-steward", Reason: reason})
	}
	return actions
}

// weaveSubmittedSince is when the item entered its waiting-to-be-pulled life:
// the wrapper's terminal timestamp, falling back to start/creation for records
// written before FinishedAt existed.
func weaveSubmittedSince(it *weaveItem) time.Time {
	if !it.FinishedAt.IsZero() {
		return it.FinishedAt
	}
	if !it.StartedAt.IsZero() {
		return it.StartedAt
	}
	return it.Created
}

// weaveSubmittedStewardReason names WHY this submission cannot simply be
// pulled — the flag is only useful if it says what decision is owed.
func weaveSubmittedStewardReason(it *weaveItem, age time.Duration) string {
	waited := fmt.Sprintf("unmerged for %s", weaveRoundDuration(age))
	switch {
	case it.IsolationViolated:
		return waited + "; isolation violated — `weave pull --force` or `weave abandon`"
	case it.VerifyExit != nil && *it.VerifyExit != 0:
		return fmt.Sprintf("%s; verify failed (exit %d) — `weave reverify` or `weave abandon`", waited, *it.VerifyExit)
	case it.ReviewBlocking:
		return waited + "; review is blocking — resolve the review or `weave abandon`"
	default:
		return waited + "; no merge detected — `weave pull " + fmt.Sprint(it.ID) + "` or `weave abandon`"
	}
}

func weaveRoundDuration(d time.Duration) time.Duration {
	if d >= time.Hour {
		return d.Round(time.Minute)
	}
	return d.Round(time.Second)
}

// weaveReapSalvageable marks a stopped run that is sitting on committed work.
//
// This is the fleet-evidence rule inverted. CommitsAhead is written by the
// WRAPPER — which, in exactly the interesting case, is the process that died.
// A tool that commits its changes and then crashes on the way out never reaches
// the evidence-recording step, so the queue records commits_ahead=0 while the
// branch carries a complete feature. Concluding "no work" from the absence of a
// report by a process that did not survive to report is the same mistake in a
// different costume, so: ask the artifact. It is right there on disk.
//
// The state is NOT changed — a crash is a crash — but "failed, and there is
// finished work on the branch" is a determinate steward decision, where plain
// "failed" was an invitation to redo the work for free.
func weaveReapSalvageable(q *weaveQueue, root, base string) []weaveReapAction {
	var actions []weaveReapAction
	for _, it := range q.Items {
		if it.State != "failed" && it.State != "killed" {
			continue
		}
		if it.Salvageable {
			continue // idempotent: already determinate
		}
		if it.Workspace == "" {
			continue
		}
		// Same classifier `weave list` displays with: it counts commits that
		// are genuinely UNMERGED (not reachable from base in the user repo),
		// not merely ahead of the recorded baseline. A run whose work already
		// landed by some other route is not salvageable, and flagging it would
		// send the steward to re-merge work that is already in main.
		salvageable, unmerged := weaveClassifySalvageable(root, base, it)
		if !salvageable {
			continue
		}
		it.Salvageable, it.UnmergedCommits = true, unmerged
		reason := fmt.Sprintf("%d commit(s) on the branch survive the %s run — inspect them, then `weave salvage %d`; do NOT re-run", unmerged, it.State, it.ID)
		actions = append(actions, weaveReapAction{Issue: it.ID, From: it.State, To: it.State,
			Flag: "salvageable", Reason: reason})
	}
	return actions
}

// weaveLimboItems reports items still sitting in a non-closed state along with
// what the machine says will move each one. `weave doctor` renders it; the
// point is that the answer is never "nothing".
func weaveLimboItems(q *weaveQueue) []*weaveItem {
	var out []*weaveItem
	for _, it := range q.Items {
		if !weaveIsClosedState(it.State) {
			out = append(out, it)
		}
	}
	return out
}

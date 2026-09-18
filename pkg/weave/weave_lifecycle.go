package weave

// THE LIFECYCLE INVARIANT
//
// Every weave item has a COMPLETE lifecycle: create -> process -> close.
// No item may sit in a state that is not a determinate step toward closure.
//
// This is the runtime twin of the fleet-evidence rule ("no success-state is
// reached by the ABSENCE of evidence") applied to LIFECYCLE: a state whose only
// exit is "somebody happens to notice" is a limbo, and limbo is how work gets
// silently lost. Observed live, each of these hung forever:
//
//   - `working` with a DEAD wrapper pid — displayed "(stale — wrapper pid dead)"
//     for hours; nothing ever transitioned it.
//   - `allocated` whose launcher died before the agent ever started.
//   - `finalizing` whose conductor disappeared mid-claim.
//   - `submitted` that nobody ever pulled — indistinguishable from in-flight work.
//   - `failed`/`killed` sitting on a branch full of finished, committed work,
//     reading exactly like a run that achieved nothing.
//
// The table below is the WHOLE state machine: every state, every legal
// transition, and who fires it. It is not documentation-that-drifts — it is the
// declaration the reaper and TestLifecycleHasNoLimbo are both checked against,
// so "no limbo" is a property this package can actually verify. The prose
// version lives in docs/weave-lifecycle-state-machine.md.

// weaveLifecycleStates is every state an item's State field may hold.
var weaveLifecycleStates = []string{
	"todo",
	"allocated",
	"working",
	"paused",
	"finalizing",
	"submitted",
	"no-op",
	"failed",
	"killed",
	"done",
	"abandoned",
}

// weaveClosedStates are the two states that END the lifecycle: the work landed
// (done) or was deliberately given up (abandoned). Note this is NARROWER than
// isTerminalState, which additionally counts submitted/no-op/failed/killed — those
// mean "the run stopped", not "the item is closed", and conflating the two is
// precisely what let a submitted item hang forever.
func weaveIsClosedState(s string) bool { return s == "done" || s == "abandoned" }

// weaveStateAssertsSuccess centralizes the success-shaped states used by
// evidence ratchets. "merged" is retained for legacy/reporting outcomes even
// though the queue lifecycle now calls that state "done".
func weaveStateAssertsSuccess(s string) bool {
	return s == "submitted" || s == "done" || s == "merged"
}

// weaveTransition is one legal edge of the state machine.
type weaveTransition struct {
	From string
	To   string
	// By names what fires the edge — a verb the steward runs, the wrapper
	// process itself, or the reaper.
	By string
	// Auto is true when the substrate fires the edge with no human in the
	// loop. Every non-closed state must have at least one Auto edge OR be a
	// state the reaper flags for a determinate steward decision (see
	// weaveLifecycleNeedsSteward).
	Auto bool
}

// weaveLifecycleTransitions is the complete legal transition set.
var weaveLifecycleTransitions = []weaveTransition{
	// --- creation and claim -------------------------------------------
	// Auto: `weave autopilot` / the heartbeat claim todo items unattended;
	// `weave start` is the same edge fired by hand.
	{From: "todo", To: "allocated", By: "weave start / autopilot claim + provision workspace", Auto: true},
	{From: "todo", To: "abandoned", By: "weave abandon"},

	// --- provisioning --------------------------------------------------
	{From: "allocated", To: "working", By: "wrapper launches the agent", Auto: true},
	{From: "allocated", To: "failed", By: "weave start (provisioning error)", Auto: true},
	{From: "allocated", To: "failed", By: "REAPER: launcher died / provisioning timed out", Auto: true},

	// --- running -------------------------------------------------------
	{From: "working", To: "submitted", By: "wrapper terminal: exit 0 AND commits ahead", Auto: true},
	{From: "working", To: "no-op", By: "wrapper terminal: exit 0, clean tree, and no commits", Auto: true},
	{From: "working", To: "failed", By: "wrapper terminal: non-zero exit or uncommitted tree", Auto: true},
	{From: "working", To: "killed", By: "weave kill / runtime+idle watchdog", Auto: true},
	{From: "working", To: "failed", By: "REAPER: wrapper pid dead without terminal evidence", Auto: true},
	{From: "working", To: "paused", By: "weave pause"},
	{From: "working", To: "finalizing", By: "weave finalize (conductor claims an idle TUI)"},

	// --- paused --------------------------------------------------------
	{From: "paused", To: "working", By: "weave resume / weave start --resume"},
	{From: "paused", To: "abandoned", By: "weave abandon"},

	// --- finalizing (short lease) ---------------------------------------
	{From: "finalizing", To: "submitted", By: "finalizer records terminal evidence", Auto: true},
	{From: "finalizing", To: "no-op", By: "finalizer records a clean tree with no commits", Auto: true},
	{From: "finalizing", To: "failed", By: "finalizer records terminal evidence", Auto: true},
	{From: "finalizing", To: "working", By: "REAPER: lease expired, wrapper still alive", Auto: true},
	{From: "finalizing", To: "failed", By: "REAPER: finalizer died before terminal evidence", Auto: true},

	// --- submitted (work exists, awaiting absorption) --------------------
	{From: "submitted", To: "done", By: "weave pull (merge into base)"},
	{From: "submitted", To: "done", By: "REAPER: work already merged into base out-of-band", Auto: true},
	{From: "submitted", To: "working", By: "weave start --resume (branch kicked back)"},
	{From: "submitted", To: "abandoned", By: "weave abandon"},

	// --- stopped-with-no-evidence -----------------------------------------
	{From: "no-op", To: "working", By: "weave start --resume"},
	{From: "no-op", To: "allocated", By: "weave start --run N -- <agent> (no workspace to resume)"},
	{From: "no-op", To: "abandoned", By: "weave abandon / weave prune --stale"},

	// --- stopped-with-a-branch -------------------------------------------
	{From: "failed", To: "working", By: "weave start --resume"},
	{From: "failed", To: "done", By: "weave salvage + weave pull"},
	{From: "failed", To: "abandoned", By: "weave abandon / weave prune --stale"},
	// --resume needs a workspace to reattach to. Without one the run is still
	// recoverable, by re-provisioning: `weave start --run N -- <agent>`.
	{From: "failed", To: "allocated", By: "weave start --run N -- <agent> (no workspace to resume)"},
	{From: "killed", To: "working", By: "weave start --resume"},
	{From: "killed", To: "done", By: "weave salvage + weave pull"},
	{From: "killed", To: "abandoned", By: "weave abandon / weave prune --stale"},
	{From: "killed", To: "allocated", By: "weave start --run N -- <agent> (no workspace to resume)"},
}

// weaveLifecycleNeedsSteward lists the non-closed states whose exit is
// deliberately a STEWARD DECISION rather than an automatic transition — because
// the choice (keep the work? re-run? give up?) is not one a machine may make.
// The reaper's obligation for these is not to move them but to FLAG them, so
// the decision is pending-and-visible instead of pending-and-invisible.
var weaveLifecycleNeedsSteward = map[string]string{
	"submitted": "past the steward threshold with no merge",
	"no-op":     "worker stopped without diff evidence; rerun or abandon",
	"failed":    "committed work on the branch (salvageable)",
	"killed":    "committed work on the branch (salvageable)",
	// paused is a deliberate hold: the steward stopped this run and is the
	// only one who knows whether it should come back. Auto-resuming or
	// auto-abandoning it would override an explicit decision — the one thing
	// worse than not making one.
	"paused": "explicitly held by a steward; resume or abandon",
}

// weaveLifecycleTransitionsFrom returns every legal edge out of a state.
func weaveLifecycleTransitionsFrom(state string) []weaveTransition {
	var out []weaveTransition
	for _, t := range weaveLifecycleTransitions {
		if t.From == state {
			out = append(out, t)
		}
	}
	return out
}

// weaveLifecycleReachesClosure reports whether a closed state is reachable from
// `state` by following legal transitions. A false here IS a limbo: an item can
// enter that state and the machine offers it no way out.
func weaveLifecycleReachesClosure(state string) bool {
	seen := map[string]bool{state: true}
	queue := []string{state}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if weaveIsClosedState(cur) {
			return true
		}
		for _, t := range weaveLifecycleTransitionsFrom(cur) {
			if !seen[t.To] {
				seen[t.To] = true
				queue = append(queue, t.To)
			}
		}
	}
	return false
}

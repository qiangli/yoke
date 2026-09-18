package weave

import (
	"fmt"
	"strings"

	"github.com/qiangli/yoke/pkg/assetring"
)

// ONE AGENT, ONE ISSUE AT A TIME — AND WHAT TO DO WHEN YOU WANT MORE.
//
// `weave start` is one issue in one process; N concurrent workers come from an
// operator running it N times. Nothing used to stop those N runs from all being
// the SAME agent, and the code leaned into it: a per-issue seat letter
// (`codex-a`, `codex-c`) told two concurrent runs apart in the thread, and a
// per-issue store kept their transcripts from colliding.
//
// But a seat letter is not an identity. Those runs still shared one agent name,
// which means one bus cursor, one kb attribution, one capability ledger — and
// downstream, one memory of what "the work" is. Handing one identity two live
// issues is how an agent comes to answer about #412 using what it learned on
// #411: not a crash, a confidently wrong answer, which is worse.
//
// So the seat letter is promoted to what it was always pretending to be. Two
// choices, and no third:
//
//	QUEUE (default)  — the agent takes one issue at a time. A second start for a
//	                   busy agent is refused, and the issue simply stays `todo`
//	                   in the queue for it to pick up next. That IS the queue;
//	                   there is nothing else to build.
//	CLONE (--clone)  — mint a real, named, ephemeral agent for this issue. Own
//	                   name, own store, own cursor, own attribution. Genuinely
//	                   parallel because genuinely separate.

// weaveAgentWorkingOn returns the issue this agent is ALREADY working, if any.
//
// "Working" means claimed, non-terminal, and with a live wrapper. The liveness
// check is what keeps a crashed run from blocking its agent forever: a dead
// wrapper's issue is stale state, not a busy agent, and the existing recovery
// paths already reclaim it.
func weaveAgentWorkingOn(q *weaveQueue, agent string, exceptID int64) *weaveItem {
	agent = strings.TrimSpace(agent)
	if q == nil || agent == "" {
		return nil
	}
	for _, it := range q.Items {
		if it == nil || it.ID == exceptID || it.LaunchSpec == nil {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(it.LaunchSpec.Agent), agent) {
			continue
		}
		if isTerminalState(it.State) || it.State == "todo" {
			continue
		}
		if it.WrapperPid > 0 && pidAlive(it.WrapperPid) {
			return it
		}
	}
	return nil
}

// weaveAgentBusyErr says what is happening and names BOTH ways forward. A
// refusal that only says "no" is how an operator learns to reach for --force.
func weaveAgentBusyErr(agent string, busy, want *weaveItem) error {
	return fmt.Errorf(
		"agent %s is already working run #%d (%s) — an agent is one identity, and two live issues "+
			"under it mix context. Run #%d stays queued for it.\n"+
			"  wait, and it picks #%d up next\n"+
			"  or: weave start --run %d --clone   mint a per-issue clone with its own context",
		agent, busy.ID, strings.TrimSpace(busy.Title), want.ID, want.ID, want.ID)
}

// weaveIssueCloneName is the ephemeral clone's name: the agent, plus the work.
// Reads as a work list in `agents list --all` rather than a pile of elif7s.
func weaveIssueCloneName(agent string, id int64) string {
	return fmt.Sprintf("%s-w%d", strings.TrimSpace(agent), id)
}

// weaveCloneAgentForIssue mints (or reuses) the ephemeral agent that works this
// issue, and returns the name to launch.
//
// FRESH CONTEXT, deliberately. `agents clone` branches the parent's transcript
// because a human cloning an agent wants it to already know things. A weave
// worker is the opposite case: it is scoped to one issue, and weave has always
// given each issue its own store for exactly that reason. What the clone adds is
// not shared history — it is a real NAME, so the worker stops borrowing the
// parent's cursor, kb attribution and ledger.
//
// Reusing an existing record is what makes `--resume` work: the same issue comes
// back to the same worker rather than minting elif-w412 twice.
func weaveCloneAgentForIssue(agent string, id int64) (string, error) {
	name := weaveIssueCloneName(agent, id)
	cat := fleetCatalog()
	if existing, ok := cat.Agent(name); ok {
		if !existing.Ephemeral {
			return "", fmt.Errorf("weave: %q already names a permanent agent; rename it or start this run under another agent", name)
		}
		return name, nil
	}
	clone, err := cat.CloneAgent(agent, name, true, fmt.Sprintf("weave #%d", id))
	if err != nil {
		return "", err
	}
	if err := cat.SaveAgent(clone); err != nil {
		return "", fmt.Errorf("weave: minting worker %q: %w", name, err)
	}
	return name, nil
}

// weaveReapIssueClones removes the ephemeral workers of issues that have
// finished.
//
// Reading IS the reconciliation, as everywhere else here: this runs when a queue
// is next started rather than from a sweeper, so there is no daemon to be down
// and no state that outlives the thing it describes. An ephemeral record left by
// a crash is reclaimed the same way.
func weaveReapIssueClones(q *weaveQueue) {
	if q == nil {
		return
	}
	cat := fleetCatalog()
	for _, it := range q.Items {
		if it == nil || !isTerminalState(it.State) || it.LaunchSpec == nil {
			continue
		}
		name := strings.TrimSpace(it.LaunchSpec.Agent)
		if name == "" {
			continue
		}
		// Only a record WE wrote is ever removed. Reaping an entry an operator
		// hand-wrote, or one an org catalog supplies, would be deleting someone
		// else's data on a timer — so the ring is checked, not just the flag.
		a, ok := cat.Agent(name)
		if !ok || !a.Ephemeral || a.Ring != assetring.RingLocal {
			continue
		}
		_ = cat.RemoveAgent(name)
	}
}

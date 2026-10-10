package weave

import "time"

// SprintRunLink is one sprint card's link to a weave run, as a READER sees it:
// the card's identity and lifecycle beside the link record it carries.
//
// Readers that answer "which sprint is this run part of?" used to join on
// (repo, id). Run ids are queue-local and RECYCLED — prune or reset a queue and
// the next `weave add` reuses the freed numbers — so that join let a sprint
// closed weeks ago claim brand-new runs that merely landed on its old numbers
// (agent-bench#2 and yoke#37/#39 shown under SPRINT #331 after a reset). The
// link record already carries the run's generation (Born, the run's immutable
// Created) and its queue tag; SprintForRun is the one place that uses them, so
// every reader (board, `bashy agents`, …) attributes a run the same way.
type SprintRunLink struct {
	Sprint int64
	// Done marks a shipped sprint. Its links are a historical record: kept and
	// still honoured for the generation they name, but outranked by a live
	// sprint's link to the same generation.
	Done bool
	// UpdatedAt is the card's last mutation. Linking mutates the card, so a
	// card last touched before a run was created cannot have linked it — the
	// one safe inference available for legacy links that carry no Born.
	UpdatedAt time.Time

	Repo  string
	ID    int64
	Queue string    // opaque queue tag; empty on records predating queue identity
	Born  time.Time // linked run's Created; zero on records predating generations
}

// SprintForRun returns the sprint that links THIS generation of run
// (repo, id) living in queue tag `queue` and created at `created`, or 0.
//
// Matching, strongest first:
//   - repo and id must be equal (they are the slot);
//   - queue tags, when both known, must be equal (same checkout);
//   - Born, when both known, must equal created (same generation). A differing
//     Born is a reused id: the link belongs to the retired run, never this one;
//   - a legacy link (no Born) is rejected when its card was last mutated before
//     the run existed — it was made for an earlier generation.
//
// A link proven by generation outranks a legacy link, and a live sprint
// outranks a done one. Within the winning tier, two different sprints are an
// ambiguity and yield 0: an unattributed run is visible as such, while a
// guessed sprint silently mis-files the work.
func SprintForRun(links []SprintRunLink, repo string, id int64, queue string, created time.Time) int64 {
	// tiers: 0 exact+live, 1 exact+done, 2 legacy+live, 3 legacy+done
	var tiers [4][]int64
	for _, l := range links {
		if l.Repo != repo || l.ID != id {
			continue
		}
		if l.Queue != "" && queue != "" && l.Queue != queue {
			continue
		}
		exact := !l.Born.IsZero() && !created.IsZero()
		if exact && !l.Born.Equal(created) {
			continue
		}
		if !exact && !created.IsZero() && !l.UpdatedAt.IsZero() && l.UpdatedAt.Before(created) {
			continue
		}
		t := 0
		if !exact {
			t = 2
		}
		if l.Done {
			t++
		}
		tiers[t] = appendUniqueSprint(tiers[t], l.Sprint)
	}
	for _, ids := range tiers {
		switch len(ids) {
		case 0:
			continue
		case 1:
			return ids[0]
		default:
			return 0
		}
	}
	return 0
}

func appendUniqueSprint(ids []int64, id int64) []int64 {
	for _, x := range ids {
		if x == id {
			return ids
		}
	}
	return append(ids, id)
}

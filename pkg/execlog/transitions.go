// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package execlog

import (
	"sort"
	"time"
)

// Transition is an observed `then` edge: how often one command followed
// another, and how often the second one worked.
//
// It is a BIGRAM over the whole corpus, not a per-occurrence edge. Recording
// one edge per adjacency would produce as many edges as commands — millions —
// and every one of them would be a singleton carrying no information. The
// count is the information.
type Transition struct {
	Src string `json:"src"`
	Dst string `json:"dst"`

	N  int `json:"n"`
	OK int `json:"ok"`

	// Recovered counts the times Src FAILED and Dst then succeeded. This is the
	// remediation signal — the one transition worth more than its frequency,
	// because it is the shape of "when X breaks, Y fixes it".
	Recovered int `json:"recovered,omitempty"`

	First time.Time `json:"first"`
	Last  time.Time `json:"last"`
}

// Transitions derives the `then` edges from the journal.
//
// Two rules bound what may be called a transition, and both exist because the
// alternative manufactures causality:
//
//  1. Only within ONE process. BASHY_EPISODE is inherited, so a `dag -j8` run
//     and every nested bashy share an episode and interleave in one file.
//     Chaining across that merge would report two unrelated shells as one
//     sequence — and it would look completely plausible.
//  2. Only between ADJACENT records of that process. A gap in the sequence
//     means records were lost before flush, and bridging the gap invents an
//     adjacency that was never observed.
//
// Wall-clock order is never used. Two commands being near each other in time
// says nothing about one following the other.
func Transitions(root string, q Query) ([]Transition, Coverage, error) {
	recs, cov, err := Read(root, q)
	if err != nil {
		return nil, cov, err
	}

	type key struct{ src, dst string }
	acc := map[key]*Transition{}

	// Read returns records grouped by (episode, pid) and ascending in seq, so
	// adjacency is a single pass — but the guards below do not RELY on that
	// ordering, because a reader that silently depended on it would break the
	// day someone changed the sort.
	for i := 1; i < len(recs); i++ {
		prev, cur := recs[i-1], recs[i]

		if prev.Episode != cur.Episode || prev.PID != cur.PID {
			continue // rule 1
		}
		if cur.Seq != prev.Seq+1 {
			continue // rule 2 — a gap is lost records, not an adjacency
		}
		if prev.Template == "" || cur.Template == "" {
			continue
		}

		k := key{prev.Template, cur.Template}
		t, ok := acc[k]
		if !ok {
			t = &Transition{Src: k.src, Dst: k.dst, First: prev.At}
			acc[k] = t
		}
		t.N++
		if cur.At.After(t.Last) {
			t.Last = cur.At
		}
		if prev.At.Before(t.First) {
			t.First = prev.At
		}

		curOK := cur.Observed && cur.Exit != nil && *cur.Exit == 0
		if curOK {
			t.OK++
		}
		// A benign non-zero is a negative answer, not a break, so recovering
		// from one is not a remediation.
		prevBroke := prev.Observed && prev.Exit != nil && *prev.Exit != 0 && !prev.Benign
		// A command does not remediate ITSELF. Re-running the same template
		// until it works is a RETRY, and calling it a recovery would render
		// "when X breaks, X fixes it" — advice that is both useless and
		// confidently stated.
		//
		// This fires more than it looks like it should, because the
		// canonicaliser collapses absolute paths outside the repo: `ls /nope`
		// and `ls /etc/hosts` are one template. That collapse is deliberate
		// (an absolute path outside the repo is identity or per-run noise), and
		// its cost is exactly this — which is why the guard is on the SELF
		// comparison rather than on the paths.
		if prevBroke && curOK && k.src != k.dst {
			t.Recovered++
		}
	}

	out := make([]Transition, 0, len(acc))
	for _, t := range acc {
		out = append(out, *t)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].N != out[j].N {
			return out[i].N > out[j].N
		}
		if out[i].Src != out[j].Src {
			return out[i].Src < out[j].Src
		}
		return out[i].Dst < out[j].Dst
	})
	return out, cov, nil
}

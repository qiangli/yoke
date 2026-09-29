// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

// The Leaderboard on the Sprint page — `bashy leaderboard`, projected for a
// browser and nothing more.
//
// The data path is the CLI's, verbatim: ReadLedger → Load (a matrix that will
// not load is NOT fatal) → capability.Compute. Compute is pure, so the app and
// the CLI cannot disagree about who leads the fleet; nothing here re-sorts,
// re-scores, or re-tiers. Ordering is Compute's (Wilson 95% lower bound for
// ranked rows, alphabetical below), and the ranked/observed/prior split is the
// Tier Compute already assigned.
//
// READ-ONLY like every other sprint route: GET only, no mutation, and no query
// parameter that changes scoring — a knob here would let a browser tab publish
// a board the CLI would not have computed. Per-duty ratings, bands and gates
// are deliberately NOT here (story #1223 owns them).
package webconsole

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/qiangli/yoke/pkg/capability"
)

const leaderboardSchemaVersion = "bashy-console-leaderboard-v1"

// readLedgerFn and loadMatrixFn are seams (the collectBoardFn convention): a
// test about how the projection renders must never read — and via Load's
// seed-on-first-use, never write — the real host store under ~/.bashy/capability.
var (
	readLedgerFn = capability.ReadLedger
	loadMatrixFn = capability.Load
)

// leaderboardTier is one of the three tiers, always present: a tier with no
// rows ships an empty list, so a reader can tell "none yet" from "not
// computed".
type leaderboardTier struct {
	ID      string     `json:"id"`
	Title   string     `json:"title"`
	Note    string     `json:"note"`
	Columns []string   `json:"columns"`
	Rows    [][]string `json:"rows"`
}

type leaderboardView struct {
	SchemaVersion string `json:"schema_version"`
	// Provenance travels with every rendering of these numbers (the capability
	// package's hard rule): detached from it, the table reads as a benchmark
	// claim, and it is not one.
	Provenance string `json:"provenance"`
	Records    int    `json:"records"`
	Agents     int    `json:"agents"`
	MinSamples int    `json:"min_samples"`
	PreLedger  int    `json:"pre_ledger_agents"`
	// Unavailable names a ledger read failure. The page stays up and says why
	// it is empty — the same "report, never swallow" rule the board follows.
	Unavailable string            `json:"unavailable,omitempty"`
	Tiers       []leaderboardTier `json:"tiers"`
}

func emptyLeaderboardTiers() []leaderboardTier {
	return []leaderboardTier{
		{ID: "ranked", Title: "Ranked",
			Note:    "by Wilson 95% lower bound on gate pass rate",
			Columns: []string{"agent", "wilson", "rate", "n", "repeat"}, Rows: [][]string{}},
		{ID: "observed", Title: "Observed, not ranked",
			Note:    "too little evidence to order; alphabetical",
			Columns: []string{"agent", "evidence"}, Rows: [][]string{}},
		{ID: "prior", Title: "Prior — not evidence",
			Note:    "seeded estimate; this host has run nothing",
			Columns: []string{"agent", "prior quality"}, Rows: [][]string{}},
	}
}

// handleLeaderboard is GET /api/sprint/leaderboard.
func (s *server) handleLeaderboard(w http.ResponseWriter, r *http.Request) {
	v := leaderboardView{
		SchemaVersion: leaderboardSchemaVersion,
		Provenance:    capability.Provenance,
		MinSamples:    capability.DefaultMinSamples,
		Tiers:         emptyLeaderboardTiers(),
	}
	recs, err := readLedgerFn()
	if err != nil {
		// 200, not 500: an unreadable ledger must not take the page down, and
		// the reason is stated rather than rendered as a silent empty board.
		v.Unavailable = "the run ledger could not be read: " + err.Error()
		writeJSON(w, http.StatusOK, v)
		return
	}
	// A matrix that will not load is not fatal — exactly the CLI's stance: the
	// ledger is the ranking source, the matrix only enumerates the rest of the
	// fleet.
	m, _ := loadMatrixFn()
	board := capability.Compute(recs, capability.ComputeOptions{
		MinSamples: capability.DefaultMinSamples, Matrix: m,
	})

	v.Records, v.Agents = board.Records, len(board.Standings)
	v.MinSamples, v.PreLedger = board.MinSamples, board.PreLedger
	// One pass in Compute's own order; the tier split is the Tier it assigned.
	for _, st := range board.Standings {
		switch st.Tier {
		case capability.TierRanked:
			v.Tiers[0].Rows = append(v.Tiers[0].Rows, []string{
				st.Agent,
				fmt.Sprintf("%.3f", st.WilsonLB),
				fmt.Sprintf("%.0f%%", st.PassRate*100),
				strconv.Itoa(st.GatedRuns),
				capability.RepeatCell(st),
			})
		case capability.TierObserved:
			v.Tiers[1].Rows = append(v.Tiers[1].Rows, []string{st.Agent, capability.ObservedNote(st)})
		default:
			v.Tiers[2].Rows = append(v.Tiers[2].Rows, []string{st.Agent, fmt.Sprintf("%.2f", st.PriorQuality)})
		}
	}
	writeJSON(w, http.StatusOK, v)
}

// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/capability"
)

// seedLeaderboard points the two leaderboard seams at caller-supplied data —
// the same package-var convention collectBoardFn and storyDetailFn use, and
// for the same reason: a test about how the projection renders must never read
// or write the real host ledger and matrix under ~/.bashy/capability.
func seedLeaderboard(t *testing.T, recs []capability.RunRecord, m *capability.Matrix) {
	t.Helper()
	origLedger, origMatrix := readLedgerFn, loadMatrixFn
	t.Cleanup(func() { readLedgerFn, loadMatrixFn = origLedger, origMatrix })
	readLedgerFn = func() ([]capability.RunRecord, error) { return recs, nil }
	loadMatrixFn = func() (*capability.Matrix, error) { return m, nil }
}

// leaderboardRecords is enough ledger to populate every tier: two ranked
// agents whose Wilson bounds differ, and one observed agent below MinSamples.
func leaderboardRecords() []capability.RunRecord {
	pass, fail := true, false
	var recs []capability.RunRecord
	add := func(agent string, outcome *bool, n int) {
		for range n {
			recs = append(recs, capability.RunRecord{
				Agent: agent, Source: capability.SourceWeave, GatePass: outcome,
			})
		}
	}
	add("alpha:one", &pass, 5)
	add("alpha:one", &fail, 1)
	add("beta:two", &pass, 7)
	add("gamma:three", &pass, 1)
	add("gamma:three", &fail, 1)
	return recs
}

func leaderboardMatrix() *capability.Matrix {
	return &capability.Matrix{Agents: map[string]map[capability.Capability]capability.Cell{
		"delta:four": {capability.CapCoding: {Quality: 0.7, Source: capability.SourcePrior}},
	}}
}

func tierByID(t *testing.T, d map[string]any, id string) map[string]any {
	t.Helper()
	tiers, ok := d["tiers"].([]any)
	if !ok {
		t.Fatalf("payload carries no tiers list: %v", keysOf(d))
	}
	for _, raw := range tiers {
		tier := raw.(map[string]any)
		if tier["id"] == id {
			return tier
		}
	}
	t.Fatalf("no %q tier in %v", id, tiers)
	return nil
}

// The API is a PROJECTION of capability.Compute, never a second computation:
// the rows must come back in Compute's order (ranked by Wilson 95% lower
// bound) carrying Compute's values. Re-sorting or re-scoring here is how the
// app and the CLI would come to disagree about who leads the fleet.
func TestLeaderboardAPIMatchesTheComputedBoard(t *testing.T) {
	h, _ := newBoardTestServer(t)
	recs, m := leaderboardRecords(), leaderboardMatrix()
	seedLeaderboard(t, recs, m)

	want := capability.Compute(recs, capability.ComputeOptions{
		MinSamples: capability.DefaultMinSamples, Matrix: m,
	})
	var ranked []capability.Standing
	for _, s := range want.Standings {
		if s.Tier == capability.TierRanked {
			ranked = append(ranked, s)
		}
	}
	if len(ranked) != 2 {
		t.Fatalf("fixture computes %d ranked agents, want 2", len(ranked))
	}

	d := getJSON(t, h, "/api/sprint/leaderboard")
	if d["schema_version"] != "bashy-console-leaderboard-v1" {
		t.Errorf("schema_version = %v", d["schema_version"])
	}
	if int(d["records"].(float64)) != want.Records ||
		int(d["agents"].(float64)) != len(want.Standings) ||
		int(d["min_samples"].(float64)) != want.MinSamples {
		t.Errorf("summary = records:%v agents:%v min_samples:%v, want %d/%d/%d",
			d["records"], d["agents"], d["min_samples"],
			want.Records, len(want.Standings), want.MinSamples)
	}

	rows := tierByID(t, d, "ranked")["rows"].([]any)
	if len(rows) != len(ranked) {
		t.Fatalf("ranked tier has %d rows, want %d", len(rows), len(ranked))
	}
	for i, s := range ranked {
		row := rows[i].([]any)
		if row[0] != s.Agent {
			t.Errorf("row %d agent = %v, want %s — the API must keep Compute's order", i, row[0], s.Agent)
		}
		if row[1] != fmt.Sprintf("%.3f", s.WilsonLB) {
			t.Errorf("row %d wilson = %v, want %.3f", i, row[1], s.WilsonLB)
		}
		if row[3] != strconv.Itoa(s.GatedRuns) {
			t.Errorf("row %d n = %v, want %d", i, row[3], s.GatedRuns)
		}
		// No events-mode record exists, so the repeat cell is the em dash:
		// "unmeasured", never "0.0" pretending to be "no repetition".
		if row[4] != "—" {
			t.Errorf("row %d repeat = %v, want the em dash for an absent measurement", i, row[4])
		}
	}

	// The observed row carries the CLI's note, matrix samples included, so an
	// unranked agent is never left as a bare name.
	obs := tierByID(t, d, "observed")["rows"].([]any)
	if len(obs) != 1 {
		t.Fatalf("observed tier has %d rows, want 1", len(obs))
	}
	if row := obs[0].([]any); row[0] != "gamma:three" || row[1] != capability.ObservedNote(want.Standings[2]) {
		t.Errorf("observed row = %v, want gamma:three with the CLI's note", row)
	}
}

// The projection is read-only in the same sense its sprint siblings are: no
// leaderboard handler answers a mutating method. The console's start page is a
// catch-all on "/", so an unmatched method falls through to the launcher
// rather than drawing a 405 — console-wide behaviour, pinned as such by
// TestBoardServesNoMutatingMethod — which makes "no leaderboard payload came
// back" the meaningful assertion here, exactly as it is there.
func TestLeaderboardAPIIsReadOnly(t *testing.T) {
	h, _ := newBoardTestServer(t)
	seedLeaderboard(t, leaderboardRecords(), leaderboardMatrix())

	for _, m := range []string{"POST", "PUT", "DELETE", "PATCH"} {
		body := do(h, m, "/api/sprint/leaderboard", "127.0.0.1:5555", nil).Body.String()
		if strings.Contains(body, "bashy-console-leaderboard-v1") {
			t.Errorf("%s /api/sprint/leaderboard was answered by the leaderboard handler; GET only", m)
		}
	}
	// And the read path really is registered, so the negative proves something.
	if !strings.Contains(do(h, "GET", "/api/sprint/leaderboard", "127.0.0.1:5555", nil).Body.String(),
		"bashy-console-leaderboard-v1") {
		t.Fatal("GET /api/sprint/leaderboard returned no leaderboard payload")
	}
}

// A tier with no rows is PRESENT with an empty rows list. Dropping it would
// make "none yet" indistinguishable from "not computed", and the reader of a
// leaderboard needs that difference — an agent missing from a tier is the same
// ambiguity as an agent missing from the board.
func TestLeaderboardAPIKeepsEmptyTiersVisible(t *testing.T) {
	h, _ := newBoardTestServer(t)
	// One observed agent, no matrix: the ranked and prior tiers have nothing.
	pass := true
	seedLeaderboard(t, []capability.RunRecord{
		{Agent: "gamma:three", Source: capability.SourceWeave, GatePass: &pass},
	}, nil)

	d := getJSON(t, h, "/api/sprint/leaderboard")
	tiers := d["tiers"].([]any)
	if len(tiers) != 3 {
		t.Fatalf("payload carries %d tiers, want ranked+observed+prior always", len(tiers))
	}
	for i, id := range []string{"ranked", "observed", "prior"} {
		tier := tiers[i].(map[string]any)
		if tier["id"] != id {
			t.Errorf("tier %d = %v, want %s", i, tier["id"], id)
		}
		rows, ok := tier["rows"].([]any)
		if !ok {
			t.Fatalf("tier %s rows is %T, want a list even when empty", id, tier["rows"])
		}
		if id == "observed" {
			if len(rows) != 1 {
				t.Errorf("observed tier has %d rows, want 1", len(rows))
			}
		} else if len(rows) != 0 {
			t.Errorf("%s tier has %d rows, want an empty list", id, len(rows))
		}
	}
}

// A ledger that cannot be read must not take the page down: the section
// degrades to a named reason, the same convention the board overview follows
// for a failed collect (an error is reported, never swallowed).
func TestLeaderboardAPIStaysUpWhenTheLedgerFails(t *testing.T) {
	h, _ := newBoardTestServer(t)
	origLedger, origMatrix := readLedgerFn, loadMatrixFn
	t.Cleanup(func() { readLedgerFn, loadMatrixFn = origLedger, origMatrix })
	readLedgerFn = func() ([]capability.RunRecord, error) { return nil, errors.New("torn jsonl") }
	loadMatrixFn = func() (*capability.Matrix, error) { return nil, nil }

	d := getJSON(t, h, "/api/sprint/leaderboard")
	if !strings.Contains(d["unavailable"].(string), "torn jsonl") {
		t.Errorf("unavailable = %v, want it to name the ledger failure", d["unavailable"])
	}
	if got := len(d["tiers"].([]any)); got != 3 {
		t.Errorf("degraded payload carries %d tiers, want the 3 empty ones", got)
	}
}

package capability

// THE LEADERBOARD — the fleet's own run evidence, ranked honestly.
//
// This is not a benchmark. It is what happened on ONE host, on ITS repos, with
// ITS gates, and the rendering says so in every emitter. The value is not
// comparability with anyone else's numbers; it is that these are the only
// numbers that were produced by the work actually being done here, under a
// gate that actually ran.
//
// # Wilson, because n is the whole story
//
// Ranking on raw pass rate makes 1/1 = 100% the best agent in the fleet, which
// is how a leaderboard becomes a random-number generator with an authoritative
// font. Standings are ordered by the WILSON 95% LOWER BOUND on gate pass rate:
// a small sample is pulled toward zero in proportion to how little is known,
// so an agent overtakes another by being *demonstrably* better rather than by
// being lucky once. 1/1 scores 0.21; 9/10 scores 0.60; 90/100 scores 0.83.
//
// # Three tiers, and the bottom two are not rankings
//
//	RANKED              >= MinSamples gated runs. Ordered by Wilson lower bound.
//	OBSERVED NOT RANKED evidence exists but is too thin to order. n is shown;
//	                    no position is implied, and the caller is not invited
//	                    to infer one from the print order.
//	PRIOR NOT EVIDENCE  a seeded research prior and nothing else. Printed so
//	                    the fleet is fully enumerated, labelled so it can never
//	                    be quoted as a measurement.
//
// The third tier is the one that makes the artifact honest. A leaderboard that
// silently omits agents with no evidence reads as "these are the agents", and
// the reader cannot tell an agent that failed from an agent that never ran.
//
// # What is deliberately NOT computed
//
// No composite score. The absorption plan's rule, inherited from
// steward-conductor-benchmark-spec.md, is that a pass is a CONJUNCTION and
// never an average: an agent that games one dimension must not be able to buy
// back a hard failure elsewhere with a good number. So the metrics are reported
// side by side and the ordering key is one of them, named.
//
// Loop discipline pools ONLY events-mode records. A pty scrape counts novel
// output, not tool calls; averaging the two produces a number that means
// nothing and looks like it means something.

import (
	"math"
	"slices"
	"sort"
	"strings"
)

// LeaderboardSchema is the --json envelope version.
const LeaderboardSchema = "bashy-leaderboard-v1"

// DefaultMinSamples is how many gated runs an agent needs before it is ranked
// rather than merely observed. Five is not a statistical threshold — no small
// number is — it is a floor low enough to be reachable on one host and high
// enough that a single fluke cannot mint a leader.
const DefaultMinSamples = 5

// Tier is how much an agent's evidence supports.
type Tier string

const (
	TierRanked   Tier = "ranked"
	TierObserved Tier = "observed"
	TierPrior    Tier = "prior"
)

// Standing is one agent's row.
type Standing struct {
	Agent string `json:"agent"` // tool:model — never a band
	Tier  Tier   `json:"tier"`

	GatedRuns int     `json:"gated_runs"`
	Passes    int     `json:"passes"`
	PassRate  float64 `json:"pass_rate"`          // point estimate; NOT the ordering key
	WilsonLB  float64 `json:"wilson_lb"`          // the ordering key for ranked rows
	Reviewed  int     `json:"reviewed,omitempty"` // runs carrying a review verdict
	Survived  int     `json:"survived,omitempty"` // review verdicts that approved

	// RepeatRatio is the MEDIAN over events-mode records only. Zero means no
	// events-mode record existed — never "no repetition".
	RepeatRatio    float64 `json:"repeat_ratio,omitempty"`
	RepeatSamples  int     `json:"repeat_samples,omitempty"`
	Steers         int     `json:"steers,omitempty"`
	RecoveredAfter int     `json:"recovered_after_steer,omitempty"`
	SteeredRuns    int     `json:"steered_runs,omitempty"`

	MedianWallMS int64 `json:"median_wall_ms,omitempty"`
	// CostIndex is RELATIVE: the cheapest ranked agent with cost evidence is
	// 1.00. Absolute spend stays host-local — it is nobody else's business and
	// it dates instantly.
	CostIndex float64 `json:"cost_index,omitempty"`

	// PriorQuality is the matrix's seeded estimate, carried only for prior-tier
	// rows so the fleet can be enumerated without implying measurement.
	PriorQuality float64 `json:"prior_quality,omitempty"`
	// MatrixSamples is what the EMA matrix has folded in for CapCoding. It is
	// reported for observed rows because it is the ONLY surviving trace of runs
	// that predate the ledger — and it is explicitly not a pass count, because
	// an EMA cannot be un-averaged.
	MatrixSamples int `json:"matrix_samples,omitempty"`
}

// Board is a computed leaderboard.
type Board struct {
	SchemaVersion string     `json:"schema_version"`
	MinSamples    int        `json:"min_samples"`
	Records       int        `json:"records"`
	Standings     []Standing `json:"standings"`
	// PreLedger reports agents whose only evidence is the EMA matrix, i.e. runs
	// that happened before the ledger existed. Stated because their absence
	// from the ranked tier is a RECORDING gap, not a performance one, and a
	// reader who does not know that will draw the wrong conclusion.
	PreLedger int `json:"pre_ledger_agents,omitempty"`
}

// ComputeOptions configures a board.
type ComputeOptions struct {
	MinSamples int
	Role       string // filter to one seat; empty = all
	// Matrix supplies the prior/observed tiers so the fleet is fully
	// enumerated. Nil computes from ledger records alone.
	Matrix *Matrix
}

// Compute builds the board. Pure: no I/O, no clock, so the emitters and the
// tests see the same function.
func Compute(records []RunRecord, opts ComputeOptions) Board {
	minS := opts.MinSamples
	if minS <= 0 {
		minS = DefaultMinSamples
	}

	type acc struct {
		gated, passes       int
		reviewed, survived  int
		repeats             []float64
		steers, steeredRuns int
		recovered           int
		walls               []int64
		costs               []int64
	}
	byAgent := map[string]*acc{}
	kept := 0
	for _, r := range records {
		if opts.Role != "" && !strings.EqualFold(r.Role, opts.Role) {
			continue
		}
		kept++
		a := byAgent[r.Agent]
		if a == nil {
			a = &acc{}
			byAgent[r.Agent] = a
		}
		// A nil GatePass is the absence of evidence and contributes to
		// NEITHER numerator nor denominator. This is the line the whole
		// evidence invariant lives on.
		if r.GatePass != nil {
			a.gated++
			if *r.GatePass {
				a.passes++
			}
		}
		if v := reviewOutcome(r.Review); v != nil {
			a.reviewed++
			if *v {
				a.survived++
			}
		}
		if r.CoachMode == "events" && r.RepeatRatio > 0 {
			a.repeats = append(a.repeats, r.RepeatRatio)
		}
		if r.Steers > 0 {
			a.steers += r.Steers
			a.steeredRuns++
			if r.Recovered != nil && *r.Recovered {
				a.recovered++
			}
		}
		if r.WallMS > 0 {
			a.walls = append(a.walls, r.WallMS)
		}
		if r.CostMicro > 0 {
			a.costs = append(a.costs, r.CostMicro)
		}
	}

	board := Board{SchemaVersion: LeaderboardSchema, MinSamples: minS, Records: kept}
	seen := map[string]bool{}
	for agent, a := range byAgent {
		seen[agent] = true
		s := Standing{
			Agent: agent, GatedRuns: a.gated, Passes: a.passes,
			Reviewed: a.reviewed, Survived: a.survived,
			Steers: a.steers, SteeredRuns: a.steeredRuns, RecoveredAfter: a.recovered,
			RepeatSamples: len(a.repeats),
			RepeatRatio:   medianF(a.repeats),
			MedianWallMS:  medianI(a.walls),
		}
		if a.gated > 0 {
			s.PassRate = float64(a.passes) / float64(a.gated)
			s.WilsonLB = wilsonLower(a.passes, a.gated)
		}
		if a.gated >= minS {
			s.Tier = TierRanked
		} else {
			s.Tier = TierObserved
		}
		s.CostIndex = float64(medianI(a.costs)) // normalised below
		board.Standings = append(board.Standings, s)
	}

	// Enumerate the rest of the fleet from the matrix, so a reader can tell an
	// agent that performed badly from one that never ran.
	if opts.Matrix != nil {
		for agent, caps := range opts.Matrix.Agents {
			if seen[agent] {
				// Carry the pre-ledger sample count onto the existing row.
				if c, ok := caps[CapCoding]; ok && c.Source == SourceHost {
					setMatrixSamples(board.Standings, agent, c.Samples)
				}
				continue
			}
			c, ok := caps[CapCoding]
			s := Standing{Agent: agent, Tier: TierPrior}
			if ok {
				s.PriorQuality = c.Quality
				if c.Source == SourceHost {
					// Host evidence with no ledger record: these runs happened
					// before the ledger existed. Observed, never ranked — the
					// EMA cannot yield a pass count.
					s.Tier = TierObserved
					s.MatrixSamples = c.Samples
					board.PreLedger++
				}
			}
			board.Standings = append(board.Standings, s)
		}
	}

	normaliseCost(board.Standings)
	sortStandings(board.Standings)
	return board
}

// sortStandings orders ranked rows by Wilson lower bound, then everything else
// alphabetically. Print order inside the non-ranked tiers carries NO meaning
// and is alphabetical precisely so it cannot be read as one.
func sortStandings(ss []Standing) {
	rank := map[Tier]int{TierRanked: 0, TierObserved: 1, TierPrior: 2}
	sort.SliceStable(ss, func(i, j int) bool {
		if rank[ss[i].Tier] != rank[ss[j].Tier] {
			return rank[ss[i].Tier] < rank[ss[j].Tier]
		}
		if ss[i].Tier == TierRanked && ss[i].WilsonLB != ss[j].WilsonLB {
			return ss[i].WilsonLB > ss[j].WilsonLB
		}
		return ss[i].Agent < ss[j].Agent
	})
}

func setMatrixSamples(ss []Standing, agent string, n int) {
	for i := range ss {
		if ss[i].Agent == agent {
			ss[i].MatrixSamples = n
			return
		}
	}
}

// normaliseCost turns absolute median cost into a relative index against the
// cheapest agent that has cost evidence. Absolute spend never leaves the host.
func normaliseCost(ss []Standing) {
	min := math.MaxFloat64
	for _, s := range ss {
		if s.CostIndex > 0 && s.CostIndex < min {
			min = s.CostIndex
		}
	}
	if min == math.MaxFloat64 {
		for i := range ss {
			ss[i].CostIndex = 0
		}
		return
	}
	for i := range ss {
		if ss[i].CostIndex > 0 {
			ss[i].CostIndex = ss[i].CostIndex / min
		}
	}
}

// reviewOutcome maps a verdict string to approve/reject, or nil for anything
// that is not a terminal judgement. An unrecognised verdict is NOT a rejection:
// a harness that wrote something we do not parse has told us nothing.
func reviewOutcome(v string) *bool {
	t, f := true, false
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "approve", "approved", "pass", "lgtm", "accept", "accepted":
		return &t
	case "reject", "rejected", "fail", "failed", "block", "blocked":
		return &f
	default:
		return nil
	}
}

// wilsonLower is the Wilson score interval's lower bound at 95%.
//
// Why not a plain proportion: 1/1 is 100% and means nothing. Wilson pulls a
// small sample toward zero by exactly how little is known, so 1/1 = 0.21,
// 9/10 = 0.60, and 90/100 = 0.83. An agent climbs by demonstrating, not by
// getting lucky early — which is the property that makes the ordering usable
// as a routing input rather than as trivia.
func wilsonLower(passes, trials int) float64 {
	if trials <= 0 {
		return 0
	}
	const z = 1.96
	n := float64(trials)
	p := float64(passes) / n
	den := 1 + z*z/n
	centre := p + z*z/(2*n)
	margin := z * math.Sqrt(p*(1-p)/n+z*z/(4*n*n))
	lb := (centre - margin) / den
	if lb < 0 {
		return 0
	}
	return lb
}

func medianF(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	return s[len(s)/2]
}

func medianI(xs []int64) int64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]int64(nil), xs...)
	slices.Sort(s)
	return s[len(s)/2]
}

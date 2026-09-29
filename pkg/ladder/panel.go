package ladder

import (
	"encoding/binary"
	"errors"
	"hash/fnv"
	"sort"
	"strings"
)

// PlantedRate is the fraction of the judge queue that carries known answers.
const PlantedRate = 0.10

// PanelMember is a judge eligible to sit on a panel.
type PanelMember struct {
	Agent  string
	Vendor string
	Judge  DutyStanding
	Manage DutyStanding
}

// PanelCase is a verdict request routed to a judge panel.
type PanelCase struct {
	ID            string
	Kind          string // "low-stakes" | "design" | "merge" | "dispute" | "escalated"
	Points        Points
	AuthorVendor  string
	LowConfidence bool
	// JudgedManage is the standing of the manager under review for manager
	// verdicts; the zero value means the case is not a manager verdict.
	JudgedManage DutyStanding
}

// Vote is one panelist's verdict with its confidence and rubric scores.
type Vote struct {
	Agent      string
	Verdict    string
	Confidence float64 // 0..1
	Rubric     map[string]float64
}

// VerdictResult is the panel's verdict. ToOwner means the panel produced no
// actionable verdict and the owner decides; then Verdict is empty.
type VerdictResult struct {
	Verdict string
	Tally   map[string]int
	ToOwner bool
	Reason  string
}

// PanelSize returns the panel size for c. The result is always odd.
func PanelSize(c PanelCase) int {
	switch c.Kind {
	case "dispute", "escalated":
		return 5
	case "design":
		return 3
	case "merge":
		if c.Points >= 5 {
			return 3
		}
	}
	if c.LowConfidence {
		return 3
	}
	return 1
}

// PanelDraw selects a panel of PanelSize(c) members from pool.
//
// Eligible members are sorted by judge conservative rating (Judge.Lower(),
// descending) with ties broken deterministically by FNV(seed, agent). For
// manager verdicts (c.JudgedManage non-zero) members that fail dominance —
// Manage.Lower() < c.JudgedManage.R — are skipped. A single judge never
// shares the author's vendor. Panels of 3+ span at least 2 vendors,
// excluding the author's vendor when enough members remain, otherwise
// including it while still spanning at least 2 vendors.
//
// It returns an error when too few eligible members remain; the caller then
// escalates to the owner. It never shrinks below the size and never returns
// an even panel.
func PanelDraw(c PanelCase, pool []PanelMember, seed int64) ([]PanelMember, error) {
	size := PanelSize(c)
	eligible := panelEligible(c, pool)
	if len(eligible) < size {
		return nil, errors.New("panel: not enough eligible members")
	}
	panelSort(seed, eligible)
	if size == 1 {
		for _, m := range eligible {
			if c.AuthorVendor == "" || m.Vendor != c.AuthorVendor {
				return []PanelMember{m}, nil
			}
		}
		return nil, errors.New("panel: no eligible judge outside author vendor")
	}
	candidates := eligible
	if c.AuthorVendor != "" {
		var without []PanelMember
		for _, m := range eligible {
			if m.Vendor != c.AuthorVendor {
				without = append(without, m)
			}
		}
		if len(without) >= size && panelVendorCount(without) >= 2 {
			candidates = without
		}
	}
	if len(candidates) < size {
		return nil, errors.New("panel: not enough eligible members")
	}
	picked := append([]PanelMember(nil), candidates[:size]...)
	if panelVendorCount(picked) < 2 {
		fixed := false
		for _, m := range candidates[size:] {
			if m.Vendor != picked[0].Vendor {
				picked[size-1] = m
				fixed = true
				break
			}
		}
		if !fixed {
			return nil, errors.New("panel: cannot span 2 vendors")
		}
	}
	panelSort(seed, picked)
	return picked, nil
}

// PanelVerdict reduces votes to a verdict by strict majority (>50%).
// ToOwner (with empty Verdict) is set when there is no majority
// ("no majority", covering any 3-way split), when a majority with dissent
// has mean majority confidence below 0.5 ("low-confidence split"), and for a
// single vote below 0.5 confidence ("escalate to 3").
func PanelVerdict(votes []Vote) VerdictResult {
	r := VerdictResult{Tally: map[string]int{}}
	if len(votes) == 0 {
		r.ToOwner = true
		r.Reason = "no votes"
		return r
	}
	for _, v := range votes {
		r.Tally[v.Verdict]++
	}
	top, topCount := "", 0
	for verdict, n := range r.Tally {
		if n > topCount {
			top, topCount = verdict, n
		}
	}
	if len(votes) == 1 {
		v := votes[0]
		if v.Confidence < 0.5 {
			return VerdictResult{Tally: r.Tally, ToOwner: true, Reason: "escalate to 3"}
		}
		return VerdictResult{Verdict: v.Verdict, Tally: r.Tally}
	}
	if topCount*2 <= len(votes) {
		r.ToOwner = true
		r.Reason = "no majority"
		return r
	}
	if topCount < len(votes) && panelMeanConfidence(votes, top) < 0.5 {
		return VerdictResult{Tally: r.Tally, ToOwner: true, Reason: "low-confidence split"}
	}
	r.Verdict = top
	return r
}

// IsPlanted reports whether caseID is a planted calibration case under seed.
// It is deterministic and true for about PlantedRate of inputs.
func IsPlanted(caseID string, seed int64) bool {
	h := fnv.New64a()
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], uint64(seed))
	h.Write(b[:])
	h.Write([]byte(caseID))
	return h.Sum64()%100 < uint64(PlantedRate*100)
}

// panelStripIdentity replaces each name in text with "author" so judging
// stays blind to identity. Longer names are replaced first.
func panelStripIdentity(text string, names []string) string {
	ordered := append([]string(nil), names...)
	sort.Slice(ordered, func(i, j int) bool { return len(ordered[i]) > len(ordered[j]) })
	for _, n := range ordered {
		if n == "" {
			continue
		}
		text = strings.ReplaceAll(text, n, "author")
	}
	return text
}

// panelEligible drops members that fail dominance for manager verdicts.
func panelEligible(c PanelCase, pool []PanelMember) []PanelMember {
	if c.JudgedManage == (DutyStanding{}) {
		return append([]PanelMember(nil), pool...)
	}
	eligible := make([]PanelMember, 0, len(pool))
	for _, m := range pool {
		if DominanceOK(m.Manage, c.JudgedManage) {
			eligible = append(eligible, m)
		}
	}
	return eligible
}

// panelSort orders members by judge conservative rating, descending,
// breaking ties deterministically with FNV(seed, agent).
func panelSort(seed int64, members []PanelMember) {
	sort.SliceStable(members, func(i, j int) bool {
		li, lj := members[i].Judge.Lower(), members[j].Judge.Lower()
		if li != lj {
			return li > lj
		}
		hi, hj := panelTieBreak(seed, members[i].Agent), panelTieBreak(seed, members[j].Agent)
		if hi != hj {
			return hi < hj
		}
		return members[i].Agent < members[j].Agent
	})
}

// panelTieBreak hashes seed with agent for deterministic tie-breaking.
func panelTieBreak(seed int64, agent string) uint64 {
	h := fnv.New64a()
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], uint64(seed))
	h.Write(b[:])
	h.Write([]byte(agent))
	return h.Sum64()
}

// panelVendorCount counts distinct vendors in members.
func panelVendorCount(members []PanelMember) int {
	seen := map[string]bool{}
	for _, m := range members {
		seen[m.Vendor] = true
	}
	return len(seen)
}

// panelMeanConfidence is the mean confidence of votes for verdict.
func panelMeanConfidence(votes []Vote, verdict string) float64 {
	var sum float64
	var n int
	for _, v := range votes {
		if v.Verdict == verdict {
			sum += v.Confidence
			n++
		}
	}
	if n == 0 {
		return 0
	}
	return sum / float64(n)
}

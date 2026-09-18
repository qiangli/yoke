package resources

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// The utilization verdict encodes the fleet invariant as a CHECKABLE SIGNAL:
//
//	It is not OK for the fleet to sit idle while issues are pending. Idle is
//	acceptable only when the board reads 0 open work.
//
// Everything below joins one capacity reading (per-band idle agents, from the
// fleet collector) with one work reading (pending items, from the board) and
// returns exactly one of three determinate verdicts. There is no "unknown"
// state on purpose: a steward or an autopilot must always be able to act on
// the answer.
const (
	// VerdictOptimal — nothing is pending, or every agent is already busy.
	VerdictOptimal = "OPTIMAL"
	// VerdictUnderUtilized — pending work AND band-appropriate idle capacity.
	// This is the one actionable failure of the invariant: dispatch now.
	VerdictUnderUtilized = "UNDER-UTILIZED"
	// VerdictSaturated — pending work but no free band-appropriate capacity.
	// The honest "waiting on compute" case. NOT a failure.
	VerdictSaturated = "SATURATED"
)

// UtilizationSchemaVersion is the envelope version for `resources utilization`.
const UtilizationSchemaVersion = "bashy-utilization-v1"

// PendingItem is one unit of open work the fleet could be dispatched against.
// Band is the minimum agent band the item needs; 0 means any band will do.
type PendingItem struct {
	Kind  string `json:"kind"` // todo | salvageable | submitted
	ID    string `json:"id"`
	Label string `json:"label,omitempty"`
	Band  int    `json:"band,omitempty"`
}

// PendingWork is the board's open-work reading.
type PendingWork struct {
	Todo        int           `json:"todo"`
	Salvageable int           `json:"salvageable"`
	Submitted   int           `json:"submitted"`
	Items       []PendingItem `json:"items,omitempty"`
}

// Total is the pending count the invariant is checked against.
func (p PendingWork) Total() int { return p.Todo + p.Salvageable + p.Submitted }

// IdleAgent is one catalog agent counted Idle by the fleet collector — found,
// available, not cooling, and not attributed to any live run.
type IdleAgent struct {
	Name     string `json:"name"`
	Tool     string `json:"tool,omitempty"`
	Model    string `json:"model,omitempty"`
	Provider string `json:"provider,omitempty"`
	Band     int    `json:"band,omitempty"`
}

// Assignment names one pending item and the idle agent that could take it.
// UNDER-UTILIZED is never reported without at least one of these: the verdict
// must say WHICH work and WHICH agent, or it is not actionable.
type Assignment struct {
	Item  PendingItem `json:"item"`
	Agent IdleAgent   `json:"agent"`
}

// Utilization is the `bashy-utilization-v1` envelope.
type Utilization struct {
	SchemaVersion string       `json:"schema_version"`
	GeneratedAt   time.Time    `json:"generated_at"`
	Verdict       string       `json:"verdict"`
	Reason        string       `json:"reason"`
	Pending       PendingWork  `json:"pending"`
	Capacity      Capacity     `json:"capacity"`
	IdleAgents    []IdleAgent  `json:"idle_agents,omitempty"`
	Assignments   []Assignment `json:"assignments,omitempty"`
}

// Capacity is the fleet side of the join.
type Capacity struct {
	Total       int         `json:"total"`
	Busy        int         `json:"busy"`
	Idle        int         `json:"idle"`
	Cooling     int         `json:"cooling"`
	Unavailable int         `json:"unavailable"`
	IdleByBand  map[int]int `json:"idle_by_band,omitempty"`
}

// UnderUtilized reports whether the invariant is currently violated.
func (u *Utilization) UnderUtilized() bool {
	return u != nil && u.Verdict == VerdictUnderUtilized
}

// EvaluateUtilization joins pending work with fleet capacity and returns the
// determinate verdict. fr may be nil (treated as zero capacity).
func EvaluateUtilization(at time.Time, pending PendingWork, fr *FleetResources) *Utilization {
	if at.IsZero() {
		at = time.Now().UTC()
	}
	u := &Utilization{
		SchemaVersion: UtilizationSchemaVersion,
		GeneratedAt:   at,
		Pending:       pending,
	}
	if fr != nil {
		u.Capacity = Capacity{
			Total:       fr.Totals.Total,
			Busy:        fr.Totals.Busy,
			Idle:        fr.Totals.Idle,
			Cooling:     fr.Totals.Cooling,
			Unavailable: fr.Totals.Unavailable,
			IdleByBand:  map[int]int{},
		}
		u.IdleAgents = append(u.IdleAgents, fr.IdleAgents...)
		for _, a := range fr.IdleAgents {
			u.Capacity.IdleByBand[a.Band]++
		}
	}

	if pending.Total() == 0 {
		u.Verdict = VerdictOptimal
		u.Reason = "no pending work; idle capacity is acceptable"
		return u
	}

	u.Assignments = matchAssignments(pending.Items, u.IdleAgents)

	switch {
	case len(u.Assignments) > 0:
		u.Verdict = VerdictUnderUtilized
		u.Reason = fmt.Sprintf("%d issue(s) pending with %d band-appropriate idle agent(s): %s",
			pending.Total(), len(u.Assignments), describeAssignments(u.Assignments))
	case u.Capacity.Idle == 0:
		u.Verdict = VerdictSaturated
		u.Reason = fmt.Sprintf("%d issue(s) pending; all %d agent(s) busy or unavailable — waiting on compute",
			pending.Total(), u.Capacity.Total)
	default:
		u.Verdict = VerdictSaturated
		u.Reason = fmt.Sprintf("%d issue(s) pending; %d idle agent(s) but none band-appropriate — waiting on compute",
			pending.Total(), u.Capacity.Idle)
	}
	return u
}

// matchAssignments greedily pairs each pending item with the lowest idle agent
// band that still satisfies it. Items are considered highest-band-first so a
// scarce high band is not consumed by work that any agent could have taken.
// An item with Band 0 (unknown requirement) matches any idle agent.
func matchAssignments(items []PendingItem, idle []IdleAgent) []Assignment {
	if len(items) == 0 || len(idle) == 0 {
		return nil
	}
	sortedItems := append([]PendingItem(nil), items...)
	sort.SliceStable(sortedItems, func(i, j int) bool {
		if sortedItems[i].Band != sortedItems[j].Band {
			return sortedItems[i].Band > sortedItems[j].Band
		}
		return sortedItems[i].ID < sortedItems[j].ID
	})
	pool := append([]IdleAgent(nil), idle...)
	sort.SliceStable(pool, func(i, j int) bool {
		if pool[i].Band != pool[j].Band {
			return pool[i].Band < pool[j].Band
		}
		return pool[i].Name < pool[j].Name
	})
	used := make([]bool, len(pool))
	var out []Assignment
	for _, item := range sortedItems {
		for i, agent := range pool {
			if used[i] || agent.Band < item.Band {
				continue
			}
			used[i] = true
			out = append(out, Assignment{Item: item, Agent: agent})
			break
		}
	}
	return out
}

func describeAssignments(as []Assignment) string {
	parts := make([]string, 0, len(as))
	for _, a := range as {
		parts = append(parts, fmt.Sprintf("%s %s -> %s (L%d)", a.Item.Kind, a.Item.ID, a.Agent.Name, a.Agent.Band))
	}
	return strings.Join(parts, ", ")
}

// Banner is the single-line rendering used by the board banner and by the
// watch loop.
func (u *Utilization) Banner() string {
	if u == nil {
		return "UTILIZATION: unavailable"
	}
	return fmt.Sprintf("UTILIZATION %s — %d pending (todo %d, salvageable %d, submitted %d); capacity %d busy / %d idle / %d total. %s",
		u.Verdict, u.Pending.Total(), u.Pending.Todo, u.Pending.Salvageable, u.Pending.Submitted,
		u.Capacity.Busy, u.Capacity.Idle, u.Capacity.Total, u.Reason)
}

// UtilizationWatcher tracks verdict transitions so a notification fires ONCE
// on entry into UNDER-UTILIZED rather than on every tick. The zero value is
// ready to use and treats the first observation as a transition if it is
// already under-utilized (a steward starting the watch on a violated fleet
// must still be told).
type UtilizationWatcher struct {
	last   string
	Notify func(*Utilization)
}

// Observe records a reading and reports whether it is a transition INTO
// UNDER-UTILIZED. When it is, Notify (if set) is invoked exactly once.
func (w *UtilizationWatcher) Observe(u *Utilization) bool {
	if u == nil {
		return false
	}
	transitioned := u.Verdict == VerdictUnderUtilized && w.last != VerdictUnderUtilized
	w.last = u.Verdict
	if transitioned && w.Notify != nil {
		w.Notify(u)
	}
	return transitioned
}

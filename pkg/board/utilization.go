package board

import (
	"context"

	"github.com/qiangli/yoke/pkg/resources"
)

// The board owns the work half of the fleet invariant: it is the only place
// that already knows what is open. Pending work is exactly the three buckets
// a steward can act on right now —
//
//	todo         open (not done) todo items
//	salvageable  terminal runs still holding unmerged commits
//	submitted    runs awaiting a merge decision
//
// — and the utilization banner joins that count with fleet capacity so an
// idle fleet next to a non-empty board is a visible, checkable violation
// rather than something a human has to notice.

// PendingItems projects the board's open work into the resources engine's
// vocabulary. Band carries the run's band so matching stays band-appropriate;
// todos have no band requirement (0 = any agent).
func (b *Board) PendingItems() resources.PendingWork {
	var work resources.PendingWork
	for _, t := range b.Todos {
		if t.Status == "done" {
			continue
		}
		work.Todo++
		work.Items = append(work.Items, resources.PendingItem{Kind: "todo", ID: t.ID, Label: t.Title})
	}
	for _, r := range b.Runs {
		switch {
		case r.Salvageable:
			work.Salvageable++
			work.Items = append(work.Items, resources.PendingItem{Kind: "salvageable", ID: itoa(r.ID), Label: r.Label, Band: r.Band})
		case r.State == "submitted":
			work.Submitted++
			work.Items = append(work.Items, resources.PendingItem{Kind: "submitted", ID: itoa(r.ID), Label: r.Label, Band: r.Band})
		}
	}
	return work
}

// PendingWork is the resources.PendingProvider a host wires into
// `bashy resources utilization`. It collects a machine-global board and
// reports its open work.
func PendingWork(ctx context.Context) (resources.PendingWork, error) {
	b, err := Collect(ctx, Options{All: true}, DefaultSources(), NewRegistry())
	if err != nil {
		return resources.PendingWork{}, err
	}
	return b.PendingItems(), nil
}

// evaluateUtilization fills b.Utilization from the board's own work reading
// and the fleet capacity derived from the board's agents and runs. It is
// called during finalize so every renderer (terminal, JSON, HTML) sees it.
func (b *Board) evaluateUtilization() {
	// No agents source means no capacity reading, and a verdict without one
	// would be a guess. Leave it nil rather than fall through to the
	// collector's LIVE branch, which spawns weave subprocesses — finalize
	// must stay a pure projection of what the sources already loaded.
	if len(b.Agents) == 0 {
		return
	}
	fr, err := resources.CollectFleetResourcesFromBoard(context.Background(), b.GeneratedAt, boardAgents(b), boardRuns(b))
	if err != nil {
		fr = nil
	}
	b.Utilization = resources.EvaluateUtilization(b.GeneratedAt, b.PendingItems(), fr)
}

func boardAgents(b *Board) []resources.BoardAgent {
	var out []resources.BoardAgent
	for _, a := range b.Agents {
		out = append(out, resources.BoardAgent{
			Name: a.Name, Tool: a.Tool, Model: a.Model, Band: a.Band,
			Available: a.Available, Found: a.Found, Availability: a.Availability, State: a.State,
		})
	}
	return out
}

func boardRuns(b *Board) []resources.BoardRun {
	var out []resources.BoardRun
	for _, r := range b.Runs {
		out = append(out, resources.BoardRun{State: r.State, Tool: r.Tool, Agent: r.Agent, Model: r.Model})
	}
	return out
}

func utilizationPanel() Panel {
	return panel{id: "utilization", build: func(b *Board) PanelView {
		v := PanelView{ID: "utilization", Title: "Utilization health",
			Columns: []string{"PENDING", "ID", "LABEL", "IDLE AGENT", "BAND"}}
		u := b.Utilization
		if u == nil {
			v.Collapsed = "unavailable"
			return v
		}
		v.Collapsed = u.Banner()
		for _, a := range u.Assignments {
			v.Rows = append(v.Rows, []string{a.Item.Kind, a.Item.ID, a.Item.Label, a.Agent.Name, band(a.Agent.Band)})
		}
		return v
	}}
}

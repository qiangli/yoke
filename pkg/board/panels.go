package board

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/policy/coord"
	"github.com/qiangli/yoke/pkg/resources"
)

type panel struct {
	id    string
	build func(*Board) PanelView
}

func (p panel) ID() string               { return p.id }
func (p panel) Build(b *Board) PanelView { return p.build(b) }

func DefaultPanels() *Registry {
	return NewRegistry(agentPanel(), todoPanel(), sprintPanel(), claimsPanel(), runPanel(), workspacePanel(), salvagePanel(), dagPanel(), fleetPanel(), resourcePanel(), utilizationPanel())
}

// claimsPanel exposes current named holds to human readers of the existing
// Sprint board. It is a projection only; the browser remains read-only.
func claimsPanel() Panel {
	return panel{id: "claims", build: func(b *Board) PanelView {
		v := PanelView{ID: "claims", Title: "Claims",
			Columns: []string{"RESOURCE", "HOLDER", "STATE", "INTENT", "HELD FOR", "MODE"}}
		for _, c := range b.Claims {
			if c.Resource == "" {
				continue
			}
			host := c.Holder.Host
			if host == "" {
				host = "this host"
			}
			state := string(c.Liveness(b.GeneratedAt)) + " on " + host
			age := int64(0)
			if !c.AcquiredAt.IsZero() {
				age = int64(b.GeneratedAt.Sub(c.AcquiredAt).Seconds())
				if age < 0 {
					age = 0
				}
			}
			mode := c.Mode
			if mode == "" {
				mode = coord.ModeLease
			}
			v.Rows = append(v.Rows, []string{c.Resource, dash(c.Holder.Name), state,
				dash(c.Intent), duration(age), mode})
		}
		v.Collapsed = fmt.Sprintf("%d named hold(s) recorded on this host", len(v.Rows))
		return v
	}}
}

// dagPanel projects recent `bashy dag` pipeline runs. Failures are what a
// steward is scanning for, so the collapsed summary leads with them.
func dagPanel() Panel {
	return panel{id: "dag", build: func(b *Board) PanelView {
		failed := 0
		for _, x := range b.DagRuns {
			if x.Failed {
				failed++
			}
		}
		v := PanelView{ID: "dag", Title: "Dag runs",
			Collapsed: fmt.Sprintf("%d recent run(s); %d failed", len(b.DagRuns), failed),
			Columns:   []string{"RESULT", "TARGETS", "AGE", "DURATION", "FILE", "RUN"}}
		for _, x := range b.DagRuns {
			result := "ok"
			if x.Failed {
				result = fmt.Sprintf("FAILED %d/%d", x.FailedN, x.Total)
			}
			age := int64(0)
			if !x.StartedAt.IsZero() {
				age = int64(time.Since(x.StartedAt).Seconds())
			}
			v.Rows = append(v.Rows, []string{
				result, dash(x.Targets), duration(age), millis(x.DurationMS),
				dash(x.File), x.RunID,
			})
		}
		return v
	}}
}

func agentPanel() Panel {
	return panel{id: "agents", build: func(b *Board) PanelView {
		bands := make([]int, 0, len(b.Summary.AgentLoadByBand))
		for level := range b.Summary.AgentLoadByBand {
			bands = append(bands, level)
		}
		sort.Ints(bands)
		load := make([]string, 0, len(bands))
		for _, level := range bands {
			load = append(load, fmt.Sprintf("L%d %s %d", level, strings.Repeat("█", b.Summary.AgentLoadByBand[level]), b.Summary.AgentLoadByBand[level]))
		}
		v := PanelView{ID: "agents", Title: "Agents",
			Collapsed: fmt.Sprintf("%d in flight; %s", b.Summary.InFlight, join(load, ", ")),
			Columns:   []string{"AGENT", "TOOL", "BAND", "MODEL", "RELIABILITY", "AVAILABILITY", "STATE"}}
		for _, a := range b.Agents {
			v.Rows = append(v.Rows, []string{a.Name, a.Tool, band(a.Band), dash(a.Model), dash(a.Reliability), dash(a.Availability), a.State})
		}
		return v
	}}
}

func todoPanel() Panel {
	return panel{id: "todo", build: func(b *Board) PanelView {
		counts := map[string]int{}
		for _, x := range b.Todos {
			counts[x.Status]++
		}
		v := PanelView{ID: "todo", Title: "Todo",
			Collapsed: fmt.Sprintf("%d total; %d open; %d blocked", len(b.Todos), len(b.Todos)-counts["done"], counts["blocked"]),
			Columns:   []string{"#", "STATUS", "PRIO", "AGE", "SCOPE", "TITLE"}}
		for _, x := range b.Todos {
			v.Rows = append(v.Rows, []string{fmt.Sprint(x.Number), x.Status, dash(x.Priority), ageSince(b.GeneratedAt, x.Created), x.Scope, x.Title})
		}
		return v
	}}
}

func sprintPanel() Panel {
	return panel{id: "sprints", build: func(b *Board) PanelView {
		counts := map[string]int{}
		for _, x := range b.Sprints {
			counts[x.Column]++
		}
		v := PanelView{ID: "sprints", Title: "Sprints",
			Collapsed: fmt.Sprintf("%d total; %d doing; %d awaiting converge", len(b.Sprints), counts["doing"], counts["review"]),
			Columns:   []string{"ID", "COLUMN", "GATE", "EPIC", "CONDUCTOR", "RUNS", "TITLE"}}
		for _, x := range b.Sprints {
			v.Rows = append(v.Rows, []string{"#" + itoa(x.ID), x.Column, dash(x.GateState), dash(x.Epic), dash(x.Conductor), fmt.Sprint(len(x.RunRefs)), x.Title})
		}
		return v
	}}
}

func runPanel() Panel {
	return panel{id: "runs", build: func(b *Board) PanelView {
		counts := map[string]int{}
		for _, x := range b.Runs {
			counts[x.State]++
		}
		v := PanelView{ID: "runs", Title: "Runs",
			Collapsed: fmt.Sprintf("%d total; %d working; %d ready to merge", len(b.Runs), counts["working"], counts["submitted"]),
			Columns:   []string{"ID", "STATE", "AGE", "UNMERGED", "TOOL", "BAND", "MODEL", "REPO", "TITLE"}}
		for _, x := range b.Runs {
			unmerged := "-"
			if x.Salvageable {
				unmerged = fmt.Sprintf("%d commits", x.UnmergedCommits)
			}
			v.Rows = append(v.Rows, []string{"#" + itoa(x.ID), x.State, duration(x.AgeSeconds), unmerged, dash(x.Tool), band(x.Band), dash(x.Model), x.Repo, x.Label})
		}
		return v
	}}
}

// workspacePanel keeps a weave run and the disk it occupies adjacent in the
// board hierarchy. This is deliberately separate from Host resources: the
// latter answers "is the filesystem full?", while this answers "which
// isolated clones are using it?".
func workspacePanel() Panel {
	return panel{id: "workspaces", build: func(b *Board) PanelView {
		type row struct {
			run Run
		}
		var rows []row
		var total uint64
		unavailable := 0
		for _, run := range b.Runs {
			if run.Workspace == "" {
				continue
			}
			rows = append(rows, row{run: run})
			if run.WorkspaceDiskError != "" {
				unavailable++
			} else if run.WorkspaceDiskBytes <= ^uint64(0)-total {
				total += run.WorkspaceDiskBytes
			}
		}
		sort.SliceStable(rows, func(i, j int) bool {
			return rows[i].run.WorkspaceDiskBytes > rows[j].run.WorkspaceDiskBytes
		})

		v := PanelView{ID: "workspaces", Title: "Workspaces",
			Collapsed: fmt.Sprintf("%d workspace(s); %s on disk", len(rows), resources.HumanBytes(total)),
			Columns:   []string{"RUN", "SPRINT", "TODO", "STATE", "DISK", "CPU", "RSS", "REPO", "WORKSPACE"}}
		if unavailable > 0 {
			v.Collapsed += fmt.Sprintf("; %d unavailable", unavailable)
		}
		for _, x := range rows {
			disk := resources.HumanBytes(x.run.WorkspaceDiskBytes)
			if x.run.WorkspaceDiskError != "" {
				disk = "unavailable: " + x.run.WorkspaceDiskError
			}
			v.Rows = append(v.Rows, []string{"#" + itoa(x.run.ID), sprintLabel(x.run.SprintID), dash(x.run.TodoID), x.run.State, disk, percentPtr(x.run.CPUPercent), bytesPtr(x.run.RSSBytes), x.run.Repo, x.run.Workspace})
		}
		return v
	}}
}

func sprintLabel(id int64) string {
	if id == 0 {
		return "-"
	}
	return "#" + itoa(id)
}

func percentPtr(v *float64) string {
	if v == nil {
		return "-"
	}
	return fmt.Sprintf("%.1f%%", *v)
}

func bytesPtr(v *uint64) string {
	if v == nil {
		return "-"
	}
	return resources.HumanBytes(*v)
}

func salvagePanel() Panel {
	return panel{id: "salvage", build: func(b *Board) PanelView {
		v := PanelView{ID: "salvage", Title: "Salvageable runs",
			Columns: []string{"ID", "STATE", "UNMERGED", "TOOL", "REPO", "TITLE"}}
		for _, x := range b.Runs {
			if !x.Salvageable {
				continue
			}
			v.Rows = append(v.Rows, []string{"#" + itoa(x.ID), x.State, fmt.Sprintf("%d commits", x.UnmergedCommits), dash(x.Tool), x.Repo, x.Label})
		}
		v.Collapsed = fmt.Sprintf("%d terminal run(s) hold unmerged commits", len(v.Rows))
		return v
	}}
}

// millis renders a millisecond duration, keeping sub-second values visible
// where the whole-second `duration` helper would collapse them to "-".
func millis(ms int64) string {
	if ms <= 0 {
		return "0s"
	}
	return (time.Duration(ms) * time.Millisecond).Round(time.Millisecond).String()
}

func band(n int) string {
	if n <= 0 {
		return "L?"
	}
	return fmt.Sprintf("L%d", n)
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func join(xs []string, sep string) string {
	if len(xs) == 0 {
		return "none"
	}
	return strings.Join(xs, sep)
}

func duration(seconds int64) string {
	if seconds <= 0 {
		return "-"
	}
	return (time.Duration(seconds) * time.Second).Round(time.Second).String()
}

func ageSince(now, created time.Time) string {
	if created.IsZero() {
		return "-"
	}
	d := now.Sub(created)
	if d < 0 {
		d = 0
	}
	if d >= 24*time.Hour {
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	}
	if d >= time.Hour {
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	return fmt.Sprintf("%dm", int(d/time.Minute))
}

func fleetPanel() Panel {
	return panel{id: "fleet", build: func(b *Board) PanelView {
		var bAgents []resources.BoardAgent
		for _, a := range b.Agents {
			bAgents = append(bAgents, resources.BoardAgent{
				Name:         a.Name,
				Tool:         a.Tool,
				Model:        a.Model,
				Band:         a.Band,
				Available:    a.Available,
				Found:        a.Found,
				Availability: a.Availability,
				State:        a.State,
			})
		}
		var bRuns []resources.BoardRun
		for _, r := range b.Runs {
			bRuns = append(bRuns, resources.BoardRun{
				State: r.State,
				Tool:  r.Tool,
				Agent: r.Agent,
				Model: r.Model,
			})
		}
		fr, err := resources.CollectFleetResourcesFromBoard(context.Background(), b.GeneratedAt, bAgents, bRuns)
		if err != nil || fr == nil {
			return PanelView{ID: "fleet", Title: "Fleet resources", Collapsed: "unavailable"}
		}
		v := PanelView{
			ID:        "fleet",
			Title:     "Fleet resources",
			Collapsed: fmt.Sprintf("%d total; %d busy; %d idle; %d cooling; %d unavailable", fr.Totals.Total, fr.Totals.Busy, fr.Totals.Idle, fr.Totals.Cooling, fr.Totals.Unavailable),
			Columns:   []string{"PROVIDER", "BAND", "TOTAL", "BUSY", "IDLE", "COOLING", "UNAVAIL", "SUB", "API", "TOKENS", "COST"},
		}
		for _, g := range fr.Groups {
			tokStr, costStr := "N/A", "N/A"
			if g.MeterPresent && g.Tokens != nil && g.CostUSD != nil {
				tokStr = fmt.Sprint(*g.Tokens)
				costStr = fmt.Sprintf("$%.4f", *g.CostUSD)
			}
			v.Rows = append(v.Rows, []string{
				g.Provider, g.Band, fmt.Sprint(g.Total), fmt.Sprint(g.Busy),
				fmt.Sprint(g.Idle), fmt.Sprint(g.Cooling), fmt.Sprint(g.Unavailable),
				fmt.Sprint(g.Subscription), fmt.Sprint(g.APIKey), tokStr, costStr,
			})
		}
		return v
	}}
}

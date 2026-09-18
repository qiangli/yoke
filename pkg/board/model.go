// Package board implements bashy's read-only steward/conductor projection.
// Sources collect the three work layers; renderers and panels are registries so
// the P2 conductor view and future statistics do not change the core model.
package board

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/resources"
)

const SchemaVersion = "bashy-board-v1"

type Options struct {
	All    bool
	Expand map[string]bool
	Now    time.Time
}

type Board struct {
	SchemaVersion string      `json:"schema_version"`
	Role          string      `json:"role"`
	Scope         string      `json:"scope"`
	Title         string      `json:"title"`
	GeneratedAt   time.Time   `json:"generated_at"`
	Rows          []Row       `json:"rows"`
	Rollup        Rollup      `json:"rollup"`
	Summary       Summary     `json:"summary"`
	Lanes         []Lane      `json:"lanes"`
	Panels        []PanelView `json:"panels"`
	Agents        []Agent     `json:"agents"`
	Todos         []Todo      `json:"todos"`
	Sprints       []Sprint    `json:"sprints"`
	Runs          []Run       `json:"runs"`
	// DagRuns are `bashy dag` pipeline runs from the local run journal. They
	// are a separate layer from Runs (which are agentic weave runs): a dag run
	// is a task graph on this host, not an agent working an issue.
	DagRuns []DagRun `json:"dag_runs,omitempty"`
	// Resources is the host reading (nil when the collector is not wired
	// into the source set, e.g. a test board).
	Resources *resources.System `json:"resources,omitempty"`
	// Utilization is the fleet-invariant verdict: idle capacity is only
	// acceptable when the board reads 0 open work.
	Utilization *resources.Utilization `json:"utilization,omitempty"`
	Warnings    []string               `json:"warnings,omitempty"`
}

// Row is the normalized record shared by every source. The richer typed
// slices remain available to panels; Rows is the stable cross-source wire view.
type Row struct {
	Source      string `json:"source"`
	Repo        string `json:"repo,omitempty"`
	ID          string `json:"id"`
	State       string `json:"state"`
	Tool        string `json:"tool,omitempty"`
	Band        int    `json:"band,omitempty"`
	Model       string `json:"model,omitempty"`
	Label       string `json:"label"`
	Points      int    `json:"points,omitempty"`
	ElapsedSecs int64  `json:"elapsed_secs,omitempty"`
	DurSecs     int64  `json:"dur_secs,omitempty"`
	SprintID    int64  `json:"sprint_id,omitempty"`
	Salvageable bool   `json:"salvageable,omitempty"`
	Unmerged    int    `json:"unmerged_commits,omitempty"`
	AgeSeconds  int64  `json:"age_seconds,omitempty"`
	Stale       bool   `json:"stale,omitempty"`
}

type Rollup struct {
	ByState       map[string]int `json:"by_state"`
	ByAgentBand   map[int]int    `json:"by_agent_band"`
	Merged        int            `json:"merged"`
	EtaMedianSecs int64          `json:"eta_median_secs,omitempty"`
}

type Summary struct {
	Todos            int         `json:"todos"`
	Sprints          int         `json:"sprints"`
	Runs             int         `json:"runs"`
	NeedsSteward     int         `json:"needs_steward"`
	Unattended       int         `json:"unattended"`
	InFlight         int         `json:"in_flight"`
	ETAMedianSeconds int64       `json:"eta_median_seconds,omitempty"`
	AgentLoadByBand  map[int]int `json:"agent_load_by_band"`
}

type Lane struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Cards   []Card `json:"cards"`
	Dropped int    `json:"dropped,omitempty"`
}

type Card struct {
	Layer       string `json:"layer"`
	ID          string `json:"id"`
	Label       string `json:"label"`
	State       string `json:"state"`
	Tool        string `json:"tool,omitempty"`
	Band        int    `json:"band,omitempty"`
	Model       string `json:"model,omitempty"`
	Elapsed     int64  `json:"elapsed_seconds,omitempty"`
	ETA         int64  `json:"eta_seconds,omitempty"`
	Scope       string `json:"scope,omitempty"`
	Salvageable bool   `json:"salvageable,omitempty"`
	Unmerged    int    `json:"unmerged_commits,omitempty"`
	AgeSeconds  int64  `json:"age_seconds,omitempty"`
	Stale       bool   `json:"stale,omitempty"`
}

type Agent struct {
	Name         string `json:"name"`
	Tool         string `json:"tool"`
	Model        string `json:"model,omitempty"`
	Band         int    `json:"band,omitempty"`
	Reliability  string `json:"reliability,omitempty"`
	Available    bool   `json:"available"`
	Found        bool   `json:"found"`
	Availability string `json:"availability"`
	State        string `json:"state"`
}

type Todo struct {
	ID       string     `json:"id"`
	Number   int        `json:"number,omitempty"`
	Title    string     `json:"title"`
	Status   string     `json:"status"`
	Priority string     `json:"priority,omitempty"`
	Scope    string     `json:"scope"`
	Due      *time.Time `json:"due,omitempty"`
	Overdue  bool       `json:"overdue,omitempty"`
	Created  time.Time  `json:"created,omitempty,omitzero"`

	// Assignee is who is working this story, empty when nobody is. It is the
	// only signal on the OVERVIEW that separates an assigned story from an
	// untouched one — a weave run does not name the story it executes, so the
	// worker cannot be derived from the run side.
	Assignee string `json:"assignee,omitempty"`

	// SprintID is the sprint card this todo is a story of, 0 when unlinked.
	// It is what lets a reader group stories under their card; without it the
	// board could only ever show a flat list, which is what it did.
	SprintID int64 `json:"sprint_id,omitempty"`

	// Store is the scope arguments that located this item (`--base-dir <repo>`
	// or `--user --owner <name>`), kept so a reader can ask todo for the item's
	// FULL record — the body especially — without re-deriving which of the
	// host's several registers it came from.
	//
	// NOT serialized. It is a lookup key inside this process, not part of the
	// board's public shape, and shipping a caller's argv in a payload would
	// invite someone to treat it as one.
	Store []string `json:"-"`
}

// Story is one item's FULL record, as `todo show --json` reports it. The board
// carries the projection; this is what a reader asks for when it wants the
// body behind a row.
//
// Deliberately fetched ONE AT A TIME rather than folded into the overview: the
// bodies on this host run to seventy lines each, and the overview is polled.
// The same reasoning already governs the panel rows — "the full set is one
// deliberate request away".
type Story struct {
	ID       string     `json:"id"`
	Kind     string     `json:"kind,omitempty"`
	Title    string     `json:"title"`
	Seq      int        `json:"seq,omitempty"`
	Status   string     `json:"status"`
	Priority string     `json:"priority,omitempty"`
	Created  time.Time  `json:"created,omitzero"`
	Assignee string     `json:"assignee,omitempty"`
	Sprint   int64      `json:"sprint,omitempty"`
	Due      *time.Time `json:"due,omitempty"`
	Closed   *time.Time `json:"closed,omitempty"`
	Body     string     `json:"body,omitempty"`
	Scope    string     `json:"scope,omitempty"`
	// Outbound is the body's resolved citations (todo show --links). The
	// board keeps it so a story that cites a kb runbook can surface it; the
	// inbound backlinks are not carried — nothing on the page reads them.
	Outbound []LinkRef `json:"outbound,omitempty"`
}

// LinkRef mirrors one entry of todo show --links: the canonical ref, the
// target's title and (for a kb page) its type, and whether it resolved.
type LinkRef struct {
	Ref    string `json:"ref"`
	Seq    int    `json:"seq,omitempty"` // a kb target's ring-local seq, when resolved and minted
	Title  string `json:"title,omitempty"`
	Type   string `json:"type,omitempty"`
	Status string `json:"status,omitempty"`
}

type Sprint struct {
	ID         int64  `json:"id"`
	Title      string `json:"title"`
	Epic       string `json:"epic,omitempty"`
	Column     string `json:"column"`
	Continuity string `json:"continuity,omitempty"`
	Conductor  string `json:"conductor,omitempty"`
	// Manager is the durable project-manager identity. Conductor is the live
	// lease holder; during a pause the manager remains the right 1:1 chat even
	// though no process currently holds the lease.
	Manager string `json:"manager,omitempty"`
	// SpecRef is the sprint's plan: the spec/handoff document `sprint add
	// --spec` recorded and `sprint show` prints as its "spec:" line. It is a
	// REPO-RELATIVE path, and a sprint spans repos — see StoryRoots — so it is
	// carried as the reference it is and never resolved to a URL here.
	SpecRef string `json:"spec_ref,omitempty"`

	// MeetRoomRef is meet's durable room identity, never its reusable short
	// number. The browser uses it to open this sprint's session history.
	MeetRoomRef   string   `json:"meet_room_ref,omitempty"`
	LeaseStale    bool     `json:"lease_stale,omitempty"`
	GateState     string   `json:"gate_state,omitempty"`
	LeaseHolder   string   `json:"lease_holder,omitempty"`
	ContinuityRef string   `json:"continuity_ref,omitempty"`
	RunRefs       []RunRef `json:"run_refs,omitempty"`
	// StoryRoots are the repo todo stores this sprint's stories live in.
	//
	// A sprint is USER-GLOBAL and spans repos by definition; its stories are
	// per-repo records. Without this the board could only find them in stores it
	// stumbled across — the reader's own working directory, and the repos of
	// weave runs — so the same sprint reported 23 stories from one directory and
	// 0 from another. A card that says where its stories are is the only source
	// that does not depend on where the reader is standing.
	StoryRoots []string `json:"story_roots,omitempty"`
	// Story counts are derived from Todos after every source has loaded. They
	// travel with the sprint so terminal, JSON, and browser projections cannot
	// disagree about progress or make closed work disappear from the total.
	StoryTotal  int `json:"story_total"`
	StoryOpen   int `json:"story_open"`
	StoryClosed int `json:"story_closed"`
}

type RunRef struct {
	Repo string `json:"repo"`
	ID   int64  `json:"id"`
}

type Run struct {
	ID              int64     `json:"id"`
	Label           string    `json:"label"`
	Repo            string    `json:"repo"`
	State           string    `json:"state"`
	Tool            string    `json:"tool,omitempty"`
	Agent           string    `json:"agent,omitempty"`
	Model           string    `json:"model,omitempty"`
	Band            int       `json:"band,omitempty"`
	StartedAt       time.Time `json:"started_at,omitempty,omitzero"`
	MaxRuntime      int64     `json:"max_runtime_seconds,omitempty"`
	FinishedAt      time.Time `json:"finished_at,omitempty,omitzero"`
	Points          int       `json:"points,omitempty"`
	SprintID        int64     `json:"sprint_id,omitempty"`
	TodoID          string    `json:"todo_id,omitempty"`
	Blocked         bool      `json:"blocked,omitempty"`
	Salvageable     bool      `json:"salvageable,omitempty"`
	UnmergedCommits int       `json:"unmerged_commits,omitempty"`
	AgeSeconds      int64     `json:"age_seconds,omitempty"`
	Stale           bool      `json:"stale,omitempty"`
	// Workspace is the filesystem-isolated clone backing this weave run.
	// Resource fields come from the shared bounded observation snapshot; an
	// unavailable measurement keeps the workspace visible with its reason.
	Workspace          string   `json:"workspace,omitempty"`
	WorkspaceDiskBytes uint64   `json:"workspace_disk_bytes,omitempty"`
	WorkspaceDiskError string   `json:"workspace_disk_error,omitempty"`
	CPUPercent         *float64 `json:"cpu_percent,omitempty"`
	RSSBytes           *uint64  `json:"rss_bytes,omitempty"`
}

// DagRun is one `bashy dag` run as recorded by that package's run journal.
type DagRun struct {
	RunID     string    `json:"run_id"`
	File      string    `json:"file"`
	Targets   string    `json:"targets,omitempty"`
	StartedAt time.Time `json:"started_at,omitempty,omitzero"`
	// Milliseconds, not seconds: a dag target is routinely sub-second, and the
	// board's whole-second duration helper renders those as "-", which hides
	// exactly the number someone opened the panel to see.
	DurationMS int64 `json:"duration_ms,omitempty"`
	Failed     bool  `json:"failed,omitempty"`
	Total      int   `json:"total"`
	OK         int   `json:"ok"`
	FailedN    int   `json:"failed_count,omitempty"`
}

type Source interface {
	Name() string
	Load(context.Context, *Board, Options) error
}

type SourceFunc struct {
	SourceName string
	Func       func(context.Context, *Board, Options) error
}

func (s SourceFunc) Name() string                                        { return s.SourceName }
func (s SourceFunc) Load(ctx context.Context, b *Board, o Options) error { return s.Func(ctx, b, o) }

type Panel interface {
	ID() string
	Build(*Board) PanelView
}

type PanelView struct {
	ID        string     `json:"id"`
	Title     string     `json:"title"`
	Collapsed string     `json:"collapsed"`
	Columns   []string   `json:"columns,omitempty"`
	Rows      [][]string `json:"rows,omitempty"`
}

type Registry struct{ panels []Panel }

func NewRegistry(panels ...Panel) *Registry {
	return &Registry{panels: append([]Panel(nil), panels...)}
}
func (r *Registry) Register(p Panel) { r.panels = append(r.panels, p) }
func (r *Registry) Build(b *Board) []PanelView {
	out := make([]PanelView, 0, len(r.panels))
	for _, p := range r.panels {
		out = append(out, p.Build(b))
	}
	return out
}

type Renderer interface {
	Render(*Board, Options) ([]byte, error)
}

func Collect(ctx context.Context, opts Options, sources []Source, panels *Registry) (*Board, error) {
	if opts.Now.IsZero() {
		opts.Now = time.Now()
	}
	b := &Board{SchemaVersion: SchemaVersion, Role: "steward", Scope: "machine-global", Title: "Bashy Steward Board", GeneratedAt: opts.Now.UTC()}
	for _, s := range sources {
		if err := s.Load(ctx, b, opts); err != nil {
			b.Warnings = append(b.Warnings, s.Name()+": "+err.Error())
		}
	}
	b.finalize(opts.Now)
	if panels == nil {
		panels = DefaultPanels()
	}
	b.Panels = panels.Build(b)
	return b, nil
}

func (b *Board) finalize(now time.Time) {
	storyCounts := make(map[int64][2]int)
	for _, t := range b.Todos {
		if t.SprintID == 0 {
			continue
		}
		counts := storyCounts[t.SprintID]
		if closedStoryStatus(t.Status) {
			counts[1]++
		} else {
			counts[0]++
		}
		storyCounts[t.SprintID] = counts
	}
	for i := range b.Sprints {
		counts := storyCounts[b.Sprints[i].ID]
		b.Sprints[i].StoryOpen = counts[0]
		b.Sprints[i].StoryClosed = counts[1]
		b.Sprints[i].StoryTotal = counts[0] + counts[1]
	}

	linked := map[string]int64{}
	for _, sprint := range b.Sprints {
		for _, ref := range sprint.RunRefs {
			linked[ref.Repo+"\x00"+itoa(ref.ID)] = sprint.ID
		}
	}
	loads := map[int]int{}
	completed := map[string][]int64{}
	var allCompleted []int64
	for _, r := range b.Runs {
		if !r.StartedAt.IsZero() && !r.FinishedAt.IsZero() && r.FinishedAt.After(r.StartedAt) {
			d := int64(r.FinishedAt.Sub(r.StartedAt).Seconds())
			completed[r.Tool] = append(completed[r.Tool], d)
			allCompleted = append(allCompleted, d)
		}
	}
	lanes := map[string][]Card{"backlog": {}, "working": {}, "review": {}, "needs-steward": {}, "done": {}}
	for i := range b.Runs {
		r := &b.Runs[i]
		if r.SprintID == 0 {
			r.SprintID = linked[r.Repo+"\x00"+itoa(r.ID)]
		}
		elapsed, eta := int64(0), int64(0)
		if !r.StartedAt.IsZero() {
			elapsed = int64(now.Sub(r.StartedAt).Seconds())
			if elapsed < 0 {
				elapsed = 0
			}
			if estimate, ok := durationMedian(completed[r.Tool]); ok && estimate > elapsed {
				eta = estimate - elapsed
			}
		}
		c := Card{Layer: "run", ID: itoa(r.ID), Label: r.Label, State: r.State, Tool: r.Tool, Band: r.Band, Model: r.Model, Elapsed: elapsed, ETA: eta, Scope: r.Repo, Salvageable: r.Salvageable, Unmerged: r.UnmergedCommits, AgeSeconds: r.AgeSeconds, Stale: r.Stale}
		dur := int64(0)
		if !r.StartedAt.IsZero() && !r.FinishedAt.IsZero() && r.FinishedAt.After(r.StartedAt) {
			dur = int64(r.FinishedAt.Sub(r.StartedAt).Seconds())
		}
		b.Rows = append(b.Rows, Row{Source: "run", Repo: r.Repo, ID: itoa(r.ID), State: r.State, Tool: r.Tool, Band: r.Band, Model: r.Model, Label: r.Label, Points: r.Points, ElapsedSecs: elapsed, DurSecs: dur, SprintID: r.SprintID, Salvageable: r.Salvageable, Unmerged: r.UnmergedCommits, AgeSeconds: r.AgeSeconds, Stale: r.Stale})
		lane := runLane(r.State)
		if r.State == "submitted" || r.Salvageable || r.Stale {
			lane = "needs-steward"
		}
		lanes[lane] = append(lanes[lane], c)
		if r.Stale {
			b.Summary.Unattended++
		}
		if r.State == "working" {
			loads[r.Band]++
			b.Summary.InFlight++
		}
	}
	for _, t := range b.Todos {
		// SprintID travels with the row. Runs and sprints already carried it and
		// todo did not, so the union view could not correlate a story to the
		// sprint it is a story OF — the one correlation the board exists to make.
		b.Rows = append(b.Rows, Row{Source: "todo", Repo: t.Scope, ID: t.ID, State: t.Status, Label: t.Title, SprintID: t.SprintID})
		if t.Status == "blocked" {
			lanes["needs-steward"] = append(lanes["needs-steward"], Card{Layer: "todo", ID: t.ID, Label: t.Title, State: t.Status, Scope: t.Scope})
		}
	}
	for _, s := range b.Sprints {
		b.Rows = append(b.Rows, Row{Source: "sprint", ID: itoa(s.ID), State: s.Column, Label: s.Title, SprintID: s.ID})
		if s.Column == "review" {
			lanes["needs-steward"] = append(lanes["needs-steward"], Card{Layer: "sprint", ID: itoa(s.ID), Label: s.Title, State: s.Column})
		}
	}
	if estimate, ok := durationMedian(allCompleted); ok {
		b.Summary.ETAMedianSeconds = estimate
	}
	b.Summary.Todos = len(b.Todos)
	b.Summary.Sprints = len(b.Sprints)
	b.Summary.Runs = len(b.Runs)
	b.Summary.NeedsSteward = len(lanes["needs-steward"])
	b.Summary.AgentLoadByBand = loads
	b.Rollup = Rollup{ByState: map[string]int{}, ByAgentBand: loads, EtaMedianSecs: b.Summary.ETAMedianSeconds}
	for _, row := range b.Rows {
		b.Rollup.ByState[row.State]++
		if row.Source == "run" && row.State == "done" {
			b.Rollup.Merged++
		}
	}
	b.evaluateUtilization()
	for _, id := range []string{"needs-steward", "working", "review", "backlog", "done"} {
		if len(lanes[id]) > 0 || id != "done" {
			b.Lanes = append(b.Lanes, Lane{ID: id, Title: laneTitle(id), Cards: lanes[id]})
		}
	}
}

func closedStoryStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "done", "closed", "cancelled", "canceled":
		return true
	default:
		return false
	}
}

func durationMedian(samples []int64) (int64, bool) {
	// Fewer than three observations is anecdote, not an ETA.
	if len(samples) < 3 {
		return 0, false
	}
	values := append([]int64(nil), samples...)
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	mid := len(values) / 2
	if len(values)%2 == 0 {
		return (values[mid-1] + values[mid]) / 2, true
	}
	return values[mid], true
}

func runLane(s string) string {
	switch s {
	case "working", "allocated", "paused":
		return "working"
	case "submitted":
		return "review"
	case "done", "abandoned", "killed", "failed":
		return "done"
	default:
		return "backlog"
	}
}
func laneTitle(s string) string {
	switch s {
	case "needs-steward":
		return "Needs steward"
	case "working":
		return "In flight"
	case "review":
		return "Review"
	case "done":
		return "History"
	default:
		return "Backlog"
	}
}
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var a [24]byte
	i := len(a)
	for n > 0 {
		i--
		a[i] = byte('0' + n%10)
		n /= 10
	}
	return string(a[i:])
}

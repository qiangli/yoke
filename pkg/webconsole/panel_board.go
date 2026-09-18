// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/qiangli/yoke/pkg/board"
	"github.com/qiangli/yoke/pkg/resources"
)

const boardSchemaVersion = "bashy-console-board-v1"

// boardTTL is how long a collected board stays fresh.
//
// Thirty seconds rather than the launcher's five, because collecting one is
// EXPENSIVE in a way the tile probe is not: board.Collect fans out across every
// weave queue root on the host, forking `weave doctor` per root and doing git
// work in each. Measured here: ~3s wall, ten subprocesses. A board is also
// slow-moving by nature — sprints and runs change on the scale of minutes.
const boardTTL = 30 * time.Second

// boardPanelRows caps how many rows of each panel the overview carries.
//
// Without a cap the payload is 5.3 MB, of which the dag panel is 1.5 MB and the
// raw dag_runs slice another 2.3 MB — 8,779 pipeline runs on this host. That is
// not merely slow: docs/cloudbox-data-plane-block.md makes bulk payloads over
// the relay fail-closed, and a board panel shipping megabytes per poll has
// quietly grown a data plane. The overview carries every panel's COLLAPSED
// summary line — which is what a scan actually reads — plus a capped window of
// rows; the full set is one deliberate request away at /api/sprint/panel/{id}.
const boardPanelRows = 25

// boardCache serves the last collected board while a refresh runs behind it.
//
// Stale-while-revalidate rather than a plain TTL: a 3s collect on the request
// path would make every poll a 3s hang, and a page that freezes on refresh is
// worse than one showing data half a minute old — the age is reported, so a
// reader can tell how stale it is rather than having to guess.
type boardCache struct {
	mu         sync.Mutex
	at         time.Time
	board      *board.Board
	refreshing bool
}

// collectBoardFn is the collector, as a var so a test can substitute one that
// does not fan out across the host's real weave queues.
var collectBoardFn = collectBoard

// storyDetailFn is the one-story lookup, as a var for the same reason
// collectBoardFn is one: a browser case about how a BODY renders must be able
// to supply that body, and driving it through a real todo store would make the
// assertion about the store instead.
var storyDetailFn = board.StoryDetail

func collectBoard(ctx context.Context) (*board.Board, error) {
	// All:true matches the CLI exactly — "steward scope is always the
	// machine-global union, including completed records". The panel filters for
	// DISPLAY; it does not collect a different board than the role dashboards do.
	return board.Collect(ctx, board.Options{All: true}, board.DefaultSources(), nil)
}

// Get returns the cached board and its age. The first call blocks; later ones
// never do.
func (c *boardCache) Get(ctx, serverCtx context.Context) (*board.Board, time.Duration, error) {
	c.mu.Lock()
	b, at, refreshing := c.board, c.at, c.refreshing
	if b != nil && time.Since(at) < boardTTL {
		c.mu.Unlock()
		return b, time.Since(at), nil
	}
	if b == nil {
		// Nothing to serve stale. Collect on this request — once.
		c.mu.Unlock()
		fresh, err := collectBoardFn(ctx)
		if err != nil {
			return nil, 0, err
		}
		c.mu.Lock()
		c.board, c.at = fresh, time.Now()
		c.mu.Unlock()
		return fresh, 0, nil
	}
	if !refreshing {
		c.refreshing = true
		// The SERVER's context, not the request's: the refresh outlives the
		// request that noticed it was needed.
		go func() {
			fresh, err := collectBoardFn(serverCtx)
			c.mu.Lock()
			if err == nil {
				c.board, c.at = fresh, time.Now()
			} else {
				// Keep serving the last good board; its age says how old it is.
				c.at = time.Now().Add(-boardTTL)
			}
			c.refreshing = false
			c.mu.Unlock()
		}()
	}
	c.mu.Unlock()
	return b, time.Since(at), nil
}

type boardPanelView struct {
	ID        string     `json:"id"`
	Title     string     `json:"title"`
	Collapsed string     `json:"collapsed"`
	Columns   []string   `json:"columns,omitempty"`
	Rows      [][]string `json:"rows,omitempty"`
	RowTotal  int        `json:"row_total"`
}

type boardView struct {
	SchemaVersion string    `json:"schema_version"`
	Title         string    `json:"title"`
	Role          string    `json:"role"`
	Scope         string    `json:"scope"`
	GeneratedAt   time.Time `json:"generated_at"`
	AgeSeconds    int       `json:"age_seconds"`
	TTLSeconds    int       `json:"ttl_seconds"`

	Summary board.Summary  `json:"summary"`
	Rollup  board.Rollup   `json:"rollup"`
	Lanes   []board.Lane   `json:"lanes"`
	Agents  []board.Agent  `json:"agents"`
	Sprints []board.Sprint `json:"sprints"`
	Runs    []board.Run    `json:"runs"`
	// Todos are the sprint's STORIES. The overview carried sprints and runs
	// but never todos, so the web board could not show a card's stories at
	// all — it showed a count in the summary strip and nothing behind it. A
	// sprint whose stories are invisible reads as an empty sprint.
	Todos    []board.Todo     `json:"todos"`
	Panels   []boardPanelView `json:"panels"`
	Warnings []string         `json:"warnings,omitempty"`

	Resources   *resources.System      `json:"resources,omitempty"`
	Utilization *resources.Utilization `json:"utilization,omitempty"`

	// All reports whether history was included, so the page never claims to
	// show everything while showing only the live slice.
	All bool `json:"all"`
	// Totals are the UNFILTERED counts, so "3 of 78" is always sayable.
	SprintTotal int `json:"sprint_total"`
	RunTotal    int `json:"run_total"`
	TodoTotal   int `json:"todo_total"`
}

// terminalRun and doneSprint name the records that are HISTORY rather than live
// work. 75 of this host's 78 sprints and 77 of its 96 runs are terminal, so
// showing everything by default would bury the three sprints and nineteen runs
// a steward is actually looking for.
func terminalRun(state string) bool {
	switch strings.ToLower(state) {
	case "done", "failed", "killed", "abandoned", "no-op":
		return true
	}
	return false
}

func doneSprint(column string) bool { return strings.EqualFold(column, "done") }

// doneTodo names a story that is finished. Kept beside terminalRun and
// doneSprint so the three "this is history" rules stay visibly one idea.
func doneTodo(status string) bool {
	switch strings.ToLower(status) {
	case "done", "closed", "cancelled", "canceled":
		return true
	}
	return false
}

// handleBoardOverview is the steward board, projected for a browser.
//
// IT NEVER TOUCHES A LEASE. `board` is the one work verb the atlas marks
// CapReadOnly — "it reports across the machine but never starts, merges, or
// kills work" — and that is exactly the property a web view must not erode. A
// panel that refreshed a sprint's lease to keep its display current would make
// an idle browser tab look like a working conductor: the same class of lie as a
// board read that consumes an agent's mail.
func (s *server) handleBoardOverview(w http.ResponseWriter, r *http.Request) {
	b, age, err := s.boards.Get(r.Context(), s.opts.Ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	q := r.URL.Query()
	all := q.Get("all") == "1"
	rows := boardPanelRows
	if n, err := strconv.Atoi(q.Get("rows")); err == nil && n >= 0 {
		rows = min(n, 500)
	}

	v := boardView{
		SchemaVersion: boardSchemaVersion,
		Title:         b.Title, Role: b.Role, Scope: b.Scope,
		GeneratedAt: b.GeneratedAt,
		AgeSeconds:  int(age.Seconds()), TTLSeconds: int(boardTTL.Seconds()),
		Summary: b.Summary, Rollup: b.Rollup, Agents: b.Agents,
		Warnings: b.Warnings, Resources: b.Resources, Utilization: b.Utilization,
		All:         all,
		SprintTotal: len(b.Sprints), RunTotal: len(b.Runs), TodoTotal: len(b.Todos),
	}

	// The `done` lane is 75 of this host's 98 cards. It is history in kanban
	// clothing, and leaving it in the default view renders the twelve cards that
	// need a steward beside seventy-five that need nobody.
	v.Lanes = make([]board.Lane, 0, len(b.Lanes))
	for _, lane := range b.Lanes {
		if all || !strings.EqualFold(lane.ID, "done") {
			v.Lanes = append(v.Lanes, lane)
		}
	}

	v.Sprints = make([]board.Sprint, 0, len(b.Sprints))
	for _, sp := range b.Sprints {
		if all || !doneSprint(sp.Column) {
			v.Sprints = append(v.Sprints, sp)
		}
	}
	v.Runs = make([]board.Run, 0, len(b.Runs))
	for _, run := range b.Runs {
		if all || !terminalRun(run.State) {
			v.Runs = append(v.Runs, run)
		}
	}

	// Stories on a visible sprint are the sprint's progress record, including
	// the closed half. Hiding them made a 16-story sprint report 11 stories and
	// erased the evidence behind its completion count. Closed UNLINKED stories
	// and stories on hidden, finished sprints remain history unless all=1.
	visibleSprints := make(map[int64]bool, len(v.Sprints))
	for _, sp := range v.Sprints {
		visibleSprints[sp.ID] = true
	}
	v.Todos = make([]board.Todo, 0, len(b.Todos))
	for _, t := range b.Todos {
		if all || !doneTodo(t.Status) || visibleSprints[t.SprintID] {
			v.Todos = append(v.Todos, t)
		}
	}

	v.Panels = make([]boardPanelView, 0, len(b.Panels))
	for _, p := range b.Panels {
		pv := boardPanelView{ID: p.ID, Title: p.Title, Collapsed: p.Collapsed,
			Columns: p.Columns, RowTotal: len(p.Rows)}
		if n := min(rows, len(p.Rows)); n > 0 {
			pv.Rows = p.Rows[:n]
		}
		v.Panels = append(v.Panels, pv)
	}

	writeJSON(w, http.StatusOK, v)
}

// handleBoardPanel serves ONE panel's rows, paged. This is where the 8,779 dag
// runs are reachable — deliberately, by a reader who asked for them, rather
// than by every poll.
func (s *server) handleBoardPanel(w http.ResponseWriter, r *http.Request) {
	b, age, err := s.boards.Get(r.Context(), s.opts.Ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	id := r.PathValue("id")
	q := r.URL.Query()
	offset, _ := strconv.Atoi(q.Get("offset"))
	limit := 200
	if n, err := strconv.Atoi(q.Get("limit")); err == nil && n > 0 {
		limit = min(n, 2000)
	}
	for _, p := range b.Panels {
		if p.ID != id {
			continue
		}
		total := len(p.Rows)
		offset = max(0, min(offset, total))
		end := min(offset+limit, total)
		writeJSON(w, http.StatusOK, map[string]any{
			"schema_version": boardSchemaVersion,
			"id":             p.ID, "title": p.Title, "collapsed": p.Collapsed,
			"columns": p.Columns, "rows": p.Rows[offset:end],
			"row_total": total, "offset": offset, "age_seconds": int(age.Seconds()),
		})
		return
	}
	writeJSON(w, http.StatusNotFound, map[string]any{"error": "no panel " + id})
}

// handleBoardPage serves the board page from the LAUNCHER's root, like the
// terminal and the message board, so it keeps the launcher's <base href>.
func (s *server) handleBoardPage(w http.ResponseWriter, r *http.Request) {
	s.servePageFile(w, r, "board.html")
}

// handleBoardStory serves ONE story's full record, body included.
//
// Separate from the overview on purpose, and for the reason the panel endpoint
// already documents: the overview is POLLED, and the story bodies on this host
// run to seventy lines each. Folding them in would put a megabyte on every
// tick to serve a detail a reader opens one of at a time.
//
// Read-only like the rest of this page: `board` is the one work verb the atlas
// marks CapReadOnly, and a detail view must not become the place that erodes
// it.
func (s *server) handleBoardStory(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if strings.TrimSpace(id) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "story id required"})
		return
	}
	b, _, err := s.boards.Get(r.Context(), s.opts.Ctx)
	if err != nil || b == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "the board has not been collected yet"})
		return
	}
	// Match on the id the page rendered, which is the short form; a prefix
	// match keeps the link working whichever length a caller pasted.
	var found *board.Todo
	for i := range b.Todos {
		if b.Todos[i].ID == id || strings.HasPrefix(b.Todos[i].ID, id) {
			found = &b.Todos[i]
			break
		}
	}
	if found == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "no story " + id + " on this board"})
		return
	}
	st, err := storyDetailFn(*found)
	if err != nil {
		// Name what failed. A detail pane that silently shows nothing is the
		// same defect class this sprint is about.
		writeJSON(w, http.StatusBadGateway, map[string]string{
			"error": "could not read story " + id + ": " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, st)
}

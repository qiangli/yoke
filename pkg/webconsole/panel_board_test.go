// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/board"
	"github.com/qiangli/yoke/pkg/policy/coord"
	"github.com/qiangli/yoke/pkg/principal"
)

// fakeBoard builds a board from an injected Source, the isolation pattern
// pkg/board's own tests use — no HOME juggling, no real weave queues, and
// crucially no `weave doctor` subprocess fan-out.
func fakeBoard(t *testing.T) *board.Board {
	t.Helper()
	src := board.SourceFunc{SourceName: "fake", Func: func(_ context.Context, b *board.Board, _ board.Options) error {
		b.Sprints = []board.Sprint{
			{ID: 1, Title: "live sprint", Column: "doing", Manager: "agent-one", MeetRoomRef: "durable-room-one"},
			{ID: 2, Title: "finished sprint", Column: "done"},
			{ID: 3, Title: "another finished", Column: "done"},
		}
		b.Runs = []board.Run{
			{ID: 10, Label: "working run", State: "submitted"},
			{ID: 11, Label: "old run", State: "done"},
			{ID: 12, Label: "gone run", State: "abandoned"},
		}
		// Stories: two linked to the live sprint, one linked to a finished
		// one, one unlinked, one closed. Enough to pin grouping, the
		// history filter, and the unlinked case at once.
		b.Todos = []board.Todo{
			{ID: "aaa", Number: 1, Title: "linked story", Status: "todo", Priority: "p1", SprintID: 1},
			{ID: "bbb", Number: 2, Title: "second linked story", Status: "doing", Priority: "p2", SprintID: 1},
			{ID: "ccc", Number: 3, Title: "story on a done sprint", Status: "todo", SprintID: 2},
			{ID: "ddd", Number: 4, Title: "free-standing item", Status: "todo"},
			{ID: "eee", Number: 5, Title: "finished story", Status: "done", SprintID: 1},
		}
		b.Warnings = []string{"fake: a source failed"}
		return nil
	}}
	b, err := board.Collect(context.Background(), board.Options{All: true}, []board.Source{src}, nil)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	return b
}

// newBoardTestServer hands back both the handler and the server behind it, so a
// test can seed the board cache and never run the real DefaultSources() — which
// forks a `weave doctor` per queue root on the host it happens to run on.
func newBoardTestServer(t *testing.T) (http.Handler, *server) {
	t.Helper()
	s, h, closer, err := newHandler(Options{Ctx: context.Background()})
	if err != nil {
		t.Fatalf("newHandler: %v", err)
	}
	t.Cleanup(func() { _ = closer() })
	return h, s
}

func TestBoardOverviewHidesHistoryByDefault(t *testing.T) {
	h, s := newBoardTestServer(t)
	b := fakeBoard(t)
	s.boards.mu.Lock()
	s.boards.board, s.boards.at = b, time.Now()
	s.boards.mu.Unlock()

	d := getJSON(t, h, "/api/sprint")
	if got := len(d["sprints"].([]any)); got != 1 {
		t.Errorf("default view shows %d sprints, want 1 live (of 3)", got)
	}
	if got := len(d["runs"].([]any)); got != 1 {
		t.Errorf("default view shows %d runs, want 1 non-terminal (of 3)", got)
	}
	// The totals must still be the UNFILTERED counts, or the page cannot say
	// "1 of 3" and a filtered view reads as an empty machine.
	if d["sprint_total"].(float64) != 3 || d["run_total"].(float64) != 3 {
		t.Errorf("totals = %v/%v, want the unfiltered 3/3", d["sprint_total"], d["run_total"])
	}
	sp := d["sprints"].([]any)[0].(map[string]any)
	if sp["manager"] != "agent-one" || sp["meet_room_ref"] != "durable-room-one" {
		t.Errorf("sprint navigation = manager:%v room:%v", sp["manager"], sp["meet_room_ref"])
	}

	all := getJSON(t, h, "/api/sprint?all=1")
	if got := len(all["sprints"].([]any)); got != 3 {
		t.Errorf("?all=1 shows %d sprints, want all 3", got)
	}
	if got := len(all["runs"].([]any)); got != 3 {
		t.Errorf("?all=1 shows %d runs, want all 3", got)
	}
}

func TestBoardOverviewCarriesExistingClaimsPanel(t *testing.T) {
	h, s := newBoardTestServer(t)
	b := fakeBoard(t)
	b.Claims = []*coord.Claim{{Resource: "do1", Mode: coord.ModeLease,
		Holder: principal.Ref{Name: "lintel", Host: "dragon"}, Intent: "leaf replay",
		AcquiredAt: b.GeneratedAt.Add(-time.Minute), Heartbeat: b.GeneratedAt}}
	b.Panels = board.DefaultPanels().Build(b)
	s.boards.mu.Lock()
	s.boards.board, s.boards.at = b, time.Now()
	s.boards.mu.Unlock()

	d := getJSON(t, h, "/api/sprint")
	for _, raw := range d["panels"].([]any) {
		p := raw.(map[string]any)
		if p["id"] == "claims" {
			if p["title"] != "Claims" || p["row_total"].(float64) != 1 {
				t.Fatalf("claims panel = %#v", p)
			}
			return
		}
	}
	t.Fatal("existing Sprint-board overview has no Claims panel")
}

// The payload bound is a correctness property, not an optimization: the raw
// envelope is 5.3 MB here (8,779 dag runs), and the data-plane block makes bulk
// payloads over the relay fail-closed.
func TestBoardOverviewCapsRowsAndDropsDagRuns(t *testing.T) {
	h, s := newBoardTestServer(t)
	b := fakeBoard(t)
	b.DagRuns = make([]board.DagRun, 5000)
	for i := range b.DagRuns {
		b.DagRuns[i] = board.DagRun{RunID: "r", File: "f"}
	}
	b.Panels = []board.PanelView{{ID: "big", Title: "Big", Collapsed: "5000 rows",
		Columns: []string{"A"}, Rows: make([][]string, 5000)}}
	for i := range b.Panels[0].Rows {
		b.Panels[0].Rows[i] = []string{"x"}
	}
	s.boards.mu.Lock()
	s.boards.board, s.boards.at = b, time.Now()
	s.boards.mu.Unlock()

	w := do(h, "GET", "/api/sprint", "127.0.0.1:5555", nil)
	if strings.Contains(w.Body.String(), "dag_runs") {
		t.Error("overview carries dag_runs; it must never ship the raw pipeline log")
	}
	d := getJSON(t, h, "/api/sprint")
	p := d["panels"].([]any)[0].(map[string]any)
	if got := len(p["rows"].([]any)); got != boardPanelRows {
		t.Errorf("panel carries %d rows, want the %d cap", got, boardPanelRows)
	}
	// The cap must not hide how much was capped, or the page silently claims a
	// 5,000-row panel has 25 rows.
	if p["row_total"].(float64) != 5000 {
		t.Errorf("row_total = %v, want 5000", p["row_total"])
	}

	// The full set is reachable, deliberately, one request at a time.
	full := getJSON(t, h, "/api/sprint/panel/big?limit=500")
	if got := len(full["rows"].([]any)); got != 500 {
		t.Errorf("panel fetch returned %d rows, want 500", got)
	}
	if full["row_total"].(float64) != 5000 {
		t.Errorf("panel row_total = %v, want 5000", full["row_total"])
	}
	page2 := getJSON(t, h, "/api/sprint/panel/big?limit=10&offset=4995")
	if got := len(page2["rows"].([]any)); got != 5 {
		t.Errorf("offset past the end returned %d rows, want the remaining 5", got)
	}
	if got := do(h, "GET", "/api/sprint/panel/nope", "127.0.0.1:5555", nil).Code; got != http.StatusNotFound {
		t.Errorf("unknown panel = %d, want 404", got)
	}
}

// A 3s collect on the request path would make every poll a 3s hang.
func TestBoardCacheServesStaleWhileRefreshing(t *testing.T) {
	var calls atomic.Int32
	var c boardCache
	slow := func(ctx context.Context) (*board.Board, error) {
		calls.Add(1)
		return &board.Board{Title: "collected"}, nil
	}
	// Prime it the way Get's first-call path does.
	first, _ := slow(context.Background())
	c.mu.Lock()
	c.board, c.at = first, time.Now().Add(-2*boardTTL)
	c.mu.Unlock()

	// Ten concurrent readers on a stale cache: every one is served immediately,
	// and only ONE refresh is started.
	origin := collectBoardFn
	collectBoardFn = slow
	t.Cleanup(func() { collectBoardFn = origin })

	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b, _, err := c.Get(context.Background(), context.Background())
			if err != nil || b == nil {
				t.Errorf("stale read failed: %v", err)
			}
		}()
	}
	wg.Wait()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		done := !c.refreshing
		c.mu.Unlock()
		if done {
			break
		}
	}
	if n := calls.Load(); n > 2 {
		t.Errorf("%d collects for one stale window; ten readers must coalesce into one refresh", n)
	}
}

func TestBoardPageAndTile(t *testing.T) {
	h, _ := newBoardTestServer(t)
	w := do(h, "GET", "/sprint/", "127.0.0.1:5555", nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "board.js") {
		t.Fatalf("board page = %d, want 200 serving board.js", w.Code)
	}

	var found bool
	for _, p := range Discover() {
		if p.Name == "sprint" {
			found = true
			if p.Label != "Sprint" || p.Path != "/sprint/" || p.Source != "atlas" || !p.Available {
				t.Fatalf("board panel = %+v, want the available Sprint app at /sprint/", p)
			}
		}
	}
	if !found {
		t.Fatal("no board tile: the atlas WebSurface declaration is what Discover reads")
	}
	s := &server{}
	if hh, _ := s.panelHandler(Panel{Name: "sprint", Path: "/sprint/"}); hh != nil {
		t.Fatal("board must not also be mounted: coopauth.Mount would take the <base href> with it")
	}
}

func TestRetiredBoardRouteIsNotMounted(t *testing.T) {
	h, _ := newBoardTestServer(t)
	for _, oldPath := range []string{"/board", "/board/"} {
		w := do(h, "GET", oldPath, "127.0.0.1:5555", nil)
		if location := w.Header().Get("Location"); location != "" {
			t.Errorf("GET %s redirects to %q", oldPath, location)
		}
		if !strings.Contains(w.Body.String(), `id="grid-host"`) {
			t.Errorf("GET %s was claimed by an app instead of falling through to the launcher", oldPath)
		}
	}
}

// The panel is a READER. `board` is the one work verb the atlas marks
// CapReadOnly — "it reports across the machine but never starts, merges, or
// kills work" — and a browser view must not erode that.
//
// The assertion is that no board handler answers a mutating method, NOT that
// the server returns 405: the console's start page is a catch-all on "/", so an
// unmatched method falls through to it and gets the launcher. That is
// console-wide behaviour and not this panel's to change — but it does mean the
// meaningful check is "no board payload came back", not the status code.
func TestBoardServesNoMutatingMethod(t *testing.T) {
	h, s := newBoardTestServer(t)
	s.boards.mu.Lock()
	s.boards.board, s.boards.at = fakeBoard(t), time.Now()
	s.boards.mu.Unlock()

	for _, path := range []string{"/api/sprint", "/api/sprint/panel/sprints", "/sprint/"} {
		for _, m := range []string{"POST", "PUT", "DELETE", "PATCH"} {
			body := do(h, m, path, "127.0.0.1:5555", nil).Body.String()
			if strings.Contains(body, boardSchemaVersion) {
				t.Errorf("%s %s was answered by a board handler; the panel must expose GET only", m, path)
			}
		}
	}
	// And the read path really is registered, so the check above is not passing
	// merely because nothing is mounted.
	if !strings.Contains(do(h, "GET", "/api/sprint", "127.0.0.1:5555", nil).Body.String(), boardSchemaVersion) {
		t.Fatal("GET /api/sprint returned no board payload — the negative test above proves nothing")
	}
}

// TestBoardOverviewCarriesStories pins the fix for the web board showing a
// sprint with no stories under it.
//
// The overview payload carried sprints and runs and dropped todos ENTIRELY —
// "todos" appeared nowhere in this package — so every card on the page
// rendered as a title with no work beneath it, and a sprint whose stories are
// invisible reads as an empty sprint. The front-end was already willing: it
// showed a todo COUNT in the summary strip, which is what made the absence
// look like "this host has no todos" rather than "the payload has no field".
func TestBoardOverviewCarriesStories(t *testing.T) {
	h, s := newBoardTestServer(t)
	b := fakeBoard(t)
	s.boards.mu.Lock()
	s.boards.board, s.boards.at = b, time.Now()
	s.boards.mu.Unlock()

	d := getJSON(t, h, "/api/sprint")
	todos, ok := d["todos"].([]any)
	if !ok {
		t.Fatalf("the overview carries no `todos` field at all: keys=%v", keysOf(d))
	}
	// A closed story on the visible sprint remains in the default payload: it
	// is progress, not unrelated history. Open work is never hidden merely
	// because its sprint is done, so all five records remain visible here.
	if len(todos) != 5 {
		t.Errorf("default view shows %d stories, want 4 open + 1 closed on visible work", len(todos))
	}
	if d["todo_total"].(float64) != 5 {
		t.Errorf("todo_total = %v, want the unfiltered 5", d["todo_total"])
	}
	closed := 0
	for _, raw := range todos {
		if strings.EqualFold(raw.(map[string]any)["status"].(string), "done") {
			closed++
		}
	}
	if closed != 1 {
		t.Errorf("default view carries %d closed stories, want the one linked to the visible sprint", closed)
	}

	// The link is what makes them STORIES rather than a flat list.
	bySprint := map[float64]int{}
	for _, raw := range todos {
		if id, ok := raw.(map[string]any)["sprint_id"].(float64); ok {
			bySprint[id]++
		}
	}
	if bySprint[1] != 3 {
		t.Errorf("sprint 1 has %d linked stories, want 2 open + 1 closed", bySprint[1])
	}
	// An unlinked item is ordinary, not an error: it simply belongs to no card.
	if got := len(todos) - bySprint[1] - bySprint[2]; got != 1 {
		t.Errorf("%d unlinked stories, want 1", got)
	}
}

func TestBoardOverviewCarriesPerSprintStoryProgress(t *testing.T) {
	h, s := newBoardTestServer(t)
	b := fakeBoard(t)
	s.boards.mu.Lock()
	s.boards.board, s.boards.at = b, time.Now()
	s.boards.mu.Unlock()

	d := getJSON(t, h, "/api/sprint")
	sp := d["sprints"].([]any)[0].(map[string]any)
	if sp["story_total"] != float64(3) || sp["story_open"] != float64(2) || sp["story_closed"] != float64(1) {
		t.Fatalf("story progress = total:%v open:%v closed:%v, want 3/2/1", sp["story_total"], sp["story_open"], sp["story_closed"])
	}
}

// TestBoardOverviewAllIncludesFinishedStories: --all is one idea across lanes,
// sprints, runs and now stories. If they ever disagree the page can claim to
// show everything while hiding one collection.
func TestBoardOverviewAllIncludesFinishedStories(t *testing.T) {
	h, s := newBoardTestServer(t)
	b := fakeBoard(t)
	s.boards.mu.Lock()
	s.boards.board, s.boards.at = b, time.Now()
	s.boards.mu.Unlock()

	d := getJSON(t, h, "/api/sprint?all=1")
	if got := len(d["todos"].([]any)); got != 5 {
		t.Errorf("all=1 shows %d stories, want all 5", got)
	}
}

func keysOf(d map[string]any) []string {
	out := make([]string, 0, len(d))
	for k := range d {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestBoardStoryDetail pins the clickable-story endpoint: the overview stays
// a projection, and the body is one deliberate request away — the same rule
// the panel endpoint already follows, and the reason the overview does not
// simply carry every body (they run to seventy lines each here, and it is
// polled).
func TestBoardStoryDetail(t *testing.T) {
	h, s := newBoardTestServer(t)
	b := fakeBoard(t)
	s.boards.mu.Lock()
	s.boards.board, s.boards.at = b, time.Now()
	s.boards.mu.Unlock()

	// A story the board does not know must 404 naming the id, never 200 with
	// an empty pane — a detail view that silently shows nothing is the defect
	// class this board exists to report on.
	w := do(h, "GET", "/api/sprint/story/nosuchstory", "127.0.0.1:5555", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown story = %d, want 404", w.Code)
	}
	if !strings.Contains(w.Body.String(), "nosuchstory") {
		t.Errorf("404 does not name the id: %q", w.Body.String())
	}

	// A known story resolves far enough to attempt the lookup. The fake board
	// carries no real register behind it, so the honest outcome is a NAMED
	// upstream failure rather than a silent empty body.
	w = do(h, "GET", "/api/sprint/story/aaa", "127.0.0.1:5555", nil)
	if w.Code == http.StatusNotFound {
		t.Fatalf("a story the board knows about was reported unknown: %q", w.Body.String())
	}
	if w.Code != http.StatusOK && !strings.Contains(w.Body.String(), "aaa") {
		t.Errorf("failure does not name the story: %q", w.Body.String())
	}
}

// TestBoardStoryEmptyIDMatchesItsSibling records what an empty id actually
// does, rather than asserting a rule the codebase does not hold.
//
// `/api/sprint/story/` does not match the {id} pattern at all, so it falls
// through to the SPA catch-all and returns the page — exactly as
// `/api/sprint/panel/` already does. That is arguably wrong for an /api/ path
// (a JSON caller gets HTML), but it is ONE pre-existing routing behaviour
// shared by every /api/ typo, not something this endpoint introduced, and
// fixing it here alone would make the two siblings disagree. Pinned so the
// next reader sees it is known rather than rediscovering it.
func TestBoardStoryEmptyIDMatchesItsSibling(t *testing.T) {
	h, _ := newBoardTestServer(t)
	story := do(h, "GET", "/api/sprint/story/", "127.0.0.1:5555", nil).Code
	panel := do(h, "GET", "/api/sprint/panel/", "127.0.0.1:5555", nil).Code
	if story != panel {
		t.Fatalf("empty-id handling diverged from the sibling endpoint: story=%d panel=%d",
			story, panel)
	}
}

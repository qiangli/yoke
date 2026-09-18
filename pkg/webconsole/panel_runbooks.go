// Runbooks on the Sprint page.
//
// A runbook is a kb page of type "runbook" (kb.TypeRunbook): the reusable
// PROCEDURE a one-off story cites as [[kb:<slug>]] while the story itself
// carries this iteration's VALUES. The Sprint page lists the runbooks of every
// repo ring a live sprint tracks (StoryRoots) plus the host ring, and serves
// one page's body for the in-page pane — that is the whole surface. There is
// no /kb tile and no write path: the console is read-only by construction,
// and a runbook is authored with `bashy kb add --type runbook`.
//
// Reads go through Store.Load, never RecordOpen: `kb show` records an
// activation "open" that feeds --use-weight and --retire-unopened-after, and a
// pane the page re-renders on every poll must not count as someone reading.
package webconsole

import (
	"net/http"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/qiangli/yoke/pkg/kb"
)

// runbookRow is one entry of GET /api/sprint/runbooks — the index line, never
// the body (the same payload discipline as `kb list --json`).
type runbookRow struct {
	Slug        string   `json:"slug"`
	Seq         int      `json:"seq,omitempty"` // the ring-local human handle (kb:<seq>); 0 for a page written before seqs
	ID          string   `json:"id,omitempty"`  // the UUIDv7 identity
	Ref         string   `json:"ref"`           // kb:<slug> — copy straight into a story body
	Title       string   `json:"title"`
	Description string   `json:"description,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	Status      string   `json:"status,omitempty"`
	Ring        string   `json:"ring"` // "repo <basename>" | "host"
}

// runbookDetail is GET /api/sprint/runbook/{slug}: the row plus the body.
type runbookDetail struct {
	runbookRow
	Type string `json:"type"`
	Body string `json:"body"`
}

// runbookRing is one kb store the page reads, labelled for display.
type runbookRing struct {
	Name  string
	Store *kb.Store
}

// runbookRingsFn is the seam (same reason as storyDetailFn): a browser case
// about how the section renders hands in t.TempDir() stores, so nothing in a
// test reads or records against ~/.bashy/kb.
var runbookRingsFn = runbookRings

// runbookRings opens each distinct story root's committed docs/kb, in order,
// then the host ring last — so a repo's page wins over a host page of the
// same slug, matching the CLI's repo-before-host resolution.
func runbookRings(roots []string) []runbookRing {
	var out []runbookRing
	seen := map[string]bool{}
	for _, root := range roots {
		root = strings.TrimSpace(root)
		if root == "" || seen[root] {
			continue
		}
		seen[root] = true
		out = append(out, runbookRing{
			Name:  "repo " + filepath.Base(root),
			Store: kb.Open(filepath.Join(root, kb.RepoSub)),
		})
	}
	return append(out, runbookRing{Name: "host", Store: kb.Open("")})
}

// sprintRoots is the union of every sprint's StoryRoots on the cached board,
// deduped in insertion order. It answers "which repos' knowledge is relevant
// to this page" without a second collection.
func sprintRoots(roots [][]string) []string {
	var out []string
	for _, rs := range roots {
		for _, r := range rs {
			if r != "" && !slices.Contains(out, r) {
				out = append(out, r)
			}
		}
	}
	return out
}

func rowOf(p *kb.Page, ring string) runbookRow {
	return runbookRow{
		Slug: p.Slug, Seq: p.Seq, ID: p.ID, Ref: "kb:" + p.Slug, Title: p.Title, Description: p.Description,
		Tags: p.Tags, Status: p.Status, Ring: ring,
	}
}

// listRunbooks keeps the runbook-typed pages of every ring, first ring wins
// on a duplicate slug, sorted by slug. A ring that cannot be read contributes
// nothing rather than failing the section — an absent docs/kb is the common
// case, not an error.
func listRunbooks(rings []runbookRing) []runbookRow {
	var out []runbookRow
	seen := map[string]bool{}
	for _, ring := range rings {
		pages, err := ring.Store.List()
		if err != nil {
			continue
		}
		for _, p := range pages {
			if p.Type != kb.TypeRunbook || seen[p.Slug] {
				continue
			}
			seen[p.Slug] = true
			out = append(out, rowOf(p, ring.Name))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out
}

// loadRunbook resolves a slug through the rings in order. Only a runbook-typed
// page answers: the endpoint is the Sprint page's runbook pane, not a general
// kb reader, so a lesson with that slug is "not a runbook", not a page.
func loadRunbook(rings []runbookRing, slug string) (*runbookDetail, bool) {
	for _, ring := range rings {
		p, err := ring.Store.Load(slug)
		if err != nil {
			// Absent here → try the next ring; unreadable → the same, since a
			// broken page in one ring must not hide a good one in another.
			continue
		}
		if p.Type != kb.TypeRunbook {
			return nil, false
		}
		return &runbookDetail{runbookRow: rowOf(p, ring.Name), Type: p.Type, Body: p.Body}, true
	}
	return nil, false
}

func (s *server) runbookRingsForBoard(r *http.Request) ([]runbookRing, bool) {
	b, _, err := s.boards.Get(r.Context(), s.opts.Ctx)
	if err != nil || b == nil {
		return nil, false
	}
	roots := make([][]string, 0, len(b.Sprints))
	for i := range b.Sprints {
		roots = append(roots, b.Sprints[i].StoryRoots)
	}
	return runbookRingsFn(sprintRoots(roots)), true
}

// handleRunbooks is GET /api/sprint/runbooks.
func (s *server) handleRunbooks(w http.ResponseWriter, r *http.Request) {
	rings, ok := s.runbookRingsForBoard(r)
	if !ok {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "the board has not been collected yet"})
		return
	}
	rows := listRunbooks(rings)
	if rows == nil {
		rows = []runbookRow{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"schema": boardSchemaVersion, "runbooks": rows})
}

// handleRunbook is GET /api/sprint/runbook/{slug}.
func (s *server) handleRunbook(w http.ResponseWriter, r *http.Request) {
	slug := strings.TrimSpace(r.PathValue("slug"))
	if slug == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "runbook slug required"})
		return
	}
	rings, ok := s.runbookRingsForBoard(r)
	if !ok {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "the board has not been collected yet"})
		return
	}
	d, ok := loadRunbook(rings, slug)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no runbook " + slug})
		return
	}
	writeJSON(w, http.StatusOK, d)
}

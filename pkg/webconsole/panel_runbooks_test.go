package webconsole

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/kb"
)

// seedRunbookRings builds a repo ring and a host ring in temp dirs and points
// the seam at them: the repo carries a runbook and a lesson, the host carries
// a second runbook AND a runbook with the repo's slug (which must lose).
func seedRunbookRings(t *testing.T) {
	t.Helper()
	repo := kb.Open(filepath.Join(t.TempDir(), kb.RepoSub))
	host := kb.Open(t.TempDir())
	write := func(st *kb.Store, slug, typ, title, body string) {
		t.Helper()
		if err := st.Write(&kb.Page{
			Slug: slug, Form: kb.FormPage, Type: typ, Title: title,
			Description: "WHEN testing the runbooks section", Status: kb.StatusCandidate,
			Tags: []string{"runbook"}, Body: body,
		}, "add"); err != nil {
			t.Fatal(err)
		}
	}
	write(repo, "cut-release", kb.TypeRunbook, "cut a release (repo)", "## Steps\n\n1. tag it\n")
	write(repo, "a-lesson", kb.TypeLesson, "a lesson", "not a runbook")
	write(host, "cut-release", kb.TypeRunbook, "cut a release (host)", "host copy must lose")
	write(host, "audit-deps", kb.TypeRunbook, "audit dependencies", "## Steps\n\n1. list them\n")

	prev := runbookRingsFn
	runbookRingsFn = func([]string) []runbookRing {
		return []runbookRing{{Name: "repo x", Store: repo}, {Name: "host", Store: host}}
	}
	t.Cleanup(func() { runbookRingsFn = prev })
}

func TestRunbooksListsRepoAndHostRingsDedupedBySlug(t *testing.T) {
	h, s := newBoardTestServer(t)
	seedRunbookRings(t)
	s.boards.mu.Lock()
	s.boards.board, s.boards.at = fakeBoard(t), time.Now()
	s.boards.mu.Unlock()

	d := getJSON(t, h, "/api/sprint/runbooks")
	rows := d["runbooks"].([]any)
	if len(rows) != 2 {
		t.Fatalf("want 2 runbooks (lesson excluded, duplicate slug deduped), got %d: %v", len(rows), rows)
	}
	first := rows[0].(map[string]any)
	if first["slug"] != "audit-deps" || first["ring"] != "host" || first["ref"] != "kb:audit-deps" {
		t.Errorf("rows are sorted by slug and carry ring + ref: %v", first)
	}
	// The seq is the ring-local handle a human types (kb:<seq>) and the id
	// the identity; both are minted by Store.Write, so a row carries them.
	if seq, _ := first["seq"].(float64); seq < 1 {
		t.Errorf("a row carries the page's seq beside the slug: %v", first)
	}
	if id, _ := first["id"].(string); id == "" {
		t.Errorf("a row carries the page's id: %v", first)
	}
	second := rows[1].(map[string]any)
	if second["slug"] != "cut-release" || second["ring"] != "repo x" || second["title"] != "cut a release (repo)" {
		t.Errorf("the repo ring must win over the host ring for a duplicate slug: %v", second)
	}
	if _, has := second["body"]; has {
		t.Errorf("the list must not carry bodies: %v", second)
	}
}

func TestRunbookDetailAndUnknownSlug404(t *testing.T) {
	h, s := newBoardTestServer(t)
	seedRunbookRings(t)
	s.boards.mu.Lock()
	s.boards.board, s.boards.at = fakeBoard(t), time.Now()
	s.boards.mu.Unlock()

	rec := do(h, "GET", "/api/sprint/runbook/cut-release", "127.0.0.1:5555", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("detail: %d %s", rec.Code, rec.Body.String())
	}
	var d map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if d["type"] != kb.TypeRunbook || d["ring"] != "repo x" || !strings.Contains(d["body"].(string), "tag it") {
		t.Errorf("detail carries type, ring and the repo body: %v", d)
	}

	for _, slug := range []string{"nope", "a-lesson"} {
		rec := do(h, "GET", "/api/sprint/runbook/"+slug, "127.0.0.1:5555", nil)
		if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), slug) {
			t.Errorf("%s: want 404 naming the slug, got %d %s", slug, rec.Code, rec.Body.String())
		}
	}
}

func TestRunbookRoutesServeNoMutatingMethod(t *testing.T) {
	h, s := newBoardTestServer(t)
	seedRunbookRings(t)
	s.boards.mu.Lock()
	s.boards.board, s.boards.at = fakeBoard(t), time.Now()
	s.boards.mu.Unlock()

	for _, path := range []string{"/api/sprint/runbooks", "/api/sprint/runbook/cut-release"} {
		for _, m := range []string{"POST", "PUT", "DELETE", "PATCH"} {
			body := do(h, m, path, "127.0.0.1:5555", nil).Body.String()
			if strings.Contains(body, `"runbooks"`) || strings.Contains(body, `"body"`) {
				t.Errorf("%s %s was answered by a runbook handler; GET only", m, path)
			}
		}
	}
	if !strings.Contains(do(h, "GET", "/api/sprint/runbooks", "127.0.0.1:5555", nil).Body.String(), `"runbooks"`) {
		t.Error("GET /api/sprint/runbooks is not mounted")
	}
}

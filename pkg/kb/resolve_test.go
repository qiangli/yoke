// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package kb

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/qiangli/yoke/pkg/ref"
)

// writePage is a tiny helper: persist p into the store at dir.
func writePage(t *testing.T, dir string, p *Page) {
	t.Helper()
	if err := Open(dir).Write(p, "add"); err != nil {
		t.Fatal(err)
	}
}

func TestResolveKBFound(t *testing.T) {
	dir := t.TempDir()
	writePage(t, dir, &Page{
		Slug: "deploy-runbook", Type: TypeRunbook, Title: "Deploy runbook",
		Description: "how to ship", Status: StatusValidated,
	})

	g := ref.NewRegistry()
	RegisterRefs(g, dir, nil)

	n, err := g.Resolve("kb:deploy-runbook")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if n.Kind != ref.KB || n.ID != "deploy-runbook" || n.Ref != "kb:deploy-runbook" {
		t.Fatalf("identity = %+v", n)
	}
	if n.Title != "Deploy runbook" {
		t.Errorf("title = %q, want %q", n.Title, "Deploy runbook")
	}
	if n.Status != StatusValidated {
		t.Errorf("status = %q, want %q", n.Status, StatusValidated)
	}
	if n.Open != "bashy kb show deploy-runbook" {
		t.Errorf("open = %q", n.Open)
	}
	if want := "dir " + dir; n.Where != want {
		t.Errorf("where = %q, want %q", n.Where, want)
	}
	if n.Successor != "" {
		t.Errorf("live page has a successor: %q", n.Successor)
	}
}

func TestResolveKBNotFound(t *testing.T) {
	dir := t.TempDir() // readable, empty store
	g := ref.NewRegistry()
	RegisterRefs(g, dir, nil)

	_, err := g.Resolve("kb:nothing-here")
	if !errors.Is(err, ref.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// Edge: a superseded page still resolves, with Status=superseded and Successor
// pointing at the page that replaced it.
func TestResolveKBSupersededCarriesSuccessor(t *testing.T) {
	dir := t.TempDir()
	// The replacement, then the invalidated page linked forward to it — the
	// exact pair `kb supersede` records (old.Status=superseded, old.SupersededBy).
	writePage(t, dir, &Page{
		Slug: "new-way", Type: TypeLesson, Title: "The corrected way",
		Description: "do this", Status: StatusCandidate,
	})
	writePage(t, dir, &Page{
		Slug: "old-way", Type: TypeLesson, Title: "The old way",
		Description: "was wrong", Status: StatusSuperseded, SupersededBy: "new-way",
	})

	g := ref.NewRegistry()
	RegisterRefs(g, dir, nil)

	n, err := g.Resolve("kb:old-way")
	if err != nil {
		t.Fatalf("a superseded page must still resolve: %v", err)
	}
	if n.Status != StatusSuperseded {
		t.Errorf("status = %q, want %q", n.Status, StatusSuperseded)
	}
	if n.Successor != "kb:new-way" {
		t.Errorf("successor = %q, want %q", n.Successor, "kb:new-way")
	}
}

// The default (empty dir) path reads the repo ring of the cwd before the host
// store, exactly as `kb show` auto-detects.
func TestResolveKBRepoRingBeatsHost(t *testing.T) {
	root := kbGitRepo(t) // temp git repo + chdir into it
	host := t.TempDir()
	t.Setenv("BASHY_KB_DIR", host)

	repoDir := filepath.Join(root, RepoSub)
	writePage(t, repoDir, &Page{
		Slug: "shared", Type: TypeFact, Title: "repo copy",
		Description: "d", Status: StatusValidated,
	})
	writePage(t, host, &Page{
		Slug: "shared", Type: TypeFact, Title: "host copy",
		Description: "d", Status: StatusValidated,
	})

	g := ref.NewRegistry()
	RegisterRefs(g, "", nil) // auto: repo ring first, then host

	n, err := g.Resolve("kb:shared")
	if err != nil {
		t.Fatal(err)
	}
	if n.Title != "repo copy" {
		t.Fatalf("title = %q, want the repo-ring copy", n.Title)
	}
	if want := "repo " + repoDir; n.Where != want {
		t.Errorf("where = %q, want %q", n.Where, want)
	}
}

func TestResolveKBThreeHandles(t *testing.T) {
	dir := t.TempDir()
	writePage(t, dir, &Page{Slug: "release-cycle", Type: TypeRunbook, Title: "Release cycle", Description: "d"})
	writePage(t, dir, &Page{Slug: "second", Type: TypeRunbook, Title: "Second", Description: "d"})
	st := Open(dir)
	p, err := st.Load("release-cycle")
	if err != nil {
		t.Fatal(err)
	}
	if p.ID == "" || p.Seq != 1 {
		t.Fatalf("Write did not mint id/seq: id=%q seq=%d", p.ID, p.Seq)
	}
	if q, _ := st.Load("second"); q.Seq != 2 || q.ID == p.ID {
		t.Fatalf("second page: seq=%d id=%q", q.Seq, q.ID)
	}
	// Re-writing keeps both handles (mint only when empty).
	p.Description = "changed"
	if err := st.Write(p, "update"); err != nil {
		t.Fatal(err)
	}
	if r, _ := st.Load("release-cycle"); r.ID != p.ID || r.Seq != 1 {
		t.Fatalf("rewrite changed handles: %+v", r)
	}

	g := ref.NewRegistry()
	RegisterRefs(g, dir, nil)
	// MEASURED: two UUIDv7s minted in the same minute share their first 8 hex
	// (the top of the ms timestamp), so the 8-char prefix is AMBIGUOUS here —
	// the resolver names both candidates — and the 13-char dashed prefix
	// (the full ms timestamp) is the shortest unique one.
	if _, err := g.Resolve("kb:" + p.ID[:8]); err == nil || !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("8-hex prefix of same-minute v7 ids: err = %v, want ErrAmbiguous", err)
	}
	for _, in := range []string{"kb:release-cycle", "kb:1", "kb:#1", "kb:" + p.ID, "kb:" + p.ID[:13], "urn:dhnt:kb:" + p.ID} {
		n, err := g.Resolve(in)
		if err != nil {
			t.Fatalf("resolve %s: %v", in, err)
		}
		// seq is input only: the ref is always kb:<slug>.
		if n.ID != "release-cycle" || n.Ref != "kb:release-cycle" || n.UID != p.ID || n.Seq != 1 {
			t.Errorf("%s → %+v", in, n)
		}
	}
	for _, in := range []string{"kb:99", "kb:0192ffffffff"} {
		if _, err := g.Resolve(in); !errors.Is(err, ref.ErrNotFound) {
			t.Errorf("%s: err = %v, want ErrNotFound", in, err)
		}
	}
}

func TestResolveKBDuplicateSeqIsAmbiguous(t *testing.T) {
	dir := t.TempDir()
	// Two branches each minted MaxSeq+1 — after the merge both pages say seq 1.
	writePage(t, dir, &Page{Slug: "a", Seq: 1, Type: TypeRunbook, Title: "A", Description: "d"})
	writePage(t, dir, &Page{Slug: "b", Seq: 1, Type: TypeRunbook, Title: "B", Description: "d"})
	g := ref.NewRegistry()
	RegisterRefs(g, dir, nil)
	_, err := g.Resolve("kb:1")
	if err == nil || errors.Is(err, ref.ErrNotFound) || !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("duplicate seq: err = %v, want ErrAmbiguous", err)
	}
	// The slugs still resolve — nothing is renumbered.
	for _, in := range []string{"kb:a", "kb:b"} {
		if _, err := g.Resolve(in); err != nil {
			t.Errorf("%s: %v", in, err)
		}
	}
}

func TestResolveKBScopeSegment(t *testing.T) {
	t.Setenv("BASHY_KB_DIR", t.TempDir()) // the host store, hermetic
	writePage(t, DefaultDir(), &Page{Slug: "host-note", Type: TypeRunbook, Title: "Host", Description: "d"})
	repo := t.TempDir()
	writePage(t, filepath.Join(repo, RepoSub), &Page{Slug: "repo-note", Type: TypeRunbook, Title: "Repo", Description: "d"})
	lookup := func(scope string) (string, error) {
		if scope == "myrepo" {
			return repo, nil
		}
		return "", errors.New("unknown checkout " + scope)
	}
	g := ref.NewRegistry()
	RegisterRefs(g, "", lookup)
	if n, err := g.Resolve("kb:myrepo/repo-note"); err != nil || n.ID != "repo-note" {
		t.Fatalf("scoped slug: %+v %v", n, err)
	}
	if n, err := g.Resolve("kb:myrepo/1"); err != nil || n.ID != "repo-note" {
		t.Fatalf("scoped seq: %+v %v", n, err)
	}
	if n, err := g.Resolve("kb:user/host-note"); err != nil || n.ID != "host-note" {
		t.Fatalf("user scope: %+v %v", n, err)
	}
	if _, err := g.Resolve("kb:myrepo/host-note"); !errors.Is(err, ref.ErrNotFound) {
		t.Fatalf("a scope is ONLY that ring: %v", err)
	}
	if _, err := g.Resolve("kb:nowhere/x"); err == nil || errors.Is(err, ref.ErrNotFound) {
		t.Fatalf("unknown scope must be a named error: %v", err)
	}
	g2 := ref.NewRegistry()
	RegisterRefs(g2, "", nil)
	if _, err := g2.Resolve("kb:myrepo/repo-note"); err == nil || errors.Is(err, ref.ErrNotFound) {
		t.Fatalf("nil lookup: %v", err)
	}
}

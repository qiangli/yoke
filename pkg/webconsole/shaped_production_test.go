// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/qiangli/yoke/pkg/shape/contain"
	shape "github.com/qiangli/yoke/pkg/shape/state"
	shapeverify "github.com/qiangli/yoke/pkg/shape/verify"
	"github.com/qiangli/yoke/pkg/websession"
)

func productionNotesSource(t *testing.T, home string) (string, string) {
	t.Helper()
	dir := filepath.Join(home, "src", "notes")
	if err := copyTree(filepath.Join(notesFixture, "app"), dir); err != nil {
		t.Fatal(err)
	}
	r, err := gogit.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	wt, _ := r.Worktree()
	if err := wt.AddWithOptions(&gogit.AddOptions{All: true}); err != nil {
		t.Fatal(err)
	}
	base, err := wt.Commit("notes: base", &gogit.CommitOptions{Author: &object.Signature{
		Name: "fixture", Email: "fixture@example.test", When: time.Now(),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Storer.SetReference(plumbing.NewHashReference("refs/heads/pristine", base)); err != nil {
		t.Fatal(err)
	}
	return dir, base.String()
}

func productionCandidate(t *testing.T, app *shapedApp, id, baseRev string, mutate func(string)) Candidate {
	t.Helper()
	dir, err := app.candidateDir("device:alice", id)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(dir, "work")
	r, err := cloneAt(context.Background(), app.repo, work, baseRev)
	if err != nil {
		t.Fatal(err)
	}
	mutate(work)
	wt, _ := r.Worktree()
	if err := wt.AddWithOptions(&gogit.AddOptions{All: true}); err != nil {
		t.Fatal(err)
	}
	rev, err := wt.Commit("shape: candidate", &gogit.CommitOptions{Author: &object.Signature{
		Name: "shape-agent", Email: "shape@example.test", When: time.Now(),
	}})
	if err != nil {
		t.Fatal(err)
	}
	c := Candidate{ID: id, App: "notes", Principal: "device:alice", Request: "change notes",
		Status: candReady, BaseRev: baseRev, Rev: rev.String(), Agent: "codex:test", Created: time.Now().UTC()}
	if err := saveCandidate(dir, c); err != nil {
		t.Fatal(err)
	}
	return c
}

func pairedPrincipal(t *testing.T, h http.Handler, store *pairStore, name string) http.Header {
	t.Helper()
	_, secret, err := store.issueTicketForPrincipal([]string{"notes"}, time.Hour, time.Minute, name)
	if err != nil {
		t.Fatal(err)
	}
	w := do(h, http.MethodGet, pairRedeemPath+"?v="+pairQRVersion+"&t="+url.QueryEscape(secret), "192.0.2.20:5555", nil)
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			return withCookie(c)
		}
	}
	t.Fatalf("pair %s = %d: %s", name, w.Code, w.Body.String())
	return nil
}

// TestShapedProductionGates is the A9 red/green proof: the HTTP approval path
// uses the real verifier, sealed evidence gate and src activation store.
func TestShapedProductionGates(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	source, baseRev := productionNotesSource(t, home)
	workRoot := filepath.Join(t.TempDir(), "shape-work")
	runner := shapeverify.TrustedRunner{Bin: "/bin/sh", Effects: []string{"read"}, Level: contain.LevelProcess}
	opts := productionShapeOptions()
	opts.Home, opts.WorkRoot, opts.Runner, opts.Level = home, workRoot, []string{"/bin/sh"}, contain.LevelProcess
	opts.Verifier = productionVerifier{home: home, runner: runner}
	opts.Activator.(*productionActivator).home = home
	panel := Panel{Name: "notes", Label: "Notes", Path: "/notes/", LaunchPath: "/shape/notes/",
		ShapedSource: source, Auth: AuthSystem, Source: "registered", Available: true}
	pairPath := filepath.Join(home, "pairing.json")
	h := newTestHandler(t, Options{
		RequireLogin: true, Sessions: websession.NewStore(time.Hour, []byte("test-key-test-key-test-key-32byt")),
		Pairing: true, PairStorePath: pairPath, Panels: []Panel{panel}, Shape: opts,
	})
	pairs := newPairStore(pairPath)
	alice := pairedPrincipal(t, h, pairs, "alice")
	bob := pairedPrincipal(t, h, pairs, "bob")
	app := &shapedApp{name: "notes", repo: source, opts: opts}
	aliceStore, err := shape.OpenAt(home, "notes", "device:alice")
	if err != nil {
		t.Fatal(err)
	}
	if err := aliceStore.WriteData("notes.txt", []byte("private original\n")); err != nil {
		t.Fatal(err)
	}

	post := func(header http.Header, path string, want int) string {
		t.Helper()
		w := send(h, http.MethodPost, path, "192.0.2.20:5555", header, "")
		if w.Code != want {
			t.Fatalf("POST %s = %d, want %d: %s", path, w.Code, want, w.Body.String())
		}
		return w.Body.String()
	}
	stateOf := func(principal string) shape.State {
		t.Helper()
		st, err := shape.OpenAt(home, "notes", principal)
		if err != nil {
			t.Fatal(err)
		}
		got, err := st.Read()
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	good := productionCandidate(t, app, "1111111111111111", baseRev, func(work string) {
		p := filepath.Join(work, "public", "index.html")
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		b = bytes.Replace(b, []byte("<h1>Notes</h1>"), []byte("<h1>Notes <em>newest first</em></h1>"), 1)
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
	})
	var approved Candidate
	if err := json.Unmarshal([]byte(post(alice, "/api/shape/notes/candidates/"+good.ID+"/approve", http.StatusOK)), &approved); err != nil {
		t.Fatal(err)
	}
	if approved.Status != candApproved || approved.Verdict == nil || !approved.Verdict.Pass {
		t.Fatalf("approve = %+v", approved)
	}
	if got := stateOf("device:alice").ActiveRevision; got != good.Rev {
		t.Fatalf("alice active = %s, want %s", got, good.Rev)
	}
	if got := stateOf("device:bob").ActiveRevision; got != "" {
		t.Fatalf("bob active moved to %s", got)
	}
	if page := do(h, http.MethodGet, "/notes/", "192.0.2.20:5555", bob); !strings.Contains(page.Body.String(), "<h1>Notes</h1>") {
		t.Fatalf("bob no longer sees base: %s", page.Body.String())
	}

	t.Run("frozen check refuses edited early exit", func(t *testing.T) {
		bad := productionCandidate(t, app, "2222222222222222", good.Rev, func(work string) {
			if err := os.WriteFile(filepath.Join(work, "public", "index.html"), []byte("BROKEN\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(work, "checks", "smoke.bsh"), []byte("exit 0\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		})
		post(alice, "/api/shape/notes/candidates/"+bad.ID+"/approve", http.StatusConflict)
		if got := stateOf("device:alice").ActiveRevision; got != good.Rev {
			t.Fatalf("failed verification moved active to %s", got)
		}
	})

	t.Run("missing evidence is fail closed", func(t *testing.T) {
		noEvidence := productionCandidate(t, app, "3333333333333333", good.Rev, func(work string) {
			p := filepath.Join(work, "public", "notes.css")
			if err := os.WriteFile(p, []byte("body { color: navy; }\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		})
		dir, _ := app.candidateDir("device:alice", noEvidence.ID)
		err := (&productionActivator{home: home}).Activate(context.Background(), ActivateRequest{
			App: "notes", Principal: "device:alice", SourceDir: source, CandidateDir: filepath.Join(dir, "work"),
			BaseRev: good.Rev, CandidateRev: noEvidence.Rev, Request: noEvidence.Request,
		})
		if err == nil || !strings.Contains(err.Error(), "evidence") {
			t.Fatalf("activation err = %v, want evidence refusal", err)
		}
		if got := stateOf("device:alice").ActiveRevision; got != good.Rev {
			t.Fatalf("missing evidence moved active to %s", got)
		}
	})

	t.Run("preview escape", func(t *testing.T) {
		preview := productionCandidate(t, app, "4444444444444444", good.Rev, func(work string) {
			p := filepath.Join(work, "public", "notes.css")
			if err := os.WriteFile(p, []byte("body { color: teal; }\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		})
		w := send(h, http.MethodPost, "/shape/notes/preview/"+preview.ID+"/api/add",
			"192.0.2.20:5555", alice, "preview only")
		if w.Code != http.StatusOK {
			t.Fatalf("preview write = %d: %s", w.Code, w.Body.String())
		}
		got, err := aliceStore.ReadData("notes.txt")
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "private original\n" {
			t.Fatalf("preview escaped into active data: %q", got)
		}
	})

	t.Run("revoked grant", func(t *testing.T) {
		if _, err := pairs.revokeAll(); err != nil {
			t.Fatal(err)
		}
		w := send(h, http.MethodPost, "/api/shape/notes/candidates", "192.0.2.20:5555", alice, `{"request":"x"}`)
		if w.Code != http.StatusUnauthorized && w.Code != http.StatusForbidden {
			t.Fatalf("revoked shaping = %d: %s", w.Code, w.Body.String())
		}
	})
}

func TestShapedLauncherTileTargetsEditor(t *testing.T) {
	p := Panel{Name: "notes", Path: "/notes/", LaunchPath: "/shape/notes/", Available: true}
	apps := catalogStatus([]Panel{p})
	raw, err := json.Marshal(apps[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"launch_path":"/shape/notes/"`) {
		t.Fatalf("tile data = %s", raw)
	}
	js, err := os.ReadFile(filepath.Join("artifact", "app.js"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(js), "a.launch_path || a.path") {
		t.Fatal("launcher tile does not prefer the shaped editor path")
	}
}

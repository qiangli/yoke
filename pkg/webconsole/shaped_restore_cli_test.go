// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	shape "github.com/qiangli/yoke/pkg/shape/state"
	shapeverify "github.com/qiangli/yoke/pkg/shape/verify"
	"github.com/qiangli/yoke/pkg/src"
	"github.com/qiangli/yoke/pkg/websession"
)

// cliRestoreSource copies the notes app with a data-schema declaration at
// the base release, so approvals record the schema they store and a later
// narrow-schema restore is schema-incompatible.
func cliRestoreSource(t *testing.T, home string) string {
	t.Helper()
	dir := filepath.Join(home, "src", "notes")
	if err := copyTree(filepath.Join(notesFixture, "app"), dir); err != nil {
		t.Fatal(err)
	}
	feature := "schema_version: bashy-shape-feature-v1\nid: notes\norigin: device:alice\nbase_app: notes\nrevision: auto\nschema_range: {min: 1, max: 5}\n"
	if err := os.WriteFile(filepath.Join(dir, "shape.yaml"), []byte(feature), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := gogit.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	wt, err := r.Worktree()
	if err != nil {
		t.Fatal(err)
	}
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
	return dir
}

type cliRestoreHistoryEntry struct {
	Activation string    `json:"activation"`
	Revision   string    `json:"revision"`
	Kind       string    `json:"kind"`
	Request    string    `json:"request"`
	Author     string    `json:"author"`
	When       time.Time `json:"when"`
}

type cliRestoreHistory struct {
	Versions []cliRestoreHistoryEntry `json:"versions"`
	Entries  []cliRestoreHistoryEntry `json:"entries"`
}

// TestS525A10CLIProductionRestore is the S525.A10 acceptance proof through
// the REAL production path with no pre-sealed evidence: three versions
// asked and approved through the production approve handler, then the
// installed src CLI restores the oldest by its activation id and moves the
// active revision, `src history --json` reports the same .versions the app
// view serves with request/author/when, `src undo` returns to the
// pre-restore version, and a schema-incompatible and a verifier-failing
// restore through the CLI refuse with the reason and move nothing.
func TestS525A10CLIProductionRestore(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture units run under /bin/sh")
	}
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	// The CLI seals through the production runner; point its interpreter
	// at the shell the fixture checks run under.
	t.Setenv("BASHY_SHAPE_RUNNER", "/bin/sh")
	before := time.Now().UTC()
	source := cliRestoreSource(t, home)

	opts := productionShapeOptions()
	opts.Home, opts.WorkRoot, opts.Runner = home, filepath.Join(t.TempDir(), "shape-work"), []string{"/bin/sh"}
	opts.Delegate = &staticShaper{}
	opts.Verifier = productionVerifier{home: home, runner: shapeverify.TrustedRunner{Bin: "/bin/sh", Effects: []string{"read"}}}
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
	const peer = "192.0.2.20:5555"
	const principal = "device:alice"

	postJSON := func(path, body string, want int) string {
		t.Helper()
		hdr := alice.Clone()
		if hdr == nil {
			hdr = http.Header{}
		}
		hdr.Set("Content-Type", "application/json")
		w := send(h, http.MethodPost, path, peer, hdr, body)
		if w.Code != want {
			t.Fatalf("POST %s = %d, want %d: %s", path, w.Code, want, w.Body.String())
		}
		return w.Body.String()
	}
	get := func(path string, want int) string {
		t.Helper()
		w := do(h, http.MethodGet, path, peer, alice)
		if w.Code != want {
			t.Fatalf("GET %s = %d, want %d: %s", path, w.Code, want, w.Body.String())
		}
		return w.Body.String()
	}
	activeOf := func() string {
		t.Helper()
		st, err := shape.OpenAt(home, "notes", principal)
		if err != nil {
			t.Fatal(err)
		}
		got, err := st.Read()
		if err != nil {
			t.Fatal(err)
		}
		return got.ActiveRevision
	}
	runCLI := func(args ...string) (string, error) {
		t.Helper()
		cmd := src.NewSrcCmd()
		var out strings.Builder
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(args)
		err := cmd.Execute()
		return out.String(), err
	}
	askAndApprove := func(request string) Candidate {
		t.Helper()
		var asked Candidate
		if err := json.Unmarshal([]byte(postJSON("/api/shape/notes/candidates",
			`{"request":`+jsonQuote(request)+`}`, http.StatusAccepted)), &asked); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(60 * time.Second)
		for {
			var c Candidate
			w := do(h, http.MethodGet, "/api/shape/notes/candidates/"+asked.ID, peer, alice)
			if w.Code != http.StatusOK {
				t.Fatalf("GET candidate = %d: %s", w.Code, w.Body.String())
			}
			if err := json.Unmarshal(w.Body.Bytes(), &c); err != nil {
				t.Fatal(err)
			}
			if c.Status == candReady || c.Status == candVerified {
				break
			}
			if c.Status == candFailed {
				t.Fatalf("candidate failed: %s", c.Error)
			}
			if time.Now().After(deadline) {
				t.Fatalf("candidate %s never became ready (status %s)", asked.ID, c.Status)
			}
			time.Sleep(200 * time.Millisecond)
		}
		var approved Candidate
		if err := json.Unmarshal([]byte(postJSON(
			"/api/shape/notes/candidates/"+asked.ID+"/approve", "", http.StatusOK)), &approved); err != nil {
			t.Fatal(err)
		}
		if approved.Status != candApproved {
			t.Fatalf("approve = %+v, want approved", approved)
		}
		return approved
	}

	// Three versions through the production approve handler: real
	// verification seals real evidence for each approval.
	askAndApprove("first change")
	askAndApprove("second change")
	third := askAndApprove("third change")
	if got := activeOf(); got != third.Rev {
		t.Fatalf("active = %s, want %s", got, third.Rev)
	}

	// The oldest version's activation commit from the production history.
	hist, err := src.History(home, "notes", principal)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist.Entries) != 3 {
		t.Fatalf("history entries = %d, want 3 approved versions", len(hist.Entries))
	}
	oldest := hist.Entries[2]
	if oldest.Activation == "" || oldest.Activation == oldest.Revision {
		t.Fatalf("oldest entry = %+v, want a distinct activation commit", oldest)
	}

	// The installed CLI restores by activation id and moves active.
	if out, err := runCLI("restore", "notes", oldest.Activation, "--user", principal); err != nil ||
		!strings.Contains(out, "restored") {
		t.Fatalf("restore CLI = %q, %v", out, err)
	}
	if got := activeOf(); got != oldest.Revision {
		t.Fatalf("after CLI restore active = %s, want %s", got, oldest.Revision)
	}

	// The CLI history is the same history view the app serves: .versions
	// with request/author/when, mirrored by the .entries alias.
	out, err := runCLI("history", "notes", "--user", principal, "--json")
	if err != nil {
		t.Fatalf("history --json CLI: %v", err)
	}
	var decoded cliRestoreHistory
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("history --json = %q: %v", out, err)
	}
	if len(decoded.Versions) != 4 || len(decoded.Entries) != 4 {
		t.Fatalf("history --json versions = %d entries = %d, want 4 and 4",
			len(decoded.Versions), len(decoded.Entries))
	}
	newest := decoded.Versions[0]
	if !strings.HasPrefix(newest.Request, "restore to ") || newest.Author != principal || newest.When.IsZero() ||
		newest.When.Before(before.Add(-time.Minute)) {
		t.Fatalf("newest version = %+v, want a restore request with author and a recent time", newest)
	}
	var webView struct {
		Versions []cliRestoreHistoryEntry `json:"versions"`
	}
	if err := json.Unmarshal([]byte(get("/api/shape/notes/history", 200)), &webView); err != nil {
		t.Fatal(err)
	}
	if len(webView.Versions) != len(decoded.Versions) {
		t.Fatalf("web versions = %d, CLI versions = %d; want the same history view",
			len(webView.Versions), len(decoded.Versions))
	}
	for i := range webView.Versions {
		if webView.Versions[i].Revision != decoded.Versions[i].Revision {
			t.Fatalf("version %d web = %s, CLI = %s; want the same history view",
				i, webView.Versions[i].Revision, decoded.Versions[i].Revision)
		}
	}

	// The installed CLI undoes the restore back to the third approval.
	if out, err := runCLI("undo", "notes", "--user", principal); err != nil ||
		!strings.Contains(out, "restored") {
		t.Fatalf("undo CLI = %q, %v", out, err)
	}
	if got := activeOf(); got != third.Rev {
		t.Fatalf("after CLI undo active = %s, want %s", got, third.Rev)
	}

	// A narrow-schema approval followed by a normal one leaves narrow on
	// the line while the current schema is 5 again; the CLI restore of it
	// refuses with the schema reason and moves nothing.
	app := &shapedApp{name: "notes", repo: source, opts: opts}
	narrow := productionCandidate(t, app, "cccccccccccccccc", activeOf(), func(work string) {
		p := filepath.Join(work, "public", "index.html")
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, append(b, "<!-- narrow-schema -->\n"...), 0o644); err != nil {
			t.Fatal(err)
		}
		p = filepath.Join(work, "shape.yaml")
		b, err = os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		b = []byte(strings.Replace(string(b), "max: 5", "max: 2", 1))
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
	})
	postJSON("/api/shape/notes/candidates/"+narrow.ID+"/approve", "", http.StatusOK)
	normal := productionCandidate(t, app, "dddddddddddddddd", activeOf(), func(work string) {
		p := filepath.Join(work, "public", "index.html")
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, append(b, "<!-- back-to-wide -->\n"...), 0o644); err != nil {
			t.Fatal(err)
		}
		// The work clone inherits narrow's tree: declare the wide range
		// again so the current data schema is 5 after this approval.
		p = filepath.Join(work, "shape.yaml")
		b, err = os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		b = []byte(strings.Replace(string(b), "max: 2", "max: 5", 1))
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
	})
	postJSON("/api/shape/notes/candidates/"+normal.ID+"/approve", "", http.StatusOK)
	tipBefore := func() string {
		t.Helper()
		hh, err := src.History(home, "notes", principal)
		if err != nil {
			t.Fatal(err)
		}
		return hh.Tip
	}()
	if out, err := runCLI("restore", "notes", narrow.Rev, "--user", principal); err == nil ||
		!strings.Contains(err.Error(), "schema") {
		t.Fatalf("schema-incompatible restore = %q, %v; want a schema refusal", out, err)
	}
	if got := activeOf(); got != normal.Rev {
		t.Fatalf("schema-refused restore moved active to %s", got)
	}
	hh, err := src.History(home, "notes", principal)
	if err != nil {
		t.Fatal(err)
	}
	if hh.Tip != tipBefore {
		t.Fatal("schema-refused restore appended to the version line")
	}

	// A version whose tree fails the frozen check refuses through the CLI
	// with the verifier's reason and moves nothing.
	brokenRev := func() string {
		t.Helper()
		p := filepath.Join(source, "public", "index.html")
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, append(b, "BROKEN\n"...), 0o644); err != nil {
			t.Fatal(err)
		}
		r, err := gogit.PlainOpen(source)
		if err != nil {
			t.Fatal(err)
		}
		wt, err := r.Worktree()
		if err != nil {
			t.Fatal(err)
		}
		if err := wt.AddWithOptions(&gogit.AddOptions{All: true}); err != nil {
			t.Fatal(err)
		}
		hh, err := wt.Commit("notes: broken", &gogit.CommitOptions{Author: &object.Signature{
			Name: "fixture", Email: "fixture@example.test", When: time.Now(),
		}})
		if err != nil {
			t.Fatal(err)
		}
		return hh.String()
	}()
	setupGate := shape.GateFunc(func(candidate, base, principal string) (shape.EvidenceRef, error) {
		return shape.EvidenceRef{Verdict: "pass", EvidenceID: "ev-setup"}, nil
	})
	if _, err := src.Activate(home, "notes", brokenRev, principal, src.ActivateOptions{Gate: setupGate}); err != nil {
		t.Fatalf("record broken: %v", err)
	}
	if out, err := runCLI("restore", "notes", brokenRev, "--user", principal); err == nil ||
		!strings.Contains(strings.ToLower(err.Error()+out), "refus") {
		t.Fatalf("verifier-failing restore = %q, %v; want a refusal", out, err)
	}
	// The refused restore leaves the active revision where the broken
	// recording put it: nothing moves on a refusal.
	if got := activeOf(); got != brokenRev {
		t.Fatalf("refused restore moved active to %s", got)
	}
}

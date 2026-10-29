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
	"runtime"
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

func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
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

// fakeOCI stands in for bashy oci. Dragon has no podman, so it launches the
// requested argv on the host under exactly the environment the container
// would get, and records every call. With allow unset any call fails the
// test: that is the proof a run did not need a sandbox.
type fakeOCI struct {
	t     *testing.T
	allow bool
	calls []string
}

func (f *fakeOCI) Argv(dir string, argv, env []string, _ contain.Spec) ([]string, error) {
	if !f.allow {
		f.t.Errorf("unit routed to oci without asking for a sandbox: %q", argv)
		return nil, &contain.UnavailableError{Level: contain.LevelSandbox, Reason: "test: no sandbox on this host"}
	}
	f.calls = append(f.calls, strings.Join(argv, " "))
	// The image's bashy consumes Bash# decorators; the fixture's /bin/sh
	// cannot, so the fake drops them from the `-c BODY` it was handed.
	args := append([]string(nil), argv[1:]...)
	if len(args) > 1 && args[0] == "-c" {
		var keep []string
		for _, line := range strings.Split(args[1], "\n") {
			if !strings.HasPrefix(line, "@") {
				keep = append(keep, line)
			}
		}
		args[1] = strings.Join(keep, "\n")
	}
	out := append([]string{"/usr/bin/env", "-i"}, env...)
	return append(append(out, "/bin/sh"), args...), nil
}

// TestShapedProductionGates is the A9 red/green proof: the HTTP approval path
// uses the real verifier, sealed evidence gate and src activation store. It
// runs with NO sandbox available — no bwrap, no sandbox-exec, no podman on
// PATH, and an oci route that fails the test if used — because shaping must
// never require one, on any OS. What it asserts is what bashy's guards and
// the shaping trust roots provide: scrubbed env, effect cap, per-user data
// separation through the routes, frozen checks, evidence, revoked grants.
func TestShapedProductionGates(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture units run under /bin/sh")
	}
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	t.Setenv("PATH", t.TempDir())
	t.Setenv("SHAPE_TEST_SECRET", "hunter2")
	source, baseRev := productionNotesSource(t, home)
	workRoot := filepath.Join(t.TempDir(), "shape-work")
	runner := shapeverify.TrustedRunner{Bin: "/bin/sh", Effects: []string{"read"}}
	oci := &fakeOCI{t: t}
	opts := productionShapeOptions()
	opts.Home, opts.WorkRoot, opts.Runner, opts.OCI = home, workRoot, []string{"/bin/sh"}, oci
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
	bobStore, err := shape.OpenAt(home, "notes", "device:bob")
	if err != nil {
		t.Fatal(err)
	}
	if err := bobStore.WriteData("notes.txt", []byte("bob original\n")); err != nil {
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
	// The static UI change previews with no sandbox anywhere.
	if page := do(h, http.MethodGet, "/shape/notes/preview/"+good.ID+"/", "192.0.2.20:5555", alice); page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "newest first") {
		t.Fatalf("static preview = %d: %s", page.Code, page.Body.String())
	}
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

	// apiCandidate commits api/<name>.md with body as its Bash# unit.
	apiCandidate := func(id, name, body string) Candidate {
		t.Helper()
		return productionCandidate(t, app, id, stateOf("device:alice").ActiveRevision, func(work string) {
			p := filepath.Join(work, "api", name+".md")
			if err := os.WriteFile(p, []byte("# "+name+"\n\n```bsh\n"+body+"```\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		})
	}
	realData := func(stage string) {
		t.Helper()
		for label, check := range map[string]struct {
			store *shape.Store
			want  string
		}{"alice": {aliceStore, "private original\n"}, "bob": {bobStore, "bob original\n"}} {
			got, err := check.store.ReadData("notes.txt")
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != check.want {
				t.Fatalf("%s changed %s real data: %q", stage, label, got)
			}
		}
	}

	t.Run("api unit previews, verifies and activates without a sandbox", func(t *testing.T) {
		before := stateOf("device:alice").ActiveRevision
		c := apiCandidate("4444444444444444", "list",
			"f=\"$BASHY_APP_DATA/notes.txt\"\nprintf '{\"v\":2,\"notes\":\"%s\"}\\n' \"$(tr -d '\\n\"' < \"$f\")\"\n")
		preview := "/shape/notes/preview/" + c.ID + "/api/"
		if w := send(h, http.MethodPost, preview+"add", "192.0.2.20:5555", alice, "preview only"); w.Code != http.StatusOK {
			t.Fatalf("preview add = %d: %s", w.Code, w.Body.String())
		}
		w := do(h, http.MethodGet, preview+"list", "192.0.2.20:5555", alice)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"v":2`) || !strings.Contains(w.Body.String(), "preview only") {
			t.Fatalf("preview list = %d: %s", w.Code, w.Body.String())
		}
		realData("preview")
		if got := stateOf("device:alice").ActiveRevision; got != before {
			t.Fatalf("preview moved alice active to %s", got)
		}
		post(alice, "/api/shape/notes/candidates/"+c.ID+"/approve", http.StatusOK)
		if got := stateOf("device:alice").ActiveRevision; got != c.Rev {
			t.Fatalf("alice active = %s, want %s", got, c.Rev)
		}
		// Per-user data separation through the routes: alice's new unit
		// reads only alice's data; bob keeps the base unit over his own.
		w = do(h, http.MethodGet, "/notes/api/list", "192.0.2.20:5555", alice)
		if !strings.Contains(w.Body.String(), `"v":2`) || !strings.Contains(w.Body.String(), "private original") || strings.Contains(w.Body.String(), "bob") {
			t.Fatalf("alice list = %d: %s", w.Code, w.Body.String())
		}
		w = do(h, http.MethodGet, "/notes/api/list", "192.0.2.20:5555", bob)
		if strings.Contains(w.Body.String(), `"v":2`) || !strings.Contains(w.Body.String(), "bob original") || strings.Contains(w.Body.String(), "private original") {
			t.Fatalf("bob list = %d: %s", w.Code, w.Body.String())
		}
		realData("activation")
	})

	t.Run("unit env is scrubbed", func(t *testing.T) {
		c := apiCandidate("6666666666666666", "env", "env\n")
		w := do(h, http.MethodGet, "/shape/notes/preview/"+c.ID+"/api/env", "192.0.2.20:5555", alice)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "BASHY_APP_DATA=") {
			t.Fatalf("env = %d: %s", w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "hunter2") || strings.Contains(w.Body.String(), "SHAPE_TEST_SECRET") {
			t.Fatalf("secret leaked into the unit env: %s", w.Body.String())
		}
	})

	t.Run("effect cap refuses a unit declaring network", func(t *testing.T) {
		c := apiCandidate("7777777777777777", "fetch", "@effects(\"read,net\")\nfetch() { echo fetched; }\nfetch\n")
		w := do(h, http.MethodGet, "/shape/notes/preview/"+c.ID+"/api/fetch", "192.0.2.20:5555", alice)
		if w.Code != http.StatusForbidden || strings.Contains(w.Body.String(), "fetched") {
			t.Fatalf("net unit = %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("a declared sandbox island routes to bashy oci", func(t *testing.T) {
		oci.allow = true
		defer func() { oci.allow = false }()
		c := apiCandidate("8888888888888888", "iso", "@sandbox()\niso() { printf 'iso:%s\\n' \"$(cat \"$BASHY_APP_DATA/notes.txt\")\"; }\niso\n")
		w := do(h, http.MethodGet, "/shape/notes/preview/"+c.ID+"/api/iso", "192.0.2.20:5555", alice)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "iso:private original") {
			t.Fatalf("island = %d: %s", w.Code, w.Body.String())
		}
		if len(oci.calls) != 1 || !strings.HasPrefix(oci.calls[0], "sh -c @sandbox()") {
			t.Fatalf("oci calls = %q", oci.calls)
		}
		oci.allow = false
		w = do(h, http.MethodGet, "/shape/notes/preview/"+c.ID+"/api/list", "192.0.2.20:5555", alice)
		if w.Code != http.StatusOK || len(oci.calls) != 1 {
			t.Fatalf("an undeclared unit reached oci: %d %q", w.Code, oci.calls)
		}
	})

	t.Run("revoked grant", func(t *testing.T) {
		existing := productionCandidate(t, app, "5555555555555555", good.Rev, func(work string) {
			p := filepath.Join(work, "public", "notes.css")
			if err := os.WriteFile(p, []byte("body { color: maroon; }\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		})
		activeBefore := stateOf("device:alice").ActiveRevision
		if _, err := pairs.revokeAll(); err != nil {
			t.Fatal(err)
		}
		assertRefused := func(method, path, body string) {
			t.Helper()
			w := send(h, method, path, "192.0.2.20:5555", alice, body)
			if w.Code != http.StatusUnauthorized && w.Code != http.StatusForbidden {
				t.Fatalf("revoked %s %s = %d: %s", method, path, w.Code, w.Body.String())
			}
		}
		assertRefused(http.MethodPost, "/api/shape/notes/candidates", `{"request":"x"}`)
		assertRefused(http.MethodPost, "/api/shape/notes/candidates/"+existing.ID+"/approve", "")
		assertRefused(http.MethodGet, "/notes/api/list", "")
		assertRefused(http.MethodPost, "/notes/api/list", "")
		if got := stateOf("device:alice").ActiveRevision; got != activeBefore {
			t.Fatalf("revoked requests moved alice active from %s to %s", activeBefore, got)
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

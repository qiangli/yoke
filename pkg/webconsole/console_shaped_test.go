// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/qiangli/yoke/pkg/shape/verify"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/shape/state"
	"github.com/qiangli/yoke/pkg/src"
	"github.com/qiangli/yoke/pkg/websession"
)

// consoleTestPeer is a well-formed desktop peer id (base58, 20+ chars).
const consoleTestPeer = "QmYwAPJzv5CZsnA625s3Xf2nemtYgPpHdWEz79ojWnPbdG"

// TestConsoleShapedPresentation is the S526.B1a proof: the launcher's
// presentation is a shaped app on the public mechanism. (a) A principal
// without a revision is served the embedded bytes and no checkout is even
// created; (b) a principal's activated revision of desktop.html reaches that
// principal only; (c) a revision that touches shape/term/accounts is refused
// — already-recorded, it is not served; as a candidate, it cannot be
// verified or approved — and the manifest rows that say so are in place.
func TestConsoleShapedPresentation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	s, h, closer, err := newHandler(Options{
		Ctx: context.Background(), RequireLogin: true,
		Sessions: websession.NewStore(time.Hour, []byte("test-key-test-key-test-key-32byt")),
		Panels:   []Panel{}, Shape: ShapeOptions{Home: home},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closer() })

	// Three people, vouched by the cloud rung: no device scope, so every
	// launcher page is reachable, and three distinct principals.
	cloud := func(email string) http.Header {
		return http.Header{"X-Forwarded-Prefix": {"/cloud/apps"}, "Remote-User": {email}, "Remote-Email": {email}}
	}
	alice, bob, carol := cloud("alice@example.test"), cloud("bob@example.test"), cloud("carol@example.test")
	const peer = "127.0.0.1:5555"
	get := func(who http.Header, path string, want int) *httptest.ResponseRecorder {
		t.Helper()
		w := do(h, http.MethodGet, path, peer, who)
		if w.Code != want {
			t.Fatalf("GET %s = %d, want %d: %s", path, w.Code, want, w.Body.String())
		}
		return w
	}
	embedded := func(name string) string {
		t.Helper()
		b, err := fs.ReadFile(spaFS, name)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	desktopMarker := `id="desktop-page"`
	if !strings.Contains(embedded("desktop.html"), "desktop") {
		t.Fatal("embedded desktop.html is not what this test expects")
	}
	desktopMarker = embeddedTitle(t, embedded("desktop.html"))
	indexMarker := embeddedTitle(t, embedded("index.html"))

	// Manifest rows (D3): the three trust-surface pages are protected, the
	// shaped pages stay skill tier.
	for _, p := range []string{"shape.html", "shape.js", "term.html", "term.js", "accounts.html", "accounts.js"} {
		if got := src.PathClass("yoke", "pkg/webconsole/artifact/"+p); got != src.ClassProtected {
			t.Errorf("PathClass(artifact/%s) = %s, want protected", p, got)
		}
	}
	for _, p := range []string{"index.html", "desktop.html", "board.html", "inbox.html", "mb.html", "app.js", "app.css"} {
		if got := src.PathClass("yoke", "pkg/webconsole/artifact/"+p); got != src.ClassSkill {
			t.Errorf("PathClass(artifact/%s) = %s, want skill", p, got)
		}
	}
	if want := []string{"public/accounts.html", "public/accounts.js", "public/shape.html", "public/shape.js", "public/term.html", "public/term.js"}; strings.Join(consoleProtectedPaths(), ",") != strings.Join(want, ",") {
		t.Fatalf("consoleProtectedPaths = %v, want %v", consoleProtectedPaths(), want)
	}

	// (a) No revision anywhere: embedded bytes, and the checkout is not
	// created just to serve them.
	sourceDir := filepath.Join(home, "src", consoleApp)
	w := get(bob, "/desktop/"+consoleTestPeer+"/", http.StatusOK)
	if !strings.Contains(w.Body.String(), desktopMarker) {
		t.Fatalf("bob's desktop page is not the embedded page:\n%s", w.Body.String())
	}
	if _, err := os.Stat(sourceDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("serving the embedded launcher created %s (err=%v); the checkout must be lazy", sourceDir, err)
	}
	w = get(bob, "/", http.StatusOK)
	if !strings.Contains(w.Body.String(), indexMarker) || !strings.Contains(w.Body.String(), `href="shape/console/"`) {
		t.Fatalf("bob's launcher must be the embedded page with the shape entry point:\n%s", w.Body.String())
	}

	// The source is seeded on first need: the embedded bytes are the base
	// commit, pristine sits on it, and the Shape panel addresses it by name.
	app := s.consoleShaped()
	repo, err := app.open()
	if err != nil {
		t.Fatal(err)
	}
	baseCommit, err := base(repo)
	if err != nil {
		t.Fatal(err)
	}
	pristine, err := repo.Reference(plumbing.NewBranchReferenceName("pristine"), true)
	if err != nil || pristine.Hash() != baseCommit.Hash {
		t.Fatalf("pristine = %v (%v), want the base %s", pristine, err, baseCommit.Hash)
	}
	baseTree, _ := baseCommit.Tree()
	for _, name := range []string{"index.html", "desktop.html", "board.html", "inbox.html", "mb.html", "app.js", "app.css", "shape.html"} {
		f, err := baseTree.File("public/" + name)
		if err != nil {
			t.Fatalf("base tree lacks public/%s: %v", name, err)
		}
		if got, _ := f.Contents(); got != embedded(name) {
			t.Fatalf("base public/%s differs from the embedded bytes", name)
		}
	}
	w = get(alice, "/api/shape/"+consoleApp, http.StatusOK)
	var shapeState map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &shapeState); err != nil {
		t.Fatalf("shape state: %v: %s", err, w.Body.String())
	}
	// Seeding again (the next process start) is a no-op on the same bytes:
	// the base does not move.
	if again, err := fleet.SeedShapedSource(sourceDir, "again", writeConsoleBase); err != nil || again != baseCommit.Hash.String() {
		t.Fatalf("re-seeding identical bytes = %s, %v; want the base %s unchanged", again, err, baseCommit.Hash)
	}

	// (b) alice activates a revision of desktop.html (and app.css) through
	// the public activation path; only alice sees it.
	const alicePrincipal = "cloud:alice@example.test"
	aliceRev := consoleRevision(t, app, baseCommit.Hash.String(), map[string]string{
		"public/desktop.html": "<!doctype html><html><head><title>Alice desk</title></head><body><header id=\"bar\"></header>ALICE-DESKTOP</body></html>\n",
		"public/app.css":      "/* alice */ body{color:rebeccapurple}\n",
	})
	activateConsole(t, home, alicePrincipal, aliceRev)
	w = get(alice, "/desktop/"+consoleTestPeer+"/", http.StatusOK)
	if !strings.Contains(w.Body.String(), "ALICE-DESKTOP") {
		t.Fatalf("alice's desktop is not her revision:\n%s", w.Body.String())
	}
	if w.Header().Get("Vary") != "Cookie" || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("per-principal page must not be shared-cacheable: %v", w.Header())
	}
	if !strings.Contains(w.Body.String(), `<base href="/cloud/apps/">`) {
		t.Fatalf("shaped page must still get the mount's <base href>:\n%s", w.Body.String())
	}
	if got := get(bob, "/desktop/"+consoleTestPeer+"/", http.StatusOK).Body.String(); strings.Contains(got, "ALICE-DESKTOP") || !strings.Contains(got, desktopMarker) {
		t.Fatalf("bob received alice's desktop revision:\n%s", got)
	}
	if got := get(alice, "/app.css", http.StatusOK).Body.String(); !strings.Contains(got, "rebeccapurple") {
		t.Fatalf("alice's app.css is not her revision: %q", got)
	}
	if got := get(bob, "/app.css", http.StatusOK).Body.String(); got != embedded("app.css") {
		t.Fatalf("bob's app.css is not the embedded one")
	}
	// Alice's unchanged pages come from her tree too and remain the base
	// bytes; the protected review page is embedded and loads embedded assets
	// even for alice, whose app.css is shaped.
	if got := get(alice, "/", http.StatusOK).Body.String(); !strings.Contains(got, indexMarker) || !strings.Contains(got, "?v="+aliceRev[:12]) {
		t.Fatalf("alice's launcher must be her revision's index stamped with her revision:\n%s", got)
	}
	shapePage := get(alice, "/shape/"+consoleApp+"/", http.StatusOK).Body.String()
	if !strings.Contains(shapePage, `href="base/app.css`) || !strings.Contains(shapePage, `src="base/shape.js`) {
		t.Fatalf("the review page must load embedded assets from base/:\n%s", shapePage)
	}
	if got := get(alice, "/base/app.css", http.StatusOK).Body.String(); got != embedded("app.css") {
		t.Fatal("base/app.css served alice's shaped stylesheet to the review page")
	}
	get(alice, "/base/shape.html", http.StatusNotFound)

	// (c) carol's recorded revision overrides shape.js: refused as a whole.
	// She is served the base, and the protected script is the embedded one.
	const carolPrincipal = "cloud:carol@example.test"
	carolRev := consoleRevision(t, app, baseCommit.Hash.String(), map[string]string{
		"public/index.html": "<!doctype html><html><head><title>Carol</title></head><body>CAROL-INDEX</body></html>\n",
		"public/shape.js":   "document.querySelector('#approve')?.click();\n",
	})
	carolState, _ := state.OpenAt(home, consoleApp, carolPrincipal)
	if err := carolState.Write(state.State{ActiveRevision: carolRev}); err != nil {
		t.Fatal(err)
	}
	if got := get(carol, "/", http.StatusOK).Body.String(); strings.Contains(got, "CAROL-INDEX") || !strings.Contains(got, indexMarker) {
		t.Fatalf("a revision touching shape.js was served:\n%s", got)
	}
	if got := get(carol, "/shape.js", http.StatusOK).Body.String(); got != embedded("shape.js") {
		t.Fatal("carol's shaped shape.js was served")
	}
	if err := app.guardProtected(repo, mustCommit(t, repo, carolRev)); err == nil || !strings.Contains(err.Error(), "public/shape.js (overridden)") {
		t.Fatalf("guardProtected(carol) = %v, want an override refusal naming public/shape.js", err)
	}
	// Alice's clean revision is unaffected by the refusal.
	if got := get(alice, "/desktop/"+consoleTestPeer+"/", http.StatusOK).Body.String(); !strings.Contains(got, "ALICE-DESKTOP") {
		t.Fatal("alice lost her revision")
	}

	// Preview: a clean candidate's pages render under the preview prefix with
	// the candidate's own bytes, the launcher's Go API answers beneath it, and
	// the protected script is still the embedded one.
	clean := consoleCandidate(t, app, alicePrincipal, "fedcba9876543210", aliceRev, func(work string) {
		if err := os.WriteFile(filepath.Join(work, "public", "index.html"),
			[]byte("<!doctype html><html><head><title>Alice candidate</title></head><body><header id=\"bar\"></header>ALICE-CANDIDATE</body></html>\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	previewPrefix := "/shape/" + consoleApp + "/preview/" + clean.ID + "/"
	preview := get(alice, previewPrefix, http.StatusOK).Body.String()
	if !strings.Contains(preview, "ALICE-CANDIDATE") || !strings.Contains(preview, `<base href="/cloud/apps`+previewPrefix+`">`) {
		t.Fatalf("preview must render the candidate page at the preview prefix:\n%s", preview)
	}
	if got := get(alice, previewPrefix+"api/session", http.StatusOK).Body.String(); !strings.Contains(got, `"user"`) {
		t.Fatalf("preview api/ must reach the launcher's API: %s", got)
	}
	if got := get(alice, previewPrefix+"desktop/"+consoleTestPeer+"/", http.StatusOK).Body.String(); !strings.Contains(got, "ALICE-DESKTOP") {
		t.Fatalf("preview desktop page is not the candidate's:\n%s", got)
	}
	if got := get(alice, previewPrefix+"shape.js", http.StatusOK).Body.String(); got != embedded("shape.js") {
		t.Fatal("preview served a non-embedded protected script")
	}
	// Links to routes outside the shaped presentation leave the preview for
	// the live page under the real mount (S526.F3a: the sparkle link 404'd).
	if w := get(alice, previewPrefix+"shape/"+consoleApp+"/", http.StatusSeeOther); w.Header().Get("Location") != "/cloud/apps/shape/"+consoleApp+"/" {
		t.Fatalf("preview shape link Location = %q, want the live shape page", w.Header().Get("Location"))
	}
	if got := get(bob, previewPrefix, http.StatusNotFound).Body.String(); strings.Contains(got, "ALICE-CANDIDATE") {
		t.Fatal("bob can preview alice's candidate")
	}

	// (c) as a candidate: dropping term.html cannot be verified or approved,
	// before any verifier is consulted (none is configured here).
	cand := consoleCandidate(t, app, carolPrincipal, "0123456789abcdef", baseCommit.Hash.String(), func(work string) {
		if err := os.Remove(filepath.Join(work, "public", "term.html")); err != nil {
			t.Fatal(err)
		}
	})
	post := func(who http.Header, path string, want int) Candidate {
		t.Helper()
		w := send(h, http.MethodPost, path, peer, who, "")
		if w.Code != want {
			t.Fatalf("POST %s = %d, want %d: %s", path, w.Code, want, w.Body.String())
		}
		var c Candidate
		_ = json.Unmarshal(w.Body.Bytes(), &c)
		return c
	}
	verified := post(carol, "/api/shape/"+consoleApp+"/candidates/"+cand.ID+"/verify", http.StatusOK)
	if verified.Verdict == nil || verified.Verdict.Pass || !strings.Contains(verified.Verdict.Detail, "public/term.html (dropped)") {
		t.Fatalf("verify of a protected-path candidate = %+v, want a failing verdict naming public/term.html", verified.Verdict)
	}
	refused := post(carol, "/api/shape/"+consoleApp+"/candidates/"+cand.ID+"/approve", http.StatusConflict)
	if refused.Status == candApproved {
		t.Fatal("a candidate dropping term.html was approved")
	}
	if st, _ := carolState.Read(); st.ActiveRevision != carolRev {
		t.Fatalf("refusal changed carol's recorded state: %+v", st)
	}
}

func embeddedTitle(t *testing.T, page string) string {
	t.Helper()
	i := strings.Index(page, "<title>")
	j := strings.Index(page, "</title>")
	if i < 0 || j < i {
		t.Fatal("page has no <title>")
	}
	return page[i : j+len("</title>")]
}

func mustCommit(t *testing.T, repo *gogit.Repository, rev string) *object.Commit {
	t.Helper()
	c, err := repo.CommitObject(plumbing.NewHash(rev))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// consoleRevision shapes a revision the way a candidate is made — a clone
// at baseRev, edits, one commit — and imports it into the managed checkout
// the way activation does. Empty content deletes the file.
func consoleRevision(t *testing.T, app *shapedApp, baseRev string, files map[string]string) string {
	t.Helper()
	work := filepath.Join(t.TempDir(), "work")
	r, err := cloneAt(context.Background(), app.repo, work, baseRev)
	if err != nil {
		t.Fatal(err)
	}
	for rel, body := range files {
		p := filepath.Join(work, filepath.FromSlash(rel))
		if body == "" {
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	wt, _ := r.Worktree()
	if err := wt.AddWithOptions(&gogit.AddOptions{All: true}); err != nil {
		t.Fatal(err)
	}
	rev, err := wt.Commit("shape: revision", &gogit.CommitOptions{Author: &object.Signature{Name: "t", Email: "t@example.test", When: time.Now()}})
	if err != nil {
		t.Fatal(err)
	}
	from := plumbing.ReferenceName("refs/heads/bashy-shape-test")
	if err := r.Storer.SetReference(plumbing.NewHashReference(from, rev)); err != nil {
		t.Fatal(err)
	}
	source, err := gogit.PlainOpen(app.repo)
	if err != nil {
		t.Fatal(err)
	}
	to := plumbing.ReferenceName("refs/bashy-shape/test/" + rev.String())
	remote := gogit.NewRemote(source.Storer, &config.RemoteConfig{Name: "t-" + rev.String()[:8], URLs: []string{work}})
	if err := remote.Fetch(&gogit.FetchOptions{RefSpecs: []config.RefSpec{config.RefSpec("+" + from.String() + ":" + to.String())}}); err != nil && !errors.Is(err, gogit.NoErrAlreadyUpToDate) {
		t.Fatal(err)
	}
	return rev.String()
}

// activateConsole switches a principal to rev through pkg/src's activation,
// with a passing gate standing in for the sealed verifier evidence.
func activateConsole(t *testing.T, home, principal, rev string) {
	t.Helper()
	gate := state.GateFunc(func(candidate, base, principal string) (state.EvidenceRef, error) {
		return state.EvidenceRef{Verdict: "pass", EvidenceID: "test"}, nil
	})
	if _, err := src.Activate(home, consoleApp, rev, principal, src.ActivateOptions{Gate: gate, Request: "test"}); err != nil {
		t.Fatal(err)
	}
}

// consoleCandidate records a ready candidate for principal: a clone at
// baseRev mutated and committed, the way the shaping agent leaves it.
func consoleCandidate(t *testing.T, app *shapedApp, principal, id, baseRev string, mutate func(work string)) Candidate {
	t.Helper()
	dir, err := app.candidateDir(principal, id)
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
	rev, err := wt.Commit("shape: candidate", &gogit.CommitOptions{Author: &object.Signature{Name: "shape-agent", Email: "shape@example.test", When: time.Now()}})
	if err != nil {
		t.Fatal(err)
	}
	c := Candidate{ID: id, App: app.name, Principal: principal, Request: "drop the terminal page",
		Status: candReady, BaseRev: baseRev, Rev: rev.String(), Agent: "codex:test", Created: time.Now().UTC()}
	if err := saveCandidate(dir, c); err != nil {
		t.Fatal(err)
	}
	return c
}

// TestConsoleBaseDeclaresAnEffectCap: the verifier refuses any candidate whose
// base declares no effect cap, so a console base without a contract made every
// launcher wish unapprovable (S526.B1a3, found live).
func TestConsoleBaseDeclaresAnEffectCap(t *testing.T) {
	root := t.TempDir()
	if err := writeConsoleBase(root); err != nil {
		t.Fatal(err)
	}
	c, err := verify.DecoratorReader{}.ReadContract(root)
	if err != nil {
		t.Fatal(err)
	}
	if c.Cap() == "" {
		t.Fatal("console base declares no effect cap")
	}
	for _, f := range []string{"checks/smoke.bsh", "public/index.html"} {
		if _, err := os.Stat(filepath.Join(root, f)); err != nil {
			t.Fatalf("console base lacks %s: %v", f, err)
		}
	}
}

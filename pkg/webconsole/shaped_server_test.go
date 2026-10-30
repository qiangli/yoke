// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/qiangli/yoke/pkg/coopauth"
	"github.com/qiangli/yoke/pkg/shape/contain"
	shape "github.com/qiangli/yoke/pkg/shape/state"
	shapeverify "github.com/qiangli/yoke/pkg/shape/verify"
	"github.com/qiangli/yoke/pkg/src"
)

const serverAppFixture = "testdata/shaped/serverapp/app"

// serverAppSource builds a managed src checkout at home/src/serverapp from
// the server fixture: the base release (version v0, under pristine), then
// v1 and v2 (each only changing the version constant) and vbroken, whose Go
// does not compile.
func serverAppSource(t *testing.T, home string) (source, base, v1, v2, vbroken string) {
	t.Helper()
	work := filepath.Join(t.TempDir(), "work")
	if err := copyTree(serverAppFixture, work); err != nil {
		t.Fatal(err)
	}
	r, err := gogit.PlainInit(work, false)
	if err != nil {
		t.Fatal(err)
	}
	commit := func(msg string, files map[string]string) string {
		t.Helper()
		for rel, body := range files {
			if err := os.WriteFile(filepath.Join(work, rel), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		wt, err := r.Worktree()
		if err != nil {
			t.Fatal(err)
		}
		if err := wt.AddWithOptions(&gogit.AddOptions{All: true}); err != nil {
			t.Fatal(err)
		}
		h, err := wt.Commit(msg, &gogit.CommitOptions{Author: &object.Signature{
			Name: "server-fixture", Email: "server@example.test", When: time.Now(),
		}})
		if err != nil {
			t.Fatal(err)
		}
		return h.String()
	}
	ver := func(v string) map[string]string {
		return map[string]string{"version.go": "package main\n\nconst version = \"" + v + "\"\n"}
	}
	base = commit("serverapp: base", nil)
	v1 = commit("serverapp: v1", ver("v1"))
	v2 = commit("serverapp: v2", ver("v2"))
	vbroken = commit("serverapp: vbroken", map[string]string{"version.go": "package main\n\nconst version = \n"})
	source = filepath.Join(home, "src", "serverapp")
	if _, err := gogit.PlainClone(source, false, &gogit.CloneOptions{URL: work}); err != nil {
		t.Fatal(err)
	}
	sr, err := gogit.PlainOpen(source)
	if err != nil {
		t.Fatal(err)
	}
	// HEAD (the base revision anonymous callers and new principals get)
	// is the base release, not the tip of the fixture's history.
	wt, err := sr.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if err := wt.Reset(&gogit.ResetOptions{Commit: plumbing.NewHash(base), Mode: gogit.HardReset}); err != nil {
		t.Fatal(err)
	}
	if err := sr.Storer.SetReference(plumbing.NewHashReference("refs/heads/pristine", plumbing.NewHash(base))); err != nil {
		t.Fatal(err)
	}
	return source, base, v1, v2, vbroken
}

type serverAppHarness struct {
	t    *testing.T
	s    *server
	home string
	app  http.Handler
}

func newServerAppHarness(t *testing.T, home string, panels ...Panel) *serverAppHarness {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH")
	}
	opts := productionShapeOptions()
	opts.Home, opts.WorkRoot, opts.Runner = home, filepath.Join(t.TempDir(), "shape-work"), []string{"/bin/sh"}
	opts.Level = contain.LevelProcess
	opts.Verifier = productionVerifier{home: home, runner: shapeverify.TrustedRunner{Bin: "/bin/sh", Effects: []string{"read"}}}
	opts.Activator.(*productionActivator).home = home
	opts.ServerIdle = time.Hour
	s, _, closer, err := newHandler(Options{Ctx: context.Background(), Panels: panels, Shape: opts})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closer() })
	return &serverAppHarness{t: t, s: s, home: home}
}

// do sends one request to panel p's mount as principal ("" = anonymous),
// through coopauth.Mount exactly as the console mounts a shaped app.
func (h *serverAppHarness) do(p Panel, principal, method, target, body string, hdr http.Header) *httptest.ResponseRecorder {
	h.t.Helper()
	mount := strings.TrimSuffix(p.Path, "/")
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	for k, vs := range hdr {
		for _, v := range vs {
			r.Header.Add(k, v)
		}
	}
	r = withPrincipal(r, principal)
	w := httptest.NewRecorder()
	coopauth.Mount(mount, h.s.shapedApp(p).handler()).ServeHTTP(w, r)
	return w
}

func activateFor(t *testing.T, home, app, rev, principal string) {
	t.Helper()
	gate := shape.GateFunc(func(candidate, base, principal string) (shape.EvidenceRef, error) {
		return shape.EvidenceRef{Verdict: "pass", EvidenceID: "ev-test"}, nil
	})
	if _, err := src.Activate(home, app, rev, principal, src.ActivateOptions{Gate: gate}); err != nil {
		t.Fatalf("activate %s for %s: %v", rev[:8], principal, err)
	}
}

func serverPanel(source string) Panel {
	return Panel{Name: "serverapp", Label: "Server app", Path: "/serverapp/", ShapedSource: source,
		Auth: AuthSystem, Source: "registered", Available: true}
}

// TestShapedServerPerPrincipalRevision: a shaped app that declares a server
// is built from each principal's active revision and proxied to — alice on
// v1, bob and anonymous callers on the base — with the mount stripped, the
// console's session cookie withheld, no-store kept, and each principal's
// data private to them. The idle sweep stops servers and the next request
// starts them again.
func TestShapedServerPerPrincipalRevision(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	source, _, v1, _, _ := serverAppSource(t, home)
	p := serverPanel(source)
	h := newServerAppHarness(t, home, p)
	const alice, bob = "device:alice", "device:bob"
	activateFor(t, home, "serverapp", v1, alice)

	cookies := http.Header{"Cookie": {sessionCookie + "=console-secret; theme=dark"}}
	w := h.do(p, alice, http.MethodGet, "/serverapp/hello/there?x=1", "", cookies)
	if w.Code != http.StatusOK {
		t.Fatalf("alice GET = %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{"version=v1 ", "path=/hello/there ", "query=x=1 ", "console-cookie=false "} {
		if !strings.Contains(body, want) {
			t.Fatalf("alice response %q lacks %q", body, want)
		}
	}
	if cc := w.Result().Header.Values("Cache-Control"); len(cc) != 1 || cc[0] != "no-store" {
		t.Fatalf("Cache-Control = %q, want exactly no-store", cc)
	}
	if got := h.do(p, bob, http.MethodGet, "/serverapp/", "", nil).Body.String(); !strings.Contains(got, "version=v0 ") {
		t.Fatalf("bob (no active revision) got %q, want the base server", got)
	}
	if got := h.do(p, "", http.MethodGet, "/serverapp/", "", nil).Body.String(); !strings.Contains(got, "version=v0 ") {
		t.Fatalf("anonymous got %q, want the base server", got)
	}

	// Each principal's server writes only that principal's data directory.
	if w := h.do(p, alice, http.MethodPost, "/serverapp/", "alice-note", nil); w.Code != http.StatusOK {
		t.Fatalf("alice POST = %d: %s", w.Code, w.Body.String())
	}
	if got := h.do(p, bob, http.MethodGet, "/serverapp/", "", nil).Body.String(); strings.Contains(got, "alice-note") {
		t.Fatalf("bob sees alice's data: %q", got)
	}
	st, err := shape.OpenAt(home, "serverapp", alice)
	if err != nil {
		t.Fatal(err)
	}
	if b, err := st.ReadData("note.txt"); err != nil || string(b) != "alice-note" {
		t.Fatalf("alice data = %q, %v; want the note in her app data", b, err)
	}

	if n := h.s.servers.running(); n != 3 {
		t.Fatalf("running servers = %d, want alice, bob and anonymous", n)
	}
	h.s.servers.reap(time.Now().Add(time.Second))
	if n := h.s.servers.running(); n != 0 {
		t.Fatalf("after the idle sweep %d servers still run", n)
	}
	if got := h.do(p, alice, http.MethodGet, "/serverapp/", "", nil).Body.String(); !strings.Contains(got, "version=v1 ") || !strings.Contains(got, "note=alice-note") {
		t.Fatalf("restarted alice server = %q, want v1 with her saved note", got)
	}
}

// TestShapedServerRestoreUndoSwitchesBinary: restore and undo move the
// active revision and the very next request is answered by that revision's
// server, as are history previews.
func TestShapedServerRestoreUndoSwitchesBinary(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	source, _, v1, v2, _ := serverAppSource(t, home)
	p := serverPanel(source)
	h := newServerAppHarness(t, home, p)
	const alice = "device:alice"
	version := func() string {
		t.Helper()
		w := h.do(p, alice, http.MethodGet, "/serverapp/", "", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("GET = %d: %s", w.Code, w.Body.String())
		}
		f := strings.Fields(w.Body.String())
		return strings.TrimPrefix(f[0], "version=")
	}
	activateFor(t, home, "serverapp", v1, alice)
	activateFor(t, home, "serverapp", v2, alice)
	if got := version(); got != "v2" {
		t.Fatalf("after activating v2 served %s", got)
	}
	runner := shapeverify.TrustedRunner{Bin: "/bin/sh", Effects: []string{"read"}}
	if _, err := src.RestoreSealed(context.Background(), home, "serverapp", v1, alice, src.RestoreSealedOptions{Runner: runner}); err != nil {
		t.Fatalf("restore v1: %v", err)
	}
	if got := version(); got != "v1" {
		t.Fatalf("after restore to v1 served %s", got)
	}
	if _, err := src.UndoSealed(context.Background(), home, "serverapp", alice, src.RestoreSealedOptions{Runner: runner}); err != nil {
		t.Fatalf("undo: %v", err)
	}
	if got := version(); got != "v2" {
		t.Fatalf("after undo served %s", got)
	}
}

// TestShapedServerBuildFailure: a revision whose server does not build is a
// 5xx naming the compiler error — never a stale binary, even while another
// principal's server for a good revision is running.
func TestShapedServerBuildFailure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	source, _, _, _, vbroken := serverAppSource(t, home)
	p := serverPanel(source)
	h := newServerAppHarness(t, home, p)
	if got := h.do(p, "device:bob", http.MethodGet, "/serverapp/", "", nil).Body.String(); !strings.Contains(got, "version=v0 ") {
		t.Fatalf("bob got %q", got)
	}
	activateFor(t, home, "serverapp", vbroken, "device:alice")
	for i := 0; i < 2; i++ { // the second answer comes from the remembered failure
		w := h.do(p, "device:alice", http.MethodGet, "/serverapp/", "", nil)
		body := w.Body.String()
		if w.Code < 500 || strings.Contains(body, "version=") {
			t.Fatalf("broken revision = %d %q, want a 5xx and no stale server", w.Code, body)
		}
		if !strings.Contains(body, "build failed") || !strings.Contains(body, "version.go") {
			t.Fatalf("build failure %q does not quote the build error", body)
		}
	}
	if got := h.do(p, "device:bob", http.MethodGet, "/serverapp/", "", nil).Body.String(); !strings.Contains(got, "version=v0 ") {
		t.Fatalf("bob after alice's failed build got %q", got)
	}
}

// TestShapedServerInvalidManifestAndManifestless: a malformed shaped.json is
// a clear 5xx (never a silent fallback to assets), and an app without one —
// the notes fixture — is served from public/ and api units as before.
func TestShapedServerInvalidManifestAndManifestless(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	bad := commitApp(t, "bad", map[string]string{
		"shaped.json":       `{"server": {"args": ["serve"]}}`,
		"public/index.html": "<h1>assets</h1>",
	})
	notes := newNotesSource(t)
	badPanel := Panel{Name: "bad", Label: "Bad", Path: "/bad/", ShapedSource: bad, Auth: AuthSystem, Source: "registered", Available: true}
	notesPanel := Panel{Name: "notes", Label: "Notes", Path: "/notes/", ShapedSource: notes, Auth: AuthSystem, Source: "registered", Available: true}
	h := newServerAppHarness(t, home, badPanel, notesPanel)
	w := h.do(badPanel, "device:alice", http.MethodGet, "/bad/", "", nil)
	if w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), "$ADDR") {
		t.Fatalf("invalid manifest = %d %q, want a 500 naming the problem", w.Code, w.Body.String())
	}
	if w := h.do(notesPanel, "device:alice", http.MethodGet, "/notes/", "", nil); w.Code != 200 || !strings.Contains(w.Body.String(), "<h1>Notes") {
		t.Fatalf("manifest-less app page = %d %.120q", w.Code, w.Body.String())
	}
	if w := h.do(notesPanel, "device:alice", http.MethodGet, "/notes/api/list", "", nil); w.Code != 200 || strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatalf("manifest-less app unit = %d %q", w.Code, w.Body.String())
	}
	if n := h.s.servers.running(); n != 0 {
		t.Fatalf("a manifest-less app started %d servers", n)
	}
}

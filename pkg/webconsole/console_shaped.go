// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/qiangli/yoke/pkg/coopauth"
	"github.com/qiangli/yoke/pkg/fleet"
	shape "github.com/qiangli/yoke/pkg/shape/state"
	"github.com/qiangli/yoke/pkg/src"
)

// The launcher's own presentation is a shaped app (Sprint 526.B1a). Its base
// tree is the embedded artifact/ directory — the index, desktop, board, inbox
// and mb pages with their scripts and styles, laid out as public/ — seeded
// into the managed src checkout every registered shaped app gets, so the
// Shape panel, candidates, verification, activation and version history
// address it by name (`console`) exactly as they address any other app. What
// differs is only where the bytes come from when nobody has shaped anything:
// a principal with no active revision, and every anonymous caller, is served
// the embedded bytes directly, and the checkout is not even opened.
//
// The Go API handlers behind the pages stay Go and are protected by the
// manifest. Three pages are trust surfaces themselves — the shaping review
// page, the terminal, and accounts — and the manifest lists their artifact
// files as protected rows (pkg/src/manifest.go). Those pages are always
// served from the embedded bytes and load their assets from the embedded-only
// `base/` route, and a revision whose tree overrides or drops one of them is
// refused as a whole: the principal is served the base until they activate
// a clean revision. consoleProtectedPaths reads the rows, so a row added
// there is enforced here without a second list.

const (
	// consoleApp is the shaped app name of the launcher's presentation: the
	// src checkout is <home>/src/console, state is <home>/shape/console/.
	consoleApp = "console"
	// consoleBaseMount serves the embedded assets regardless of principal,
	// for the protected pages. Reserved against registered apps.
	consoleBaseMount = "base"
	// consoleArtifactDir is the repo-relative directory the manifest rows
	// name; a row <dir>/<file> protects tree path public/<file>.
	consoleArtifactDir = "pkg/webconsole/artifact"
)

var (
	consoleProtectedOnce sync.Once
	consoleProtectedList []string
	consoleProtectedSet  map[string]bool
)

// consoleProtectedPaths lists the tree-relative paths (public/<name>) the
// protected manifest keeps out of a principal's console revision.
func consoleProtectedPaths() []string {
	consoleProtectedOnce.Do(func() {
		consoleProtectedSet = map[string]bool{}
		for _, e := range src.PackageProtectedParts("yoke", consoleArtifactDir) {
			rel := strings.TrimPrefix(e.Path, consoleArtifactDir+"/")
			if rel == e.Path || rel == "" {
				continue
			}
			p := "public/" + rel
			consoleProtectedList = append(consoleProtectedList, p)
			consoleProtectedSet[p] = true
		}
		sort.Strings(consoleProtectedList)
	})
	return consoleProtectedList
}

// consoleProtectedAsset reports whether the launcher asset name (as served
// at the root: "shape.html", "term.js") is a protected row.
func consoleProtectedAsset(name string) bool {
	consoleProtectedPaths()
	return consoleProtectedSet["public/"+strings.TrimPrefix(name, "/")]
}

// consoleSourceDir is the managed checkout: the one path productionActivator
// accepts for app "console".
func (s *server) consoleSourceDir() (string, error) {
	home, err := s.opts.Shape.home()
	if err != nil {
		return "", err
	}
	return filepath.Join(src.SrcDir(home), consoleApp), nil
}

// consoleShaped is the launcher's presentation as a shapedApp. It is not a
// Panel: it has no mount of its own (its pages ARE the launcher's root) and
// no tile, so it never appears as a device scope or a /meta entry; the Shape
// panel finds it by name (shapedPanel) and the header carries the way in.
func (s *server) consoleShaped() *shapedApp {
	dir, _ := s.consoleSourceDir()
	return &shapedApp{
		name: consoleApp, repo: dir, opts: s.opts.Shape, calls: &s.calls, servers: &s.servers,
		seed: s.seedConsoleSource, protected: consoleProtectedPaths(),
	}
}

// seedConsoleSource makes the managed checkout hold the embedded bytes as
// its base revision — once per process, through the public seam every
// embedded shaped app would use (fleet.SeedShapedSource). It runs lazily,
// on the first request that needs the checkout, so a console nobody has
// shaped never writes under the Bashy home.
func (s *server) seedConsoleSource() error {
	s.consoleSeed.Do(func() {
		dir, err := s.consoleSourceDir()
		if err != nil {
			s.consoleSeedErr = err
			return
		}
		_, s.consoleSeedErr = fleet.SeedShapedSource(dir, "console: base from the embedded launcher ("+BuildOf().Version+")", writeConsoleBase)
	})
	return s.consoleSeedErr
}

// writeConsoleBase lays the embedded artifact/ out as the app's public/.
func writeConsoleBase(root string) error {
	if spaFS == nil {
		return errors.New("console UI missing from this build")
	}
	return os.CopyFS(filepath.Join(root, "public"), spaFS)
}

// consoleRevision resolves the commit the request's principal is served
// from, or nil when the embedded bytes apply: no principal, no recorded
// revision, a checkout that cannot be opened, or every recorded revision
// unresolvable or refused by the protected-path guard.
func (s *server) consoleRevision(r *http.Request) *object.Commit {
	principal, ok := PrincipalFromRequest(r)
	if !ok {
		return nil
	}
	home, err := s.opts.Shape.home()
	if err != nil {
		return nil
	}
	st, err := shape.OpenAt(home, consoleApp, principal)
	if err != nil {
		return nil
	}
	state, err := st.Read()
	if err != nil || (state.ActiveRevision == "" && state.LastKnownGood == "") {
		return nil
	}
	a := s.consoleShaped()
	repo, err := a.open()
	if err != nil {
		slog.Warn("console: shaped source unavailable, serving the embedded launcher", "err", err)
		return nil
	}
	c, err := a.active(repo, principal)
	if err != nil {
		return nil
	}
	if b, berr := base(repo); berr == nil && b.Hash == c.Hash {
		return nil // fell back to base: the embedded bytes, served directly
	}
	return c
}

// consoleAsset returns public/<name> from the principal's active console
// revision, and that revision, or false when the embedded bytes apply — a
// protected page, no revision, or a revision whose tree lacks the file.
func (s *server) consoleAsset(r *http.Request, name string) ([]byte, *object.Commit, bool) {
	name = strings.TrimPrefix(name, "/")
	if name == "" || consoleProtectedAsset(name) {
		return nil, nil, false
	}
	c := s.consoleRevision(r)
	if c == nil {
		return nil, nil, false
	}
	tree, err := c.Tree()
	if err != nil {
		return nil, nil, false
	}
	f, err := tree.File("public/" + name)
	if err != nil {
		return nil, nil, false
	}
	body, err := f.Contents()
	if err != nil {
		return nil, nil, false
	}
	return []byte(body), c, true
}

// handleBaseAsset serves an embedded launcher asset no matter who asks. The
// protected pages reference their scripts and styles here so a principal's
// shaped app.css or app.js can never be what the review page, the terminal
// or the accounts page runs.
func (s *server) handleBaseAsset(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/"+consoleBaseMount+"/")
	if name == "" || strings.HasSuffix(name, ".html") {
		http.NotFound(w, r)
		return
	}
	serveEmbeddedAsset(w, r, name)
}

// baseAssetRef matches a local relative asset reference, with any version
// query already stamped.
var baseAssetRef = regexp.MustCompile(`(src|href)="([A-Za-z0-9_./-]+\.(?:js|css)(?:\?[^"]*)?)"`)

// rebaseAssets points a protected page's local asset references at the
// embedded-only route.
func rebaseAssets(doc []byte) []byte {
	return baseAssetRef.ReplaceAllFunc(doc, func(m []byte) []byte {
		g := baseAssetRef.FindSubmatch(m)
		ref := string(g[2])
		if strings.HasPrefix(ref, "/") || strings.HasPrefix(ref, consoleBaseMount+"/") {
			return m
		}
		return []byte(string(g[1]) + `="` + consoleBaseMount + "/" + ref + `"`)
	})
}

// shapeConsoleButtonHTML is the launcher header's way into shaping itself:
// the Shape panel for the console app. Injected server-side so a revision
// cannot shape the way back out of itself.
func shapeConsoleButtonHTML() string {
	return `<a id="shape-btn" class="iconbtn" href="shape/` + consoleApp + `/" title="Shape this launcher" aria-label="Shape this launcher">` +
		`<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round">` +
		`<path d="M12 3l1.8 4.6L18.4 9l-4.6 1.8L12 15.4l-1.8-4.6L5.6 9l4.6-1.4z"/>` +
		`<path d="M5 17l.9 2.1L8 20l-2.1.9L5 23l-.9-2.1L2 20l2.1-.9z"/>` +
		`</svg></a>`
}

// injectShapeButton places the shape button in the launcher's nav, before
// the theme button; a revision that removed that marker gets it at the end
// of the header bar instead.
func injectShapeButton(doc []byte) []byte {
	if bytes.Contains(doc, []byte(`id="shape-btn"`)) {
		return doc
	}
	marker := []byte(`<button id="theme-btn"`)
	if i := bytes.Index(doc, marker); i >= 0 {
		out := make([]byte, 0, len(doc)+len(shapeConsoleButtonHTML()))
		out = append(out, doc[:i]...)
		out = append(out, shapeConsoleButtonHTML()...)
		out = append(out, doc[i:]...)
		return out
	}
	return insertBeforeCloseOf(doc, `<header id="bar"`, "</header>", shapeConsoleButtonHTML())
}

// consolePreviewPage maps a path under the preview prefix to the launcher
// page the root router would serve for it, so a candidate's tiles lead to
// the candidate's own pages. Anything else is an asset of that name.
func consolePreviewPage(rel string) (string, bool) {
	switch rel {
	case "", "index.html":
		return "index.html", true
	case "sprint/", "sprint/index.html":
		return "board.html", true
	case "mb/", "mb/index.html":
		return "mb.html", true
	case "inbox/", "inbox/index.html":
		return "inbox.html", true
	}
	if _, ws, ok := desktopPeer("/" + rel); ok && !ws {
		return "desktop.html", true
	}
	return rel, false
}

// serveConsolePreview answers one request under /shape/console/preview/<id>/
// from candidate commit c: pages and assets from the candidate's tree with
// <base href> at the preview prefix, protected pages and assets from the
// embedded bytes as always, and api/ routed back through the launcher's own
// gated handler — the Go API is not part of the tree, and the candidate's
// pages are only meaningful against it. prefix is the un-mounted preview
// path; r's URL.Path is already relative to it.
func (s *server) serveConsolePreview(w http.ResponseWriter, r *http.Request, c *object.Commit, prefix string) {
	rel := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if rel != "" && strings.HasSuffix(r.URL.Path, "/") {
		rel += "/"
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Vary", "Cookie")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if strings.HasPrefix(rel, "api/") {
		if s.root == nil {
			http.NotFound(w, r)
			return
		}
		r2 := r.Clone(r.Context())
		r2.URL.Path, r2.URL.RawPath = "/"+rel, ""
		s.root.ServeHTTP(w, r2)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name, isPage := consolePreviewPage(rel)
	if consoleProtectedAsset(name) {
		if isPage {
			s.servePageFile(w, r, name)
		} else if !serveEmbeddedAsset(w, r, name) {
			http.NotFound(w, r)
		}
		return
	}
	tree, err := c.Tree()
	if err != nil {
		http.Error(w, "candidate unreadable", http.StatusInternalServerError)
		return
	}
	f, err := tree.File("public/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	body, err := f.Contents()
	if err != nil {
		http.Error(w, "candidate asset unreadable", http.StatusInternalServerError)
		return
	}
	if !isPage {
		serveRevisionAsset(w, r, name, []byte(body), c)
		return
	}
	href := coopauth.BaseHref(r) + strings.TrimPrefix(prefix, "/") + "/"
	doc := versionAssets(injectChrome(injectBase([]byte(body), href), name, s.openAppsMode()), c.Hash.String()[:12])
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(doc)
}

// protectedVerdicts memoizes guardProtected per commit: a verdict depends
// only on the commit's own tree and its base ancestor, neither of which
// changes once the commit exists.
var protectedVerdicts sync.Map // plumbing.Hash -> protectedVerdict

type protectedVerdict struct{ err error }

// guardProtected refuses a revision whose tree overrides, drops or adds one
// of the app's protected paths relative to the base it was shaped from — the
// nearest first-parent ancestor on the pristine line, so an upgrade that
// moves the base does not retroactively refuse a revision shaped from the
// previous one. Apps without protected paths pass.
func (a *shapedApp) guardProtected(r *gogit.Repository, c *object.Commit) error {
	if len(a.protected) == 0 {
		return nil
	}
	if v, ok := protectedVerdicts.Load(c.Hash); ok {
		return v.(protectedVerdict).err
	}
	baseC, err := shapedBaseOf(r, c)
	if err != nil {
		return fmt.Errorf("shaped app %s: base of %s: %w", a.name, c.Hash.String()[:12], err)
	}
	baseTree, err := baseC.Tree()
	if err != nil {
		return err
	}
	tree, err := c.Tree()
	if err != nil {
		return err
	}
	verdict := protectedDiff(baseTree, tree, a.protected)
	if verdict != nil {
		verdict = fmt.Errorf("revision %s touches protected paths: %w", c.Hash.String()[:12], verdict)
		slog.Warn("shaped app: revision refused", "app", a.name, "err", verdict)
	}
	protectedVerdicts.Store(c.Hash, protectedVerdict{err: verdict})
	return verdict
}

// shapedBaseOf finds the base revision c derives from: c itself or its
// nearest first-parent ancestor that lies on the pristine branch. Without a
// pristine branch the checkout's HEAD stands in; a commit with no base
// ancestor at all is judged against the pristine tip.
func shapedBaseOf(r *gogit.Repository, c *object.Commit) (*object.Commit, error) {
	ref, err := r.Reference(plumbing.NewBranchReferenceName("pristine"), true)
	if err != nil {
		return base(r)
	}
	tip, err := r.CommitObject(ref.Hash())
	if err != nil {
		return nil, err
	}
	onLine := map[plumbing.Hash]bool{}
	iter, err := r.Log(&gogit.LogOptions{From: tip.Hash})
	if err != nil {
		return nil, err
	}
	const cap = 10000
	n := 0
	_ = iter.ForEach(func(x *object.Commit) error {
		onLine[x.Hash] = true
		n++
		if n >= cap {
			return errors.New("stop")
		}
		return nil
	})
	cur := c
	for i := 0; i < cap; i++ {
		if onLine[cur.Hash] {
			return cur, nil
		}
		if cur.NumParents() == 0 {
			break
		}
		p, err := cur.Parent(0)
		if err != nil {
			break
		}
		cur = p
	}
	return tip, nil
}

// protectedDiff compares the protected paths between a base tree and a
// candidate tree. Equal (or absent on both sides) passes; a changed blob is
// an override, a missing one a drop, a new one an addition — all refused,
// named.
func protectedDiff(baseTree, tree *object.Tree, paths []string) error {
	var bad []string
	for _, p := range paths {
		be, berr := baseTree.FindEntry(p)
		ce, cerr := tree.FindEntry(p)
		switch {
		case berr != nil && cerr != nil:
			continue
		case berr == nil && cerr != nil:
			bad = append(bad, p+" (dropped)")
		case berr != nil && cerr == nil:
			bad = append(bad, p+" (added)")
		case be.Hash != ce.Hash:
			bad = append(bad, p+" (overridden)")
		}
	}
	if len(bad) == 0 {
		return nil
	}
	return errors.New(strings.Join(bad, ", "))
}

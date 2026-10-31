// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"bytes"
	"context"
	"html"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/qiangli/yoke/pkg/coopauth"
	"github.com/qiangli/yoke/pkg/shape/appserver"
)

// The common Genie button (Sprint 526.B1g). The sparkle used to live in the
// launcher header only; the desk apps had no in-app way to start a wish. Now
// ONE component — artifact/genie.js, plain JS carrying its own CSS — is
// injected server-side into every shaped app page the console serves: the
// launcher's own pages (servePageFile), every registered shaped app's pages
// (serveTree for static trees, the reverse proxy for declared servers), and
// candidate previews of both. Apps need no change, and the same seam will
// serve proxied external apps later.
//
// The component is shaping PLUMBING, with the Shape page: the manifest
// (pkg/src) lists artifact/genie.js as a protected row, so a console
// revision that overrides or drops it is refused as a whole, and the script
// is served from the embedded bytes alone at the fixed route /_shape/genie.js
// — never from a principal's tree, never from a candidate. What a wish MAY
// move is its placement: shaped.json's genie key (position, anchor, hidden)
// is shaped source read here and handed to the component on its own tag, so
// "move the button to the bottom left" is a config-only candidate.
//
// Three modes, decided by the server from what it is serving: live (the
// wish builds a candidate), protected (the page is a trust surface — the
// Shape page, sign-in and pairing, the terminal, accounts — so the wish is
// filed as a request and no candidate is built), and preview (a candidate
// is being previewed; the button is disabled).
//
// In preview mode the same component is the IN-PLACE REVIEW (operator
// 2026-10-31, docs/sprint-526/document-model-shaping-2026-10-31.md): the tag
// names the candidate (data-candidate) and the app's live mount
// (data-mount), and the component pins a review bar on top of the page —
// the wish, "Reviewing candidate <id>", what changed in plain words (the
// agent's coverage list + the changed files), Approve (the server re-runs
// the trusted verifier and activates only on pass; a refusal is shown with
// its reason), Reject, Back — outlines the elements that differ from the
// live page (best effort), flips before/after, and keeps the code diff one
// click away. The pages themselves keep every link inside the preview
// prefix (base href / relative links), and a protected page reached from a
// review says so: the component remembers the review in the tab's session
// storage, which is how the live page knows (F4: a preview had no banner).

const (
	// genieAsset is the embedded component file.
	genieAsset = "genie.js"
	// genieMount is the fixed route the component is served at, under the
	// launcher's root, from the embedded bytes whoever asks.
	genieMount = "_shape"

	genieLive      = "live"
	genieProtected = "protected"
	geniePreview   = "preview"

	// genieConsoleAnchor is where the button goes on the launcher's own
	// pages when nobody has placed it: inside the header bar, where the
	// sparkle has always been. Every launcher page carries <header id="bar">.
	genieConsoleAnchor = "#bar #nav, #bar"

	// geniePageLimit bounds the page context carried on the tag.
	geniePageLimit = 256
)

// genieTag is everything the component is told, written as data attributes
// on its own <script> tag.
type genieTag struct {
	Base     string // the launcher's base href, trailing slash
	App      string
	Page     string // the page/route, relative to the app, with its query
	Mode     string
	Position string
	Anchor   string
	Hidden   bool
	Landing  bool
	// Candidate is the candidate being previewed (preview mode only): the
	// component reviews it in place.
	Candidate string
	// Mount is the app's live mount relative to Base, trailing slash; ""
	// for the launcher itself, whose pages sit at Base. The component builds
	// the live counterpart of a previewed page from it (before/after).
	Mount string
}

// html renders the script tag. Every value is attribute-escaped; the src is
// absolute under the launcher's mount so the tag is the same whether the
// page sits at the root, under an app mount, or under a preview prefix, and
// it carries the content version so a cached old component never outlives
// a fix (see versionAssets).
func (g genieTag) html() string {
	attr := func(k, v string) string { return ` data-` + k + `="` + html.EscapeString(v) + `"` }
	flag := func(b bool) string {
		if b {
			return "1"
		}
		return "0"
	}
	src := g.Base + genieMount + "/" + genieAsset + "?v=" + strings.Trim(etagFor(genieAsset), `"`)
	return `<script src="` + html.EscapeString(src) + `" data-bashy-genie` +
		attr("base", g.Base) + attr("app", g.App) + attr("page", g.Page) + attr("mode", g.Mode) +
		attr("position", g.Position) + attr("anchor", g.Anchor) +
		attr("hidden", flag(g.Hidden)) + attr("landing", flag(g.Landing)) +
		attr("candidate", g.Candidate) + attr("mount", g.Mount) +
		` defer></script>` + "\n"
}

// injectGenie places the component tag at the end of the page body. A page
// that already carries one (a double pass) is left alone; a page without a
// body close tag gets it appended, which a browser still runs.
func injectGenie(doc []byte, g genieTag) []byte {
	if g.App == "" || bytes.Contains(doc, []byte("data-bashy-genie")) {
		return doc
	}
	tag := g.html()
	if bytes.LastIndex(doc, []byte("</body>")) < 0 {
		return append(append(make([]byte, 0, len(doc)+len(tag)), doc...), tag...)
	}
	return insertBeforeLast(doc, "</body>", tag)
}

// geniePage is the page context for a request: the path relative to the
// app plus its query, bounded.
func geniePage(path, query string) string {
	if path == "" || path[0] != '/' {
		path = "/" + path
	}
	p := path
	if query != "" {
		p += "?" + query
	}
	if len(p) > geniePageLimit {
		p = p[:geniePageLimit]
	}
	return p
}

// genieLanding reports whether page is the app's landing page — where the
// button stays available even when the app hides it elsewhere.
func genieLanding(page string) bool {
	p := page
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i]
	}
	return p == "/" || p == "/index.html" || p == ""
}

// genieConfigOf reads the app's placement from shaped.json in tree. No
// manifest, no genie key, or an unreadable one all mean the defaults. ok
// reports whether the app placed the button itself.
func genieConfigOf(tree *object.Tree) (cfg appserver.Genie, ok bool) {
	cfg = appserver.Genie{Position: appserver.GenieDefaultPosition}
	if tree == nil {
		return cfg, false
	}
	f, err := tree.File(appserver.File)
	if err != nil {
		return cfg, false
	}
	body, err := f.Contents()
	if err != nil {
		return cfg, false
	}
	m, err := appserver.Parse([]byte(body))
	if err != nil || m.Genie == nil {
		return cfg, false
	}
	return *m.Genie, true
}

// genieTagFor builds the tag for one page of app from the tree the page is
// served from. mount is the app's live mount relative to the launcher base
// ("" for the launcher). mode is live unless the request is a preview, in
// which case the tag also names the candidate; a caller serving a protected
// page overrides the mode.
func genieTagFor(r *http.Request, app, mount string, tree *object.Tree, page, defaultAnchor string) genieTag {
	cfg, placed := genieConfigOf(tree)
	anchor := cfg.Anchor
	if !placed {
		anchor = defaultAnchor
	}
	mode, candidate := genieLive, ""
	if id, ok := previewOf(r); ok {
		mode, candidate = geniePreview, id
	}
	return genieTag{
		Base: launcherBase(r), App: app, Page: page, Mode: mode,
		Position: cfg.Position, Anchor: anchor, Hidden: cfg.Hidden, Landing: genieLanding(page),
		Candidate: candidate, Mount: mount,
	}
}

// protectedGenieTag is the tag for a Go-rendered trust page (sign-in,
// pairing): the console app, protected mode, placed in the corner — those
// pages have no header bar and no shaped revision.
func (s *server) protectedGenieTag(r *http.Request, page string) genieTag {
	return genieTag{Base: launcherBase(r), App: consoleApp, Page: page, Mode: genieProtected,
		Position: appserver.GenieDefaultPosition, Landing: genieLanding(page)}
}

// ---- request context -------------------------------------------------------

type launcherBaseKey struct{}
type previewKey struct{}

// rememberLauncherBase records the launcher's own base href before any
// mount strips a prefix: a shaped app page under /notepad/ (and, through the
// tunnel, under /matrix/h/<host>/app/<name>/notepad/) must address the
// component and the shaping API at the LAUNCHER's root, not its own.
func rememberLauncherBase(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), launcherBaseKey{}, coopauth.BaseHref(r))))
	})
}

// launcherBase is the launcher's base href for this request: the one
// remembered at the root, or (for a handler reached without the console's
// front door, as in tests) the request's own.
func launcherBase(r *http.Request) string {
	if b, ok := r.Context().Value(launcherBaseKey{}).(string); ok && b != "" {
		return b
	}
	return coopauth.BaseHref(r)
}

// withPreview marks a request as serving a preview of candidate id; the
// component then reviews that candidate in place instead of taking wishes.
func withPreview(r *http.Request, id string) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), previewKey{}, id))
}

// previewOf reports the candidate a preview request serves.
func previewOf(r *http.Request) (string, bool) {
	id, ok := r.Context().Value(previewKey{}).(string)
	return id, ok && id != ""
}

// ---- the asset route ---------------------------------------------------------

// handleGenieAsset serves the component from the embedded bytes, whoever
// asks and whatever they have shaped: the one file under /_shape/.
func handleGenieAsset(w http.ResponseWriter, r *http.Request) {
	if r.PathValue("name") != genieAsset {
		http.NotFound(w, r)
		return
	}
	if !serveEmbeddedAsset(w, r, genieAsset) {
		http.Error(w, "console UI missing from this build", http.StatusInternalServerError)
	}
}

// ---- proxied pages -----------------------------------------------------------

// genieProxyBodyLimit bounds the HTML page a proxied server's response is
// buffered for; a larger page is passed through untouched.
const genieProxyBodyLimit = 4 << 20

// injectGenieResponse rewrites a proxied HTML page in place so the
// component rides on a declared server's pages exactly as on a static
// tree's. Only a complete, identity-encoded HTML success response is
// touched; anything else passes through byte for byte.
func injectGenieResponse(resp *http.Response, tag func(page string) genieTag) error {
	if resp.StatusCode != http.StatusOK || resp.Request == nil || resp.Request.Method == http.MethodHead {
		return nil
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(ct)), "text/html") {
		return nil
	}
	if enc := resp.Header.Get("Content-Encoding"); enc != "" && !strings.EqualFold(enc, "identity") {
		return nil
	}
	if resp.ContentLength > genieProxyBodyLimit {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, genieProxyBodyLimit+1))
	_ = resp.Body.Close()
	if err != nil {
		return err
	}
	if len(body) > genieProxyBodyLimit {
		resp.Body = io.NopCloser(bytes.NewReader(body))
		return nil
	}
	out := injectGenie(body, tag(geniePage(resp.Request.URL.Path, resp.Request.URL.RawQuery)))
	resp.Body = io.NopCloser(bytes.NewReader(out))
	resp.ContentLength = int64(len(out))
	resp.Header.Set("Content-Length", strconv.Itoa(len(out)))
	return nil
}

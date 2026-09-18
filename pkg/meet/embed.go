package meet

import (
	"bytes"
	"fmt"
	"html"
	"io/fs"
	"net/http"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/coopauth"
)

// Serving the room's web UI.
//
// The SPA is a single-page app: the browser fetches index.html once, and every
// subsequent "page" is a client-side route that the server has never heard of.
// Two things follow, and both are load-bearing rather than conventional.
//
// # The trailing slash in <base href>
//
// Through Tessaro this app is mounted at a PREFIX — /matrix/h/<host>/app/meet —
// and outpost rewrites Location headers but not HTML. So the document must say
// where it lives, with `<base href="<prefix>/">`, and the trailing slash is the
// entire mechanism: a browser resolves a relative URL against everything in the
// base up to the LAST slash. With `href="/app/meet"` the base directory is `/app`
// and `src="assets/x.js"` fetches /app/assets/x.js — outside the mount, 404, a
// blank page. With `href="/app/meet/"` it fetches /app/meet/assets/x.js.
//
// It is injected at SERVE time, not baked into the build, because the same binary
// is reached at `/` on loopback and under a prefix through the tunnel, and only
// the request knows which.
//
// # The SPA is optional at build time
//
// pkg/meet/web/ is built by a separate track (Vite/React/Tailwind, issue #156)
// and its dist/ is build output. A bare `go:embed web/dist` would make this
// package refuse to compile until that directory exists, which would hold the
// whole repo hostage to a frontend build.
//
// So the embed lives behind the `meetspa` build tag (embed_spa.go) and spaFS is
// nil without it. The API and /observe work either way — only `/` changes, and it
// says so plainly instead of 404-ing. Build the shipping binary with
// `go build -tags meetspa`.

// spaFS is the built SPA, or nil in a binary compiled without it. Assigned by
// embed_spa.go under the `meetspa` build tag.
var spaFS fs.FS

// spaIndex is the SPA entrypoint, relative to spaFS.
const spaIndex = "index.html"

// baseTag matches an existing <base …> element, so a build that emitted its own
// placeholder is REPLACED rather than shadowed. Two base elements are not an
// error in HTML — the first wins — so appending ours would silently do nothing.
var baseTag = regexp.MustCompile(`(?is)<base\b[^>]*>`)

// headOpen matches the opening <head>, which is where a base element must go: it
// only governs URLs that appear after it in the document.
var headOpen = regexp.MustCompile(`(?is)<head\b[^>]*>`)

// handleSPA serves the app and its assets, with history fallback.
func handleSPA(w http.ResponseWriter, r *http.Request) {
	if spaFS == nil {
		spaMissing(w, r)
		return
	}
	name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if name == "" || name == "." || name == spaIndex {
		serveSPAIndex(w, r)
		return
	}

	f, err := spaFS.Open(name)
	if err != nil {
		// History fallback, but only for things that could BE a route. A request
		// with a file extension is an asset the build did not produce, and
		// answering it with index.html would hand the browser HTML where it asked
		// for JavaScript — which fails later, further away, and with a parse error
		// instead of a 404.
		if path.Ext(name) != "" {
			http.NotFound(w, r)
			return
		}
		serveSPAIndex(w, r)
		return
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil || st.IsDir() {
		serveSPAIndex(w, r)
		return
	}
	rs, ok := f.(interface {
		Read([]byte) (int, error)
		Seek(int64, int) (int64, error)
	})
	if !ok {
		http.Error(w, "meet: unreadable asset", http.StatusInternalServerError)
		return
	}
	http.ServeContent(w, r, name, st.ModTime(), rs)
}

// serveSPAIndex sends index.html with this request's <base href> injected.
//
// Never cached: the body is request-dependent (the same binary serves `/` on
// loopback and a prefixed mount through the tunnel), so a cached copy would send
// one caller the other's base and break every relative URL on the page.
func serveSPAIndex(w http.ResponseWriter, r *http.Request) {
	b, err := fs.ReadFile(spaFS, spaIndex)
	if err != nil {
		spaMissing(w, r)
		return
	}
	body := injectBase(b, coopauth.BaseHref(r))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, spaIndex, time.Time{}, bytes.NewReader(body))
}

// injectBase rewrites (or inserts) the document's <base href>. href is expected
// to be trailing-slashed — coopauth.BaseHref guarantees it.
func injectBase(doc []byte, href string) []byte {
	tag := []byte(fmt.Sprintf(`<base href="%s">`, html.EscapeString(href)))
	if baseTag.Match(doc) {
		return baseTag.ReplaceAll(doc, tag)
	}
	if loc := headOpen.FindIndex(doc); loc != nil {
		out := make([]byte, 0, len(doc)+len(tag))
		out = append(out, doc[:loc[1]]...)
		out = append(out, tag...)
		return append(out, doc[loc[1]:]...)
	}
	// No <head> to put it in. Prepending is still correct — a base element takes
	// effect for everything after it — and a document this malformed is a build
	// bug we should not silently paper over by serving it unchanged.
	return append(tag, doc...)
}

// spaMissing explains a binary built without the web UI, rather than 404-ing.
//
// A blank 404 on `/` reads as "the server is broken". It is not: the API and the
// WebSocket are fully functional, and the operator needs to know that the missing
// piece is a build flag, not a fault.
func spaMissing(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	fmt.Fprint(w, "meet: this binary was built without the web room.\n\n"+
		"The API and the live stream are running:\n"+
		"  GET  /api/rooms\n"+
		"  GET  /api/agents\n"+
		"  GET  /observe?room=<ROOM|id>   (WebSocket)\n\n"+
		"The UI is normally compiled in. If it is missing, pkg/meet/artifact was\n"+
		"empty at build time — rebuild the SPA and promote it:\n"+
		"  scripts/build-meet-spa.sh\n")
}

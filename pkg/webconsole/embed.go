// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io"
	"io/fs"
	"net/http"
	"regexp"
	"strings"
	"sync"

	"github.com/qiangli/yoke/pkg/coopauth"
)

// The start page is embedded UNCONDITIONALLY — no build tag.
//
// pkg/meet keeps its SPA behind -tags meetspa and leaves web/dist untracked;
// the console cannot, because the console is meant to be ALWAYS available, and
// with no tag to hide behind an unbuilt bundle would be a COMPILE error for
// everyone rather than a missing page for the few.
//
// So the embedded bytes live in artifact/, which is TRACKED, while the SPA's own
// build output (web/dist/) stays ignored like meet's. The two are deliberately
// different directories: dist/ is scratch that every local build churns, and
// artifact/ is "these bytes ship" — promoted into the tree by the build script
// as a reviewable act rather than as a side effect of whoever last ran vite.
//
// The cost of tracking is the mirror image of the build break it prevents: a
// stale bundle ships silently. That is what the release verification gate exists
// to catch.
//
//go:embed all:artifact
var spaEmbed embed.FS

var spaFS fs.FS

func init() {
	if sub, err := fs.Sub(spaEmbed, "artifact"); err == nil {
		spaFS = sub
	}
}

// assetRef matches a local relative src=/href= in the page markup.
var assetRef = regexp.MustCompile(`(src|href)="([A-Za-z0-9_./-]+\.(?:js|css))"`)

// versionAssets stamps every local asset reference with a content version.
//
// The ETag alone is not enough to rescue a browser that ALREADY cached an asset
// back when the server sent no validator at all: with nothing to revalidate
// against it may not even ask, and it keeps running the old script past any
// number of reloads. The HTML itself is no-store, so a version in the query
// string is fetched fresh every time and pins the exact bytes the page expects.
//
// This is why a fixed bug can keep reproducing on the reporter's machine while
// every server-side check says the fix shipped.
func versionAssets(doc []byte) []byte {
	return assetRef.ReplaceAllFunc(doc, func(m []byte) []byte {
		g := assetRef.FindSubmatch(m)
		name := string(g[2])
		tag := strings.Trim(etagFor(name), `"`)
		if tag == "0" {
			return m // not an embedded asset; leave it alone
		}
		return []byte(string(g[1]) + `="` + name + `?v=` + tag + `"`)
	})
}

// etagFor returns a strong ETag over an embedded file's bytes, computed once.
//
// Keyed on content rather than a build stamp so a rebuild that does not change
// an asset still answers 304, and any change to it always busts the cache.
func etagFor(name string) string {
	etagOnce.Do(func() { etags = map[string]string{} })
	etagMu.Lock()
	defer etagMu.Unlock()
	if tag, ok := etags[name]; ok {
		return tag
	}
	tag := `"0"`
	if b, err := fs.ReadFile(spaFS, name); err == nil {
		sum := sha256.Sum256(b)
		tag = `"` + hex.EncodeToString(sum[:8]) + `"`
	}
	etags[name] = tag
	return tag
}

var (
	etagOnce sync.Once
	etagMu   sync.Mutex
	etags    map[string]string
)

// handleTermPage serves the standalone terminal page.
//
// It is served from the LAUNCHER's root rather than from under the /term mount
// so its <base href> is the launcher's: one place holds the vendored xterm
// bundle, and the page can address the socket as "term/ws" and the launcher as
// "./" without knowing how deep it is mounted.
func (s *server) handleTermPage(w http.ResponseWriter, r *http.Request) {
	s.servePageFile(w, r, "term.html")
}

// handleSPA serves the start page, falling back to index.html so client-side
// routes survive a reload.
func (s *server) handleSPA(w http.ResponseWriter, r *http.Request) {
	if spaFS == nil {
		http.Error(w, "console UI missing from this build", http.StatusInternalServerError)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/")
	if name != "" {
		if f, err := spaFS.Open(name); err == nil {
			defer f.Close()
			if st, err := f.Stat(); err == nil && !st.IsDir() {
				// Embedded files carry a ZERO modtime, so without an explicit
				// validator a browser has nothing to revalidate against and
				// applies heuristic caching — it keeps serving the previous
				// build's script forever. That is not a cosmetic staleness: the
				// launcher shipped a version whose tiles opened apps in an
				// iframe, and after the fix the old script kept running from
				// cache, so the bug looked unfixed.
				//
				// no-cache means "revalidate every time", not "do not store":
				// with the content ETag below an unchanged asset costs a 304.
				w.Header().Set("ETag", etagFor(name))
				w.Header().Set("Cache-Control", "no-cache")
				http.ServeFileFS(w, r, spaFS, name)
				return
			}
		}
	}
	s.servePageFile(w, r, "index.html")
}

// servePageFile writes an embedded HTML page with <base href> injected for the
// CURRENT mount.
//
// The href is computed per request, not at build time, because the same binary
// is reached at / on loopback and at /matrix/h/<host>/app/console/ through the
// tunnel — and, when the console mounts a panel, one level deeper again. The
// trailing slash is the entire mechanism: a page that builds every URL as
// new URL(x, document.baseURI) then needs no per-mount configuration.
func (s *server) servePageFile(w http.ResponseWriter, r *http.Request, name string) {
	if spaFS == nil {
		http.Error(w, "console UI missing from this build", http.StatusInternalServerError)
		return
	}
	f, err := spaFS.Open(name)
	if err != nil {
		http.Error(w, "console UI missing from this build", http.StatusInternalServerError)
		return
	}
	defer f.Close()
	doc, err := io.ReadAll(f)
	if err != nil {
		http.Error(w, "console UI unreadable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(versionAssets(injectChrome(injectBase(doc, coopauth.BaseHref(r)), name, s.openAppsMode())))
}

// injectChrome adds the console's shared chrome — the copyright footer on
// every page, plus the header button back to the launcher on every standalone
// managed-app page (term.html, board.html, mb.html) when apps open in the
// SAME tab — server-side, once, here.
//
// The mode is the persisted global "Open apps" setting, and the two halves of
// the contract hold together: in same-tab mode an app REPLACES the launcher,
// so the app needs an in-page way back (the button); in new-tab mode the
// launcher is still sitting in the tab behind the app, so the button would
// duplicate the browser's own affordance and is omitted. The footer is
// mode-independent — every page states the same copyright.
//
// All of this exists so the markup is not hand-copied into every embedded
// HTML file: index.html already owns a richer footer with live build/session
// detail (rendered client-side by app.js), so this only appends the copyright
// line to it; a managed app has no such footer at all, so this builds the
// whole thing. Either way the text and the button are defined in exactly one
// place.
func injectChrome(doc []byte, name, openApps string) []byte {
	if name == "index.html" {
		// index.html's own <footer id="foot"> already carries the version
		// detail (#foot-left / #foot-build / #foot-right) — leave it alone
		// and only append the copyright line inside it.
		return insertBeforeLast(doc, "</footer>", chromeCopyrightHTML())
	}
	// A managed app carries no version detail — the mark next to the wordmark
	// and the build line in the footer are the launcher's own, not repeated
	// here — only the copyright line, in a footer of its own.
	if openApps != OpenNewTab {
		// Same-tab mode (the default, and the fail-safe for any unknown
		// value): the app replaced the launcher, so the return control is
		// part of the page itself.
		//
		// It goes in the CONSOLE BAR, which is not necessarily the page's last
		// <header>: the inbox has a second one over its message list, so
		// appending before the final </header> filed the return control inside
		// the content area — a per-page position for a control that is the same
		// control everywhere.
		doc = insertBeforeCloseOf(doc, `<header id="bar"`, "</header>", allAppsButtonHTML())
	}
	doc = insertBeforeLast(doc, "</body>", `<footer id="app-foot">`+chromeCopyrightHTML()+`</footer>`+"\n")
	return doc
}

// chromeCopyrightHTML is the one statement of the copyright line, shared by
// the launcher and every managed app so it reads identically everywhere.
//
// The year is intentionally fixed by the product copy, not derived from the
// server clock. Static, server-owned text only; nothing a client can influence
// reaches this string.
func chromeCopyrightHTML() string {
	return `<span id="copyright">&copy; 2026 qiangli. All rights reserved.</span>`
}

// allAppsButtonHTML is the header button every managed app carries to return
// to the central launcher in same-tab mode. It is appended last inside
// <header id="bar">, so it lands rightmost after whatever app-specific header
// content precedes it — the same #bar/.iconbtn/.spacer layout language the
// launcher's own header buttons use, and the same relative-URL ("./" against
// <base href>) routing the brand mark and every other in-console link already
// relies on, which is what keeps it correct under a route prefix.
func allAppsButtonHTML() string {
	return `<a id="all-apps-btn" class="iconbtn" href="./" title="All apps" aria-label="Back to all apps">` +
		`<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round">` +
		`<rect x="3" y="3" width="7" height="7" rx="1.5"/><rect x="14" y="3" width="7" height="7" rx="1.5"/>` +
		`<rect x="3" y="14" width="7" height="7" rx="1.5"/><rect x="14" y="14" width="7" height="7" rx="1.5"/>` +
		`</svg></a>`
}

// insertBeforeCloseOf inserts text immediately before the close tag that ends
// the element opened by open — the FIRST close after it, which is the right one
// because the console's bar never nests another element of the same kind.
//
// A page with no such element falls back to the last close tag, which is what
// this did before there was a page with two of them.
func insertBeforeCloseOf(doc []byte, openTag, closeTag, insert string) []byte {
	i := bytes.Index(doc, []byte(openTag))
	if i < 0 {
		return insertBeforeLast(doc, closeTag, insert)
	}
	j := bytes.Index(doc[i:], []byte(closeTag))
	if j < 0 {
		return doc
	}
	at := i + j
	out := make([]byte, 0, len(doc)+len(insert))
	out = append(out, doc[:at]...)
	out = append(out, insert...)
	out = append(out, doc[at:]...)
	return out
}

// insertBeforeLast inserts text immediately before the last occurrence of
// marker, or returns doc unchanged if marker is absent — a page missing its
// own closing tag is a bug this should surface as a broken page, not a panic.
func insertBeforeLast(doc []byte, marker, insert string) []byte {
	i := bytes.LastIndex(doc, []byte(marker))
	if i < 0 {
		return doc
	}
	out := make([]byte, 0, len(doc)+len(insert))
	out = append(out, doc[:i]...)
	out = append(out, insert...)
	out = append(out, doc[i:]...)
	return out
}

// injectBase rewrites (or inserts) the document's <base href>.
func injectBase(doc []byte, href string) []byte {
	if i := bytes.Index(doc, []byte("<base href=\"")); i >= 0 {
		start := i + len("<base href=\"")
		if end := bytes.IndexByte(doc[start:], '"'); end >= 0 {
			out := make([]byte, 0, len(doc)+len(href))
			out = append(out, doc[:start]...)
			out = append(out, href...)
			out = append(out, doc[start+end:]...)
			return out
		}
	}
	if i := bytes.Index(doc, []byte("<head>")); i >= 0 {
		at := i + len("<head>")
		tag := "\n    <base href=\"" + href + "\">"
		out := make([]byte, 0, len(doc)+len(tag))
		out = append(out, doc[:at]...)
		out = append(out, tag...)
		out = append(out, doc[at:]...)
		return out
	}
	return doc
}

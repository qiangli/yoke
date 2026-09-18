// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// ---- helpers -------------------------------------------------------------

// lookPut issues a PUT /api/look the way the launcher's dialog does, and
// returns the response recorder.
func lookPut(t *testing.T, h http.Handler, v string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"open_apps":` + strconv.Quote(v) + `}`
	r := httptest.NewRequest("PUT", "/api/look", strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:5555"
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// lookGet returns the decoded GET /api/look body.
func lookGet(t *testing.T, h http.Handler) map[string]any {
	t.Helper()
	w := do(h, "GET", "/api/look", "127.0.0.1:5555", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/look = %d (body %q)", w.Code, w.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("GET /api/look body: %v", err)
	}
	return m
}

// assertCopyright pins the one copyright line, identically on every page: the
// fixed copyright year and rights reservation, on its own centered line.
func assertCopyright(t *testing.T, where, foot string) {
	t.Helper()
	want := regexp.MustCompile(`<span id="copyright">&copy; 2026 qiangli\. All rights reserved\.</span>`)
	if !want.MatchString(foot) {
		t.Errorf("%s: copyright line missing or malformed, footer = %q", where, foot)
	}
}

// ---- footer content ------------------------------------------------------

// The central launcher must keep its existing version-detail footer content
// and additionally state the polished copyright line, inside the SAME footer.
func TestLauncherFooterKeepsVersionAndGainsCopyright(t *testing.T) {
	h := newTestHandler(t, Options{})
	w := do(h, "GET", "/", "127.0.0.1:5555", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET / = %d, want 200", w.Code)
	}
	body := w.Body.String()

	footStart := strings.Index(body, `<footer id="foot">`)
	footEnd := strings.Index(body, "</footer>")
	if footStart < 0 || footEnd < 0 || footEnd < footStart {
		t.Fatalf("index.html has no <footer id=%q>...</footer> block", "foot")
	}
	foot := body[footStart:footEnd]

	for _, want := range []string{`id="foot-left"`, `id="foot-build"`, `id="foot-right"`} {
		if !strings.Contains(foot, want) {
			t.Errorf("launcher footer lost existing version detail %q", want)
		}
	}
	assertCopyright(t, "launcher", foot)
	// index.html is the central page and must not grow the managed-app "back
	// to apps" button — it would point at itself.
	if strings.Contains(body, `id="all-apps-btn"`) {
		t.Errorf("launcher page grew an all-apps button; it IS the apps page")
	}
}

func TestLauncherConstrainsPhoneViewport(t *testing.T) {
	h := newTestHandler(t, Options{})
	w := do(h, "GET", "/app.css", "127.0.0.1:5555", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("app.css = %d, want 200", w.Code)
	}
	css := w.Body.String()
	for _, want := range []string{
		"@media (max-width: 40rem)",
		"overflow-x: hidden",
		"#who, #ver { display: none; }",
		"#foot code { white-space: normal; overflow-wrap: anywhere; word-break: break-word; }",
		"max-height: calc(100svh - 1rem)",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("phone layout is missing %q", want)
		}
	}
}

// Every managed app must carry the same copyright footer, but never the
// launcher's own version/build detail — that identity belongs to the
// launcher, not to an app served alongside it. The footer is mode-independent.
func TestManagedAppFooterHasCopyrightNotVersion(t *testing.T) {
	for _, mode := range []string{OpenSameTab, OpenNewTab} {
		t.Run(mode, func(t *testing.T) {
			h := newTestHandler(t, Options{LookStorePath: filepath.Join(t.TempDir(), "ui.json")})
			if w := lookPut(t, h, mode); w.Code != http.StatusOK {
				t.Fatalf("PUT /api/look = %d, want 200", w.Code)
			}
			for _, page := range []string{"/term/", "/sprint/", "/mb/"} {
				w := do(h, "GET", page, "127.0.0.1:5555", nil)
				if w.Code != http.StatusOK {
					t.Fatalf("GET %s = %d, want 200", page, w.Code)
				}
				body := w.Body.String()

				footStart := strings.Index(body, `<footer id="app-foot">`)
				footEnd := strings.Index(body[footStart:], "</footer>")
				if footStart < 0 || footEnd < 0 {
					t.Fatalf("%s has no shared app footer", page)
				}
				assertCopyright(t, page, body[footStart:footStart+footEnd])
				for _, versionMarker := range []string{`id="ver"`, `id="foot-build"`, `id="foot-left"`} {
					if strings.Contains(body, versionMarker) {
						t.Errorf("%s footer carries launcher version detail %q, which it must omit", page, versionMarker)
					}
				}
			}
		})
	}
}

// ---- logo roots ----------------------------------------------------------

// Each managed app's logo returns to (or stays on) ITS OWN root, not the
// central launcher — a separate, mode-aware control now owns that route.
func TestManagedAppLogoLinksToItsOwnRoot(t *testing.T) {
	h := newTestHandler(t, Options{})
	cases := []struct {
		page, wantHref, wantTitleContains string
	}{
		{"/term/", `href="term/"`, "terminal"},
		{"/sprint/", `href="sprint/"`, "sprint"},
		{"/mb/", `href="mb/"`, "messages"},
	}
	for _, c := range cases {
		t.Run(c.page, func(t *testing.T) {
			w := do(h, "GET", c.page, "127.0.0.1:5555", nil)
			body := w.Body.String()
			brandStart := strings.Index(body, `<a id="brand"`)
			if brandStart < 0 {
				t.Fatalf("%s: no <a id=\"brand\"> element", c.page)
			}
			brandEnd := strings.Index(body[brandStart:], ">")
			brandTag := body[brandStart : brandStart+brandEnd]
			if !strings.Contains(brandTag, c.wantHref) {
				t.Errorf("%s: brand link = %q, want href %q (its own root, not the launcher)", c.page, brandTag, c.wantHref)
			}
			if !strings.Contains(strings.ToLower(brandTag), c.wantTitleContains) {
				t.Errorf("%s: brand title %q does not name the app itself", c.page, brandTag)
			}
		})
	}

	// The launcher's own brand legitimately points at itself with "./".
	w := do(h, "GET", "/", "127.0.0.1:5555", nil)
	if !strings.Contains(w.Body.String(), `<a id="brand" href="./" title="bashy apps">`) {
		t.Errorf("launcher brand link changed unexpectedly")
	}
}

// ---- conditional return control -------------------------------------------

// In same-tab mode (the default) every managed app carries ONE rightmost,
// accessible header button back to the launcher.
func TestSameTabDefaultShowsRightmostReturnControl(t *testing.T) {
	h := newTestHandler(t, Options{}) // no store file: pure default
	assertReturnControl(t, h, true)
}

// In new-tab mode the control is omitted everywhere — the launcher is still
// in the tab behind the app, so the button would duplicate the browser.
func TestNewTabModeOmitsReturnControl(t *testing.T) {
	h := newTestHandler(t, Options{LookStorePath: filepath.Join(t.TempDir(), "ui.json")})
	if w := lookPut(t, h, OpenNewTab); w.Code != http.StatusOK {
		t.Fatalf("PUT /api/look = %d, want 200", w.Code)
	}
	for _, page := range []string{"/term/", "/sprint/", "/mb/", "/inbox/"} {
		w := do(h, "GET", page, "127.0.0.1:5555", nil)
		if strings.Contains(w.Body.String(), `id="all-apps-btn"`) {
			t.Errorf("%s: return control present in new-tab mode; it must be omitted", page)
		}
	}
	// The launcher never carries it in either mode.
	if strings.Contains(do(h, "GET", "/", "127.0.0.1:5555", nil).Body.String(), `id="all-apps-btn"`) {
		t.Errorf("launcher grew a return control in new-tab mode")
	}
}

// assertReturnControl checks presence, rightmost placement inside the header,
// the shared .iconbtn look, the relative "./" route, and the accessible name.
func assertReturnControl(t *testing.T, h http.Handler, want bool) {
	t.Helper()
	// Every standalone managed page, INCLUDING the inbox — which is the one
	// page with a second <header> (over its message list). It was left out of
	// this map, and the return control was duly injected into that inner header
	// instead of the console bar, where it read as an inbox control rather than
	// the console's.
	appSpecificMarker := map[string]string{
		"/term/":   `id="status"`,
		"/sprint/": `id="bd-age"`,
		"/mb/":     `id="mb-who"`,
		"/inbox/":  `id="ib-who"`,
	}
	for page, marker := range appSpecificMarker {
		t.Run(page, func(t *testing.T) {
			w := do(h, "GET", page, "127.0.0.1:5555", nil)
			body := w.Body.String()

			btnStart := strings.Index(body, `id="all-apps-btn"`)
			if !want {
				if btnStart >= 0 {
					t.Errorf("%s: return control present, want omitted", page)
				}
				return
			}
			if btnStart < 0 {
				t.Fatalf("%s: no all-apps-btn in same-tab mode", page)
			}
			markerStart := strings.Index(body, marker)
			if markerStart < 0 {
				t.Fatalf("%s: app-specific header marker %q not found (test itself is stale)", page, marker)
			}
			if btnStart < markerStart {
				t.Errorf("%s: all-apps-btn appears before app-specific header content %q; it must be rightmost", page, marker)
			}
			headerEnd := strings.Index(body, "</header>")
			if headerEnd < 0 || btnStart > headerEnd {
				t.Errorf("%s: all-apps-btn is not inside <header id=\"bar\">", page)
			}

			btnEnd := strings.Index(body[btnStart:], "</a>")
			btnTag := body[btnStart : btnStart+btnEnd]
			if !strings.Contains(btnTag, `class="iconbtn"`) {
				t.Errorf("%s: return control does not use the shared .iconbtn look", page)
			}
			if !strings.Contains(btnTag, `href="./"`) {
				t.Errorf("%s: return control does not route via the launcher's \"./\" convention", page)
			}
			if !strings.Contains(btnTag, `aria-label="Back to all apps"`) {
				t.Errorf("%s: return control has no accessible label", page)
			}
		})
	}
}

// ---- launcher tile links (both modes, rel=noopener) ----------------------

// The launcher's tiles are built by app.js in the browser, so the contract is
// pinned on the shipped script the way assets_test.go pins entry points: the
// same-tab default must NOT set target, and the new-tab branch must set BOTH
// target=_blank and rel=noopener.
func TestLauncherTilesFollowOpenAppsMode(t *testing.T) {
	h := newTestHandler(t, Options{})
	w := do(h, "GET", "/app.js", "127.0.0.1:5555", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("app.js = %d, want 200", w.Code)
	}
	js := w.Body.String()

	if n := strings.Count(js, `btn.target = "_blank"`); n != 1 {
		t.Errorf("app.js sets target=_blank %d times; exactly one mode-guarded assignment is expected", n)
	}
	// The two attributes must be set together, INSIDE the new-tab branch —
	// target without rel would reintroduce the reverse-tabnabbing hole the
	// setting exists to make conditional, not optional.
	branch := regexp.MustCompile(`if \(openApps === "new-tab"\) \{\s*` +
		`btn\.target = "_blank";\s*` +
		`btn\.rel = "noopener";\s*\}`)
	if !branch.MatchString(js) {
		t.Errorf("app.js: target=_blank and rel=noopener must be set together, only inside the new-tab branch")
	}
	// And the mode must come from the server's structured look endpoint, so
	// launcher tiles and injected chrome cannot disagree.
	for _, need := range []string{
		`let openApps = "same-tab";`,
		`fetch(url("api/look"))`,
		`l.open_apps === "new-tab"`,
	} {
		if !strings.Contains(js, need) {
			t.Errorf("app.js lost %q — the tile mode would no longer track the console setting", need)
		}
	}
	// The settings surface must expose both documented values as real,
	// aria-labelled buttons — the human-first surface for this setting.
	html := do(h, "GET", "/", "127.0.0.1:5555", nil).Body.String()
	for _, need := range []string{
		`id="open-apps-seg"`,
		`data-open-val="same-tab">Same tab<`,
		`data-open-val="new-tab">New tab<`,
		`aria-label="Open apps"`,
	} {
		if !strings.Contains(html, need) {
			t.Errorf("settings dialog lost %q — the mode has no human surface", need)
		}
	}
	for _, need := range []string{
		`document.querySelectorAll("#open-apps-seg button")`,
		`b.dataset.openVal === openApps`,
		`b.onclick = () => saveLook(b.dataset.openVal)`,
	} {
		if !strings.Contains(js, need) {
			t.Errorf("app.js lost open-apps settings binding %q", need)
		}
	}
}

// The structured API half: versioned JSON, both modes readable, exactly the
// two documented values writable.
func TestLookAPIIsStructured(t *testing.T) {
	h := newTestHandler(t, Options{LookStorePath: filepath.Join(t.TempDir(), "ui.json")})
	m := lookGet(t, h)
	if m["schema_version"] != lookSchema {
		t.Errorf("schema_version = %v, want %q", m["schema_version"], lookSchema)
	}
	if m["open_apps"] != OpenSameTab {
		t.Errorf("open_apps = %v, want %q", m["open_apps"], OpenSameTab)
	}
	if w := lookPut(t, h, OpenNewTab); w.Code != http.StatusOK {
		t.Fatalf("PUT = %d", w.Code)
	} else {
		var got map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || got["open_apps"] != OpenNewTab {
			t.Errorf("PUT response = %q (want open_apps echoed)", w.Body.String())
		}
	}
}

// ---- persistence, reload, validation --------------------------------------

// The setting must survive a full console restart (a fresh handler reading
// the same file) and be reflected in BOTH surfaces: the API and the injected
// chrome.
func TestLookSettingPersistsAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ui.json")

	h1 := newTestHandler(t, Options{LookStorePath: path})
	if got := lookGet(t, h1)["open_apps"]; got != OpenSameTab {
		t.Fatalf("fresh console open_apps = %v, want %q (same tab is the default)", got, OpenSameTab)
	}
	if w := lookPut(t, h1, OpenNewTab); w.Code != http.StatusOK {
		t.Fatalf("PUT /api/look = %d, want 200", w.Code)
	}

	// A brand-new console over the same state directory: reload, not memory.
	h2 := newTestHandler(t, Options{LookStorePath: path})
	if got := lookGet(t, h2)["open_apps"]; got != OpenNewTab {
		t.Fatalf("reloaded console open_apps = %v, want %q", got, OpenNewTab)
	}
	if strings.Contains(do(h2, "GET", "/term/", "127.0.0.1:5555", nil).Body.String(), `id="all-apps-btn"`) {
		t.Errorf("reloaded new-tab console still injects the return control")
	}

	// And back to same tab, which restores the control.
	if w := lookPut(t, h2, OpenSameTab); w.Code != http.StatusOK {
		t.Fatalf("PUT /api/look (same-tab) = %d, want 200", w.Code)
	}
	assertReturnControl(t, newTestHandler(t, Options{LookStorePath: path}), true)
}

// The persisted document is small, mode-exact JSON with 0600 permissions and
// no leftover temp files from the atomic write.
func TestLookStoreWritesExactAtomicDocument(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ui.json")

	h := newTestHandler(t, Options{LookStorePath: path})
	if w := lookPut(t, h, OpenNewTab); w.Code != http.StatusOK {
		t.Fatalf("PUT = %d", w.Code)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("settings document: %v", err)
	}
	want := `{"schema_version": "bashy-console-look-v1", "open_apps": "new-tab"}`
	if got := string(bytes.TrimSpace(raw)); got != want && strings.Join(strings.Fields(string(raw)), "") != strings.Join(strings.Fields(want), "") {
		t.Errorf("persisted document = %q, want %q", raw, want)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("settings document perms = %v (%v), want 0600", fi.Mode().Perm(), err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("state dir holds leftover temp files: %v", names)
	}
}

// Unknown and near-miss values are rejected loudly (400 naming both valid
// modes) and leave the stored setting untouched.
func TestLookRejectsUnknownValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ui.json")
	h := newTestHandler(t, Options{LookStorePath: path})
	if w := lookPut(t, h, OpenNewTab); w.Code != http.StatusOK {
		t.Fatalf("seed PUT = %d", w.Code)
	}
	for _, bad := range []string{"newtab", "new_tab", "Same Tab", "iframe", ""} {
		w := lookPut(t, h, bad)
		if w.Code != http.StatusBadRequest {
			t.Errorf("PUT open_apps=%q = %d, want 400 (near-misses must fail loudly)", bad, w.Code)
			continue
		}
		if !strings.Contains(w.Body.String(), OpenSameTab) || !strings.Contains(w.Body.String(), OpenNewTab) {
			t.Errorf("PUT open_apps=%q rejection %q does not name both valid modes", bad, w.Body.String())
		}
	}
	if got := lookGet(t, h)["open_apps"]; got != OpenNewTab {
		t.Errorf("rejected PUTs changed the setting to %v", got)
	}
}

// A corrupt or hand-mangled document must not take the console down or leak
// an arbitrary mode: the console serves the same-tab default, still injects
// chrome, and the next valid PUT heals the file.
func TestLookCorruptStoreFailsSafeToDefault(t *testing.T) {
	for name, junk := range map[string]string{
		"not-json":     `{"open_apps": "new-tab",`,
		"unknown-mode": `{"schema_version":"bashy-console-look-v1","open_apps":"iframe"}`,
		"binary-junk":  "\x00\x01not json at all",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "ui.json")
			if err := os.WriteFile(path, []byte(junk), 0o600); err != nil {
				t.Fatal(err)
			}
			h := newTestHandler(t, Options{LookStorePath: path})
			if got := lookGet(t, h)["open_apps"]; got != OpenSameTab {
				t.Errorf("corrupt store served open_apps = %v, want same-tab default", got)
			}
			// Fail-safe default also means the return control is back.
			w := do(h, "GET", "/term/", "127.0.0.1:5555", nil)
			if !strings.Contains(w.Body.String(), `id="all-apps-btn"`) {
				t.Errorf("corrupt store: same-tab default did not restore the return control")
			}
			// And the store heals on the next valid write.
			if w := lookPut(t, h, OpenNewTab); w.Code != http.StatusOK {
				t.Fatalf("healing PUT = %d, want 200", w.Code)
			}
			if got := lookGet(t, h)["open_apps"]; got != OpenNewTab {
				t.Errorf("healing PUT left open_apps = %v", got)
			}
		})
	}
}

// An external edit to the document is picked up without a restart: reads are
// cached on mtime+size, exactly like the pairing store.
func TestLookReloadsAfterExternalEdit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ui.json")
	h := newTestHandler(t, Options{LookStorePath: path})
	if got := lookGet(t, h)["open_apps"]; got != OpenSameTab {
		t.Fatalf("initial = %v", got)
	}
	heal := `{"schema_version":"bashy-console-look-v1","open_apps":"new-tab"}`
	if err := os.WriteFile(path, []byte(heal), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := lookGet(t, h)["open_apps"]; got != OpenNewTab {
		t.Errorf("after external edit open_apps = %v, want new-tab (cache must re-read on mtime change)", got)
	}
}

// ---- route prefix handling -------------------------------------------------

// Under outpost's forwarded prefix every page keeps its prefixed <base href>
// and the injected chrome stays RELATIVE, so the return control resolves
// against the mount, not the host root.
//
// The request goes to the page seam (s.handleTermPage/handleSPA) rather than
// through the gate: a forwarded-prefix request is, by definition, an
// arrived-via-cloud one, and whether it carries a vouch is the gate's
// contract, already covered by the gate tests — this test is about the bytes
// the seam renders under that prefix.
func TestChromePreservesRoutePrefix(t *testing.T) {
	s, h, closer, err := newHandler(Options{})
	if err != nil {
		t.Fatalf("newHandler: %v", err)
	}
	defer func() { _ = closer() }()
	prefixed := func(page string) string {
		r := httptest.NewRequest("GET", page, nil)
		r.RemoteAddr = "127.0.0.1:5555"
		r.Header.Set("X-Forwarded-Prefix", "/matrix/h/laptop/app/console")
		w := httptest.NewRecorder()
		if page == "/" {
			s.handleSPA(w, r)
		} else {
			s.handleTermPage(w, r)
		}
		return w.Body.String()
	}
	for _, page := range []string{"/", "/term/"} {
		body := prefixed(page)
		if !strings.Contains(body, `<base href="/matrix/h/laptop/app/console/`) {
			t.Errorf("%s: prefixed <base href> missing under X-Forwarded-Prefix", page)
		}
		if page != "/" {
			// Same-tab default injects the control; it must ride "./".
			if !strings.Contains(body, `id="all-apps-btn" class="iconbtn" href="./"`) {
				t.Errorf("%s: return control not present as a relative ./ link under a prefix", page)
			}
			if strings.Contains(body, `href="/`) && strings.Contains(body, `href="/term/"`) {
				t.Errorf("%s: a chrome link went root-absolute; prefix handling regressed", page)
			}
		}
	}
	// The unprefixed control still exists for parity with the full-stack test.
	if !strings.Contains(do(h, "GET", "/term/", "127.0.0.1:5555", nil).Body.String(), `href="./"`) {
		t.Errorf("unprefixed term page lost its relative links")
	}
}

// ---- launcher tile marks --------------------------------------------------

// EVERY BUILT-IN TILE MUST HAVE A DRAWN MARK.
//
// appIcon's fallback chain is server icon → our own mark for a known name →
// the app's initial letter. The letter is honest for a third-party app we know
// nothing about; for one of OUR panels it is a bug, and a silent one — the
// grid still renders, so nothing fails.
//
// That is exactly how it shipped: `relay` and `board` were renamed to `meet`
// and `sprint` without rekeying the SVG map, so two of the six built-ins
// dropped to "M" and "S". The lookup is by PANEL NAME, so this test pins the
// join the rename broke.
func TestEveryBuiltinTileHasItsOwnMark(t *testing.T) {
	h := newTestHandler(t, Options{})
	w := do(h, "GET", "/app.js", "127.0.0.1:5555", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("app.js = %d, want 200", w.Code)
	}
	js := w.Body.String()

	block := regexp.MustCompile(`(?s)const SVG = \{(.*?)\n\};`).FindStringSubmatch(js)
	if block == nil {
		t.Fatal("app.js has no SVG mark table")
	}
	marks := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^  ([A-Za-z]\w*):`).FindAllStringSubmatch(block[1], -1) {
		marks[m[1]] = true
	}
	colors := map[string]bool{}
	if b := regexp.MustCompile(`(?s)const COLORS = \{(.*?)\n\};`).FindStringSubmatch(js); b != nil {
		for _, m := range regexp.MustCompile(`([A-Za-z]\w*):`).FindAllStringSubmatch(b[1], -1) {
			colors[m[1]] = true
		}
	}

	for _, p := range Discover() {
		if p.Icon != "" {
			continue // the panel describes its own mark; the table is not consulted
		}
		if !marks[p.Name] {
			t.Errorf("panel %q has no mark in app.js's SVG table — its tile falls back to the letter %q",
				p.Name, strings.ToUpper(p.Name[:1]))
		}
		if !colors[p.Name] {
			t.Errorf("panel %q has no pinned tile colour — it would be hashed into the generic palette", p.Name)
		}
	}
}

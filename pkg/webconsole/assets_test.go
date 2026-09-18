package webconsole

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/treesitter"
)

var assetRe = regexp.MustCompile(`(?:src|href)="([^":?#][^":]*)"`)

// Every asset a served page references must actually resolve.
//
// This exists because a page can look fine in the markup and still be broken:
// the launcher once shipped an app.js whose repaint entry point had been spliced
// out, and the only symptom was a blank card — no 404, no server error, nothing
// in the response to assert on. A missing or unreachable asset is the same class
// of silent failure, and it is cheap to rule out.
func TestServedPagesReferenceOnlyResolvableAssets(t *testing.T) {
	h := newTestHandler(t, Options{})

	// Every page the console serves at its own root, not just the launcher: a
	// standalone app page that referenced a script nobody shipped would render
	// its chrome and do nothing, with a 200 on every server-side check.
	for _, page := range []string{"/", "/term/", "/mb/", "/sprint/", "/inbox/"} {
		w := do(h, "GET", page, "127.0.0.1:5555", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", page, w.Code)
		}
		body := w.Body.String()

		found := 0
		for _, m := range assetRe.FindAllStringSubmatch(body, -1) {
			ref := m[1]
			// Only local, relative asset paths; skip data: URIs, absolute URLs
			// and in-page anchors.
			if strings.HasPrefix(ref, "data:") || strings.Contains(ref, "://") ||
				strings.HasPrefix(ref, "#") || ref == "./" || ref == "/" {
				continue
			}
			found++
			got := do(h, "GET", "/"+strings.TrimPrefix(ref, "/"), "127.0.0.1:5555", nil)
			if got.Code != http.StatusOK {
				t.Errorf("%s references %q, which serves %d", page, ref, got.Code)
			}
		}
		if found == 0 {
			t.Errorf("%s referenced no local assets — the page is probably not what we think", page)
		}
	}
}

// The launcher's own script must define the repaint entry point it calls on
// load. Losing it is invisible from the server side: every asset still returns
// 200 and the page renders empty.
func TestLauncherScriptDefinesItsEntryPoint(t *testing.T) {
	h := newTestHandler(t, Options{})
	w := do(h, "GET", "/app.js", "127.0.0.1:5555", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("app.js = %d", w.Code)
	}
	js := w.Body.String()
	for _, fn := range []string{"function render(", "function renderHome(", "function tile(", "async function refresh("} {
		if !strings.Contains(js, fn) {
			t.Errorf("app.js calls but does not define %q", strings.TrimSuffix(fn, "("))
		}
	}
}

// The launcher is a tracked, embedded artifact, so a bad merge can leave every
// HTTP check green while the browser refuses to execute any of it. Parse the
// exact bytes served to the browser and reject tree-sitter recovery nodes.
func TestLauncherScriptIsValidJavaScript(t *testing.T) {
	h := newTestHandler(t, Options{})
	for _, script := range consoleScripts {
		w := do(h, "GET", script, "127.0.0.1:5555", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("%s = %d", script, w.Code)
		}
		tree, err := treesitter.NewParser().Parse(context.Background(), w.Body.Bytes(), "javascript")
		if err != nil {
			t.Fatalf("parse %s: %v", script, err)
		}
		if tree.Root == nil || tree.Root.HasError() {
			t.Errorf("%s contains invalid JavaScript syntax", script)
		}
	}
}

// consoleScripts is every script the console ships. Held in one place so the
// syntax check and the validator check cannot cover different subsets — the
// gap that lets an unparseable page slip through with a green suite.
var consoleScripts = []string{"/app.js", "/term.js", "/mb.js", "/board.js", "/inbox.js"}

// Embedded assets must carry a validator.
//
// embed.FS reports a zero modtime, so an asset served without an ETag gives the
// browser nothing to revalidate against and it applies heuristic caching — the
// launcher kept running a previous build's script for an entire session, which
// made a fixed bug look unfixed.
func TestAssetsAreRevalidatable(t *testing.T) {
	h := newTestHandler(t, Options{})

	for _, asset := range append(append([]string{}, consoleScripts...), "/app.css", "/backgrounds.css") {
		w := do(h, "GET", asset, "127.0.0.1:5555", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("%s = %d", asset, w.Code)
		}
		etag := w.Header().Get("ETag")
		if etag == "" {
			t.Errorf("%s has no ETag; a browser cannot tell a new build from the old one", asset)
			continue
		}
		if cc := w.Header().Get("Cache-Control"); cc == "" {
			t.Errorf("%s has no Cache-Control", asset)
		}
		// And the validator must actually work, or it is decoration.
		again := do(h, "GET", asset, "127.0.0.1:5555", http.Header{"If-None-Match": {etag}})
		if again.Code != http.StatusNotModified {
			t.Errorf("%s with If-None-Match returned %d, want 304", asset, again.Code)
		}
	}
}

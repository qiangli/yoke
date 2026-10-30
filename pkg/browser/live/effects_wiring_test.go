package live

import (
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/browser/wire"
)

// Structural pins for the S536.4 live wire-up: the new text method is
// advertised (so the hub's per-method gate lets it through), routed,
// and carried end to end; the click/scroll/screenshot paths keep their
// shape while reporting the new outcomes.

// TestTextMethodIsAdvertisedAndRouted: an unadvertised method is refused
// loudly by the hub gate, so the stranded-method class is pinned here,
// not found in production.
func TestTextMethodIsAdvertisedAndRouted(t *testing.T) {
	src := extensionFile(t, "background.js")
	if !strings.Contains(src, `"text",`) {
		t.Fatal("text is not advertised in SUPPORTED_METHODS — the hub's gate will refuse it")
	}
	if !strings.Contains(src, `case "text":`) || !strings.Contains(src, "textInTab(tabId, params)") {
		t.Fatal("background.js does not route the text method to textInTab")
	}
	if !strings.Contains(src, "async function textInTab(tabId, params)") {
		t.Fatal("textInTab is gone: addressed targets lose frame routing, whole pages lose merging")
	}
}

// TestEffectsParamsReachTheExtension guards the silent-drop class for
// the new and extended knobs: a flag the CLI accepts but the transport
// discards is exactly how `screenshot <path>` came to exit 0 having
// done nothing.
func TestEffectsParamsReachTheExtension(t *testing.T) {
	method, params, err := actionToParams(wire.Action{Type: wire.ActionText, Selector: "#bio"})
	if err != nil {
		t.Fatal(err)
	}
	if method != "text" || params["selector"] != "#bio" {
		t.Fatalf("text params = %q %#v", method, params)
	}
	method, params, err = actionToParams(wire.Action{Type: wire.ActionScroll, Direction: "down", Amount: 500, Selector: "#panel"})
	if err != nil {
		t.Fatal(err)
	}
	if method != "scroll" || params["selector"] != "#panel" || params["amount"] != 500 {
		t.Fatalf("addressed scroll params = %q %#v", method, params)
	}
	method, params, err = actionToParams(wire.Action{Type: wire.ActionScroll, Direction: "down"})
	if err != nil {
		t.Fatal(err)
	}
	if method != "scroll" || params["direction"] != "down" {
		t.Fatalf("window scroll params = %q %#v", method, params)
	}
}

// TestPageCoreExportsEffectsFunctions: the CDP stitcher and the live
// extension share page.js shotLayout/shotSet/shotRestore (and text), so
// a rename that strands one backend fails here, not in production.
func TestPageCoreExportsEffectsFunctions(t *testing.T) {
	page := extensionFile(t, "page.js")
	for _, want := range []string{
		"extract, count, click, type, select, scroll, text,",
		"resolveSelector, shotLayout, shotSet, shotRestore,",
		"CLICK_OBSERVE_MS",
		"navigated", `"dialog"`, "dom-changed", "no-effect",
		"scrollTargetScroller", "visibleTextOf",
		"suppressed: \"password\"",
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("page.js lost %q", want)
		}
	}
}

// TestStitchedCaptureIsBoundedAndRestoring: the live stitch must be
// time-bounded (the hub ceiling is 30 s), restore positions on failure,
// and name its path in the result — a silent downgrade reads as one.
func TestStitchedCaptureIsBoundedAndRestoring(t *testing.T) {
	src := extensionFile(t, "background.js")
	for _, want := range []string{
		"STITCH_TIMEOUT_MS",
		"withTimeout(stitchFullPage(tabId), STITCH_TIMEOUT_MS",
		"async function stitchFullPage(tabId)",
		"shotRestore",
		"stitched_full_page",
		"stitched capture failed, falling back",
	} {
		if !strings.Contains(src, want) {
			t.Fatalf("live stitch lost %q", want)
		}
	}
}

// TestExtensionVersionCarriesText: the text method needs extension
// 0.10.0+ (it is advertised, not defaulted). The floor stays where the
// stale-extension gate put it; this pins the new capability's arrival.
func TestExtensionVersionCarriesText(t *testing.T) {
	if versionLess(EmbeddedVersion(), "0.10.0") {
		t.Fatalf("embedded extension v%s predates the text method (0.10.0)", EmbeddedVersion())
	}
}

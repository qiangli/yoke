package live

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

func extensionFile(t *testing.T, name string) string {
	t.Helper()
	b, err := extensionFS.ReadFile("extension/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestExtensionVersionMeetsTheFloor: the embedded extension is what
// `browser setup live` hands the user, so shipping one below the hub's
// own floor would make every live call fail on a stale-version error
// the user cannot fix.
func TestExtensionVersionMeetsTheFloor(t *testing.T) {
	var manifest struct {
		Version string   `json:"version"`
		Perms   []string `json:"permissions"`
	}
	if err := json.Unmarshal([]byte(extensionFile(t, "manifest.json")), &manifest); err != nil {
		t.Fatal(err)
	}
	if versionLess(manifest.Version, LiveExtensionMinVersion) {
		t.Fatalf("embedded extension v%s is below the hub floor v%s",
			manifest.Version, LiveExtensionMinVersion)
	}
	// The correct-tab capture path needs chrome.debugger to reach a
	// tab that is not in the foreground.
	if !containsStr(manifest.Perms, "debugger") {
		t.Fatal("manifest lost the debugger permission: background-tab screenshot falls back to focus-stealing")
	}
}

// TestScreenshotResolvesTheDrivenTab is the structural guard for the
// expensive defect: captureVisibleTab captures a WINDOW's foreground
// tab, so calling it without first proving the driven tab IS the
// foreground tab returns a real PNG of the wrong page — a failure that
// looks exactly like a success.
func TestScreenshotResolvesTheDrivenTab(t *testing.T) {
	src := extensionFile(t, "background.js")

	if !strings.Contains(src, "async function takeScreenshot(tabId, params)") {
		t.Fatal("takeScreenshot no longer takes the resolved tabId + params")
	}
	// Every captureVisibleTab call must be guarded by a foreground
	// check or reached only after activating the target tab.
	for _, guard := range []string{
		`chrome.tabs.query({ active: true, windowId: tab.windowId })`,
		"front.id === tabId",
		"captureViaDebugger",
		"Page.captureScreenshot",
	} {
		if !strings.Contains(src, guard) {
			t.Fatalf("screenshot lost its correct-tab guard: %q missing", guard)
		}
	}
	// A paint barrier before capture, so an image taken right after a
	// click is not the pre-click frame.
	if !strings.Contains(src, "settleTab(tabId") || !strings.Contains(src, "requestAnimationFrame") {
		t.Fatal("screenshot lost its paint barrier — stale frames come back as success")
	}
}

// TestClickReportsAnUnresolvedTarget: an empty injection frame used to
// surface as {"success":true}. Both the guard and the honest error
// must stay.
func TestClickReportsAnUnresolvedTarget(t *testing.T) {
	src := extensionFile(t, "background.js")
	if !strings.Contains(src, "async function injectOne(") {
		t.Fatal("injectOne is gone: an empty injection frame becomes success again")
	}
	if strings.Contains(src, "return result || {}") {
		t.Fatal("a bare `result || {}` fallback is back — that is the success-that-did-not-happen bug")
	}
	for _, want := range []string{"__miss", "is not a CSS selector", "out of range"} {
		if !strings.Contains(src, want) {
			t.Fatalf("click lost its unresolved-target reason: %q missing", want)
		}
	}
	// click-by-index and extract must enumerate identically, or [N]
	// from a listing addresses a different element than it names.
	if strings.Count(src, `"a, button, input, select, textarea, [role='button'], [role='link']"`) < 2 {
		t.Fatal("the click and extract enumerations diverged")
	}
}

// TestTabActionsAndMethodsAreDiscoverable pins the class-C rules on
// the extension side: an invalid action names the valid set, and the
// new method is advertised so the hub's per-method gate lets it
// through.
func TestTabActionsAndMethodsAreDiscoverable(t *testing.T) {
	src := extensionFile(t, "background.js")
	if !strings.Contains(src, `TAB_ACTIONS = ["list", "switch", "new", "close"]`) {
		t.Fatal("the tab vocabulary is no longer a named set")
	}
	if !strings.Contains(src, "expected one of: ${TAB_ACTIONS.join") {
		t.Fatal("an invalid tab action no longer names the valid set")
	}
	if !strings.Contains(src, `"dispatch_event",`) {
		t.Fatal("dispatch_event is not advertised in SUPPORTED_METHODS — the hub's gate will refuse it")
	}
	// Structured tab data, not a re-parseable pretty-printed string.
	if !strings.Contains(src, "function tabRecord(") {
		t.Fatal("tabs list no longer emits typed records")
	}
	// Substring addressing, so a script is not racing a shifting index.
	if !strings.Contains(src, "match_url") || !strings.Contains(src, "be more specific") {
		t.Fatal("tabs lost substring addressing / its ambiguity error")
	}
}

// TestExtractReportsViewport: with page CSP blocking eval there is no
// other way to read window.innerWidth, and "a responsive breakpoint
// hid it" vs "it is absent" is otherwise untestable.
func TestExtractReportsViewport(t *testing.T) {
	src := extensionFile(t, "background.js")
	for _, want := range []string{"devicePixelRatio", "viewport: viewport", "include_hidden", "visible: vis"} {
		if !strings.Contains(src, want) {
			t.Fatalf("extract lost %q", want)
		}
	}
}

// TestNoBareCaptureVisibleTabRegression is a belt-and-braces scan: the
// only captureVisibleTab calls allowed are the two guarded ones inside
// takeScreenshot.
func TestNoBareCaptureVisibleTabRegression(t *testing.T) {
	src := extensionFile(t, "background.js")
	re := regexp.MustCompile(`captureVisibleTab\(`)
	if n := len(re.FindAllString(src, -1)); n != 2 {
		t.Fatalf("captureVisibleTab is called %d times; expected exactly the 2 guarded calls in takeScreenshot", n)
	}
}

func containsStr(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// TestBackgroundCaptureIsTimeBounded pins the fix for the defect the FIRST
// live-mode run found, which no test over this file could have predicted:
// Page.captureScreenshot on a hidden tab does not fail, it NEVER RETURNS
// (Chrome suspends compositing for a background tab). The focus-and-restore
// fallback is reached only by a thrown exception, so an unbounded await
// bypassed the fallback entirely and blocked until the caller's 30s ceiling.
func TestBackgroundCaptureIsTimeBounded(t *testing.T) {
	src := extensionFile(t, "background.js")
	for _, want := range []string{
		"CDP_CAPTURE_TIMEOUT_MS",
		"function withTimeout(",
		"captureViaDebugger(tabId, fullPage), CDP_CAPTURE_TIMEOUT_MS",
	} {
		if !strings.Contains(src, want) {
			t.Fatalf("the CDP capture is no longer time-bounded: %q missing", want)
		}
	}
	// A timed-out capture must not leak the debugger attach it was holding.
	if !strings.Contains(src, "debuggerAttach.release(tabId); } catch") {
		t.Fatal("the timeout path no longer releases the debugger attach")
	}
	// The surface override is what lets a hidden tab be captured at all
	// without stealing focus.
	for _, want := range []string{
		"Emulation.setDeviceMetricsOverride",
		"Emulation.clearDeviceMetricsOverride",
		"function viewportOf(",
	} {
		if !strings.Contains(src, want) {
			t.Fatalf("the hidden-tab surface override is gone: %q missing", want)
		}
	}
}

// TestHiddenTabSkipsThePaintBarrier pins the SECOND defect the live gate
// found, after the first fix addressed the wrong cause.
//
// requestAnimationFrame NEVER FIRES in a hidden tab — Chrome stops servicing
// frames for one — so the paint barrier awaited a frame that never came and
// took the whole screenshot request down with it. Measured on hardware:
// script injection into the hidden tab returned in 0s while a rAF callback in
// the same tab had not fired after 2s. Visibility must therefore be resolved
// BEFORE settling, because it selects which barrier is even possible.
func TestHiddenTabSkipsThePaintBarrier(t *testing.T) {
	src := extensionFile(t, "background.js")

	if !strings.Contains(src, "async function settleTab(tabId, settleMs, visible)") {
		t.Fatal("settleTab no longer takes visibility, so it cannot skip the barrier")
	}
	if !strings.Contains(src, "if (!visible) {") {
		t.Fatal("settleTab no longer skips the rAF barrier for a hidden tab")
	}
	if !strings.Contains(src, "SETTLE_TIMEOUT_MS") {
		t.Fatal("the paint barrier is unbounded again")
	}

	// Order matters: visibility is resolved, THEN we settle. Settling first
	// was the bug.
	vis := strings.Index(src, "visible = !!front && front.id === tabId")
	settle := strings.Index(src, "await settleTab(tabId, params.settle_ms, visible)")
	if vis < 0 || settle < 0 {
		t.Fatal("takeScreenshot no longer resolves visibility before settling")
	}
	if settle < vis {
		t.Fatal("takeScreenshot settles BEFORE resolving visibility — that is the hang")
	}
}

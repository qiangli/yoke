package cdpactions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/browser/wire"
)

// The hidden-only refusal must read exactly like live's (page.js
// resolveTarget, opts.visibleOnly): probe/solo share the message and
// the --include-hidden override.
func TestHiddenOnlyMessageMatchesLive(t *testing.T) {
	pageJS, err := os.ReadFile(filepath.Join("..", "..", "live", "extension", "page.js"))
	if err != nil {
		t.Fatalf("read live page.js: %v", err)
	}
	const refusal = "matches only hidden elements (pass --include-hidden to act on one anyway)"
	if !strings.Contains(string(pageJS), refusal) {
		t.Fatalf("live page.js no longer refuses with %q", refusal)
	}
	got := hiddenOnlyMessage("input[type=email]")
	want := `selector "input[type=email]" matches only hidden elements (pass --include-hidden to act on one anyway)`
	if got != want {
		t.Fatalf("hiddenOnlyMessage=%q, want live's %q", got, want)
	}
}

// Comma lists and :not() must work: resolution goes through
// querySelectorAll, never by splitting the selector, and the
// visibility predicate mirrors live's shown().
func TestResolveSelectorScriptCoversCommaLists(t *testing.T) {
	script := resolveSelectorScript("input, textarea")
	if !strings.Contains(script, "querySelectorAll") {
		t.Fatalf("resolution must use querySelectorAll:\n%s", script)
	}
	if strings.Contains(script, `split(",")`) || strings.Contains(script, `split(',')`) {
		t.Fatalf("resolution must not split comma lists:\n%s", script)
	}
	for _, want := range []string{`getComputedStyle`, `getBoundingClientRect`, `"visible"`, `"hidden"`, `"missing"`, `"bad"`} {
		if !strings.Contains(script, want) {
			t.Fatalf("resolution script missing %s:\n%s", want, script)
		}
	}
	if !strings.Contains(resolveSelectorScript(`input:not([type=hidden])`), `"input:not([type=hidden])"`) {
		t.Fatalf(":not() selector must reach querySelectorAll intact")
	}
}

func TestTargetJSPathPrefersVisible(t *testing.T) {
	vis := targetJSPath("input, textarea", false)
	if !strings.Contains(vis, "querySelectorAll") || !strings.Contains(vis, "shown(") {
		t.Fatalf("default target must pick the first visible match:\n%s", vis)
	}
	hidden := targetJSPath("input, textarea", true)
	if !strings.Contains(hidden, "querySelector(") || strings.Contains(hidden, "shown(") {
		t.Fatalf("--include-hidden must keep the first match, hidden or not:\n%s", hidden)
	}
}

func TestActionTimeoutDefaultsAndOverrides(t *testing.T) {
	if got := actionTimeout(wire.Action{}); got != 5*time.Second {
		t.Fatalf("default timeout=%v, want 5s", got)
	}
	if got := actionTimeout(wire.Action{TimeoutMs: 1500}); got != 1500*time.Millisecond {
		t.Fatalf("override timeout=%v, want 1.5s", got)
	}
	if DefaultActionTimeout != 5*time.Second {
		t.Fatalf("DefaultActionTimeout=%v, want 5s", DefaultActionTimeout)
	}
}

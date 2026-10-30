package live

import (
	"strings"
	"testing"
)

// Real Chrome (BASHY_BROWSER_LIVE=1), page.js loaded as the extension
// loads it: hidden text must not come back as text, and an addressed
// scroll must climb out of a shadow tree through its host.

const hiddenTextFixture = `<!doctype html><html><body>
<div id="empty" style="visibility:hidden">only hidden words</div>
<div id="gone" style="display:none">display none words <b>bold gone</b></div>
<div id="mixed">shown <span style="display:none">SECRET-HIDDEN</span> tail<div hidden>ALSO-HIDDEN</div></div>
<div id="wrap"><p style="display:none">nested hidden only</p></div>
<svg id="icon" width="40" height="20"><text>svg shown</text><g style="display:none"><text>svg hidden</text></g></svg>
<p id="plain">visible plain</p>
</body></html>`

func TestPageTextNeverFallsBackToHiddenText(t *testing.T) {
	h := newHeaderHarness(t, map[string]headerFixture{"/h": {body: hiddenTextFixture}})
	h.open("/h")

	for _, sel := range []string{"#empty", "#gone", "#wrap"} {
		res := h.call("text", map[string]any{"selector": sel})
		if res["error"] != nil {
			t.Fatalf("text %s: %v", sel, res["error"])
		}
		if res["content"] != "" {
			t.Fatalf("text %s = %q, want empty (hidden text must not leak)", sel, res["content"])
		}
	}
	mixed := h.call("text", map[string]any{"selector": "#mixed"})
	got, _ := mixed["content"].(string)
	if !strings.Contains(got, "shown") || !strings.Contains(got, "tail") ||
		strings.Contains(got, "SECRET-HIDDEN") || strings.Contains(got, "ALSO-HIDDEN") {
		t.Fatalf("mixed text = %q, want visible parts only", got)
	}
	svg := h.call("text", map[string]any{"selector": "#icon"})
	sv, _ := svg["content"].(string)
	if !strings.Contains(sv, "svg shown") || strings.Contains(sv, "svg hidden") {
		t.Fatalf("svg text = %q, want shown text only", sv)
	}
	whole := h.call("text", nil)
	wc, _ := whole["content"].(string)
	if strings.Contains(wc, "SECRET-HIDDEN") || strings.Contains(wc, "display none words") || strings.Contains(wc, "nested hidden only") {
		t.Fatalf("whole-page text leaks hidden text:\n%s", wc)
	}
	if !strings.Contains(wc, "visible plain") {
		t.Fatalf("whole-page text lost visible text:\n%s", wc)
	}
}

// The scroller is a light-DOM ancestor of the shadow host: only a walk
// through ShadowRoot.host reaches it.
const shadowScrollFixture = `<!doctype html><html><body style="margin:0">
<div id="outer" style="height:150px;overflow-y:auto;border:1px solid #000">
  <div id="host"></div>
  <div style="height:1000px">spacer</div>
</div>
<script>
const root = document.getElementById('host').attachShadow({mode:'open'});
root.innerHTML = '<p id="inner">inside the shadow</p>';
</script>
<div style="height:2000px">tall</div>
</body></html>`

func TestPageScrollCrossesShadowHost(t *testing.T) {
	h := newHeaderHarness(t, map[string]headerFixture{"/s": {body: shadowScrollFixture}})
	h.open("/s")

	res := h.call("scroll", map[string]any{"selector": "#inner", "amount": 80})
	if res["error"] != nil {
		t.Fatalf("scroll into shadow target: %v", res["error"])
	}
	var outerTop, winY float64
	h.mainEval("document.getElementById('outer').scrollTop", &outerTop)
	h.mainEval("window.scrollY", &winY)
	if outerTop != 80 || winY != 0 {
		t.Fatalf("shadow-addressed scroll moved outer=%v window=%v, want 80 and 0 (res %v)", outerTop, winY, res)
	}
}

const editableFixture = `<!doctype html><html><body>
<label for="pt">Plain notes</label>
<div id="pt" contenteditable="plaintext-only" style="min-height:30px;border:1px solid #000"></div>
<div id="lbl">Rich notes</div>
<div id="rt" contenteditable="true" aria-labelledby="lbl" style="min-height:30px;border:1px solid #000"></div>
<div id="em" contenteditable="" aria-label="Empty attr" style="min-height:30px;border:1px solid #000"></div>
<div id="off" contenteditable="false" aria-label="Not editable" style="min-height:30px">locked</div>
</body></html>`

func TestPageExtractsAndAddressesContenteditable(t *testing.T) {
	h := newHeaderHarness(t, map[string]headerFixture{"/e": {body: editableFixture}})
	h.open("/e")

	ex := h.call("extract", map[string]any{})
	out, _ := ex["elements"].(string)
	data, _ := ex["data"].(string)
	out += data
	for _, want := range []string{"Plain notes", "Rich notes", "Empty attr"} {
		if !strings.Contains(out, want) {
			t.Fatalf("extract lost editable field %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Not editable") {
		t.Fatalf("extract listed contenteditable=false:\n%s", out)
	}
	for label, id := range map[string]string{"Plain notes": "pt", "Rich notes": "rt"} {
		if res := h.call("type", map[string]any{"selector": "label=" + label, "text": "v-" + id}); res["error"] != nil {
			t.Fatalf("type label=%s: %v", label, res["error"])
		}
		var got string
		h.mainEval("document.getElementById('"+id+"').innerText", &got)
		if got != "v-"+id {
			t.Fatalf("#%s text = %q, want %q", id, got, "v-"+id)
		}
	}
}

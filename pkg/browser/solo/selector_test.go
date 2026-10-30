package solo

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"

	"github.com/qiangli/yoke/pkg/browser/wire"
)

// hiddenFirstFixture is the Typeform shape that hung probe/solo: the
// first match of a generic selector lives in a display:none step while
// the field the user sees comes later. Both a comma list and a :not()
// selector match the hidden input first.
const hiddenFirstFixture = `<!doctype html><html><body>
<section style="display:none"><input type="password" id="q-hidden"></section>
<section><input type="text" id="q-visible"><textarea id="q-area"></textarea></section>
<section style="display:none"><input type="email" id="q-only-hidden"></section>
<label for="q-labelled">Labelled answer</label><input id="q-labelled" name="labelled">
<div id="shadow-host"></div>
<script>
document.getElementById('shadow-host').attachShadow({mode:'open'}).innerHTML = '<label for="shadow-answer">Shadow answer</label><input id="shadow-answer" name="shadow">';
window.hit='';
document.getElementById('q-visible').addEventListener('click', () => { window.hit = 'visible'; });
document.getElementById('q-hidden').addEventListener('click', () => { window.hit = 'hidden'; });
</script></body></html>`

// TestSoloSelectorPrefersVisibleMatch drives type/click/wait-for-selector
// through a real headless Chrome: each acts on the visible match when the
// first match is hidden, refuses a hidden-only selector with live's
// message unless --include-hidden, and never exceeds its timeout.
// Opt-in like the other live-Chrome tests: BASHY_BROWSER_LIVE=1.
func TestSoloSelectorPrefersVisibleMatch(t *testing.T) {
	if os.Getenv("BASHY_BROWSER_LIVE") == "" {
		t.Skip("live Chrome test disabled; set BASHY_BROWSER_LIVE=1 to run (launches a real Chrome)")
	}
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("BASHY_SECRETS_TOKEN", "")
	t.Setenv("BASHY_API_KEY", "")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, hiddenFirstFixture)
	}))
	defer srv.Close()

	ctx := context.Background()
	c := New(Config{})
	if !c.Available(ctx) {
		t.Skip("no Chrome or Chromium binary found")
	}
	defer c.Close()

	exec := func(a wire.Action) *wire.Result {
		t.Helper()
		res, err := c.Execute(ctx, a)
		if err != nil {
			t.Fatalf("%s %q: %v", a.Type, a.Selector, err)
		}
		return res
	}
	value := func(id string) string {
		t.Helper()
		var v string
		expr := fmt.Sprintf("document.getElementById(%q).value", id)
		if err := chromedp.Run(c.current(), chromedp.Evaluate(expr, &v)); err != nil {
			t.Fatal(err)
		}
		return v
	}
	str := func(expr string) string {
		t.Helper()
		var v string
		if err := chromedp.Run(c.current(), chromedp.Evaluate(expr, &v)); err != nil {
			t.Fatal(err)
		}
		return v
	}
	open := func() {
		t.Helper()
		if res := exec(wire.Action{Type: wire.ActionNavigate, URL: srv.URL}); !res.Success {
			t.Fatalf("navigate: %+v", res)
		}
	}

	// type acts on the visible match for both selector shapes; the
	// hidden first match stays empty.
	for _, sel := range []string{"input, textarea", "input:not([type=hidden])"} {
		open()
		start := time.Now()
		if res := exec(wire.Action{Type: wire.ActionType, Selector: sel, Text: "hello"}); !res.Success {
			t.Fatalf("type %q: %+v", sel, res)
		}
		if d := time.Since(start); d > 15*time.Second {
			t.Fatalf("type %q took %v, want well under the timeout", sel, d)
		}
		if got := value("q-visible"); got != "hello" {
			t.Fatalf("type %q filled visible=%q, want %q", sel, got, "hello")
		}
		if got := value("q-hidden"); got != "" {
			t.Fatalf("type %q hit the hidden first match: %q", sel, got)
		}
		if got := value("q-area"); got != "" {
			t.Fatalf("type %q skipped past the first visible match into the textarea: %q", sel, got)
		}
	}
	for _, sel := range []string{"xpath=//*[@id='q-labelled']", "//*[@id='q-labelled']", "(//*[@id='q-labelled'])[1]", "label=Labelled answer"} {
		if res := exec(wire.Action{Type: wire.ActionType, Selector: sel, Text: "labelled"}); !res.Success {
			t.Fatalf("selector %q: %+v", sel, res)
		}
	}
	for _, sel := range []string{"#shadow-answer", "label=Shadow answer", "xpath=//*[@id='shadow-answer']"} {
		if res := exec(wire.Action{Type: wire.ActionType, Selector: sel, Text: "shadow"}); !res.Success {
			t.Fatalf("shadow selector %q: %+v", sel, res)
		}
	}

	// click lands on the visible match too.
	open()
	if res := exec(wire.Action{Type: wire.ActionClick, Selector: "input, textarea"}); !res.Success {
		t.Fatalf("click: %+v", res)
	}
	if hit := str("window.hit"); hit != "visible" {
		t.Fatalf("click hit %q, want the visible field", hit)
	}
	if active := str("document.activeElement && document.activeElement.id"); active != "q-visible" {
		t.Fatalf("click focused %q, want q-visible", active)
	}

	// wait-for-selector resolves promptly instead of parking on the
	// hidden first match until timeout.
	for _, sel := range []string{"input, textarea", "input:not([type=hidden])"} {
		start := time.Now()
		res := exec(wire.Action{Type: wire.ActionWaitForSelector, Selector: sel, TimeoutMs: 5000})
		if !res.Success {
			t.Fatalf("wait %q: %+v", sel, res)
		}
		if d := time.Since(start); d > 4*time.Second {
			t.Fatalf("wait %q took %v, want a prompt visible match", sel, d)
		}
	}

	// A selector matching only hidden elements is refused with live's
	// message — by type, click, and wait — unless --include-hidden.
	for _, tc := range []wire.Action{
		{Type: wire.ActionType, Selector: "input[type=email]", Text: "a@b.c"},
		{Type: wire.ActionClick, Selector: "input[type=email]"},
		{Type: wire.ActionWaitForSelector, Selector: "input[type=email]", TimeoutMs: 2000},
	} {
		start := time.Now()
		res := exec(tc)
		if res.Success || !strings.Contains(res.Error, "only hidden") {
			t.Fatalf("%s hidden-only: want the live refusal, got %+v", tc.Type, res)
		}
		if d := time.Since(start); d > 15*time.Second {
			t.Fatalf("%s hidden-only took %v, nothing may exceed the timeout", tc.Type, d)
		}
	}

	// --include-hidden is the explicit override: the hidden field fills.
	if res := exec(wire.Action{Type: wire.ActionType, Selector: "input[type=email]", Text: "a@b.c", IncludeHidden: true}); !res.Success {
		t.Fatalf("include-hidden type: %+v", res)
	} else if got := value("q-only-hidden"); got != "a@b.c" {
		t.Fatalf("include-hidden type filled %q, want %q", got, "a@b.c")
	}

	// A selector that matches nothing times out inside its budget.
	start := time.Now()
	res := exec(wire.Action{Type: wire.ActionWaitForSelector, Selector: "#never-here", TimeoutMs: 1500})
	if res.Success || !strings.Contains(res.Error, "no visible match") {
		t.Fatalf("missing wait: want a bounded miss, got %+v", res)
	}
	if d := time.Since(start); d < time.Second || d > 15*time.Second {
		t.Fatalf("missing wait took %v, want about the 1.5s budget", d)
	}

	open()
	ex := exec(wire.Action{Type: wire.ActionExtract})
	if !ex.Success {
		t.Fatalf("extract: %+v", ex)
	}
	var listing struct {
		Elements []struct {
			Index    int    `json:"index"`
			Name     string `json:"name"`
			StableID string `json:"stable_id"`
			Label    string `json:"label"`
			Selector string `json:"selector"`
		} `json:"elements"`
	}
	if err := json.Unmarshal([]byte(ex.Data), &listing); err != nil {
		t.Fatal(err)
	}
	var idx int
	for _, e := range listing.Elements {
		if e.Name == "labelled" {
			idx = e.Index
			if e.StableID != "q-labelled" || e.Label != "Labelled answer" || e.Selector != "#q-labelled" {
				t.Fatalf("field metadata: %+v", e)
			}
		}
	}
	if idx == 0 {
		t.Fatalf("labelled field absent: %s", ex.Data)
	}
	if res := exec(wire.Action{Type: wire.ActionType, ElementID: idx, Text: "first"}); !res.Success {
		t.Fatalf("index fill: %+v", res)
	}
	if err := chromedp.Run(c.current(), chromedp.Evaluate(`document.getElementById('q-labelled').outerHTML = '<input id="q-labelled" name="labelled" aria-label="Labelled answer">'`, nil)); err != nil {
		t.Fatal(err)
	}
	if res := exec(wire.Action{Type: wire.ActionType, ElementID: idx, Text: "rerendered"}); !res.Success {
		t.Fatalf("same field rerender: %+v", res)
	}
	if err := chromedp.Run(c.current(), chromedp.Evaluate(`document.body.insertAdjacentHTML('afterbegin', '<input id="new" name="new">')`, nil)); err != nil {
		t.Fatal(err)
	}
	if res := exec(wire.Action{Type: wire.ActionType, ElementID: idx, Text: "wrong"}); res.Success || !strings.Contains(res.Error, "guard:fill-mismatch") {
		t.Fatalf("shifted index: %+v", res)
	}
}

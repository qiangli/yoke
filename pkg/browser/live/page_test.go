package live

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

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

// The page-side core (extension/page.js) is exercised here against
// local fixture pages in a headless Chrome. It is loaded into an
// ISOLATED world, exactly as chrome.scripting.executeScript injects
// it, so main-world framework hooks (React's value tracker, isTrusted
// checks) behave as they do under the extension.
//
// Launches a real Chrome, so it is opt-in like the solo live test:
// BASHY_BROWSER_LIVE=1 go test ./pkg/browser/live -run TestPage
// BASHY_PAGE_JS=<file> swaps in another core (used to show red on the
// pre-fix logic).

type pageHarness struct {
	t      *testing.T
	ctx    context.Context
	srv    *httptest.Server
	script string
}

func newPageHarness(t *testing.T, fixtures map[string]string) *pageHarness {
	t.Helper()
	if os.Getenv("BASHY_BROWSER_LIVE") == "" {
		t.Skip("live Chrome test disabled; set BASHY_BROWSER_LIVE=1 to run (launches a headless Chrome)")
	}
	script := extensionFile(t, "page.js")
	if alt := os.Getenv("BASHY_PAGE_JS"); alt != "" {
		b, err := os.ReadFile(alt)
		if err != nil {
			t.Fatal(err)
		}
		script = string(b)
	}
	mux := http.NewServeMux()
	for path, body := range fixtures {
		body := body
		mux.HandleFunc(path, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, body)
		})
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	opts := append(chromedp.DefaultExecAllocatorOptions[:], chromedp.Headless, chromedp.WindowSize(1200, 900))
	alloc, cancelAlloc := chromedp.NewExecAllocator(context.Background(), opts...)
	ctx, cancelCtx := chromedp.NewContext(alloc)
	ctx, cancelTimeout := context.WithTimeout(ctx, 60*time.Second)
	t.Cleanup(func() { cancelTimeout(); cancelCtx(); cancelAlloc() })
	return &pageHarness{t: t, ctx: ctx, srv: srv, script: script}
}

func (h *pageHarness) open(path string) {
	h.t.Helper()
	if err := chromedp.Run(h.ctx, chromedp.Navigate(h.srv.URL+path)); err != nil {
		h.t.Fatal(err)
	}
}

// call runs __bashyPage[fn](params) in a fresh isolated world of the
// top frame, after loading page.js there, and decodes the result.
func (h *pageHarness) call(fn string, params map[string]any) map[string]any {
	h.t.Helper()
	pj, _ := json.Marshal(params)
	var out map[string]any
	err := chromedp.Run(h.ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		tree, err := page.GetFrameTree().Do(ctx)
		if err != nil {
			return err
		}
		world, err := page.CreateIsolatedWorld(tree.Frame.ID).WithWorldName("bashy-live").Do(ctx)
		if err != nil {
			return err
		}
		if _, exc, err := runtime.Evaluate(h.script).WithContextID(world).Do(ctx); err != nil || exc != nil {
			return fmt.Errorf("load page.js: %v %v", err, exc)
		}
		expr := fmt.Sprintf("globalThis.__bashyPage[%q](%s)", fn, pj)
		res, exc, err := runtime.Evaluate(expr).WithContextID(world).WithAwaitPromise(true).WithReturnByValue(true).Do(ctx)
		if err != nil {
			return err
		}
		if exc != nil {
			return fmt.Errorf("%s threw: %s", fn, exc.Text+" "+string(exc.Exception.Description))
		}
		return json.Unmarshal(res.Value, &out)
	}))
	if err != nil {
		h.t.Fatal(err)
	}
	return out
}

// mainEval reads page state from the MAIN world (what the site sees).
func (h *pageHarness) mainEval(expr string, v any) {
	h.t.Helper()
	if err := chromedp.Run(h.ctx, chromedp.Evaluate(expr, v)); err != nil {
		h.t.Fatal(err)
	}
}

// reactLike is a controlled input wired the way React wires one: an
// instance `value` property feeding a tracker, change detection that
// compares tracker vs node, and a re-render that writes state back.
// strictTrusted additionally ignores untrusted input events, like
// masked inputs and bot-guarded fields do.
const controlledFixture = `<!doctype html><html><body>
<form>
<label>Organization <input id="org" name="org"></label>
<label>Code <input id="code" name="code"></label>
<label>Digits <input id="digits" name="digits"></label>
<input id="plain" name="plain">
</form>
<p id="state"></p>
<script>
const proto = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value');
function controlled(el, opts) {
  let state = '', tracked = '';
  Object.defineProperty(el, 'value', {
    configurable: true,
    get() { return proto.get.call(this); },
    set(v) { tracked = '' + v; proto.set.call(this, v); },
  });
  el.addEventListener('input', (e) => {
    if (opts.trustedOnly && !e.isTrusted) return;
    const cur = proto.get.call(el);
    if (cur === tracked) return;
    tracked = cur;
    state = opts.accept ? opts.accept(cur) : cur;
    window.states[el.id] = state;
  });
  setInterval(() => { if (proto.get.call(el) !== state) el.value = state; }, 10);
}
window.states = {};
controlled(document.getElementById('org'), {});
controlled(document.getElementById('code'), { trustedOnly: true });
controlled(document.getElementById('digits'), { accept: (v) => v.replace(/[^0-9]/g, '') });
</script></body></html>`

func TestPageTypeSticksOnControlledInputs(t *testing.T) {
	h := newPageHarness(t, map[string]string{"/form": controlledFixture})
	h.open("/form")

	for _, id := range []string{"org", "code"} {
		res := h.call("type", map[string]any{"selector": "#" + id, "text": "DHNT Inc."})
		if res["error"] != nil {
			t.Fatalf("type #%s: %v", id, res["error"])
		}
		var state string
		h.mainEval(fmt.Sprintf("window.states[%q] || ''", id), &state)
		if state != "DHNT Inc." {
			t.Fatalf("type #%s reported %v but the app state is %q", id, res["content"], state)
		}
	}
}

func TestPageTypeReportsARejectedValue(t *testing.T) {
	h := newPageHarness(t, map[string]string{"/form": controlledFixture})
	h.open("/form")

	res := h.call("type", map[string]any{"selector": "#digits", "text": "abc"})
	msg, _ := res["error"].(string)
	if !strings.Contains(msg, "did not stick") || !strings.Contains(msg, `field shows ""`) {
		t.Fatalf("a field that rejected the value must be reported, got %v", res)
	}
	if res["retry_trusted"] != true {
		t.Fatalf("a rejected synthetic type must ask background.js for the trusted retry: %v", res)
	}
}

func TestPageClickByIndexFocusesTheField(t *testing.T) {
	h := newPageHarness(t, map[string]string{"/form": controlledFixture})
	h.open("/form")

	ex := h.call("extract", map[string]any{})
	lines := strings.Split(ex["elements"].(string), "\n")
	idx := 0
	for _, l := range lines {
		if strings.Contains(l, `name="plain"`) {
			fmt.Sscanf(l, "[%d]", &idx)
		}
	}
	if idx == 0 {
		t.Fatalf("extract did not list the plain input:\n%s", ex["elements"])
	}
	if res := h.call("click", map[string]any{"element_id": idx}); res["error"] != nil {
		t.Fatal(res["error"])
	}
	var active string
	h.mainEval("document.activeElement && document.activeElement.id", &active)
	if active != "plain" {
		t.Fatalf("click --index %d left focus on %q, want the clicked field", idx, active)
	}
}

// clickTargetsFixture reproduces the AWS Budgets page failure: the
// same words on a nav link, a heading link, a longer link, and the
// intended button; plus a modal whose close control is an icon.
const clickTargetsFixture = `<!doctype html><html><body>
<nav><a href="#nav" id="navlink">Create budget</a></nav>
<main>
<h2><a href="#h" id="headlink">Create budget</a></h2>
<a href="#more" id="morelink">Create budget overview</a>
<p>Close account settings: <button id="closeacct">Close account</button></p>
<button id="create" onclick="window.hit='create'">Create budget</button>
<button class="save" onclick="window.hit='save1'">Save</button>
<button class="save" onclick="window.hit='save2'">Save</button>
</main>
<div role="dialog" aria-modal="true" id="dlg" style="position:fixed;top:10px;right:10px;background:#fff">
  <button id="x" onclick="window.hit='dialog-close'"><svg width="16" height="16"><title>Close</title><path d="M0 0L16 16"/></svg></button>
  <p>Cookie preferences</p>
</div>
<script>window.hit='';</script>
</body></html>`

func TestPageClickTextPicksTheIntendedElement(t *testing.T) {
	h := newPageHarness(t, map[string]string{"/b": clickTargetsFixture})
	h.open("/b")

	// The open dialog wins first: its icon-only close is named "Close".
	if res := h.call("click", map[string]any{"match_text": "close"}); res["error"] != nil {
		t.Fatal(res["error"])
	}
	var hit string
	h.mainEval("window.hit", &hit)
	if hit != "dialog-close" {
		t.Fatalf(`click --text close hit %q, want the dialog's icon close`, hit)
	}
	h.mainEval("document.getElementById('dlg').remove(); window.hit=''", nil)

	if res := h.call("click", map[string]any{"match_text": "Create budget"}); res["error"] != nil {
		t.Fatal(res["error"])
	}
	h.mainEval("window.hit", &hit)
	if hit != "create" {
		t.Fatalf(`click --text "Create budget" hit %q, want the button`, hit)
	}

	res := h.call("click", map[string]any{"match_text": "Save"})
	msg, _ := res["error"].(string)
	if !strings.Contains(msg, "2 elements match") || !strings.Contains(msg, "--index") {
		t.Fatalf("an exact tie must be refused with candidates, got %v", res)
	}
}

func TestPageExtractNamesIconOnlyButtons(t *testing.T) {
	h := newPageHarness(t, map[string]string{"/b": clickTargetsFixture})
	h.open("/b")
	ex := h.call("extract", map[string]any{})
	if !strings.Contains(ex["elements"].(string), "<button >Close</button>") {
		t.Fatalf("icon-only close not named in extract:\n%s", ex["elements"])
	}
}

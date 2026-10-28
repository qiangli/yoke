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

	"github.com/chromedp/cdproto/cdp"
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

	// Site isolation off: a cross-origin iframe then stays in the page's
	// frame tree, where the harness can open a world in it. (The
	// extension reaches out-of-process frames through allFrames.)
	opts := append(chromedp.DefaultExecAllocatorOptions[:], chromedp.Headless, chromedp.WindowSize(1200, 900),
		chromedp.Flag("disable-features", "IsolateOrigins,site-per-process"),
		chromedp.Flag("disable-site-isolation-trials", true))
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
	return h.callIn("", fn, params)
}

// frames lists the tab's frames in tree order (top first), the order
// the extension's frameIds follow for a freshly loaded page.
func (h *pageHarness) frames() []cdp.FrameID {
	h.t.Helper()
	var ids []cdp.FrameID
	err := chromedp.Run(h.ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		tree, err := page.GetFrameTree().Do(ctx)
		if err != nil {
			return err
		}
		var walk func(*page.FrameTree)
		walk = func(n *page.FrameTree) {
			ids = append(ids, n.Frame.ID)
			for _, c := range n.ChildFrames {
				walk(c)
			}
		}
		walk(tree)
		return nil
	}))
	if err != nil {
		h.t.Fatal(err)
	}
	return ids
}

// callIn is call in a given frame ("" = top).
func (h *pageHarness) callIn(frame cdp.FrameID, fn string, params map[string]any) map[string]any {
	h.t.Helper()
	pj, _ := json.Marshal(params)
	var out map[string]any
	err := chromedp.Run(h.ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		if frame == "" {
			tree, err := page.GetFrameTree().Do(ctx)
			if err != nil {
				return err
			}
			frame = tree.Frame.ID
		}
		world, err := page.CreateIsolatedWorld(frame).WithWorldName("bashy-live").Do(ctx)
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

// dropdownFixture: the widget shapes from the Google for Startups and
// AWS Startups forms — a Material-style select that opens on mousedown
// and renders its options in a detached popup, a searchable combobox
// whose options exist only after typing, a radio group whose inputs
// are visually hidden behind styled labels, and a native select.
const dropdownFixture = `<!doctype html><html><head><style>
.vh { position:absolute; opacity:0; width:1px; height:1px; }
.pill { display:inline-block; padding:4px; border:1px solid #888; }
</style></head><body><form>
<div id="industry" role="combobox" aria-haspopup="listbox" aria-controls="industry-pop" aria-expanded="false" tabindex="0" class="pill">Select industry</div>
<label for="launch">Launch month</label>
<input id="launch" role="combobox" aria-autocomplete="list" aria-controls="launch-pop">
<fieldset id="stage"><legend>Stage</legend>
  <input class="vh" type="radio" name="stage" id="s1" value="idea"><label class="pill" for="s1">Idea</label>
  <input class="vh" type="radio" name="stage" id="s2" value="seed"><label class="pill" for="s2">Pre-seed / Seed</label>
</fieldset>
<select id="country"><option value="">Choose</option><option value="us">United States</option><option value="ie">Ireland</option></select>
</form>
<script>
const ind = document.getElementById('industry');
ind.addEventListener('mousedown', () => {
  if (document.getElementById('industry-pop')) return;
  const ul = document.createElement('ul');
  ul.id = 'industry-pop'; ul.setAttribute('role', 'listbox');
  for (const name of ['Agriculture', 'Developer tools', 'Developer services', 'Fintech']) {
    const li = document.createElement('li'); li.setAttribute('role', 'option'); li.textContent = name;
    li.addEventListener('click', () => { ind.textContent = name; ul.remove(); ind.setAttribute('aria-expanded', 'false'); });
    ul.appendChild(li);
  }
  document.body.appendChild(ul); ind.setAttribute('aria-expanded', 'true');
});
const months = ['January 2027', 'February 2027', 'March 2027'];
const launch = document.getElementById('launch');
launch.addEventListener('input', () => {
  let ul = document.getElementById('launch-pop'); if (ul) ul.remove();
  ul = document.createElement('ul'); ul.id = 'launch-pop'; ul.setAttribute('role', 'listbox');
  for (const m of months.filter((x) => launch.value && x.toLowerCase().startsWith(launch.value.toLowerCase()))) {
    const li = document.createElement('li'); li.setAttribute('role', 'option'); li.textContent = m;
    li.addEventListener('mousedown', (e) => { e.preventDefault(); launch.value = m; ul.remove(); });
    ul.appendChild(li);
  }
  document.body.appendChild(ul);
});
</script></body></html>`

func TestPageSelectCustomWidgets(t *testing.T) {
	h := newPageHarness(t, map[string]string{"/d": dropdownFixture})
	h.open("/d")

	ex := h.call("extract", map[string]any{})
	els := ex["elements"].(string)
	for _, want := range []string{`role="combobox"`, `current="Select industry"`, `>Pre-seed / Seed</label>`, `options="Choose | United States | Ireland"`} {
		if !strings.Contains(els, want) {
			t.Fatalf("extract does not list %s:\n%s", want, els)
		}
	}

	cases := []struct {
		target map[string]any
		option string
		check  string
		want   string
	}{
		{map[string]any{"selector": "#industry"}, "Developer tools", "document.getElementById('industry').textContent", "Developer tools"},
		{map[string]any{"selector": "#launch"}, "March 2027", "document.getElementById('launch').value", "March 2027"},
		{map[string]any{"selector": "#stage"}, "Pre-seed", "document.querySelector('input[name=stage]:checked')?.value || ''", "seed"},
		{map[string]any{"match_text": "Choose"}, "Ireland", "document.getElementById('country').value", "ie"},
	}
	for _, c := range cases {
		p := map[string]any{"option": c.option}
		for k, v := range c.target {
			p[k] = v
		}
		res := h.call("select", p)
		if res["error"] != nil {
			t.Fatalf("select %v %q: %v", c.target, c.option, res["error"])
		}
		var got string
		h.mainEval(c.check, &got)
		if got != c.want {
			t.Fatalf("select %v %q reported %v but the page has %q", c.target, c.option, res["data"], got)
		}
	}

	res := h.call("select", map[string]any{"selector": "#industry", "option": "Biotech"})
	if msg, _ := res["error"].(string); !strings.Contains(msg, "no option") || !strings.Contains(msg, "Fintech") {
		t.Fatalf("a missing option must be an error naming the visible options, got %v", res)
	}
}

// TestPageFramesAreOnePage: the Azure investor-code step lives in a
// cross-origin, sandboxed iframe. Every frame runs page.js; frames.js
// numbers their elements tab-wide and routes an index back to its
// frame — the same code background.js runs.
func TestPageFramesAreOnePage(t *testing.T) {
	inner := `<!doctype html><html><body><p>Investor code</p>
<input id="code" aria-label="Investor code"><button id="go" onclick="window.hit='go'">Apply code</button>
<script>window.hit='';</script></body></html>`
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, inner)
	}))
	defer other.Close()
	top := `<!doctype html><html><body><h1>Offer</h1><button id="top">Continue</button>
<iframe src="/same" width="400" height="120"></iframe>
<iframe sandbox="allow-scripts allow-forms" src="` + other.URL + `/x" width="400" height="160"></iframe>
</body></html>`
	same := `<!doctype html><html><body><a href="#terms">Terms</a></body></html>`
	h := newPageHarness(t, map[string]string{"/top": top, "/same": same})
	h.open("/top")
	time.Sleep(300 * time.Millisecond) // let the iframes load

	ids := h.frames()
	if len(ids) != 3 {
		t.Fatalf("want 3 frames (top, same-origin, cross-origin), got %d", len(ids))
	}
	// Collect per-frame answers the way callFrames does.
	type frameAnswer struct {
		FrameID int            `json:"frameId"`
		Result  map[string]any `json:"result"`
	}
	var extracts, counts []frameAnswer
	for i, id := range ids {
		extracts = append(extracts, frameAnswer{i, h.callIn(id, "extract", map[string]any{"limit": 1000000})})
		counts = append(counts, frameAnswer{i, h.callIn(id, "count", map[string]any{})})
	}
	framesJS := extensionFile(t, "frames.js")
	ej, _ := json.Marshal(extracts)
	cj, _ := json.Marshal(counts)
	var merged struct {
		Elements string `json:"elements"`
		Total    int    `json:"total"`
		Content  string `json:"content"`
		At       struct {
			FrameID int `json:"frameId"`
			Local   int `json:"local"`
		} `json:"at"`
	}
	// Find "Apply code" in the merged listing, then route its index.
	h.mainEval(fmt.Sprintf(`(() => { %s
	  const m = mergeExtract(%s, {});
	  const line = m.elements.split("\n").find((l) => l.includes("Apply code"));
	  const n = line ? parseInt(line.slice(1)) : 0;
	  return { elements: m.elements, total: m.total, content: m.content, at: locateIndex(%s, n) };
	})()`, framesJS, ej, cj), &merged)
	if merged.Total != 4 || !strings.Contains(merged.Elements, "(frame 2)") || !strings.Contains(merged.Content, "Investor code") {
		t.Fatalf("frames not merged into one listing (total %d):\n%s\n%s", merged.Total, merged.Elements, merged.Content)
	}
	if merged.At.FrameID != 2 {
		t.Fatalf("Apply code routed to frame %d (local %d), want the cross-origin frame 2:\n%s", merged.At.FrameID, merged.At.Local, merged.Elements)
	}
	if r := h.callIn(ids[2], "type", map[string]any{"match_text": "Investor code", "text": "DHNT-2026"}); r["error"] != nil {
		t.Fatal(r["error"])
	}
	if r := h.callIn(ids[2], "click", map[string]any{"element_id": merged.At.Local}); r["error"] != nil {
		t.Fatal(r["error"])
	}
	// The text in the top frame is not found there: background.js moves on.
	if r := h.callIn(ids[0], "click", map[string]any{"match_text": "Apply code"}); r["notfound"] != true {
		t.Fatalf("a miss in one frame must be flagged notfound so the next frame is tried: %v", r)
	}
}

package cdpactions

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"

	"github.com/qiangli/yoke/pkg/browser/guard"
	"github.com/qiangli/yoke/pkg/browser/live/extension"
	"github.com/qiangli/yoke/pkg/browser/wire"
)

type extractedIdentity struct {
	Tag      string `json:"tag"`
	Name     string `json:"name"`
	Label    string `json:"label"`
	StableID string `json:"stable_id"`
}

type extractSnapshot struct {
	Scope         string
	IncludeHidden bool
	URL           string
	Fields        map[int]extractedIdentity
}

var extractSnapshots = struct {
	sync.Mutex
	byTarget map[string]extractSnapshot
}{byTarget: make(map[string]extractSnapshot)}

func targetKey(ctx context.Context) string {
	if c := chromedp.FromContext(ctx); c != nil && c.Target != nil {
		return string(c.Target.TargetID)
	}
	return ""
}

func rememberExtract(ctx context.Context, a wire.Action, url, data string) {
	key := targetKey(ctx)
	if key == "" {
		return
	}
	fields := parseExtractIdentities(data)
	extractSnapshots.Lock()
	extractSnapshots.byTarget[key] = extractSnapshot{Scope: a.Scope, IncludeHidden: a.IncludeHidden, URL: url, Fields: fields}
	extractSnapshots.Unlock()
}

func parseExtractIdentities(data string) map[int]extractedIdentity {
	var listing struct {
		Elements []struct {
			Index int `json:"index"`
			extractedIdentity
		} `json:"elements"`
	}
	if json.Unmarshal([]byte(data), &listing) != nil {
		return nil
	}
	fields := make(map[int]extractedIdentity, len(listing.Elements))
	for _, e := range listing.Elements {
		fields[e.Index] = e.extractedIdentity
	}
	return fields
}

// expectedIdentity supplies the extract-time field identity to every CDP
// write and its guard preflight. Never trust a fresh index enumeration alone.
func expectedIdentity(ctx context.Context, a wire.Action) (map[string]any, error) {
	params := map[string]any{"scope": a.Scope, "element_id": a.ElementID, "selector": a.Selector,
		"match_text": a.MatchText, "include_hidden": a.IncludeHidden}
	if a.ElementID <= 0 {
		return params, nil
	}
	key := targetKey(ctx)
	extractSnapshots.Lock()
	snap, ok := extractSnapshots.byTarget[key]
	extractSnapshots.Unlock()
	identity, found := snap.Fields[a.ElementID]
	if !ok || !found || snap.Scope != a.Scope || snap.IncludeHidden != a.IncludeHidden {
		return nil, fmt.Errorf("guard:fill-mismatch: index %d has no matching extract identity", a.ElementID)
	}
	var currentURL string
	if err := chromedp.Run(ctx, chromedp.Location(&currentURL)); err != nil {
		return nil, err
	}
	if currentURL != snap.URL {
		return nil, fmt.Errorf("guard:fill-mismatch: page for index %d changed", a.ElementID)
	}
	params["expected_identity"] = identity
	return params, nil
}

// Run dispatches one wire action to an already-attached chromedp target.
func Run(ctx context.Context, mode string, action wire.Action) (*wire.Result, error) {
	switch action.Type {
	case wire.ActionNavigate:
		return Navigate(ctx, action.URL)
	case wire.ActionClick:
		return Click(ctx, action)
	case wire.ActionType:
		return Type(ctx, action)
	case wire.ActionText:
		return Text(ctx, action)
	case wire.ActionScroll:
		return Scroll(ctx, action)
	case wire.ActionScreenshot:
		return Screenshot(ctx, action)
	case wire.ActionExtract:
		return Extract(ctx, action)
	case wire.ActionBack:
		return Back(ctx)
	case wire.ActionTabs:
		return Tabs(ctx, mode, action)
	case wire.ActionEvaluate:
		return Evaluate(ctx, action.Script)
	case wire.ActionWaitForSelector:
		return WaitForSelector(ctx, action)
	case wire.ActionKeyboardPress:
		return KeyboardPress(ctx, action)
	case wire.ActionCookiesGet:
		return CookiesGet(ctx, action)
	case wire.ActionCapabilities:
		return Capabilities(mode)
	case wire.ActionDispatchEvent:
		return DispatchEvent(ctx, action)
	}
	return &wire.Result{Error: fmt.Sprintf("%s: action %q not supported", mode, action.Type)}, nil
}

func Navigate(ctx context.Context, url string) (*wire.Result, error) {
	if url == "" {
		return &wire.Result{Error: "navigate: url required"}, nil
	}
	var title, currentURL, body string
	err := chromedp.Run(ctx,
		chromedp.Navigate(url),
		chromedp.Title(&title),
		chromedp.Location(&currentURL),
		chromedp.Text("body", &body, chromedp.NodeVisible),
	)
	if err != nil {
		return &wire.Result{Error: err.Error()}, nil
	}
	return &wire.Result{Success: true, Title: title, URL: currentURL, Content: Truncate(body, 16000)}, nil
}

// DefaultActionTimeout bounds every selector-resolving chromedp action
// (click, type, keyboard-press focus, wait-for-selector). chromedp's
// Click/SendKeys/Focus wait for the FIRST match in document order to
// become visible; on a form that keeps hidden steps in the DOM that
// first match never shows, so an unbounded action hangs. An action's
// own TimeoutMs overrides this default.
const DefaultActionTimeout = 5 * time.Second

func actionTimeout(a wire.Action) time.Duration {
	if a.TimeoutMs > 0 {
		return time.Duration(a.TimeoutMs) * time.Millisecond
	}
	return DefaultActionTimeout
}

// hiddenOnlyMessage refuses a selector that matches only hidden
// elements. The text is the live extension's miss verbatim
// (page.js resolveTarget, opts.visibleOnly), so probe/solo report the
// same refusal and the same --include-hidden override.
func hiddenOnlyMessage(selector string) string {
	return fmt.Sprintf("selector %s matches only hidden elements (pass --include-hidden to act on one anyway)", JSString(selector))
}

// resolveSelectorScript reports how sel resolves, as JSON:
// {"state":"visible"} when a match shows, {"state":"hidden"} when every
// match is hidden, {"state":"missing"} when nothing matches, and
// {"state":"bad","reason":...} for an invalid selector.
// querySelectorAll handles comma lists and :not() natively; the shown
// predicate mirrors the live extension's shown().
func resolveSelectorScript(sel string) string {
	return "globalThis.__bashyCDPTraverseFrames=true;\n" + extension.PageJS + "\n" + `(function(){
  var SEL=` + JSString(sel) + `;
  function shown(el){var cs=getComputedStyle(el);if(cs.visibility==="hidden"||cs.display==="none"||cs.opacity==="0")return false;var r=el.getBoundingClientRect();return r.width>0&&r.height>0;}
  var all;
  try { all = globalThis.__bashyPage.resolveSelector(document, SEL); }
  catch (e) { return JSON.stringify({state:"bad", reason:String((e && e.message) || e)}); }
  if (!all.length) return JSON.stringify({state:"missing"});
  for (var i=0; i<all.length; i++) { if (shown(all[i])) return JSON.stringify({state:"visible"}); }
  return JSON.stringify({state:"hidden"});
})()`
}

// targetJSPath is a JS expression (for chromedp.ByJSPath) resolving to
// the element a selector action must act on: the first VISIBLE match,
// or the first match when includeHidden keeps hidden elements. A
// resolveSelector check runs first, so this never sees a missing or
// invalid selector; the fallbacks only cover a DOM that changed
// between the check and the action.
func targetJSPath(sel string, includeHidden bool) string {
	if includeHidden {
		return `(function(){globalThis.__bashyCDPTraverseFrames=true;` + extension.PageJS + `var SEL=` + JSString(sel) + `;try{return globalThis.__bashyPage.resolveSelector(document, SEL)[0] || null;}catch(e){return null;}})()`
	}
	return `(function(){globalThis.__bashyCDPTraverseFrames=true;` + extension.PageJS + `
  var SEL=` + JSString(sel) + `;
  function shown(el){var cs=getComputedStyle(el);if(cs.visibility==="hidden"||cs.display==="none"||cs.opacity==="0")return false;var r=el.getBoundingClientRect();return r.width>0&&r.height>0;}
  var all;
  try { all = globalThis.__bashyPage.resolveSelector(document, SEL); }
  catch (e) { return null; }
  for (var i=0; i<all.length; i++) { if (shown(all[i])) return all[i]; }
  return all[0] || null;
})()`
}

// resolveVisibleTarget checks sel and returns the ByJSPath expression
// for the element to act on, plus whether that element is hidden (true
// only under --include-hidden). A selector with no match, an invalid
// selector, or (unless includeHidden) one matching only hidden
// elements is an error, never a hang.
func resolveVisibleTarget(ctx context.Context, sel string, includeHidden bool) (path string, hidden bool, err error) {
	var raw string
	if err := chromedp.Run(ctx, chromedp.Evaluate(resolveSelectorScript(sel), &raw)); err != nil {
		return "", false, err
	}
	var r struct {
		State  string `json:"state"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		return "", false, fmt.Errorf("selector %s: cannot resolve (%s)", JSString(sel), strings.TrimSpace(raw))
	}
	switch r.State {
	case "visible":
		return targetJSPath(sel, includeHidden), false, nil
	case "hidden":
		if includeHidden {
			return targetJSPath(sel, includeHidden), true, nil
		}
		return "", false, fmt.Errorf("%s", hiddenOnlyMessage(sel))
	case "missing":
		return "", false, fmt.Errorf("no element matches selector %s", JSString(sel))
	case "bad":
		if r.Reason != "" {
			return "", false, fmt.Errorf("%s is not a valid CSS selector: %s", JSString(sel), r.Reason)
		}
		return "", false, fmt.Errorf("%s is not a valid CSS selector", JSString(sel))
	}
	return "", false, fmt.Errorf("selector %s: cannot resolve (%s)", JSString(sel), strings.TrimSpace(raw))
}

// clickHiddenNode clicks an element chromedp's visibility-gated Click
// would wait on forever: under --include-hidden the operator accepted
// a hidden target, so a DOM click dispatches without a box.
func clickHiddenNode(ctx context.Context, path string) error {
	script := `(function(){var el=(` + path + `);if(!el)return false;el.click();return true;})()`
	var ok bool
	if err := chromedp.Run(ctx, chromedp.Evaluate(script, &ok)); err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("click: element is gone")
	}
	return nil
}

// typeHiddenNode fills an element chromedp's visibility-gated SendKeys
// would wait on forever: under --include-hidden the operator accepted
// a hidden target, so the value is set in the DOM with the input and
// change events a listener expects.
func typeHiddenNode(ctx context.Context, path, text string) error {
	script := `(function(){var el=(` + path + `);var TEXT=` + JSString(text) + `;if(!el)return "missing";
	try{
		if("value" in el && !el.isContentEditable){el.value=TEXT;}
		else if(el.isContentEditable){el.focus();document.execCommand("insertText",false,TEXT);}
		else{el.textContent=TEXT;}
		el.dispatchEvent(new Event("input",{bubbles:true}));
		el.dispatchEvent(new Event("change",{bubbles:true}));
		return "ok";
	}catch(e){return "error:"+String((e&&e.message)||e);}})()`
	var out string
	if err := chromedp.Run(ctx, chromedp.Evaluate(script, &out)); err != nil {
		return err
	}
	if out != "ok" {
		return fmt.Errorf("type: cannot fill hidden element (%s)", out)
	}
	return nil
}

// focusHiddenNode focuses an element chromedp's Focus would park on:
// under --include-hidden the operator accepted a hidden target. DOM
// focus on a non-rendered element is a silent no-op, so existing is
// enough for the follow-up key press to be dispatched.
func focusHiddenNode(ctx context.Context, path string) error {
	script := `(function(){var el=(` + path + `);if(!el)return false;try{el.focus();}catch(e){}return true;})()`
	var ok bool
	if err := chromedp.Run(ctx, chromedp.Evaluate(script, &ok)); err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("keyboard_press: element is gone")
	}
	return nil
}

// Click resolves by element index, CSS selector, or visible text — in
// that order — and every form returns the SAME envelope on success:
// the OBSERVED effect (`content: "clicked: navigated" | "clicked:
// dialog" | "clicked: dom-changed" | "clicked: no-effect"`, with
// data.effect naming it). A click that resolved nothing is an error,
// never a success; a click that resolved but observably did nothing
// reports no-effect (the click happened; the page did not react
// within the bounded watch).
func Click(ctx context.Context, a wire.Action) (*wire.Result, error) {
	if a.ElementID > 0 {
		params, err := expectedIdentity(ctx, a)
		if err != nil {
			return &wire.Result{Error: err.Error()}, nil
		}
		return clickPage(ctx, params)
	}
	if a.Selector != "" {
		wctx, cancel := context.WithTimeout(ctx, actionTimeout(a))
		defer cancel()
		params, _ := expectedIdentity(wctx, a)
		return clickPage(wctx, params)
	}
	if a.MatchText != "" {
		params, _ := expectedIdentity(ctx, a)
		return clickPage(ctx, params)
	}
	return &wire.Result{Error: "click: selector, element index, or match_text required"}, nil
}

// clickPage runs page.js click — which watches the page for a bounded
// moment after dispatching — and reports the observed outcome.
func clickPage(ctx context.Context, params map[string]any) (*wire.Result, error) {
	var out struct {
		Content string `json:"content"`
		Data    string `json:"data"`
		Error   string `json:"error"`
	}
	if err := evalGuard(ctx, pageCall("click", params), &out); err != nil {
		return &wire.Result{Error: err.Error()}, nil
	}
	if out.Error != "" {
		return &wire.Result{Error: out.Error}, nil
	}
	if out.Content == "" {
		out.Content = "clicked: no-effect"
	}
	res := &wire.Result{Success: true, Content: out.Content}
	if out.Data != "" {
		res.Data = out.Data
	}
	return res, nil
}

// ClickByIndexScript mirrors ExtractScript's enumeration exactly — same
// landmark skip, same visibility filter — so [N] in an extract listing
// is the element this clicks.
func ClickByIndexScript(a wire.Action) string {
	return extension.PageJS + "\n" + `globalThis.__bashyPage.click({scope:` + JSString(a.Scope) + `,element_id:` + fmt.Sprintf("%d", a.ElementID) + `,include_hidden:` + fmt.Sprintf("%v", a.IncludeHidden) + `})`
}

func pageCall(method string, params any) string {
	b, _ := json.Marshal(params)
	return extension.PageJS + "\n" + "globalThis.__bashyPage." + method + "(" + string(b) + ")"
}

// DispatchEvent fires a named DOM event. Over CDP this is a genuine
// remedy for a page whose Content-Security-Policy omits 'unsafe-eval':
// Runtime.evaluate is not subject to the page's script-src.
func DispatchEvent(ctx context.Context, a wire.Action) (*wire.Result, error) {
	if strings.TrimSpace(a.Event) == "" {
		return &wire.Result{Error: "dispatch_event: event name required"}, nil
	}
	script := "globalThis.__bashyCDPTraverseFrames=true;\n" + extension.PageJS + "\n" + `(function(){
  var NAME=` + JSString(a.Event) + `, DETAIL=` + JSString(a.Detail) + `, SEL=` + JSString(a.Selector) + `;
  var detail = null;
  if (DETAIL) { try { detail = JSON.parse(DETAIL); } catch (e) { detail = DETAIL; } }
  var target = window, where = "window";
  if (SEL === "document") { target = document; where = "document"; }
  else if (SEL) {
    try { target = globalThis.__bashyPage.resolveSelector(document, SEL)[0]; } catch (e) { return JSON.stringify({error:"dispatch_event: "+SEL+" is not a valid selector"}); }
    if (!target) return JSON.stringify({error:"dispatch_event: no element matches "+SEL});
    where = SEL;
  }
  var ev = detail === null ? new Event(NAME,{bubbles:true,cancelable:true})
                           : new CustomEvent(NAME,{bubbles:true,cancelable:true,detail:detail});
  var ok = target.dispatchEvent(ev);
  return JSON.stringify({target:where, default_prevented: !ok});
})()`
	var raw string
	if err := chromedp.Run(ctx, chromedp.Evaluate(script, &raw)); err != nil {
		return &wire.Result{Error: err.Error()}, nil
	}
	var inner struct {
		Error            string `json:"error"`
		Target           string `json:"target"`
		DefaultPrevented bool   `json:"default_prevented"`
	}
	_ = json.Unmarshal([]byte(raw), &inner)
	if inner.Error != "" {
		return &wire.Result{Error: inner.Error}, nil
	}
	return &wire.Result{
		Success: true,
		Content: fmt.Sprintf("dispatched %s on %s", a.Event, inner.Target),
		Data:    raw,
	}, nil
}

func ClickByTextScript(matchText, scope string) string {
	return `(function(){
  var want = ` + JSString(matchText) + `.toLowerCase();
  var scope = ` + JSString(scope) + `;
  var root = scope ? document.querySelector(scope) : document;
  if (!root) return false;
  var nodes = root.querySelectorAll("a, button, input[type=button], input[type=submit], [role='button'], [role='link']");
  for (var i=0; i<nodes.length; i++) {
    var n = nodes[i];
    var v = ((n.innerText) || n.value || n.getAttribute("aria-label") || "").trim().toLowerCase();
    if (v.indexOf(want) >= 0) { n.click(); return true; }
  }
  return false;
})()`
}

func Type(ctx context.Context, a wire.Action) (*wire.Result, error) {
	if a.ElementID > 0 {
		params, err := expectedIdentity(ctx, a)
		if err != nil {
			return &wire.Result{Error: err.Error()}, nil
		}
		params["text"] = a.Text
		var out struct {
			Content string `json:"content"`
			Error   string `json:"error"`
		}
		expr := pageCall("type", params)
		if err := evalGuard(ctx, expr, &out); err != nil {
			return &wire.Result{Error: err.Error()}, nil
		}
		if out.Error != "" {
			return &wire.Result{Error: out.Error}, nil
		}
		return &wire.Result{Success: true, Content: "typed"}, nil
	}
	if a.Selector == "" {
		return &wire.Result{Error: "type: selector required"}, nil
	}
	wctx, cancel := context.WithTimeout(ctx, actionTimeout(a))
	defer cancel()
	params, _ := expectedIdentity(wctx, a)
	params["text"] = a.Text
	var out struct {
		Error string `json:"error"`
	}
	if err := evalGuard(wctx, pageCall("type", params), &out); err != nil {
		return &wire.Result{Error: err.Error()}, nil
	}
	if out.Error != "" {
		return &wire.Result{Error: out.Error}, nil
	}
	return &wire.Result{Success: true, Content: "typed"}, nil
}

// Scroll moves the window, or — when the action addresses an element —
// the element's nearest scrollable ancestor (an inner panel, not the
// window behind it). The addressed form runs page.js scroll in the
// guard's isolated world, so the shared selector syntax applies.
func Scroll(ctx context.Context, a wire.Action) (*wire.Result, error) {
	if a.Selector == "" && a.ElementID == 0 && a.MatchText == "" {
		amount := a.Amount
		if amount == 0 {
			amount = 500
		}
		if a.Direction == "up" {
			amount = -amount
		}
		script := fmt.Sprintf("window.scrollBy(0, %d); window.scrollY", amount)
		var y float64
		if err := chromedp.Run(ctx, chromedp.Evaluate(script, &y)); err != nil {
			return &wire.Result{Error: err.Error()}, nil
		}
		return &wire.Result{Success: true, Data: fmt.Sprintf("scrollY=%g", y)}, nil
	}
	params := map[string]any{"scope": a.Scope, "element_id": a.ElementID, "selector": a.Selector,
		"match_text": a.MatchText, "include_hidden": a.IncludeHidden,
		"direction": a.Direction, "amount": a.Amount}
	var out struct {
		Content string `json:"content"`
		Data    string `json:"data"`
		Error   string `json:"error"`
	}
	if err := evalGuard(ctx, pageCall("scroll", params), &out); err != nil {
		return &wire.Result{Error: err.Error()}, nil
	}
	if out.Error != "" {
		return &wire.Result{Error: out.Error}, nil
	}
	return &wire.Result{Success: true, Content: out.Content, Data: out.Data}, nil
}

func Screenshot(ctx context.Context, a wire.Action) (*wire.Result, error) {
	// Let the renderer paint what the DOM already reports before
	// capturing, so an image taken right after a click is not the
	// pre-click frame.
	settle := a.SettleMs
	if settle == 0 {
		settle = 150
	}
	if settle > 0 {
		if err := chromedp.Run(ctx, chromedp.Sleep(time.Duration(settle)*time.Millisecond)); err != nil {
			return &wire.Result{Error: err.Error()}, nil
		}
	}
	var buf []byte
	if a.FullPage {
		// Inner scroll areas show only their visible slice in a
		// beyond-viewport capture, so a page with panels gets its
		// regions stitched tile by tile (positions restored even on
		// failure — the stitch's own defer). Without panels the single
		// capture below is exact; a failed stitch falls back to it too
		// (the meta names the path taken, so a silent downgrade reads
		// as one — the same fallback the live extension takes).
		if stitched, ok, err := StitchFullPage(ctx); err != nil {
			return finishFullPageFallback(ctx, a, err)
		} else if ok {
			buf = stitched
			return finishCapture(a, buf, captureMetaStitched())
		}
		// quality 100 selects PNG in chromedp; anything else silently
		// yields a JPEG, which would write JPEG bytes to a .png path.
		if err := chromedp.Run(ctx, chromedp.FullScreenshot(&buf, 100)); err != nil {
			return &wire.Result{Error: err.Error()}, nil
		}
		return finishCapture(a, buf, captureMetaFullPage())
	}
	if err := chromedp.Run(ctx, chromedp.CaptureScreenshot(&buf)); err != nil {
		return &wire.Result{Error: err.Error()}, nil
	}
	return finishCapture(a, buf, "")
}

// finishCapture applies SavePath / MaxBytes to a finished PNG,
// honouring the existing output contract in one place.
func finishCapture(a wire.Action, buf []byte, meta string) (*wire.Result, error) {
	if a.SavePath != "" {
		path, err := writeScreenshot(buf, a.SavePath)
		if err != nil {
			return &wire.Result{Error: err.Error()}, nil
		}
		return &wire.Result{Success: true, Path: path, Data: meta}, nil
	}
	img := base64.StdEncoding.EncodeToString(buf)
	if a.MaxBytes > 0 && len(img) > a.MaxBytes {
		path, err := writeScreenshot(buf, "")
		if err != nil {
			return &wire.Result{Error: err.Error()}, nil
		}
		return &wire.Result{Success: true, Path: path, Data: meta}, nil
	}
	return &wire.Result{Success: true, Image: img, Data: meta}, nil
}

func writeScreenshot(buf []byte, savePath string) (string, error) {
	path := savePath
	if path == "" {
		f, err := os.CreateTemp("", "bashy-browser-*.png")
		if err != nil {
			return "", err
		}
		path = f.Name()
		if _, err := f.Write(buf); err != nil {
			_ = f.Close()
			return "", err
		}
		return path, f.Close()
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return "", err
	}
	return abs, os.WriteFile(abs, buf, 0o644)
}

func Extract(ctx context.Context, a wire.Action) (*wire.Result, error) {
	var title, url string
	if err := chromedp.Run(ctx, chromedp.Title(&title), chromedp.Location(&url)); err != nil {
		return &wire.Result{Error: err.Error()}, nil
	}
	// Evaluated in the guard's isolated world (guard.go): the listing
	// asks guard.js which fields are guarded and never prints their values.
	var raw string
	if err := evalGuard(ctx, ExtractScript(a), &raw); err != nil {
		return &wire.Result{Error: err.Error()}, nil
	}
	out := ParseExtractPayload(raw)
	out.Title = title
	out.URL = url
	if out.Success {
		rememberExtract(ctx, a, url, out.Data)
	}
	return out, nil
}

func ExtractScript(a wire.Action) string {
	scope := JSString(a.Scope)
	match := JSString(a.MatchText)
	if match == `""` && a.Goal != "" {
		match = JSString(a.Goal)
	}
	limit := a.Limit
	if limit <= 0 {
		limit = 50
	}
	offset := a.Offset
	if offset < 0 {
		offset = 0
	}
	return extension.PageJS + "\n" + `var SCOPE_SEL=` + scope + `, MATCH=` + match + `, LIMIT=` + fmt.Sprintf("%d", limit) + `, OFFSET=` + fmt.Sprintf("%d", offset) + `, INCLUDE_HIDDEN=` + fmt.Sprintf("%v", a.IncludeHidden) + `;` + "\n" + `JSON.stringify(globalThis.__bashyPage.extract({scope:SCOPE_SEL,match_text:MATCH,limit:LIMIT,offset:OFFSET,include_hidden:INCLUDE_HIDDEN}))`
}

func ParseExtractPayload(raw string) *wire.Result {
	var inner struct {
		Content   string         `json:"content"`
		Elements  string         `json:"elements"`
		Total     int            `json:"total"`
		Truncated bool           `json:"truncated"`
		Error     string         `json:"error"`
		Viewport  *wire.Viewport `json:"viewport"`
		Data      string         `json:"data"`
	}
	_ = json.Unmarshal([]byte(raw), &inner)
	if inner.Error != "" {
		return &wire.Result{Error: inner.Error}
	}
	return &wire.Result{Success: true, Content: inner.Content, Elements: inner.Elements,
		Total: inner.Total, Truncated: inner.Truncated, Viewport: inner.Viewport, Data: inner.Data}
}

func Back(ctx context.Context) (*wire.Result, error) {
	if err := chromedp.Run(ctx, chromedp.NavigateBack()); err != nil {
		return &wire.Result{Error: err.Error()}, nil
	}
	return &wire.Result{Success: true}, nil
}

func Tabs(ctx context.Context, mode string, a wire.Action) (*wire.Result, error) {
	switch a.TabAction {
	case "list", "":
		targets, err := chromedp.Targets(ctx)
		if err != nil {
			return &wire.Result{Error: err.Error()}, nil
		}
		type rec struct {
			Index int    `json:"index"`
			ID    string `json:"id"`
			Title string `json:"title"`
			URL   string `json:"url"`
		}
		var b strings.Builder
		records := make([]rec, 0, len(targets))
		for i, t := range targets {
			if a.MatchURL != "" && !strings.Contains(strings.ToLower(t.URL), strings.ToLower(a.MatchURL)) {
				continue
			}
			if a.MatchTitle != "" && !strings.Contains(strings.ToLower(t.Title), strings.ToLower(a.MatchTitle)) {
				continue
			}
			records = append(records, rec{Index: i + 1, ID: t.TargetID.String(), Title: t.Title, URL: t.URL})
			fmt.Fprintf(&b, "[%d] %s\n    %s\n", i+1, t.Title, t.URL)
		}
		data, _ := json.Marshal(records)
		return &wire.Result{Success: true, Content: b.String(), Data: string(data), Total: len(records)}, nil
	}
	// Naming the valid set is the whole point: the error already knows
	// the action is invalid, so it knows what would have been valid.
	// Discovering `list|switch|new|close` used to take eight guesses.
	return &wire.Result{Error: fmt.Sprintf(
		"%s: unknown tab action %q; %s supports: list (switch, new, close are live-mode only)",
		mode, a.TabAction, mode)}, nil
}

func Evaluate(ctx context.Context, script string) (*wire.Result, error) {
	if script == "" {
		return &wire.Result{Error: "evaluate: argument 'script' is required (alias 'expression' also accepted)"}, nil
	}
	// Raw password-input values never leave the browser context: collect them
	// (guard's isolated world) around the evaluation and redact the result.
	secrets, err := passwordValues(ctx)
	if err != nil {
		return &wire.Result{Error: "evaluate: " + err.Error()}, nil
	}
	var out any
	if err := chromedp.Run(ctx, chromedp.Evaluate(script, &out)); err != nil {
		return &wire.Result{Error: err.Error()}, nil
	}
	after, err := passwordValues(ctx)
	if err != nil {
		return &wire.Result{Error: "evaluate: " + err.Error()}, nil
	}
	out = guard.RedactSecrets(out, append(secrets, after...))
	return &wire.Result{Success: true, Data: fmt.Sprintf("%v", out)}, nil
}

func WaitForSelector(ctx context.Context, a wire.Action) (*wire.Result, error) {
	if a.Selector == "" {
		return &wire.Result{Error: "wait_for_selector: selector required"}, nil
	}
	timeout := actionTimeout(a)
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	state := strings.ToLower(a.State)
	var err error
	switch state {
	case "", "visible":
		state = "visible"
		err = waitForVisibleMatch(wctx, a.Selector, a.IncludeHidden, timeout)
	case "attached":
		err = waitForPresenceMatch(wctx, a.Selector, true, timeout)
	case "detached":
		err = waitForPresenceMatch(wctx, a.Selector, false, timeout)
	default:
		return &wire.Result{Error: fmt.Sprintf("wait_for_selector: unknown state %q (visible|attached|detached)", a.State)}, nil
	}
	if err != nil {
		return &wire.Result{Error: err.Error()}, nil
	}
	return &wire.Result{Success: true, Data: fmt.Sprintf("state=%s", state)}, nil
}

func waitForPresenceMatch(ctx context.Context, sel string, attached bool, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		var raw string
		if err := chromedp.Run(ctx, chromedp.Evaluate(resolveSelectorScript(sel), &raw)); err != nil {
			if ctx.Err() != nil {
				break
			}
			return err
		}
		var r struct {
			State  string `json:"state"`
			Reason string `json:"reason"`
		}
		if err := json.Unmarshal([]byte(raw), &r); err != nil {
			return err
		}
		if r.State == "bad" {
			return fmt.Errorf("%s is not a valid selector: %s", JSString(sel), r.Reason)
		}
		present := r.State == "visible" || r.State == "hidden"
		if present == attached {
			return nil
		}
		if ctx.Err() != nil || time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(waitPollInterval):
		}
	}
	state := "attached"
	if !attached {
		state = "detached"
	}
	return fmt.Errorf("wait_for_selector: selector %s did not become %s within %s", JSString(sel), state, timeout)
}

// waitPollInterval is the re-check cadence of waitForVisibleMatch: fast
// enough that an appearing element resolves promptly, sparse enough to
// stay out of the renderer's way.
const waitPollInterval = 100 * time.Millisecond

// waitForVisibleMatch polls until sel has a visible match — or any match
// when includeHidden keeps hidden elements — so a hidden first match
// never parks the wait on a node that will never show. At the deadline
// a hidden-only selector is refused with the live message; a selector
// with no match times out. An invalid selector fails immediately.
func waitForVisibleMatch(ctx context.Context, sel string, includeHidden bool, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	last := ""
	for {
		var raw string
		if err := chromedp.Run(ctx, chromedp.Evaluate(resolveSelectorScript(sel), &raw)); err != nil {
			if ctx.Err() != nil {
				break
			}
			return err
		}
		var r struct {
			State  string `json:"state"`
			Reason string `json:"reason"`
		}
		if err := json.Unmarshal([]byte(raw), &r); err != nil {
			return fmt.Errorf("selector %s: cannot resolve (%s)", JSString(sel), strings.TrimSpace(raw))
		}
		switch r.State {
		case "visible":
			return nil
		case "hidden":
			if includeHidden {
				return nil
			}
			last = "hidden"
		case "missing":
			last = "missing"
		case "bad":
			if r.Reason != "" {
				return fmt.Errorf("%s is not a valid CSS selector: %s", JSString(sel), r.Reason)
			}
			return fmt.Errorf("%s is not a valid CSS selector", JSString(sel))
		default:
			last = r.State
		}
		if ctx.Err() != nil || time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(waitPollInterval):
		}
	}
	if last == "hidden" && !includeHidden {
		return fmt.Errorf("%s", hiddenOnlyMessage(sel))
	}
	if includeHidden {
		return fmt.Errorf("wait_for_selector: no match for selector %s within %s", JSString(sel), timeout)
	}
	return fmt.Errorf("wait_for_selector: no visible match for selector %s within %s", JSString(sel), timeout)
}

func KeyboardPress(ctx context.Context, a wire.Action) (*wire.Result, error) {
	if a.Key == "" {
		return &wire.Result{Error: "keyboard_press: key required"}, nil
	}
	wctx, cancel := context.WithTimeout(ctx, actionTimeout(a))
	defer cancel()
	actions := []chromedp.Action{}
	if a.Selector != "" {
		path, hidden, err := resolveVisibleTarget(wctx, a.Selector, a.IncludeHidden)
		if err != nil {
			return &wire.Result{Error: err.Error()}, nil
		}
		if hidden {
			if err := focusHiddenNode(wctx, path); err != nil {
				return &wire.Result{Error: err.Error()}, nil
			}
		} else {
			actions = append(actions, chromedp.Focus(path, chromedp.ByJSPath))
		}
	}
	actions = append(actions, chromedp.KeyEvent(KeyFor(a.Key)))
	if err := chromedp.Run(wctx, actions...); err != nil {
		return &wire.Result{Error: err.Error()}, nil
	}
	return &wire.Result{Success: true, Data: "pressed=" + a.Key}, nil
}

func KeyFor(k string) string {
	switch strings.ToLower(k) {
	case "enter", "return":
		return kb.Enter
	case "tab":
		return kb.Tab
	case "escape", "esc":
		return kb.Escape
	case "backspace":
		return kb.Backspace
	case "delete":
		return kb.Delete
	case "arrowup", "up":
		return kb.ArrowUp
	case "arrowdown", "down":
		return kb.ArrowDown
	case "arrowleft", "left":
		return kb.ArrowLeft
	case "arrowright", "right":
		return kb.ArrowRight
	case "home":
		return kb.Home
	case "end":
		return kb.End
	case "pageup":
		return kb.PageUp
	case "pagedown":
		return kb.PageDown
	}
	return k
}

func CookiesGet(ctx context.Context, a wire.Action) (*wire.Result, error) {
	var cookies []*network.Cookie
	err := chromedp.Run(ctx, chromedp.ActionFunc(func(c context.Context) error {
		_ = network.Enable().Do(c)
		var inner error
		cookies, inner = network.GetCookies().Do(c)
		return inner
	}))
	if err != nil {
		return &wire.Result{Error: err.Error()}, nil
	}
	out := FilterCookies(cookies, a.Name, a.Domain)
	raw, _ := json.Marshal(out)
	return &wire.Result{Success: true, Data: string(raw)}, nil
}

type CookieView struct {
	Name           string  `json:"name"`
	Value          string  `json:"value"`
	Domain         string  `json:"domain"`
	Path           string  `json:"path"`
	Secure         bool    `json:"secure"`
	HTTPOnly       bool    `json:"httpOnly"`
	Session        bool    `json:"session"`
	SameSite       string  `json:"sameSite,omitempty"`
	ExpirationDate float64 `json:"expirationDate,omitempty"`
}

func FilterCookies(cookies []*network.Cookie, wantName, wantDomain string) []CookieView {
	out := make([]CookieView, 0, len(cookies))
	for _, c := range cookies {
		if wantName != "" && c.Name != wantName {
			continue
		}
		if wantDomain != "" {
			cd := strings.TrimPrefix(c.Domain, ".")
			if cd != wantDomain && !strings.HasSuffix(wantDomain, "."+cd) && !strings.HasSuffix(cd, "."+wantDomain) {
				continue
			}
		}
		out = append(out, CookieView{
			Name: c.Name, Value: c.Value, Domain: c.Domain, Path: c.Path,
			Secure: c.Secure, HTTPOnly: c.HTTPOnly, Session: c.Session,
			SameSite: string(c.SameSite), ExpirationDate: c.Expires,
		})
	}
	return out
}

func Capabilities(mode string) (*wire.Result, error) {
	caps := map[string]any{
		"mode": mode,
		"methods": []string{
			wire.ActionNavigate,
			wire.ActionClick,
			wire.ActionType,
			wire.ActionScroll,
			wire.ActionScreenshot,
			wire.ActionExtract,
			wire.ActionText,
			wire.ActionBack,
			wire.ActionTabs,
			wire.ActionEvaluate,
			wire.ActionWaitForSelector,
			wire.ActionKeyboardPress,
			wire.ActionCookiesGet,
			wire.ActionCapabilities,
			wire.ActionDispatchEvent,
		},
	}
	raw, _ := json.Marshal(caps)
	return &wire.Result{Success: true, Data: string(raw)}, nil
}

func JSString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func Truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n... (truncated)"
}

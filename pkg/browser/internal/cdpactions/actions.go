package cdpactions

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"

	"github.com/qiangli/yoke/pkg/browser/wire"
)

// Run dispatches one wire action to an already-attached chromedp target.
func Run(ctx context.Context, mode string, action wire.Action) (*wire.Result, error) {
	switch action.Type {
	case wire.ActionNavigate:
		return Navigate(ctx, action.URL)
	case wire.ActionClick:
		return Click(ctx, action)
	case wire.ActionType:
		return Type(ctx, action)
	case wire.ActionScroll:
		return Scroll(ctx, action.Direction, action.Amount)
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

// Click resolves by element index, CSS selector, or visible text — in
// that order — and every form returns the SAME envelope on success
// (`content: "clicked"`). The shapes used to differ, and the shape was
// the only signal that a form had silently resolved nothing.
func Click(ctx context.Context, a wire.Action) (*wire.Result, error) {
	if a.ElementID > 0 {
		var out any
		if err := chromedp.Run(ctx, chromedp.Evaluate(ClickByIndexScript(a), &out)); err != nil {
			return &wire.Result{Error: err.Error()}, nil
		}
		if b, ok := out.(bool); ok && b {
			return &wire.Result{Success: true, Content: "clicked"}, nil
		}
		return &wire.Result{Error: fmt.Sprintf("click: element index %d out of range for the current extract enumeration", a.ElementID)}, nil
	}
	if a.Selector != "" {
		if err := chromedp.Run(ctx, chromedp.Click(a.Selector, chromedp.ByQuery)); err != nil {
			return &wire.Result{Error: err.Error()}, nil
		}
		return &wire.Result{Success: true, Content: "clicked"}, nil
	}
	if a.MatchText != "" {
		var out any
		if err := chromedp.Run(ctx, chromedp.Evaluate(ClickByTextScript(a.MatchText, a.Scope), &out)); err != nil {
			return &wire.Result{Error: err.Error()}, nil
		}
		if b, ok := out.(bool); ok && b {
			return &wire.Result{Success: true, Content: "clicked"}, nil
		}
		return &wire.Result{Error: fmt.Sprintf("click: no element whose text contains %q", a.MatchText)}, nil
	}
	return &wire.Result{Error: "click: selector, element index, or match_text required"}, nil
}

// ClickByIndexScript mirrors ExtractScript's enumeration exactly — same
// landmark skip, same visibility filter — so [N] in an extract listing
// is the element this clicks.
func ClickByIndexScript(a wire.Action) string {
	return `(function(){
  var SCOPE_SEL=` + JSString(a.Scope) + `, IDX=` + fmt.Sprintf("%d", a.ElementID) + `, INCLUDE_HIDDEN=` + fmt.Sprintf("%v", a.IncludeHidden) + `;
  var root = SCOPE_SEL ? document.querySelector(SCOPE_SEL) : document;
  if (!root) return false;
  var navFilter = !SCOPE_SEL;
  function shown(el){var cs=getComputedStyle(el);if(cs.visibility==="hidden"||cs.display==="none"||cs.opacity==="0")return false;var r=el.getBoundingClientRect();return r.width>0&&r.height>0;}
  var all = root.querySelectorAll("a, button, input, select, textarea, [role='button'], [role='link']");
  var flat = [];
  for (var i=0;i<all.length;i++){
    var el=all[i];
    if (navFilter && el.closest && el.closest("nav, aside, [role='navigation'], [role='complementary']")) continue;
    if (!INCLUDE_HIDDEN && !shown(el)) continue;
    flat.push(el);
  }
  var t = flat[IDX-1];
  if (!t) return false;
  t.click();
  return true;
})()`
}

// DispatchEvent fires a named DOM event. Over CDP this is a genuine
// remedy for a page whose Content-Security-Policy omits 'unsafe-eval':
// Runtime.evaluate is not subject to the page's script-src.
func DispatchEvent(ctx context.Context, a wire.Action) (*wire.Result, error) {
	if strings.TrimSpace(a.Event) == "" {
		return &wire.Result{Error: "dispatch_event: event name required"}, nil
	}
	script := `(function(){
  var NAME=` + JSString(a.Event) + `, DETAIL=` + JSString(a.Detail) + `, SEL=` + JSString(a.Selector) + `;
  var detail = null;
  if (DETAIL) { try { detail = JSON.parse(DETAIL); } catch (e) { detail = DETAIL; } }
  var target = window, where = "window";
  if (SEL === "document") { target = document; where = "document"; }
  else if (SEL) {
    try { target = document.querySelector(SEL); } catch (e) { return JSON.stringify({error:"dispatch_event: "+SEL+" is not a valid CSS selector"}); }
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
	if a.Selector == "" {
		return &wire.Result{Error: "type: selector required"}, nil
	}
	if err := chromedp.Run(ctx, chromedp.SendKeys(a.Selector, a.Text, chromedp.ByQuery)); err != nil {
		return &wire.Result{Error: err.Error()}, nil
	}
	return &wire.Result{Success: true, Content: "typed"}, nil
}

func Scroll(ctx context.Context, direction string, amount int) (*wire.Result, error) {
	if amount == 0 {
		amount = 500
	}
	if direction == "up" {
		amount = -amount
	}
	script := fmt.Sprintf("window.scrollBy(0, %d); window.scrollY", amount)
	var y float64
	if err := chromedp.Run(ctx, chromedp.Evaluate(script, &y)); err != nil {
		return &wire.Result{Error: err.Error()}, nil
	}
	return &wire.Result{Success: true, Data: fmt.Sprintf("scrollY=%g", y)}, nil
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
	capture := chromedp.CaptureScreenshot(&buf)
	if a.FullPage {
		// quality 100 selects PNG in chromedp; anything else silently
		// yields a JPEG, which would write JPEG bytes to a .png path.
		capture = chromedp.FullScreenshot(&buf, 100)
	}
	if err := chromedp.Run(ctx, capture); err != nil {
		return &wire.Result{Error: err.Error()}, nil
	}
	if a.SavePath != "" {
		path, err := writeScreenshot(buf, a.SavePath)
		if err != nil {
			return &wire.Result{Error: err.Error()}, nil
		}
		return &wire.Result{Success: true, Path: path}, nil
	}
	img := base64.StdEncoding.EncodeToString(buf)
	if a.MaxBytes > 0 && len(img) > a.MaxBytes {
		path, err := writeScreenshot(buf, "")
		if err != nil {
			return &wire.Result{Error: err.Error()}, nil
		}
		return &wire.Result{Success: true, Path: path}, nil
	}
	return &wire.Result{Success: true, Image: img}, nil
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
	var raw string
	if err := chromedp.Run(ctx, chromedp.Evaluate(ExtractScript(a), &raw)); err != nil {
		return &wire.Result{Error: err.Error()}, nil
	}
	out := ParseExtractPayload(raw)
	out.Title = title
	out.URL = url
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
	return `(function(){
  var SCOPE_SEL=` + scope + `, MATCH=` + match + `, LIMIT=` + fmt.Sprintf("%d", limit) + `, OFFSET=` + fmt.Sprintf("%d", offset) + `, INCLUDE_HIDDEN=` + fmt.Sprintf("%v", a.IncludeHidden) + `;
  var root = SCOPE_SEL ? document.querySelector(SCOPE_SEL) : document;
  if (!root) return JSON.stringify({error:"extract: scope "+SCOPE_SEL+" not found"});
  var navFilter = !SCOPE_SEL;
  function shown(el){var cs=getComputedStyle(el);if(cs.visibility==="hidden"||cs.display==="none"||cs.opacity==="0")return false;var r=el.getBoundingClientRect();return r.width>0&&r.height>0;}
  var all = root.querySelectorAll("a, button, input, select, textarea, [role='button'], [role='link']");
  var matches = [];
  for (var i=0; i<all.length; i++) {
    var el = all[i];
    if (navFilter && el.closest && el.closest("nav, aside, [role='navigation'], [role='complementary']")) continue;
    var text = (el.innerText || el.value || el.getAttribute("aria-label") || "").trim();
    if (MATCH && text.toLowerCase().indexOf(MATCH.toLowerCase()) < 0) {
      var ph = el.getAttribute && el.getAttribute("placeholder");
      var ar = el.getAttribute && el.getAttribute("aria-label");
      if (!(ph && ph.toLowerCase().indexOf(MATCH.toLowerCase())>=0) && !(ar && ar.toLowerCase().indexOf(MATCH.toLowerCase())>=0)) continue;
    }
    if (!INCLUDE_HIDDEN && !shown(el)) continue;
    matches.push(el);
  }
  var total = matches.length;
  var slice = matches.slice(OFFSET, OFFSET+LIMIT);
  var lines = [], records = [];
  for (var j=0; j<slice.length; j++) {
    var el = slice[j];
    var tag = el.tagName.toLowerCase();
    var text = (el.innerText || el.value || el.getAttribute("aria-label") || "").trim().slice(0,80);
    var attrs = [], attrMap = {};
    var keys = ["type","placeholder","href","name","value","role","aria-label"];
    for (var k=0; k<keys.length; k++) {
      var v = el.getAttribute(keys[k]);
      if (v) { attrs.push(keys[k]+"=\""+String(v).slice(0,60)+"\""); attrMap[keys[k]] = String(v).slice(0,60); }
    }
    var vis = shown(el);
    var r = el.getBoundingClientRect();
    lines.push("["+(OFFSET+j+1)+"]"+(vis?"":" (hidden)")+" <"+tag+" "+attrs.join(" ")+">"+text+"</"+tag+">");
    records.push({index:OFFSET+j+1, tag:tag, text:text, attrs:attrMap, visible:vis,
                  box:{x:Math.round(r.x),y:Math.round(r.y),w:Math.round(r.width),h:Math.round(r.height)}});
  }
  var body = (root === document ? (document.body && document.body.innerText) : root.innerText) || "";
  var viewport = {
    width: Math.round(document.documentElement.clientWidth || window.innerWidth || 0),
    height: Math.round(document.documentElement.clientHeight || window.innerHeight || 0),
    dpr: window.devicePixelRatio || 1,
  };
  return JSON.stringify({
    content: body.length>16000 ? body.slice(0,16000)+"\n... (truncated)" : body,
    elements: lines.join("\n"),
    total: total,
    truncated: total > (OFFSET+LIMIT),
    viewport: viewport,
    data: JSON.stringify({viewport: viewport, elements: records}),
  });
})()`
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
	var out any
	if err := chromedp.Run(ctx, chromedp.Evaluate(script, &out)); err != nil {
		return &wire.Result{Error: err.Error()}, nil
	}
	return &wire.Result{Success: true, Data: fmt.Sprintf("%v", out)}, nil
}

func WaitForSelector(ctx context.Context, a wire.Action) (*wire.Result, error) {
	if a.Selector == "" {
		return &wire.Result{Error: "wait_for_selector: selector required"}, nil
	}
	timeout := time.Duration(a.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	state := strings.ToLower(a.State)
	var err error
	switch state {
	case "", "visible":
		state = "visible"
		err = chromedp.Run(wctx, chromedp.WaitVisible(a.Selector, chromedp.ByQuery))
	case "attached":
		err = chromedp.Run(wctx, chromedp.WaitReady(a.Selector, chromedp.ByQuery))
	case "detached":
		err = chromedp.Run(wctx, chromedp.WaitNotPresent(a.Selector, chromedp.ByQuery))
	default:
		return &wire.Result{Error: fmt.Sprintf("wait_for_selector: unknown state %q (visible|attached|detached)", a.State)}, nil
	}
	if err != nil {
		return &wire.Result{Error: err.Error()}, nil
	}
	return &wire.Result{Success: true, Data: fmt.Sprintf("state=%s", state)}, nil
}

func KeyboardPress(ctx context.Context, a wire.Action) (*wire.Result, error) {
	if a.Key == "" {
		return &wire.Result{Error: "keyboard_press: key required"}, nil
	}
	actions := []chromedp.Action{}
	if a.Selector != "" {
		actions = append(actions, chromedp.Focus(a.Selector, chromedp.ByQuery))
	}
	actions = append(actions, chromedp.KeyEvent(KeyFor(a.Key)))
	if err := chromedp.Run(ctx, actions...); err != nil {
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

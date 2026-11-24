package live

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"

	"github.com/qiangli/yoke/pkg/browser/guard"
	"github.com/qiangli/yoke/pkg/browser/wire"
)

const bannerJS = `(document.getElementById("__bashy_live_banner__")||{}).textContent||""`

// The disposable real-extension fixture for the agent route: a banner on the
// driven tab only, selected vs driven, the note reaching the exact chat, and
// the route going away with the tab or the socket. Opt-in like the other
// extension E2E tests (BASHY_BROWSER_LIVE=1).
func TestExtensionE2EAgentRoute(t *testing.T) {
	guardIsolate(t)
	t.Setenv(guard.FakeAskEnv, "fake:stop")
	svc, ctx1 := startExtensionE2E(t, map[string]string{
		"/one":   `<!doctype html><title>Alpha</title><p>first tab</p>`,
		"/two":   `<!doctype html><title>Beta</title><p>second tab</p>`,
		"/three": `<!doctype html><title>Gamma</title><p>third tab</p>`,
	}, "/one")
	var loc string
	if err := chromedp.Run(ctx1, chromedp.Location(&loc)); err != nil {
		t.Fatal(err)
	}
	base := loc[:strings.LastIndex(loc, "/")]
	ctx2, cancel2 := chromedp.NewContext(ctx1)
	defer cancel2()
	if err := chromedp.Run(ctx2, chromedp.Navigate(base+"/two")); err != nil { // the selected tab
		t.Fatal(err)
	}

	ctx4, cancel4 := chromedp.NewContext(ctx1)
	defer cancel4()
	if err := chromedp.Run(ctx4, chromedp.Navigate(base+"/three")); err != nil { // driven second
		t.Fatal(err)
	}
	// The human selects tab two last.
	if err := chromedp.Run(ctx2, chromedp.ActionFunc(func(c context.Context) error { return page.BringToFront().Do(c) })); err != nil {
		t.Fatal(err)
	}

	do := func(a wire.Action) *wire.Result {
		t.Helper()
		c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		res, err := svc.Execute(c, a)
		if err != nil || !res.Success {
			t.Fatalf("%+v: %+v, %v", a, res, err)
		}
		return res
	}
	banner := func(ctx context.Context) string {
		var s string
		if err := chromedp.Run(ctx, chromedp.Evaluate(bannerJS, &s)); err != nil {
			t.Fatal(err)
		}
		return s
	}
	waitBanner := func(ctx context.Context, want string) {
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); banner(ctx) != want; time.Sleep(50 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("banner = %q, want %q", banner(ctx), want)
			}
		}
	}

	// Legacy calls (no agent/chat) leave no banner anywhere.
	do(wire.Action{Type: wire.ActionExtract, MatchURL: "/one"})
	if banner(ctx1) != "" || banner(ctx2) != "" {
		t.Fatal("a call without agent and chat painted a banner")
	}

	// Drive tab one; the selected tab (two) gets nothing.
	first := do(wire.Action{Type: wire.ActionExtract, MatchURL: "/one", Agent: "claude-x", Chat: "chat-123"}).TabID
	waitBanner(ctx1, "Bashy · claude-x")
	if banner(ctx2) != "" {
		t.Fatal("the banner leaked to the tab the human selected")
	}

	// An extension page stands in for the popup: it talks to the same worker.
	var extURL string
	targets, err := chromedp.Targets(ctx1)
	if err != nil {
		t.Fatal(err)
	}
	for _, tg := range targets {
		if tg.Type == "service_worker" && strings.HasPrefix(tg.URL, "chrome-extension://") {
			extURL = tg.URL[:strings.Index(tg.URL[len("chrome-extension://"):], "/")+len("chrome-extension://")]
		}
	}
	if extURL == "" {
		t.Fatal("no extension service worker target")
	}
	ctx3, cancel3 := chromedp.NewContext(ctx1)
	defer cancel3()
	if err := chromedp.Run(ctx3, chromedp.Navigate(extURL+"/popup.html")); err != nil {
		t.Fatal(err)
	}
	call := func(expr string, out any) {
		t.Helper()
		if err := chromedp.Run(ctx3, chromedp.Evaluate(expr, out, func(p *runtime.EvaluateParams) *runtime.EvaluateParams {
			return p.WithAwaitPromise(true)
		})); err != nil {
			t.Fatal(err)
		}
	}
	type routeRec struct {
		TabID int    `json:"tabId"`
		Agent string `json:"agent"`
		Chat  string `json:"chat"`
	}
	var st struct {
		Routes []routeRec `json:"routes"`
	}
	status := func() map[int]routeRec {
		call(`chrome.runtime.sendMessage({type:"ycode-live:status"})`, &st)
		m := map[int]routeRec{}
		for _, r := range st.Routes {
			m[r.TabID] = r
		}
		return m
	}
	if r := status(); len(r) != 1 || r[first].Agent != "claude-x" || r[first].Chat != "chat-123" {
		t.Fatalf("status routes = %+v, driven tab %d", st.Routes, first)
	}

	// The same agent drives a second tab: both keep their banners and routes,
	// and the selected tab (two) still has none.
	second := do(wire.Action{Type: wire.ActionExtract, MatchURL: "/three", Agent: "claude-x", Chat: "chat-123"}).TabID
	waitBanner(ctx4, "Bashy · claude-x")
	waitBanner(ctx1, "Bashy · claude-x")
	if banner(ctx2) != "" {
		t.Fatal("the banner leaked to the selected tab")
	}
	if r := status(); len(r) != 2 || r[first].Chat != "chat-123" || r[second].Chat != "chat-123" {
		t.Fatalf("two bound tabs: %+v", st.Routes)
	}
	// A failed action (no such tab) binds nothing new.
	c, cancelFail := context.WithTimeout(context.Background(), 30*time.Second)
	if res, err := svc.Execute(c, wire.Action{Type: wire.ActionExtract, MatchURL: "/nowhere", Agent: "other", Chat: "chat-x"}); err == nil && res != nil && res.Success {
		t.Fatal("an action on a missing tab succeeded")
	}
	cancelFail()
	if r := status(); len(r) != 2 {
		t.Fatalf("a failed action bound a route: %+v", st.Routes)
	}

	// The note on the bound tab reaches the handler with the exact chat; the
	// same press from another tab is refused before it leaves the browser.
	got := captureShape(t, ShapeReceipt{Via: "steer", To: "claude-x", Status: "sent to claude-x's live chat session"}, nil)
	var resp struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	press := func(id int, route bool, to string) {
		call(`chrome.runtime.sendMessage({type:"ycode-live:shape", useRoute:`+strconv.FormatBool(route)+`, note:"look", to:"`+to+`", tab:{id:`+itoa(id)+`,url:"u",title:"t"}})`, &resp)
	}
	for _, id := range []int{first, second} {
		press(id, true, "")
		if m := <-got; !resp.OK || m.Chat != "chat-123" || m.TabID != id || m.Note != "look" {
			t.Fatalf("bound press from tab %d: %+v %+v", id, resp, m)
		}
	}
	// The selected, unbound tab is refused when it asks for the route...
	press(first+1000, true, "")
	if resp.OK {
		t.Fatal("a press from a tab that is not bound was delivered")
	}
	noShape(t, got)
	// ...and a legacy/manual press carries no chat, only its recipient.
	press(first+1000, false, "codex")
	if m := <-got; !resp.OK || m.Chat != "" || m.To != "codex" {
		t.Fatalf("manual press: %+v %+v", resp, m)
	}

	// Closing one driven tab clears only its route.
	call(`chrome.tabs.remove(`+itoa(first)+`).then(()=>true)`, new(bool))
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if r := status(); len(r) == 1 && r[second].Chat == "chat-123" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("routes after closing one tab: %+v", st.Routes)
		}
	}

	// A socket disconnect clears route and banner.
	do(wire.Action{Type: wire.ActionExtract, MatchURL: "/two", Agent: "codex", Chat: "chat-9"})
	waitBanner(ctx2, "Bashy · codex")
	svc.hub.mu.Lock()
	conn := svc.hub.conn
	svc.hub.mu.Unlock()
	_ = conn.Close()
	waitBanner(ctx2, "")
	waitBanner(ctx4, "") // the other driven tab loses its banner with the socket too
}

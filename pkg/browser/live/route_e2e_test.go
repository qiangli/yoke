package live

import (
	"context"
	"strings"
	"testing"
	"time"

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
		"/one": `<!doctype html><title>Alpha</title><p>first tab</p>`,
		"/two": `<!doctype html><title>Beta</title><p>second tab</p>`,
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
	var st struct {
		Route *struct {
			TabID int    `json:"tabId"`
			Agent string `json:"agent"`
			Chat  string `json:"chat"`
		} `json:"route"`
	}
	call(`chrome.runtime.sendMessage({type:"ycode-live:status"})`, &st)
	if st.Route == nil || st.Route.TabID != first || st.Route.Agent != "claude-x" || st.Route.Chat != "chat-123" {
		t.Fatalf("status route = %+v, driven tab %d", st.Route, first)
	}

	// The note on the bound tab reaches the handler with the exact chat; the
	// same press from another tab is refused before it leaves the browser.
	got := captureShape(t, ShapeReceipt{Via: "steer", To: "claude-x", Status: "sent to claude-x's live chat session"}, nil)
	var resp struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	press := func(id int) {
		call(`chrome.runtime.sendMessage({type:"ycode-live:shape", useRoute:true, note:"look", to:"", tab:{id:`+itoa(id)+`,url:"u",title:"t"}})`, &resp)
	}
	press(first)
	if m := <-got; !resp.OK || m.Chat != "chat-123" || m.TabID != first || m.Note != "look" {
		t.Fatalf("bound press: %+v %+v", resp, m)
	}
	press(first + 1000)
	if resp.OK {
		t.Fatal("a press from a tab that is not the bound one was delivered")
	}
	noShape(t, got)

	// Closing the driven tab clears the route.
	call(`chrome.tabs.remove(`+itoa(first)+`).then(()=>true)`, new(bool))
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		call(`chrome.runtime.sendMessage({type:"ycode-live:status"})`, &st)
		if st.Route == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("route survived the tab closing")
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
}

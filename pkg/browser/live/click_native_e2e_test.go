package live

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"

	"github.com/qiangli/yoke/pkg/browser/wire"
)

// Real extension (BASHY_BROWSER_LIVE=1): a real navigation — a reload
// to the same URL included — reports navigated, and a native JS dialog
// reports dialog, bounded, without the extension answering it.

const e2eNativeFixture = `<!doctype html><html><head><title>load</title></head><body>
<script>
sessionStorage.n = String(+(sessionStorage.n || 0) + 1);
document.title = 'load-' + sessionStorage.n;
</script>
<button id="reload" onclick="location.reload()">Reload</button>
<a id="away" href="/two">Away</a>
<button id="alerter" onclick="alert('native hello')">Alert</button>
<button id="confirmer" onclick="window.answer = String(confirm('sure?'))">Confirm</button>
</body></html>`

func TestExtensionE2EClickNavigationAndNativeDialog(t *testing.T) {
	guardIsolate(t)
	fixtures := map[string]headerFixture{
		"/nav": {body: e2eNativeFixture},
		"/two": {body: `<!doctype html><title>two</title><p>second page</p>`},
	}
	svc, tab, site := startEffectsE2E(t, fixtures, "/nav")
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	do := func(a wire.Action) *wire.Result {
		t.Helper()
		res, err := svc.Execute(ctx, a)
		if err != nil || res == nil {
			t.Fatalf("%s: %v %+v", a.Type, err, res)
		}
		return res
	}
	title := func() string {
		var s string
		if err := chromedp.Run(tab, chromedp.Title(&s)); err != nil {
			t.Fatal(err)
		}
		return s
	}
	waitTitle := func(want string) {
		t.Helper()
		for deadline := time.Now().Add(8 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
			if title() == want {
				return
			}
		}
		t.Fatalf("title = %q, want %q", title(), want)
	}
	open := func() {
		t.Helper()
		if err := chromedp.Run(tab, chromedp.Navigate(site+"/nav")); err != nil {
			t.Fatal(err)
		}
	}

	waitTitle("load-1")
	if res := do(wire.Action{Type: wire.ActionClick, Selector: "#reload"}); !res.Success || res.Content != "clicked: navigated" {
		t.Fatalf("reload click = %+v, want clicked: navigated", res)
	}
	waitTitle("load-2")

	open()
	if res := do(wire.Action{Type: wire.ActionClick, Selector: "#away"}); !res.Success || res.Content != "clicked: navigated" {
		t.Fatalf("link click = %+v, want clicked: navigated", res)
	}
	waitTitle("two")

	for _, sel := range []string{"#alerter", "#confirmer"} {
		open()
		var closed atomic.Int32
		chromedp.ListenTarget(tab, func(e any) {
			if _, ok := e.(*page.EventJavascriptDialogClosed); ok {
				closed.Add(1)
			}
		})
		start := time.Now()
		res := do(wire.Action{Type: wire.ActionClick, Selector: sel})
		if !res.Success || res.Content != "clicked: dialog" {
			t.Fatalf("click %s = %+v, want clicked: dialog", sel, res)
		}
		if d := time.Since(start); d > 6*time.Second {
			t.Fatalf("click %s took %s, want a bounded wait", sel, d)
		}
		time.Sleep(300 * time.Millisecond)
		if closed.Load() != 0 {
			t.Fatalf("click %s answered the native dialog itself", sel)
		}
		// The fixture dialog is the test's own: dismiss it.
		if err := chromedp.Run(tab, chromedp.ActionFunc(func(ctx context.Context) error {
			return page.HandleJavaScriptDialog(false).Do(ctx)
		})); err != nil && !strings.Contains(err.Error(), "No dialog") {
			t.Fatalf("dialog was not open for the operator after %s: %v", sel, err)
		}
	}
}

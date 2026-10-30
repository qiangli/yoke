package solo

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"

	"github.com/qiangli/yoke/pkg/browser/wire"
)

// Real-Chrome (BASHY_BROWSER_LIVE=1) check of the click effects page
// script cannot see: a real navigation (same-URL reload included) and a
// native JS dialog, which must be reported without being answered.

const clickNativeFixture = `<!doctype html><html><head><title>load</title></head><body>
<script>
sessionStorage.n = String(+(sessionStorage.n || 0) + 1);
document.title = 'load-' + sessionStorage.n;
</script>
<button id="reload" onclick="location.reload()">Reload</button>
<a id="away" href="/two">Away</a>
<button id="alerter" onclick="alert('native hello')">Alert</button>
<button id="confirmer" onclick="window.answer = String(confirm('sure?'))">Confirm</button>
</body></html>`

func TestSoloClickReportsNavigationAndNativeDialog(t *testing.T) {
	if os.Getenv("BASHY_BROWSER_LIVE") == "" {
		t.Skip("live Chrome test disabled; set BASHY_BROWSER_LIVE=1 to run (launches a real Chrome)")
	}
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("BASHY_SECRETS_TOKEN", "")
	t.Setenv("BASHY_API_KEY", "")

	mux := http.NewServeMux()
	mux.HandleFunc("/nav", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, clickNativeFixture)
	})
	mux.HandleFunc("/two", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!doctype html><title>two</title><p>second page</p>`)
	})
	srv := httptest.NewServer(mux)
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
		if err != nil || res == nil {
			t.Fatalf("%s: %v %+v", a.Type, err, res)
		}
		return res
	}
	title := func() string {
		var s string
		if err := chromedp.Run(c.current(), chromedp.Title(&s)); err != nil {
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
		if res := exec(wire.Action{Type: wire.ActionNavigate, URL: srv.URL + "/nav"}); !res.Success {
			t.Fatalf("navigate: %+v", res)
		}
	}

	// Same-URL reload: the URL does not move, the page does.
	open()
	waitTitle("load-1")
	if res := exec(wire.Action{Type: wire.ActionClick, Selector: "#reload"}); !res.Success || res.Content != "clicked: navigated" {
		t.Fatalf("reload click = %+v, want clicked: navigated", res)
	}
	waitTitle("load-2")

	// A link to another document.
	open()
	if res := exec(wire.Action{Type: wire.ActionClick, Selector: "#away"}); !res.Success || res.Content != "clicked: navigated" {
		t.Fatalf("link click = %+v, want clicked: navigated", res)
	}
	waitTitle("two")

	// Native dialogs: reported, bounded, left open for the operator.
	for _, sel := range []string{"#alerter", "#confirmer"} {
		open()
		var closed atomic.Int32
		chromedp.ListenTarget(c.current(), func(e any) {
			if _, ok := e.(*page.EventJavascriptDialogClosed); ok {
				closed.Add(1)
			}
		})
		start := time.Now()
		res := exec(wire.Action{Type: wire.ActionClick, Selector: sel})
		if !res.Success || res.Content != "clicked: dialog" {
			t.Fatalf("click %s = %+v, want clicked: dialog", sel, res)
		}
		if d := time.Since(start); d > 3*time.Second {
			t.Fatalf("click %s took %s, want a bounded wait", sel, d)
		}
		time.Sleep(300 * time.Millisecond)
		if closed.Load() != 0 {
			t.Fatalf("click %s answered the native dialog itself", sel)
		}
		// The fixture dialog is the test's own: dismiss it.
		if err := chromedp.Run(c.current(), chromedp.ActionFunc(func(ctx context.Context) error {
			return page.HandleJavaScriptDialog(false).Do(ctx)
		})); err != nil {
			t.Fatalf("dialog was not open for the operator after %s: %v", sel, err)
		}
	}
}

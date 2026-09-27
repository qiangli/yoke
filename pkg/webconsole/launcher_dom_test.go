//go:build verifydom

package webconsole

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/chromedp/chromedp"
)

// This integration test loads the tutorial itself, not a copy of its HTML.
func TestDOMCustomLauncher(t *testing.T) {
	t.Setenv("BASHY_APPS_DIR", t.TempDir())
	dir := os.Getenv("BASHY_LAUNCHER_EXAMPLE_DIR")
	if dir == "" {
		dir = filepath.Join("..", "..", "..", "bashy", "examples", "launcher")
	}
	if _, err := os.Stat(filepath.Join(dir, "index.html")); err != nil {
		t.Skip("custom launcher integration requires the bashy example sibling or BASHY_LAUNCHER_EXAMPLE_DIR")
	}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	port := up.Listener.Addr().(*net.TCPAddr).Port
	base, ctx, errors := domEnv(t, Options{Launcher: dir})
	var sorted, stockRemove bool
	if err := chromedp.Run(ctx,
		chromedp.Navigate(base+"/"),
		chromedp.WaitVisible("#apps li"),
		chromedp.Evaluate(`(() => { const names = [...document.querySelectorAll('#apps li')].map(row => row.dataset.name); return names.length > 0 && names.every((name, i) => i === 0 || names[i-1].localeCompare(name) <= 0); })()`, &sorted),
		chromedp.Evaluate(`!!document.querySelector('#apps button')`, &stockRemove),
		chromedp.SendKeys(`#add-app input[name="name"]`, "aaa-example"),
		chromedp.SendKeys(`#add-app input[name="port"]`, strconv.Itoa(port)),
		chromedp.SendKeys(`#add-app input[name="label"]`, "Example app"),
		chromedp.Click("#add-app button"),
		chromedp.WaitVisible(`#apps li[data-name="aaa-example"]`),
	); err != nil {
		t.Fatal(err)
	}
	if !sorted || stockRemove {
		t.Errorf("alphabetical=%v stock remove=%v", sorted, stockRemove)
	}
	var addedSorted bool
	var label string
	if err := chromedp.Run(ctx,
		chromedp.Evaluate(`document.querySelector('#apps li').dataset.name === 'aaa-example'`, &addedSorted),
		chromedp.Text(`#apps li[data-name="aaa-example"] a`, &label),
		chromedp.Click(`#apps li[data-name="aaa-example"] button`),
		chromedp.WaitNotPresent(`#apps li[data-name="aaa-example"]`),
	); err != nil {
		t.Fatal(err)
	}
	if !addedSorted || label != "aaa-example — Example app (ready)" {
		t.Errorf("added sorted=%v label=%q", addedSorted, label)
	}
	var errorText string
	if err := chromedp.Run(ctx, chromedp.Text("#error", &errorText)); err != nil {
		t.Fatal(err)
	}
	if errorText != "" {
		t.Errorf("launcher request error: %s", errorText)
	}
	assertNoJSErrors(t, "custom launcher add/remove", errors())
}

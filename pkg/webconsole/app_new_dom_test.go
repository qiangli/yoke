// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

//go:build verifydom

package webconsole

import (
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// TestDOMNewAppFromLauncher: the launcher's "New app" card names a blank app
// and opens its empty page, the way File > New opens a new document.
func TestDOMNewAppFromLauncher(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	t.Setenv("BASHY_APPS_DIR", t.TempDir())
	base, ctx, errs := domEnv(t, Options{})

	var path, blank string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(base+"/"),
		chromedp.WaitVisible("#new-app-open"),
		chromedp.Click("#new-app-open"),
		chromedp.WaitVisible("#new-app-label"),
		chromedp.SendKeys("#new-app-label", "Sign-up sheet"),
		chromedp.Click("#new-app-create"),
		chromedp.WaitVisible("main[data-bashy-blank]"),
		chromedp.Evaluate(`location.pathname`, &path),
		chromedp.Text("main[data-bashy-blank] h1", &blank),
		chromedp.Sleep(200*time.Millisecond),
	); err != nil {
		t.Fatalf("chromedp: %v", err)
	}
	assertNoJSErrors(t, "new app", errs())
	if path != "/sign-up-sheet/" || blank != "Sign-up sheet" {
		t.Fatalf("new app opened %q with heading %q", path, blank)
	}
}

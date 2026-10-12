//go:build verifydom

package webconsole

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

func TestDOMSprintInitialLoadingIndicator(t *testing.T) {
	stubFilterBoard(t)
	var fail atomic.Bool
	fail.Store(true)
	base, ctx, errs := domEnvWrapped(t, Options{}, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/sprint" {
				select {
				case <-time.After(2 * time.Second):
				case <-r.Context().Done():
					return
				}
				if fail.Load() {
					http.Error(w, "unavailable", http.StatusServiceUnavailable)
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	})
	var busy, animation string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(base+"/sprint/"),
		chromedp.WaitVisible(`#bd-loading .bd-loading-spinner`, chromedp.ByQuery),
		chromedp.Evaluate(`document.getElementById("bd-page").getAttribute("aria-busy")`, &busy),
		chromedp.Evaluate(`getComputedStyle(document.querySelector(".bd-loading-spinner")).animationName`, &animation),
	); err != nil {
		t.Fatal(err)
	}
	if busy != "true" || animation != "spin" {
		t.Errorf("loading busy=%q animation=%q", busy, animation)
	}
	var message string
	if err := chromedp.Run(ctx,
		chromedp.WaitVisible(`#bd-summary [role=alert]`, chromedp.ByQuery),
		chromedp.Text(`#bd-summary`, &message, chromedp.ByQuery),
		chromedp.Poll(`document.getElementById("bd-loading").hidden && document.getElementById("bd-page").getAttribute("aria-busy") === "false"`, nil),
	); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(message, "Refresh") || !strings.Contains(message, "503") {
		t.Errorf("error=%q", message)
	}
	fail.Store(false)
	if err := chromedp.Run(ctx,
		chromedp.Click(`#f-refresh`, chromedp.ByQuery),
		chromedp.WaitVisible(`#bd-loading .bd-loading-spinner`, chromedp.ByQuery),
		chromedp.WaitVisible(`#bd-sprints .bd-sprint`, chromedp.ByQuery),
		chromedp.Poll(`document.getElementById("bd-loading").hidden && document.getElementById("bd-page").getAttribute("aria-busy") === "false"`, nil),
		chromedp.Click(`#f-refresh`, chromedp.ByQuery),
	); err != nil {
		t.Fatal(err)
	}
	var hidden bool
	if err := chromedp.Run(ctx, chromedp.Evaluate(`document.getElementById("bd-loading").hidden && !!document.querySelector("#bd-sprints .bd-sprint")`, &hidden)); err != nil {
		t.Fatal(err)
	}
	if !hidden {
		t.Error("background refresh replaced loaded content with spinner")
	}
	assertNoJSErrors(t, "initial sprint loading", errs())
}

//go:build verifydom

package webconsole

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// The launcher paints from the catalog and loads everything else on its own:
// a slow supplemental endpoint must never hold the tiles, and a tile click
// loads only the app that was clicked.

// supplemental reports whether a launcher request is one the first paint must
// not wait for — everything but the page, its assets and the catalog.
func supplemental(r *http.Request) bool {
	switch r.URL.Path {
	case "/api/apps":
		return r.URL.Query().Get("catalog") != "1"
	case "/api/session", "/api/look", "/api/inbox", "/api/cloud", "/api/neighborhood":
		return true
	}
	return false
}

// requestLog records every path the browser asked for.
type requestLog struct {
	mu    sync.Mutex
	paths []string
}

func (l *requestLog) add(p string) { l.mu.Lock(); l.paths = append(l.paths, p); l.mu.Unlock() }
func (l *requestLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.paths...)
}

// delaySupplemental holds every supplemental answer for d.
func delaySupplemental(d time.Duration, log *requestLog) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if log != nil {
				log.add(r.URL.Path)
			}
			if supplemental(r) {
				select {
				case <-time.After(d):
				case <-r.Context().Done():
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// slowApp is a registered upstream that answers after delay, with failing
// answering 503 until it is cleared.
func slowApp(t *testing.T, name string, delay time.Duration, failing *atomic.Bool) *atomic.Int32 {
	t.Helper()
	requests := &atomic.Int32{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			requests.Add(1)
		}
		time.Sleep(delay)
		if failing != nil && failing.Load() {
			http.Error(w, "warming up", http.StatusServiceUnavailable)
			return
		}
		io.WriteString(w, "<!doctype html><title>up</title><p id=up>upstream "+r.URL.Path+"</p>")
	}))
	t.Cleanup(up.Close)
	_, port, _ := net.SplitHostPort(up.Listener.Addr().String())
	writeAppRecord(t, name, "name: "+name+"\nlabel: Slow App\nport: "+port+"\n")
	t.Setenv("BASHY_HOME", t.TempDir())
	return requests
}

func TestDOMLauncherTilesDoNotWaitForSupplementalEndpoints(t *testing.T) {
	slowApp(t, "slowapp", 0, nil)
	log := &requestLog{}
	base, ctx, errs := domEnvWrapped(t, Options{Ctx: context.Background()}, delaySupplemental(6*time.Second, log))

	var badge string
	var elapsed float64
	wctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := chromedp.Run(wctx,
		chromedp.Navigate(base+"/"),
		chromedp.WaitVisible(`a.tile[data-app="slowapp"]`, chromedp.ByQuery),
		chromedp.Evaluate(`performance.now()`, &elapsed),
		chromedp.Evaluate(`document.querySelector('a.tile[data-app="slowapp"] .badge').className`, &badge),
	); err != nil {
		t.Fatalf("tiles never painted: %v", err)
	}
	// Every supplemental answer is held 6s; the tiles must beat that by far.
	if elapsed > 3000 {
		t.Errorf("tiles painted %.0fms after navigation start; supplemental endpoints gated them", elapsed)
	}
	if badge != "badge pending" {
		t.Errorf("catalog tile badge = %q, want pending until probed", badge)
	}
	for _, p := range log.snapshot() {
		if strings.HasPrefix(p, "/slowapp") || strings.HasPrefix(p, "/sprint") || (strings.HasPrefix(p, "/api/board") || strings.HasPrefix(p, "/api/sprint") || strings.Contains(p, "/vendor/xterm")) {
			t.Errorf("launcher eagerly requested app content %q", p)
		}
	}

	// The supplemental answers still land, each on its own.
	if err := chromedp.Run(ctx,
		chromedp.WaitReady(`a.tile[data-app="slowapp"] .badge.ready`, chromedp.ByQuery),
		chromedp.Poll(`document.getElementById("who").textContent.includes("·")`, nil, chromedp.WithPollingTimeout(15*time.Second)),
	); err != nil {
		t.Fatalf("supplemental answers never applied: %v", err)
	}
	assertNoJSErrors(t, "delayed supplemental", errs())
}

func TestDOMLauncherSlowAppShowsSpinnerThenOpens(t *testing.T) {
	requests := slowApp(t, "slowapp", 2*time.Second, nil)
	base, ctx, errs := domEnv(t, Options{Ctx: context.Background()})

	var busy, live string
	var spinner bool
	if err := chromedp.Run(ctx,
		chromedp.Navigate(base+"/"),
		chromedp.WaitVisible(`a.tile[data-app="slowapp"]`, chromedp.ByQuery),
		chromedp.Click(`a.tile[data-app="slowapp"]`, chromedp.ByQuery),
		chromedp.WaitReady(`.tile-wrap.opening .spinner`, chromedp.ByQuery),
		chromedp.Evaluate(`!!document.querySelector('a.tile[data-app="slowapp"] .spinner')`, &spinner),
		chromedp.Evaluate(`document.querySelector('a.tile[data-app="slowapp"]').getAttribute("aria-busy")||""`, &busy),
		chromedp.Evaluate(`document.getElementById("launch-status").textContent`, &live),
	); err != nil {
		t.Fatalf("no loading state: %v", err)
	}
	if !spinner || busy != "true" || !strings.Contains(live, "Opening Slow App") {
		t.Errorf("loading state: spinner=%v aria-busy=%q live=%q", spinner, busy, live)
	}
	assertNoJSErrors(t, "launcher before navigation", errs())

	var landed, path string
	if err := chromedp.Run(ctx,
		chromedp.WaitVisible(`#up`, chromedp.ByQuery),
		chromedp.Text(`#up`, &landed, chromedp.ByQuery),
		chromedp.Evaluate(`location.pathname`, &path),
	); err != nil {
		t.Fatalf("never opened the slow app: %v", err)
	}
	if requests.Load() != 1 {
		t.Errorf("selected app requested %d times, want once", requests.Load())
	}
	if landed != "upstream /" || path != "/slowapp/" {
		t.Errorf("landed on %q at %q", landed, path)
	}
}

// An upstream failure remains its own page; Back returns to a usable launcher
// and another click retries the app without a duplicate preflight request.
func TestDOMLauncherOpenFailureIsRecoverable(t *testing.T) {
	var failing atomic.Bool
	failing.Store(true)
	requests := slowApp(t, "slowapp", 0, &failing)
	base, ctx, errs := domEnv(t, Options{Ctx: context.Background()})
	var body string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(base+"/"),
		chromedp.WaitVisible(`a.tile[data-app="slowapp"]`, chromedp.ByQuery),
		chromedp.Click(`a.tile[data-app="slowapp"]`, chromedp.ByQuery),
		chromedp.WaitVisible(`body > pre`, chromedp.ByQuery),
		chromedp.Text(`body`, &body, chromedp.ByQuery),
	); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "warming up") {
		t.Errorf("error page = %q", body)
	}
	failing.Store(false)
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := chromedp.Run(ctx, chromedp.NavigateBack()); err != nil {
		t.Fatalf("back navigation: %v", err)
	}
	if err := chromedp.Run(ctx,
		chromedp.WaitVisible(`a.tile[data-app="slowapp"]`, chromedp.ByQuery),
		chromedp.Poll(`!document.querySelector(".tile-wrap.opening")`, nil),
	); err != nil {
		t.Fatalf("launcher after back: %v", err)
	}
	if err := chromedp.Run(ctx, chromedp.Click(`a.tile[data-app="slowapp"]`, chromedp.ByQuery), chromedp.WaitVisible(`#up`, chromedp.ByQuery)); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if requests.Load() != 2 {
		t.Errorf("app requested %d times, want one per click", requests.Load())
	}
	assertNoJSErrors(t, "failure and retry", errs())
}

// clickProbe dispatches a click on the tile and reports whether the launcher
// took it over (preventDefault) — the window listener cancels the navigation
// afterwards so the page stays put either way.
const clickProbe = `(() => {
	let taken = null;
	const watch = (e) => { taken = e.defaultPrevented; e.preventDefault(); };
	addEventListener("click", watch, { once: true });
	const a = document.querySelector('a.tile[data-app="slowapp"]');
	a.dispatchEvent(new MouseEvent("click", { bubbles: true, cancelable: true, button: %s, ctrlKey: %s, metaKey: %s, shiftKey: %s }));
	return JSON.stringify({ taken, opening: !!document.querySelector(".tile-wrap.opening"), target: a.target, rel: a.rel });
})()`

func probeClick(button, ctrl, meta, shift string) string {
	s := clickProbe
	for _, v := range []string{button, ctrl, meta, shift} {
		s = strings.Replace(s, "%s", v, 1)
	}
	return s
}

func TestDOMLauncherKeepsLinkSemantics(t *testing.T) {
	slowApp(t, "slowapp", 3*time.Second, nil)
	base, ctx, errs := domEnv(t, Options{Ctx: context.Background()})
	if err := chromedp.Run(ctx,
		chromedp.Navigate(base+"/"),
		chromedp.WaitVisible(`a.tile[data-app="slowapp"]`, chromedp.ByQuery),
	); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ name, button, ctrl, meta, shift string }{
		{"ctrl", "0", "true", "false", "false"},
		{"meta", "0", "false", "true", "false"},
		{"shift", "0", "false", "false", "true"},
		{"middle", "1", "false", "false", "false"},
	} {
		var got string
		if err := chromedp.Run(ctx, chromedp.Evaluate(probeClick(c.button, c.ctrl, c.meta, c.shift), &got)); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got, `"taken":false`) || !strings.Contains(got, `"opening":false`) {
			t.Errorf("%s-click was intercepted: %s", c.name, got)
		}
	}

	// New tab mode: a real target=_blank link the launcher never takes over.
	var got string
	if err := chromedp.Run(ctx,
		chromedp.Evaluate(`fetch("api/look",{method:"PUT",headers:{"Content-Type":"application/json"},body:JSON.stringify({open_apps:"new-tab"})}).then(r=>r.status)`, nil, awaitPromise),
		chromedp.Reload(),
		chromedp.WaitVisible(`a.tile[data-app="slowapp"][target="_blank"]`, chromedp.ByQuery),
		chromedp.Evaluate(probeClick("0", "false", "false", "false"), &got),
	); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, `"taken":false`) || !strings.Contains(got, `"rel":"noopener"`) {
		t.Errorf("new-tab click: %s", got)
	}
	assertNoJSErrors(t, "link semantics", errs())
}

// Under a base prefix every launcher URL — the catalog and the opened app —
// resolves under the mount, never at the origin root.
func TestDOMLauncherOpensUnderBasePrefix(t *testing.T) {
	slowApp(t, "slowapp", 500*time.Millisecond, nil)
	const prefix = "/matrix/h/dev/app/console"
	base, ctx, errs := domEnvWrapped(t, Options{Ctx: context.Background()}, cloudFront(prefix))
	var path, landed string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(base+prefix+"/"),
		chromedp.WaitVisible(`a.tile[data-app="slowapp"]`, chromedp.ByQuery),
		chromedp.Click(`a.tile[data-app="slowapp"]`, chromedp.ByQuery),
		chromedp.WaitVisible(`#up`, chromedp.ByQuery),
		chromedp.Text(`#up`, &landed, chromedp.ByQuery),
		chromedp.Evaluate(`location.pathname`, &path),
	); err != nil {
		t.Fatalf("prefixed open: %v", err)
	}
	if path != prefix+"/slowapp/" || landed != "upstream /" {
		t.Errorf("opened %q (%q)", path, landed)
	}
	assertNoJSErrors(t, "prefixed launcher", errs())
}

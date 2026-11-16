package solo

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/browser/wire"
)

func TestResolveChromePathHonorsMissingExplicitPath(t *testing.T) {
	c := New(Config{ChromePath: "/definitely/not/a/chrome"})
	if got := c.ResolveChromePath(); got != "" {
		t.Fatalf("ResolveChromePath=%q, want empty", got)
	}
}

func TestLiveSoloSkippedWhenNoChrome(t *testing.T) {
	// Opt-in only. This test LAUNCHES a real Chrome, and on a machine with
	// Chrome installed but no GUI/WindowServer session (headless CI, a weave
	// fleet worker, an ssh session) that launch aborts in _RegisterApplication
	// (SIGABRT) BEFORE the launchability check below can skip — firing a
	// crash-reporter popup on every `go test ./...`. Gate it so an automated
	// run never touches Chrome; set BASHY_BROWSER_LIVE=1 to run it by hand on a
	// desktop where a live smoke check of the solo driver is actually useful.
	if os.Getenv("BASHY_BROWSER_LIVE") == "" {
		t.Skip("live Chrome test disabled; set BASHY_BROWSER_LIVE=1 to run (launches a real Chrome)")
	}
	ctx := context.Background()
	c := New(Config{})
	if !c.Available(ctx) {
		t.Skip("no Chrome or Chromium binary found")
	}
	defer c.Close()
	res, err := c.Execute(ctx, wire.Action{Type: wire.ActionEvaluate, Script: "1+1"})
	if err != nil {
		t.Skipf("Chrome found but not launchable in this environment: %v", err)
	}
	if !res.Success || res.Data != "2" {
		t.Fatalf("unexpected live result: %#v", res)
	}
}

// TestLiveSoloNavigateBodyTextInCSS pins todo f1cd947e: navigate read the
// page with a plain-text DOM search for "body", which also matched the
// CSS rule `body{...}`; that text node has no box model, so the
// visibility wait never finished and every real page timed out.
func TestLiveSoloNavigateBodyTextInCSS(t *testing.T) {
	if os.Getenv("BASHY_BROWSER_LIVE") == "" {
		t.Skip("live Chrome test disabled; set BASHY_BROWSER_LIVE=1 to run (launches a real Chrome)")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html><title>t</title><style>body{margin:auto}</style><body><p>hello body</p></body>`))
	}))
	defer srv.Close()
	c := New(Config{})
	if !c.Available(context.Background()) {
		t.Skip("no Chrome or Chromium binary found")
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	start := time.Now()
	res, err := c.Execute(ctx, wire.Action{Type: wire.ActionNavigate, URL: srv.URL})
	if err != nil {
		t.Fatalf("navigate: %v", err)
	}
	if !res.Success || !strings.Contains(res.Content, "hello body") {
		t.Fatalf("navigate result after %s: %#v", time.Since(start), res)
	}
}

package solo

import (
	"context"
	"os"
	"testing"

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

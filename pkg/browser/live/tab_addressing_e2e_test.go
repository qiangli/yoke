package live

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"

	"github.com/qiangli/yoke/pkg/browser/guard"
	"github.com/qiangli/yoke/pkg/browser/wire"
)

// Two tabs through the real extension: the first (ordinary field) is NOT
// active, the second (sensitive field) is. Opt-in like the other extension
// E2E tests (BASHY_BROWSER_LIVE=1).
func TestExtensionE2ETabAddressing(t *testing.T) {
	guardIsolate(t)
	t.Setenv(guard.FakeAskEnv, "fake:stop")
	svc, ctx1 := startExtensionE2E(t, map[string]string{
		"/one": `<!doctype html><title>Alpha</title><input type="text" id="note" aria-label="note"><p>first tab</p>`,
		"/two": typeformScreen("Social Security Number", `<input type="text" aria-label="ssn">`),
	}, "/one")
	var loc string
	if err := chromedp.Run(ctx1, chromedp.Location(&loc)); err != nil {
		t.Fatal(err)
	}
	base := loc[:strings.LastIndex(loc, "/")]
	ctx2, cancel2 := chromedp.NewContext(ctx1)
	defer cancel2()
	if err := chromedp.Run(ctx2, chromedp.Navigate(base+"/two")); err != nil {
		t.Fatal(err)
	}

	do := func(a wire.Action) *wire.Result {
		t.Helper()
		c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		res, err := svc.Execute(c, a)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	one := wire.Action{MatchURL: "/one"}
	with := func(typ string, f func(*wire.Action)) wire.Action {
		a := one
		a.Type = typ
		f(&a)
		return a
	}

	ex := do(with(wire.ActionExtract, func(*wire.Action) {}))
	if !ex.Success || !strings.HasSuffix(ex.URL, "/one") || ex.TabID == 0 {
		t.Fatalf("addressed extract hit the wrong tab: %+v", ex)
	}
	firstID := ex.TabID

	if r := do(with(wire.ActionType, func(a *wire.Action) { a.Selector, a.Text = "#note", "hello" })); !r.Success || r.TabID != firstID {
		t.Fatalf("addressed type past the active sensitive tab: %+v", r)
	}
	var val string
	if err := chromedp.Run(ctx1, chromedp.Value("#note", &val, chromedp.ByQuery)); err != nil || val != "hello" {
		t.Fatalf("first tab field = %q, %v", val, err)
	}

	if r := do(with(wire.ActionEvaluate, func(a *wire.Action) { a.Script = "document.title" })); !r.Success || r.TabID != firstID || !strings.Contains(r.Content+r.Data, "Alpha") {
		t.Fatalf("addressed evaluate: %+v", r)
	}
	if r := do(with(wire.ActionScreenshot, func(*wire.Action) {})); !r.Success || r.TabID != firstID {
		t.Fatalf("addressed screenshot: %+v", r)
	}
	if r := do(with(wire.ActionScroll, func(a *wire.Action) { a.Direction, a.Amount = "down", 100 })); !r.Success || r.TabID != firstID {
		t.Fatalf("addressed scroll: %+v", r)
	}
	byID := wire.Action{Type: wire.ActionExtract, TabID: firstID}
	if r := do(byID); !r.Success || !strings.HasSuffix(r.URL, "/one") {
		t.Fatalf("extract by id: %+v", r)
	}

	for name, a := range map[string]wire.Action{
		"ambiguous": {Type: wire.ActionExtract, MatchURL: "127.0.0.1"},
		"missing":   {Type: wire.ActionExtract, MatchURL: "no-such-tab"},
		"bad id":    {Type: wire.ActionExtract, TabID: 999999},
		"write":     {Type: wire.ActionType, Selector: "#note", Text: "x", MatchTitle: "no-such-title"},
	} {
		if r := do(a); r.Success || r.Error == "" {
			t.Fatalf("%s must fail, got %+v", name, r)
		}
	}
	if err := chromedp.Run(ctx1, chromedp.Value("#note", &val, chromedp.ByQuery)); err != nil || val != "hello" {
		t.Fatalf("a failed address still wrote: %q, %v", val, err)
	}

	before, _ := chromedp.Targets(ctx1)
	nav := do(wire.Action{Type: wire.ActionNavigate, URL: base + "/two?fresh=1"})
	if !nav.Success || nav.TabID == 0 || nav.TabID == firstID {
		t.Fatalf("unaddressed navigate must open a new tab: %+v", nav)
	}
	after, _ := chromedp.Targets(ctx1)
	if len(after) <= len(before) {
		t.Fatalf("targets %d -> %d: no new tab", len(before), len(after))
	}
	if err := chromedp.Run(ctx1, chromedp.Location(&loc)); err != nil || !strings.HasSuffix(loc, "/one") {
		t.Fatalf("first tab was navigated away: %q, %v", loc, err)
	}
}

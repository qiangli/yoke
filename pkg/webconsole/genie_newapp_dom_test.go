// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

//go:build verifydom

package webconsole

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// signupShaper grows a blank app's page into a sign-up form, the way an
// agent answering the first wish on a new app would.
type signupShaper struct{}

func (signupShaper) Delegate(_ context.Context, req DelegateRequest) (DelegateResult, error) {
	index := filepath.Join(req.Dir, "public", "index.html")
	b, err := os.ReadFile(index)
	if err != nil {
		return DelegateResult{}, err
	}
	grown := strings.Replace(string(b), "</main>", `<form id="signup"><input name="name"> <input name="email"> <button>Sign up</button></form></main>`, 1)
	if err := os.WriteFile(index, []byte(grown), 0o644); err != nil {
		return DelegateResult{}, err
	}
	return DelegateResult{Agent: "codex:fake-shaper", Identity: "genie-delegate:codex-fake", Receipt: "r-new",
		Output: "Made a sign-up form.\n\nCOVERED:\n- sign-up form\nNOT COVERED:\n- (none)\n"}, nil
}

// TestDOMGenieNewApp: a new app is made from the same Genie button as any
// other wish, on any page. In the wish panel the target switches from
// "This app" to "New app"; with a name and a wish, the blank app is
// created, the wish becomes the first step of an edit session on it, and
// the browser lands in that draft with the result in place, to Save or
// Cancel like a document.
func TestDOMGenieNewApp(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	t.Setenv("BASHY_APPS_DIR", t.TempDir())
	base, ctx, errs := domEnv(t, Options{Shape: ShapeOptions{Home: home, WorkRoot: t.TempDir(),
		Delegate: signupShaper{}, Verifier: &fakeVerifier{}, Activator: &fakeActivator{home: home}}})

	until := func(step, expr string) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for {
			var ok bool
			if err := chromedp.Run(ctx, chromedp.Evaluate(expr, &ok)); err != nil {
				t.Fatalf("%s: %v", step, err)
			}
			if ok {
				return
			}
			if time.Now().After(deadline) {
				var st string
				_ = chromedp.Run(ctx, chromedp.Evaluate(`((document.querySelector('.bgn-status') || {}).textContent || '') + ' @ ' + location.href`, &st))
				t.Fatalf("%s: never true: %s (status %q; js errors %q)", step, expr, st, errs())
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	run := func(step string, a ...chromedp.Action) {
		t.Helper()
		if err := chromedp.Run(ctx, a...); err != nil {
			t.Fatalf("%s: %v (js errors %q)", step, err, errs())
		}
	}
	run("open launcher", chromedp.Navigate(base+"/"))
	until("genie button", `!!document.querySelector('[data-bashy-genie-btn="live"]')`)
	run("open panel", chromedp.Click(`[data-bashy-genie-btn="live"]`, chromedp.ByQuery))
	until("target choice", `!!document.querySelector('[data-bgn-target="new"]')`)
	run("pick new app", chromedp.Click(`[data-bgn-target="new"]`, chromedp.ByQuery))
	until("name box", `(() => { const i = document.querySelector('[data-bgn-newapp-name]'); return !!i && !i.hidden; })()`)
	run("name", chromedp.SendKeys(`[data-bgn-newapp-name]`, "Sign-up sheet", chromedp.ByQuery, chromedp.NodeReady))
	run("wish", chromedp.SendKeys(`[data-bashy-genie-panel] textarea`, "a sign-up sheet for Saturday's class with name and email", chromedp.ByQuery, chromedp.NodeReady))
	run("make the app", chromedp.Click(`[data-bashy-genie-panel] .bgn-b.bgn-p`, chromedp.ByQuery, chromedp.NodeReady))
	until("lands in the new app's draft", `location.pathname.includes('/shape/sign-up-sheet/draft/') && !!document.querySelector('#signup') && !!document.querySelector('[data-bgn-draft]')`)
	var bar string
	run("read bar", chromedp.Evaluate(`document.querySelector('[data-bgn-draft]').textContent`, &bar))
	assertNoJSErrors(t, "genie new app", errs())
	if !strings.Contains(bar, "1 wish") {
		t.Fatalf("draft bar = %q, want one wish applied", bar)
	}
}

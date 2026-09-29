// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

//go:build verifydom

package webconsole

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

func TestDOMBuiltinLoomAndHostTilesRender(t *testing.T) {
	t.Setenv("OUTPOST_ADMIN_ADDR", "")
	withOutpostAdminResolver(t, outpostConfig{}, nil, "127.0.0.1:17777")
	base, ctx, errs := domEnv(t, Options{})

	var raw string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(base+"/"),
		chromedp.Sleep(1500*time.Millisecond),
		chromedp.Evaluate(`JSON.stringify([...document.querySelectorAll(".tile .label")].map(e => e.textContent))`, &raw),
	); err != nil {
		t.Fatalf("chromedp: %v", err)
	}
	assertNoJSErrors(t, "builtin tiles", errs())

	var labels []string
	if err := json.Unmarshal([]byte(raw), &labels); err != nil {
		t.Fatalf("decode labels: %v (%s)", err, raw)
	}
	seen := map[string]bool{}
	for _, label := range labels {
		seen[label] = true
	}
	for _, want := range []string{"Loom", "Host"} {
		if !seen[want] {
			t.Fatalf("launcher labels = %v, missing %s", labels, want)
		}
	}
}

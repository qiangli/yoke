package cdpactions

import (
	"context"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// A stalled page RPC outlives the old 15-second cleanup budget. Restoration
// must get a fresh deadline after the operation expires, on the same target.
func TestStitchRestoresAfterLongOperationTimeout(t *testing.T) {
	if os.Getenv("BASHY_BROWSER_LIVE") == "" {
		t.Skip("requires real Chrome")
	}
	alloc, stopAlloc := chromedp.NewExecAllocator(context.Background(), chromedp.DefaultExecAllocatorOptions[:]...)
	defer stopAlloc()
	ctx, stop := chromedp.NewContext(alloc)
	defer stop()
	ctx, deadline := context.WithTimeout(ctx, 35*time.Second)
	defer deadline()
	fixture := `<body style="margin:0"><header id="fixed" style="position:fixed;top:0">fixed</header><div style="height:200px"></div><div id="panel" style="height:100px;overflow:auto"><div style="height:500px">rows</div></div><div style="height:2000px"></div></body>`
	if err := chromedp.Run(ctx, chromedp.Navigate("data:text/html,"+url.PathEscape(fixture)), chromedp.Evaluate(`window.scrollTo(0,80); document.getElementById('panel').scrollTop=40`, nil)); err != nil {
		t.Fatal(err)
	}
	world, err := shotWorld(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := worldEval(ctx, world, `(() => { const original=__bashyPage.shotSet; __bashyPage.shotSet=async p => { await original(p); return new Promise(()=>{}); }; return true; })()`, nil); err != nil {
		t.Fatal(err)
	}
	op, cancel := context.WithTimeout(ctx, 16*time.Second)
	defer cancel()
	if _, _, err := StitchFullPage(op); err == nil {
		t.Fatal("stalled capture unexpectedly succeeded")
	}
	var restored bool
	if err := chromedp.Run(ctx, chromedp.Evaluate(`window.scrollY===80 && document.getElementById('panel').scrollTop===40 && getComputedStyle(document.getElementById('fixed')).visibility!=='hidden'`, &restored)); err != nil {
		t.Fatal(err)
	}
	if !restored {
		t.Fatal("timed-out capture left scroll positions or fixed content changed")
	}
}

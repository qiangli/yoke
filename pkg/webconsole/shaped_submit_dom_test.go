//go:build verifydom

package webconsole

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/qiangli/yoke/pkg/shape/state"
)

// The browser must invoke the real submit route and display its review record.
func TestDOMShapeSubmitForReview(t *testing.T) {
	e := newSubmitWebEnv(t)
	base, ctx, jsErrors := domEnv(t, e.options)
	_, ticket, err := newPairStore(e.options.PairStorePath).issueTicketForPrincipal([]string{"notes"}, time.Hour, time.Minute, "alice")
	if err != nil {
		t.Fatal(err)
	}
	var status string
	if err = chromedp.Run(ctx,
		chromedp.Navigate(base+pairRedeemPath+"?v="+pairQRVersion+"&t="+url.QueryEscape(ticket)),
		chromedp.Navigate(base+"/shape/notes/"),
		chromedp.WaitVisible(`#history-list button[aria-label^="Submit version"]`, chromedp.ByQuery),
		chromedp.Click(`#history-list button[aria-label^="Submit version"]`, chromedp.ByQuery),
		chromedp.Poll(`document.getElementById("history-status").textContent.includes("Submitted for review:")`, nil, chromedp.WithPollingTimeout(15*time.Second)),
		chromedp.Text(`#history-status`, &status, chromedp.ByQuery),
	); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(status, "proposal ") || !strings.Contains(status, ", run ") {
		t.Fatalf("missing review identity %q", status)
	}
	store, err := state.OpenAt(e.home, "notes", e.caller.Principal)
	if err != nil {
		t.Fatal(err)
	}
	current, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if current.ActiveRevision != e.candidate.Rev {
		t.Fatal("submit changed active app")
	}
	if got := jsErrors(); len(got) > 0 {
		t.Fatal(got)
	}
}

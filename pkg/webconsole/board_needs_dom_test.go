//go:build verifydom

package webconsole

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/chromedp/chromedp"
	"github.com/qiangli/yoke/pkg/ask"
)

// The Needs-you alert leads the board: a loopback reader sees each request
// and the shell line that answers it, and no buttons.
func TestDOMSprintNeedsYouReadOnly(t *testing.T) {
	stubFilterBoard(t)
	isolateNeedsStore(t)
	raiseNeed(t, ask.NeedSpec{Kind: ask.NeedDecision, Title: "Ship the check-in hotfix?",
		Options: []ask.ApprovalOption{{ID: "ship", Name: "Ship now"}, {ID: "hold"}}, Recommend: "ship", Risk: []string{"deploy"}})
	base, ctx, errs := domEnvWrapped(t, Options{}, nil)
	var text string
	var buttons int
	if err := chromedp.Run(ctx,
		chromedp.Navigate(base+"/sprint/"),
		chromedp.WaitVisible(`#bd-needs .bd-need`, chromedp.ByQuery),
		chromedp.Text(`#bd-needs`, &text, chromedp.ByQuery),
		chromedp.Evaluate(`document.querySelectorAll("#bd-needs button").length`, &buttons),
	); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "Needs you (1)") || !strings.Contains(text, "Ship the check-in hotfix?") ||
		!strings.Contains(text, "bashy approve allow") || !strings.Contains(text, "risk deploy") {
		t.Errorf("alert text = %q", text)
	}
	if buttons != 0 {
		t.Errorf("a reader who cannot answer was offered %d buttons", buttons)
	}
	assertNoJSErrors(t, "needs read-only", errs())
}

// A viewer the server says can answer gets one button per option, the
// recommendation marked, and a click posts that option.
func TestDOMSprintNeedsYouAnswerButtons(t *testing.T) {
	stubFilterBoard(t)
	isolateNeedsStore(t)
	r := raiseNeed(t, ask.NeedSpec{Kind: ask.NeedDecision, Title: "Review the lifted specs",
		Options: []ask.ApprovalOption{{ID: "accept", Name: "Accept"}, {ID: "rework", Name: "Rework"}}, Recommend: "accept"})
	var mu sync.Mutex
	var posted []map[string]any
	base, ctx, errs := domEnvWrapped(t, Options{}, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			switch {
			case req.Method == http.MethodGet && req.URL.Path == "/api/sprint":
				rec := httptest.NewRecorder()
				next.ServeHTTP(rec, req)
				var v map[string]any
				_ = json.Unmarshal(rec.Body.Bytes(), &v)
				v["can_answer"] = true
				mu.Lock()
				if len(posted) > 0 {
					v["needs_you"] = []any{}
				}
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(v)
			case req.Method == http.MethodPost && strings.HasPrefix(req.URL.Path, "/api/sprint/needs/"):
				b, _ := io.ReadAll(req.Body)
				var body map[string]any
				_ = json.Unmarshal(bytes.TrimSpace(b), &body)
				body["id"] = strings.TrimPrefix(req.URL.Path, "/api/sprint/needs/")
				mu.Lock()
				posted = append(posted, body)
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"ok":true}`))
			default:
				next.ServeHTTP(w, req)
			}
		})
	})
	var labels string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(base+"/sprint/"),
		chromedp.WaitVisible(`#bd-needs .bd-need button`, chromedp.ByQuery),
		chromedp.Evaluate(`[...document.querySelectorAll("#bd-needs button")].map(b=>b.textContent).join("|")`, &labels),
		chromedp.SendKeys(`#bd-needs input[type=text]`, "tests green", chromedp.ByQuery),
		chromedp.Click(`#bd-needs button.rec`, chromedp.ByQuery),
		chromedp.Poll(`!document.querySelector("#bd-needs .bd-need")`, nil),
	); err != nil {
		t.Fatal(err)
	}
	if labels != "Accept (recommended)|Rework|Decline all" {
		t.Errorf("buttons = %q", labels)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(posted) != 1 || posted[0]["verdict"] != "allow" || posted[0]["option"] != "accept" ||
		posted[0]["reason"] != "tests green" || posted[0]["id"] != r.ID {
		t.Errorf("posted = %v", posted)
	}
	assertNoJSErrors(t, "needs answer", errs())
}

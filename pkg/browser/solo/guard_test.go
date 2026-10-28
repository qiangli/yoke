package solo

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"

	"github.com/qiangli/yoke/pkg/browser/guard"
	"github.com/qiangli/yoke/pkg/browser/wire"
)

// The sensitive-input guard end to end through a real (headless) Chrome:
// solo — and probe, which shares cdpactions — runs the SAME guard.js field
// check the live extension injects, through the same gate. Opt-in like the
// solo smoke test: BASHY_BROWSER_LIVE=1.

const typeformSSN = `<!doctype html><html><body>
<div data-qa="question"><h1>Social Security Number *</h1>
<input type="text" id="answer" placeholder="Type your answer here..."><button>OK</button></div>
</body></html>`

const orderPage = `<!doctype html><html><body><form>
<label>Order number <input id="order"></label>
<label>Phone <input id="phone" type="tel"></label>
<label>Notes <input id="notes"></label>
</form><p>Confirmation: SSN 212-48-7731 on file.</p></body></html>`

func TestSoloGuardEndToEnd(t *testing.T) {
	if os.Getenv("BASHY_BROWSER_LIVE") == "" {
		t.Skip("live Chrome test disabled; set BASHY_BROWSER_LIVE=1 to run (launches a real Chrome)")
	}
	root := t.TempDir()
	t.Setenv("BASHY_ASK_DIR", filepath.Join(root, "ask"))
	t.Setenv("BASHY_HOME", filepath.Join(root, "home"))
	t.Setenv("BASHY_ROOM_DIR", filepath.Join(root, "home", "room"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("BASHY_SECRETS_TOKEN", "")
	t.Setenv("BASHY_API_KEY", "")

	mux := http.NewServeMux()
	for path, body := range map[string]string{"/ssn": typeformSSN, "/order": orderPage} {
		body := body
		mux.HandleFunc(path, func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, body) })
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ctx := context.Background()
	c := New(Config{})
	if !c.Available(ctx) {
		t.Skip("no Chrome or Chromium binary found")
	}
	defer c.Close()
	exec := func(a wire.Action) *wire.Result {
		t.Helper()
		res, err := c.Execute(ctx, a)
		if err != nil {
			t.Fatalf("%s: %v", a.Type, err)
		}
		return res
	}
	value := func(sel string) string {
		var v string
		if err := chromedp.Run(c.current(), chromedp.Evaluate(fmt.Sprintf("document.querySelector(%q).value", sel), &v)); err != nil {
			t.Fatal(err)
		}
		return v
	}

	exec(wire.Action{Type: wire.ActionNavigate, URL: srv.URL + "/ssn"})
	// Headless solo has no human: stopped, nothing typed.
	res := exec(wire.Action{Type: wire.ActionType, Selector: "#answer", Text: "Ada"})
	if res.OutcomeClass != guard.ClassStopped || res.Reason != "field:label-ssn" || value("#answer") != "" {
		t.Fatalf("headless: %+v value=%q", res, value("#answer"))
	}
	// The scripted operator chooses type-myself: the field is focused, still
	// empty, and the caller sees only the outcome.
	t.Setenv(guard.FakeAskEnv, "fake:type-myself")
	res = exec(wire.Action{Type: wire.ActionType, Selector: "#answer", Text: "Ada"})
	var active string
	_ = chromedp.Run(c.current(), chromedp.Evaluate("document.activeElement.id", &active))
	if res.OutcomeClass != guard.ClassHumanTyped || active != "answer" || value("#answer") != "" {
		t.Fatalf("type-myself: %+v active=%q value=%q", res, active, value("#answer"))
	}
	t.Setenv(guard.FakeAskEnv, "")

	exec(wire.Action{Type: wire.ActionNavigate, URL: srv.URL + "/order"})
	// Ordinary values type normally.
	for sel, v := range map[string]string{"#order": "ORD-2026-001234", "#phone": "(415) 555-0132"} {
		if res := exec(wire.Action{Type: wire.ActionType, Selector: sel, Text: v}); !res.Success || value(sel) != v {
			t.Fatalf("type %s: %+v", sel, res)
		}
	}
	// A card number into an innocuous field is refused by the value check.
	res = exec(wire.Action{Type: wire.ActionType, Selector: "#notes", Text: "4111 1111 1111 1111"})
	if res.Reason != "value:luhn" || value("#notes") != "" {
		t.Fatalf("card into notes: %+v value=%q", res, value("#notes"))
	}
	// extract masks the SSN the page shows.
	res = exec(wire.Action{Type: wire.ActionExtract})
	if strings.Contains(res.Content, "212-48-7731") || !strings.Contains(res.Content, "[guarded:ssn]") {
		t.Fatalf("extract not masked: %q", res.Content)
	}
}

// probe/solo extract never lists a guarded field's value.
func TestSoloExtractHidesGuardedValues(t *testing.T) {
	if os.Getenv("BASHY_BROWSER_LIVE") == "" {
		t.Skip("live Chrome test disabled; set BASHY_BROWSER_LIVE=1 to run (launches a real Chrome)")
	}
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("BASHY_SECRETS_TOKEN", "")
	t.Setenv("BASHY_API_KEY", "")
	page := `<!doctype html><html><body><label>Password <input type="password" id="pw" value="hunter2-initial"></label>
<label>Nickname <input id="nick" value="ada"></label></body></html>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, page) }))
	defer srv.Close()
	ctx := context.Background()
	c := New(Config{})
	if !c.Available(ctx) {
		t.Skip("no Chrome or Chromium binary found")
	}
	defer c.Close()
	if _, err := c.Execute(ctx, wire.Action{Type: wire.ActionNavigate, URL: srv.URL}); err != nil {
		t.Fatal(err)
	}
	res, err := c.Execute(ctx, wire.Action{Type: wire.ActionExtract})
	if err != nil || !res.Success {
		t.Fatalf("%v %+v", err, res)
	}
	if strings.Contains(res.Elements+res.Data, "hunter2") || !strings.Contains(res.Elements, `guarded="field:type-password"`) || !strings.Contains(res.Elements, `value="ada"`) {
		t.Fatalf("extract listing:\n%s", res.Elements)
	}
}

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

// typeformTrunc is a page that truncates whatever is typed to five
// characters: the operator's answer cannot stick, so the fill must fail.
const typeformTrunc = `<!doctype html><html><body>
<div data-qa="question"><h1>Social Security Number *</h1>
<input type="text" id="answer" oninput="this.value=this.value.slice(0,5)"><button>OK</button></div>
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
	for path, body := range map[string]string{"/ssn": typeformSSN, "/trunc": typeformTrunc, "/order": orderPage} {
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
	// No human channel installed: stopped, nothing typed.
	res := exec(wire.Action{Type: wire.ActionType, Selector: "#answer", Text: "Ada"})
	if res.OutcomeClass != guard.ClassStopped || res.Reason != "field:label-ssn" || value("#answer") != "" {
		t.Fatalf("no asker: %+v value=%q", res, value("#answer"))
	}
	// Headless: nobody can type into this browser, so "self" is not on
	// offer — an operator who picks it anyway stops the form.
	t.Setenv(guard.FakeAskEnv, "fake:self")
	res = exec(wire.Action{Type: wire.ActionType, Selector: "#answer", Text: "Ada"})
	if res.OutcomeClass != guard.ClassStopped || value("#answer") != "" {
		t.Fatalf("self headless: %+v value=%q", res, value("#answer"))
	}
	// The operator PROVIDES the answer: it is filled by reference through
	// CDP; the agent's text never reaches the page and the result carries
	// only the outcome.
	t.Setenv(guard.FakeAskEnv, "fake:provide")
	t.Setenv(guard.FakeValueEnv, "078-05-1120")
	res = exec(wire.Action{Type: wire.ActionType, Selector: "#answer", Text: "Ada"})
	if res.OutcomeClass != guard.ClassFilledByHuman || value("#answer") != "078-05-1120" ||
		strings.Contains(res.Content+res.Data+res.Error, "078-05-1120") {
		t.Fatalf("provide: %+v value=%q", res, value("#answer"))
	}
	if !strings.Contains(res.Data, `"verified":"match"`) || !strings.Contains(res.Data, `"field_len":11`) {
		t.Fatalf("provide not verified: %s", res.Data)
	}
	// A page that truncates the answer: the fill is verified as a mismatch
	// and FAILS (exit 6) — only the verdict and the lengths come back.
	exec(wire.Action{Type: wire.ActionNavigate, URL: srv.URL + "/trunc"})
	res = exec(wire.Action{Type: wire.ActionType, Selector: "#answer", Text: "Ada"})
	if code, _ := guard.ExitCode(res); res.OutcomeClass != guard.ClassFillMismatch || code != guard.ExitFillMismatch || res.Success ||
		!strings.Contains(res.Data, `"verified":"mismatch"`) || !strings.Contains(res.Data, `"field_len":5`) ||
		!strings.Contains(res.Data, `"value_len":11`) || strings.Contains(res.Content+res.Data+res.Error, "078-0") {
		t.Fatalf("truncated provide: %+v value=%q", res, value("#answer"))
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

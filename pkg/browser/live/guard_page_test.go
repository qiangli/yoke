package live

import (
	"fmt"
	"strings"
	"testing"
)

// Page fixtures for the sensitive-input guard's FIELD check (extension
// guard.js, run through page.js exactly as the extension runs it). Opt-in
// like the other page tests: BASHY_BROWSER_LIVE=1 go test ./pkg/browser/live -run TestGuard

// typeformScreen renders a Typeform-style screen: ONE question per screen,
// the question in a heading over a plain, unlabelled text box, with the
// previous and next screens still in the DOM but hidden — the shape where
// only the VISIBLE QUESTION TEXT says what the field is.
func typeformScreen(question, input string) string {
	return fmt.Sprintf(`<!doctype html><html><body>
<header><a href="/">Acme forms</a></header>
<div class="screen" style="display:none"><h1>What's your first name?</h1><input type="text" name="prev"></div>
<div class="screen" data-qa="question">
  <div class="q-head"><span class="num">3 →</span><h1><span>%s</span><span>*</span></h1></div>
  <p class="hint">Your answer is required.</p>
  <div class="q-input">%s</div>
  <button type="button">OK ✓</button> <span>press Enter ↵</span>
</div>
<div class="screen" style="display:none"><h1>Anything else?</h1><textarea name="next"></textarea></div>
<footer>Powered by Typeform</footer>
</body></html>`, question, input)
}

const plainBox = `<input type="text" id="answer" placeholder="Type your answer here...">`

// (a) The visible question text counts as the field's label: refused
// BEFORE typing whatever the answer, per kind.
func TestGuardFieldReadsTheVisibleQuestion(t *testing.T) {
	cases := map[string]string{
		"/ssn":      "Social Security Number",
		"/ssn2":     "Please enter your SSN",
		"/tax":      "Tax ID (TIN / EIN)",
		"/ein":      "Employer Identification Number",
		"/bank":     "Bank account",
		"/routing":  "Bank routing number",
		"/card":     "Credit card number",
		"/password": "Password",
		"/username": "Username",
	}
	want := map[string]string{
		"/ssn": "field:label-ssn", "/ssn2": "field:label-ssn", "/tax": "field:label-tax",
		"/ein": "field:label-ein", "/bank": "field:label-account", "/routing": "field:label-routing",
		"/card": "field:label-card", "/password": "field:label-password", "/username": "field:label-username",
	}
	fixtures := map[string]string{}
	for path, q := range cases {
		fixtures[path] = typeformScreen(q, plainBox)
	}
	h := newPageHarness(t, fixtures)
	for path, q := range cases {
		h.open(path)
		g := h.call("guardField", map[string]any{"selector": "#answer"})
		if g["guarded"] != true || g["reason"] != want[path] {
			t.Errorf("%s (%q): guardField = %v, want guarded %s", path, q, g, want[path])
		}
		if qt, _ := g["question"].(string); !strings.Contains(qt, q) || strings.Contains(qt, "first name") || strings.Contains(qt, "Powered") {
			t.Errorf("%s: question text = %q, want the visible question only", path, qt)
		}
		// page.js type refuses on its own (a client that skipped the gate),
		// without echoing the text.
		res := h.call("type", map[string]any{"selector": "#answer", "text": "Ada Lovelace"})
		msg, _ := res["error"].(string)
		if !strings.Contains(msg, "sensitive-input guard") || strings.Contains(msg, "Ada Lovelace") {
			t.Errorf("%s: type was not refused page-side (or echoed the text): %v", path, res)
		}
		var v string
		h.mainEval("document.querySelector('#answer').value", &v)
		if v != "" {
			t.Errorf("%s: the guarded field was filled: %q", path, v)
		}
	}
}

// Field kinds: input type, autocomplete tokens, name/id, <label>,
// aria-label, aria-labelledby, placeholder — each refused whatever the value.
func TestGuardFieldKinds(t *testing.T) {
	page := `<!doctype html><html><body><form>
<div><input type="password" id="pw"></div>
<div><input id="ac-user" autocomplete="username"></div>
<div><input id="ac-otp" autocomplete="one-time-code"></div>
<div><input id="ac-cc" autocomplete="cc-number"></div>
<div><input id="ac-cvc" autocomplete="section-pay cc-csc"></div>
<div><input id="nm" name="taxId"></div>
<div><input id="routing_number"></div>
<div><label for="lb">Account number</label><input id="lb"></div>
<div><input id="aria" aria-label="Card security code"></div>
<div><span id="lbl">Social security no.</span><input id="by" aria-labelledby="lbl"></div>
<div><input id="ph" placeholder="Enter password"></div>
</form></body></html>`
	want := map[string]string{
		"#pw": "field:type-password", "#ac-user": "field:autocomplete-username", "#ac-otp": "field:autocomplete-one-time-code",
		"#ac-cc": "field:autocomplete-cc-number", "#ac-cvc": "field:autocomplete-cc-csc", "#nm": "field:name-tax",
		"#routing_number": "field:name-routing", "#lb": "field:label-account", "#aria": "field:label-card",
		"#by": "field:label-ssn", "#ph": "field:label-password",
	}
	h := newPageHarness(t, map[string]string{"/kinds": page})
	h.open("/kinds")
	for sel, reason := range want {
		g := h.call("guardField", map[string]any{"selector": sel})
		if g["guarded"] != true || g["reason"] != reason {
			t.Errorf("%s: %v, want %s", sel, g, reason)
		}
	}
}

// The false-positive page: order numbers, phone numbers, dates, ZIP codes
// and amounts — fields an agent fills all the time — pass the field check
// and type normally.
func TestGuardFalsePositivePagePassesThrough(t *testing.T) {
	page := `<!doctype html><html><body><h1>Order lookup</h1><form>
<div><label for="order">Order number</label><input id="order" name="order_number"></div>
<div><label for="phone">Phone number</label><input id="phone" type="tel" autocomplete="tel"></div>
<div><label for="date">Delivery date</label><input id="date" name="delivery_date"></div>
<div><label for="zip">ZIP code</label><input id="zip" autocomplete="postal-code"></div>
<div><label for="amt">Amount (USD)</label><input id="amt" name="amount"></div>
<div><label for="nm">Full name</label><input id="nm" autocomplete="name"></div>
<div><label for="em">Email</label><input id="em" type="email" autocomplete="email"></div>
</form></body></html>`
	h := newPageHarness(t, map[string]string{"/fp": page, "/fp1": typeformScreen("What's your order number?", plainBox)})
	h.open("/fp")
	values := map[string]string{"#order": "ORD-2026-001234", "#phone": "(415) 555-0132", "#date": "2026-10-28",
		"#zip": "94105-1234", "#amt": "1234.56", "#nm": "Ada Lovelace", "#em": "ada@example.com"}
	for sel, v := range values {
		if g := h.call("guardField", map[string]any{"selector": sel}); g["guarded"] != false {
			t.Errorf("%s flagged: %v", sel, g)
		}
		if res := h.call("type", map[string]any{"selector": sel, "text": v}); res["error"] != nil {
			t.Errorf("type %s: %v", sel, res["error"])
		}
	}
	h.open("/fp1")
	if g := h.call("guardField", map[string]any{"selector": "#answer"}); g["guarded"] != false {
		t.Errorf("typeform order-number screen flagged: %v", g)
	}
}

// (4) extract never shows a guarded field's value — not even one a human
// typed — and marks the field guarded.
func TestGuardExtractHidesGuardedValues(t *testing.T) {
	page := `<!doctype html><html><body><form>
<label>Password <input type="password" id="pw" value="hunter2-initial"></label>
<label>Card number <input id="cc" value="4111111111111111"></label>
<label>Nickname <input id="nick" value="ada"></label>
</form></body></html>`
	h := newPageHarness(t, map[string]string{"/x": page})
	h.open("/x")
	ex := h.call("extract", map[string]any{})
	el := ex["elements"].(string)
	if strings.Contains(el, "hunter2") || strings.Contains(el, "4111111111111111") {
		t.Fatalf("extract shows a guarded value:\n%s", el)
	}
	if !strings.Contains(el, `guarded="field:type-password"`) || !strings.Contains(el, `value="ada"`) {
		t.Fatalf("extract listing wrong:\n%s", el)
	}
	if strings.Contains(ex["data"].(string), "hunter2") {
		t.Fatal("extract data shows a guarded value")
	}
}

// The human path's focus: guardFocus puts the caret in the refused field
// (and nothing types into it).
func TestGuardFocusHandsTheFieldToTheHuman(t *testing.T) {
	h := newPageHarness(t, map[string]string{"/ssn": typeformScreen("Social Security Number", plainBox)})
	h.open("/ssn")
	if res := h.call("guardFocus", map[string]any{"selector": "#answer"}); res["error"] != nil {
		t.Fatal(res["error"])
	}
	var active, value string
	h.mainEval("document.activeElement && document.activeElement.id", &active)
	h.mainEval("document.querySelector('#answer').value", &value)
	if active != "answer" || value != "" {
		t.Fatalf("focus=%q value=%q, want the empty field focused", active, value)
	}
	// With no target, guardField reads the focused element (keypress path).
	if g := h.call("guardField", map[string]any{}); g["guarded"] != true {
		t.Fatalf("focused guarded field not reported: %v", g)
	}
}

// No page.js message describes a guarded field by its contents.
func TestGuardMessagesNeverEchoAGuardedValue(t *testing.T) {
	page := `<!doctype html><html><body><label>Password <input type="password" id="pw" value="hunter2-initial"></label></body></html>`
	h := newPageHarness(t, map[string]string{"/x": page})
	h.open("/x")
	for _, fn := range []string{"click", "type", "guardFocus"} {
		res := h.call(fn, map[string]any{"selector": "#pw", "text": "x"})
		if b := fmt.Sprint(res); strings.Contains(b, "hunter2") {
			t.Errorf("%s echoed the field's value: %s", fn, b)
		}
	}
}

// provide: guardFill types an operator's ADMITTED answer into a guarded
// field (the hub checked the admission first) and answers without the value;
// an unguarded type path is untouched and the agent's own type still fails.
func TestGuardFillTypesTheOperatorsAnswer(t *testing.T) {
	h := newPageHarness(t, map[string]string{
		"/ssn": typeformScreen("Social Security Number", plainBox),
		"/pw":  typeformScreen("Sign in", `<input type="password" id="answer">`),
	})
	for _, path := range []string{"/ssn", "/pw"} {
		h.open(path)
		if res := h.call("type", map[string]any{"selector": "#answer", "text": "agent text"}); res["error"] == nil {
			t.Fatalf("%s: the agent's type went through: %v", path, res)
		}
		res := h.call("guardFill", map[string]any{"selector": "#answer", "text": "078-05-1120"})
		if res["content"] != "filled" || res["error"] != nil {
			t.Fatalf("%s: guardFill = %v", path, res)
		}
		if b := fmt.Sprint(res); strings.Contains(b, "078-05-1120") {
			t.Fatalf("%s: guardFill echoed the value: %s", path, b)
		}
		var v string
		h.mainEval("document.querySelector('#answer').value", &v)
		if v != "078-05-1120" {
			t.Fatalf("%s: field holds %q", path, v)
		}
	}
	// A field that rejects the value: the error names the field, not the value.
	h2 := newPageHarness(t, map[string]string{"/reject": typeformScreen("SSN",
		`<input type="text" id="answer" oninput="this.value=''">`)})
	h2.open("/reject")
	res := h2.call("guardFill", map[string]any{"selector": "#answer", "text": "078-05-1120"})
	if msg, _ := res["error"].(string); msg == "" || strings.Contains(fmt.Sprint(res), "078-05-1120") {
		t.Fatalf("rejected guardFill = %v", res)
	}
}

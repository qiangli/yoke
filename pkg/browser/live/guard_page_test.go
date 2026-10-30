package live

import (
	"crypto/sha256"
	"encoding/hex"
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

const ssnBox = `<input type="text" id="answer" aria-label="Social Security Number">`

// (a) The surrounding/visible question never classifies a field: a
// Typeform heading over an unlabelled box is NOT guarded whatever it says —
// but the question text is still reported for the ask. The field's OWN
// identity (label, aria-label, name) still guards.
func TestGuardFieldIgnoresSurroundingQuestion(t *testing.T) {
	cases := map[string]string{
		"/aws":    "AWS account ID",
		"/org":    "Org ID",
		"/nvidia": "Which NVIDIA card and account tier do you use for your password manager GPU?",
		"/bank":   "Bank account number",
		"/card":   "Credit card number",
	}
	fixtures := map[string]string{}
	for path, q := range cases {
		fixtures[path] = typeformScreen(q, plainBox)
	}
	h := newPageHarness(t, fixtures)
	for path, q := range cases {
		h.open(path)
		g := h.call("guardField", map[string]any{"selector": "#answer"})
		if g["guarded"] != false {
			t.Errorf("%s (%q): heading alone guarded the field: %v", path, q, g)
		}
		if qt, _ := g["question"].(string); !strings.Contains(qt, q) || strings.Contains(qt, "first name") || strings.Contains(qt, "Powered") {
			t.Errorf("%s: question text = %q, want the visible question only", path, qt)
		}
	}
}

// Red/green: ordinary identifiers pass; the credential/bank/card set stays
// guarded. describedby/help text naming a sensitive word never counts.
func TestGuardFieldOrdinaryVersusSensitive(t *testing.T) {
	page := `<!doctype html><html><body><form>
<div><label for="aws">AWS account ID</label><input id="aws"></div>
<div><label for="org">Org ID</label><input id="org" name="org_id"></div>
<div><label for="em">Company email</label><input id="em" type="email" autocomplete="email"></div>
<div><label for="cc">CC</label><input id="cc" name="cc_recipients"></div>
<div><label for="hp">Project name</label><input id="hp" aria-describedby="help"><span id="help">Never enter your password, card number or bank account here</span></div>
<div><label for="gpu">NVIDIA technology you use</label><input id="gpu" placeholder="e.g. GPU card"></div>
<div><label for="pw">Password</label><input id="pw"></div>
<div><label for="otp">One time code</label><input id="otp"></div>
<div><label for="ba">Bank account number</label><input id="ba"></div>
<div><label for="cn">Card number</label><input id="cn"></div>
<div><label for="cv">CVV</label><input id="cv"></div>
<div><label for="a1">Account number</label><input id="a1"></div>
<div><label for="a2">Acct #</label><input id="a2"></div>
<div><label for="a3">Account no.</label><input id="a3"></div>
<div><label for="a4">Routing number</label><input id="a4"></div>
</form></body></html>`
	h := newPageHarness(t, map[string]string{"/mix": page})
	h.open("/mix")
	for _, sel := range []string{"#aws", "#org", "#em", "#cc", "#hp", "#gpu"} {
		if g := h.call("guardField", map[string]any{"selector": sel}); g["guarded"] != false {
			t.Errorf("%s guarded: %v", sel, g)
		}
	}
	for sel, reason := range map[string]string{"#pw": "field:label-password", "#otp": "field:name-otp",
		"#ba": "field:label-account", "#cn": "field:label-card", "#cv": "field:label-card",
		"#a1": "field:label-account", "#a2": "field:label-account", "#a3": "field:label-account", "#a4": "field:label-routing"} {
		if g := h.call("guardField", map[string]any{"selector": sel}); g["guarded"] != true || g["reason"] != reason {
			t.Errorf("%s: %v, want guarded %s", sel, g, reason)
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
<div><label for="lb">Bank account number</label><input id="lb"></div>
<div><input id="aria" aria-label="Card security code"></div>
<div><span id="lbl">Social security no.</span><input id="by" aria-labelledby="lbl"></div>
<div><input id="ph" placeholder="Enter password"></div>
</form></body></html>`
	want := map[string]string{
		"#pw": "field:type-password", "#ac-otp": "field:autocomplete-one-time-code",
		"#ac-cc": "field:autocomplete-cc-number", "#ac-cvc": "field:autocomplete-cc-csc", "#nm": "field:name-tax",
		"#routing_number": "field:name-routing", "#lb": "field:label-account", "#aria": "field:label-card",
		"#by": "field:label-ssn", "#ph": "field:label-password", "#ac-user": "field:autocomplete-username",
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
	if !strings.Contains(el, `current="4111111111111111"`) {
		t.Fatalf("non-password reads must be unmasked:\n%s", el)
	}
}

// The human path's focus: guardFocus puts the caret in the refused field
// (and nothing types into it).
func TestGuardFocusHandsTheFieldToTheHuman(t *testing.T) {
	h := newPageHarness(t, map[string]string{"/ssn": typeformScreen("Social Security Number", ssnBox)})
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
		if res["content"] != "filled" || res["error"] != nil || res["verified"] != "match" ||
			res["field_len"] != float64(11) || res["value_len"] != float64(11) {
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

// The by-reference fill is VERIFIED in the page: guardVerify (run by
// background.js after the synthetic and trusted inserts) compares the field
// with the operator's answer and answers only match/mismatch and the two
// lengths. A page that accepts the value matches; one that rewrites,
// truncates or clears it does not — and nothing of the value, nor a digest
// of it, comes back.
func TestGuardFillVerifiesTheField(t *testing.T) {
	const value = "078-05-1120"
	cases := map[string]struct {
		input    string
		verified string
		fieldLen float64
	}{
		"/accept":   {plainBox, "match", 11},
		"/truncate": {`<input type="text" id="answer" oninput="this.value=this.value.slice(0,5)">`, "mismatch", 5},
		"/rewrite":  {`<input type="text" id="answer" oninput="this.value=this.value.replace(/-/g,'')">`, "mismatch", 9},
		"/clear":    {`<input type="text" id="answer" oninput="this.value=''">`, "mismatch", 0},
	}
	fixtures := map[string]string{}
	for path, tc := range cases {
		fixtures[path] = typeformScreen("Social Security Number", tc.input)
	}
	h := newPageHarness(t, fixtures)
	sum := sha256.Sum256([]byte(value))
	for path, tc := range cases {
		h.open(path)
		fill := h.call("guardFill", map[string]any{"selector": "#answer", "text": value})
		res := h.call("guardVerify", map[string]any{"selector": "#answer", "text": value})
		if res["verified"] != tc.verified || res["field_len"] != tc.fieldLen || res["value_len"] != float64(11) {
			t.Fatalf("%s: guardVerify = %v", path, res)
		}
		if tc.verified == "match" && fill["verified"] != "match" {
			t.Fatalf("%s: guardFill = %v", path, fill)
		}
		for _, out := range []string{fmt.Sprint(fill), fmt.Sprint(res)} {
			for _, leak := range []string{value, "07805", "078051120", hex.EncodeToString(sum[:]), hex.EncodeToString(sum[:8])} {
				if strings.Contains(out, leak) {
					t.Fatalf("%s: the value (or a digest) came back: %s", path, out)
				}
			}
		}
	}
}

// Done/Skip/Stop: the human-focus outline is removed again (and an outline
// the page had set is restored).
func TestGuardReleaseClearsHumanFocusOutline(t *testing.T) {
	page := `<!doctype html><html><body><input id="answer" aria-label="Social Security Number" style="outline:1px dotted blue"></body></html>`
	h := newPageHarness(t, map[string]string{"/r": page})
	h.open("/r")
	h.call("guardFocus", map[string]any{"selector": "#answer"})
	var o string
	h.mainEval("document.querySelector('#answer').style.outline", &o)
	if !strings.Contains(o, "217") && !strings.Contains(o, "d93025") {
		t.Fatalf("focus did not outline the field: %q", o)
	}
	if res := h.call("guardFocus", map[string]any{"selector": "#answer", "release": true}); res["error"] != nil {
		t.Fatal(res["error"])
	}
	h.mainEval("document.querySelector('#answer').style.outline", &o)
	if strings.Contains(o, "217") || strings.Contains(o, "d93025") || !strings.Contains(o, "dotted") {
		t.Fatalf("outline not restored: %q", o)
	}
}

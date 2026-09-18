// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"net/http"
	"strings"
	"testing"
)

// The login form's show/hide-password control swaps two <svg> icons. Those are
// SVGElement, and `hidden` is an IDL attribute of HTMLElement, which SVGElement
// does NOT inherit — so `eye.hidden = true` sets a meaningless JS expando,
// leaves the real attribute untouched, and the icons never swap. That shipped
// once; this pins the fix.
//
// It is a source-level assertion because this package has no browser harness.
// It therefore checks the one thing that distinguishes correct from broken:
// the toggle drives the ATTRIBUTE, and never assigns the property on an icon.
func TestLoginPasswordToggleDrivesTheHiddenAttribute(t *testing.T) {
	h := newTestHandler(t, Options{})
	w := do(h, "GET", "/login", "127.0.0.1:5555", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /login = %d", w.Code)
	}
	page := w.Body.String()

	// Both icons must exist, and exactly one starts hidden.
	for _, want := range []string{`class="eye"`, `class="eye-off"`} {
		if !strings.Contains(page, want) {
			t.Errorf("login page is missing the %s icon", want)
		}
	}
	if !strings.Contains(page, `class="eye-off" viewBox="0 0 24 24" aria-hidden="true" hidden`) {
		t.Error("the eye-off icon must start with the hidden ATTRIBUTE set")
	}

	// The defect, stated exactly: assigning .hidden on an SVG element.
	for _, bad := range []string{"eye.hidden =", "eyeOff.hidden ="} {
		if strings.Contains(page, bad) {
			t.Errorf("toggle assigns %q — SVGElement has no hidden property, so the icons never swap; "+
				"set/removeAttribute(\"hidden\") instead", bad)
		}
	}
	for _, want := range []string{`removeAttribute("hidden")`, `setAttribute("hidden", "")`} {
		if !strings.Contains(page, want) {
			t.Errorf("toggle does not call %s — it must drive the attribute, not the property", want)
		}
	}

	// Driving the attribute is only half of it. The UA's [hidden]{display:none}
	// does NOT apply to an inline <svg>, so without an author rule BOTH icons
	// render at once — measured: deleting this rule makes the DOM test next
	// door report eye=block eye-off=block. That was the shipped bug.
	if !strings.Contains(page, `.password-toggle svg[hidden]{display:none}`) {
		t.Error("login CSS is missing .password-toggle svg[hidden]{display:none}; " +
			"the UA hidden rule does not hide an inline <svg>, so both icons would show")
	}
}

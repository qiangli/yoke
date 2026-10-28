// Package guard is bashy browser's sensitive-input guard: the ONE gate every
// browser backend (live, probe, solo) routes a page-writing action through.
//
// A rule agents are asked to follow is not enough, so bashy enforces it:
//
//  1. VALUE check — type/select/evaluate/clipboard_write/dispatch_event/
//     navigate/keyboard_press carry text; that text is checked against strict
//     validators (pkg/sensitive: SSN/ITIN shape rules, EIN prefixes, ABA
//     routing checksum, card Luhn + issuer, IBAN mod-97) and against the
//     bashy vault's values (exact and embedded; digests only, see vault.go).
//  2. FIELD check — before a type (or a printable keypress) the page-side
//     guard (extension guard.js, also injected by probe/solo) inspects the
//     target WITHOUT the value: input type=password, sensitive autocomplete
//     tokens, and name/id/label/aria/placeholder/visible question text
//     saying ssn/tax/ein/routing/account/card/password/username.
//  3. HUMAN PATH — a refusal never fails silently and never fills with the
//     agent's text: the operator gets ONE question (HumanAsker, human.go)
//     naming the site, the question and the reason, with choices provide /
//     self / skip / stop (no answer = stop). provide fills the field BY
//     REFERENCE from the operator's answer (admit.go); self focuses the field
//     in the operator's browser and waits for "done". The value never passes
//     through the agent, the ask record, logs or receipts; the caller sees
//     only the outcome.
//  4. MASK — every result's text (extract content/elements/data, eval data,
//     errors, titles, URLs) has validator- and vault-matched values replaced
//     by [guarded:<kind>]. Screenshots are NOT masked (recorded gap).
//  5. PROTECTED — there is no flag, environment variable or skill metadata
//     that disables any of this. The package, pkg/sensitive (the validators
//     that decide what is blocked), the extension's guard.js and the
//     backends' guard wiring are rows in pkg/src's protected manifest, so a
//     change merges only with a `bashy approve`d src approval.
package guard

import (
	"context"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/qiangli/yoke/pkg/browser/wire"
	"github.com/qiangli/yoke/pkg/sensitive"
)

// Outcome classes and exit statuses of a guarded refusal (the contract
// shared with site skills): filled-by-human 0, human-typed 0, skipped 4,
// stopped 5, fill-mismatch 6 (a by-reference fill whose page field does not
// hold the operator's answer afterwards — the page rewrote, truncated or
// rejected it; the fill FAILED and the form needs a human).
const (
	ClassFilledByHuman = "guard_filled_by_human"
	ClassHumanTyped    = "guard_human_typed"
	ClassSkipped       = "guard_skipped"
	ClassStopped       = "guard_stopped"
	ClassFillMismatch  = "guard_fill_mismatch"

	ContentFilledByHuman = "guard:filled-by-human"
	ContentHumanTyped    = "guard:human-typed"
	ContentSkipped       = "guard:skipped"
	ContentStopped       = "guard:stopped"
	ContentFillMismatch  = "guard:fill-mismatch"

	ExitFilledByHuman = 0
	ExitHumanTyped    = 0
	ExitSkipped       = 4
	ExitStopped       = 5
	ExitFillMismatch  = 6
)

// Verification results of a by-reference fill (data.verified).
const (
	VerifiedMatch    = "match"
	VerifiedMismatch = "mismatch"
)

// Verify is a by-reference fill's read-back, compared where the value
// already is (the page, or the backend's own process): whether the field
// now equals the operator's answer, and both lengths. It never carries the
// value nor any digest of it — an SSN-class value is brute-forceable from a
// hash.
type Verify struct {
	Verified string // VerifiedMatch | VerifiedMismatch
	FieldLen int
	ValueLen int
}

// ExitCode maps a result's outcome class to the CLI exit status; ok is
// false when the result is not a guard outcome.
func ExitCode(r *wire.Result) (int, bool) {
	if r == nil {
		return 0, false
	}
	switch r.OutcomeClass {
	case ClassFilledByHuman:
		return ExitFilledByHuman, true
	case ClassHumanTyped:
		return ExitHumanTyped, true
	case ClassSkipped:
		return ExitSkipped, true
	case ClassStopped:
		return ExitStopped, true
	case ClassFillMismatch:
		return ExitFillMismatch, true
	}
	return 0, false
}

// Field is the page-side verdict on an action's target field.
type Field struct {
	Guarded  bool   `json:"guarded"`
	Reason   string `json:"reason,omitempty"`   // field:type-password, field:label-ssn, …
	Question string `json:"question,omitempty"` // the visible question/label text
	Matched  string `json:"matched,omitempty"`  // <input> description, never a value
	Site     string `json:"site,omitempty"`     // host of the page
}

// Backend is what a browser mode supplies to the gate.
type Backend interface {
	// InspectField runs the page-side field check on the target of a (a
	// type's selector/index/text; a keypress's selector or the focused
	// element). It must NOT send a.Text anywhere.
	InspectField(ctx context.Context, a wire.Action) (Field, error)
	// FocusField focuses a's target in the operator's browser so the
	// human can type into it.
	FocusField(ctx context.Context, a wire.Action) error
	// FillAdmitted types value — an operator's answer the guard admitted
	// for a's target — into that target, bypassing the page-side refusal
	// that stops an agent's own text, then reads the field back and reports
	// whether it equals value (Verify). A field the page rewrote is a
	// mismatch, not an error; an error means nothing could be filled or
	// verified. It must not echo value in any error.
	FillAdmitted(ctx context.Context, a wire.Action, askID string, value []byte) (Verify, error)
	// Site is the host of the driven page ("" when unknown).
	Site(ctx context.Context) string
	// HumanChannel reports whether a human can act in this browser
	// (false for a headless solo Chrome).
	HumanChannel() bool
}

// ExecFunc performs an action that passed the gate.
type ExecFunc func(ctx context.Context, a wire.Action) (*wire.Result, error)

// refusal is why the gate stopped an action. It carries no value.
type refusal struct {
	Reason   string
	Question string
	Site     string
	Matched  string
}

// Execute is THE gate. Backends call it from their Execute with their own
// dispatch as next; nothing reaches the page without passing it.
func Execute(ctx context.Context, b Backend, a wire.Action, next ExecFunc) (*wire.Result, error) {
	if a.FromAsk != "" {
		return Mask(ctx, fillFromAsk(ctx, b, a)), nil
	}
	ref, failed := check(ctx, b, a)
	if failed != nil {
		return Mask(ctx, failed), nil
	}
	if ref != nil {
		return human(ctx, b, a, *ref), nil
	}
	res, err := next(ctx, a)
	return Mask(ctx, res), err
}

// Guarded reports whether an action type passes through the value check.
func Guarded(actionType string) bool {
	switch actionType {
	case wire.ActionType, wire.ActionSelect, wire.ActionEvaluate, wire.ActionClipboardWrite,
		wire.ActionDispatchEvent, wire.ActionNavigate, wire.ActionKeyboardPress:
		return true
	}
	return false
}

// needsFieldCheck reports whether the action writes characters into a
// field: a type, or a keypress that produces one printable character.
func needsFieldCheck(a wire.Action) bool {
	switch a.Type {
	case wire.ActionType:
		return true
	case wire.ActionKeyboardPress:
		return len(a.Modifiers) == 0 && utf8.RuneCountInString(a.Key) == 1
	}
	return false
}

// hasField reports whether the action addresses a field the human path can
// focus.
func hasField(a wire.Action) bool {
	return a.Type == wire.ActionType || a.Type == wire.ActionSelect || a.Type == wire.ActionKeyboardPress
}

// actionValues is the text an action would put on a page.
func actionValues(a wire.Action) []string {
	switch a.Type {
	case wire.ActionType, wire.ActionSelect, wire.ActionClipboardWrite:
		return []string{a.Text}
	case wire.ActionEvaluate:
		return []string{a.Script}
	case wire.ActionDispatchEvent:
		return []string{a.Detail, a.Event}
	case wire.ActionNavigate:
		out := []string{a.URL}
		if u, err := url.QueryUnescape(a.URL); err == nil && u != a.URL {
			out = append(out, u)
		}
		return out
	}
	return nil
}

// ValueReason is the value-check verdict for one string: "value:<kind>" or
// "". Exposed so the hub can apply the same check to a raw /dispatch.
func ValueReason(ctx context.Context, s string) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	vm := loadVault(ctx)
	if vm.equal(s) || vm.equal(strings.TrimSpace(s)) || len(vm.spans(s)) > 0 {
		return "value:" + KindVault
	}
	if k := sensitive.CheckValue(s); k != "" {
		return "value:" + k
	}
	return ""
}

// check returns a refusal, or a failed result when the field check could not
// run (fail closed), or neither when the action may proceed.
func check(ctx context.Context, b Backend, a wire.Action) (*refusal, *wire.Result) {
	if !Guarded(a.Type) {
		return nil, nil
	}
	var reason string
	for _, v := range actionValues(a) {
		if reason = ValueReason(ctx, v); reason != "" {
			break
		}
	}
	if needsFieldCheck(a) {
		f, err := b.InspectField(ctx, a)
		if err != nil {
			if reason != "" {
				// The value alone is enough to refuse; ask without the question.
				return &refusal{Reason: reason, Site: b.Site(ctx)}, nil
			}
			return nil, &wire.Result{Error: "guard: field check failed, refusing " + a.Type + ": " + err.Error()}
		}
		if reason == "" && f.Guarded {
			reason = f.Reason
		}
		if reason != "" {
			return &refusal{Reason: reason, Question: f.Question, Site: f.Site, Matched: f.Matched}, nil
		}
		return nil, nil
	}
	if reason == "" {
		return nil, nil
	}
	ref := &refusal{Reason: reason}
	if hasField(a) {
		if f, err := b.InspectField(ctx, a); err == nil {
			ref.Question, ref.Site, ref.Matched = f.Question, f.Site, f.Matched
		}
	}
	if ref.Site == "" {
		if a.Type == wire.ActionNavigate {
			if u, err := url.Parse(a.URL); err == nil {
				ref.Site = u.Hostname()
			}
		} else {
			ref.Site = b.Site(ctx)
		}
	}
	return ref, nil
}

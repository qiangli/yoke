package live

// The live backend's wiring into the sensitive-input guard (pkg/browser/guard).
// This file is a PROTECTED path (pkg/src manifest): weakening it needs a
// `bashy approve`d src approval, like the guard package itself.
//
// Old extensions. The page-side field check lives in extension guard.js,
// advertised as the guard_field method (extension 0.9.0); guard_fill, the
// by-reference fill of an operator's answer, arrived in 0.9.1 and verifies
// the field in-page from 0.9.2 — an older extension makes a provide answer
// stop (fill-failed), never type the agent's text nor report an unproven
// fill. Rather than raise
// LiveExtensionMinVersion — which would block navigate/extract/screenshot on
// every older extension for a check only writes need — the hub gates on the
// ADVERTISED METHOD: type, select and keyboard_press are refused loudly unless
// the extension advertises guard_field. Every other method keeps working, so
// an agent mid-task can still read pages while the operator reloads.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/browser/guard"
	"github.com/qiangli/yoke/pkg/browser/wire"
)

// guardMethod is the extension method that carries the field check;
// focusMethod hands the field to the human; fillMethod types an operator's
// admitted answer (extension 0.9.1+).
//
// Both follow-ups are PINNED to the tab the field check ran in (tab_id,
// honoured from extension 0.9.3; older ones ignore it), and the hub waits
// for a reconnecting extension before sending one (GuardReconnectWait): the
// operator's dialog is the longest idle stretch a live session has, and an
// MV3 worker stopped or hot-reloaded in it comes back without its driven
// tab. 0.9.2 then filled whatever tab was active — or nothing — and the
// operator's answer ended as guard:stopped (fill-failed).
const (
	guardMethod = "guard_field"
	focusMethod = "guard_focus"
	fillMethod  = "guard_fill"
)

// guardNeeded is the set of methods that write into a field and so need
// the page-side field check present.
func guardNeeded(method string) bool {
	return method == "type" || method == "select" || method == "keyboard_press"
}

// guardMissing reports (loudly) that the connected extension cannot enforce
// the field check, for a method that needs it. "" when fine.
func guardMissing(method, ver string, methods []string) string {
	if !guardNeeded(method) || ver == "" || slices.Contains(methods, guardMethod) {
		return ""
	}
	return "live: refusing " + method + ": extension v" + ver + " has no sensitive-input field check (" +
		guardMethod + " not advertised; needs extension 0.9.0+). Reload it at chrome://extensions, " +
		"or run: bashy browser setup live"
}

// guardDispatch is the hub-side check on a raw /dispatch: the value check on
// whatever text the call carries, and the field-check presence for writes.
func (h *hub) guardDispatch(ctx context.Context, method string, params map[string]any) string {
	if msg := guardMissing(method, h.ExtVersion(), h.ExtMethods()); msg != "" {
		return msg
	}
	a := wire.Action{Type: method}
	str := func(k string) string { v, _ := params[k].(string); return v }
	if method == fillMethod {
		// Typed past the field check: only for an operator answer the
		// guard claimed for exactly this field, once.
		t := wire.Action{Selector: str("selector"), MatchText: str("match_text"), Scope: str("scope")}
		if f, ok := params["element_id"].(float64); ok {
			t.ElementID = int(f)
		}
		if v, ok := params["include_hidden"].(bool); ok {
			t.IncludeHidden = v
		}
		if err := guard.ConsumeClaimed(str("admit"), t); err != nil {
			return "live: refusing guard_fill: " + err.Error()
		}
		return ""
	}
	switch method {
	case "type", "clipboard_write":
		a.Text = str("text")
	case "select":
		a.Text = str("option")
	case "evaluate":
		a.Script = str("script")
	case "dispatch_event":
		a.Event, a.Detail = str("event"), str("detail")
	case "navigate":
		a.URL = str("url")
	default:
		return ""
	}
	for _, v := range []string{a.Text, a.Script, a.Event, a.Detail, a.URL} {
		if r := guard.ValueReason(ctx, v); r != "" {
			return "guard: refused " + method + " (" + r + ") at the live hub; a guarded value is typed by the operator, " +
				"never sent — route the action through `bashy browser` so the operator is asked"
		}
		if method == "navigate" && v != "" {
			if u, err := url.QueryUnescape(v); err == nil && u != v {
				if r := guard.ValueReason(ctx, u); r != "" {
					return "guard: refused navigate (" + r + ") at the live hub"
				}
			}
		}
	}
	return ""
}

// guardBackend adapts a Service to guard.Backend. Nothing here ever sends
// an action's text: the field check and the focus carry only the target.
type guardBackend struct{ s *Service }

func targetParams(a wire.Action) map[string]any {
	p := map[string]any{"selector": a.Selector}
	if a.Type != wire.ActionKeyboardPress {
		p["element_id"], p["match_text"], p["scope"], p["include_hidden"] = a.ElementID, a.MatchText, a.Scope, a.IncludeHidden
	}
	for k, v := range tabSelector(a) {
		p[k] = v
	}
	return p
}

// callRaw is one extension round trip in either role, returning the raw
// result object.
func (s *Service) callRaw(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	s.mu.Lock()
	role, h, client := s.role, s.hub, s.http
	s.mu.Unlock()
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	switch role {
	case roleHub:
		if h == nil {
			return nil, errors.New("live: no hub")
		}
		if ver := h.ExtVersion(); ver != "" {
			if methods := h.ExtMethods(); !slices.Contains(methods, method) {
				return nil, errors.New(methodNotAdvertisedError(method, ver))
			}
		}
		return hubCall(cctx, h, method, params)
	case roleClient:
		if client == nil {
			client = &http.Client{Timeout: 35 * time.Second}
		}
		return s.dispatchClient(cctx, client, method, params)
	}
	return nil, errors.New("live: not ready (call EnsureReady first)")
}

func (g guardBackend) InspectField(ctx context.Context, a wire.Action) (guard.Field, error) {
	raw, err := g.s.callRaw(ctx, guardMethod, targetParams(a))
	if err != nil {
		return guard.Field{}, err
	}
	var f struct {
		guard.Field
		TabID int `json:"tab_id"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return guard.Field{}, errors.New("live: unreadable guard_field result")
	}
	if f.TabID > 0 {
		g.s.mu.Lock()
		g.s.guardTab = f.TabID
		g.s.mu.Unlock()
	}
	return f.Field, nil
}

// followUpParams is targetParams pinned to the tab the field was checked in.
func (g guardBackend) followUpParams(a wire.Action) map[string]any {
	p := targetParams(a)
	g.s.mu.Lock()
	if g.s.guardTab > 0 {
		p["tab_id"] = g.s.guardTab
	}
	g.s.mu.Unlock()
	return p
}

func (g guardBackend) FocusField(ctx context.Context, a wire.Action) error {
	_, err := g.s.callRaw(ctx, focusMethod, g.followUpParams(a))
	return err
}

// ReleaseField clears the outline guard_focus put on the field (guard_focus
// with release set; page.js routes it to guard.js releaseFocus).
func (g guardBackend) ReleaseField(ctx context.Context, a wire.Action) error {
	p := g.followUpParams(a)
	p["release"] = true
	_, err := g.s.callRaw(ctx, focusMethod, p)
	return err
}

// FillAdmitted sends an operator's admitted answer to the extension's
// guard_fill: page.js types it past the field check, then compares the
// field with the answer INSIDE the page and answers only the verdict and
// the two lengths (extension 0.9.2+) — never the value, never a hash. An
// extension that answers without a verdict cannot prove the fill and is
// refused (fill-failed). The error is generic: an extension error could
// quote the field.
func (g guardBackend) FillAdmitted(ctx context.Context, a wire.Action, askID string, value []byte) (guard.Verify, error) {
	p := g.followUpParams(a)
	p["text"], p["admit"] = string(value), askID
	raw, err := g.s.callRaw(ctx, fillMethod, p)
	v, verr := fillVerdict(raw)
	if cause := fillCause(raw, err, verr); cause != "" {
		logWarn("live: guard_fill failed", "cause", cause)
		return guard.Verify{}, guard.FillFailure{Cause: cause}
	}
	return v, nil
}

// fillCausePattern is the one shape of cause the extension reports inside
// its (otherwise generic) guard_fill error: a fixed lower-case code.
var fillCausePattern = regexp.MustCompile(`\(cause: ([a-z][a-z-]{0,23})\)`)

// fillCause classifies a failed guard_fill into a VALUE-FREE code for the
// log and the outcome: "" when the fill produced a verdict. Nothing of the
// extension's message passes but a code matched by fillCausePattern.
//
//	refused        the hub refused it (no admission for this field)
//	not-connected  no extension (or it did not come back in time)
//	timeout        the call's deadline passed
//	tab-gone       the tab the field was checked in was closed
//	not-found, no-frame, inject-failed, page-error
//	               the extension threw, by its own code
//	ext-threw      the extension threw without a code (older than 0.9.3)
//	no-verdict     it answered without a verification (older than 0.9.2)
//	parse          it answered something unreadable
func fillCause(raw json.RawMessage, err, verdictErr error) string {
	if err != nil {
		msg := err.Error()
		switch {
		case strings.Contains(msg, "refusing guard_fill"):
			return "refused"
		case errors.Is(err, context.DeadlineExceeded) || strings.Contains(msg, "deadline exceeded"):
			return "timeout"
		case strings.Contains(msg, "not connected") || strings.Contains(msg, "extension disconnected") ||
			strings.Contains(msg, "no hub") || strings.Contains(msg, "dispatch to hub") || strings.Contains(msg, "live: write"):
			return "not-connected"
		}
		if m := fillCausePattern.FindStringSubmatch(msg); m != nil {
			return m[1]
		}
		return "ext-threw"
	}
	if verdictErr != nil {
		if errors.Is(verdictErr, errNoVerdict) {
			return "no-verdict"
		}
		return "parse"
	}
	return ""
}

var errNoVerdict = errors.New("live: guard_fill answered without a verification (extension older than 0.9.2; reload it)")

// fillVerdict reads guard_fill's {verified, field_len, value_len} — from
// the result itself or its data document. Any other key is ignored, so a
// read-back of the field could not leak through here even if one appeared.
func fillVerdict(raw json.RawMessage) (guard.Verify, error) {
	type verdict struct {
		Verified *string `json:"verified"`
		FieldLen int     `json:"field_len"`
		ValueLen int     `json:"value_len"`
	}
	var top struct {
		verdict
		Data string `json:"data"`
	}
	if json.Unmarshal(raw, &top) != nil {
		return guard.Verify{}, errors.New("live: unreadable guard_fill result")
	}
	v := top.verdict
	if v.Verified == nil && top.Data != "" {
		_ = json.Unmarshal([]byte(top.Data), &v)
	}
	if v.Verified == nil {
		return guard.Verify{}, errNoVerdict
	}
	out := guard.Verify{Verified: guard.VerifiedMismatch, FieldLen: v.FieldLen, ValueLen: v.ValueLen}
	if *v.Verified == guard.VerifiedMatch {
		out.Verified = guard.VerifiedMatch
	}
	return out, nil
}

func (g guardBackend) Site(ctx context.Context) string {
	raw, err := g.s.callRaw(ctx, guardMethod, map[string]any{})
	if err == nil {
		var f guard.Field
		if json.Unmarshal(raw, &f) == nil && f.Site != "" {
			return f.Site
		}
	}
	g.s.mu.Lock()
	h := g.s.hub
	g.s.mu.Unlock()
	if h != nil {
		if u, err := url.Parse(h.LastTabURL()); err == nil {
			return u.Hostname()
		}
	}
	return ""
}

// HumanChannel: live mode drives the operator's own Chrome.
func (guardBackend) HumanChannel() bool { return true }

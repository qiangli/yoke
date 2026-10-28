package live

// The live backend's wiring into the sensitive-input guard (pkg/browser/guard).
// This file is a PROTECTED path (pkg/src manifest): weakening it needs a
// `bashy approve`d src approval, like the guard package itself.
//
// Old extensions. The page-side field check lives in extension guard.js,
// advertised as the guard_field method (extension 0.9.0). Rather than raise
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
	"slices"
	"time"

	"github.com/qiangli/yoke/pkg/browser/guard"
	"github.com/qiangli/yoke/pkg/browser/wire"
)

// guardMethod is the extension method that carries the field check.
const guardMethod = "guard_field"

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
	if a.Type == wire.ActionKeyboardPress {
		return map[string]any{"selector": a.Selector}
	}
	return map[string]any{
		"selector":       a.Selector,
		"element_id":     a.ElementID,
		"match_text":     a.MatchText,
		"scope":          a.Scope,
		"include_hidden": a.IncludeHidden,
	}
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
	var f guard.Field
	if err := json.Unmarshal(raw, &f); err != nil {
		return guard.Field{}, errors.New("live: unreadable guard_field result")
	}
	return f, nil
}

func (g guardBackend) FocusField(ctx context.Context, a wire.Action) error {
	_, err := g.s.callRaw(ctx, "guard_focus", targetParams(a))
	return err
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

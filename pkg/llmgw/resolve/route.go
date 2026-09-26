package resolve

import (
	"encoding/json"
	"net/http"
	"strings"
)

// Routing posture — the local-vs-remote knob that makes the gateway
// hybrid: pooled self-hosted models for cost and privacy, remote
// commercial models when warranted.
//
// Resolution precedence (highest wins):
//
//	per-request override (X-LLM-Route header / body "route" field)
//	→ the principal's configured default
//	→ DefaultRoutePolicy (shipped default)
//
// The shipped default is local_first: prefer the pool, reach a remote
// provider only on a real gap. Everything above overrides it, so a
// privacy-strict or cost-optimizing principal just sets a default.
const (
	// RouteLocalFirst (default): prefer pooled-local candidates;
	// append remote backends only as fallback when no local backend
	// serves.
	RouteLocalFirst = "local_first"

	// RoutePrivacy: local-only. A remote backend is used only when the
	// request explicitly opts in (see RequestAllowsRemote); otherwise
	// a model no local backend serves fails rather than silently
	// leaving the pool. Hard data-residency posture.
	RoutePrivacy = "privacy"

	// RouteCost: order candidates cheapest-capable first (local is $0
	// and usually wins, but a cheap remote model may beat a slow local
	// one); escalate to the next on failure.
	RouteCost = "cost"

	// RouteSmart: capability-aware. Classify the request's capability
	// bar (reasoning / vision / long-context) and keep only candidates
	// that meet it, local-first; auto-escalate to the cheapest capable
	// remote model when no local candidate clears the bar.
	RouteSmart = "smart"

	// DefaultRoutePolicy is the shipped posture when nothing is
	// configured.
	DefaultRoutePolicy = RouteLocalFirst
)

// RouteHeader is the per-request override for the routing posture. Value
// is one of the Route* postures (local_first / privacy / cost / smart);
// an empty or unknown value is ignored so resolution falls through to the
// configured defaults.
const RouteHeader = "X-LLM-Route"

// AllowRemoteHeader is the per-request opt-in that lets a privacy-posture
// caller permit remote (commercial) routing for this one request. Without
// it (header absent / falsy and no body allow_cloud:true), a `privacy`
// policy keeps the request local-only. Ignored under non-privacy
// postures, which already permit remote backends. The header and body
// spellings are wire contract and stay as they are.
const AllowRemoteHeader = "X-LLM-Allow-Cloud"

// NormalizeRoutePolicy lowercases + trims s and returns it when it names
// a known posture, else "". An unknown or empty value returns "" so the
// precedence resolver falls through to the next level rather than locking
// in a bogus posture.
func NormalizeRoutePolicy(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case RouteLocalFirst:
		return RouteLocalFirst
	case RoutePrivacy:
		return RoutePrivacy
	case RouteCost:
		return RouteCost
	case RouteSmart:
		return RouteSmart
	}
	return ""
}

// routeBodyEnvelope is the minimal slice of the request body the route
// resolver peeks: an optional top-level "route" field (the body-borne
// alternative to the X-LLM-Route header, so OpenAI-compat clients that
// cannot set arbitrary headers can still pick a posture) and an optional
// "allow_cloud" flag (the body form of AllowRemoteHeader). Everything
// else in the body is ignored here and passed through to the backend
// untouched.
type routeBodyEnvelope struct {
	Route      string `json:"route"`
	AllowCloud bool   `json:"allow_cloud"`
}

// RequestAllowsRemote reports whether this request permits remote
// (commercial) routing — true when the allow-remote header is truthy or
// the body carries allow_cloud:true. Only consulted under the `privacy`
// posture; every other posture already allows remote backends.
func RequestAllowsRemote(r *http.Request, body []byte) bool {
	if r != nil && IsTruthy(r.Header.Get(AllowRemoteHeader)) {
		return true
	}
	var env routeBodyEnvelope
	if len(body) > 0 && json.Unmarshal(body, &env) == nil {
		return env.AllowCloud
	}
	return false
}

// PeekRouteField extracts the optional top-level "route" field from a
// request body. Tolerant of any other shape — a non-object body or a
// missing field yields "". Pure; no catalog access.
func PeekRouteField(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var env routeBodyEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return ""
	}
	return strings.TrimSpace(env.Route)
}

// RoutePolicy computes the effective routing posture for one inbound
// request. A func type rather than a method so a host can install its own
// precedence without reimplementing the request-parsing half;
// ResolveRoutePolicy is the built-in implementation.
//
// principalDefault is whatever default the host resolved for this caller
// (per-key, then per-account, in hosts that have both levels) — the
// gateway does not know how many levels a host keeps, only that one value
// arrives already chosen. An unset or unrecognised value falls through.
type RoutePolicy func(r *http.Request, body []byte, principalDefault string) string

// ResolveRoutePolicy applies the shipped precedence:
//
//  1. per-request override — X-LLM-Route header, else body "route" field
//  2. the principal's configured default
//  3. DefaultRoutePolicy (local_first)
//
// Each level is normalized; an unset or unknown value at any level falls
// through to the next. Always returns a valid posture (never "").
func ResolveRoutePolicy(r *http.Request, body []byte, principalDefault string) string {
	// 1. per-request override (header wins over body).
	if r != nil {
		if p := NormalizeRoutePolicy(r.Header.Get(RouteHeader)); p != "" {
			return p
		}
	}
	if p := NormalizeRoutePolicy(PeekRouteField(body)); p != "" {
		return p
	}

	// 2. the principal's configured default.
	if p := NormalizeRoutePolicy(principalDefault); p != "" {
		return p
	}

	// 3. shipped default.
	return DefaultRoutePolicy
}

// ResolveRoutePolicy satisfies RoutePolicy.
var _ RoutePolicy = ResolveRoutePolicy

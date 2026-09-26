package resolve

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Model resolution — exact-with-substitution.
//
// Client surface: send the concrete model name you would write in a
// config file ("qwen2.5-coder:7b") plus opt in to substitution via
// either:
//   - request header `X-LLM-Auto: true` (or "1", "yes", "on")
//   - model-name suffix "@auto" — "qwen2.5-coder:7b@auto" — which is
//     OpenAI-compat-safe (clients that strict-validate the JSON body
//     can still encode the signal).
//
// Substitution policy via `X-LLM-Auto-Mode`:
//   - "" or "prefer-exact" (default) — try the exact model first; only
//     substitute when no reachable backend has it.
//   - "best-warm" — pick a class-equivalent model that is currently
//     loaded somewhere, even if a different backend has the exact model
//     resident. Maximises pool utilisation, accepts wider output
//     variance.
//
// Beyond a concrete name, the model field also accepts:
//   - "auto" / "smart" — classify the class from the prompt itself
//     (see ClassifyFromPrompt).
//   - "tier:LX/domain" — a capability spec, addressed by class.
//   - an operator-defined alias, resolved through Catalog.Alias.

// AutoHeader is the canonical client signal.
const AutoHeader = "X-LLM-Auto"

// AutoModeHeader picks the substitution policy. Optional; absent ⇒
// prefer-exact.
const AutoModeHeader = "X-LLM-Auto-Mode"

// NoResolveHeader is the reproducibility opt-out — even if the model
// name carries an @auto suffix, this header forces exact-only behaviour
// and strips the suffix before lookup. Useful for eval suites that
// share a model string across runs and want strict semantics.
const NoResolveHeader = "X-LLM-No-Resolve"

// Response headers stamped on dispatch so the client can see what
// actually ran (debug aid; not load-bearing).
const (
	ResolvedModelHeader  = "X-LLM-Resolved-Model"
	ResolvedReasonHeader = "X-LLM-Resolved-Reason"
)

// Reason values for ResolvedReasonHeader.
const (
	ReasonExactMatch          = "exact-match"
	ReasonSubstitutedWarm     = "substituted-warm"
	ReasonSubstitutedFailover = "substituted-failover"
)

// AutoSuffix is the model-name-borne form of the auto signal.
const AutoSuffix = "@auto"

// Substitution modes, the values of AutoModeHeader.
const (
	ModePreferExact = "prefer-exact"
	ModeBestWarm    = "best-warm"
)

// Request captures everything the router needs to know about one inbound
// request to make a routing decision. Built once per chat-completions or
// embeddings call.
type Request struct {
	// Model is the bare model name after the @auto suffix (if any)
	// has been stripped.
	Model string

	// AutoEnabled is true when the client opted in to substitution
	// via header or @auto suffix.
	AutoEnabled bool

	// Mode is the substitution policy when AutoEnabled is true.
	// Empty otherwise.
	Mode string
}

// ParseAutoSignal reads the X-LLM-Auto header, the @auto suffix on the
// model name, and the X-LLM-No-Resolve override to compute the effective
// (Model, AutoEnabled, Mode) for this request. Pure — no catalog access.
func ParseAutoSignal(r *http.Request, model string) Request {
	out := Request{Model: model}

	// Strip @auto suffix regardless of header — clients should not see
	// "@auto" land in the upstream JSON body, and the suffix is the
	// alternative transport for the same signal.
	hasSuffix := false
	if i := strings.Index(model, AutoSuffix); i >= 0 && i+len(AutoSuffix) == len(model) {
		out.Model = strings.TrimSpace(model[:i])
		hasSuffix = true
	}

	// X-LLM-No-Resolve wins. Stripped suffix but auto stays off — the
	// client gets pure exact-model behaviour, no substitution.
	if IsTruthy(r.Header.Get(NoResolveHeader)) {
		return out
	}

	headerOn := IsTruthy(r.Header.Get(AutoHeader))
	out.AutoEnabled = headerOn || hasSuffix
	if out.AutoEnabled {
		out.Mode = strings.ToLower(strings.TrimSpace(r.Header.Get(AutoModeHeader)))
		if out.Mode == "" {
			out.Mode = ModePreferExact
		}
	}
	return out
}

// IsTruthy reports whether a header value is an affirmative flag. The
// accepted set is deliberately small and case-insensitive; anything else
// (including "maybe" and "false") reads as off.
func IsTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// ParseCapabilitySpec recognises the "tier:LX/domain" capability spec
// form ("tier:L2/coding"). Returns (class, domains, true) on a successful
// parse; (_, _, false) when the string is not a spec. Gives ops-savvy
// callers a way to address by capability without an alias entry.
func ParseCapabilitySpec(s string) (int, []string, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	if !strings.HasPrefix(s, "tier:") {
		return 0, nil, false
	}
	rest := strings.TrimPrefix(s, "tier:")
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 {
		return 0, nil, false
	}
	tier := 0
	switch parts[0] {
	case "l1":
		tier = TierL1
	case "l2":
		tier = TierL2
	case "l3":
		tier = TierL3
	case "l4":
		tier = TierL4
	default:
		return 0, nil, false
	}
	if parts[1] == "*" || parts[1] == "" {
		// "tier:L2/*" — any domain. Pass all known domains as the
		// candidate filter; they are OR-ed.
		return tier, AllDomains, true
	}
	// Comma-separated list of domains permitted: "tier:L2/coding"
	// or "tier:L2/coding,reasoning".
	return tier, SplitCSVField(parts[1]), true
}

// CandidateModels returns the priority-ordered list of model names the
// router may dispatch to for this request. The exact model is always
// first — and alone — when AutoEnabled is false. When AutoEnabled is
// true, the ordering depends on req.Mode and on which substitute models
// are currently loaded somewhere reachable.
//
// principal identifies the caller to the catalog (the reachability
// scope). loaded may be nil, which keeps prefer-exact ordering. body is
// the raw JSON request body, used only by the "auto" model path to
// classify from the prompt; hist is the classifier chain's history stage
// and may likewise be nil.
//
// A nil return means "no reachable candidate" and is distinct from a
// one-element return of the exact model: the class-addressed forms
// (auto / capability spec / alias) resolve to nothing rather than
// falling back to a model name the client never asked for.
func CandidateModels(ctx context.Context, cat Catalog, req Request, principal string, loaded LoadedFunc, hist PromptHistory, body []byte) []string {
	if !req.AutoEnabled || req.Model == "" {
		return []string{req.Model}
	}

	var rows []ModelRow
	if cat != nil {
		rows = cat.Rows(ctx, principal)
	}

	// "auto" model name → classify from the prompt.
	if IsAutoModelName(req.Model) {
		class, doms, _ := ClassifyFromPrompt(body, hist)
		inClass := filterInClass(rows, class, doms)
		if len(inClass) == 0 {
			return nil
		}
		return uniqueModelNames(inClass)
	}

	// "tier:LX/domain" capability spec.
	if class, domains, ok := ParseCapabilitySpec(req.Model); ok {
		inClass := filterInClass(rows, class, domains)
		if len(inClass) == 0 {
			return nil
		}
		return uniqueModelNames(inClass)
	}

	// Operator-defined alias.
	if class, domains, ok := LookupAliasClass(ctx, cat, req.Model); ok && len(domains) > 0 {
		inClass := filterInClass(rows, class, domains)
		if len(inClass) == 0 {
			return nil
		}
		return uniqueModelNames(inClass)
	}

	// A concrete model name: learn its class from the catalog, and
	// failing that from the name itself, then widen to the class.
	class, domains := lookupClass(rows, req.Model)
	if class == 0 && len(domains) == 0 {
		c, d := ClassifyModel(req.Model, "", "", nil)
		if class == 0 {
			class = c
		}
		if len(domains) == 0 {
			domains = d
		}
	}
	if len(domains) == 0 {
		return []string{req.Model}
	}

	inClass := filterInClass(rows, class, domains)
	if len(inClass) == 0 {
		return []string{req.Model}
	}

	// Collect unique model names, exact first.
	seen := map[string]struct{}{req.Model: {}}
	candidates := []string{req.Model}
	for _, r := range inClass {
		if r.Name == req.Model {
			continue
		}
		if _, dup := seen[r.Name]; dup {
			continue
		}
		seen[r.Name] = struct{}{}
		candidates = append(candidates, r.Name)
	}

	if req.Mode == ModeBestWarm && loaded != nil {
		// Re-order so models with at least one loaded backend come
		// first. Stable within each bucket so exact still wins ties.
		var warm []string
		var cold []string
		for _, name := range candidates {
			anyLoaded := false
			// A model is "warm somewhere" when ANY backend that
			// carries it in the candidate class has it resident.
			for _, backend := range BackendsExposing(inClass, name) {
				if loaded(backend, name) {
					anyLoaded = true
					break
				}
			}
			if anyLoaded {
				warm = append(warm, name)
			} else {
				cold = append(cold, name)
			}
		}
		return append(warm, cold...)
	}

	return candidates
}

// PatchModelField rewrites the top-level "model" field of a JSON request
// body. Used when the resolver picked a substitute or when the original
// model string carried the "@auto" suffix that needs stripping before the
// body reaches the backend.
//
// Implementation note: we unmarshal into map[string]json.RawMessage so
// the nested message / tool-call / vision-part subtrees stay as raw bytes
// — only the model field is touched. Re-marshaling does drop the original
// key ordering (a side effect of Go's map iteration) but JSON treats
// objects as unordered and every OpenAI-compat backend accepts any key
// order, so this is behaviourally invisible upstream.
func PatchModelField(body []byte, newModel string) ([]byte, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("body not JSON object: %w", err)
	}
	enc, err := json.Marshal(newModel)
	if err != nil {
		return nil, err
	}
	raw["model"] = enc
	return json.Marshal(raw)
}

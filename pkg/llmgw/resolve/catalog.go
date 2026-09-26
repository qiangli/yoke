package resolve

import (
	"context"
	"strings"
	"time"
)

// Catalog is the resolver's view of the model inventory: which models a
// principal may dispatch to, on which backends, and what class each one
// belongs to. It is the ONE seam between this package and a host's
// registry — the resolver never learns about accounts, sharing, freshness
// or storage.
//
// Two methods, because the resolver asks exactly two questions:
//
//	Rows  — "what can this principal reach right now?"
//	Alias — "is this friendly name an operator-defined class?"
//
// Implementations are expected to be cheap: CandidateModels calls Rows at
// most once per request, but the gateway calls CandidateModels once per
// inbound request, so a per-request cache or a refreshed snapshot is the
// intended shape.
type Catalog interface {
	// Rows returns every model row the principal can reach, in
	// PRIORITY ORDER — the principal's own backends first, then rows
	// reachable through sharing. The resolver preserves that order
	// when it builds the candidate list, and the first row matching a
	// model name is the one whose class is believed.
	//
	// Staleness is the catalog's business: a row that is returned is
	// one the resolver may route to. An empty or nil slice is a valid
	// answer and means "nothing reachable", never an error.
	Rows(ctx context.Context, principal string) []ModelRow

	// Alias resolves an operator-defined model alias ("best-coder") to
	// the (class, domains) it stands for. Aliases are global, not
	// per-principal. Returns ok=false when the name is not an alias —
	// which is the common case, since most requests name a model.
	Alias(ctx context.Context, name string) (class int, domains []string, ok bool)
}

// ModelRow is one (backend, model) pair the principal can reach. It is a
// flattened, storage-free view: whatever a host's inventory looks like, it
// projects onto this.
type ModelRow struct {
	// Name is the model name as a client would write it in a request
	// body ("qwen2.5-coder:7b"). Matching against the requested model
	// is exact and case-sensitive.
	Name string

	// Backend identifies the machine or endpoint exposing this model.
	// Opaque to the resolver: it only ever compares backends for
	// equality, de-duplicates them, and hands them to LoadedFunc.
	Backend string

	// Capabilities is what the backend reports about the model
	// ("completion", "tools", "vision", "embedding"). Consumed by
	// ClassifyModel and by model-listing surfaces; the resolver itself
	// routes on Class/Domains, which the catalog has already derived.
	Capabilities []string

	// ContextLen is the maximum sequence length the model accepts, or
	// 0 when the backend did not report one. Client metadata — the
	// resolver does not route on it.
	ContextLen int64

	// Class is the size tier, TierL1..TierL4 (see ClassifyModel), or
	// TierUnknown for a row the catalog has not classified. Candidate
	// selection filters on an exact Class match when a class is asked
	// for; a zero class in the REQUEST disables the filter, a zero
	// class in the ROW simply never matches a class-filtered query.
	Class int

	// Domains is the multi-label domain set (embedding, vision,
	// coding, reasoning, general). Candidate selection keeps a row
	// when any requested domain appears in this set, matched as a
	// substring of the comma-joined labels so that a catalog storing
	// a label list and one storing a delimited string agree.
	Domains []string

	// Loaded reports whether the backend had this model resident when
	// the catalog snapshot was taken. Advisory metadata for listing
	// surfaces: the best-warm re-ordering deliberately asks a live
	// LoadedFunc instead, because residency changes far faster than a
	// catalog refresh.
	Loaded bool

	// UpdatedAt is when the row was last refreshed from its backend.
	// Freshness policy belongs to the catalog — the resolver treats
	// every returned row as routable — so this is metadata for
	// listing surfaces and diagnostics.
	UpdatedAt time.Time
}

// LoadedFunc reports whether backend currently has model resident (warm).
// The best-warm substitution mode consults it to prefer models that need
// no load. A nil LoadedFunc disables the re-ordering rather than treating
// everything as cold, so a host with no residency signal keeps
// prefer-exact ordering.
type LoadedFunc func(backend, model string) bool

// LookupAliasClass returns the (class, domains) bound to an
// operator-defined alias, or (0, nil, false) when the name is not one.
// Cheap single lookup; the alias set is small and read-heavy.
func LookupAliasClass(ctx context.Context, cat Catalog, name string) (int, []string, bool) {
	if cat == nil {
		return 0, nil, false
	}
	class, domains, ok := cat.Alias(ctx, name)
	if !ok {
		return 0, nil, false
	}
	return class, domains, true
}

// filterInClass keeps the rows whose class matches AND whose domain set
// overlaps any entry in domains, preserving the catalog's ordering.
//
// class == 0 disables the class filter. An empty domains list matches
// NOTHING — an unconstrained class query would return the whole
// inventory, which is never what a resolution wants.
func filterInClass(rows []ModelRow, class int, domains []string) []ModelRow {
	if len(domains) == 0 {
		return nil
	}
	out := make([]ModelRow, 0, len(rows))
	for _, r := range rows {
		if class > 0 && r.Class != class {
			continue
		}
		if !domainsOverlap(r.Domains, domains) {
			continue
		}
		out = append(out, r)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// domainsOverlap reports whether any wanted domain appears in the row's
// domain set. Matched as a substring of the comma-joined labels, so a
// catalog that stores "coding,general" as one field and one that stores
// two labels resolve identically.
func domainsOverlap(rowDomains, wanted []string) bool {
	joined := strings.Join(rowDomains, ",")
	for _, d := range wanted {
		if strings.Contains(joined, d) {
			return true
		}
	}
	return false
}

// lookupClass returns the (class, domains) recorded for one model name,
// taking the first matching row — which is the principal's own when the
// catalog orders rows as documented. Returns (0, nil) when no row carries
// the name, and the caller then classifies from the name alone.
func lookupClass(rows []ModelRow, name string) (int, []string) {
	for _, r := range rows {
		if r.Name == name {
			return r.Class, r.Domains
		}
	}
	return 0, nil
}

// BackendsExposing returns the unique backends that carry name in the
// given rows, in row order. Used by best-warm to ask which of those
// backends (if any) currently has the model resident.
func BackendsExposing(rows []ModelRow, name string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		if r.Name != name {
			continue
		}
		if _, dup := seen[r.Backend]; dup {
			continue
		}
		seen[r.Backend] = struct{}{}
		out = append(out, r.Backend)
	}
	return out
}

// uniqueModelNames flattens a row set into a stable de-duplicated slice of
// model names.
func uniqueModelNames(rows []ModelRow) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		if _, dup := seen[r.Name]; dup {
			continue
		}
		seen[r.Name] = struct{}{}
		out = append(out, r.Name)
	}
	return out
}

// SplitCSVField splits a comma-separated field into trimmed, non-empty
// parts. Exported because catalogs that store domain or capability lists
// as one delimited column need exactly this to fill a ModelRow.
func SplitCSVField(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

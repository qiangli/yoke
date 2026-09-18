// Package principal resolves a name to the thing it names, and says how to
// reach it.
//
// When an agent writes "Qiang requested X" or "agent 007 commented Y" or
// "deploy it on host-a", nothing today binds those strings to an entity.
// Prose names are unverifiable and unreachable. This package makes them
// both.
//
// # Two families
//
//	assets      declarative, ownable, syncable    tools · models · agents · skills
//	principals  can act or be addressed           people · agents · hosts
//
// An agent belongs to both: the asset row is its definition (a tool:model
// binding), the principal is a view of it that adds runtime facts.
//
// # Contact methods
//
// Reachability is not a property of hosts alone. A person is reachable by
// email, a tool by exec, a model through a gateway, an agent by launching
// its CLI, a host over ssh or a relay. Each principal therefore carries a
// ranked list of Contacts, and which of them is live depends on the host's
// space-time coordinate: ssh only on the same network, a relay only when
// paired, `cli:codex` only when codex is on PATH.
//
// A host is on the LAN one minute and remote the next. So the resolver
// always returns the whole ranked ladder and never memoizes a choice, the
// network probes it reads are volatile (see pkg/spacetime), and a directly
// observed fact always outranks an inferred one.
//
// # Standalone-first
//
// Resolving a name never requires the network. Every kind resolves from the
// local fleet catalog, the host's own configuration, and direct observation.
// Pairing adds contacts; it is never a gate.
package principal

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Kind is the noun a name resolves to.
type Kind string

const (
	KindPerson Kind = "person"
	KindAgent  Kind = "agent"
	KindTool   Kind = "tool"
	KindModel  Kind = "model"
	KindHost   Kind = "host"
	// KindRole is an addressable seat (steward, conductor:22) rather than
	// whoever holds it. The addresser resolves these — mail to a role survives
	// a handover — so the resolver must too: whois cannot know fewer names
	// than the thing that sends (see role.go).
	KindRole Kind = "role"
)

// kinds is the set accepted as a `kind:name` prefix.
var kinds = map[Kind]bool{
	KindPerson: true, KindAgent: true, KindTool: true,
	KindModel: true, KindHost: true, KindRole: true,
}

// LocalOwner is the implicit owner of every entry on an unpaired host. On
// pairing it becomes the account email; because the uniqueness key is
// (owner, kind, name) on both sides, a local name slots in additively.
const LocalOwner = "local"

// Ref names a principal. It is what structured attribution records instead
// of a bare string: kb page provenance, meet transcripts, weave runs.
type Ref struct {
	URN     string `json:"urn,omitempty" yaml:"urn,omitempty"`
	Kind    Kind   `json:"kind,omitempty" yaml:"kind,omitempty"`
	Name    string `json:"name,omitempty" yaml:"name,omitempty"`
	Episode string `json:"episode,omitempty" yaml:"episode,omitempty"`
	Host    string `json:"host,omitempty" yaml:"host,omitempty"`
}

// URN builds the canonical identifier: dhnt:<kind>/<name>[@<owner>].
// The owner suffix is omitted for the implicit local owner, so prose stays
// readable while the stored form stays unambiguous.
func URN(kind Kind, name, owner string) string {
	s := "dhnt:" + string(kind) + "/" + name
	if owner != "" && owner != LocalOwner {
		s += "@" + owner
	}
	return s
}

// ParseURN splits a canonical identifier back into its parts.
func ParseURN(s string) (kind Kind, name, owner string, err error) {
	rest, ok := strings.CutPrefix(s, "dhnt:")
	if !ok {
		return "", "", "", fmt.Errorf("principal: %q is not a dhnt URN", s)
	}
	k, rest, ok := strings.Cut(rest, "/")
	if !ok || !kinds[Kind(k)] {
		return "", "", "", fmt.Errorf("principal: %q has no known kind", s)
	}
	name, owner, hasOwner := strings.Cut(rest, "@")
	if name == "" {
		return "", "", "", fmt.Errorf("principal: %q has no name", s)
	}
	if !hasOwner {
		owner = LocalOwner
	}
	return Kind(k), name, owner, nil
}

// SplitQuery splits a short-form query into an optional kind and a name.
//
// The left token is a kind keyword only when it is one of the five kinds.
// This matters: `agent:007` is a typed principal, but `codex:deepseek-v4`
// is a tool:model binding, and both use a colon. Anything handed to a
// tool:model helper must go through here first, or `capability.ToolOf`
// would happily report the tool of "agent:007" as "agent".
func SplitQuery(q string) (Kind, string) {
	if k, rest, ok := strings.Cut(q, ":"); ok && kinds[Kind(k)] && rest != "" {
		return Kind(k), rest
	}
	return "", q
}

// Confidence grades how firmly a fact is known. A guess must be visible as
// a guess: an ssh contact built from the local $USER is not the same claim
// as one read from the host's own ssh_config.
type Confidence string

const (
	// Observed — measured directly, here and now (an mDNS answer, a binary
	// on PATH).
	Observed Confidence = "observed"
	// Declared — read from configuration a human wrote.
	Declared Confidence = "declared"
	// Inferred — derived from a signal that can be wrong (an external-IP
	// comparison that shares a CGNAT egress).
	Inferred Confidence = "inferred"
	// Assumed — a fallback with no evidence behind it.
	Assumed Confidence = "assumed"
)

// rank orders confidences best-first.
func (c Confidence) rank() int {
	switch c {
	case Observed:
		return 0
	case Declared:
		return 1
	case Inferred:
		return 2
	default:
		return 3
	}
}

// Contact is one way to reach a principal, with the evidence behind it.
type Contact struct {
	Method     string     `json:"method"`  // ssh | mdns | cli | chat | email | relay | gateway | ...
	Address    string     `json:"address"` // "ssh://al@host-a" · "claude --model opus"
	Source     string     `json:"source,omitempty"`
	Confidence Confidence `json:"confidence,omitempty"`
	// Live reports whether this method works at the current coordinate.
	Live bool `json:"live"`
	// Why explains a method that is not live, or qualifies one that is.
	Why string `json:"why,omitempty"`
	// Cost is an optional ordering hint among live methods; lower is better.
	Cost int `json:"cost,omitempty"`
}

// Where a resolution's identity claim comes from. A catalog entry is a
// DECLARED identity — a human (or a launcher) wrote it down. An observed
// entry is INFERRED from the traces a name left in the coordination stores
// (board cursors, meet rosters, bus subscriptions); it proves something is
// acting under the name, not who declared it.
const (
	SourceFleet    = "fleet"
	SourceObserved = "observed"
	// SourceHost marks an identity the running host itself vouches for: a
	// role seat its owner package registered, or the OS login this session
	// runs as. Neither is a catalog entry, but neither is a mere trace read
	// out of a store — the authority that created the name declared it.
	SourceHost = "host"
)

// Resolution is what a name resolves to.
type Resolution struct {
	URN string `json:"urn"`
	// Canonical is the uniform `<kind>:<id>` ref (pkg/ref) for this
	// principal — the spelling prose, the command line, and JSON share.
	// The JSON key is `ref`; the Go name differs only because Ref()
	// already returns the structured principal.Ref.
	Canonical string   `json:"ref,omitempty"`
	Kind      Kind     `json:"kind"`
	Name      string   `json:"name"`
	Owner     string   `json:"owner,omitempty"`
	Aliases   []string `json:"aliases,omitempty"`
	Display   string   `json:"display,omitempty"`
	Summary   string   `json:"summary,omitempty"`
	// Source and Confidence grade the identity claim itself, so a caller can
	// tell a declared catalog entry from one inferred out of observation.
	// An observed match is always Inferred — below any catalog entry.
	Source     string      `json:"source,omitempty"`
	Confidence Confidence  `json:"confidence,omitempty"`
	Facts      [][2]string `json:"facts,omitempty"` // ordered key/value detail
	Contacts   []Contact   `json:"contacts,omitempty"`
	Claim      *LiveClaim  `json:"claim,omitempty"`
}

// LiveClaim is the process-backed singleton currently occupying an agent name.
// It intentionally exposes no session digest or vendor identifier.
type LiveClaim struct {
	State string `json:"state"`
	ID    string `json:"id"`
	Mode  string `json:"mode,omitempty"`
	PID   int    `json:"pid"`
}

// Ref returns a structured reference to this principal.
func (r Resolution) Ref() Ref {
	return Ref{URN: r.URN, Kind: r.Kind, Name: r.Name}
}

// Best returns the highest-ranked live contact.
func (r Resolution) Best() (Contact, bool) {
	for _, c := range r.Contacts {
		if c.Live {
			return c, true
		}
	}
	return Contact{}, false
}

// rankContacts orders contacts live-first, then by confidence, then by cost.
//
// It ranks; it never collapses. Two methods may be live at once — a host on
// the LAN is also reachable through the relay — and a caller that tried only
// the cached "best" would break the moment the machine roamed.
func rankContacts(cs []Contact) {
	sort.SliceStable(cs, func(i, j int) bool {
		a, b := cs[i], cs[j]
		if a.Live != b.Live {
			return a.Live
		}
		if ar, br := a.Confidence.rank(), b.Confidence.rank(); ar != br {
			return ar < br
		}
		return a.Cost < b.Cost
	})
}

// Answer is a whois result: one or more matches for a query.
type Answer struct {
	SchemaVersion string       `json:"schema_version"`
	Query         string       `json:"query"`
	Resolved      bool         `json:"resolved"`
	Matches       []Resolution `json:"matches,omitempty"`
}

// SchemaVersion is the whois envelope version.
const SchemaVersion = "bashy-whois-v1"

// Ambiguous reports whether a query matched more than one kind, which the
// caller must disambiguate rather than guess at.
func (a Answer) Ambiguous() bool { return len(a.Matches) > 1 }

// Kinds lists the kinds a query matched, in resolution order.
func (a Answer) Kinds() []string {
	out := make([]string, 0, len(a.Matches))
	for _, m := range a.Matches {
		out = append(out, string(m.Kind))
	}
	return out
}

// ambiguityHint explains a name that means more than one thing and shows how
// to disambiguate it.
func ambiguityHint(name string, kinds []string) string {
	return "names " + strconv.Itoa(len(kinds)) + " kinds (" + strings.Join(kinds, ", ") +
		") — qualify it, e.g. " + kinds[0] + ":" + name
}

// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package coord

import (
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Ref names one claimable thing: a kind and a name inside it ("repo:yoke",
// "name:do1", "sprint:408").
type Ref struct{ Kind, Name string }

// KindName is the kind a bare name gets: the original host-local named lease.
const KindName = "name"

// ParseRef reads "kind:name". A bare name — or a prefix that cannot be a kind,
// like a Windows drive letter — is a KindName ref.
func ParseRef(s string) Ref {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, ':'); i > 1 && validKindToken(s[:i]) {
		return Ref{Kind: s[:i], Name: s[i+1:]}
	}
	return Ref{Kind: KindName, Name: s}
}

func (r Ref) String() string { return r.Kind + ":" + r.Name }

func validKindToken(s string) bool {
	for _, c := range s {
		if !unicode.IsLetter(c) && !unicode.IsDigit(c) && c != '-' && c != '_' {
			return false
		}
	}
	return s != ""
}

// MatchRule decides when two claims of one domain collide.
type MatchRule string

const (
	// MatchName: the claims name the same thing.
	MatchName MatchRule = "name"
	// MatchMember: the claims share at least one member string.
	MatchMember MatchRule = "member"
	// MatchPath: members are paths; the claims collide when one equals or
	// contains another.
	MatchPath MatchRule = "path"
)

// Kind is one class of claimable resource.
type Kind struct {
	Name string
	// Domain groups kinds that are compared with each other. Different domains
	// never conflict. Empty means the kind's own name.
	Domain string
	Match  MatchRule
	// TTL overrides the package TTL for lease-mode claims of this kind.
	TTL time.Duration
	// Modes the kind permits; empty permits all three.
	Modes []string
	// Resolve and Probe are the commands a CLI uses to turn a name into
	// members and to test that a name exists. The engine does not run them.
	Resolve, Probe string
}

func (k Kind) domain() string {
	if k.Domain != "" {
		return k.Domain
	}
	return k.Name
}

func (k Kind) allows(mode string) bool {
	if len(k.Modes) == 0 {
		return true
	}
	for _, m := range k.Modes {
		if m == mode {
			return true
		}
	}
	return false
}

// KindProvider supplies a kind together with how to find its members.
type KindProvider interface {
	Kind() Kind
	Exists(name string) bool
	Members(name string) ([]string, error)
}

// EffectiveKinder is an optional KindProvider extension for names whose
// claim semantics come from somewhere else: a registry entry declaring
// which kind it is held under. When the provider names an effective kind
// for name, the engine matches, modes and TTLs under that kind while the
// claim itself stays stored under the requested ref. Providers that do not
// implement it claim under their own kind, exactly as before.
type EffectiveKinder interface {
	EffectiveKind(name string) (Kind, bool)
}

// BareMapper is an optional KindProvider extension for records addressable
// without a kind prefix. When the provider claims a bare name, the engine
// resolves the ref to the full one before anything else — key, backend,
// members and conflicts all see the mapped ref. Providers that do not
// implement it never see bare names.
type BareMapper interface {
	MapBare(name string) (Ref, bool)
}

var (
	regMu     sync.RWMutex
	kindReg   = map[string]Kind{}
	providers = map[string]KindProvider{}
	backends  = map[string]Backend{}
)

var allModes = []string{ModeLease, ModeAttached, ModeAnnounce}

func init() {
	RegisterKind(Kind{Name: KindName, Match: MatchName, Modes: allModes})
	RegisterKind(Kind{Name: "path", Domain: "fs", Match: MatchPath, Modes: allModes})
	RegisterKind(Kind{Name: kindRepo, Domain: "fs", Match: MatchPath, Modes: allModes})
}

// RegisterKind adds or replaces a kind.
func RegisterKind(k Kind) {
	if k.Name == "" {
		return
	}
	if k.Match == "" {
		k.Match = MatchName
	}
	regMu.Lock()
	kindReg[k.Name] = k
	regMu.Unlock()
}

// LookupKind returns a registered kind.
func LookupKind(name string) (Kind, bool) {
	regMu.RLock()
	defer regMu.RUnlock()
	k, ok := kindReg[name]
	return k, ok
}

// Kinds lists the registered kinds by name.
func Kinds() []Kind {
	regMu.RLock()
	out := make([]Kind, 0, len(kindReg))
	for _, k := range kindReg {
		out = append(out, k)
	}
	regMu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// RegisterProvider registers p's kind and uses p to resolve the members of a
// request that carries none.
func RegisterProvider(p KindProvider) {
	k := p.Kind()
	RegisterKind(k)
	regMu.Lock()
	providers[k.Name] = p
	regMu.Unlock()
}

func providerFor(kind string) (KindProvider, bool) {
	regMu.RLock()
	defer regMu.RUnlock()
	p, ok := providers[kind]
	return p, ok
}

// providersSorted lists the registered providers in kind-name order, so a
// scan across providers is deterministic no matter the registration order.
func providersSorted() []KindProvider {
	regMu.RLock()
	names := make([]string, 0, len(providers))
	for n := range providers {
		names = append(names, n)
	}
	regMu.RUnlock()
	sort.Strings(names)
	regMu.RLock()
	defer regMu.RUnlock()
	out := make([]KindProvider, 0, len(names))
	for _, n := range names {
		if p, ok := providers[n]; ok {
			out = append(out, p)
		}
	}
	return out
}

// effectiveKind is the kind a ref claims as: the provider's effective kind
// when it names one for this name, else the registered kind, else the plain
// name-matched default for an unknown kind. The claim stays stored under
// the requested ref either way — only matching, modes and TTL follow the
// effective kind.
func effectiveKind(ref Ref) Kind {
	if p, ok := providerFor(ref.Kind); ok {
		if ek, ok := p.(EffectiveKinder); ok {
			if k, ok := ek.EffectiveKind(ref.Name); ok {
				if k.Name == "" {
					k.Name = ref.Kind
				}
				if k.Match == "" {
					k.Match = MatchName
				}
				return k
			}
		}
	}
	return kindOrDefault(ref.Kind)
}

// mapBare resolves a bare name through the providers that claim bare names.
// Anything else passes through unchanged, so names no provider claims keep
// their long-standing meaning.
func mapBare(ref Ref) Ref {
	if ref.Kind != KindName || ref.Name == "" {
		return ref
	}
	for _, p := range providersSorted() {
		m, ok := p.(BareMapper)
		if !ok {
			continue
		}
		if full, ok := m.MapBare(ref.Name); ok {
			full.Kind, full.Name = strings.TrimSpace(full.Kind), strings.TrimSpace(full.Name)
			if full.Kind == "" {
				full.Kind = KindName
			}
			if full.Name != "" {
				return full
			}
		}
	}
	return ref
}

// kindOrDefault treats an unregistered kind as a plain name-matched kind in
// its own domain, so a typo is still a claim rather than a crash.
func kindOrDefault(name string) Kind {
	if k, ok := LookupKind(name); ok {
		return k
	}
	return Kind{Name: name, Match: MatchName}
}

// Backend stores the claim for one key. The engine reads, decides and writes
// without holding a lock a custom backend could honour, so the contract is what
// makes that sequence safe:
//
//   - CommitIfRev is a compare-and-swap on the stored record's Rev (0 when no
//     record exists). It must fail with ErrEpochMismatch when the stored Rev is
//     not prevRev — a refresh that leaves the epoch alone still changes the Rev,
//     so a decision made on a stale read cannot commit. next == nil deletes.
//   - A successful commit assigns next.Rev in place, to a value greater than any
//     Rev ever stored under the key, deletions included: a record that was
//     released and recreated must never compare equal to a stale read.
//   - The backend keeps the key's epoch high-water mark across deletion, and a
//     commit that creates a record where none exists lifts next.Epoch above it
//     (in place), so a released key never re-issues an epoch.
//
// A backend that can enumerate its claims may also implement
// Claims() ([]*Claim, error); List includes those claims.
// A backend that requires a presented epoch may implement
// MissingEpoch(current uint64) error. Coord calls it when a holder tries to
// reuse, refresh, or release a held claim with epoch zero.
type Backend interface {
	Load(key string) (*Claim, error)
	CommitIfRev(key string, prevRev uint64, next *Claim) error
}

// RegisterBackend routes every claim of kind to b instead of the file store. A
// custom backend sees only its own keys, so cross-key matching (repo × path)
// is a file-store feature; same-key exclusion and fencing work everywhere.
// A nil b restores the file store.
func RegisterBackend(kind string, b Backend) {
	regMu.Lock()
	defer regMu.Unlock()
	if b == nil {
		delete(backends, kind)
		return
	}
	backends[kind] = b
}

func customBackend(kind string) Backend {
	regMu.RLock()
	defer regMu.RUnlock()
	return backends[kind]
}

// kindsConflict reports whether a claim on (ka, na, ma) and one on (kb, nb,
// mb) collide: the kinds must share a domain, and a mixed-rule domain falls
// back to member equality. Same-key exclusion is the caller's job — names are
// not unique across paths (two repos can both be called "yoke").
func kindsConflict(ka Kind, na string, ma []string, kb Kind, nb string, mb []string) bool {
	if ka.domain() != kb.domain() {
		return false
	}
	rule := ka.Match
	if kb.Match != rule {
		rule = MatchMember
	}
	switch rule {
	case MatchName:
		return na == nb
	case MatchPath:
		return Intersects(ma, mb)
	default:
		for _, x := range ma {
			for _, y := range mb {
				if x != "" && x == y {
					return true
				}
			}
		}
		return false
	}
}

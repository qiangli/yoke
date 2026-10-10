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

// kindOrDefault treats an unregistered kind as a plain name-matched kind in
// its own domain, so a typo is still a claim rather than a crash.
func kindOrDefault(name string) Kind {
	if k, ok := LookupKind(name); ok {
		return k
	}
	return Kind{Name: name, Match: MatchName}
}

// Backend stores the claim for one key. CommitIfEpoch is a compare-and-swap on
// the stored claim's epoch (0 when absent): it must fail with ErrEpochMismatch
// when the stored epoch is not prevEpoch, and next == nil deletes.
type Backend interface {
	Load(key string) (*Claim, error)
	CommitIfEpoch(key string, prevEpoch uint64, next *Claim) error
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

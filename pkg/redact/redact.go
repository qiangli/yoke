// Package redact removes HOST IDENTITY from text before it is stored, shared,
// or exported.
//
// # Not a secrets redactor
//
// Three of those already exist and this is not a fourth:
//
//	policy/audit.Redact   credential-looking values in an argv
//	dag.redactSecrets     the VALUES of declared secrets, matched literally
//	meet.redactHome       the home directory, for display
//
// Those hide things that are confidential. This hides things that are
// IDENTIFYING: hostnames, usernames, home paths, IP and MAC addresses, email
// addresses. None of them is a secret — a hostname is not a password — but all
// of them say WHOSE machine this is, and that is exactly what must not travel
// when a host's learned knowledge is shared.
//
// The distinction is load-bearing rather than pedantic. A secret leaking is a
// security incident. An identity leaking is a privacy failure that also
// pollutes a shared catalog with facts true on exactly one machine.
//
// # Preserve co-reference, destroy identity
//
// Placeholders are content-derived and stable: the same value always becomes
// the same tag, and two different values never collide. So
//
//	"ssh build@workshop then rsync from workshop:/tmp"
//
// becomes
//
//	"ssh ‹user:6f1a› @‹host:9c2d› then rsync from ‹host:9c2d›:/tmp"
//
// A reader can still see that both mentions are the SAME host — which is what
// makes the sentence mean anything — while learning nothing about which host.
// A flat placeholder would destroy that structure and turn a usable procedure
// into mush; a reversible one would not be redaction at all. The tag is a
// truncated SHA-256, so it is stable across processes and machines and cannot
// be inverted.
//
// # Where this sits in the model
//
// Knowledge splits two ways. What GENERALISES (an OS-specific workaround) keys
// on a space-time coordinate, contains no identity by construction, and is
// freely shareable. What is PARTICULAR (this host's login, that box's address)
// binds to an entity, stays in the host-local ring, and is scrubbed on every
// path that leaves the machine. This package is the enforcement point for the
// second half.
package redact

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Kind names a class of identifying value.
type Kind string

const (
	KindHost  Kind = "host"
	KindUser  Kind = "user"
	KindPath  Kind = "path"
	KindIP    Kind = "ip"
	KindMAC   Kind = "mac"
	KindEmail Kind = "email"
)

// Finding is one replacement the scrubber made (or would make).
type Finding struct {
	Kind        Kind   `json:"kind"`
	Placeholder string `json:"placeholder"`
	// Value is the ORIGINAL text. It is populated for local diagnostics only
	// and must never itself be stored or shipped — a findings list with values
	// is precisely the identity leak this package exists to prevent.
	Value string `json:"-"`
}

// Scrubber replaces identifying values with stable, non-reversible tags.
//
// The zero value is not useful; construct with New.
type Scrubber struct {
	// literals are exact strings to replace, longest-first so a hostname is
	// never partially eaten by a shorter substring of itself.
	literals     []literal
	keepLoopback bool
}

type literal struct {
	value string
	kind  Kind
}

// Option configures a Scrubber.
type Option func(*Scrubber)

// WithHost adds a hostname (and its short form) to scrub.
func WithHost(h string) Option {
	return func(s *Scrubber) {
		h = strings.TrimSpace(h)
		if h == "" {
			return
		}
		s.addLiteral(h, KindHost)
		// A FQDN also appears as its short form; both must go, and the short
		// form is registered separately so "workshop" is caught on its own.
		if short, _, ok := strings.Cut(h, "."); ok && short != "" {
			s.addLiteral(short, KindHost)
		}
	}
}

// WithUser adds a username to scrub.
func WithUser(u string) Option {
	return func(s *Scrubber) { s.addLiteral(strings.TrimSpace(u), KindUser) }
}

// WithHomeDir adds a home directory path to scrub.
func WithHomeDir(dir string) Option {
	return func(s *Scrubber) {
		dir = strings.TrimSpace(strings.TrimRight(dir, `/\`))
		if dir == "" {
			return
		}
		s.addLiteral(dir, KindPath)
		// Windows paths appear both ways once a path has been through a shell,
		// so register the alternate separator form too.
		if alt := filepath.ToSlash(dir); alt != dir {
			s.addLiteral(alt, KindPath)
		}
	}
}

// WithLiteral adds an arbitrary exact string to scrub under a kind.
func WithLiteral(v string, k Kind) Option {
	return func(s *Scrubber) { s.addLiteral(v, k) }
}

// KeepLoopback leaves 127.0.0.1 / ::1 / localhost intact.
//
// On by default: loopback is identical on every machine, so it identifies
// nobody, and "connect to localhost" is a procedure step worth keeping legible.
// Turn it off only when even that is too much.
func KeepLoopback(keep bool) Option {
	return func(s *Scrubber) { s.keepLoopback = keep }
}

func (s *Scrubber) addLiteral(v string, k Kind) {
	v = strings.TrimSpace(v)
	// Below three characters a "username" matches half the English language;
	// scrubbing it would shred the text without protecting anything.
	if len(v) < 3 {
		return
	}
	for _, l := range s.literals {
		if l.value == v {
			return
		}
	}
	s.literals = append(s.literals, literal{value: v, kind: k})
}

// New builds a Scrubber from explicit options only. It probes NOTHING about
// the host, which keeps it hermetic and makes tests deterministic; use
// FromHost for the live-machine scrubber.
func New(opts ...Option) *Scrubber {
	s := &Scrubber{keepLoopback: true}
	for _, o := range opts {
		o(s)
	}
	s.sortLiterals()
	return s
}

// FromHost builds a Scrubber seeded with this machine's identity, then applies
// any extra options.
//
// Detection is best-effort by design: a probe that fails must not disable
// redaction, because the pattern-based passes (IP, MAC, email) still run and
// are the ones that catch what a caller forgot to declare.
func FromHost(opts ...Option) *Scrubber {
	var seeded []Option
	if h, err := os.Hostname(); err == nil {
		seeded = append(seeded, WithHost(h))
	}
	if home, err := os.UserHomeDir(); err == nil {
		seeded = append(seeded, WithHomeDir(home))
		// The last path element is very often the username, and is a more
		// reliable source than $USER (which an agent harness may not set).
		if base := filepath.Base(home); base != "" && base != "." {
			seeded = append(seeded, WithUser(base))
		}
	}
	for _, k := range []string{"USER", "USERNAME", "LOGNAME"} {
		if v := os.Getenv(k); v != "" {
			seeded = append(seeded, WithUser(v))
		}
	}
	return New(append(seeded, opts...)...)
}

// sortLiterals orders replacements longest-first.
//
// Order is correctness, not tidiness: with "workshop.local" and "workshop" both
// registered, replacing the short form first would leave a dangling ".local"
// behind and split one host into two apparent entities.
func (s *Scrubber) sortLiterals() {
	sort.SliceStable(s.literals, func(i, j int) bool {
		if len(s.literals[i].value) != len(s.literals[j].value) {
			return len(s.literals[i].value) > len(s.literals[j].value)
		}
		return s.literals[i].value < s.literals[j].value
	})
}

// Tag is the placeholder for one value: stable, collision-resistant, and
// non-reversible.
func Tag(kind Kind, value string) string {
	sum := sha256.Sum256([]byte(string(kind) + "\x00" + value))
	return fmt.Sprintf("‹%s:%s›", kind, hex.EncodeToString(sum[:])[:4])
}

var (
	// Ordered most-specific first: an IPv4-looking run inside a MAC address
	// must not be clipped out before the MAC is recognised.
	macRE   = regexp.MustCompile(`\b(?:[0-9A-Fa-f]{2}[:-]){5}[0-9A-Fa-f]{2}\b`)
	emailRE = regexp.MustCompile(`\b[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}\b`)
	ipv4RE  = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)
	// Deliberately permissive as a CANDIDATE finder — empty groups are allowed
	// so `::` compression is caught — with net.ParseIP as the actual gate in
	// keepIP. Validating rather than pattern-perfecting is what keeps
	// "2001:db8::1" in scope while leaving a clock time like "10:30" alone: two
	// colon groups minimum means a single colon never qualifies.
	ipv6RE = regexp.MustCompile(`\b(?:[0-9A-Fa-f]{1,4})?(?::[0-9A-Fa-f]{0,4}){2,7}\b`)
)

// String scrubs text, returning the cleaned result.
func (s *Scrubber) String(text string) string {
	out, _ := s.Scrub(text)
	return out
}

// Strings scrubs each element (an argv, a set of tags).
func (s *Scrubber) Strings(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	for i, v := range in {
		out[i] = s.String(v)
	}
	return out
}

// Scrub returns the cleaned text and what was replaced.
//
// Declared literals are applied BEFORE the pattern passes, so a hostname the
// caller named explicitly is tagged as a host rather than being swept up by a
// looser pattern and mislabelled.
func (s *Scrubber) Scrub(text string) (string, []Finding) {
	if text == "" {
		return "", nil
	}
	var found []Finding
	out := text

	for _, l := range s.literals {
		if !strings.Contains(out, l.value) {
			continue
		}
		tag := Tag(l.kind, l.value)
		out = strings.ReplaceAll(out, l.value, tag)
		found = append(found, Finding{Kind: l.kind, Placeholder: tag, Value: l.value})
	}

	out = s.replacePattern(out, macRE, KindMAC, &found, nil)
	out = s.replacePattern(out, emailRE, KindEmail, &found, nil)
	out = s.replacePattern(out, ipv4RE, KindIP, &found, s.keepIP)
	out = s.replacePattern(out, ipv6RE, KindIP, &found, s.keepIP)

	return out, found
}

// replacePattern applies one regexp pass. keep, when non-nil, reports matches
// to leave alone.
func (s *Scrubber) replacePattern(text string, re *regexp.Regexp, kind Kind, found *[]Finding, keep func(string) bool) string {
	return re.ReplaceAllStringFunc(text, func(m string) string {
		if keep != nil && keep(m) {
			return m
		}
		tag := Tag(kind, m)
		for _, f := range *found {
			if f.Placeholder == tag {
				return tag
			}
		}
		*found = append(*found, Finding{Kind: kind, Placeholder: tag, Value: m})
		return tag
	})
}

// keepIP reports an address that carries no identity and is worth keeping
// legible.
//
// Loopback only. Private ranges are NOT kept: 10.x and 192.168.x describe a
// specific network's topology, which is exactly the kind of particular fact
// that must not travel off the host that learned it.
func (s *Scrubber) keepIP(m string) bool {
	if !s.keepLoopback {
		return false
	}
	ip := net.ParseIP(m)
	if ip == nil {
		// Not a real address (a version string like 1.2.3.4 in prose, or an
		// out-of-range quad). Leave it: tagging non-addresses would corrupt
		// text without protecting anything.
		return true
	}
	return ip.IsLoopback() || ip.IsUnspecified()
}

// Clean reports whether text contains nothing identifying — the assertion an
// export path makes before letting content leave the host.
func (s *Scrubber) Clean(text string) bool {
	_, found := s.Scrub(text)
	return len(found) == 0
}

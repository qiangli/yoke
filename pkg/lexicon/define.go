package lexicon

// DEFINE — "what is this word, here?"
//
// The one question an agent or a newcomer actually asks. Everything else in
// this package exists to be able to answer it.
//
// Three kinds of answer, and the third is the one most systems get wrong:
//
//	KNOWN      the term is in a projected registry — a verb, an agent binding,
//	           a skill, an env var, a local command, a path segment. Answered
//	           with what it denotes HERE.
//	SHAPED     not in any registry, but its FORM is recognisable — an address,
//	           an email, and above all something shaped like a credential.
//	UNKNOWN    genuinely not known. Said plainly.
//
// # Saying "I don't know" is a feature
//
// A resolver that guesses is worse than no resolver, because a confident wrong
// definition propagates: the agent acts on it, and nothing reports the error.
// This is the same failure the rest of the system is built to avoid — an
// absence of evidence must never be dressed up as an answer.
//
// # Classifying a secret without storing or echoing it
//
// "That looks like an API key" is a genuinely useful answer, and it is safe:
// it is a statement about SHAPE, computed on the spot from the argument the
// caller already holds. Nothing is stored, nothing is looked up, and the term
// is NOT echoed back — printing a credential into a terminal, a log, or an
// agent transcript is how it ends up somewhere permanent.
//
// So a sensitive term produces a classification and a warning, never a
// round-trip of the value.

import (
	"regexp"
	"sort"
	"strings"
)

// Definition is the answer to one lookup.
type Definition struct {
	// Term is the word asked about — EMPTY when Sensitive, so a credential
	// cannot be echoed back through a JSON pipeline or a log.
	Term string `json:"term,omitempty"`
	// Found reports a hit in a projected registry.
	Found bool `json:"found"`
	// Concept is the PRIMARY reading — the most authoritative one, first in
	// projection order.
	Concept *Concept `json:"concept,omitempty"`
	// Concepts is every reading, because on a real host one string genuinely is
	// several things at once: a name can be a username AND a hostname AND a
	// command. Reporting only the first is how a caller confidently acts in the
	// wrong world; the ambiguity is information, not noise to be resolved away.
	Concepts []*Concept `json:"concepts,omitempty"`
	// Classification describes the term's shape when it is not a known term.
	Classification string `json:"classification,omitempty"`
	// Advice is what to do about it.
	Advice string `json:"advice,omitempty"`
	// Sensitive marks a term that must not be echoed, logged, or stored.
	Sensitive bool `json:"sensitive,omitempty"`
}

var (
	// credentialish: long, unbroken, mixed-alphabet runs — the shape of a key
	// rather than of a word. Deliberately conservative: a false "looks like a
	// credential" costs one confusing answer, while a false "ordinary word"
	// invites someone to paste a live key somewhere permanent.
	credentialish = regexp.MustCompile(`^[A-Za-z0-9_\-./+=]{24,}$`)
	// Well-known credential prefixes, which are worth naming precisely because
	// the vendor documented them.
	credentialPrefixes = []string{
		"sk-", "sk_", "pk_", "ghp_", "gho_", "ghu_", "ghs_", "ghr_", "github_pat_",
		"xoxb-", "xoxp-", "xoxa-", "AKIA", "ASIA", "AIza", "ya29.", "hf_", "glpat-",
	}
	emailish = regexp.MustCompile(`^[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}$`)
	ipv4ish  = regexp.MustCompile(`^(?:\d{1,3}\.){3}\d{1,3}$`)
	macish   = regexp.MustCompile(`^(?:[0-9A-Fa-f]{2}[:-]){5}[0-9A-Fa-f]{2}$`)
	hexish   = regexp.MustCompile(`^[0-9a-f]{7,}$`)
	uuidish  = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

// Define answers what a term means on this host, across every namespace.
func (s *Store) Define(term string) Definition { return s.DefineKinds(term, nil) }

// DefineKinds is Define restricted to certain kinds — the namespace filter.
//
// A filter NARROWS the answer; it never invents one. Asking for a kind the term
// does not have yields "not known AS THAT", which is a different and more useful
// statement than "not known".
func (s *Store) DefineKinds(term string, kinds []Kind) Definition {
	t := strings.TrimSpace(term)
	if t == "" {
		return Definition{Advice: "nothing to define"}
	}

	// The credential check runs FIRST, before any lookup. A registry hit would
	// otherwise cause the term to be echoed in the answer, and by then it is
	// already in the transcript.
	if kind, ok := credentialShape(t); ok {
		return Definition{
			Sensitive:      true,
			Classification: kind,
			Advice: "not echoed, not stored, and not looked up. If this is live, " +
				"rotate it — a credential that has been pasted into a terminal is in " +
				"a history file, a scrollback buffer, and probably an agent transcript.",
		}
	}

	all := filterKinds(s.ResolveAll(t), kinds)
	if len(all) > 0 {
		return Definition{Term: t, Found: true, Concept: all[0], Concepts: all}
	}

	// A shape classification is about the STRING, so a kind filter does not
	// apply to it — reporting "that is an IP address" when the caller asked for
	// commands would answer a question nobody asked.
	if len(kinds) == 0 {
		if class, advice, ok := shapeOf(t); ok {
			return Definition{Term: t, Classification: class, Advice: advice}
		}
	}

	if len(kinds) > 0 {
		return Definition{
			Term: t,
			Advice: "not known as " + kindList(kinds) + " on this host. " +
				"Drop the filter to see whether it is known as something else.",
		}
	}
	// The advice names the namespaces this host CAN answer for, not just the
	// project vocabulary: `lexicon list` shows only the registry projections
	// (verbs, bindings, skills), so pointing a caller there after an unknown
	// env var or command sends them somewhere the answer could never have been.
	// Saying "I don't know" is only useful when it comes with what IS known.
	return Definition{
		Term: t,
		Advice: "not a known term on this host. It may be ordinary English, or jargon " +
			"this host has no registry for — `bashy define --list-kinds` shows every " +
			"namespace this host can answer for, and `bashy lexicon list` the project's " +
			"own vocabulary.",
	}
}

// filterKinds narrows readings to the requested kinds; nil keeps everything.
func filterKinds(in []*Concept, kinds []Kind) []*Concept {
	if len(kinds) == 0 {
		return in
	}
	want := make(map[Kind]bool, len(kinds))
	for _, k := range kinds {
		want[k] = true
	}
	out := in[:0:0]
	for _, c := range in {
		if want[c.Kind] {
			out = append(out, c)
		}
	}
	return out
}

func kindList(kinds []Kind) string {
	parts := make([]string, 0, len(kinds))
	for _, k := range kinds {
		parts = append(parts, string(k))
	}
	return strings.Join(parts, " or ")
}

// Kinds returns every kind the store currently holds, sorted — the closed
// vocabulary a --kind filter may name, derived rather than hard-coded so it
// cannot drift from what is actually projected.
func (s *Store) Kinds() []string {
	seen := map[string]bool{}
	var out []string
	for i := range s.Concepts {
		if k := string(s.Concepts[i].Kind); k != "" && !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// credentialShape reports a term that looks like a secret.
func credentialShape(t string) (string, bool) {
	for _, p := range credentialPrefixes {
		if strings.HasPrefix(t, p) {
			return "a credential (recognised vendor token prefix)", true
		}
	}
	// A long unbroken mixed-alphabet run with no separators is key-shaped. A
	// hex run is excluded here and classified below: git shas are ubiquitous
	// and calling every one a secret would train people to ignore the warning.
	if credentialish.MatchString(t) && !hexish.MatchString(t) && hasMixedAlphabet(t) {
		return "possibly a credential (long, high-entropy, no word structure)", true
	}
	return "", false
}

// hasMixedAlphabet reports both letters and digits, which is what separates a
// key from a long identifier like a package path.
func hasMixedAlphabet(t string) bool {
	var letters, digits bool
	for _, r := range t {
		switch {
		case r >= '0' && r <= '9':
			digits = true
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'):
			letters = true
		}
	}
	return letters && digits
}

// shapeOf classifies a term by form when no registry knows it.
func shapeOf(t string) (class, advice string, ok bool) {
	switch {
	case uuidish.MatchString(t):
		return "a UUID", "an identifier, not vocabulary — it denotes one object, not a concept", true
	case emailish.MatchString(t):
		return "an email address", "identity, not vocabulary — it is not stored in the lexicon", true
	case macish.MatchString(t):
		return "a MAC address", "hardware identity, not vocabulary", true
	case ipv4ish.MatchString(t):
		return "an IPv4 address", "network identity — it names a host, but not a concept", true
	case hexish.MatchString(t) && len(t) >= 7:
		return "a hex digest (a git sha, a content hash, or similar)",
			"a content address, not vocabulary — `bashy git show` may know it", true
	}
	return "", "", false
}

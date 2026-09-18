package craft

// RESOLVE — ask for a capability in your own words.
//
// The catalog is addressed by NAME today: you must already know a skill is
// called `go-repo-health` to reach it. That is a 1980s API for a knowledge
// base, and it is also the mechanism behind the failure mode this whole design
// exists to remove.
//
// Skill shadowing is, definitionally, a SELECTION FAILURE AMONG NAMED PEERS:
// the model is handed a list and picks the wrong row, and the more
// semantically-overlapping rows there are, the worse it gets. Capability keys
// already stop variants from being peers. Query resolution removes the list.
// Together the failure is not mitigated but unrepresentable — there is no menu
// to mis-pick from, because the answer is composed rather than chosen.
//
// # The floor never needs a model
//
// Retrieval degrades in tiers, and the bottom tier is stdlib-only:
//
//	tier 0  field-weighted lexical scoring + GRAPH EXPANSION
//	tier 1  TF-IDF over the corpus            (not yet built)
//	tier 2  embeddings, injected              (not yet built)
//
// Tier 0 does more than it sounds like, because of the expansion: a lexical hit
// on one node drags in its typed neighbourhood — the other implementations of
// the same capability, the skills that compose it. That supplies much of what
// people reach for embeddings to get, at zero dependency cost.
//
// Standalone-first is not negotiable here: every gate and every test runs at the
// floor, so results are reproducible. A tier that needs a network is an
// accelerator, never a requirement.
//
// # Nothing beats the least-bad row
//
// Scoring alone will always rank something first, so a ranker with no admission
// rule answers every question, including the ones this host has no answer to. A
// result must therefore account for more than half the meaningful words in the
// question before it is returned at all, and matching is per WORD rather than
// per substring. Both rules exist because of one measured answer: `find "ssh
// into a machine"` returned a Go build-and-test gate, on the strength of the
// word "machine-verified" in its prose.
//
// # Name is a matcher, not a key
//
// An exact name match is simply the highest-precedence signal inside Resolve,
// which is what keeps `bashy conductor` and bare-name dispatch working while the
// primary interface becomes a question.

import (
	"sort"
	"strings"
	"unicode"

	dhntskills "github.com/dhnt/dhnt/skills"

	"github.com/qiangli/yoke/pkg/skills"
)

// Implementation is one skill under a capability.
type Implementation struct {
	Name     string
	Identity string
	Skill    dhntskills.Skill
	// Description is the prose one-liner, when the catalog carries one.
	Description string
	// Bindings map contract predicates and step primitives to concrete
	// commands. Their presence is what makes a band-0 rendering possible.
	Bindings map[string]string
}

// Capability groups the implementations that make one guarantee.
type Capability struct {
	Key             string
	Implementations []Implementation
}

// Match is one scored result.
type Match struct {
	Capability *Capability `json:"-"`
	Key        string      `json:"capability"`
	// Primary is the elected implementation for this coordinate.
	Primary Implementation `json:"-"`
	Name    string         `json:"name"`
	Score   float64        `json:"score"`
	// Why names the signals that fired, so a ranking can be explained rather
	// than trusted. A retrieval system nobody can interrogate is one nobody can
	// debug when it starts returning the wrong thing.
	Why []string `json:"why,omitempty"`
	// Terms is how many distinct meaningful words the question carried; Covered
	// is how many of them this capability accounted for. The pair is what a
	// score alone cannot say: whether the answer addressed the QUESTION or
	// merely recognised one incidental word in it.
	Terms   int `json:"terms,omitempty"`
	Covered int `json:"covered,omitempty"`
	// Alternatives counts the other implementations of the same guarantee.
	Alternatives int `json:"alternatives,omitempty"`
}

// Index is the queryable view over capabilities.
type Index struct {
	caps  map[string]*Capability
	order []string // insertion order, for deterministic tie-breaking
}

// NewIndex builds an index from implementations, grouping by capability.
//
// An implementation with no contract has no capability and is DROPPED here
// rather than given a synthetic one: it states no guarantee, so there is nothing
// for a query about a guarantee to match.
func NewIndex(impls []Implementation) *Index {
	ix := &Index{caps: map[string]*Capability{}}
	for _, im := range impls {
		key, err := skills.CapabilityKey(im.Skill)
		if err != nil {
			continue
		}
		c, ok := ix.caps[key]
		if !ok {
			c = &Capability{Key: key}
			ix.caps[key] = c
			ix.order = append(ix.order, key)
		}
		c.Implementations = append(c.Implementations, im)
	}
	return ix
}

// Len reports how many distinct capabilities are indexed.
func (ix *Index) Len() int { return len(ix.caps) }

// Get returns one capability by key.
func (ix *Index) Get(key string) (*Capability, bool) {
	c, ok := ix.caps[key]
	return c, ok
}

// Query is a retrieval request.
type Query struct {
	// Text is what the caller actually asked, in their own words.
	Text string
	// Coordinate is the space-time context key; when set, implementations
	// attested here are preferred over ones attested elsewhere.
	Coordinate string
	// Limit caps results (default 5).
	Limit int
}

// field weights. Deliberately close to pkg/kb's, which were tuned on real
// retrieval rather than invented here: a name match is worth several body
// matches, because a name is chosen to be searched for.
const (
	wName      = 4.0
	wDesc      = 3.0
	wPredicate = 3.0
	wBinding   = 2.0 // below description: a binding is how, not what is guaranteed
	wEffect    = 1.0
	wExact     = 100.0 // an exact name match outranks everything
)

// Resolve ranks capabilities against a query.
func (ix *Index) Resolve(q Query) []Match {
	terms := tokenize(q.Text)
	limit := q.Limit
	if limit <= 0 {
		limit = 5
	}

	var out []Match
	for _, key := range ix.order {
		c := ix.caps[key]
		m, ok := ix.score(c, q, terms)
		if !ok {
			continue
		}
		out = append(out, m)
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		// Deterministic tie-break: the index must return the same order twice,
		// or nothing built on it is reproducible.
		return out[i].Name < out[j].Name
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func (ix *Index) score(c *Capability, q Query, terms []string) (Match, bool) {
	primary := elect(c, q.Coordinate)
	var score float64
	var why []string

	lowerQ := strings.ToLower(strings.TrimSpace(q.Text))
	exact := false
	for _, im := range c.Implementations {
		if strings.EqualFold(im.Name, lowerQ) {
			score += wExact
			why = append(why, "exact name match")
			primary = im
			exact = true
			break
		}
	}

	covered := 0
	for _, t := range terms {
		hit := false
		// A signal counts ONCE per capability, not once per implementation.
		// Scored per-implementation, a guarantee with three implementations
		// outranked a better answer with one — an artefact of how the catalog
		// happens to be written rather than of what was asked.
		if anyImpl(c, func(im Implementation) bool { return fieldHas(im.Name, t) }) {
			score += wName
			why = appendOnce(why, "name")
			hit = true
		}
		if anyImpl(c, func(im Implementation) bool { return fieldHas(im.Description, t) }) {
			score += wDesc
			why = appendOnce(why, "description")
			hit = true
		}
		// GRAPH EXPANSION: a capability is also described by what it
		// GUARANTEES, so contract predicates and effect atoms are searchable
		// text. This is what lets "verify the build" find a skill whose prose
		// never says "verify" — the contract says `builida`. The contract is
		// read off the primary because it is what the capability key is
		// computed over: every implementation under one key states the same
		// guarantee.
		for _, chk := range primary.Skill.Contract {
			if fieldHas(chk.Predicate, t) {
				score += wPredicate
				why = appendOnce(why, "contract")
				hit = true
			}
		}
		for _, e := range primary.Skill.EffectCap {
			if fieldHas(e.String(), t) {
				score += wEffect
				why = appendOnce(why, "effect")
				hit = true
			}
		}
		// BINDINGS ARE CUES. A decomposed skill states its steps as dhnt
		// predicates (`fanouto`, `coniveroge`) which no English question will
		// ever contain, so decomposition made two shipped skills LESS findable
		// than the prose they replaced: `find "fan out work to a fleet"` missed
		// a capability whose own binding reads `bashy weave fleet`. The English
		// survives in the bindings, so that is where it is searched.
		if anyImpl(c, func(im Implementation) bool {
			for _, cmd := range im.Bindings {
				if fieldHas(bindingCue(cmd), t) {
					return true
				}
			}
			return false
		}) {
			score += wBinding
			why = appendOnce(why, "binding")
			hit = true
		}
		if hit {
			covered++
		}
	}

	// THE RELEVANCE FLOOR, and it is the whole point of this function.
	//
	// A capability must account for MORE THAN HALF the meaningful words in the
	// question. Without it, one incidental word was a match: `find "ssh into a
	// machine"` returned a Go build-and-test gate, because its prose happens to
	// say "machine-verified" — full marks for confidence, no relation to ssh.
	//
	// That is worse than returning nothing. Nothing is a state the caller can
	// act on ("this host cannot do that yet"); a plausible wrong row is acted
	// on as an answer, and `compose` will happily render the wrong skill as a
	// runnable script. Silence beats a wrong answer.
	//
	// An exact name is exempt: it is not a guess, and bare-name dispatch has to
	// keep working.
	if !exact && 2*covered <= len(terms) {
		return Match{}, false
	}
	if score == 0 {
		return Match{}, false
	}
	return Match{
		Capability:   c,
		Key:          c.Key,
		Primary:      primary,
		Name:         primary.Name,
		Score:        score,
		Why:          why,
		Terms:        len(terms),
		Covered:      covered,
		Alternatives: len(c.Implementations) - 1,
	}, true
}

// bindingCue reduces a bound command to its discriminating words.
//
// It drops the PROGRAM NAME of every pipeline segment. Nearly every binding in
// this catalog starts with the same harness (`bashy …`), so indexing argv[0]
// would make one word match every capability at once — it would inflate `covered`
// and walk incidental matches straight through the relevance floor, which is the
// exact failure the floor was built to stop. argv[0] names the harness; the
// subcommand and its arguments name the capability, and those are kept.
func bindingCue(cmd string) string {
	var keep []string
	for _, seg := range strings.FieldsFunc(cmd, func(r rune) bool {
		return r == '|' || r == ';' || r == '&' || r == '\n'
	}) {
		f := strings.Fields(seg)
		if len(f) < 2 {
			continue // a bare program name carries nothing to search
		}
		for _, w := range f[1:] {
			keep = append(keep, strings.TrimLeft(w, "-"))
		}
	}
	return strings.Join(keep, " ")
}

func anyImpl(c *Capability, pred func(Implementation) bool) bool {
	for _, im := range c.Implementations {
		if pred(im) {
			return true
		}
	}
	return false
}

// fieldHas reports whether a query term matches any WORD of a field.
//
// Substring matching over the raw field was the other half of the wrong answer
// above: it makes every field a haystack in which short words are always found
// ("cat" inside "concatenate", "art" inside "start"), and those hits are
// indistinguishable from real ones by the time they reach the score.
//
// Matching is per word, and prefix-tolerant only where a prefix carries real
// information: "repo" reaches "repository" and "build" reaches "builds", while
// a token shorter than four characters must match a word exactly. Three-letter
// words are where substring matching does its damage, and they are also the
// ones a reader most expects to be precise.
func fieldHas(field, term string) bool {
	if field == "" || term == "" {
		return false
	}
	for _, w := range words(field) {
		if w == term {
			return true
		}
		short := len(w)
		if len(term) < short {
			short = len(term)
		}
		if short < 4 {
			continue
		}
		if strings.HasPrefix(w, term) || strings.HasPrefix(term, w) {
			return true
		}
	}
	return false
}

// words splits a field into lowercase word tokens, on the same boundaries
// tokenize uses so a query and the text it is matched against are cut the same
// way.
func words(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// elect picks the implementation to answer with.
//
// Deliberately simple for now: prefer one that can render at the lowest band
// (most bindings = least model needed), then fall back to declaration order.
// Election by ATTESTED success at this coordinate is the real answer and needs
// an evidence floor a single host does not reach alone — acting on a handful of
// runs would discard a sound implementation on noise.
func elect(c *Capability, coordinate string) Implementation {
	if len(c.Implementations) == 0 {
		return Implementation{}
	}
	best := c.Implementations[0]
	bestBound := len(best.Bindings)
	for _, im := range c.Implementations[1:] {
		if len(im.Bindings) > bestBound {
			best, bestBound = im, len(im.Bindings)
		}
	}
	return best
}

// tokenize lowercases and splits on non-letter/digit runs, dropping tokens
// shorter than three characters — below that a token matches everything and
// ranks nothing.
//
// Repeats are dropped too, and that is not tidiness: terms are the denominator
// of the relevance floor, so a question that says "build" twice must not become
// a question that is half-answered by matching "build" once.
func tokenize(s string) []string {
	fields := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	out := make([]string, 0, len(fields))
	seen := make(map[string]bool, len(fields))
	for _, f := range fields {
		if len(f) < 3 || stopWords[f] || seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
	}
	return out
}

// stopWords are the words a natural-language question is made of. They carry no
// retrieval signal and, left in, they match everything equally — which is worse
// than useless because it flattens the ranking.
//
// They are also the DENOMINATOR of the relevance floor, which is what makes the
// list worth maintaining rather than merely tidy: "make sure the build
// compiles" is a two-word question wearing five words, and counting the filler
// against the answer rejects a skill that answered everything asked.
//
// The bar for adding one is that it can never be the CONTENT of a question
// about a skill. "make", "run", "build" and "use" all can be, and are
// deliberately absent — a catalog that cannot be asked about `make` because
// somebody classified it as filler is the same failure as a subcommand
// stealing a word.
var stopWords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "how": true,
	"can": true, "you": true, "that": true, "this": true, "get": true,
	"into": true, "from": true, "want": true, "need": true, "does": true,
	"what": true, "when": true, "where": true, "have": true, "are": true,
	"please": true, "would": true, "could": true, "should": true, "sure": true,
	"also": true, "then": true, "will": true, "was": true, "were": true,
	"but": true, "not": true, "any": true, "who": true, "why": true,
	"did": true,
}

func appendOnce(dst []string, v string) []string {
	for _, s := range dst {
		if s == v {
			return dst
		}
	}
	return append(dst, v)
}

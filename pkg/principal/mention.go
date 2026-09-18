package principal

import (
	"regexp"
	"strings"
)

// A mention is `@name` or `@kind:name` in prose — a kb page body, a meet
// message, a weave issue. `@@` escapes a literal at-sign.
//
// One implementation resolves them, and every consumer calls it. The moment
// kb, meet, and weave each grow their own idea of what a name means, they
// drift — which is exactly the fragmentation this package exists to end.
var mentionRe = regexp.MustCompile(`@([a-z]+:)?([A-Za-z0-9][A-Za-z0-9._-]*)`)

// Mention is one `@name` found in text.
type Mention struct {
	Raw   string `json:"raw"`
	Kind  Kind   `json:"kind,omitempty"`
	Name  string `json:"name"`
	Start int    `json:"start"`
}

// Mentions extracts every mention from text, skipping `@@` escapes.
//
// A mention must open a word. Without that rule `alice@example.com` reads as
// a mention of `example.com`, and every email address in a kb page becomes a
// lint warning.
func Mentions(text string) []Mention {
	var out []Mention
	for _, m := range mentionRe.FindAllStringSubmatchIndex(text, -1) {
		start := m[0]
		if start > 0 && !opensWord(rune(text[start-1])) {
			continue // @@escape, or the @ of an email address
		}
		raw := text[m[0]:m[1]]
		var kind Kind
		if m[2] >= 0 {
			kind = Kind(strings.TrimSuffix(text[m[2]:m[3]], ":"))
			if !kinds[kind] {
				// `@user:something` is not a typed mention; treat the whole
				// token as a name so a false positive never becomes an error.
				continue
			}
		}
		// Dots are legal INSIDE a name (gpt-5.5, kimi-k2.7-code) but a name
		// never ends in one: `@smarty.` mentions smarty, not "smarty.".
		name := strings.TrimRight(text[m[4]:m[5]], "._-")
		if name == "" {
			continue
		}
		raw = raw[:len(raw)-(m[5]-m[4])+len(name)]
		out = append(out, Mention{Raw: raw, Kind: kind, Name: name, Start: start})
	}
	return out
}

// opensWord reports whether a mention may begin after this rune: anything
// that is not itself part of a word, and not another '@'.
func opensWord(r rune) bool {
	switch {
	case r == '@', r == '.', r == '-', r == '_':
		return false
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return false
	}
	return true
}

// Unresolved is a mention that names nothing this host knows.
type Unresolved struct {
	Mention
	Why string `json:"why"`
}

// CheckMentions resolves every mention in text and reports the ones that
// name nothing, or name more than one kind.
//
// Callers warn on the result; they do not fail. An unresolvable mention may
// simply live on another host that has not synced yet, and making a name
// resolve only when a control plane answers would put the network on the
// path of reading a page — precisely what standalone-first forbids. A strict
// mode belongs to CI, not to the default read path.
func (r *Resolver) CheckMentions(text string) []Unresolved {
	var out []Unresolved
	seen := map[string]bool{}
	for _, m := range Mentions(text) {
		q := m.Name
		if m.Kind != "" {
			q = string(m.Kind) + ":" + m.Name
		}
		if seen[q] {
			continue
		}
		seen[q] = true

		ans := r.Resolve(q)
		switch {
		case !ans.Resolved:
			out = append(out, Unresolved{Mention: m, Why: "names nothing on this host"})
		case ans.Ambiguous():
			out = append(out, Unresolved{Mention: m, Why: ambiguityHint(m.Name, ans.Kinds())})
		}
	}
	return out
}

// Expand rewrites each resolvable mention into a form an agent can act on:
// `@007` becomes `@007 (agent, claude:fable)`. This is where the registry
// earns its keep — kb, meet, and weave already inject context, so injecting
// resolved principals means every agent on the host reads one directory.
func (r *Resolver) Expand(text string) string {
	ms := Mentions(text)
	if len(ms) == 0 {
		return text
	}
	var b strings.Builder
	last := 0
	for _, m := range ms {
		q := m.Name
		if m.Kind != "" {
			q = string(m.Kind) + ":" + m.Name
		}
		ans := r.Resolve(q)
		if !ans.Resolved || ans.Ambiguous() {
			continue
		}
		res := ans.Matches[0]
		end := m.Start + len(m.Raw)
		b.WriteString(text[last:end])
		b.WriteString(" (" + string(res.Kind))
		if res.Summary != "" {
			b.WriteString(", " + res.Summary)
		}
		b.WriteString(")")
		last = end
	}
	b.WriteString(text[last:])
	return b.String()
}

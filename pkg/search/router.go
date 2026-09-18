package search

import "strings"

// Lane is the search primitive a query is routed to. The router classifies a
// query to a lane and dispatches to the cheapest primitive that can answer it —
// this is bashy search's spine (see docs/bashy-search-design.md). No lane builds
// a persistent code index.
type Lane string

const (
	LaneContent Lane = "content" // literal/regex text → grep-style scan
	LaneFiles   Lane = "files"   // filename/path → find
	LaneSymbol  Lane = "symbol"  // "where is X defined" → ast (treesitter)
	LaneRefs    Lane = "refs"    // "who calls X" / impact → graph / ast refs
	LaneKB      Lane = "kb"      // concept / lesson → kb knowledge
)

// lanePrefixes are explicit query prefixes that force a lane. An operator (or an
// agent that knows its intent) always wins over the heuristic.
var lanePrefixes = []struct {
	prefix string
	lane   Lane
}{
	{"sym:", LaneSymbol}, {"def:", LaneSymbol},
	{"ref:", LaneRefs}, {"refs:", LaneRefs}, {"uses:", LaneRefs}, {"callers:", LaneRefs}, {"impact:", LaneRefs},
	{"file:", LaneFiles}, {"path:", LaneFiles}, {"name:", LaneFiles},
	{"kb:", LaneKB},
	{"content:", LaneContent}, {"grep:", LaneContent},
}

// Classify routes a query to a lane. An explicit prefix wins; otherwise the
// default is LaneContent — the always-safe, always-useful lane (a text scan
// answers or narrows nearly anything, and never lies about structure). Symbol
// and reference intent must be asked for explicitly (a bareword is ambiguous
// between "the string Foo" and "the definition of Foo"; defaulting to content
// is the honest, non-surprising choice).
func Classify(query string) (Lane, string) {
	q := strings.TrimSpace(query)
	lower := strings.ToLower(q)
	for _, p := range lanePrefixes {
		if strings.HasPrefix(lower, p.prefix) {
			return p.lane, strings.TrimSpace(q[len(p.prefix):])
		}
	}
	return LaneContent, q
}

// laneVerb names the specialized bashy verb a lane not yet wired as a callable
// library resolves through, so the router can point an agent at it. Content /
// files / kb are answered in-process (see Local); symbol / refs route to the
// code-intel verbs until those expose a library API (the P0.5 extraction).
func laneVerb(l Lane) string {
	switch l {
	case LaneSymbol:
		return "ast search"
	case LaneRefs:
		return "graph impact / ast refs"
	default:
		return ""
	}
}

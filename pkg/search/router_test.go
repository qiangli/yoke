package search

import "testing"

func TestClassify(t *testing.T) {
	cases := []struct {
		query string
		lane  Lane
		term  string
	}{
		{"HandleRequest", LaneContent, "HandleRequest"},    // bareword defaults to content (honest: string, not symbol)
		{"func.*Handler", LaneContent, "func.*Handler"},    // regex-shaped → content
		{"sym:HandleRequest", LaneSymbol, "HandleRequest"}, // explicit symbol
		{"def:Indexer", LaneSymbol, "Indexer"},             // def: → symbol
		{"ref:HandleRequest", LaneRefs, "HandleRequest"},   // who-calls
		{"callers:Foo", LaneRefs, "Foo"},                   // callers → refs
		{"impact:pkg/search", LaneRefs, "pkg/search"},      // impact → refs
		{"file:local.go", LaneFiles, "local.go"},           // filename
		{"path:pkg/search", LaneFiles, "pkg/search"},       // path → files
		{"kb:retry policy", LaneKB, "retry policy"},        // knowledge
		{"grep:TODO", LaneContent, "TODO"},                 // explicit content
		{"  SYM:Foo  ", LaneSymbol, "Foo"},                 // case-insensitive prefix + trim
	}
	for _, c := range cases {
		lane, term := Classify(c.query)
		if lane != c.lane || term != c.term {
			t.Errorf("Classify(%q) = (%s, %q), want (%s, %q)", c.query, lane, term, c.lane, c.term)
		}
	}
}

func TestBuildMatcher(t *testing.T) {
	// literal substring, case-insensitive
	if !buildMatcher("handler")("the Handler here") {
		t.Error("literal match should be case-insensitive")
	}
	if buildMatcher("handler")("nothing here") {
		t.Error("literal non-match")
	}
	// regex when metacharacters present
	if !buildMatcher("func.*Handler")("func fooHandler() {") {
		t.Error("regex should match")
	}
	// a broken regex degrades to literal, not a panic/error
	if !buildMatcher("foo(")("a foo( call") {
		t.Error("bad regex should fall back to literal substring")
	}
}

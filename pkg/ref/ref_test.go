// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package ref

import (
	"errors"
	"go/build"
	"slices"
	"strings"
	"testing"
)

// TestKindsIsTheVocabulary is the RATCHET. The vocabulary is closed; this
// table is the design note's list plus role (D2). Adding a kind means editing
// this test in the same commit as the table — deliberately, so a new kind is a
// reviewed decision and the atlas/docs row lands with it.
func TestKindsIsTheVocabulary(t *testing.T) {
	want := []Kind{"kb", "todo", "sprint", "run", "meet", "mb", "bus",
		"agent", "person", "host", "tool", "model", "skill", "episode", "role"}
	if got := Kinds(); !slices.Equal(got, want) {
		t.Fatalf("vocabulary drifted:\n got %v\nwant %v\n(a new kind needs a table row here, a resolver in its store, and an atlas row)", got, want)
	}
	if len(Kinds()) != 15 {
		t.Fatalf("15 kinds, got %d", len(Kinds()))
	}
	for _, k := range Kinds() {
		if !Known(k) {
			t.Errorf("Known(%q) = false for a vocabulary kind", k)
		}
	}
	if Known(Unknown) {
		t.Error("Unknown must never be a vocabulary member")
	}
	if Known("issue") {
		t.Error("issue is not a kind: it is todo at repo scope (docs/work-tracking-model.md)")
	}
}

// TestLeaf pins that pkg/ref imports the standard library only. Every store
// returns ref.Node; the first non-stdlib import here is the first cycle.
func TestLeaf(t *testing.T) {
	pkg, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, imp := range pkg.Imports {
		if strings.Contains(imp, ".") {
			t.Errorf("pkg/ref imports %q — it must stay standard-library only", imp)
		}
	}
}

func TestParseSpellings(t *testing.T) {
	cases := []struct {
		in   string
		kind Kind
		id   string
		qual string
	}{
		{"kb:deploy-runbook", KB, "deploy-runbook", ""},
		{"  todo:a5f5cfc8  ", Todo, "a5f5cfc8", ""},
		{"todo:#a5f5cfc8", Todo, "a5f5cfc8", ""},
		{"sprint:#168", Sprint, "168", ""},
		{"run:bashy-12", Run, "bashy-12", ""},
		{"meet:m-9f3a", Meet, "m-9f3a", ""},
		{"mb:412", MB, "412", ""},
		{"bus:#77", Bus, "77", ""},
		{"agent:codex-gpt5.6-sol", Agent, "codex-gpt5.6-sol", ""},
		{"person:qli", Person, "qli", ""},
		{"host:buildbox", Host, "buildbox", ""},
		{"tool:codex", Tool, "codex", ""},
		{"model:opus4.8", Model, "opus4.8", ""},
		{"skill:conductor", Skill, "conductor", ""},
		{"episode:e-2026-09-13-1", Episode, "e-2026-09-13-1", ""},
		{"role:steward", Role, "steward", ""},
		{"role:conductor:22", Role, "conductor:22", ""}, // the id may itself carry a colon
		// fully qualified
		{"urn:dhnt:kb:deploy-runbook", KB, "deploy-runbook", ""},
		{"urn:dhnt:sprint:168", Sprint, "168", ""},
		{"urn:dhnt:role:conductor:22", Role, "conductor:22", ""},
		// legacy principal spelling, input only
		{"dhnt:agent/codex", Agent, "codex", ""},
		{"dhnt:person/qli@someone@example.com", Person, "qli", "someone@example.com"},
		{"dhnt:host/buildbox", Host, "buildbox", ""},
	}
	for _, c := range cases {
		r, err := Parse(c.in)
		if err != nil {
			t.Errorf("Parse(%q): %v", c.in, err)
			continue
		}
		if r.Kind != c.kind || r.ID != c.id || r.Qualifier != c.qual {
			t.Errorf("Parse(%q) = %+v, want kind=%q id=%q qual=%q", c.in, r, c.kind, c.id, c.qual)
		}
		if r.Raw != c.in {
			t.Errorf("Parse(%q).Raw = %q, want the input verbatim", c.in, r.Raw)
		}
		if got := r.String(); got != string(c.kind)+":"+c.id {
			t.Errorf("String() = %q", got)
		}
		if got := r.URN(); got != "urn:dhnt:"+string(c.kind)+":"+c.id {
			t.Errorf("URN() = %q", got)
		}
		// Round trip: the canonical and URN spellings parse back to the same ref.
		for _, again := range []string{r.String(), r.URN()} {
			r2, err := Parse(again)
			if err != nil || r2.Kind != c.kind || r2.ID != c.id {
				t.Errorf("round trip %q → %+v, %v", again, r2, err)
			}
		}
	}
}

// TestParseNotRef: a bare word and a `x:y` with a non-vocabulary prefix are
// WORDS, not refs. The tool:model binding shape (`codex:gpt5.6-sol`) is the
// one that matters — principal.SplitQuery guards it and so must this.
func TestParseNotRef(t *testing.T) {
	for _, in := range []string{
		"", "   ", "handoff", "deploy-runbook",
		"codex:gpt5.6-sol", "opencode:deepseek-v4-pro", "claude:opus5",
		"note: remember this", "https://example.com/x", "mailto:x@y",
		"dhnt:agent",   // legacy prefix without the slash is not the legacy shape
		"urn:sprint:1", // bare urn:<kind>: is refused by design (no NID)
	} {
		_, err := Parse(in)
		if !errors.Is(err, ErrNotRef) {
			t.Errorf("Parse(%q) err = %v, want ErrNotRef", in, err)
		}
		if Is(in) {
			t.Errorf("Is(%q) = true", in)
		}
	}
}

// TestParseUnknownKind: the fully-qualified and legacy spellings CLAIM to be
// refs, so an unknown kind there is an error worth explaining — not silence.
func TestParseUnknownKind(t *testing.T) {
	for _, in := range []string{"urn:dhnt:issue:12", "urn:dhnt:foo:bar", "dhnt:widget/x"} {
		r, err := Parse(in)
		if !errors.Is(err, ErrUnknownKind) {
			t.Errorf("Parse(%q) err = %v, want ErrUnknownKind", in, err)
		}
		if r.Kind != Unknown {
			t.Errorf("Parse(%q).Kind = %q, want Unknown", in, r.Kind)
		}
	}
}

func TestParseEmptyID(t *testing.T) {
	for _, in := range []string{"kb:", "todo:#", "urn:dhnt:sprint:", "dhnt:agent/", "dhnt:agent/@owner"} {
		if _, err := Parse(in); !errors.Is(err, ErrEmptyID) {
			t.Errorf("Parse(%q) err = %v, want ErrEmptyID", in, err)
		}
	}
}

func TestRegistry(t *testing.T) {
	g := NewRegistry()
	if m := g.Missing(); len(m) != len(Kinds()) {
		t.Fatalf("empty registry misses %d kinds, want all %d", len(m), len(Kinds()))
	}
	g.Register(KB, ResolverFunc(func(id string) (Node, error) {
		if id != "here" {
			return Node{}, ErrNotFound
		}
		n := NewNode(KB, id)
		n.Title = "a page"
		return n, nil
	}))
	if m := g.Missing(); slices.Contains(m, KB) || len(m) != len(Kinds())-1 {
		t.Fatalf("Missing after one Register = %v", m)
	}

	n, err := g.Resolve("urn:dhnt:kb:here")
	if err != nil || n.Ref != "kb:here" || n.Title != "a page" {
		t.Fatalf("Resolve = %+v, %v", n, err)
	}
	if _, err := g.Resolve("kb:gone"); !errors.Is(err, ErrNotFound) {
		t.Errorf("not found → %v", err)
	}
	if _, err := g.Resolve("sprint:1"); !errors.Is(err, ErrNoResolver) {
		t.Errorf("unwired kind → %v, want ErrNoResolver (distinct from not found)", err)
	}
	if _, err := g.Resolve("codex:gpt5.6-sol"); !errors.Is(err, ErrNotRef) {
		t.Errorf("binding → %v, want ErrNotRef", err)
	}
	if _, err := g.Resolve("urn:dhnt:foo:x"); !errors.Is(err, ErrUnknownKind) {
		t.Errorf("unknown kind → %v", err)
	}

	// A resolver that forgets Ref gets it filled.
	g.Register(Sprint, ResolverFunc(func(id string) (Node, error) { return Node{Kind: Sprint, ID: id}, nil }))
	if n, _ := g.Resolve("sprint:168"); n.Ref != "sprint:168" {
		t.Errorf("Ref not filled: %+v", n)
	}

	defer func() {
		if recover() == nil {
			t.Error("Register(Unknown) must panic: the vocabulary is closed")
		}
	}()
	g.Register(Unknown, nil)
}

func TestShapeOf(t *testing.T) {
	cases := map[string]Shape{
		"0192f3a4":                               ShapeUID, // UUIDv7 prefix — leading zero keeps it out of seq
		"12345678":                               ShapeSeq, // decimal wins over hex at 8
		"deadbeef":                               ShapeUID,
		"123456789012":                           ShapeUID, // 12 hex is ALWAYS a uid (an all-digit todo id)
		"c081345fd901":                           ShapeUID,
		"release-cycle":                          ShapeSlug,
		"cafe":                                   ShapeSlug, // hex but too short for a prefix
		"#3":                                     ShapeSeq,
		"3":                                      ShapeSeq,
		"007":                                    ShapeSlug, // leading zero: not a seq, too short for a uid
		"":                                       ShapeSlug,
		"0192f3a4-7c1e-7d2a-9b1f-1234567890ab":   ShapeUID,
		"0192f3a4-7c1e":                          ShapeUID,  // dashed prefix
		"0192f3a4-7c1e-7d2a-9b1f-1234567890abcd": ShapeSlug, // too long for a uuid
		"not-a-uuid-1":                           ShapeSlug,
		"e2e-refs-note":                          ShapeSlug,
	}
	for in, want := range cases {
		if got := ShapeOf(in); got != want {
			t.Errorf("ShapeOf(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestSplitScope(t *testing.T) {
	for _, c := range []struct{ in, scope, local string }{
		{"release-cycle", "", "release-cycle"},
		{"coreutils/release-cycle", "coreutils", "release-cycle"},
		{"coreutils/148", "coreutils", "148"},
		{"user/3", "user", "3"},
		{"a/b/c", "a/b", "c"},     // the scope is an ancestral PATH; the local part is one handle (D14)
		{"alice/hostA/226", "alice/hostA", "226"},
	} {
		scope, local := SplitScope(c.in)
		if scope != c.scope || local != c.local {
			t.Errorf("SplitScope(%q) = (%q, %q), want (%q, %q)", c.in, scope, local, c.scope, c.local)
		}
	}
	// `#` is stripped on the local part only, scope untouched.
	r, err := Parse("todo:coreutils/#3")
	if err != nil || r.ID != "coreutils/3" {
		t.Fatalf("Parse(todo:coreutils/#3) = %+v, %v", r, err)
	}
	r, err = Parse("kb:#22")
	if err != nil || r.ID != "22" {
		t.Fatalf("Parse(kb:#22) = %+v, %v", r, err)
	}
}

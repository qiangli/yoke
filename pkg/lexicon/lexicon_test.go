// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package lexicon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func store(t *testing.T) *Store {
	t.Helper()
	// nil catalog: verbs only. The binding half is host-specific by nature, and a
	// test that depended on a real fleet registry would be testing the host, not
	// the code.
	return Build(nil, map[string]string{
		"handoff": "pause this session and hand the work on",
		"gate":    "does this project pass?",
	}, "test-host", Overlay{})
}

// The headline: the word a user actually says must resolve to the thing it denotes
// HERE. "handoff" is not the English word.
func TestResolvesTheBareWord(t *testing.T) {
	s := store(t)
	c, ok := s.Resolve("handoff")
	if !ok {
		t.Fatal(`"handoff" did not resolve — the word a user actually says must work`)
	}
	if c.Kind != KindVerb || c.PrefLabel != "bashy handoff" {
		t.Fatalf("resolved to the wrong thing: %+v", c)
	}
	if c.ScopeNote == "" {
		t.Fatal("no scope note: the precedence rule is the whole point")
	}
}

// A term must resolve in all three forms it appears in the wild: bare in speech,
// [[marked]] in an artifact, and namespaced when a human is disambiguating.
func TestResolvesMarkedAndNamespaced(t *testing.T) {
	s := store(t)
	for _, form := range []string{"handoff", "[[handoff]]", "verb:handoff", "[[verb:handoff]]", "HANDOFF"} {
		if _, ok := s.Resolve(form); !ok {
			t.Errorf("%q did not resolve", form)
		}
	}
}

// An ALIAS is a term, not a concept. Hearing the OLD word must resolve to the NEW
// thing — otherwise every rename silently breaks the vocabulary of everyone who has
// not caught up yet.
func TestAliasesResolveToTheirConcept(t *testing.T) {
	s := store(t)
	c, ok := s.Resolve("invoke") // compatibility alias for the canonical `chat`
	if !ok {
		t.Fatal(`the compatibility name "invoke" did not resolve`)
	}
	if c.PrefLabel != "bashy chat" {
		t.Fatalf(`"invoke" resolved to %q, want the canonical chat concept`, c.PrefLabel)
	}
	if _, ok := s.Resolve("verify"); !ok {
		t.Error(`the old name "verify" did not resolve to conform`)
	}
}

// An unknown term is an ERROR, never an empty answer. Silence invites the agent to
// fall back on the English word — the exact failure this feature exists to prevent.
func TestUnknownTermDoesNotResolve(t *testing.T) {
	s := store(t)
	if _, ok := s.Resolve("banana"); ok {
		t.Fatal("an ordinary English word resolved as jargon")
	}
}

// EVERY term must resolve to ITS OWN concept. This is a regression test for a bug
// that shipped in the first build and was caught only by dogfooding: byTerm stored
// *Concept pointers into the s.Concepts slice, and `append` REALLOCATES the backing
// array — so every pointer taken before a growth silently aliased the OLD array.
// The symptom was spectacular and would have poisoned the whole feature:
//
//	$ bashy lexicon resolve codex
//	bashy yarn  (verb)
//
// Indexes are stable across append and across sort; pointers into a growing slice
// are not. A cross-check of every term against its own concept is cheap and would
// have caught it before a human ever saw it.
func TestEveryTermResolvesToItsOwnConcept(t *testing.T) {
	s := store(t)
	if len(s.Concepts) < 5 {
		t.Fatalf("only %d concepts — the test is not exercising slice growth", len(s.Concepts))
	}
	for i := range s.Concepts {
		c := s.Concepts[i]
		got, ok := s.Resolve(c.PrefLabel)
		if !ok {
			t.Errorf("%q (its own prefLabel) did not resolve", c.PrefLabel)
			continue
		}
		if got.ID != c.ID {
			t.Errorf("%q resolved to %q — a term must resolve to ITS OWN concept "+
				"(pointer aliasing into a reallocated slice?)", c.PrefLabel, got.ID)
		}
		for _, alt := range c.AltLabels {
			got, ok := s.Resolve(alt)
			if !ok {
				t.Errorf("alt label %q of %q did not resolve", alt, c.ID)
				continue
			}
			if got.ID != c.ID {
				t.Errorf("alt label %q of %q resolved to %q", alt, c.ID, got.ID)
			}
		}
	}
}

// The marker makes the lexicon FALSIFIABLE: a [[term]] that resolves to nothing is
// a BROKEN LINK, and broken links are findable. A prose glossary rots silently; a
// linked one cannot.
func TestUnresolvedFindsBrokenLinks(t *testing.T) {
	s := store(t)
	text := "First [[handoff]] the work, then [[gate]] it, then [[flurb]] it."
	got := s.Unresolved(text)
	if len(got) != 1 || got[0] != "flurb" {
		t.Fatalf("Unresolved = %v, want exactly [flurb] — the one term that names nothing", got)
	}
}

// The always-on block must carry the precedence rule and the resolver, and must NOT
// dump the registry: selection accuracy degrades past ~15-20 terms in rotation.
func TestEmitCarriesPrecedenceAndResolverAndStaysShort(t *testing.T) {
	s := store(t)
	block := s.EmitAgentsMD("test")

	if !strings.Contains(block, "NOT their") || !strings.Contains(block, "English senses") {
		t.Fatal("the block does not state the precedence rule — the one sentence that does most of the work")
	}
	if !strings.Contains(block, "lexicon resolve") {
		t.Fatal("the block does not carry the resolver command — the long tail is unreachable without it")
	}
	terms := strings.Count(block, "\n- **")
	if terms > MaxAlwaysOn {
		t.Fatalf("the block lists %d terms, over the %d cap — dumping the registry HARMS grounding", terms, MaxAlwaysOn)
	}
	if !strings.Contains(block, BeginMarker) || !strings.Contains(block, EndMarker) {
		t.Fatal("the block is not delimited, so it cannot be regenerated in place")
	}
}

// Regeneration must be idempotent and must REPLACE, never append. Otherwise every
// run grows the file and the stale copy above outlives the fresh one below.
func TestWriteIntoIsIdempotentAndReplaces(t *testing.T) {
	s := store(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "AGENTS.md")
	if err := os.WriteFile(p, []byte("# Project\n\nHand-written prose that must survive.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	block := s.EmitAgentsMD("test")
	for i := 0; i < 3; i++ {
		if err := WriteInto(p, block); err != nil {
			t.Fatal(err)
		}
	}
	b, _ := os.ReadFile(p)
	got := string(b)

	if n := strings.Count(got, BeginMarker); n != 1 {
		t.Fatalf("the block appears %d times after 3 writes — it must REPLACE, not append", n)
	}
	if !strings.Contains(got, "Hand-written prose that must survive.") {
		t.Fatal("regeneration destroyed the hand-written content around it")
	}
}

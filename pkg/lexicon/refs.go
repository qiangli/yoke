// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package lexicon

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/qiangli/yoke/pkg/ref"
)

// RefResolvers is the registry `define <kind>:<id>` asks. The embedding shell
// fills it — one resolver per vocabulary kind, from the store that owns the
// kind — and this package only ever reads it.
//
// A hook rather than an import, for the same reason as Synopses, KnownCommands,
// RecordDiscovery and SkillSource: pkg/lexicon imports NO store package. The
// stores (kb, todo, weave, meet, fleet, …) import each other in a partial
// order and several already reach the fleet this package projects agents
// from; the first store imported here is an import cycle. pkg/ref is the leaf
// they all sit on, so the registry can live here and the shell can register
// into it. WIRING IT IS THE LOAD-BEARING STEP: an empty registry does not make
// a ref "unknown" — define reports the kind as not wired, which is a
// different answer from "not found", and ref.Registry.Missing lets the shell's
// coverage test prove every kind is registered.
var RefResolvers = ref.NewRegistry()

// errNotARef is defineRef's "not mine": the token is not shaped like a ref,
// so the caller runs the term path. Every other outcome is decided in defineRef.
var errNotARef = errors.New("lexicon: not a ref")

// defineRef is the ref half of `define`: it runs BEFORE the term path, and
// hands back errNotARef when the token is not shaped like a ref at all, so
// the caller falls through to the existing lookup unchanged (a bare word, a
// `tool:model` binding such as codex:gpt5.6-sol, the prose `note: x`).
//
// Every other outcome is decided here. The five are kept distinct on purpose:
// found (the node), not found (the store was read and the id is absent), no
// resolver (this build never wired the kind — the nil-hook state, and it must
// never read as an empty store), a kind outside the vocabulary (the caller
// CLAIMED a ref with urn:dhnt: or dhnt: and misspelled the kind), and a store
// that could not be read (its error, verbatim).
func defineRef(out io.Writer, term string, asJSON bool) error {
	r, err := ref.Parse(term)
	switch {
	case errors.Is(err, ref.ErrNotRef):
		return errNotARef
	case errors.Is(err, ref.ErrUnknownKind):
		return fmt.Errorf("%s: %s is not a kind; kinds: %s",
			strings.TrimSpace(term), unknownKindName(r), strings.Join(ref.KindNames(), " "))
	case errors.Is(err, ref.ErrEmptyID):
		return fmt.Errorf("%s: a ref needs an id after the kind — <kind>:<id>, as in %s:<id> "+
			"(`define --list-kinds` shows the kinds)", strings.TrimSpace(term), r.Kind)
	case err != nil:
		return err
	}

	n, err := resolveRef(RefResolvers, r)
	switch {
	case errors.Is(err, ref.ErrNoResolver):
		return fmt.Errorf("kind %s: no resolver wired on this build", r.Kind)
	case errors.Is(err, ref.ErrNotFound):
		return fmt.Errorf("%s: no %s with id %s here\n  %s", r, r.Kind, r.ID, refListHint(r.Kind, n))
	case err != nil:
		return err
	}

	if asJSON {
		b, _ := json.MarshalIndent(n, "", "  ")
		fmt.Fprintln(out, string(b))
		return nil
	}
	writeNode(out, n)
	return nil
}

// resolveRef is ref.Registry.Resolve for an already-parsed ref, keeping the
// node a resolver hands back BESIDE ErrNotFound: the registry discards it, and
// it is where a store can carry its listing verb (Open) for the hint.
func resolveRef(g *ref.Registry, r ref.Ref) (ref.Node, error) {
	res, ok := g.Lookup(r.Kind)
	if !ok {
		return ref.Node{}, fmt.Errorf("%w: %s", ref.ErrNoResolver, r.Kind)
	}
	n, err := res.Resolve(r.ID)
	if err != nil {
		return n, err
	}
	if n.Ref == "" {
		n.Ref = ref.Format(n.Kind, n.ID)
	}
	return n, nil
}

// refListHint is the way out of "not found": the store's listing verb. It is
// the node's Open with the id taken off the end — a resolver that fills Open
// on its ErrNotFound answer gets a precise hint; one that does not gets the
// generic pointer, never a verb this package would have to guess at.
func refListHint(k ref.Kind, n ref.Node) string {
	if open := strings.TrimSpace(n.Open); open != "" {
		if i := strings.LastIndex(open, " "); i > 0 {
			open = open[:i]
		}
		return fmt.Sprintf("`%s` without an id lists the %s records here", open, k)
	}
	return fmt.Sprintf("the %s store's list verb shows what is here (`define --list-kinds` names the kinds)", k)
}

// unknownKindName recovers the misspelled kind from what Parse filed under
// ref.Unknown: the text after the urn:dhnt: prefix (`<x>:<rest>`) or after the
// legacy dhnt: prefix (`<x>/<rest>`).
func unknownKindName(r ref.Ref) string {
	x := r.ID
	if i := strings.IndexAny(x, ":/"); i >= 0 {
		x = x[:i]
	}
	return strings.TrimSpace(x)
}

// writeNode renders one resolved ref: the ref and its title on the first line,
// then the store's status, where it lives, how to open it, and — when it was
// superseded — the ref that replaced it. One printer for every kind, which is
// the point of ref.Node carrying nothing store-specific.
func writeNode(out io.Writer, n ref.Node) {
	if n.Title != "" {
		fmt.Fprintf(out, "%s  %s\n", n.Ref, n.Title)
	} else {
		fmt.Fprintln(out, n.Ref)
	}
	if n.Status != "" {
		fmt.Fprintf(out, "  status: %s\n", n.Status)
	}
	// The entity's other handles (ref.Shape): the uuid it is known by across
	// hosts, and the running number a human types — shown, never made the ref.
	if n.UID != "" && n.UID != n.ID {
		fmt.Fprintf(out, "  uid:    %s\n", n.UID)
	}
	if n.Seq != 0 {
		fmt.Fprintf(out, "  seq:    #%d\n", n.Seq)
	}
	if n.Where != "" {
		fmt.Fprintf(out, "  where:  %s\n", n.Where)
	}
	if n.Open != "" {
		fmt.Fprintf(out, "  open:   %s\n", n.Open)
	}
	if n.Successor != "" {
		fmt.Fprintf(out, "  successor: %s\n", n.Successor)
	}
}

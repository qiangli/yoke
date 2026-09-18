// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package advice

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/qiangli/yoke/pkg/atlas"
)

// Cap is a guard's effect cap: the set of atlas-11 effect atoms a guarded call
// is allowed to exercise.
//
// Caps only ever narrow. WithCap intersects with any cap already on the
// context, so a nested guard cannot widen what an outer guard allowed —
// deny-only, by construction.
type Cap struct {
	set map[string]bool
}

// capVocabulary is the atlas-11 effect vocabulary.
var capVocabulary = func() map[string]bool {
	m := make(map[string]bool, len(atlas.Effects()))
	for _, e := range atlas.Effects() {
		m[e] = true
	}
	return m
}()

// ParseCap parses a comma-separated effect list ("read,net") into a Cap,
// rejecting empty lists and atoms outside the atlas-11 vocabulary.
func ParseCap(spec string) (Cap, error) {
	set := map[string]bool{}
	for tok := range strings.SplitSeq(spec, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		if !capVocabulary[tok] {
			return Cap{}, fmt.Errorf("unknown effect %q (effects: %s)", tok, strings.Join(vocabularySorted(), ", "))
		}
		set[tok] = true
	}
	if len(set) == 0 {
		return Cap{}, fmt.Errorf("empty effect cap (effects: %s)", strings.Join(vocabularySorted(), ", "))
	}
	return Cap{set: set}, nil
}

func vocabularySorted() []string {
	out := make([]string, 0, len(capVocabulary))
	for k := range capVocabulary {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Effects returns the cap's atoms, sorted.
func (c Cap) Effects() []string {
	out := make([]string, 0, len(c.set))
	for k := range c.set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// String renders the cap as its canonical comma-joined sorted spelling.
func (c Cap) String() string { return strings.Join(c.Effects(), ",") }

// Intersect returns the atoms in both caps — the effective cap when a guard
// nests inside another. The zero Cap (no atoms) is absorbing: nothing
// projected is allowed under it.
func (c Cap) Intersect(o Cap) Cap {
	set := map[string]bool{}
	for k := range c.set {
		if o.set[k] {
			set[k] = true
		}
	}
	return Cap{set: set}
}

// Exceeded compares unprojected effects and returns the
// atoms the cap does not allow, sorted and deduplicated; empty means the effects
// fit the cap. Pure is ignored as intrinsically no governed side effect.
// Unknown atoms deny, and an empty/missing command effect declaration denies as unknown.
func (c Cap) Exceeded(atlasEffects []string) []string {
	if len(atlasEffects) == 0 {
		return []string{"unknown"}
	}

	denySet := map[string]bool{}
	for _, e := range atlasEffects {
		if e == atlas.EffPure {
			continue
		}
		if !capVocabulary[e] {
			denySet["unknown"] = true
		} else if !c.set[e] {
			denySet[e] = true
		}
	}

	if len(denySet) == 0 {
		return nil
	}
	out := make([]string, 0, len(denySet))
	for k := range denySet {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// capKey carries the effective cap on a context. Unexported: the cap rides
// on the ctx handed to Next() via WithCap/CapFrom only, so no handler-context
// field is added anywhere (design non-goal).
type capKey struct{}

// WithCap returns a context carrying c as the effective cap. If ctx already
// carries a cap, the result carries the INTERSECTION — a nested guard can
// only narrow, never widen, what an outer guard allowed.
func WithCap(ctx context.Context, c Cap) context.Context {
	if outer, ok := CapFrom(ctx); ok {
		c = outer.Intersect(c)
	}
	return context.WithValue(ctx, capKey{}, c)
}

// CapFrom returns the effective cap on ctx, if any.
func CapFrom(ctx context.Context) (Cap, bool) {
	c, ok := ctx.Value(capKey{}).(Cap)
	return c, ok
}

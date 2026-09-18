// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package advice

import (
	"context"
	"reflect"
	"testing"

	"github.com/qiangli/yoke/pkg/atlas"
)

func mustCap(t *testing.T, spec string) Cap {
	t.Helper()
	c, err := ParseCap(spec)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestParseCap(t *testing.T) {
	c := mustCap(t, " net , read ,net")
	if got := c.String(); got != "net,read" {
		t.Fatalf("cap = %q, want canonical sorted dedup", got)
	}
	for _, bad := range []string{"", " , ", "read,time", "invalid", "READ"} {
		if _, err := ParseCap(bad); err == nil {
			t.Fatalf("ParseCap(%q) accepted", bad)
		}
	}
	// Pure is accepted as an atlas-11 constant
	mustCap(t, "pure")
}

// The cap vocabulary is exactly the atlas-11 constants.
func TestCapVocabularyMatchesAtlas(t *testing.T) {
	if len(capVocabulary) != len(atlas.Effects()) {
		t.Fatalf("vocabulary size = %d, want %d", len(capVocabulary), len(atlas.Effects()))
	}
	for _, e := range atlas.Effects() {
		if !capVocabulary[e] {
			t.Fatalf("atlas effect %q is not a cap atom", e)
		}
	}
}

func TestExceeded(t *testing.T) {
	c := mustCap(t, "read")
	cases := []struct {
		name    string
		effects []string
		want    []string
	}{
		{"within cap", []string{atlas.EffRead}, nil},
		{"pure is ignored and allows under any cap", []string{atlas.EffRead, atlas.EffPure}, nil},
		{"pure alone passes", []string{atlas.EffPure}, nil},
		{"write exceeds", []string{atlas.EffWrite, atlas.EffRead}, []string{"write"}},
		{"exec, cred, priv, persist, remote deny under read", []string{atlas.EffExec, atlas.EffCred, atlas.EffPriv, atlas.EffPersist, atlas.EffRemote, atlas.EffRead}, []string{"cred", "exec", "persist", "priv", "remote"}},
		{"unknown atoms deny", []string{atlas.EffRead, "madeup"}, []string{"unknown"}},
		{"empty missing effects denies as unknown", []string{}, []string{"unknown"}},
		{"nil missing effects denies as unknown", nil, []string{"unknown"}},
		{"deterministic dedup", []string{atlas.EffWrite, atlas.EffWrite, atlas.EffNet}, []string{"net", "write"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := c.Exceeded(tc.effects); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Exceeded(%v) = %v, want %v", tc.effects, got, tc.want)
			}
		})
	}
}

// A nested guard intersects with the outer cap and can never widen it.
func TestNestedCapsNeverWiden(t *testing.T) {
	ctx := WithCap(context.Background(), mustCap(t, "read"))
	outer, ok := CapFrom(ctx)
	if !ok || outer.String() != "read" {
		t.Fatalf("outer cap = %v, %t", outer, ok)
	}
	// Inner guard asks for MORE than the outer allows: effective stays "read".
	inner := WithCap(ctx, mustCap(t, "read,net,write"))
	eff, ok := CapFrom(inner)
	if !ok || eff.String() != "read" {
		t.Fatalf("widening attempt: effective = %q, want read", eff)
	}
	if got := eff.Exceeded([]string{atlas.EffNet}); !reflect.DeepEqual(got, []string{"net"}) {
		t.Fatalf("net allowed through a widened nested cap: %v", got)
	}
	// Inner guard narrows: effective is the intersection.
	narrowed := WithCap(ctx, mustCap(t, "net"))
	eff, _ = CapFrom(narrowed)
	if eff.String() != "" {
		t.Fatalf("disjoint intersection = %q, want empty", eff)
	}
	if got := eff.Exceeded([]string{atlas.EffRead}); !reflect.DeepEqual(got, []string{"read"}) {
		t.Fatalf("empty cap allowed read: %v", got)
	}
	// The outer context is untouched — a child's cap never leaks upward.
	outer, _ = CapFrom(ctx)
	if outer.String() != "read" {
		t.Fatalf("outer cap mutated to %q", outer)
	}
	// No cap on a fresh context.
	if _, ok := CapFrom(context.Background()); ok {
		t.Fatal("fresh context reported a cap")
	}
}

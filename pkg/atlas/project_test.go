// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package atlas

import (
	"reflect"
	"testing"
)

// The projection is a table, and the table is the contract: every atlas atom
// either lands on exactly one dhnt atom or is dropped, and nothing else.
func TestProjectEffects(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"read carries", []string{EffRead}, []string{"read"}},
		{"write carries", []string{EffWrite}, []string{"write"}},
		{"destroy carries", []string{EffDestroy}, []string{"destroy"}},
		{"net carries", []string{EffNet}, []string{"net"}},
		{"spend carries", []string{EffSpend}, []string{"spend"}},
		{"remote folds into net", []string{EffRemote}, []string{"net"}},
		{"net+remote dedupe", []string{EffNet, EffRemote}, []string{"net"}},
		{"exec dropped", []string{EffExec}, nil},
		{"cred dropped", []string{EffCred}, nil},
		{"priv dropped", []string{EffPriv}, nil},
		{"persist dropped", []string{EffPersist}, nil},
		{"pure dropped", []string{EffPure}, nil},
		{"unknown atom dropped, no error", []string{"teleport", EffRead}, []string{"read"}},
		{"sorted", []string{EffWrite, EffRead, EffDestroy}, []string{"destroy", "read", "write"}},
		{"duplicates collapse", []string{EffRead, EffRead, EffWrite, EffWrite}, []string{"read", "write"}},
		{"registry shape", RegistryEntry(4).Effects, []string{"net"}},
		{"empty", nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ProjectEffects(tc.in); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ProjectEffects(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// Every atom in the closed atlas vocabulary is accounted for by the table —
// either mapped or in the documented drop set — so adding an atlas effect
// without deciding its projection fails here rather than silently dropping.
func TestProjectEffects_CoversVocabulary(t *testing.T) {
	dropped := map[string]bool{EffExec: true, EffCred: true, EffPriv: true, EffPersist: true, EffPure: true}
	for _, e := range Effects() {
		_, mapped := effectProjection[e]
		if mapped == dropped[e] {
			t.Errorf("effect %q: mapped=%v dropped=%v — must be exactly one", e, mapped, dropped[e])
		}
	}
	// The projection never invents an atom outside the dhnt-6 lattice.
	lattice := map[string]bool{"read": true, "write": true, "net": true, "spend": true, "destroy": true, "time": true}
	for a, d := range effectProjection {
		if !lattice[d] {
			t.Errorf("effect %q projects to %q, outside the dhnt-6 lattice", a, d)
		}
	}
}

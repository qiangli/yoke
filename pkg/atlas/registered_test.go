// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package atlas_test

import (
	"slices"
	"testing"

	"github.com/qiangli/yoke/pkg/atlas"
)

// A registered command's atlas entry is DERIVED, never hand-listed, and it
// must sit inside every closed vocabulary so the views need no special case.
func TestRegisteredEntryDefaultsAndVocabulary(t *testing.T) {
	for _, kind := range atlas.RegisteredKinds() {
		e := atlas.RegisteredEntry(atlas.RegisteredSpec{Kind: kind, Effects: []string{atlas.EffRead}})
		if e.Origin != atlas.OriginRegistered || e.Subclass != atlas.SubclassRegistered {
			t.Errorf("%s: origin/subclass = %q/%q", kind, e.Origin, e.Subclass)
		}
		if e.Group != atlas.GroupShellutils || e.Tier != atlas.TierUserland || e.Stage != atlas.StageCross {
			t.Errorf("%s: defaults = %q/%q/%q", kind, e.Group, e.Tier, e.Stage)
		}
		if e.Shape != atlas.ShapeResult {
			t.Errorf("%s: shape default = %q, want result", kind, e.Shape)
		}
		if !slices.Equal(e.OS, atlas.OSes()) || !e.Portable() {
			t.Errorf("%s: OS default = %v", kind, e.OS)
		}
		if !slices.Contains(atlas.Groups(), e.Group) || !slices.Contains(atlas.Tiers(), e.Tier) || !slices.Contains(atlas.Stages(), e.Stage) {
			t.Errorf("%s: outside the closed vocabularies", kind)
		}
		for _, c := range e.Caps {
			if !slices.Contains(atlas.Capabilities(), c) {
				t.Errorf("%s: cap %q not in vocabulary", kind, c)
			}
		}
		if !slices.IsSorted(e.Caps) || !slices.IsSorted(e.Effects) {
			t.Errorf("%s: caps/effects not sorted: %v %v", kind, e.Caps, e.Effects)
		}
	}
}

// The mechanism adds only what it knows for certain, and never touches the
// author's effects: an exec spawns a process; a download is cached, needs the
// network and self-provisions; a script adds nothing.
func TestRegisteredEntryMechanismCaps(t *testing.T) {
	exec := atlas.RegisteredEntry(atlas.RegisteredSpec{Kind: atlas.RegisteredExec, Effects: []string{atlas.EffExec}})
	if !slices.Contains(exec.Caps, atlas.CapSpawnsProcesses) || slices.Contains(exec.Caps, atlas.CapCached) {
		t.Errorf("exec caps = %v", exec.Caps)
	}
	dl := atlas.RegisteredEntry(atlas.RegisteredSpec{Kind: atlas.RegisteredDownload, Effects: []string{atlas.EffNet, atlas.EffExec}})
	for _, c := range []string{atlas.CapCached, atlas.CapNeedsNetwork, atlas.CapSelfProvisioning, atlas.CapSpawnsProcesses} {
		if !slices.Contains(dl.Caps, c) {
			t.Errorf("download caps = %v, missing %s", dl.Caps, c)
		}
	}
	if !slices.Equal(dl.Effects, []string{atlas.EffExec, atlas.EffNet}) {
		t.Errorf("download effects = %v (must be the author's, sorted)", dl.Effects)
	}
	sc := atlas.RegisteredEntry(atlas.RegisteredSpec{Kind: atlas.RegisteredScript, Effects: []string{atlas.EffWrite}, Caps: []string{atlas.CapJSON, atlas.CapJSON}})
	if !slices.Equal(sc.Caps, []string{atlas.CapJSON}) {
		t.Errorf("script caps = %v, want the author's, deduped", sc.Caps)
	}
}

// registered is an exclusive origin beside the five table origins, with a
// label, and no table entry may ever carry it.
func TestRegisteredOriginIsDerivedOnly(t *testing.T) {
	if !slices.Contains(atlas.Origins(), atlas.OriginRegistered) {
		t.Fatal("Origins() must list registered")
	}
	if atlas.OriginLabel(atlas.OriginRegistered) == atlas.OriginRegistered {
		t.Fatal("registered has no label")
	}
	for _, n := range append(atlas.ToolNames(), atlas.VerbNames()...) {
		if e, _ := atlas.Lookup(n); e.Origin == atlas.OriginRegistered {
			t.Errorf("%s: a table entry claims origin registered", n)
		}
	}
}

// commands is the CRUD front door of the ring: rm destroys, add/set write,
// verify on a download provisions over the net.
func TestCommandsVerbDeclaresRingEffects(t *testing.T) {
	e, ok := atlas.Lookup("commands")
	if !ok {
		t.Fatal("no commands entry")
	}
	for _, want := range []string{atlas.EffRead, atlas.EffWrite, atlas.EffDestroy, atlas.EffNet} {
		if !slices.Contains(e.Effects, want) {
			t.Errorf("commands effects = %v, missing %s", e.Effects, want)
		}
	}
	if !slices.Contains(e.Caps, atlas.CapDestructive) || slices.Contains(e.Caps, atlas.CapReadOnly) {
		t.Errorf("commands caps = %v", e.Caps)
	}
}

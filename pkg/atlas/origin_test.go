// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package atlas_test

import (
	"bufio"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/atlas"
)

// TestEveryEntryHasOrigin: provenance is mandatory, like the security effect.
// An entry with no origin is unclassified, and unclassified must fail the
// build rather than render as "unknown" in a listing an agent reads.
func TestEveryEntryHasOrigin(t *testing.T) {
	origins := sliceSet(atlas.Origins())
	posix := sliceSet(atlas.PosixRequired())
	for _, n := range append(atlas.ToolNames(), atlas.VerbNames()...) {
		e, _ := atlas.Lookup(n)
		if !origins[e.Origin] {
			t.Errorf("%s: origin %q not in vocabulary %v", n, e.Origin, atlas.Origins())
		}
		if e.Posix != posix[n] {
			t.Errorf("%s: Posix=%v but PosixRequired says %v", n, e.Posix, posix[n])
		}
		if e.Origin == atlas.OriginBash {
			t.Errorf("%s: OriginBash is the embedder's to stamp; no table entry may claim it", n)
		}
	}
}

// TestOriginFollowsSubclass: "external" means exec'd, and the only thing that
// says a command is exec'd is its Subclass. The two must never disagree.
func TestOriginFollowsSubclass(t *testing.T) {
	for _, n := range append(atlas.ToolNames(), atlas.VerbNames()...) {
		e, _ := atlas.Lookup(n)
		exec := e.Subclass == atlas.SubclassManagedExternal || e.Subclass == atlas.SubclassProvisioner
		if exec != (e.Origin == atlas.OriginExternal) {
			t.Errorf("%s: subclass %q but origin %q", n, e.Subclass, e.Origin)
		}
	}
	// A verb bashy did not exec is a verb bashy wrote.
	for _, n := range atlas.VerbNames() {
		e, _ := atlas.Lookup(n)
		if e.Origin != atlas.OriginExternal && e.Origin != atlas.OriginBashy {
			t.Errorf("verb %s: origin %q (verbs are external or bashy)", n, e.Origin)
		}
	}
}

// TestGNUOriginMatchesUpstream: OriginGNU is exactly "in the GNU coreutils
// inventory and implemented here". The inventory is 108; three are missing.
func TestGNUOriginMatchesUpstream(t *testing.T) {
	up := atlas.GNUCoreutilsUpstream()
	if !sort.StringsAreSorted(up) {
		t.Errorf("GNUCoreutilsUpstream not sorted")
	}
	if len(up) != 108 {
		t.Errorf("GNUCoreutilsUpstream: %d names, want 108 (coreutils 9.x)", len(up))
	}
	upSet := sliceSet(up)
	var missing []string
	for _, n := range up {
		if _, ok := atlas.Lookup(n); !ok {
			missing = append(missing, n)
		}
	}
	if want := []string{"chroot", "coreutils", "runcon"}; strings.Join(missing, " ") != strings.Join(want, " ") {
		t.Errorf("GNU names with no implementation: %v, want %v", missing, want)
	}
	for _, n := range atlas.ToolNames() {
		e, _ := atlas.Lookup(n)
		if upSet[n] && e.Origin != atlas.OriginGNU && e.Origin != atlas.OriginExternal {
			t.Errorf("%s: in the GNU inventory but origin %q", n, e.Origin)
		}
		if !upSet[n] && e.Origin == atlas.OriginGNU && e.AliasOf == "" {
			t.Errorf("%s: origin gnu but not in the GNU inventory", n)
		}
	}
}

// TestPosixRequiredMatchesManifest ratchets the embedded list against the
// generator's input, docs/posix-required-commands.tsv — the same file
// cmds/posixgate's spec is generated from. Every row the manifest assigns to
// a Go applet or a pinned provider must be an atlas tool; shell-owned rows
// (cd, alias, …) are the embedder's builtins and appear in no table.
func TestPosixRequiredMatchesManifest(t *testing.T) {
	// The manifest lives with the certified package (coreutils/docs); this
	// module is its flat sibling, in the umbrella and in a standalone clone.
	f, err := os.Open("../../../coreutils/docs/posix-required-commands.tsv")
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	defer f.Close()
	var names []string
	byDisposition := map[string][]string{}
	sc := bufio.NewScanner(f)
	for line := 0; sc.Scan(); line++ {
		if line == 0 {
			continue // header
		}
		cols := strings.Split(sc.Text(), "\t")
		if len(cols) < 5 || cols[0] == "" {
			continue
		}
		names = append(names, cols[0])
		byDisposition[cols[4]] = append(byDisposition[cols[4]], cols[0])
	}
	sort.Strings(names)
	got := atlas.PosixRequired()
	if !sort.StringsAreSorted(got) {
		t.Errorf("PosixRequired not sorted")
	}
	if len(got) != 116 || strings.Join(got, " ") != strings.Join(names, " ") {
		t.Errorf("PosixRequired (%d) drifted from docs/posix-required-commands.tsv (%d): regenerate origin.go's list", len(got), len(names))
	}
	for _, disp := range []string{"go_applet", "external_provider"} {
		for _, n := range byDisposition[disp] {
			e, ok := atlas.Lookup(n)
			if !ok {
				t.Errorf("%s: manifest disposition %s but no atlas entry", n, disp)
				continue
			}
			if !e.Posix {
				t.Errorf("%s: manifest-required but Posix=false", n)
			}
			if disp == "external_provider" && e.Origin != atlas.OriginExternal {
				t.Errorf("%s: pinned provider but origin %q", n, e.Origin)
			}
		}
	}
	for _, n := range byDisposition["shell"] {
		if !atlas.IsPosixRequired(n) {
			t.Errorf("%s: shell-owned POSIX name missing from PosixRequired", n)
		}
	}
}

// TestOriginCountsPinned: the tool table's origin split is stable data, so a
// reclassification (a tool quietly becoming "bashy", a GNU name slipping to
// "unix") shows up as a diff here instead of in a listing nobody rereads.
// Verbs are not pinned by count — they are pinned structurally above.
func TestOriginCountsPinned(t *testing.T) {
	got := map[string]int{}
	for _, n := range atlas.ToolNames() {
		e, _ := atlas.Lookup(n)
		got[e.Origin]++
	}
	want := map[string]int{
		atlas.OriginGNU:      106, // 105 upstream names + `[`, the alias of test
		atlas.OriginUnix:     50,  // S216 #536: + cygpath, wslpath (classic interop tools, not GNU)
		atlas.OriginExternal: 12,
		atlas.OriginBashy:    12,
	}
	for o, n := range want {
		if got[o] != n {
			t.Errorf("tool origin %s: %d entries, want %d (update the pin with the reclassification, not silently)", o, got[o], n)
		}
	}
}

// TestAliasesInheritOrigin: an alias is a second spelling, so it can have no
// provenance of its own.
func TestAliasesInheritOrigin(t *testing.T) {
	for _, n := range append(atlas.ToolNames(), atlas.VerbNames()...) {
		e, _ := atlas.Lookup(n)
		if e.AliasOf == "" {
			continue
		}
		target, ok := atlas.Lookup(e.AliasOf)
		if !ok {
			continue // TestAliasTargetsExist reports it
		}
		if e.Origin != target.Origin {
			t.Errorf("%s: origin %q but its target %s is %q", n, e.Origin, e.AliasOf, target.Origin)
		}
	}
}

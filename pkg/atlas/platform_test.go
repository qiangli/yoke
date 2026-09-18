// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package atlas_test

import (
	"sort"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/atlas"
)

// TestEveryEntryHasPlatforms: support is declared, not assumed; a partial
// platform must be a supported one; aliases inherit.
func TestEveryEntryHasPlatforms(t *testing.T) {
	vocab := sliceSet(atlas.OSes())
	for _, n := range append(atlas.ToolNames(), atlas.VerbNames()...) {
		e, _ := atlas.Lookup(n)
		if len(e.OS) == 0 {
			t.Errorf("%s: no platform support declared", n)
		}
		for _, o := range e.OS {
			if !vocab[o] {
				t.Errorf("%s: os %q not in vocabulary", n, o)
			}
		}
		for _, o := range e.Partial {
			if !e.SupportedOn(o) {
				t.Errorf("%s: partial on %q but not supported there", n, o)
			}
		}
		if e.AliasOf != "" {
			if tgt, ok := atlas.Lookup(e.AliasOf); ok && strings.Join(tgt.OS, ",") != strings.Join(e.OS, ",") {
				t.Errorf("%s: os %v differs from its target %s %v", n, e.OS, e.AliasOf, tgt.OS)
			}
		}
	}
}

// TestPlatformPinsFromStubEvidence pins the curated lists to what the stubs
// say, so a reclassification is a reviewed diff, not a quiet drift.
func TestPlatformPinsFromStubEvidence(t *testing.T) {
	want := map[string][]string{
		"ps":              {"linux"},
		"chcon":           {"linux"},
		"chown":           {"darwin", "linux"},
		"mkfifo":          {"darwin", "linux"},
		"m4":              {"darwin", "linux"},
		"posix-providers": {"darwin", "linux"},
		"ollama":          {"darwin", "linux"},
		"cat":             {"darwin", "linux", "windows"},
		"weave":           {"darwin", "linux", "windows"},
		"oci":             {"darwin", "linux", "windows"},
	}
	for n, oses := range want {
		e, ok := atlas.Lookup(n)
		if !ok {
			t.Errorf("%s: missing", n)
			continue
		}
		got := append([]string(nil), e.OS...)
		sort.Strings(got)
		if strings.Join(got, ",") != strings.Join(oses, ",") {
			t.Errorf("%s: os %v, want %v", n, got, oses)
		}
	}
	for n, os := range map[string]string{"more": "windows", "find": "windows", "renice": "darwin"} {
		e, _ := atlas.Lookup(n)
		if !e.PartialOn(os) {
			t.Errorf("%s: should be partial on %s", n, os)
		}
		if e.Portable() {
			t.Errorf("%s: partial on %s, cannot be portable", n, os)
		}
	}
	for _, n := range []string{"cat", "grep", "weave", "kb", "oci", "jq"} {
		e, _ := atlas.Lookup(n)
		if !e.Portable() {
			t.Errorf("%s: should be portable (os %v, partial %v)", n, e.OS, e.Partial)
		}
	}
	// Counts pinned: the non-portable set is small and every member has a
	// stub citation in platform.go.
	unsupported, partial := 0, 0
	for _, n := range atlas.ToolNames() {
		e, _ := atlas.Lookup(n)
		if len(e.OS) < 3 {
			unsupported++
		} else if len(e.Partial) > 0 {
			partial++
		}
	}
	if unsupported != 24 || partial != 17 {
		t.Errorf("tool table: %d not-everywhere + %d partial, want 24 + 17 (update the pin with the evidence)", unsupported, partial)
	}
}

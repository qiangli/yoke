// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

// The release stability tier (bashy docs/release-roadmap-and-versioning.md,
// §Stability tiers) is the five-point bar's point (1): every Yoke verb on the
// v1.0.0 list carries a DECLARED tier, reported by `bashy commands --atlas`.
// The tier is curated here, never inferred; a verb added without a stab() line
// is unclassified, and unclassified fails the ratchet instead of reading as a
// tier nobody declared.
package atlas_test

import (
	"testing"

	"github.com/qiangli/yoke/pkg/atlas"
)

// Stabilities is the closed ladder, in graduation order.
func TestStabilityVocabulary(t *testing.T) {
	want := []string{atlas.StabilityExperimental, atlas.StabilityPreview, atlas.StabilitySupported}
	got := atlas.Stabilities()
	if len(got) != len(want) {
		t.Fatalf("Stabilities() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Stabilities()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	for i, s := range []string{"experimental", "preview", "supported"} {
		if got[i] != s {
			t.Errorf("tier %d spelled %q, want the roadmap spelling %q", i, got[i], s)
		}
	}
}

// Every VERB row is classified exactly once, by hand. Tool rows (the
// certified coreutils package) are outside the Yoke tier ladder and stay
// unclassified unless a front door declares one (graph).
func TestStabilityCoverageOnVerbs(t *testing.T) {
	vocab := sliceSet(atlas.Stabilities())
	for _, n := range atlas.VerbNames() {
		e, ok := atlas.Lookup(n)
		if !ok {
			t.Fatalf("Lookup(%q) missing for listed verb", n)
		}
		if e.Stability == "" {
			t.Errorf("%s: no stability tier (declare it in pkg/atlas via stab())", n)
			continue
		}
		if !vocab[e.Stability] {
			t.Errorf("%s: stability %q not in vocabulary %v", n, e.Stability, atlas.Stabilities())
		}
	}
	for _, n := range atlas.ToolNames() {
		e, _ := atlas.Lookup(n)
		if e.Stability != "" && !vocab[e.Stability] {
			t.Errorf("%s: stability %q not in vocabulary %v", n, e.Stability, atlas.Stabilities())
		}
	}
}

// An alias is a spelling: it reports its target's tier, never one of its own.
func TestStabilityAliasFollowsTarget(t *testing.T) {
	for _, n := range atlas.VerbNames() {
		e, _ := atlas.Lookup(n)
		if e.AliasOf == "" {
			continue
		}
		target, ok := atlas.Lookup(e.AliasOf)
		if !ok {
			t.Fatalf("%s: alias target %q missing", n, e.AliasOf)
		}
		if e.Stability != target.Stability {
			t.Errorf("%s stability = %q, but its target %s is %q", n, e.Stability, e.AliasOf, target.Stability)
		}
	}
}

// The v1.0.0 declarations, pinned so a tier change is a deliberate edit here.
// preview is declared only where bashy's release bar already records BOTH a
// named consumer AND a versioned JSON envelope probe for the verb, and the
// bashy atlas does not mark it experimental (curated-hidden). Everything else
// enters at experimental — the roadmap's entry tier. supported is not
// assigned to any verb before the post-1.0 foundation gate.
func TestStabilitySpotTiers(t *testing.T) {
	want := map[string]string{
		"sprint":        atlas.StabilityPreview, // embedded bashy skill + loom-v2
		"todo":          atlas.StabilityPreview,
		"weave":         atlas.StabilityPreview,
		"dag":           atlas.StabilityPreview,
		"mb":            atlas.StabilityPreview,
		"meet":          atlas.StabilityPreview,
		"inbox":         atlas.StabilityPreview,
		"whois":         atlas.StabilityPreview,
		"agent":         atlas.StabilityPreview, // conductor skill + bashy-fleet-list-v1
		"agents":        atlas.StabilityPreview, // alias of agent
		"inspect":       atlas.StabilityPreview,
		"commands":      atlas.StabilityPreview,
		"ollama":        atlas.StabilityPreview, // llm model door + bashy-ollama-status-v1
		"install-agent": atlas.StabilityPreview,
		"skill":         atlas.StabilityPreview,
		"genie":         atlas.StabilityPreview, // roadmap: the reference agent ships at preview
		"context":       atlas.StabilityPreview, // alias of inspect (bashy-context-v1 is the same envelope)
		"audit":         atlas.StabilityPreview, // alias of inspect in this atlas: see the Story 1533 report
		// curated-hidden in bashy (status: experimental) — never promoted here
		"supervise": atlas.StabilityExperimental,
		"run":       atlas.StabilityExperimental,
		"check":     atlas.StabilityExperimental,
		"conform":   atlas.StabilityExperimental,
		"mcp":       atlas.StabilityExperimental,
		// envelope or consumer still missing on the bar
		"llm":     atlas.StabilityExperimental,
		"chat":    atlas.StabilityExperimental,
		"model":   atlas.StabilityExperimental,
		"models":  atlas.StabilityExperimental,
		"tool":    atlas.StabilityExperimental,
		"tools":   atlas.StabilityExperimental,
		"verify":  atlas.StabilityExperimental,
		"kb":      atlas.StabilityExperimental,
		"graph":   atlas.StabilityExperimental, // tool row, bashy front door
		"craft":   atlas.StabilityExperimental,
		"app":     atlas.StabilityExperimental,
		"ycode":   atlas.StabilityExperimental,
		"ping":    atlas.StabilityExperimental,
		"bus":     atlas.StabilityExperimental,
		"notify":  atlas.StabilityExperimental,
		"ask":     atlas.StabilityExperimental,
		"oci":     atlas.StabilityExperimental,
		"sandbox": atlas.StabilityExperimental,
		"loom":    atlas.StabilityExperimental,
	}
	for name, tier := range want {
		e, ok := atlas.Lookup(name)
		if !ok {
			t.Fatalf("%s is absent from atlas", name)
		}
		if e.Stability != tier {
			t.Errorf("%s stability = %q, want %q", name, e.Stability, tier)
		}
	}
	for _, n := range atlas.VerbNames() {
		if e, _ := atlas.Lookup(n); e.Stability == atlas.StabilitySupported {
			t.Errorf("%s declares supported; nothing graduates to supported before the post-1.0 foundation gate", n)
		}
	}
}

// Derived entries (registry CLIs, operator-registered commands) are not
// curated rows: they carry no validated tier and take the entry tier,
// experimental, never anything a reader could mistake for a promise.
func TestStabilityDerivedEntries(t *testing.T) {
	if got := atlas.RegistryEntry(6).Stability; got != atlas.StabilityExperimental {
		t.Errorf("RegistryEntry(6) stability = %q, want %q", got, atlas.StabilityExperimental)
	}
	if got := atlas.RegisteredEntry(atlas.RegisteredSpec{}).Stability; got != atlas.StabilityExperimental {
		t.Errorf("RegisteredEntry(empty) stability = %q, want %q", got, atlas.StabilityExperimental)
	}
}

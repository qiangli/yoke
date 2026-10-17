// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package atlas_test

// The tier axis after Sprint 449 (dhnt docs/execution-tiers.md): a tier names a
// command's PRIMARY PURPOSE, tier 2 is `workplace` (agent work and
// collaboration), and `workspace` is not a tier spelling at all — it keeps its
// filesystem meaning (a weave clone). These tests pin the vocabulary, the
// required workplace membership, alias inheritance and recipe agreement; the
// full purpose review is docs/command-tier-classification.md.

import (
	"reflect"
	"slices"
	"testing"

	"github.com/qiangli/yoke/pkg/atlas"
)

func TestTiersAreExactlyTheSixInStackOrder(t *testing.T) {
	want := []string{"userland", "workplace", "sandbox", "sphere", "cluster", "cloud"}
	if got := atlas.Tiers(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Tiers() = %v, want exactly %v", got, want)
	}
	if atlas.TierWorkplace != "workplace" {
		t.Errorf("TierWorkplace = %q, want %q", atlas.TierWorkplace, "workplace")
	}
	// No `workspace` alias, by operator decision: the word is the weave clone.
	if slices.Contains(atlas.Tiers(), "workspace") {
		t.Error("Tiers() still lists \"workspace\"; the tier is workplace and workspace is not an alias")
	}
	// account is the front door beside the stack, never a seventh tier.
	if slices.Contains(atlas.Tiers(), atlas.TierAccount) {
		t.Errorf("Tiers() lists %q; the account front door is represented separately", atlas.TierAccount)
	}
	if got := atlas.TierName(2); got != atlas.TierWorkplace {
		t.Errorf("TierName(2) = %q, want %q", got, atlas.TierWorkplace)
	}
}

// The required workplace members (Sprint 449 story ca04fac71ca3), plus the
// purpose-over-package spot checks: general utilities and toolchains stay
// userland whatever package implements them, and the dedicated OCI, peer, DKS
// and provider operations keep their tiers.
func TestWorkplaceMembership(t *testing.T) {
	for _, n := range []string{"weave", "sprint", "loom", "dag", "kb", "todo", "meet", "inbox"} {
		e, ok := atlas.Lookup(n)
		if !ok {
			t.Fatalf("%s: not in the atlas", n)
		}
		if e.Tier != atlas.TierWorkplace {
			t.Errorf("%s: tier %q, want %q", n, e.Tier, atlas.TierWorkplace)
		}
	}
	spot := map[string]string{
		// general utilities / toolchains, by purpose
		"mod": atlas.TierUserland, "git": atlas.TierUserland, "gh": atlas.TierUserland,
		"go": atlas.TierUserland, "rclone": atlas.TierUserland, "search": atlas.TierUserland,
		"grep": atlas.TierUserland, "why": atlas.TierUserland,
		// dedicated operations keep their tiers
		"oci": atlas.TierSandbox, "sandbox": atlas.TierSandbox, "act": atlas.TierSandbox,
		"peer": atlas.TierSphere, "ollama": atlas.TierSphere,
		"dks": atlas.TierCluster, "kubectl": atlas.TierCluster, "helm": atlas.TierCluster,
		"tessaro": atlas.TierAccount, "login": atlas.TierAccount,
	}
	for n, want := range spot {
		e, ok := atlas.Lookup(n)
		if !ok {
			t.Fatalf("%s: not in the atlas", n)
		}
		if e.Tier != want {
			t.Errorf("%s: tier %q, want %q", n, e.Tier, want)
		}
	}
	if e := atlas.RegistryEntry(6); e.Tier != atlas.TierCloud {
		t.Errorf("RegistryEntry(6).Tier = %q, want %q (provider CLIs)", e.Tier, atlas.TierCloud)
	}
}

// An alias is a second spelling of its target, so it carries the target's
// tier — every alias in both tables, including the hand-written ones that
// predate aliasVerb.
func TestAliasesInheritTier(t *testing.T) {
	seen := 0
	for _, n := range slices.Concat(atlas.ToolNames(), atlas.VerbNames()) {
		e, _ := atlas.Lookup(n)
		if e.AliasOf == "" {
			continue
		}
		seen++
		target, ok := atlas.Lookup(e.AliasOf)
		if !ok {
			t.Errorf("%s: alias_of %q does not resolve", n, e.AliasOf)
			continue
		}
		if e.Tier != target.Tier {
			t.Errorf("%s: tier %q differs from target %s: %q", n, e.Tier, e.AliasOf, target.Tier)
		}
	}
	for _, n := range []string{"issue", "invoke", "messages", "sphere", "docker", "[", "rust"} {
		if e, _ := atlas.Lookup(n); e.AliasOf == "" {
			t.Errorf("%s: expected an alias (the inheritance check needs real aliases to bite)", n)
		}
	}
	if seen == 0 {
		t.Fatal("no aliases found; the inheritance check is vacuous")
	}
}

// A recipe (idiom) is classified by the purpose of the commands it composes:
// its tier is the tier of at least one member. A recipe made only of shell
// builtins has no atlas member and is userland.
func TestIdiomsAgreeWithMetadata(t *testing.T) {
	for _, id := range atlas.Idioms() {
		members := map[string]bool{}
		for _, c := range id.Commands {
			if e, ok := atlas.Lookup(c); ok {
				members[e.Tier] = true
			}
		}
		if len(members) == 0 {
			if id.Tier != atlas.TierUserland {
				t.Errorf("idiom %s: builtin-only recipe is tier %q, want %q", id.ID, id.Tier, atlas.TierUserland)
			}
			continue
		}
		if !members[id.Tier] {
			t.Errorf("idiom %s: tier %q is not the tier of any member %v", id.ID, id.Tier, id.Commands)
		}
	}
}

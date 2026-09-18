// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package atlas_test

import (
	"slices"
	"testing"

	"github.com/qiangli/yoke/pkg/atlas"
)

// LOCAL-FIRST, AS A RATCHET.
//
// bashy's central claim is that an agent needs nothing but bashy: the whole software
// lifecycle — record what is wanted, do the work in isolation, decide whether it passes,
// build it, ship it — closes on ONE MACHINE, with NO NETWORK. Not "works offline in a
// degraded mode": the loop is local by construction, and the network is an optional
// extension of it.
//
// A claim like that is worth exactly as much as its enforcement. Written in a README it
// survives until the first well-meaning commit that reaches for a hosted API "just for
// this one verb" — and nobody notices, because nobody re-reads the README. So it lives
// here instead, in the axis the atlas already maintains, and a verb that starts phoning
// home fails the build.
//
// The escape hatch is deliberate and narrow: to make one of these verbs reach the
// network you must DELETE IT FROM THIS LIST, in a diff, where a reviewer can see the
// philosophy being traded away and ask what for.

// theLoop is the local-first SDLC spine — the verbs that must work in an air-gapped
// room with no account, no forge, and no cloud.
var theLoop = []string{
	"todo",   // PLAN   — what is wanted, per repo (docs/todo/) or per host. No forge.
	"sprint", // PLAN  — the board.
	"weave",  // CODE   — isolated workspaces, real git, local branches.
	"gate",   // TEST   — run the command; the exit status is the verdict.
	"check",  // TEST   — static preflight.
	"dag",    // CROSS  — build/test/deploy targets; the make replacement.
	"kb",     // CROSS  — what this host has learned.
	"skill",  // CROSS  — what this host knows how to do.
}

func TestTheSDLCLoopIsAirGapped(t *testing.T) {
	for _, name := range theLoop {
		e, ok := atlas.Lookup(name)
		if !ok {
			t.Errorf("%s: not in the atlas — the local-first loop names a verb that does not exist", name)
			continue
		}
		if slices.Contains(e.Effects, atlas.EffNet) {
			t.Errorf(`%s declares the "net" effect.

%s is part of bashy's local-first SDLC loop: the claim is that an agent can record a
requirement, do the work, decide whether it passes, and build it, on ONE MACHINE with NO
NETWORK. A verb in this loop that reaches the network breaks that claim for everyone in
an air-gapped room — and quietly, because nothing else would have noticed.

If the network is genuinely required, remove %q from theLoop in this file, so the
trade-off is visible in the diff and a reviewer can ask what it bought.`, name, name, name)
		}
		if slices.Contains(e.Effects, atlas.EffRemote) && name != "dag" {
			// dag is the ONE exception, and it is an honest one: `dag --fleet` can
			// distribute chunks to other hosts. That is an OPT-IN extension of a runner
			// that is local by default — one host is an ordinary fleet size, not a
			// fallback.
			t.Errorf("%s declares %q: the loop must not require another machine", name, atlas.EffRemote)
		}
	}
}

// The one irreducible external is INFERENCE — and bashy ships a local answer for it.
//
// judge, invoke and meet need a model. That is the single point in the lifecycle where
// bashy cannot be self-sufficient by arithmetic alone... unless the model runs here too.
// It can: `bashy ollama` is a managed LOCAL runtime. So the air-gapped story has no hole
// in it, only a prerequisite — and the prerequisite is in the box.
//
// This test exists so that stays true. If `ollama` ever stops being a verb bashy can
// launch itself, the local-first claim quietly becomes false for every LLM-shaped verb,
// and this is the only place that would say so.
func TestInferenceHasALocalAnswer(t *testing.T) {
	if _, ok := atlas.Lookup("ollama"); !ok {
		t.Fatal(`bashy no longer ships a local inference runtime.

judge/invoke/meet need a model, and that is the ONE thing in the lifecycle bashy cannot
compute for itself. ` + "`bashy ollama`" + ` is what makes the local-first claim survive
contact with an air-gapped room. Without it, every LLM-shaped verb silently requires a
hosted API, and "bashy is all an agent needs" stops being true.`)
	}
	for _, n := range []string{"pair", "judge", "invoke"} {
		e, ok := atlas.Lookup(n)
		if !ok {
			t.Errorf("%s missing from the atlas", n)
			continue
		}
		// These verbs are ALLOWED to touch the network — they call a model, and the
		// model may be hosted. What is not allowed is for them to be the only option:
		// the point is that the same verb works against a local runtime.
		if !slices.Contains(e.Effects, atlas.EffSpend) {
			t.Errorf("%s does not declare %q — an agent must be able to see that a verb costs money before it fans out",
				n, atlas.EffSpend)
		}
	}
}

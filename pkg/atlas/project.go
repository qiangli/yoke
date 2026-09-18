// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package atlas

import "sort"

// effectProjection maps the atlas-11 security effects onto the dhnt-6 skill
// effect lattice (read write net spend destroy time). It is the "future policy
// engine" projection the Effects vocabulary promises, made concrete and
// table-driven so both sides can be read at a glance.
//
// Six atoms carry across. `remote` folds into `net`: crossing the machine
// boundary is a network egress as far as an effect CAP is concerned, and the
// finer distinction is still available on the atlas side. The remaining five —
// exec, cred, priv, persist, pure — have NO counterpart in the dhnt lattice and
// are dropped: `pure` is the explicit absence of a governed effect, and the
// other four are the shell-side distinctions the lattice does not draw. A
// consumer that needs them reads the atlas effects unprojected.
var effectProjection = map[string]string{
	EffRead:    "read",
	EffWrite:   "write",
	EffDestroy: "destroy",
	EffNet:     "net",
	EffRemote:  "net",
	EffSpend:   "spend",
}

// ProjectEffects projects atlas effect atoms onto the dhnt-6 lattice.
//
// Pure: the output is sorted and deduplicated, and depends only on the input.
// An atom outside the atlas vocabulary is dropped silently rather than
// reported — the atlas is ratcheted at init, so an unknown atom here means a
// caller assembled the slice by hand, and a projection that fails on it would
// only turn a typo into an outage. Returns nil when nothing carries across.
func ProjectEffects(atlas []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, a := range atlas {
		d, ok := effectProjection[a]
		if !ok || seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}

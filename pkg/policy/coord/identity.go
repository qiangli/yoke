// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package coord

import (
	"crypto/rand"
	"encoding/hex"
	"os"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/principal"
)

// EpisodeEnv is the session id every bashy process inherits.
const EpisodeEnv = "BASHY_EPISODE"

// Self resolves who this process is, and MINTS an identity when there is none.
//
// This is the gap that made the whole failure possible, and it is worth stating
// exactly:
//
// principal.Resolver already resolves an identity — but BASHY_PRINCIPAL and
// BASHY_EPISODE are only ever injected into children SPAWNED by weave or by the
// agent runner. A human who types `claude` in a terminal gets NOTHING. Two such
// sessions are therefore not merely uncoordinated; they are mutually INVISIBLE.
// There is no id to compare, no holder to name, nothing to collide with. You cannot
// build a claim on top of an identity that does not exist.
//
// So: if the ambient environment has no episode, mint one and export it, so that
// every child of this shell shares it. The tool name still comes from the harness
// markers (CLAUDECODE, CODEX_SANDBOX, …), which do work for a human-launched
// session — it is only the INSTANCE that was missing.
func Self() principal.Ref {
	ref, _ := principal.NewResolver(fleet.New(), principal.DefaultEnv()).Self()

	if ref.Episode == "" {
		ep := os.Getenv(EpisodeEnv)
		if ep == "" {
			ep = mintEpisode()
			// Export it: every child of this process — a subagent, a hook, a
			// `bashy git commit` — must be recognised as the SAME agent, or a
			// session would collide with itself.
			_ = os.Setenv(EpisodeEnv, ep)
		}
		ref.Episode = ep
	}
	if ref.Name == "" {
		// An unattributed session still gets a stable, distinguishable name — an
		// anonymous holder is still a holder, and "someone else is working here" is
		// infinitely more useful than silence.
		ref.Name = "session-" + short(ref.Episode)
	}
	if ref.Host == "" {
		ref.Host, _ = os.Hostname()
	}
	return ref
}

func mintEpisode() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "ep-unknown"
	}
	return "ep-" + hex.EncodeToString(b)
}

func short(s string) string {
	if len(s) > 9 {
		return s[3:9] // skip the "ep-" prefix
	}
	return s
}

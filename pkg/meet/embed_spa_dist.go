// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

//go:build meetspa

package meet

import (
	"embed"
	"io/fs"
)

// The SPA as web/dist — the bundle a Vite build JUST produced, rather than the
// artifact/ copy last promoted into the tree.
//
// This exists for exactly one caller: pkg/meet/web/e2e, whose whole claim is
// that it tests THIS checkout's SPA. That claim had quietly become false. The
// tag was removed when the default embed moved to artifact/, but the harness
// kept passing `-tags meetspa` — a tag nothing consumed — so it built the SPA,
// discarded it, and asserted against the committed bundle instead. A browser
// suite that cannot see the code under test is worse than no browser suite: it
// reports green for a change it never loaded.
//
// Building WITH this tag requires web/dist to exist (`npm run build` in
// pkg/meet/web). That is not a hazard the way it once was, because it is no
// longer the shipping path — the untagged build embeds artifact/ and always
// compiles.

//go:embed all:web/dist
var spaDistEmbed embed.FS

func init() {
	// all: is deliberate — Vite emits dot-prefixed files (.vite/manifest.json)
	// that a bare go:embed silently skips.
	if sub, err := fs.Sub(spaDistEmbed, "web/dist"); err == nil {
		spaFS = sub
	}
}

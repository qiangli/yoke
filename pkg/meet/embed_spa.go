//go:build !meetspa

package meet

import (
	"embed"
	"io/fs"
)

// The web room, compiled in unconditionally.
//
// This used to sit behind `-tags meetspa` because the embed pointed at
// web/dist — another track's build output — and a go:embed of a directory that
// does not exist yet is a compile error for the whole package. The tag bought
// build safety and charged for it in a worse way: a binary built the ordinary
// way silently had no UI, and every consumer had to know a magic tag.
//
// artifact/ is the fix. It is TRACKED, so the embed always resolves and the
// room is in every build; web/dist stays ignored as the SPA's own scratch, and
// the build script promotes dist -> artifact as a deliberate, reviewable step.
// The whole bundle is ~650 KB.
//
// `-tags meetspa` (embed_spa_dist.go) embeds web/dist INSTEAD, for the one
// caller that must test the bundle it just built rather than the last one
// promoted: the Playwright harness. The tag exists only for that, and the
// DEFAULT is still artifact/ — which is what removed the old dilemma, since an
// untagged build is never left with no UI.

//go:embed all:artifact
var spaEmbed embed.FS

func init() {
	// all: is deliberate — Vite emits dot-prefixed files (.vite/manifest.json)
	// that a bare go:embed silently skips.
	if sub, err := fs.Sub(spaEmbed, "artifact"); err == nil {
		spaFS = sub
	}
}

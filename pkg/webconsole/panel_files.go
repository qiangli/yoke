// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"net/http"
	"os"

	"github.com/filebrowser/filebrowser/v2/fbembed"
)

// filesLogoPath is the launcher's files-tile folder mark
// (pkg/webconsole/artifact/app.js): a single-stroke, no-fill, 24-grid path.
// It replaces File Browser's stock floppy-disk logo so the files page carries
// the same mark as its tile.
const filesLogoPath = "M4 8a2 2 0 0 1 2-2h3.5l2 2H18a2 2 0 0 1 2 2v6a2 2 0 0 1-2 2H6a2 2 0 0 1-2-2z"

// filesLogoSVG is the branded replacement for the stock img/logo.svg, drawn
// in the files tile's blue (COLORS.files). The header constrains it to
// height 2.5em with width auto, so a square 24-grid viewBox scales cleanly.
var filesLogoSVG = []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" width="64" height="64" fill="none" stroke="#3478f6" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="` + filesLogoPath + `"/></svg>`)

// filesPanel mounts File Browser in-process.
//
// fbembed is the seam outpost already uses for its `files` builtin: stateless
// (no database — scope and permissions are recomputed from Options every boot),
// NoAuth because the EMBEDDING HOST is the access gate, and it already renders
// <base href>/StaticURL from X-Forwarded-Prefix, so it needs no per-mount config
// to work both on loopback and under a path prefix.
//
// Read-only by default. AllowWrite flips create/upload/edit/rename/delete
// together; command execution is never enabled.
//
// NOTE this panel is a DATA PLANE — it serves file bytes. That is fine on
// loopback and through outpost's direct tunnel; a download path that rides the
// cloudbox relay would violate the fail-closed data-plane block.
func filesPanel(scope string, allowWrite bool) (http.Handler, func() error, error) {
	// Default to the user's home, NOT fbembed's own default of the working
	// directory. A launcher is usually started from whatever directory the shell
	// happened to be in, so a cwd default means the same command shows a
	// different file tree each time — and one that silently narrows to a project
	// checkout. Home is what outpost's `files` builtin uses, and it is the answer
	// a person expects from a tile labelled "Files".
	if scope == "" {
		if home, err := os.UserHomeDir(); err == nil {
			scope = home
		}
	}
	h, closer, err := fbembed.New(fbembed.Options{
		Scope:      scope,
		AllowWrite: allowWrite,
	})
	if err != nil {
		return nil, nil, err
	}
	return filesBrandLogo(h), closer, nil
}

// filesBrandLogo serves the branded logo over File Browser's stock one. The
// SPA loads its top-left mark from <staticURL>/img/logo.svg, which reaches
// this handler mount-relative as /static/img/logo.svg (the mount prefix is
// already stripped), so one exact-path override rebrands every mount with no
// behaviour change — everything else passes through untouched.
//
// Why an override here rather than the fork's Branding.Files knob or a
// patched asset: Branding.Files needs Options plumbing in the sibling
// filebrowser checkout (a separate repo, not this branch) plus a host-side
// directory, and img/logo.svg ships inside fbembed's generated dist.zip
// (a Node-toolchain rebuild to repatch). This keeps the vendored bytes
// pristine and the branding where the seam already lives.
func filesBrandLogo(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/static/img/logo.svg" {
			w.Header().Set("Content-Type", "image/svg+xml")
			w.Header().Set("Cache-Control", "public, max-age=86400")
			_, _ = w.Write(filesLogoSVG)
			return
		}
		next.ServeHTTP(w, r)
	})
}

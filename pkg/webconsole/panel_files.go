// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"log/slog"
	"net/http"
	"os"
	"sync"

	"github.com/gtsteffaniak/filebrowser/backend/fbembed"
)

// filesLogoPath is the launcher's files-tile folder mark
// (pkg/webconsole/artifact/app.js): a single-stroke, no-fill, 24-grid path.
// It replaces File Browser's stock floppy-disk logo so the files page carries
// the same mark as its tile.
const filesLogoPath = "M4 8a2 2 0 0 1 2-2h3.5l2 2H18a2 2 0 0 1 2 2v6a2 2 0 0 1-2 2H6a2 2 0 0 1-2-2z"

// filesLogoSVG is the branded replacement for the stock favicon.svg, drawn
// in the files tile's blue (COLORS.files). A square 24-grid viewBox scales
// cleanly wherever the page shows its icon.
var filesLogoSVG = []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" width="64" height="64" fill="none" stroke="#3478f6" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="` + filesLogoPath + `"/></svg>`)

// filesPanel mounts File Browser in-process.
//
// fbembed is the seam outpost already uses for its `files` builtin: scope and
// permissions are recomputed from Options every boot and the engine's own
// state (lone user, index, preview cache) lives in a temporary directory the
// closer removes; noauth because the EMBEDDING HOST is the access gate; and it
// renders baseURL/staticURL from X-Forwarded-Prefix, so it needs no per-mount
// config to work both on loopback and under a path prefix.
//
// Read-only by default. AllowWrite flips create/upload/edit/rename/delete
// together; command execution is never enabled.
//
// The engine boots on the FIRST request, not when the launcher starts: booting
// it scans the scope into its index in the background, and a home directory
// is large. A console whose files tile is never opened pays nothing, and the
// many consoles a test run creates stay as cheap as they were with the
// previous, index-free seam. The engine is process-global (one instance at a
// time), so a boot failure is answered with 503 and logged rather than taking
// the launcher down.
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
	p := &lazyFilesPanel{opts: fbembed.Options{Scope: scope, AllowWrite: allowWrite}}
	return filesBrandLogo(p), p.close, nil
}

// lazyFilesPanel boots the embedded File Browser on first use and closes it
// once. Requests after close are refused rather than booting a new instance
// the owner has no closer for.
type lazyFilesPanel struct {
	opts fbembed.Options

	mu     sync.Mutex
	h      http.Handler
	closer func() error
	err    error
	closed bool
}

func (p *lazyFilesPanel) handler() (http.Handler, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, http.ErrServerClosed
	}
	if p.h == nil && p.err == nil {
		p.h, p.closer, p.err = fbembed.New(p.opts)
		if p.err != nil {
			slog.Error("files panel", "err", p.err)
		}
	}
	return p.h, p.err
}

func (p *lazyFilesPanel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h, err := p.handler()
	if err != nil {
		http.Error(w, "files: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	h.ServeHTTP(w, r)
}

func (p *lazyFilesPanel) close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	if p.closer == nil {
		return nil
	}
	closer := p.closer
	p.closer, p.h = nil, nil
	return closer()
}

// filesBrandLogo serves the branded icon over File Browser's stock one. The
// SPA has no header mark of its own; its icon is the favicon the page links
// from <staticURL>/favicon.svg, which reaches this handler mount-relative as
// /public/static/favicon.svg (the mount prefix is already stripped), so one
// exact-path override rebrands every mount with no behaviour change —
// everything else passes through untouched.
//
// Why an override here rather than the engine's custom-favicon setting: that
// needs a host-side file the engine validates and rasterizes into PWA icons
// on every boot, and favicon.svg ships inside fbembed's generated dist.zip (a
// Node-toolchain rebuild to repatch). This keeps the vendored bytes pristine
// and the branding where the seam already lives.
func filesBrandLogo(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/public/static/favicon.svg" {
			w.Header().Set("Content-Type", "image/svg+xml")
			w.Header().Set("Cache-Control", "public, max-age=86400")
			_, _ = w.Write(filesLogoSVG)
			return
		}
		next.ServeHTTP(w, r)
	})
}

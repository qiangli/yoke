// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"net/http"
	"os"

	"github.com/filebrowser/filebrowser/v2/fbembed"
)

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
	return fbembed.New(fbembed.Options{
		Scope:      scope,
		AllowWrite: allowWrite,
	})
}

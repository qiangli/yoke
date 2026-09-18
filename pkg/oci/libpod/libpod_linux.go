//go:build linux || freebsd

// Package libpod re-exports Podman libpod runtime for in-process API server.
package libpod

import "go.podman.io/podman/v6/libpod"

var NewRuntime = libpod.NewRuntime

// Package podman_embed self-extracts an embedded podman binary into
// the user cache on first use. Built into the ycode binary via
// `-tags embed_podman` when scripts/embed-podman.sh has produced
// internal/container/podman_embed/podman.gz.
//
// Upstream:    github.com/containers/podman (cmd/podman)
// License:     Apache-2.0 — verified against external/podman/LICENSE.
//
//	Permissive OSI per the ycode embed policy.
//
// How to rebuild the embed:
//
//	make podman-embed             # explicit
//	make build                    # implicit, via podman-embed-if-missing
//
// embed-podman.sh prefers an upstream-podman binary already installed
// on $PATH (detected by `podman --version` returning the upstream
// signature); falls back to building from external/podman/cmd/podman/
// with `-tags remote exclude_graphdriver_btrfs containers_image_openpgp`
// on macOS/Windows (client-only — the engine runs in a podman-machine
// VM) and a native build on Linux (full engine, no VM). The script
// never auto-installs upstream podman; users who explicitly want their
// own system binary should run ycode with --use-system-binaries
// (or set container.useSystem: true in settings.json).
package podman_embed

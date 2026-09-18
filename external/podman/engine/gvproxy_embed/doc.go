// Package gvproxy_embed self-extracts an embedded gvproxy binary (the
// user-mode network proxy `podman machine` forwards host sockets
// through) into the user cache on first use. Built into the ycode
// binary via `-tags embed_gvproxy` when scripts/embed-gvproxy.sh has
// produced internal/container/gvproxy_embed/gvproxy.gz. Used on macOS
// and Windows; not needed on Linux (podman uses its native socket
// directly).
//
// Upstream:    github.com/containers/gvisor-tap-vsock (cmd/gvproxy)
// License:     Apache-2.0 — permissive OSI per the ycode embed policy.
// How to rebuild the embed:
//
//	make gvproxy-embed                  # explicit
//	make build                          # implicit, via gvproxy-embed-if-applicable
//
// The build sources the binary from the Go module cache (the version
// pinned by external/podman/go.mod), not from a system install. There
// is no brew/upstream-install fallback by design — upstream podman
// distributions ship gvproxy as a separate package, so we build it
// ourselves to avoid version skew.
package gvproxy_embed

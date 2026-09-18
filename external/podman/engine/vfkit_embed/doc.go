// Package vfkit_embed self-extracts an embedded vfkit binary (Apple
// Virtualization Framework helper for `podman machine`) into the user
// cache on first use. Built into the ycode binary via `-tags embed_vfkit`
// when scripts/embed-vfkit.sh has produced
// internal/container/vfkit_embed/vfkit.gz. macOS only — other platforms
// don't use it.
//
// Upstream:    github.com/crc-org/vfkit
// License:     Apache-2.0 — permissive OSI per the ycode embed policy.
// How to rebuild the embed:
//
//	make vfkit-embed              # explicit
//	make build                    # implicit, via vfkit-embed-if-darwin
//
// On macOS the embed script ad-hoc-signs the binary with the
// `com.apple.security.virtualization` entitlement (sourced from
// upstream's vf.entitlements), otherwise Virtualization.framework
// rejects it with VZError Code=2 on the first VM start.
package vfkit_embed

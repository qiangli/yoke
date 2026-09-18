//go:build !embed_podman

package podman_embed

// No embedded podman — Available() returns false.
// Podman will be discovered via socket, PATH, or explicit config.
// Build with -tags embed_podman after running: make podman-embed

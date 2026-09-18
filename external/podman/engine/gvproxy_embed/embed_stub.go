//go:build !embed_gvproxy

package gvproxy_embed

// No embedded gvproxy — Available() returns false.
// gvproxy will be discovered via the host's helper_binaries_dir.

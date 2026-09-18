//go:build !embed_vfkit

package vfkit_embed

// No embedded vfkit — Available() returns false.
// vfkit will be discovered via PATH or Homebrew.

//go:build !embed_runner

package runner_embed

// No embedded runner — Available() returns false.
// The runner will be discovered via PATH or explicit config.
// Build with -tags embed_runner after running: make runner-build-thin

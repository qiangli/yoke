// Package runner_embed self-extracts an embedded ollama inference
// runner subprocess (llama.cpp + thin HTTP server) into the user cache
// on first use. The embed_runner build requires both ycode-runner.gz and
// ycode-runner.sigstore.json. Release identity, tag, and final executable
// SHA256 must be committed in extract.go before it becomes available.
//
// The CUDA variant (linux/amd64, cuda.go) is not embedded: it is fetched by
// binmgr from the pinned signed release, installed in upstream's bin/ +
// lib/ollama/ layout, and fails closed before start (layout, host CUDA
// runtime) and after start (no CUDA device in the runner's log). Hosts route
// their runner re-exec through DispatchCUDA first.
//
// Upstream:    github.com/ollama/ollama (cmd/runner)
// License:     MIT.
package runner_embed

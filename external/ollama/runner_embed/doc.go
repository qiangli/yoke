// Package runner_embed self-extracts an embedded ollama inference
// runner subprocess (llama.cpp + thin HTTP server) into the user cache
// on first use. The embed_runner build requires both ycode-runner.gz and
// ycode-runner.sigstore.json. Release identity, tag, and final executable
// SHA256 must be committed in extract.go before it becomes available.
//
// Upstream:    github.com/ollama/ollama (cmd/runner)
// License:     MIT.
package runner_embed

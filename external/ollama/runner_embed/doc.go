// Package runner_embed self-extracts an embedded ollama inference
// runner subprocess (llama.cpp + thin HTTP server) into the user cache
// on first use. Built into the binary via `-tags embed_runner`
// when scripts/build-runner-thin.sh has produced
// external/ollama/runner_embed/ycode-runner.gz.
//
// Upstream:    github.com/ollama/ollama (cmd/runner)
// License:     MIT.
package runner_embed

package registry

import (
	"context"
	"fmt"

	"github.com/qiangli/yoke/pkg/binmgr"
)

// OllamaVersion is the default pinned release of Ollama.
// Retained for compatibility; the registry download path is disabled per
// docs/bashy-service-supply-chain-policy.md (supply-chain policy). The
// embedded server (external/ollama, runner_embed + src) is the only allowed
// runtime.
const OllamaVersion = "v0.40.1"

const ollamaSupplyChainPolicy = "docs/bashy-service-supply-chain-policy.md"

// ollama — the managed local LLM runtime (ollama/ollama, MIT), tier 3 (sandbox).
// Third-party release archives are DISABLED per docs/bashy-service-supply-chain-policy.md.
// bashy must never download or exec an upstream ollama release archive; the
// embedded runner compiled from source (external/ollama: runner_embed, src,
// ollama_component.go) is the only allowed runtime. See Sprint 434 / Story 2005.
func init() {
	register(Entry{
		Name:           "ollama",
		Tier:           3,
		License:        "MIT",
		Synopsis:       "Ollama local LLM runner (embedded only; no third-party download)",
		EnvVersion:     "OLLAMA_VERSION",
		DefaultVersion: OllamaVersion,
		Long: `ollama (ollama/ollama, MIT) is the managed local LLM runtime — now embedded from source only.
Third-party release archives are disabled per docs/bashy-service-supply-chain-policy.md (supply-chain policy): bashy never downloads or execs an upstream ollama release archive. Use the embedded server (build with -tags embed_runner; see external/ollama/runner_embed and external/ollama/src). $OLLAMA_VERSION is retained for compatibility but does not trigger a download.`,
		Resolve: func(ctx context.Context, version string) (binmgr.Tool, error) {
			if version == "" {
				version = OllamaVersion
			}
			return binmgr.Tool{}, fmt.Errorf("ollama: third-party download disabled per %s — refusing to resolve %q (embedded only; build with -tags embed_runner; supply-chain policy)", ollamaSupplyChainPolicy, version)
		},
	})
}

// ollamaEntrypoint and ollamaAssetMatch are retained as stubs so existing
// callers that reference them continue to compile. They are no longer used
// for downloads and always indicate no match.
func ollamaEntrypoint() string { return "bin/ollama" }

func ollamaAssetMatch(name, goos, goarch string) bool { return false }

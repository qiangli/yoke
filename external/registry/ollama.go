package registry

import (
	"context"
	"runtime"
	"strings"

	"github.com/qiangli/yoke/pkg/binmgr"
)

// OllamaVersion is the default pinned release of Ollama.
const OllamaVersion = "v0.40.1"

// ollama — the managed local LLM runtime (ollama/ollama, MIT), tier 3 (sandbox).
// Ships per-platform release archives with executable and runner libraries.
func init() {
	register(Entry{
		Name:           "ollama",
		Tier:           3,
		License:        "MIT",
		Synopsis:       "Ollama local LLM runner (managed external, MIT)",
		EnvVersion:     "OLLAMA_VERSION",
		DefaultVersion: OllamaVersion,
		Long: `ollama (ollama/ollama, MIT) is the managed local LLM runtime — downloaded from
GitHub releases, sha256-verified against pinned digests, and cached by binmgr (not
compiled in). $OLLAMA_VERSION pins the release; all args pass through to ollama.`,
		Resolve: func(ctx context.Context, version string) (binmgr.Tool, error) {
			if version == "" {
				version = OllamaVersion
			}
			return binmgr.ResolveGitHub(ctx, binmgr.GitHubSpec{
				Name:       "ollama",
				Repo:       "ollama/ollama",
				Version:    version,
				Tree:       true,
				Entrypoint: ollamaEntrypoint(),
				AssetMatch: ollamaAssetMatch,
			})
		},
	})
}

func ollamaEntrypoint() string {
	switch runtime.GOOS {
	case "darwin":
		return "ollama"
	case "windows":
		return "ollama.exe"
	default:
		return "bin/ollama"
	}
}

func ollamaAssetMatch(name, goos, goarch string) bool {
	n := strings.ToLower(name)
	switch goos {
	case "darwin":
		return n == "ollama-darwin.tgz"
	case "windows":
		return n == "ollama-windows-"+goarch+".zip"
	case "linux":
		return n == "ollama-linux-"+goarch+".tar.zst"
	default:
		return false
	}
}

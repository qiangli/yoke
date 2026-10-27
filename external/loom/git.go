package loom

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/qiangli/yoke/external/gitscm"
)

// Gitea executes Git itself. A Windows host can run "bashy git" without a
// bare git on PATH, so point Gitea at bashy's verified, managed MinGit.
func ensureRuntimeConfig(ctx context.Context, dataDir, addr string, port int, rootURL string, actions bool, st *Storage) (string, error) {
	cfg, err := ensureConfig(dataDir, addr, port, rootURL, actions, st)
	if err != nil || runtime.GOOS != "windows" {
		return cfg, err
	}
	err = ensureGitConfig(ctx, cfg, gitscm.Ensure)
	return cfg, err
}

func ensureGitConfig(ctx context.Context, cfg string, resolve func(context.Context) (string, error)) error {
	git, err := resolve(ctx)
	if err != nil {
		return fmt.Errorf("loom: provision git: %w", err)
	}
	if git == "" {
		return fmt.Errorf("loom: provision git: empty executable path")
	}
	data, err := os.ReadFile(cfg)
	if err != nil {
		return err
	}
	ini := ensureSectionKeys(string(data), "git", map[string]string{"PATH": strings.ReplaceAll(git, "\\", "/")})
	if ini == string(data) {
		return nil
	}
	return os.WriteFile(cfg, []byte(ini), 0o600)
}

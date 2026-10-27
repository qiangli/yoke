package loom

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/qiangli/yoke/external/gitscm"
)

// Gitea executes Git itself. A Windows host can run "bashy git" without a
// bare git on PATH, so point Gitea at bashy's verified, managed MinGit. The
// second result is that git's executable path, empty off Windows.
func ensureRuntimeConfig(ctx context.Context, dataDir, addr string, port int, rootURL string, actions bool, st *Storage) (string, string, error) {
	cfg, err := ensureConfig(dataDir, addr, port, rootURL, actions, st)
	if err != nil || runtime.GOOS != "windows" {
		return cfg, "", err
	}
	git, err := ensureGitRuntime(ctx, cfg, gitscm.Ensure)
	return cfg, git, err
}

// ensureGitRuntime provisions the managed git, records it in app.ini, and makes
// that git able to EXECUTE the hook scripts Gitea writes into every repo — the
// MinGit tree ships bash only under the name "sh" (see hooks.go).
func ensureGitRuntime(ctx context.Context, cfg string, resolve func(context.Context) (string, error)) (string, error) {
	git, err := resolve(ctx)
	if err != nil {
		return "", fmt.Errorf("loom: provision git: %w", err)
	}
	if git == "" {
		return "", fmt.Errorf("loom: provision git: empty executable path")
	}
	root := gitRootFromExe(git)
	if root == "" {
		return "", fmt.Errorf("loom: provision hook shell: unexpected managed git layout: %s", git)
	}
	if _, err := ensureHookShell(filepath.Join(root, "usr", "bin")); err != nil {
		return "", err
	}
	if err := writeGitConfig(cfg, git); err != nil {
		return "", err
	}
	return git, nil
}

// writeGitConfig records git's path in app.ini's [git] section.
func writeGitConfig(cfg, git string) error {
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

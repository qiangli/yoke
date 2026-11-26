// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package ollama

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/artifact"
)

const (
	defaultOfflineNumParallel     = "1"
	defaultOfflineMaxLoadedModels = "1"
	defaultOfflineContextLength   = "4096"
)

func hasEnvKey(env []string, key string) bool {
	prefix := key + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) && strings.TrimSpace(strings.TrimPrefix(entry, prefix)) != "" {
			return true
		}
	}
	return false
}

func defaultEnvIfUnset(env []string, key, value string) []string {
	if hasEnvKey(env, key) {
		return env
	}
	return replaceEnv(env, key, value)
}

func setProcessEnvDefault(key, value string) {
	if strings.TrimSpace(os.Getenv(key)) == "" {
		_ = os.Setenv(key, value)
	}
}

func openReadArtifactBackend(ctx context.Context, root string) (artifact.Backend, bool, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		root = artifact.DefaultRoot()
	}
	if client, ok := artifact.TryConnectService(ctx, root); ok {
		return client, true, nil
	}
	dbPath := filepath.Join(root, "catalog.db")
	fi, err := os.Stat(dbPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, err
	}
	if fi.IsDir() {
		return nil, false, nil
	}
	st, err := artifact.OpenStore(ctx, artifact.StoreOptions{Root: root, ReadOnly: true})
	if err != nil {
		if errors.Is(err, artifact.ErrNotFound) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return st, true, nil
}

// pullModelSealed restores model locally from the protected artifact store when
// sealed mode is active, or fails closed with artifact.ErrSealedNetworkDenied
// without making any network connection.
func pullModelSealed(ctx context.Context, root, modelsDir, model string) error {
	backend, ok, err := openReadArtifactBackend(ctx, root)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("ollama pull %s: no local artifact catalog: %w", model, artifact.ErrSealedNetworkDenied)
	}
	defer backend.Close()

	if _, err := artifact.RestoreOllamaModel(ctx, backend, modelsDir, model); err != nil {
		if errors.Is(err, artifact.ErrNotFound) {
			return fmt.Errorf("ollama pull %s: model not pinned in local artifact store: %w", model, artifact.ErrSealedNetworkDenied)
		}
		return err
	}
	return nil
}

// restoreManagedModelsIfAvailable restores any pinned Ollama models from the
// local artifact store into modelsDir before starting the managed server so
// inference and embeddings survive OLLAMA_MODELS cache cleanup and restarts.
func restoreManagedModelsIfAvailable(ctx context.Context, root, modelsDir string) (int, error) {
	artRoot := strings.TrimSpace(root)
	if artRoot == "" {
		artRoot = artifact.DefaultRoot()
	}
	sealed := artifact.IsSealed(artRoot)

	backend, ok, err := openReadArtifactBackend(ctx, artRoot)
	if err != nil {
		if sealed {
			return 0, err
		}
		return 0, nil
	}
	if !ok {
		return 0, nil
	}
	defer backend.Close()

	restored, err := artifact.RestoreAllPinnedModels(ctx, backend, modelsDir)
	if err != nil {
		if sealed {
			return 0, err
		}
		return 0, nil
	}
	return len(restored), nil
}

func newOllamaPushCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "push MODEL",
		Short: "Push a model to a registry (disabled in offline/no-cloud mode)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if artifact.IsSealed("") {
				return fmt.Errorf("ollama push %s: %w", args[0], artifact.ErrSealedNetworkDenied)
			}
			return fmt.Errorf("ollama push %s: cloud registry push disabled by OLLAMA_NO_CLOUD=1: %w", args[0], artifact.ErrSealedNetworkDenied)
		},
	}
}

func newOllamaSigninCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "signin",
		Short: "Sign in to ollama cloud (disabled in offline/no-cloud mode)",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if artifact.IsSealed("") {
				return fmt.Errorf("ollama signin: %w", artifact.ErrSealedNetworkDenied)
			}
			return fmt.Errorf("ollama signin: cloud authentication disabled by OLLAMA_NO_CLOUD=1: %w", artifact.ErrSealedNetworkDenied)
		},
	}
}

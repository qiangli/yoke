// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/qiangli/yoke/pkg/artifact"
)

func seedTestOllamaModelInCAS(t *testing.T, artRoot, modelRef string) {
	t.Helper()
	ctx := context.Background()
	srcModels := filepath.Join(t.TempDir(), "seed-ollama-models")
	blobsDir := filepath.Join(srcModels, "blobs")
	if err := os.MkdirAll(blobsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	writeBlob := func(mediaType string, data []byte) artifact.OllamaManifestLayer {
		dgst := artifact.DigestBytes(data)
		fname, err := artifact.OllamaBlobFilename(dgst)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(blobsDir, fname), data, 0o644); err != nil {
			t.Fatal(err)
		}
		return artifact.OllamaManifestLayer{
			MediaType: mediaType,
			Digest:    dgst,
			Size:      int64(len(data)),
		}
	}

	ref, err := artifact.ParseOllamaModelRef(modelRef)
	if err != nil {
		t.Fatal(err)
	}
	cfg := writeBlob("application/vnd.docker.container.image.v1+json", []byte(`{"model_format":"gguf"}`))
	weights := writeBlob("application/vnd.ollama.image.model", []byte("GGUF-test-weights-"+ref.Canonical()))
	tmpl := writeBlob("application/vnd.ollama.image.template", []byte("{{ .Prompt }}"))

	rawManifest, err := json.Marshal(artifact.OllamaDiskManifest{
		SchemaVersion: 2,
		MediaType:     "application/vnd.docker.distribution.manifest.v2+json",
		Config:        cfg,
		Layers:        []artifact.OllamaManifestLayer{weights, tmpl},
	})
	if err != nil {
		t.Fatal(err)
	}
	mPath := filepath.Join(srcModels, filepath.FromSlash(ref.ManifestRelPath()))
	if err := os.MkdirAll(filepath.Dir(mPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mPath, rawManifest, 0o644); err != nil {
		t.Fatal(err)
	}

	store, err := artifact.OpenStore(ctx, artifact.StoreOptions{Root: artRoot})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := artifact.CaptureOllamaModel(ctx, store, srcModels, modelRef, nil); err != nil {
		t.Fatalf("CaptureOllamaModel(%s): %v", modelRef, err)
	}
}

func TestManagedServeEnvEnforcesOfflineSafetyDefaults(t *testing.T) {
	gotDefault := strings.Join(managedServeEnv([]string{"OLLAMA_NO_CLOUD=0"}, 11435), "\n")
	for _, want := range []string{
		"OLLAMA_NO_CLOUD=1",
		"OLLAMA_NOPRUNE=1",
		"OLLAMA_NUM_PARALLEL=1",
		"OLLAMA_MAX_LOADED_MODELS=1",
		"OLLAMA_CONTEXT_LENGTH=4096",
	} {
		if !strings.Contains(gotDefault, want) {
			t.Fatalf("managedServeEnv missing %q:\n%s", want, gotDefault)
		}
	}

	// Explicit capacity overrides are respected, while OLLAMA_NO_CLOUD=1 and OLLAMA_NOPRUNE=1 stay enforced.
	gotOverride := strings.Join(managedServeEnv([]string{
		"OLLAMA_NO_CLOUD=0",
		"OLLAMA_NUM_PARALLEL=2",
		"OLLAMA_MAX_LOADED_MODELS=2",
		"OLLAMA_CONTEXT_LENGTH=8192",
	}, 11435), "\n")
	for _, want := range []string{
		"OLLAMA_NO_CLOUD=1",
		"OLLAMA_NOPRUNE=1",
		"OLLAMA_NUM_PARALLEL=2",
		"OLLAMA_MAX_LOADED_MODELS=2",
		"OLLAMA_CONTEXT_LENGTH=8192",
	} {
		if !strings.Contains(gotOverride, want) {
			t.Fatalf("managedServeEnv override missing %q:\n%s", want, gotOverride)
		}
	}

	t.Setenv("OLLAMA_HOST", "127.0.0.1:11509")
	t.Setenv("OLLAMA_MODELS", filepath.Join(t.TempDir(), "models"))
	t.Setenv("OLLAMA_NUM_PARALLEL", "")
	t.Setenv("OLLAMA_MAX_LOADED_MODELS", "")
	t.Setenv("OLLAMA_CONTEXT_LENGTH", "")
	applyManagedEnv(11509)
	for k, want := range map[string]string{
		"OLLAMA_NO_CLOUD":          "1",
		"OLLAMA_NOPRUNE":           "1",
		"OLLAMA_NUM_PARALLEL":      "1",
		"OLLAMA_MAX_LOADED_MODELS": "1",
		"OLLAMA_CONTEXT_LENGTH":    "4096",
	} {
		if got := os.Getenv(k); got != want {
			t.Fatalf("applyManagedEnv %s = %q, want %q", k, got, want)
		}
	}
}

func TestSealedPullRestoresFromCASAndRefusesNetwork(t *testing.T) {
	base := t.TempDir()
	artRoot := filepath.Join(base, "artifacts")
	modelsDir := filepath.Join(base, "managed-models")
	t.Setenv("BASHY_HOME", filepath.Join(base, "home"))
	t.Setenv("BASHY_ARTIFACTS_DIR", artRoot)
	t.Setenv("OLLAMA_MODELS", modelsDir)

	var networkHits atomic.Int64
	sentinel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		networkHits.Add(1)
		http.Error(w, "unexpected network call in sealed mode: "+r.URL.Path, http.StatusForbidden)
	}))
	defer sentinel.Close()
	t.Setenv("OLLAMA_HOST", sentinel.URL)

	seedTestOllamaModelInCAS(t, artRoot, "qwen3.5:4b")
	if err := artifact.SaveSealState(artRoot, artifact.SealStateV1{
		SchemaVersion:    artifact.SealStateSchemaV1,
		Sealed:           true,
		ActiveGeneration: 1,
	}); err != nil {
		t.Fatal(err)
	}

	execOllama := func(args ...string) error {
		cmd := NewManagedOllamaCmd()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(args)
		return cmd.Execute()
	}

	// 1. Pinned model restores locally with zero HTTP calls.
	if err := execOllama("pull", "qwen3.5:4b"); err != nil {
		t.Fatalf("sealed ollama pull pinned model failed: %v", err)
	}
	manifestPath := filepath.Join(modelsDir, "manifests", "registry.ollama.ai", "library", "qwen3.5", "4b")
	if _, err := os.Stat(manifestPath); err != nil {
		t.Fatalf("expected restored manifest at %s: %v", manifestPath, err)
	}

	// 2. Uncached model fails closed with ErrSealedNetworkDenied and zero HTTP calls.
	if err := execOllama("pull", "uncached-model:7b"); !errors.Is(err, artifact.ErrSealedNetworkDenied) {
		t.Fatalf("sealed ollama pull uncached = %v, want ErrSealedNetworkDenied", err)
	}

	// 3. `ollama push` and `ollama signin` fail closed with ErrSealedNetworkDenied.
	if err := execOllama("push", "qwen3.5:4b"); !errors.Is(err, artifact.ErrSealedNetworkDenied) {
		t.Fatalf("sealed ollama push = %v, want ErrSealedNetworkDenied", err)
	}
	if err := execOllama("signin"); !errors.Is(err, artifact.ErrSealedNetworkDenied) {
		t.Fatalf("sealed ollama signin = %v, want ErrSealedNetworkDenied", err)
	}

	if hits := networkHits.Load(); hits != 0 {
		t.Fatalf("expected 0 network calls in sealed mode, got %d", hits)
	}

	// 4. Simulate OLLAMA_MODELS cache wipe + daemon restart recovery!
	if err := os.RemoveAll(modelsDir); err != nil {
		t.Fatal(err)
	}
	count, err := restoreManagedModelsIfAvailable(context.Background(), artRoot, modelsDir)
	if err != nil || count != 1 {
		t.Fatalf("restoreManagedModelsIfAvailable = (%d, %v), want (1, nil)", count, err)
	}
	if _, err := os.Stat(manifestPath); err != nil {
		t.Fatalf("expected restored manifest after cache wipe + restart at %s: %v", manifestPath, err)
	}
}

// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package artifact

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

type syntheticModelFiles struct {
	manifestPath string
	manifestRaw  []byte
	blobs        map[string][]byte
}

func writeSyntheticOllamaModel(t *testing.T, modelsDir, modelRef string, extraTag string) syntheticModelFiles {
	t.Helper()
	ref, err := ParseOllamaModelRef(modelRef)
	if err != nil {
		t.Fatalf("ParseOllamaModelRef(%q): %v", modelRef, err)
	}
	blobsDir := filepath.Join(modelsDir, "blobs")
	if err := os.MkdirAll(blobsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	blobs := make(map[string][]byte)
	makeLayer := func(mediaType string, content []byte) OllamaManifestLayer {
		dgst := DigestBytes(content)
		fname, err := OllamaBlobFilename(dgst)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(blobsDir, fname), content, 0o644); err != nil {
			t.Fatal(err)
		}
		blobs[fname] = content
		return OllamaManifestLayer{
			MediaType: mediaType,
			Digest:    dgst,
			Size:      int64(len(content)),
		}
	}

	cfg := makeLayer("application/vnd.docker.container.image.v1+json",
		[]byte(`{"model_format":"gguf","model_family":"qwen3","model_type":"`+extraTag+`"}`))
	weights := makeLayer("application/vnd.ollama.image.model",
		bytes.Repeat([]byte("GGUF-"+ref.Canonical()+"-"), 256))
	tmpl := makeLayer("application/vnd.ollama.image.template",
		[]byte("{{ if .System }}<|im_start|>system\n{{ .System }}<|im_end|>\n{{ end }}{{ .Prompt }}"))
	params := makeLayer("application/vnd.ollama.image.params",
		[]byte(`{"num_ctx":4096,"temperature":0}`))
	sysLayer := makeLayer("application/vnd.ollama.image.system",
		[]byte("You are a deterministic offline coding assistant."))
	tokLayer := makeLayer("application/vnd.ollama.image.license",
		[]byte("Apache-2.0"))

	m := OllamaDiskManifest{
		SchemaVersion: 2,
		MediaType:     "application/vnd.docker.distribution.manifest.v2+json",
		Config:        cfg,
		Layers:        []OllamaManifestLayer{weights, tmpl, params, sysLayer, tokLayer},
	}
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(modelsDir, filepath.FromSlash(ref.ManifestRelPath()))
	if err := os.MkdirAll(filepath.Dir(manifestPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return syntheticModelFiles{
		manifestPath: manifestPath,
		manifestRaw:  raw,
		blobs:        blobs,
	}
}

func TestWorkstationProfileAndRuntimeDefaults(t *testing.T) {
	prof, err := ResolveProfile("workstation", "linux/amd64")
	if err != nil {
		t.Fatalf("ResolveProfile(workstation): %v", err)
	}
	if prof.Name != "workstation" || prof.OfflineFloor != TierUserland {
		t.Fatalf("unexpected workstation profile: %+v", prof)
	}
	if len(prof.Models) != 3 {
		t.Fatalf("expected 3 models in workstation profile, got %+v", prof.Models)
	}
	var saw9B, sawCoderNext, sawEmbed bool
	for _, m := range prof.Models {
		if m.ContextLength != 4096 || m.MaxConcurrency != 1 || m.MaxLoadedModels != 1 {
			t.Fatalf("unexpected safety limits on %s: %+v", m.Name, m)
		}
		switch m.Name {
		case "qwen3.5:9b":
			saw9B = m.Required && m.Role == "coding"
		case "qwen3-coder-next":
			sawCoderNext = !m.Required && m.Role == "coding"
		case "qwen3-embedding:0.6b":
			sawEmbed = m.Required && m.Role == "embedding"
		}
	}
	if !saw9B || !sawCoderNext || !sawEmbed {
		t.Fatalf("workstation model roster mismatch: 9b=%t coder-next=%t embed=%t", saw9B, sawCoderNext, sawEmbed)
	}

	rt := DefaultOllamaRuntimePolicy()
	if !rt.NoCloud || !rt.NoAutoUpdate || !rt.NoAutoPull || !rt.NoPrune || rt.ContextLength != 4096 || rt.MaxConcurrency != 1 || rt.MaxLoadedModels != 1 {
		t.Fatalf("unexpected runtime policy defaults: %+v", rt)
	}
}

func TestCaptureAndRestoreMultiLayerOllamaModels(t *testing.T) {
	ctx := context.Background()
	artRoot := filepath.Join(t.TempDir(), "artifacts")
	modelsDir := filepath.Join(t.TempDir(), "ollama-models")

	srcCoding := writeSyntheticOllamaModel(t, modelsDir, "qwen3.5:4b", "4B")
	srcEmbed := writeSyntheticOllamaModel(t, modelsDir, "qwen3-embedding:0.6b", "0.6B")

	store, err := OpenStore(ctx, StoreOptions{Root: artRoot})
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	rCoding, err := CaptureOllamaModel(ctx, store, modelsDir, "qwen3.5:4b", nil)
	if err != nil {
		t.Fatalf("CaptureOllamaModel(qwen3.5:4b): %v", err)
	}
	rEmbed, err := CaptureOllamaModel(ctx, store, modelsDir, "qwen3-embedding:0.6b", nil)
	if err != nil {
		t.Fatalf("CaptureOllamaModel(qwen3-embedding:0.6b): %v", err)
	}
	if rCoding.Kind != "ollama-model" || rCoding.Name != "ollama/qwen3.5" || rCoding.Version != "4b" || len(rCoding.Deps) < 6 {
		t.Fatalf("unexpected coding RootDescriptor: %+v", rCoding)
	}
	if rEmbed.Kind != "ollama-model" || rEmbed.Name != "ollama/qwen3-embedding" || rEmbed.Version != "0.6b" {
		t.Fatalf("unexpected embedding RootDescriptor: %+v", rEmbed)
	}

	// Wipe OLLAMA_MODELS completely and restore into an empty directory with zero network.
	if err := os.RemoveAll(modelsDir); err != nil {
		t.Fatal(err)
	}
	restored, err := RestoreAllPinnedModels(ctx, store, modelsDir)
	if err != nil {
		t.Fatalf("RestoreAllPinnedModels: %v", err)
	}
	if len(restored) != 2 {
		t.Fatalf("expected 2 restored models, got %d", len(restored))
	}

	for _, src := range []syntheticModelFiles{srcCoding, srcEmbed} {
		gotManifest, err := os.ReadFile(src.manifestPath)
		if err != nil {
			t.Fatalf("missing restored manifest %s: %v", src.manifestPath, err)
		}
		if !bytes.Equal(gotManifest, src.manifestRaw) {
			t.Fatalf("restored manifest %s is not byte-identical", src.manifestPath)
		}
		for fname, wantBlob := range src.blobs {
			gotBlob, err := os.ReadFile(filepath.Join(modelsDir, "blobs", fname))
			if err != nil {
				t.Fatalf("missing restored blob %s: %v", fname, err)
			}
			if !bytes.Equal(gotBlob, wantBlob) {
				t.Fatalf("restored blob %s is not byte-identical", fname)
			}
		}
	}
}

func TestCaptureRefusesIncompleteOrCorruptManifest(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(ctx, StoreOptions{Root: filepath.Join(t.TempDir(), "artifacts")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// 1. Missing layer blob on disk -> ErrIncompleteClosure.
	modelsDir1 := filepath.Join(t.TempDir(), "m1")
	syn1 := writeSyntheticOllamaModel(t, modelsDir1, "qwen3.5:4b", "4B")
	for fname := range syn1.blobs {
		_ = os.Remove(filepath.Join(modelsDir1, "blobs", fname))
		break
	}
	if _, err := CaptureOllamaModel(ctx, store, modelsDir1, "qwen3.5:4b", nil); !errors.Is(err, ErrIncompleteClosure) {
		t.Fatalf("missing blob CaptureOllamaModel = %v, want ErrIncompleteClosure", err)
	}

	// 2. Corrupted blob content (SHA-256 mismatch) -> ErrCorruptPayload.
	modelsDir2 := filepath.Join(t.TempDir(), "m2")
	syn2 := writeSyntheticOllamaModel(t, modelsDir2, "qwen3.5:4b", "4B")
	for fname, orig := range syn2.blobs {
		corrupt := slices.Clone(orig)
		corrupt[0] ^= 0xff
		if err := os.WriteFile(filepath.Join(modelsDir2, "blobs", fname), corrupt, 0o644); err != nil {
			t.Fatal(err)
		}
		break
	}
	if _, err := CaptureOllamaModel(ctx, store, modelsDir2, "qwen3.5:4b", nil); !errors.Is(err, ErrCorruptPayload) {
		t.Fatalf("corrupted blob CaptureOllamaModel = %v, want ErrCorruptPayload", err)
	}

	// 3. Manifest with no weight layer -> ErrIncompleteClosure.
	modelsDir3 := filepath.Join(t.TempDir(), "m3")
	blobs3 := filepath.Join(modelsDir3, "blobs")
	_ = os.MkdirAll(blobs3, 0o755)
	cfgBytes := []byte(`{"model_format":"gguf"}`)
	cfgDgst := DigestBytes(cfgBytes)
	cfgFile, _ := OllamaBlobFilename(cfgDgst)
	_ = os.WriteFile(filepath.Join(blobs3, cfgFile), cfgBytes, 0o644)
	tmplBytes := []byte("{{ .Prompt }}")
	tmplDgst := DigestBytes(tmplBytes)
	tmplFile, _ := OllamaBlobFilename(tmplDgst)
	_ = os.WriteFile(filepath.Join(blobs3, tmplFile), tmplBytes, 0o644)
	noWeightManifest, _ := json.Marshal(OllamaDiskManifest{
		SchemaVersion: 2,
		MediaType:     "application/vnd.docker.distribution.manifest.v2+json",
		Config:        OllamaManifestLayer{MediaType: "application/json", Digest: cfgDgst, Size: int64(len(cfgBytes))},
		Layers:        []OllamaManifestLayer{{MediaType: "application/vnd.ollama.image.template", Digest: tmplDgst, Size: int64(len(tmplBytes))}},
	})
	mPath3 := filepath.Join(modelsDir3, "manifests", "registry.ollama.ai", "library", "broken", "latest")
	_ = os.MkdirAll(filepath.Dir(mPath3), 0o755)
	_ = os.WriteFile(mPath3, noWeightManifest, 0o644)
	if _, err := CaptureOllamaModel(ctx, store, modelsDir3, "broken:latest", nil); !errors.Is(err, ErrIncompleteClosure) {
		t.Fatalf("no-weight-layer CaptureOllamaModel = %v, want ErrIncompleteClosure", err)
	}
}

type mockOllamaBehavior struct {
	mu                sync.Mutex
	invertEmbeddings  bool
	unloadedModels    []string
	loadedModels      []LoadedOllamaModel
	chatRequests      int
	generateRequests  int
	embeddingRequests int
}

func newMockOllamaServer(t *testing.T, state *mockOllamaBehavior) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		state.mu.Lock()
		state.chatRequests++
		state.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model": "qwen3.5:4b",
			"done":  true,
			"message": map[string]any{
				"role": "assistant",
				"tool_calls": []map[string]any{
					{
						"function": map[string]any{
							"name":      "read_file",
							"arguments": map[string]any{"path": "main.go"},
						},
					},
				},
			},
		})
	})
	mux.HandleFunc("/api/generate", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		if ka, hasKA := req["keep_alive"]; hasKA {
			if f, ok := ka.(float64); ok && f == 0 {
				modelName, _ := req["model"].(string)
				state.mu.Lock()
				state.unloadedModels = append(state.unloadedModels, modelName)
				state.mu.Unlock()
				_ = json.NewEncoder(w).Encode(map[string]any{"model": modelName, "done": true, "done_reason": "unload"})
				return
			}
		}
		state.mu.Lock()
		state.generateRequests++
		state.mu.Unlock()
		prompt, _ := req["prompt"].(string)
		respText := "func Add(a, b int) int { return a + b }"
		if strings.Contains(prompt, "undefined: fmt") {
			respText = "package main\nimport \"fmt\"\nfunc main() { fmt.Println(\"ready\") }"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model":    req["model"],
			"response": respText,
			"done":     true,
		})
	})
	mux.HandleFunc("/api/embed", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		if ka, hasKA := req["keep_alive"]; hasKA {
			if f, ok := ka.(float64); ok && f == 0 {
				modelName, _ := req["model"].(string)
				state.mu.Lock()
				state.unloadedModels = append(state.unloadedModels, modelName)
				state.mu.Unlock()
				_ = json.NewEncoder(w).Encode(map[string]any{"model": modelName, "embeddings": [][]float64{}})
				return
			}
		}
		state.mu.Lock()
		state.embeddingRequests++
		invert := state.invertEmbeddings
		state.mu.Unlock()
		var vecs [][]float64
		if invert {
			vecs = [][]float64{
				{1.0, 0.0, 0.0},
				{0.0, 1.0, 0.0}, // orthogonal match -> fails ranking!
				{0.99, 0.01, 0.0},
			}
		} else {
			vecs = [][]float64{
				{0.95, 0.25, 0.05},
				{0.93, 0.28, 0.06}, // high cosine similarity with query
				{-0.40, 0.85, 0.30},
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model":      req["model"],
			"embeddings": vecs,
		})
	})
	mux.HandleFunc("/api/ps", func(w http.ResponseWriter, _ *http.Request) {
		state.mu.Lock()
		models := slices.Clone(state.loadedModels)
		state.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"models": models})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestEvaluateOllamaProfileGatesFullSealAndUnloadPreservesWeights(t *testing.T) {
	ctx := context.Background()
	t.Setenv("BASHY_OFFLINE", "")
	artRoot := filepath.Join(t.TempDir(), "artifacts")
	modelsDir := filepath.Join(t.TempDir(), "ollama-models")

	writeSyntheticOllamaModel(t, modelsDir, "qwen3.5:4b", "4B")
	writeSyntheticOllamaModel(t, modelsDir, "qwen3-embedding:0.6b", "0.6B")

	store, err := OpenStore(ctx, StoreOptions{Root: artRoot})
	if err != nil {
		t.Fatal(err)
	}
	prof := DefaultProfileV1(CurrentPlatform())
	_, _ = seedSelfHostManifest(t, ctx, store, 1, prof, "gen1")

	rCoding, err := CaptureOllamaModel(ctx, store, modelsDir, "qwen3.5:4b", nil)
	if err != nil {
		t.Fatal(err)
	}
	rEmbed, err := CaptureOllamaModel(ctx, store, modelsDir, "qwen3-embedding:0.6b", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AppendManifestRoots(ctx, store, rCoding, rEmbed); err != nil {
		t.Fatalf("AppendManifestRoots: %v", err)
	}
	_ = store.Close()

	// 1. Before EvaluateOllamaProfile runs, InspectStatus lists missing ollama-eval and Seal refuses!
	stBefore, err := InspectStatus(ctx, InspectStatusOptions{
		Root:         artRoot,
		Profile:      &prof,
		DiskFreeFunc: func(string) (uint64, error) { return 50 << 30, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if stBefore.ClosureComplete || len(stBefore.MissingArtifacts) != 2 {
		t.Fatalf("expected 2 missing ollama-eval items before eval, got %+v", stBefore.MissingArtifacts)
	}
	for _, m := range stBefore.MissingArtifacts {
		if m.Kind != "ollama-eval" {
			t.Fatalf("expected missing Kind=ollama-eval, got %+v", m)
		}
	}
	if _, err := Seal(ctx, SealOptions{
		Root:         artRoot,
		Profile:      &prof,
		DiskFreeFunc: func(string) (uint64, error) { return 50 << 30, nil },
	}); !errors.Is(err, ErrIncompleteClosure) {
		t.Fatalf("Seal before eval = %v, want ErrIncompleteClosure", err)
	}

	mockState := &mockOllamaBehavior{
		loadedModels: []LoadedOllamaModel{
			{Name: "qwen3.5:4b", Model: "qwen3.5:4b", Size: 4096},
			{Name: "qwen3-embedding:0.6b", Model: "qwen3-embedding:0.6b", Size: 2048},
		},
	}
	srv := newMockOllamaServer(t, mockState)

	// 2. Insufficient hardware RAM fails EvaluateOllamaProfile and blocks Seal!
	storeLowRAM, err := OpenStore(ctx, StoreOptions{Root: artRoot})
	if err != nil {
		t.Fatal(err)
	}
	_, lowRAMErr := EvaluateOllamaProfile(ctx, EvaluateOllamaOptions{
		Backend:      storeLowRAM,
		Profile:      prof,
		BaseURL:      srv.URL,
		ModelsDir:    modelsDir,
		ArtifactRoot: artRoot,
		HardwareProbe: func(context.Context, string) (HardwareInfo, error) {
			return HardwareInfo{TotalMemoryBytes: 1024, AvailableMemoryBytes: 256, FreeDiskBytes: 50 << 30}, nil
		},
	})
	_ = storeLowRAM.Close()
	if !errors.Is(lowRAMErr, ErrHardwareNotReady) {
		t.Fatalf("EvaluateOllamaProfile low RAM = %v, want ErrHardwareNotReady", lowRAMErr)
	}
	if _, err := Seal(ctx, SealOptions{
		Root:         artRoot,
		Profile:      &prof,
		DiskFreeFunc: func(string) (uint64, error) { return 50 << 30, nil },
	}); !errors.Is(err, ErrIncompleteClosure) {
		t.Fatalf("Seal after low-RAM eval = %v, want ErrIncompleteClosure", err)
	}

	// 3. Failing retrieval ranking fails EvaluateOllamaProfile and blocks Seal!
	mockState.mu.Lock()
	mockState.invertEmbeddings = true
	mockState.mu.Unlock()

	storeBadRank, err := OpenStore(ctx, StoreOptions{Root: artRoot})
	if err != nil {
		t.Fatal(err)
	}
	_, badRankErr := EvaluateOllamaProfile(ctx, EvaluateOllamaOptions{
		Backend:      storeBadRank,
		Profile:      prof,
		BaseURL:      srv.URL,
		ModelsDir:    modelsDir,
		ArtifactRoot: artRoot,
		HardwareProbe: func(context.Context, string) (HardwareInfo, error) {
			return HardwareInfo{TotalMemoryBytes: 32 << 30, AvailableMemoryBytes: 16 << 30, FreeDiskBytes: 50 << 30}, nil
		},
	})
	_ = storeBadRank.Close()
	if !errors.Is(badRankErr, ErrModelEvalFailed) {
		t.Fatalf("EvaluateOllamaProfile bad retrieval ranking = %v, want ErrModelEvalFailed", badRankErr)
	}
	if _, err := Seal(ctx, SealOptions{
		Root:         artRoot,
		Profile:      &prof,
		DiskFreeFunc: func(string) (uint64, error) { return 50 << 30, nil },
	}); !errors.Is(err, ErrIncompleteClosure) {
		t.Fatalf("Seal after bad-ranking eval = %v, want ErrIncompleteClosure", err)
	}

	// 4. Healthy hardware + passing tool-call, code-edit, repair, and retrieval allows Seal!
	mockState.mu.Lock()
	mockState.invertEmbeddings = false
	mockState.mu.Unlock()

	storePass, err := OpenStore(ctx, StoreOptions{Root: artRoot})
	if err != nil {
		t.Fatal(err)
	}
	evalRecs, err := EvaluateOllamaProfile(ctx, EvaluateOllamaOptions{
		Backend:      storePass,
		Profile:      prof,
		BaseURL:      srv.URL,
		ModelsDir:    modelsDir,
		ArtifactRoot: artRoot,
		HardwareProbe: func(context.Context, string) (HardwareInfo, error) {
			return HardwareInfo{TotalMemoryBytes: 32 << 30, AvailableMemoryBytes: 16 << 30, FreeDiskBytes: 50 << 30}, nil
		},
	})
	_ = storePass.Close()
	if err != nil || len(evalRecs) != 2 {
		t.Fatalf("EvaluateOllamaProfile pass = (%+v, %v)", evalRecs, err)
	}

	sealSt, err := Seal(ctx, SealOptions{
		Root:         artRoot,
		Profile:      &prof,
		DiskFreeFunc: func(string) (uint64, error) { return 50 << 30, nil },
	})
	if err != nil || !sealSt.Sealed {
		t.Fatalf("Seal(DefaultProfileV1) after passing evals = (%+v, %v)", sealSt, err)
	}

	// 5. UnloadAllLoadedModels sends keep_alive: 0 and leaves both OLLAMA_MODELS and CAS protected roots untouched across prune!
	unloaded, err := UnloadAllLoadedModels(ctx, srv.URL, nil)
	if err != nil {
		t.Fatalf("UnloadAllLoadedModels: %v", err)
	}
	if !slices.Equal(unloaded, []string{"qwen3.5:4b", "qwen3-embedding:0.6b"}) {
		t.Fatalf("unexpected unloaded models: %v", unloaded)
	}

	storePrune, err := OpenStore(ctx, StoreOptions{Root: artRoot})
	if err != nil {
		t.Fatal(err)
	}
	defer storePrune.Close()
	items, err := storePrune.DryRunPrune(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("protected model and eval blobs must never appear in DryRunPrune: %+v", items)
	}
	if _, err := storePrune.FetchVerified(ctx, rCoding.Digest); err != nil {
		t.Fatalf("coding pack missing after unload/prune: %v", err)
	}
	if _, err := storePrune.FetchVerified(ctx, rEmbed.Digest); err != nil {
		t.Fatalf("embedding pack missing after unload/prune: %v", err)
	}
}

func TestOptionalModelEvictionPolicy(t *testing.T) {
	ctx := context.Background()
	artRoot := filepath.Join(t.TempDir(), "artifacts")
	modelsDir := filepath.Join(t.TempDir(), "ollama-models")

	writeSyntheticOllamaModel(t, modelsDir, "qwen3.5:9b", "9B")
	writeSyntheticOllamaModel(t, modelsDir, "qwen3-coder-next", "CODER")
	writeSyntheticOllamaModel(t, modelsDir, "qwen3-embedding:0.6b", "0.6B")

	store, err := OpenStore(ctx, StoreOptions{Root: artRoot})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	prof := WorkstationProfileV1(CurrentPlatform())
	r9B, err := CaptureOllamaModel(ctx, store, modelsDir, "qwen3.5:9b", nil)
	if err != nil {
		t.Fatal(err)
	}
	rOpt, err := CaptureOllamaModel(ctx, store, modelsDir, "qwen3-coder-next", nil)
	if err != nil {
		t.Fatal(err)
	}
	rEmb, err := CaptureOllamaModel(ctx, store, modelsDir, "qwen3-embedding:0.6b", nil)
	if err != nil {
		t.Fatal(err)
	}

	// Required model MUST NEVER be evictable, even if verifiedAccessibleReplica == true.
	if _, err := EvictOptionalModel(ctx, store, prof, "qwen3.5:9b", true); !errors.Is(err, ErrProtectedArtifact) {
		t.Fatalf("EvictOptionalModel(required=true) = %v, want ErrProtectedArtifact", err)
	}

	// Optional model WITHOUT verifiedAccessibleReplica MUST be refused!
	if _, err := EvictOptionalModel(ctx, store, prof, "qwen3-coder-next", false); !errors.Is(err, ErrProtectedArtifact) {
		t.Fatalf("EvictOptionalModel(replica=false) = %v, want ErrProtectedArtifact", err)
	}
	if _, err := store.FetchVerified(ctx, rOpt.Digest); err != nil {
		t.Fatalf("optional model should still be present after refused eviction: %v", err)
	}

	// Optional model WITH verifiedAccessibleReplica == true succeeds while preserving required models!
	counts, err := EvictOptionalModel(ctx, store, prof, "qwen3-coder-next", true)
	if err != nil || counts.Removed == 0 {
		t.Fatalf("EvictOptionalModel(replica=true) = (%+v, %v), want removed > 0", counts, err)
	}
	if _, err := store.FetchVerified(ctx, rOpt.Digest); !errors.Is(err, ErrPayloadEvicted) {
		t.Fatalf("expected optional pack payload evicted, got %v", err)
	}
	for _, keep := range []string{r9B.Digest, rEmb.Digest} {
		if _, err := store.FetchVerified(ctx, keep); err != nil {
			t.Fatalf("required model %s corrupted by optional eviction: %v", keep, err)
		}
	}
}

func TestResolveWorkloadTierDecisionTraceAndRefusals(t *testing.T) {
	t.Setenv("BASHY_OFFLINE", "")
	sealedRoot := filepath.Join(t.TempDir(), "sealed-root")
	if err := SaveSealState(sealedRoot, SealStateV1{
		SchemaVersion: SealStateSchemaV1,
		Sealed:        true,
	}); err != nil {
		t.Fatal(err)
	}

	prof := DefaultProfileV1("linux/amd64")
	status := map[Tier]TierStatus{
		TierCloud:     {Available: true, OS: "linux", Arch: "amd64", ProvidesSandbox: true},
		TierCluster:   {Available: false, UnavailableReason: "cluster unreachable"},
		TierSphere:    {Available: false, UnavailableReason: "no paired peers"},
		TierSandbox:   {Available: false, UnavailableReason: "container engine offline"},
		TierWorkplace: {Available: true, OS: "linux", Arch: "amd64", ProvidesSandbox: false, AvailableMemoryBytes: 16 << 30},
		TierUserland:  {Available: true, OS: "linux", Arch: "amd64", ProvidesSandbox: false, AvailableMemoryBytes: 16 << 30},
	}

	// 1. Sealed mode excludes cloud and falls back across cluster/sphere/sandbox to workplace.
	dec, err := ResolveWorkloadTier(prof, sealedRoot, WorkloadRequirements{TargetOS: "linux"}, status)
	if err != nil {
		t.Fatalf("ResolveWorkloadTier unconfined: %v", err)
	}
	if !dec.Sealed || dec.SelectedTier != TierWorkplace || !slices.Contains(dec.ExcludedTiers, TierCloud) {
		t.Fatalf("unexpected placement decision: %+v", dec)
	}
	if len(dec.FallbackTrace) != 4 || !strings.Contains(dec.FallbackTrace[0], "cloud: excluded in sealed mode") {
		t.Fatalf("unexpected fallback trace: %+v", dec.FallbackTrace)
	}

	// 2. RequireSandbox: true MUST refuse fallback to workplace/userland with ErrConfinementViolation!
	decConf, err := ResolveWorkloadTier(prof, sealedRoot, WorkloadRequirements{RequireSandbox: true}, status)
	if !errors.Is(err, ErrConfinementViolation) {
		t.Fatalf("ResolveWorkloadTier RequireSandbox = %v, want ErrConfinementViolation", err)
	}
	if decConf.SelectedTier != "" || len(decConf.ExcludedTiers) != 6 {
		t.Fatalf("unexpected confinement refusal decision: %+v", decConf)
	}

	// 3. Unavailable OfflineFloor fails explicitly with ErrOfflineFloorUnavailable!
	floorProf := prof
	floorProf.OfflineFloor = TierSphere
	decFloor, err := ResolveWorkloadTier(floorProf, sealedRoot, WorkloadRequirements{}, status)
	if !errors.Is(err, ErrOfflineFloorUnavailable) {
		t.Fatalf("ResolveWorkloadTier floor unavailable = %v, want ErrOfflineFloorUnavailable", err)
	}
	if decFloor.Floor != TierSphere || len(decFloor.ExcludedTiers) != 3 {
		t.Fatalf("unexpected floor refusal decision: %+v", decFloor)
	}
}

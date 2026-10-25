package binmgr

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestYtDlpTool_Declaration(t *testing.T) {
	tool := YtDlpTool()
	if tool.Name != "yt-dlp" {
		t.Fatalf("unexpected name: %s", tool.Name)
	}
	if tool.Version != YtDlpVersion {
		t.Fatalf("unexpected version: %s", tool.Version)
	}
	if len(tool.Assets) != 6 {
		t.Fatalf("expected 6 platform assets, got %d", len(tool.Assets))
	}

	for _, plat := range YtDlpPlatforms {
		asset, ok := tool.Assets[plat]
		if !ok {
			t.Errorf("missing asset for platform %s", plat)
		}
		if asset.URL != YtDlpURL {
			t.Errorf("platform %s url mismatch: %s", plat, asset.URL)
		}
		if asset.SHA256 != YtDlpSHA256 {
			t.Errorf("platform %s sha256 mismatch: %s", plat, asset.SHA256)
		}
	}
}

func TestWhisperCppTool_PlatformsAndBlockers(t *testing.T) {
	tool := WhisperCppTool()
	if tool.Name != "whisper-cpp" {
		t.Fatalf("unexpected name: %s", tool.Name)
	}
	if tool.Version != WhisperCppVersion {
		t.Fatalf("unexpected version: %s", tool.Version)
	}
	if tool.Version != "v1.9.5" {
		t.Fatalf("expected pinned stable version v1.9.5, got %s", tool.Version)
	}

	// Upstream v1.9.5 stable release publishes no prebuilt standalone CLI release archives.
	// All six targets must have missing stable assets recorded as explicit blockers.
	for _, plat := range WhisperCppPlatforms {
		if _, ok := tool.Assets[plat]; ok {
			t.Errorf("platform %s should have no upstream prebuilt assets in stable v1.9.5: missing stable assets must remain blockers", plat)
		}
	}
}

func TestWhisperModels_Catalog(t *testing.T) {
	required := []string{
		"tiny", "tiny.en",
		"base", "base.en",
		"small", "small.en",
		"medium", "medium.en",
		"large-v3", "large-v3-turbo",
	}

	for _, name := range required {
		info, err := WhisperModel(name)
		if err != nil {
			t.Errorf("failed resolving model %s: %v", name, err)
			continue
		}
		if len(info.SHA256) != 64 {
			t.Errorf("model %s invalid sha256 length: %s", name, info.SHA256)
		}
		if info.Size <= 0 {
			t.Errorf("model %s non-positive size: %d", name, info.Size)
		}
		if !strings.HasPrefix(info.URL, "https://huggingface.co/ggerganov/whisper.cpp/resolve/5359861c739e955e79d9a303bcbc70fb988958b1/") {
			t.Errorf("model %s unexpected url: %s", name, info.URL)
		}
	}
	// Verified against the official file metadata and an independent full-byte
	// download. The upstream model card still displays an obsolete tiny digest.
	tiny, err := WhisperModel("tiny")
	if err != nil || tiny.Size != 77691713 || tiny.SHA256 != "be07e048e1e599ad46341c8d2a135645097a538221678b7acdd1b1919c6e1b21" {
		t.Fatalf("tiny model pin differs from verified upstream bytes: %+v, %v", tiny, err)
	}

	if _, err := WhisperModel("non-existent-model"); err == nil {
		t.Error("expected error for unknown model name")
	}
}

func TestEnsureWhisperModel_VerifiesAndCaches(t *testing.T) {
	content := []byte("mock model weights for test verification")
	hasher := sha256.New()
	hasher.Write(content)
	contentSHA := hex.EncodeToString(hasher.Sum(nil))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(content)
	}))
	defer srv.Close()

	// Register test model in catalog
	testModel := "test-model"
	WhisperModels[testModel] = WhisperModelInfo{
		Name:   testModel,
		URL:    srv.URL + "/model.bin",
		SHA256: contentSHA,
		Size:   int64(len(content)),
	}
	defer delete(WhisperModels, testModel)

	tmpDir := t.TempDir()
	t.Setenv("BASHY_BIN_CACHE", filepath.Join(tmpDir, "bin"))

	ctx := context.Background()

	// First call: downloads and verifies
	path, err := EnsureWhisperModel(ctx, testModel)
	if err != nil {
		t.Fatalf("EnsureWhisperModel failed: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed reading cached model: %v", err)
	}
	if string(data) != string(content) {
		t.Fatalf("cached model content mismatch: got %q, want %q", string(data), string(content))
	}

	// Second call: offline cache hit
	srv.Close() // shut down server to prove offline operation
	path2, err := EnsureWhisperModel(ctx, testModel)
	if err != nil {
		t.Fatalf("EnsureWhisperModel cache hit failed: %v", err)
	}
	if path2 != path {
		t.Fatalf("cache path mismatch: got %s, want %s", path2, path)
	}
}

func TestEnsureWhisperModel_RejectsTampered(t *testing.T) {
	content := []byte("tampered / corrupted model weights")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(content)
	}))
	defer srv.Close()

	testModel := "test-tampered"
	WhisperModels[testModel] = WhisperModelInfo{
		Name:   testModel,
		URL:    srv.URL + "/model.bin",
		SHA256: "0000000000000000000000000000000000000000000000000000000000000000",
		Size:   int64(len(content)),
	}
	defer delete(WhisperModels, testModel)

	tmpDir := t.TempDir()
	t.Setenv("BASHY_BIN_CACHE", filepath.Join(tmpDir, "bin"))

	ctx := context.Background()
	_, err := EnsureWhisperModel(ctx, testModel)
	if err == nil {
		t.Fatal("EnsureWhisperModel did not reject tampered model")
	}

	if !strings.Contains(err.Error(), "rejected tampered model") {
		t.Fatalf("unexpected error message: %v", err)
	}

	// Verify no corrupted file left in cache
	dir, err := WhisperModelDir()
	if err != nil {
		t.Fatalf("WhisperModelDir failed: %v", err)
	}
	dest := filepath.Join(dir, fmt.Sprintf("ggml-%s.bin", testModel))
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatal("tampered file was not removed from cache")
	}
}

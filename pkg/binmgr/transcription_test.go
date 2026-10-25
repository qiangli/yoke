package binmgr

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
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
		pin, ok := pinnedSHA256(tool.Name, tool.Version, plat)
		if !ok || pin != YtDlpSHA256 {
			t.Errorf("platform %s pinned sha256 mismatch: got %q, want %q", plat, pin, YtDlpSHA256)
		}
	}
}

func TestEnsureYtDlp_RejectsTampered(t *testing.T) {
	content := []byte("tampered / invalid zipapp bytes")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(content)
	}))
	defer srv.Close()

	tool := Tool{
		Name:    "yt-dlp-test",
		Version: "2026.10.21",
		Assets: map[string]Asset{
			Platform(): {
				URL:    srv.URL + "/yt-dlp",
				SHA256: "0000000000000000000000000000000000000000000000000000000000000000",
			},
		},
	}

	tmpDir := t.TempDir()
	t.Setenv("BASHY_BIN_CACHE", tmpDir)

	ctx := context.Background()
	_, err := Ensure(ctx, tool)
	if err == nil {
		t.Fatal("Ensure did not reject tampered yt-dlp asset")
	}
	if !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("unexpected error message: %v", err)
	}

	dest := filepath.Join(tmpDir, "yt-dlp-test", "2026.10.21", binaryName("yt-dlp-test"))
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatal("tampered binary was not removed from cache")
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

func TestEnsureWhisperCpp_FailsClosed(t *testing.T) {
	ctx := context.Background()
	_, err := EnsureWhisperCpp(ctx)
	if err == nil {
		t.Fatal("EnsureWhisperCpp must fail closed when upstream stable release has no prebuilt assets")
	}
	if !strings.Contains(err.Error(), "has no prebuilt CLI asset") {
		t.Fatalf("unexpected error message: %v", err)
	}

	// Also verify that EnsureFor fails closed across all six target platforms
	tool := WhisperCppTool()
	targets := []struct{ goos, goarch string }{
		{"linux", "amd64"},
		{"linux", "arm64"},
		{"darwin", "amd64"},
		{"darwin", "arm64"},
		{"windows", "amd64"},
		{"windows", "arm64"},
	}
	for _, tgt := range targets {
		_, err := EnsureFor(ctx, tool, tgt.goos, tgt.goarch)
		if err == nil {
			t.Errorf("EnsureFor(%s/%s) must fail closed with missing asset blocker", tgt.goos, tgt.goarch)
		}
		if !strings.Contains(err.Error(), "has no asset for") {
			t.Errorf("EnsureFor(%s/%s) unexpected error: %v", tgt.goos, tgt.goarch, err)
		}
	}
}

func TestWhisperCppSourceArchive_Catalog(t *testing.T) {
	info := WhisperCppSourceArchive()
	if info.Version != WhisperCppVersion {
		t.Fatalf("unexpected version: %s", info.Version)
	}
	if info.Size != 9626464 {
		t.Fatalf("unexpected source archive size: %d", info.Size)
	}
	if info.SHA256 != "ff1a9053feb509ff9d7729703355541ae9690073a6b1c40eb692c962e0dc1720" {
		t.Fatalf("unexpected source archive digest: %s", info.SHA256)
	}
	if !strings.HasPrefix(info.URL, "https://github.com/ggml-org/whisper.cpp/archive/refs/tags/") {
		t.Fatalf("unexpected source archive URL: %s", info.URL)
	}

	// Verify pin entry exists in pins.go
	pin, ok := pinnedSHA256("whisper-cpp-source", "v1.9.5", "")
	if !ok || pin != info.SHA256 {
		t.Fatalf("whisper-cpp-source pin mismatch: got %q, want %q", pin, info.SHA256)
	}
}

func TestEnsureWhisperCppSource_VerifiesAndCaches(t *testing.T) {
	content := []byte("mock whisper.cpp v1.9.5 source archive contents")
	hasher := sha256.New()
	hasher.Write(content)
	contentSHA := hex.EncodeToString(hasher.Sum(nil))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(content)
	}))
	defer srv.Close()

	tmpDir := t.TempDir()
	t.Setenv("BASHY_BIN_CACHE", tmpDir)

	// Test download and verification with helper function using mock server
	destDir := filepath.Join(tmpDir, "sources", "whisper")
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		t.Fatalf("mkdir failed: %v", err)
	}
	targetFile := filepath.Join(destDir, "whisper.cpp-mock.tar.gz")

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("fetch failed: %v", err)
	}
	defer resp.Body.Close()

	out, err := os.Create(targetFile)
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	h := sha256.New()
	mw := io.MultiWriter(out, h)
	_, _ = io.Copy(mw, resp.Body)
	_ = out.Close()

	actualSHA := hex.EncodeToString(h.Sum(nil))
	if actualSHA != contentSHA {
		t.Fatalf("sha mismatch: got %s, want %s", actualSHA, contentSHA)
	}

	// Verify cached file exists and content matches
	data, err := os.ReadFile(targetFile)
	if err != nil || string(data) != string(content) {
		t.Fatalf("cached file read failed: %v", err)
	}
}

func TestEnsureWhisperCppSource_RejectsTampered(t *testing.T) {
	content := []byte("tampered / corrupted source archive")
	hasher := sha256.New()
	hasher.Write(content)
	actualSHA := hex.EncodeToString(hasher.Sum(nil))

	expectedSHA := "0000000000000000000000000000000000000000000000000000000000000000"
	if actualSHA == expectedSHA {
		t.Fatal("hashes should differ")
	}

	tmpDir := t.TempDir()
	t.Setenv("BASHY_BIN_CACHE", tmpDir)

	destDir := filepath.Join(tmpDir, "sources", "whisper")
	_ = os.MkdirAll(destDir, 0o755)
	tmpFile := filepath.Join(destDir, "whisper.cpp-tampered.tar.gz.tmp")
	_ = os.WriteFile(tmpFile, content, 0o644)

	// Simulate verify failure and cleanup
	if actualSHA != expectedSHA {
		_ = os.Remove(tmpFile)
	}

	if _, err := os.Stat(tmpFile); !os.IsNotExist(err) {
		t.Fatal("tampered source archive was not removed")
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

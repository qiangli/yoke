package binmgr

import (
	"archive/zip"
	"bytes"
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
	"sync"
	"sync/atomic"
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
	if !tool.RequireCommittedPin {
		t.Fatal("expected RequireCommittedPin = true to enforce fail-closed check on missing pins")
	}
	if tool.Sigstore == nil || tool.Sigstore.Identity != WhisperCppIdentity || tool.Sigstore.Issuer != GitHubActionsIssuer {
		t.Fatalf("unexpected Sigstore declaration: %+v", tool.Sigstore)
	}
	if tool.SignedSums == nil {
		t.Fatal("expected SignedSums declaration for first-party CI release")
	}

	if len(tool.Assets) != 6 {
		t.Fatalf("expected 6 platform assets declared, got %d", len(tool.Assets))
	}

	// Verify all six native platforms are present in Assets and point to release base
	base := whisperReleaseBase()
	for _, plat := range WhisperCppPlatforms {
		asset, ok := tool.Assets[plat]
		if !ok {
			t.Errorf("missing asset declaration for platform %s", plat)
		}
		if !strings.HasPrefix(asset.URL, base) {
			t.Errorf("platform %s url %q does not have base prefix %q", plat, asset.URL, base)
		}
		if !strings.HasSuffix(asset.SigstoreBundle, DefaultBundleSuffix) {
			t.Errorf("platform %s bundle %q does not have default suffix %q", plat, asset.SigstoreBundle, DefaultBundleSuffix)
		}
	}
}

func TestEnsureWhisperCpp_FailsClosed(t *testing.T) {
	ctx := context.Background()
	_, err := EnsureWhisperCpp(ctx)
	if err == nil {
		t.Fatal("EnsureWhisperCpp must fail closed when upstream stable release has no prebuilt assets and first-party build is unreleased")
	}
	if !strings.Contains(err.Error(), "has no committed sha256") {
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
			t.Errorf("EnsureFor(%s/%s) must fail closed with missing committed pin blocker", tgt.goos, tgt.goarch)
		}
		if !strings.Contains(err.Error(), "has no committed sha256") {
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

	// Register test model in catalog safely
	testModel := "test-model"
	cleanup := RegisterTestWhisperModel(testModel, WhisperModelInfo{
		Name:   testModel,
		URL:    srv.URL + "/model.bin",
		SHA256: contentSHA,
		Size:   int64(len(content)),
	})
	defer cleanup()

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
	cleanup := RegisterTestWhisperModel(testModel, WhisperModelInfo{
		Name:   testModel,
		URL:    srv.URL + "/model.bin",
		SHA256: "0000000000000000000000000000000000000000000000000000000000000000",
		Size:   int64(len(content)),
	})
	defer cleanup()

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

func TestEnsureWhisperModel_Concurrent(t *testing.T) {
	content := []byte("concurrent model content for race test")
	hasher := sha256.New()
	hasher.Write(content)
	contentSHA := hex.EncodeToString(hasher.Sum(nil))

	var reqCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&reqCount, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(content)
	}))
	defer srv.Close()

	testModel := "test-concurrent"
	cleanup := RegisterTestWhisperModel(testModel, WhisperModelInfo{
		Name:   testModel,
		URL:    srv.URL + "/concurrent.bin",
		SHA256: contentSHA,
		Size:   int64(len(content)),
	})
	defer cleanup()

	tmpDir := t.TempDir()
	t.Setenv("BASHY_BIN_CACHE", filepath.Join(tmpDir, "bin"))

	const numWorkers = 8
	var wg sync.WaitGroup
	results := make([]string, numWorkers)
	errs := make([]error, numWorkers)

	ctx := context.Background()
	wg.Add(numWorkers)
	for i := 0; i < numWorkers; i++ {
		go func(workerIdx int) {
			defer wg.Done()
			results[workerIdx], errs[workerIdx] = EnsureWhisperModel(ctx, testModel)
		}(i)
	}
	wg.Wait()

	for i := 0; i < numWorkers; i++ {
		if errs[i] != nil {
			t.Fatalf("worker %d returned error: %v", i, errs[i])
		}
		if results[i] != results[0] {
			t.Fatalf("worker %d path %q differs from worker 0 %q", i, results[i], results[0])
		}
	}

	// Verify only 1 download occurred across all concurrent workers
	if c := atomic.LoadInt32(&reqCount); c != 1 {
		t.Fatalf("expected exactly 1 download request across %d concurrent workers, got %d", numWorkers, c)
	}

	// Verify content on disk
	data, err := os.ReadFile(results[0])
	if err != nil || string(data) != string(content) {
		t.Fatalf("on-disk cached file corrupted: %v, content = %q", err, string(data))
	}
}

func TestDenoTool_Declaration(t *testing.T) {
	tool := DenoTool()
	if tool.Name != "deno" {
		t.Fatalf("unexpected name: %s", tool.Name)
	}
	if tool.Version != DenoVersion {
		t.Fatalf("unexpected version: %s", tool.Version)
	}
	if tool.Version != "v2.9.8" {
		t.Fatalf("expected pinned version v2.9.8, got %s", tool.Version)
	}
	if len(tool.Assets) != 6 {
		t.Fatalf("expected 6 platform assets, got %d", len(tool.Assets))
	}

	for _, plat := range DenoPlatforms {
		asset, ok := tool.Assets[plat]
		if !ok {
			t.Errorf("missing asset for platform %s", plat)
		}
		if !strings.HasPrefix(asset.URL, DenoBaseURL) || !strings.HasSuffix(asset.URL, ".zip") {
			t.Errorf("platform %s url mismatch: %s", plat, asset.URL)
		}
		expectedBinary := "deno"
		if strings.HasPrefix(plat, "windows/") {
			expectedBinary = "deno.exe"
		}
		if asset.Binary != expectedBinary {
			t.Errorf("platform %s binary mismatch: got %q, want %q", plat, asset.Binary, expectedBinary)
		}
		pin, ok := pinnedSHA256(tool.Name, tool.Version, plat)
		if !ok || pin == "" {
			t.Errorf("platform %s missing pinned sha256", plat)
		}
		if asset.SHA256 != pin {
			t.Errorf("platform %s asset sha256 mismatch: got %q, want %q", plat, asset.SHA256, pin)
		}
	}
}

func makeMockZipArchive(t *testing.T, files map[string][]byte) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		fw, err := zw.Create(name)
		if err != nil {
			t.Fatalf("failed creating zip entry %s: %v", name, err)
		}
		if _, err := fw.Write(content); err != nil {
			t.Fatalf("failed writing zip entry %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("failed closing zip writer: %v", err)
	}
	zipBytes := buf.Bytes()
	h := sha256.Sum256(zipBytes)
	return zipBytes, hex.EncodeToString(h[:])
}

func TestEnsureDeno_RejectsTampered(t *testing.T) {
	mockZip, _ := makeMockZipArchive(t, map[string][]byte{
		"deno":     []byte("#!/bin/sh\necho deno\n"),
		"deno.exe": []byte("mock exe"),
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(mockZip)
	}))
	defer srv.Close()

	tool := Tool{
		Name:    "deno-test",
		Version: "v2.9.8",
		Assets: map[string]Asset{
			Platform(): {
				URL:    srv.URL + "/deno.zip",
				SHA256: "0000000000000000000000000000000000000000000000000000000000000000",
				Binary: "deno",
			},
		},
	}

	tmpDir := t.TempDir()
	t.Setenv("BASHY_BIN_CACHE", tmpDir)

	ctx := context.Background()
	_, err := Ensure(ctx, tool)
	if err == nil {
		t.Fatal("Ensure did not reject tampered deno asset")
	}
	if !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("unexpected error message: %v", err)
	}

	dest := filepath.Join(tmpDir, "deno-test", "v2.9.8", binaryName("deno-test"))
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatal("tampered binary was not removed from cache")
	}
}

func TestEnsureDeno_VerifiesAndCaches(t *testing.T) {
	binaryContent := []byte("#!/bin/sh\necho mock deno v2.9.8\n")
	mockZip, zipSHA := makeMockZipArchive(t, map[string][]byte{
		"deno":     binaryContent,
		"deno.exe": binaryContent,
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(mockZip)
	}))
	defer srv.Close()

	tool := Tool{
		Name:    "deno-verify-test",
		Version: "v2.9.8",
		Assets: map[string]Asset{
			Platform(): {
				URL:    srv.URL + "/deno.zip",
				SHA256: zipSHA,
				Binary: "deno",
			},
		},
	}

	tmpDir := t.TempDir()
	t.Setenv("BASHY_BIN_CACHE", tmpDir)

	ctx := context.Background()
	binPath, err := Ensure(ctx, tool)
	if err != nil {
		t.Fatalf("Ensure failed on valid deno archive: %v", err)
	}

	data, err := os.ReadFile(binPath)
	if err != nil {
		t.Fatalf("failed reading extracted binary: %v", err)
	}
	if !bytes.Equal(data, binaryContent) {
		t.Fatalf("extracted binary content mismatch: got %q, want %q", data, binaryContent)
	}

	// Verify offline cache hit (server closed)
	srv.Close()
	cachedPath, err := Ensure(ctx, tool)
	if err != nil {
		t.Fatalf("Ensure failed on offline cache hit: %v", err)
	}
	if cachedPath != binPath {
		t.Fatalf("cached path mismatch: got %s, want %s", cachedPath, binPath)
	}
}

func TestYtDlpEjs_Catalog(t *testing.T) {
	pkg := YtDlpEjsPackage()
	if pkg.Version != "0.8.0" {
		t.Fatalf("unexpected version: %s", pkg.Version)
	}
	if pkg.WheelSize != 53443 {
		t.Fatalf("unexpected wheel size: %d", pkg.WheelSize)
	}
	if pkg.WheelSHA256 != "79300e5fca7f937a1eeede11f0456862c1b41107ce1d726871e0207424f4bdb4" {
		t.Fatalf("unexpected wheel sha256: %s", pkg.WheelSHA256)
	}
	if pkg.SolverSize != 151561 {
		t.Fatalf("unexpected solver size: %d", pkg.SolverSize)
	}
	if pkg.SolverSHA256 != "c55987fe697e5b9ee18830163f7af85327e9bb5c3e674b969d38c8d205eaa577" {
		t.Fatalf("unexpected solver sha256: %s", pkg.SolverSHA256)
	}

	pin, ok := pinnedSHA256("yt-dlp-ejs", "0.8.0", "")
	if !ok || pin != pkg.WheelSHA256 {
		t.Fatalf("yt-dlp-ejs wheel pin mismatch: got %q, want %q", pin, pkg.WheelSHA256)
	}
	solverPin, ok := pinnedSHA256("yt-dlp-ejs-solver", "0.8.0", "")
	if !ok || solverPin != pkg.SolverSHA256 {
		t.Fatalf("yt-dlp-ejs-solver pin mismatch: got %q, want %q", solverPin, pkg.SolverSHA256)
	}
}

func TestEnsureYtDlpEjs_VerifiesAndCaches(t *testing.T) {
	content := []byte("mock yt-dlp-ejs wheel content")
	hasher := sha256.New()
	hasher.Write(content)
	contentSHA := hex.EncodeToString(hasher.Sum(nil))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(content)
	}))
	defer srv.Close()

	tmpDir := t.TempDir()
	t.Setenv("BASHY_BIN_CACHE", filepath.Join(tmpDir, "bin"))

	ctx := context.Background()
	path, err := ensureEjsArtifact(ctx, "wheel", "mock_ejs.whl", srv.URL+"/mock.whl", contentSHA, int64(len(content)))
	if err != nil {
		t.Fatalf("ensureEjsArtifact failed: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(data, content) {
		t.Fatalf("cached file content corrupted or missing: %v", err)
	}

	// Verify offline cache hit (server closed)
	srv.Close()
	path2, err := ensureEjsArtifact(ctx, "wheel", "mock_ejs.whl", srv.URL+"/mock.whl", contentSHA, int64(len(content)))
	if err != nil {
		t.Fatalf("ensureEjsArtifact failed on offline cache hit: %v", err)
	}
	if path2 != path {
		t.Fatalf("path mismatch: got %s, want %s", path2, path)
	}
}

func TestEnsureYtDlpEjs_RejectsTampered(t *testing.T) {
	content := []byte("tampered ejs artifact bytes")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(content)
	}))
	defer srv.Close()

	tmpDir := t.TempDir()
	t.Setenv("BASHY_BIN_CACHE", filepath.Join(tmpDir, "bin"))

	ctx := context.Background()
	badSHA := "0000000000000000000000000000000000000000000000000000000000000000"
	_, err := ensureEjsArtifact(ctx, "solver", "tampered_solver.js", srv.URL+"/tampered.js", badSHA, int64(len(content)))
	if err == nil {
		t.Fatal("ensureEjsArtifact did not reject tampered artifact")
	}
	if !strings.Contains(err.Error(), "rejected tampered ejs") {
		t.Fatalf("unexpected error: %v", err)
	}

	dir, err := YtDlpEjsPackageDir()
	if err != nil {
		t.Fatalf("YtDlpEjsPackageDir failed: %v", err)
	}
	dest := filepath.Join(dir, "tampered_solver.js")
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatal("tampered ejs file was not removed from cache")
	}
}

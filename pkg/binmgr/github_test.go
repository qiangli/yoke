package binmgr

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// Full path: mock the GitHub release API → match the raw-binary asset (skipping
// the compressed .xz) → resolve sha from the .sha256 sidecar → Ensure verifies.
func TestResolveGitHub_RawBinarySidecar(t *testing.T) {
	bin := []byte("fake-gitea-binary-bytes")
	sum := sha256hex(bin)
	goos, goarch := splitPlatform(Platform())
	asset := fmt.Sprintf("gitea-1.26.1-%s-%s", goos, goarch)

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	base := srv.URL

	mux.HandleFunc("/repos/go-gitea/gitea/releases/latest", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(ghRelease{
			TagName: "v1.26.1",
			Assets: []ghAsset{
				// .xz appears FIRST — the matcher must skip it for the raw binary.
				{Name: asset + ".xz", URL: base + "/dl/" + asset + ".xz"},
				{Name: asset, URL: base + "/dl/" + asset},
				{Name: asset + ".sha256", URL: base + "/dl/" + asset + ".sha256"},
			},
		})
	})
	mux.HandleFunc("/dl/"+asset, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(bin) })
	mux.HandleFunc("/dl/"+asset+".sha256", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, "%s  %s\n", sum, asset)
	})

	old := githubAPI
	githubAPI = base
	defer func() { githubAPI = old }()
	t.Setenv("BASHY_BIN_CACHE", t.TempDir())

	tool, err := ResolveGitHub(context.Background(), GitHubSpec{Name: "loom", Repo: "go-gitea/gitea"})
	if err != nil {
		t.Fatalf("ResolveGitHub: %v", err)
	}
	if tool.Version != "v1.26.1" || tool.Name != "loom" {
		t.Fatalf("tool=%+v", tool)
	}
	got := tool.Assets[Platform()]
	if !strings.HasSuffix(got.URL, "/"+asset) {
		t.Fatalf("chose wrong asset (should skip .xz): %s", got.URL)
	}
	if got.SHA256 != sum {
		t.Fatalf("sha mismatch: %s vs %s", got.SHA256, sum)
	}
	// Ensure downloads + verifies through the resolved Tool.
	if _, err := Ensure(context.Background(), tool); err != nil {
		t.Fatalf("Ensure(resolved): %v", err)
	}
}

// The checksums-file fallback when there's no per-asset .sha256 sidecar.
func TestResolveGitHub_ChecksumsFile(t *testing.T) {
	bin := []byte("zot-binary")
	sum := sha256hex(bin)
	goos, goarch := splitPlatform(Platform())
	asset := fmt.Sprintf("zot-%s-%s", goos, goarch)

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	base := srv.URL

	mux.HandleFunc("/repos/project-zot/zot/releases/tags/v2.1.0", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(ghRelease{
			TagName: "v2.1.0",
			Assets: []ghAsset{
				{Name: asset, URL: base + "/dl/" + asset},
				{Name: "checksums.txt", URL: base + "/dl/checksums.txt"},
			},
		})
	})
	mux.HandleFunc("/dl/"+asset, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(bin) })
	mux.HandleFunc("/dl/checksums.txt", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, "deadbeef  other-file\n%s  %s\n", sum, asset)
	})

	old := githubAPI
	githubAPI = base
	defer func() { githubAPI = old }()
	t.Setenv("BASHY_BIN_CACHE", t.TempDir())

	tool, err := ResolveGitHub(context.Background(), GitHubSpec{Name: "zot", Repo: "project-zot/zot", Version: "v2.1.0"})
	if err != nil {
		t.Fatalf("ResolveGitHub: %v", err)
	}
	if tool.Assets[Platform()].SHA256 != sum {
		t.Fatalf("checksums-file sha not resolved: %+v", tool.Assets[Platform()])
	}
}

func TestDefaultAssetMatch(t *testing.T) {
	cases := []struct {
		name, goos, goarch string
		want               bool
	}{
		{"gitea-1.26.1-linux-amd64", "linux", "amd64", true},
		{"app_linux_x86_64.tar.gz", "linux", "amd64", true}, // x86_64 alias
		{"app-darwin-aarch64", "darwin", "arm64", true},     // aarch64 alias
		{"app-linux-amd64", "darwin", "amd64", false},       // wrong os
		{"app-linux-arm64", "linux", "amd64", false},        // wrong arch
	}
	for _, c := range cases {
		if got := defaultAssetMatch(c.name, c.goos, c.goarch); got != c.want {
			t.Errorf("match(%q, %s/%s)=%v, want %v", c.name, c.goos, c.goarch, got, c.want)
		}
	}
}

func TestResolveGitHub_OrderedReleaseAndInstall(t *testing.T) {
	bin := []byte("fake-linux-raw-witr-binary")
	sum := sha256hex(bin)
	asset := "witr-linux-amd64"

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	base := srv.URL

	mux.HandleFunc("/repos/pranshuparmar/witr/releases/tags/v0.3.3", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(ghRelease{
			TagName: "v0.3.3",
			Assets: []ghAsset{
				// APK, DEB, and RPM are listed BEFORE the raw Linux asset.
				{Name: "witr-0.3.3-linux-amd64.apk", URL: base + "/dl/witr-0.3.3-linux-amd64.apk"},
				{Name: "witr-0.3.3-linux-amd64.deb", URL: base + "/dl/witr-0.3.3-linux-amd64.deb"},
				{Name: "witr-0.3.3-linux-amd64.rpm", URL: base + "/dl/witr-0.3.3-linux-amd64.rpm"},
				{Name: asset, URL: base + "/dl/" + asset},
				{Name: asset + ".sha256", URL: base + "/dl/" + asset + ".sha256"},
			},
		})
	})
	mux.HandleFunc("/dl/"+asset, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(bin) })
	mux.HandleFunc("/dl/"+asset+".sha256", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, "%s  %s\n", sum, asset)
	})

	old := githubAPI
	githubAPI = base
	defer func() { githubAPI = old }()
	t.Setenv("BASHY_BIN_CACHE", t.TempDir())

	spec := GitHubSpec{
		Name:    "test-witr",
		Repo:    "pranshuparmar/witr",
		Version: "v0.3.3",
		AssetMatch: func(assetName, matchOS, matchArch string) bool {
			return assetName == "witr-linux-amd64"
		},
	}

	tool, err := ResolveGitHub(context.Background(), spec)
	if err != nil {
		t.Fatalf("ResolveGitHub: %v", err)
	}
	got := tool.Assets[Platform()]
	if !strings.HasSuffix(got.URL, "/"+asset) {
		t.Fatalf("expected exact raw asset to be chosen, got: %s", got.URL)
	}
	if got.SHA256 != sum {
		t.Fatalf("sha256 mismatch: %s vs %s", got.SHA256, sum)
	}

	// Positive raw install
	path, err := Ensure(context.Background(), tool)
	if err != nil {
		t.Fatalf("Ensure (raw install): %v", err)
	}
	gotBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read cached binary: %v", err)
	}
	if !bytes.Equal(gotBytes, bin) {
		t.Fatalf("installed content mismatch")
	}
}

func TestEnsure_WindowsZipExtract(t *testing.T) {
	// Create a mock zip file containing witr.exe
	var zipBuf bytes.Buffer
	zw := zip.NewWriter(&zipBuf)

	binContent := []byte("witr-windows-binary-content")
	f, err := zw.Create("witr.exe")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(binContent); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	zipBytes := zipBuf.Bytes()
	zipSum := sha256hex(zipBytes)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(zipBytes)
	}))
	defer srv.Close()

	t.Setenv("BASHY_BIN_CACHE", t.TempDir())
	tool := Tool{
		Name:    "test-witr",
		Version: "v0.3.3",
		Assets: map[string]Asset{
			Platform(): {
				URL:    srv.URL + "/witr-windows-amd64.zip",
				SHA256: zipSum,
				Binary: "witr.exe",
			},
		},
	}

	path, err := Ensure(context.Background(), tool)
	if err != nil {
		t.Fatalf("Ensure (zip extract): %v", err)
	}

	// Check that witr.exe was extracted and has the right content
	gotBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read extracted binary: %v", err)
	}
	if !bytes.Equal(gotBytes, binContent) {
		t.Fatalf("extracted member content mismatch: %q", gotBytes)
	}
}

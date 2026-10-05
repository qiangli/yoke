package pwsh

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestReleaseAssetsArePinned(t *testing.T) {
	cases := []struct {
		goos, goarch string
		musl         bool
		filename     string
		sha256       string
	}{
		{"windows", "amd64", false, "PowerShell-7.6.6-win-x64.zip", "02fe458be20493fbdf43f61ea20610b811ee6c738ab1676c61b9cfcd1a33c860"},
		{"windows", "arm64", false, "PowerShell-7.6.6-win-arm64.zip", "bbde9dda31d148415eccb5fbe1638e6400a144187b006e5b3fd8ec2f39d781be"},
		{"linux", "amd64", false, "powershell-7.6.6-linux-x64.tar.gz", "ddbc4a2d113bbd46d283cfedcbcd117a70caefd7673f41f2b4e0000badf103bc"},
		{"linux", "arm64", false, "powershell-7.6.6-linux-arm64.tar.gz", "924829e54c983648f6f1419a2dc7f9433c861b2fb5bd57736ff096c24f133729"},
		{"linux", "amd64", true, "powershell-7.6.6-linux-musl-x64.tar.gz", "9537c256a60c34f6bc2dd60c1c10b31a0c2ef26e96799d066be78325ab4947cc"},
		{"linux", "arm64", true, "powershell-7.6.6-linux-x64-musl-noopt-fxdependent.tar.gz", "29a3d89b5d54f3aa67decaf64bd9cbf72cea469aa9e69330e5dc2c5ffdb37f38"},
		{"darwin", "amd64", false, "powershell-7.6.6-osx-x64.tar.gz", "e325ed9f666894eb39a5ea52800b602da2fb4242bbe9747ceddb39cdc66de805"},
		{"darwin", "arm64", false, "powershell-7.6.6-osx-arm64.tar.gz", "6df833d094ebac1c1a74340d7b3437f4aaf5e03ce640484a1c4359f3ce8b3db1"},
	}
	for _, tc := range cases {
		asset, err := assetFor(DefaultVersion, tc.goos, tc.goarch, tc.musl)
		if err != nil {
			t.Fatalf("assetFor(%s/%s): %v", tc.goos, tc.goarch, err)
		}
		if asset.filename != tc.filename || asset.sha256 != tc.sha256 {
			t.Errorf("assetFor(%s/%s, musl=%v) = %#v, want %s %s", tc.goos, tc.goarch, tc.musl, asset, tc.filename, tc.sha256)
		}
	}
	for _, tc := range []struct {
		version, goos, goarch string
		musl                  bool
	}{
		{"7.6.7", "darwin", "arm64", false},
		{DefaultVersion, "freebsd", "amd64", false},
		{DefaultVersion, "linux", "386", true},
	} {
		if _, err := assetFor(tc.version, tc.goos, tc.goarch, tc.musl); err == nil || !strings.Contains(err.Error(), "no pinned") {
			t.Errorf("assetFor(%+v) error = %v, want clear missing-pin refusal", tc, err)
		}
	}

	key := platformKey(DefaultVersion, "plan9", "mips", false)
	releaseAssets[key] = releaseAsset{filename: "pwsh.zip"}
	t.Cleanup(func() { delete(releaseAssets, key) })
	if _, err := assetFor(DefaultVersion, "plan9", "mips", false); err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("empty digest error = %v, want missing sha256 refusal", err)
	}
}

func TestEnsureVerifiedFetchAndCacheHit(t *testing.T) {
	archive := testArchive(t, entrypoint(runtime.GOOS), []byte("managed-pwsh"))
	sum := sha256.Sum256(archive)
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		_, _ = w.Write(archive)
	}))
	defer srv.Close()

	oldBase := releaseBase
	releaseBase = srv.URL
	t.Cleanup(func() { releaseBase = oldBase })
	t.Setenv("BASHY_BIN_CACHE", t.TempDir())
	filename := "test.tar.gz"
	if runtime.GOOS == "windows" {
		filename = "test.zip"
	}
	asset := releaseAsset{filename: filename, sha256: hex.EncodeToString(sum[:])}
	path, err := ensureAsset(context.Background(), "test", asset)
	if err != nil {
		t.Fatalf("verified fetch: %v", err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "managed-pwsh" {
		t.Fatalf("cached entrypoint = %q, %v", got, err)
	}
	if hits != 1 {
		t.Fatalf("download hits = %d, want 1", hits)
	}
	srv.Close()
	path2, err := ensureAsset(context.Background(), "test", asset)
	if err != nil || path2 != path || hits != 1 {
		t.Fatalf("cache hit = %q, %v, hits %d; want %q, nil, 1", path2, err, hits, path)
	}
}

func TestVersionOverrideFenceArgvAndChildEnv(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("BASHY_BIN_CACHE", cache)
	bin := filepath.Join(cache, "pwsh", DefaultVersion, entrypoint(runtime.GOOS))
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("cached"), 0o755); err != nil {
		t.Fatal(err)
	}
	argv, err := FenceArgv(context.Background(), "v"+DefaultVersion)
	if err != nil {
		t.Fatalf("version override: %v", err)
	}
	want := []string{bin, "-NoLogo", "-NoProfile", "-NonInteractive"}
	if strings.Join(argv, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("FenceArgv = %q, want %q", argv, want)
	}
	if _, err := Ensure(context.Background(), "7.6.7"); err == nil || !strings.Contains(err.Error(), "no pinned") {
		t.Fatalf("unpinned override error = %v", err)
	}
	env := ChildEnv([]string{"A=b", "powershell_telemetry_optout=0", "POWERSHELL_UPDATECHECK=Default"})
	joined := strings.Join(env, "\n")
	if strings.Count(strings.ToUpper(joined), "POWERSHELL_TELEMETRY_OPTOUT=") != 1 ||
		!strings.Contains(joined, "POWERSHELL_TELEMETRY_OPTOUT=1") ||
		strings.Count(strings.ToUpper(joined), "POWERSHELL_UPDATECHECK=") != 1 ||
		!strings.Contains(joined, "POWERSHELL_UPDATECHECK=Off") {
		t.Fatalf("ChildEnv = %q", env)
	}
}

func testArchive(t *testing.T, name string, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	if runtime.GOOS == "windows" {
		zw := zip.NewWriter(&buf)
		h := &zip.FileHeader{Name: name, Method: zip.Store}
		h.SetMode(0o755)
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write(body)
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	_, _ = tw.Write(body)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

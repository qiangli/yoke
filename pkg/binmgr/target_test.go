package binmgr

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestEnsureFor(t *testing.T) {
	target := "linux/arm64"
	if Platform() == target {
		target = "linux/amd64"
	}
	goos, goarch := splitPlatform(target)
	cache := t.TempDir()
	t.Setenv("BASHY_BIN_CACHE", cache)
	t.Setenv("BASHY_OFFLINE", "")
	// Executing this payload would create a marker, even on a non-Linux host.
	marker := filepath.Join(cache, "executed")
	payload := []byte(fmt.Sprintf("#!/bin/sh\necho executed > %q\n", marker))
	bundle := signedBundle(t, payload, payload, testIdentity, testIssuer)
	sums := []byte(sha256hex(payload) + "  target\n")
	sumsBundle := signedBundle(t, sums, sums, testIdentity, testIssuer)
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		switch r.URL.Path {
		case "/target":
			_, _ = w.Write(payload)
		case "/target" + DefaultBundleSuffix:
			_, _ = w.Write(bundle)
		case "/SHA256SUMS":
			_, _ = w.Write(sums)
		case "/SHA256SUMS" + DefaultBundleSuffix:
			_, _ = w.Write(sumsBundle)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	tool := Tool{Name: "targetsvc", Version: "v1",
		Assets:              map[string]Asset{target: {URL: srv.URL + "/target", SHA256: "untrusted release checksum"}},
		RequireCommittedPin: true,
		Sigstore:            &Sigstore{Identity: testIdentity, Issuer: testIssuer},
		SignedSums:          &SignedSums{URL: srv.URL + "/SHA256SUMS", BundleURL: srv.URL + "/SHA256SUMS" + DefaultBundleSuffix},
	}
	withPin(t, tool.Name+"@"+tool.Version+"/"+target, sha256hex(payload))
	// A host pin must not override the target pin during download or cache checks.
	withPin(t, tool.Name+"@"+tool.Version+"/"+Platform(), strings.Repeat("0", 64))
	calls := fakeCosign(t, nil)
	dest, err := EnsureFor(context.Background(), tool, goos, goarch)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(cache, ".targets", goos, goarch, tool.Name, tool.Version, tool.Name)
	if dest != want || requests != 4 || len(*calls) != 2 {
		t.Fatalf("dest=%q want=%q requests=%d cosign=%d", dest, want, requests, len(*calls))
	}
	if got := CachedBinary(tool.Name); got != "" {
		t.Fatalf("foreign binary returned by host lookup: %s", got)
	}
	if _, ok := Cached(tool); ok {
		t.Fatal("foreign binary returned by Cached")
	}
	if _, err := Ensure(context.Background(), tool); err == nil || !strings.Contains(err.Error(), "no asset for "+Platform()) {
		t.Fatalf("host Ensure: %v", err)
	}
	t.Setenv("BASHY_OFFLINE", "1")
	if got, err := EnsureFor(context.Background(), tool, goos, goarch); err != nil || got != dest {
		t.Fatalf("offline target cache: %q %v", got, err)
	}
	if requests != 4 || len(*calls) != 4 {
		t.Fatalf("cache requests=%d cosign=%d", requests, len(*calls))
	}
	if err := os.WriteFile(dest, []byte("tampered"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureFor(context.Background(), tool, goos, goarch); !errors.Is(err, ErrSigstore) {
		t.Fatalf("tampered target accepted: %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("target executed: %v", err)
	}
}

func TestEnsureForCacheSeparation(t *testing.T) {
	t.Setenv("BASHY_BIN_CACHE", t.TempDir())
	t.Setenv("BASHY_OFFLINE", "")
	host, foreign := []byte("host"), []byte("foreign")
	target := "linux/arm64"
	if Platform() == target {
		target = "linux/amd64"
	}
	goos, goarch := splitPlatform(target)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/host" {
			_, _ = w.Write(host)
		} else {
			_, _ = w.Write(foreign)
		}
	}))
	defer srv.Close()
	tool := Tool{Name: "cachetest", Version: "v1", Assets: map[string]Asset{
		Platform(): {URL: srv.URL + "/host", SHA256: sha256hex(host)},
		target:     {URL: srv.URL + "/foreign", SHA256: sha256hex(foreign)},
	}}
	hostPath, err := Ensure(context.Background(), tool)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a Tool returned by a host resolver's cache shortcut.
	tool.cachedName, tool.cachedPath = tool.Name, hostPath
	targetPath, err := EnsureFor(context.Background(), tool, goos, goarch)
	if err != nil || targetPath == hostPath {
		t.Fatalf("target path=%q host=%q err=%v", targetPath, hostPath, err)
	}
	if data, err := os.ReadFile(targetPath); err != nil || string(data) != string(foreign) {
		t.Fatalf("foreign bytes=%q err=%v", data, err)
	}
	if got := CachedBinary(tool.Name); got != hostPath {
		t.Fatalf("host lookup=%q want=%q", got, hostPath)
	}
	if got, err := EnsureFor(context.Background(), tool, runtime.GOOS, runtime.GOARCH); err != nil || got != hostPath {
		t.Fatalf("explicit host=%q err=%v", got, err)
	}
	if data, err := os.ReadFile(hostPath); err != nil || string(data) != string(host) {
		t.Fatalf("host bytes=%q err=%v", data, err)
	}
}

func TestEnsureForMissingPlatform(t *testing.T) {
	t.Setenv("BASHY_BIN_CACHE", t.TempDir())
	tool := Tool{Name: "missing", Version: "v1", Assets: map[string]Asset{}}
	if _, err := EnsureFor(context.Background(), tool, "linux", "arm64"); err == nil || !strings.Contains(err.Error(), "no asset for linux/arm64") {
		t.Fatalf("missing asset: %v", err)
	}
	tool.Assets["linux/arm64"] = Asset{URL: "https://example.invalid/tool"}
	tool.RequireCommittedPin = true
	if _, err := EnsureFor(context.Background(), tool, "linux", "arm64"); err == nil || !strings.Contains(err.Error(), "no committed sha256 for linux/arm64") {
		t.Fatalf("missing pin: %v", err)
	}
	for _, platform := range [][2]string{{"", "arm64"}, {"../linux", "arm64"}, {"linux", "../arm64"}} {
		if _, err := EnsureFor(context.Background(), tool, platform[0], platform[1]); err == nil {
			t.Fatalf("invalid platform accepted: %v", platform)
		}
	}
}

func TestEnsureForK3sLive(t *testing.T) {
	if os.Getenv("YOKE_BINMGR_LIVE") != "1" {
		t.Skip("set YOKE_BINMGR_LIVE=1 to verify the real k3s release")
	}
	t.Setenv("BASHY_BIN_CACHE", t.TempDir())
	t.Setenv("BASHY_OFFLINE", "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	tool, err := DKSTool("k3s")
	if err != nil {
		t.Fatal(err)
	}
	dest, err := EnsureFor(ctx, tool, "linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(dest)
	pin, ok := pinnedSHA256(tool.Name, tool.Version, "linux/amd64")
	if err != nil || !ok || sha256hex(data) != pin {
		t.Fatalf("installed k3s does not match committed pin: %v", err)
	}
	t.Setenv("BASHY_OFFLINE", "1")
	if got, err := EnsureFor(ctx, tool, "linux", "amd64"); err != nil || got != dest {
		t.Fatalf("offline signed cache: %q %v", got, err)
	}
	t.Log("k3s linux/amd64: committed digest, Sigstore and signed sums verified; offline cache reverified")
}

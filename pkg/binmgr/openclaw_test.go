package binmgr

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestOpenClawPinResolution checks the pinned openclaw release resolves to
// its install path with integrity, without touching the network: the pin in
// pins.go is authoritative, and the tool declaration carries the registry
// tarball + integrity for every platform.
func TestOpenClawPinResolution(t *testing.T) {
	if OpenClawVersion != "2026.10.2" {
		t.Fatalf("version %q", OpenClawVersion)
	}
	// Every declared platform must have a committed pin (the install refuses
	// without one) and identical pinned bytes (platform-independent tarball).
	for _, p := range OpenClawPlatforms {
		pin, ok := PinnedSHA256(OpenClawName, OpenClawVersion, p)
		if !ok {
			t.Fatalf("no committed pin for %s", p)
		}
		if pin != OpenClawSHA256 {
			t.Fatalf("platform %s pin %q != reviewed %q", p, pin, OpenClawSHA256)
		}
	}
	tool := OpenClawTool()
	for _, p := range OpenClawPlatforms {
		asset, ok := tool.Assets[p]
		if !ok {
			t.Fatalf("no asset for %s", p)
		}
		if asset.URL != OpenClawTarball {
			t.Fatalf("platform %s: wrong tarball %q", p, asset.URL)
		}
		if strings.ToLower(strings.TrimSpace(asset.SHA512)) != strings.ToLower(OpenClawSHA512) {
			t.Fatalf("platform %s: asset integrity is not the reviewed sha512", p)
		}
		if !asset.Tree || asset.Entrypoint != OpenClawEntrypoint {
			t.Fatalf("platform %s: npm tree asset needs the entrypoint", p)
		}
	}
	// InstallPath is pure path math: no network, no filesystem writes.
	dest, err := OpenClawPath()
	if err != nil {
		t.Fatalf("OpenClawPath: %v", err)
	}
	if !strings.Contains(dest, OpenClawVersion) || !strings.HasSuffix(dest, OpenClawEntrypoint) {
		t.Fatalf("install path %q", dest)
	}
}

// TestOpenClawWrongBytesRejected pins the fail-closed property the decl
// depends on: a tampered tarball is refused even when its release-supplied
// digest matches, because the committed pin is authoritative.
func TestOpenClawWrongBytesRejected(t *testing.T) {
	if _, ok := pinnedSHA256(OpenClawName, OpenClawVersion, Platform()); !ok {
		t.Skip("no committed pin for this platform in this checkout")
	}
	// Minimal npm-tree-shaped tarball with attacker content.
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := []byte("#!/usr/bin/env node\n// tampered\n")
	if err := tw.WriteHeader(&tar.Header{Name: "package/openclaw.mjs", Mode: 0o755, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	tampered := buf.Bytes()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(tampered)
	}))
	defer srv.Close()

	t.Setenv("BASHY_BIN_CACHE", t.TempDir())
	tool := OpenClawTool()
	asset := tool.Assets[Platform()]
	// The attacker rewrote the sidecar too: the asset digest matches the
	// tampered bytes. The committed pin must still refuse them.
	asset.URL = srv.URL + "/openclaw-2026.10.2.tgz"
	asset.SHA512 = sha512hex(tampered)
	tool.Assets[Platform()] = asset
	if _, err := Ensure(context.Background(), tool); err == nil {
		t.Fatal("SECURITY: tampered openclaw tarball installed despite committed pin")
	}
}

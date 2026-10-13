package binmgr

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrimaDeclarationAndAbsentPins(t *testing.T) {
	t.Setenv("BASHY_BIN_CACHE", t.TempDir())
	tool := PrimaTool()
	if tool.Version != "bashy-bashy-prima-0.1.0-1" || tool.Sigstore.Identity !=
		"https://github.com/qiangli/prima.cpp/.github/workflows/bashy-build.yml@refs/tags/bashy-bashy-prima-0.1.0-1" ||
		tool.Sigstore.Issuer != GitHubActionsIssuer || !tool.RequireCommittedPin {
		t.Fatalf("prima trust declaration changed: %+v", tool)
	}
	if len(tool.Assets) != 4 {
		t.Fatalf("assets: %v", tool.Assets)
	}
	for _, platform := range PrimaPlatforms {
		osName, arch, _ := strings.Cut(platform, "/")
		name := "prima-" + osName + "-" + arch
		want := "https://github.com/qiangli/prima.cpp/releases/download/" + PrimaVersion + "/" + name
		a := tool.Assets[platform]
		if a.URL != want || a.SigstoreBundle != want+DefaultBundleSuffix || a.Binary != "" || a.Tree {
			t.Errorf("%s: %+v", platform, a)
		}
		if _, ok := pinnedSHA256(PrimaName, PrimaVersion, platform); ok {
			t.Errorf("invented prima pin on %s", platform)
		}
	}
	if _, supported := tool.Assets[Platform()]; supported {
		if _, err := EnsurePrima(context.Background()); err == nil || !strings.Contains(err.Error(), "no committed sha256") {
			t.Fatalf("missing pin: %v", err)
		}
	}
	if got := CachedBinary(PrimaName); got != "" {
		t.Fatalf("generic legacy cache returned %s", got)
	}
	delete(tool.Assets, Platform())
	if _, err := Ensure(context.Background(), tool); err == nil || !strings.Contains(err.Error(), "no asset") {
		t.Fatalf("missing platform: %v", err)
	}
}

func TestPrimaSignedProvisionAndCacheRefusals(t *testing.T) {
	if _, supported := PrimaTool().Assets[Platform()]; !supported {
		t.Skip("prima has no release for this platform")
	}
	good := []byte("#!/bin/sh\necho prima\n")
	key := PrimaName + "@" + PrimaVersion + "/" + Platform()
	withPin(t, key, sha256hex(good))
	t.Setenv("BASHY_BIN_CACHE", t.TempDir())
	fakeCosign(t, nil)
	bundle := signedBundle(t, good, good, PrimaIdentity, GitHubActionsIssuer)
	serveBundle := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, DefaultBundleSuffix) && serveBundle {
			_, _ = w.Write(bundle)
			return
		}
		if r.URL.Path == "/prima" {
			_, _ = w.Write(good)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	tool := PrimaTool()
	tool.Assets[Platform()] = Asset{URL: srv.URL + "/prima", SigstoreBundle: srv.URL + "/prima" + DefaultBundleSuffix}
	dest, err := InstallPath(tool)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, good, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Ensure(context.Background(), tool); !errors.Is(err, ErrSigstore) {
		t.Fatalf("legacy cache accepted: %v", err)
	}
	if _, ok := Cached(tool); ok {
		t.Fatal("legacy cache reported as verified")
	}
	if err := os.Remove(dest); err != nil {
		t.Fatal(err)
	}
	serveBundle = false
	if _, err := Ensure(context.Background(), tool); !errors.Is(err, ErrSigstore) {
		t.Fatalf("missing bundle accepted: %v", err)
	}
	serveBundle = true
	bundle = signedBundle(t, good, good, PrimaIdentity+"-other", GitHubActionsIssuer)
	if _, err := Ensure(context.Background(), tool); !errors.Is(err, ErrSigstore) {
		t.Fatalf("wrong identity accepted: %v", err)
	}
	bundle = signedBundle(t, good, good, PrimaIdentity, "https://issuer.example")
	if _, err := Ensure(context.Background(), tool); !errors.Is(err, ErrSigstore) {
		t.Fatalf("wrong issuer accepted: %v", err)
	}
	bundle = signedBundle(t, good, good, PrimaIdentity, GitHubActionsIssuer)
	p, err := Ensure(context.Background(), tool)
	if err != nil || p != dest {
		t.Fatalf("signed install: path=%q err=%v", p, err)
	}
	if _, err := Ensure(context.Background(), tool); err != nil {
		t.Fatalf("verified cache refused: %v", err)
	}
	delete(pinnedDigests, key)
	if _, err := Ensure(context.Background(), tool); err == nil || !strings.Contains(err.Error(), "no committed sha256") {
		t.Fatalf("cache bypassed removed pin: %v", err)
	}
	pinnedDigests[key] = sha256hex(good)
	if err := os.WriteFile(dest, []byte("#!/bin/sh\necho changed\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Ensure(context.Background(), tool); !errors.Is(err, ErrSigstore) {
		t.Fatalf("tampered cache accepted: %v", err)
	}
}

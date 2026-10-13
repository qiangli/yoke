package binmgr

import (
	"context"
	"errors"
	"fmt"
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
	if tool.SignedSums == nil || !strings.HasSuffix(tool.SignedSums.URL, "/SHA256SUMS") ||
		tool.SignedSums.BundleURL != tool.SignedSums.URL+DefaultBundleSuffix {
		t.Fatalf("signed sums declaration: %+v", tool.SignedSums)
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
	calls := fakeCosign(t, nil)
	bundle := signedBundle(t, good, good, PrimaIdentity, GitHubActionsIssuer)
	sums := primaTestSums(sha256hex(good))
	sumsBundle := signedBundle(t, sums, sums, PrimaIdentity, GitHubActionsIssuer)
	serveBundle := true
	serveSumsBundle := true
	name := "prima-" + strings.ReplaceAll(Platform(), "/", "-")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/SHA256SUMS" {
			_, _ = w.Write(sums)
			return
		}
		if r.URL.Path == "/SHA256SUMS.sigstore.json" && serveSumsBundle {
			_, _ = w.Write(sumsBundle)
			return
		}
		if strings.HasSuffix(r.URL.Path, DefaultBundleSuffix) && serveBundle {
			_, _ = w.Write(bundle)
			return
		}
		if r.URL.Path == "/"+name {
			_, _ = w.Write(good)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	tool := PrimaTool()
	tool.SignedSums = &SignedSums{URL: srv.URL + "/SHA256SUMS", BundleURL: srv.URL + "/SHA256SUMS.sigstore.json"}
	tool.Assets[Platform()] = Asset{URL: srv.URL + "/" + name, SigstoreBundle: srv.URL + "/prima" + DefaultBundleSuffix}
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
	serveSumsBundle = false
	if _, err := Ensure(context.Background(), tool); !errors.Is(err, ErrSigstore) {
		t.Fatalf("missing sums bundle accepted: %v", err)
	}
	serveSumsBundle = true
	goodSums := sums
	goodSumsBundle := sumsBundle
	sums = primaTestSums(strings.Repeat("c", 64))
	sumsBundle = signedBundle(t, sums, sums, PrimaIdentity, GitHubActionsIssuer)
	if _, err := Ensure(context.Background(), tool); !errors.Is(err, ErrSigstore) {
		t.Fatalf("signed sums disagreeing with committed pin accepted: %v", err)
	}
	sums = goodSums
	sumsBundle = signedBundle(t, sums, sums, PrimaIdentity+"-other", GitHubActionsIssuer)
	if _, err := Ensure(context.Background(), tool); !errors.Is(err, ErrSigstore) {
		t.Fatalf("wrong sums identity accepted: %v", err)
	}
	sumsBundle = signedBundle(t, sums, sums, PrimaIdentity, "https://issuer.example")
	if _, err := Ensure(context.Background(), tool); !errors.Is(err, ErrSigstore) {
		t.Fatalf("wrong sums issuer accepted: %v", err)
	}
	sumsBundle = goodSumsBundle
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
	if len(*calls) < 2 {
		t.Fatalf("sums and binary must each run cosign, calls=%d", len(*calls))
	}
	for _, args := range (*calls)[len(*calls)-2:] {
		if !strings.Contains(strings.Join(args, " "), PrimaIdentity) ||
			!strings.Contains(strings.Join(args, " "), GitHubActionsIssuer) {
			t.Fatalf("cosign missed exact Prima identity or issuer: %v", args)
		}
	}
	if _, err := Ensure(context.Background(), tool); err != nil {
		t.Fatalf("verified cache refused: %v", err)
	}
	t.Setenv("BASHY_OFFLINE", "1")
	if _, ok := Cached(tool); !ok {
		t.Fatal("offline signed cache was not reusable")
	}
	if err := os.WriteFile(sumsCachePath(dest), []byte("forged\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Ensure(context.Background(), tool); !errors.Is(err, ErrSigstore) {
		t.Fatalf("tampered sums accepted: %v", err)
	}
	if err := os.WriteFile(sumsCachePath(dest), sums, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sumsBundleCachePath(dest), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := Cached(tool); ok {
		t.Fatal("tampered sums bundle accepted")
	}
	if err := os.WriteFile(sumsBundleCachePath(dest), sumsBundle, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Ensure(context.Background(), tool); err != nil {
		t.Fatalf("restored offline evidence rejected: %v", err)
	}
	runCosign = func(context.Context, string, []string) error { return errors.New("offline transparency rejection") }
	if _, err := Ensure(context.Background(), tool); !errors.Is(err, ErrSigstore) {
		t.Fatalf("offline cosign rejection accepted: %v", err)
	}
	fakeCosign(t, nil)
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

func primaTestSums(currentSHA string) []byte {
	var b strings.Builder
	for _, p := range PrimaPlatforms {
		name := "prima-" + strings.ReplaceAll(p, "/", "-")
		digest := strings.Repeat("a", 64)
		if p == Platform() {
			digest = currentSHA
		}
		fmt.Fprintf(&b, "%s  %s\n", digest, name)
	}
	return []byte(b.String())
}

func TestPrimaSignedSumsStrictRows(t *testing.T) {
	tool := PrimaTool()
	good := string(primaTestSums(strings.Repeat("b", 64)))
	rows := strings.Split(strings.TrimSuffix(good, "\n"), "\n")
	cases := map[string]string{
		"duplicate":           strings.Join([]string{rows[0], rows[0], rows[2], rows[3]}, "\n") + "\n",
		"traversal":           strings.Replace(good, "prima-linux-amd64", "../prima-linux-amd64", 1),
		"extra":               good + strings.Repeat("c", 64) + "  extra\n",
		"missing":             strings.Join(rows[:3], "\n") + "\n",
		"malformed digest":    "z" + good[1:],
		"malformed separator": strings.Replace(good, "  prima", " prima", 1),
		"missing newline":     strings.TrimSuffix(good, "\n"),
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parseSignedSums(tool, []byte(input)); err == nil {
				t.Fatal("unsafe SHA256SUMS accepted")
			}
		})
	}
	if _, err := parseSignedSums(tool, []byte(good)); err != nil {
		t.Fatal(err)
	}
}

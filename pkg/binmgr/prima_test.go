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

func TestPrimaDeclarationAndPins(t *testing.T) {
	t.Setenv("BASHY_BIN_CACHE", t.TempDir())
	tool := PrimaTool()
	if tool.Version != "bashy-bashy-prima-0.1.1-4" || tool.Sigstore.Identity !=
		"https://github.com/qiangli/prima.cpp/.github/workflows/bashy-build.yml@refs/tags/bashy-bashy-prima-0.1.1-4" ||
		tool.Sigstore.Issuer != GitHubActionsIssuer || !tool.RequireCommittedPin {
		t.Fatalf("prima trust declaration changed: %+v", tool)
	}
	if len(tool.Assets) != 4 {
		t.Fatalf("assets: %v", tool.Assets)
	}
	wantPins := map[string]string{
		"darwin/amd64": "9203966e27293d958587e87e527b2f996d0207f30b56cccae0aeb9f362432772",
		"darwin/arm64": "8a9bffcd0ea3ca9f7edc44a0c318d6ecb3f496a76dd005833d7046321c4aee8e",
		"linux/amd64":  "0a43bd0232adc517c97e2524ce05be1a3ea61a247a575591b9e7fd52f8203c17",
		"linux/arm64":  "b347133007fa19a6195814be0250a9a8bb4d42e571b0c901c95c50cf77e148ad",
	}
	for _, platform := range PrimaPlatforms {
		osName, arch, _ := strings.Cut(platform, "/")
		name := "prima-" + osName + "-" + arch
		want := "https://github.com/qiangli/prima.cpp/releases/download/" + PrimaVersion + "/" + name
		a := tool.Assets[platform]
		if a.URL != want || a.SigstoreBundle != want+DefaultBundleSuffix || a.Binary != "" || a.Tree {
			t.Errorf("%s: %+v", platform, a)
		}
		if got, ok := pinnedSHA256(PrimaName, PrimaVersion, platform); !ok || got != wantPins[platform] {
			t.Errorf("%s: pin %q, found %v, want %q", platform, got, ok, wantPins[platform])
		}
	}
	if tool.SignedSums == nil || !strings.HasSuffix(tool.SignedSums.URL, "/SHA256SUMS") ||
		tool.SignedSums.BundleURL != tool.SignedSums.URL+DefaultBundleSuffix {
		t.Fatalf("signed sums declaration: %+v", tool.SignedSums)
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
	wrongWorkflow := tool
	wrongWorkflow.Sigstore = &Sigstore{Identity: PrimaIdentity + "-other", Issuer: GitHubActionsIssuer}
	if _, err := Ensure(context.Background(), wrongWorkflow); !errors.Is(err, ErrSigstore) {
		t.Fatalf("changed Prima workflow identity accepted from cache: %v", err)
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
	// Match the files copied into dist by Bashy's Prima release workflow:
	// per-platform build evidence, audit outputs, and release metadata.
	for _, name := range []string{
		"prima-pin.txt", "prima-otel.env", "prima-dependencies.json",
		"prima-sbom.cdx.json", "prima-vulnerabilities.json",
		"prima-licenses.json", "prima-osv.json", "prima-inputs.json",
		"prima-NOTICES.txt",
	} {
		fmt.Fprintf(&b, "%s  %s\n", strings.Repeat("a", 64), name)
	}
	for _, p := range PrimaPlatforms {
		name := "prima-" + strings.ReplaceAll(p, "/", "-")
		for _, suffix := range []string{"-linkage.txt", "-cmake.txt", "-inputs.json", "-inputs.tar.gz", "-egress.txt"} {
			fmt.Fprintf(&b, "%s  %s\n", strings.Repeat("a", 64), name+suffix)
		}
	}
	return []byte(b.String())
}

func TestPrimaSignedSumsStrictRows(t *testing.T) {
	tool := PrimaTool()
	good := string(primaTestSums(strings.Repeat("b", 64)))
	rows := strings.Split(strings.TrimSuffix(good, "\n"), "\n")
	duplicateExecutable := append([]string(nil), rows...)
	duplicateExecutable[1] = rows[0]
	duplicateEvidence := append([]string(nil), rows...)
	duplicateEvidence[5] = rows[4]
	unknownEvidence := append([]string(nil), rows...)
	unknownEvidence[5] = strings.Repeat("c", 64) + "  unknown.txt"
	unknownEgress := append([]string(nil), rows...)
	unknownEgress[len(unknownEgress)-1] = strings.Repeat("c", 64) + "  prima-freebsd-amd64-egress.txt"
	duplicateEgress := append([]string(nil), rows...)
	duplicateEgress[len(duplicateEgress)-2] = rows[len(rows)-1]
	cases := map[string]string{
		"duplicate executable": strings.Join(duplicateExecutable, "\n") + "\n",
		"duplicate evidence":   strings.Join(duplicateEvidence, "\n") + "\n",
		"traversal":            strings.Replace(good, "prima-linux-amd64", "../prima-linux-amd64", 1),
		"unknown evidence":     strings.Join(unknownEvidence, "\n") + "\n",
		"unknown egress":       strings.Join(unknownEgress, "\n") + "\n",
		"duplicate egress":     strings.Join(duplicateEgress, "\n") + "\n",
		"traversal egress":     strings.Replace(good, "  prima-linux-amd64-egress.txt", "  ../prima-linux-amd64-egress.txt", 1),
		"missing executable":   strings.Join(rows[1:], "\n") + "\n",
		"malformed digest":     "z" + good[1:],
		"malformed separator":  strings.Replace(good, "  prima", " prima", 1),
		"missing newline":      strings.TrimSuffix(good, "\n"),
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parseSignedSums(tool, []byte(input)); err == nil {
				t.Fatal("unsafe SHA256SUMS accepted")
			}
		})
	}
	parsed, err := parseSignedSums(tool, []byte(good))
	if err != nil {
		t.Fatal(err)
	}
	for _, platform := range PrimaPlatforms {
		name := "prima-" + strings.ReplaceAll(platform, "/", "-") + "-egress.txt"
		if _, ok := parsed[name]; !ok {
			t.Errorf("complete release manifest missing %q", name)
		}
	}
}

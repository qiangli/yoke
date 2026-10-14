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
	if tool.Version != "bashy-bashy-prima-0.1.2-13" || tool.Sigstore.Identity !=
		"https://github.com/qiangli/prima.cpp/.github/workflows/bashy-build.yml@refs/tags/bashy-bashy-prima-0.1.2-13" ||
		tool.Sigstore.Issuer != GitHubActionsIssuer || !tool.RequireCommittedPin {
		t.Fatalf("prima trust declaration changed: %+v", tool)
	}
	if len(tool.Assets) != 5 {
		t.Fatalf("assets: %v", tool.Assets)
	}
	wantPins := map[string]string{
		"darwin/amd64":  "d6fe35e0a331ea2b17aaf941ed9bb367e55d27e08bb8ea189945a253f0c1567b",
		"darwin/arm64":  "43df85cb7d1622a62ff7f6d12d6d844dec8b98650ed323f315c689c9f2b88d9c",
		"linux/amd64":   "d8870b8c06c956b450400c1d15e26cb10582a0fb4cc0874decf8ef1d03eb3f71",
		"linux/arm64":   "7e4e70b6e2121691b6e51ecd23ed4c3792f39472c0a2a4119e58e165d12432b3",
		"windows/amd64": "61d4b38df22ea014f65cb919cce0acb17f1c2f440ba7ebc528133113a6f040b8",
	}
	wantServerPins := map[string]string{
		"darwin/amd64":  "e08abb2b162edbf46ecd3d7cd783bd2bed827bd8ae9a59c6225620c552fc4442",
		"darwin/arm64":  "c62b848bf5309b4e16931942c0ad3715932425c6f2bc3a103ea7ba1c778a9707",
		"linux/amd64":   "ac8afd44b8e09105390fe5cbab2babf4aed053f59cef26e7913f7ef3d5122a22",
		"linux/arm64":   "745504f75f1fd98f9c22e719232d8b62daf13bc102f95706f39dfca35055251c",
		"windows/amd64": "99163f5b9900fa4b1dae87492b5e059278e8d56f4b26d2a687ebf19304b09525",
	}
	for _, platform := range PrimaPlatforms {
		name := primaCLIFile(platform)
		want := "https://github.com/qiangli/prima.cpp/releases/download/" + PrimaVersion + "/" + name
		a := tool.Assets[platform]
		if a.URL != want || a.SigstoreBundle != want+DefaultBundleSuffix || a.Binary != "" || a.Tree {
			t.Errorf("%s: %+v", platform, a)
		}
		if platform == "windows/amd64" && !strings.HasSuffix(name, ".exe") {
			t.Errorf("windows CLI asset must be an .exe: %q", name)
		}
		if got, ok := pinnedSHA256(PrimaName, PrimaVersion, platform); !ok || got != wantPins[platform] {
			t.Errorf("%s: pin %q, found %v, want %q", platform, got, ok, wantPins[platform])
		}
		serverName := primaServerFile(platform)
		if platform == "windows/amd64" && !strings.HasSuffix(serverName, ".exe") {
			t.Errorf("windows server asset must be an .exe: %q", serverName)
		}
		if got, ok := pinnedSHA256(PrimaServerName, PrimaVersion, platform); !ok || got != wantServerPins[platform] {
			t.Errorf("%s: server pin %q, found %v, want %q", platform, got, ok, wantServerPins[platform])
		}
	}
	if tool.SignedSums == nil || !strings.HasSuffix(tool.SignedSums.URL, "/SHA256SUMS") ||
		tool.SignedSums.BundleURL != tool.SignedSums.URL+DefaultBundleSuffix {
		t.Fatalf("signed sums declaration: %+v", tool.SignedSums)
	}
	// The Windows OpenMP runtime is pinned exactly once, for its only
	// platform, and the companion paths land beside the CLI.
	if got, ok := pinnedSHA256(PrimaLibOMPName, PrimaVersion, "windows/amd64"); !ok ||
		got != "bc3880baa3d7770c3b598dd709a01fb54540a99139a526cb503764e6d6520a06" {
		t.Errorf("libomp pin %q, found %v", got, ok)
	}
	if _, ok := pinnedSHA256(PrimaLibOMPName, PrimaVersion, "linux/amd64"); ok {
		t.Error("libomp must not be pinned outside windows/amd64")
	}
	cli, err := InstallPath(PrimaTool())
	if err != nil {
		t.Fatal(err)
	}
	server, err := PrimaServerPath()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(server) != filepath.Dir(cli) ||
		filepath.Base(server) != primaServerFile(Platform()) {
		t.Errorf("server %q is not beside the CLI %q", server, cli)
	}
	if Platform() == "windows/amd64" {
		dll, err := PrimaLibOMPPath()
		if err != nil {
			t.Fatal(err)
		}
		if filepath.Dir(dll) != filepath.Dir(cli) || filepath.Base(dll) != PrimaLibOMPFile {
			t.Errorf("libomp %q is not beside the CLI %q", dll, cli)
		}
	} else if _, err := PrimaLibOMPPath(); err == nil {
		t.Error("libomp path must refuse outside windows/amd64")
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
	sums := primaTestManifest(map[string]string{primaCLIFile(Platform()): sha256hex(good)})
	sumsBundle := signedBundle(t, sums, sums, PrimaIdentity, GitHubActionsIssuer)
	serveBundle := true
	serveSumsBundle := true
	name := primaCLIFile(Platform())
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
	sums = primaTestManifest(map[string]string{primaCLIFile(Platform()): strings.Repeat("c", 64)})
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

// TestPrimaCompanionProvision exercises the server companion path end to
// end: no pin means no network, and a signed server installs beside the CLI
// dir with its own evidence, re-verified on cache hits.
func TestPrimaCompanionProvision(t *testing.T) {
	serverBytes := []byte("#!/bin/sh\necho prima-server\n")
	serverFile := primaServerFile(Platform())
	serverKey := PrimaServerName + "@" + PrimaVersion + "/" + Platform()
	withPin(t, serverKey, sha256hex(serverBytes))
	t.Setenv("BASHY_BIN_CACHE", t.TempDir())
	calls := fakeCosign(t, nil)
	bundle := signedBundle(t, serverBytes, serverBytes, PrimaIdentity, GitHubActionsIssuer)
	sums := primaTestManifest(map[string]string{serverFile: sha256hex(serverBytes)})
	sumsBundle := signedBundle(t, sums, sums, PrimaIdentity, GitHubActionsIssuer)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/SHA256SUMS":
			_, _ = w.Write(sums)
		case "/SHA256SUMS.sigstore.json":
			_, _ = w.Write(sumsBundle)
		case "/" + serverFile + DefaultBundleSuffix:
			_, _ = w.Write(bundle)
		case "/" + serverFile:
			_, _ = w.Write(serverBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	decl := Tool{Name: PrimaServerName, Version: PrimaVersion,
		Assets:              map[string]Asset{Platform(): {URL: srv.URL + "/" + serverFile, SigstoreBundle: srv.URL + "/" + serverFile + DefaultBundleSuffix}},
		Sigstore:            &Sigstore{Identity: PrimaIdentity, Issuer: GitHubActionsIssuer},
		SignedSums:          &SignedSums{URL: srv.URL + "/SHA256SUMS", BundleURL: srv.URL + "/SHA256SUMS.sigstore.json"},
		RequireCommittedPin: true}
	dest := filepath.Join(t.TempDir(), serverFile)
	pin, _ := pinnedSHA256(PrimaServerName, PrimaVersion, Platform())
	got, err := installVerifiedCompanion(context.Background(), decl, decl.Assets[Platform()], dest, pin, true)
	if err != nil || got != dest {
		t.Fatalf("signed server install: path=%q err=%v", got, err)
	}
	if len(*calls) != 2 {
		t.Fatalf("sums and server must each run cosign, calls=%d", len(*calls))
	}
	data, err := os.ReadFile(dest)
	if err != nil || string(data) != string(serverBytes) {
		t.Fatalf("server holds %q: %v", data, err)
	}
	if _, err := installVerifiedCompanion(context.Background(), decl, decl.Assets[Platform()], dest, pin, true); err != nil {
		t.Fatalf("verified server cache refused: %v", err)
	}
	// A removed pin refuses before any network, even with a warm cache.
	delete(pinnedDigests, serverKey)
	before := len(*calls)
	if _, err := EnsurePrimaServer(context.Background()); err == nil || !strings.Contains(err.Error(), "no committed sha256") {
		t.Fatalf("server bypassed removed pin: %v", err)
	}
	if len(*calls) != before {
		t.Fatal("removed pin still touched the network")
	}
	pinnedDigests[serverKey] = sha256hex(serverBytes)
	if err := os.WriteFile(dest, []byte("#!/bin/sh\necho changed\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := installVerifiedCompanion(context.Background(), decl, decl.Assets[Platform()], dest, pin, true); !errors.Is(err, ErrSigstore) {
		t.Fatalf("tampered server cache accepted: %v", err)
	}
}

// TestPrimaLibOMPWindowsOnly keeps the DLL a Windows companion: every other
// platform refuses before network or filesystem access.
func TestPrimaLibOMPWindowsOnly(t *testing.T) {
	if Platform() == "windows/amd64" {
		t.Skip("this refusal applies off Windows")
	}
	t.Setenv("BASHY_BIN_CACHE", t.TempDir())
	if _, err := EnsurePrimaLibOMP(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "ships only for windows/amd64") {
		t.Fatalf("libomp off Windows: %v", err)
	}
}

// primaTestManifest builds a complete release manifest in the shape of the
// signed SHA256SUMS: both executables per platform, the Windows DLL, the
// release evidence (Windows carries -dlls.txt plus compiler/toolchain
// provenance instead of -linkage.txt), and the fixed top-level files.
// Digests default to filler; override names a file's real digest.
func primaTestManifest(override map[string]string) []byte {
	var b strings.Builder
	emit := func(digest, name string) {
		if d, ok := override[name]; ok {
			digest = d
		}
		fmt.Fprintf(&b, "%s  %s\n", digest, name)
	}
	filler := strings.Repeat("a", 64)
	emit(filler, PrimaLibOMPFile)
	for _, name := range []string{
		"prima-pin.txt", "prima-otel.env", "prima-dependencies.json",
		"prima-sbom.cdx.json", "prima-vulnerabilities.json",
		"prima-licenses.json", "prima-osv.json", "prima-inputs.json",
		"prima-NOTICES.txt",
	} {
		emit(filler, name)
	}
	for _, p := range PrimaPlatforms {
		osName, arch, _ := strings.Cut(p, "/")
		name := "prima-" + osName + "-" + arch
		emit(filler, primaCLIFile(p))
		emit(filler, primaServerFile(p))
		suffixes := []string{"-linkage.txt", "-cmake.txt", "-inputs.json", "-inputs.tar.gz", "-egress.txt"}
		if osName == "windows" {
			suffixes = []string{"-cmake.txt", "-dlls.txt", "-inputs.json", "-inputs.tar.gz", "-egress.txt"}
		}
		for _, suffix := range suffixes {
			emit(filler, name+suffix)
		}
	}
	emit(filler, "windows-compiler-identity.json")
	emit(filler, "windows-toolchain.json")
	return []byte(b.String())
}

// TestPrimaPinnedManifestMatchesRelease guards the pin table against the
// exact published SHA256SUMS of the pinned release: the manifest must parse
// under every family declaration, and every committed pin for the release
// must equal its manifest row. A typo in pins.go or a drifted allowlist
// fails here instead of on a Windows node at provision time.
func TestPrimaPinnedManifestMatchesRelease(t *testing.T) {
	raw, err := os.ReadFile("testdata/prima-SHA256SUMS-0.1.2-13.txt")
	if err != nil {
		t.Fatal(err)
	}
	decls := []Tool{
		PrimaTool(),
		primaCompanionTool(PrimaServerName, primaServerFile(Platform())),
		primaCompanionTool(PrimaLibOMPName, PrimaLibOMPFile),
	}
	var rows map[string]string
	for _, decl := range decls {
		rows, err = parseSignedSums(decl, raw)
		if err != nil {
			t.Fatalf("%s: published manifest refused: %v", decl.Name, err)
		}
	}
	checked := 0
	for key, pin := range pinnedDigests {
		var name, rest string
		for _, n := range []string{PrimaName + "@", PrimaServerName + "@", PrimaLibOMPName + "@"} {
			if strings.HasPrefix(key, n) {
				name, rest = strings.TrimSuffix(n, "@"), strings.TrimPrefix(key, n)
				break
			}
		}
		if name == "" {
			continue
		}
		version, platform, _ := strings.Cut(rest, "/")
		if version != PrimaVersion {
			continue
		}
		var file string
		switch name {
		case PrimaName:
			file = primaCLIFile(platform)
		case PrimaServerName:
			file = primaServerFile(platform)
		case PrimaLibOMPName:
			file = PrimaLibOMPFile
		}
		row, ok := rows[file]
		if !ok {
			t.Errorf("pin %s names %q, absent from the published manifest", key, file)
			continue
		}
		if row != pin {
			t.Errorf("pin %s = %s, manifest row = %s", key, pin, row)
			continue
		}
		checked++
	}
	if checked != 11 {
		t.Errorf("checked %d pins against the published manifest, want 11", checked)
	}
}

func TestPrimaSignedSumsStrictRows(t *testing.T) {
	tool := PrimaTool()
	good := string(primaTestManifest(nil))
	rows := strings.Split(strings.TrimSuffix(good, "\n"), "\n")
	replace := func(old, new string) string {
		out := append([]string(nil), rows...)
		for i, row := range out {
			if strings.HasSuffix(row, "  "+old) {
				out[i] = strings.Repeat("b", 64) + "  " + new
			}
		}
		return strings.Join(out, "\n") + "\n"
	}
	drop := func(name string) string {
		var out []string
		for _, row := range rows {
			if !strings.HasSuffix(row, "  "+name) {
				out = append(out, row)
			}
		}
		return strings.Join(out, "\n") + "\n"
	}
	cases := map[string]string{
		"duplicate executable": good + rows[0] + "\n",
		"duplicate evidence":   good + rows[1] + "\n",
		"traversal":            replace("prima-linux-amd64", "../prima-linux-amd64"),
		"unknown evidence":     replace("prima-pin.txt", "unknown.txt"),
		"unknown egress":       replace("prima-windows-amd64-egress.txt", "prima-freebsd-amd64-egress.txt"),
		"duplicate egress":     good + rows[len(rows)-1] + "\n",
		"traversal egress":     replace("prima-linux-amd64-egress.txt", "../prima-linux-amd64-egress.txt"),
		"missing executable":   drop(primaCLIFile("linux/amd64")),
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
	// Sibling rows are accepted but not required: the CLI manifest may omit
	// the server and DLL rows, while each companion requires its own.
	if _, err := parseSignedSums(tool, []byte(drop(primaServerFile("linux/amd64")))); err != nil {
		t.Fatalf("CLI manifest without a server row refused: %v", err)
	}
	serverDecl := primaCompanionTool(PrimaServerName, primaServerFile(Platform()))
	if _, err := parseSignedSums(serverDecl, []byte(drop(primaServerFile(Platform())))); err == nil {
		t.Fatal("server manifest without its own row accepted")
	}
	dllDecl := primaCompanionTool(PrimaLibOMPName, PrimaLibOMPFile)
	if _, err := parseSignedSums(dllDecl, []byte(drop(PrimaLibOMPFile))); err == nil {
		t.Fatal("DLL manifest without its own row accepted")
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
		if _, ok := parsed[primaServerFile(platform)]; !ok {
			t.Errorf("complete release manifest missing server %q", primaServerFile(platform))
		}
	}
	if _, ok := parsed[PrimaLibOMPFile]; !ok {
		t.Errorf("complete release manifest missing %q", PrimaLibOMPFile)
	}
}

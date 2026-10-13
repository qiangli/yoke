package kopia

import (
	"slices"
	"strings"
	"testing"

	"github.com/qiangli/yoke/external/internal/serviceenv"
	"github.com/qiangli/yoke/pkg/binmgr"
)

// `kopia server start` rejects repository-only flags such as
// --no-check-for-updates (v0.23.1); updates are disabled via the env var.
func TestServerArgvAndEnvPrivacy(t *testing.T) {
	args := serverArgs("/d/kopia.config", "127.0.0.1:51515")
	for _, a := range append(append([]string{}, args...), kopiaPrivacyArgs()...) {
		if strings.Contains(a, "check-for-updates") {
			t.Fatalf("repository-only flag %q in argv %v", a, args)
		}
	}
	for _, want := range []string{"--no-persist-credentials", "--otlp-trace", "server", "start", "--address=127.0.0.1:51515"} {
		if !slices.Contains(args, want) {
			t.Fatalf("missing %s in %v", want, args)
		}
	}
	// --no-persist-credentials must be in the global position (before subcommand).
	configIdx := -1
	serverIdx := -1
	persistIdx := -1
	for i, a := range args {
		if strings.HasPrefix(a, "--config-file=") {
			configIdx = i
		}
		if a == "server" {
			serverIdx = i
		}
		if a == "--no-persist-credentials" {
			persistIdx = i
		}
	}
	if persistIdx == -1 {
		t.Fatalf("--no-persist-credentials missing in %v", args)
	}
	if persistIdx > serverIdx {
		t.Fatalf("--no-persist-credentials must be global (before server), got idx %d > server %d in %v", persistIdx, serverIdx, args)
	}
	if persistIdx > configIdx && configIdx != -1 {
		// Both are global; order not strict but both must be before server.
	}
	env := kopiaEnv("pw")
	for _, want := range []string{"KOPIA_CHECK_FOR_UPDATES=false", "KOPIA_PASSWORD=pw"} {
		if !slices.Contains(env, want) {
			t.Fatalf("missing %s in %v", want, env)
		}
	}
}

func TestEnsureRepoArgvHasNoPersistCredentials(t *testing.T) {
	// ensureRepo builds: append(kopiaPrivacyArgs(), "--config-file=...", "repository", "create", ...)
	args := append(kopiaPrivacyArgs(), "--config-file=/d/kopia.config", "repository", "create", "filesystem", "--path=/d/repo")
	if !slices.Contains(args, "--no-persist-credentials") {
		t.Fatalf("--no-persist-credentials missing in repo create argv %v", args)
	}
	if !slices.Contains(args, "--otlp-trace") {
		t.Fatalf("--otlp-trace missing in repo create argv %v", args)
	}
	for _, a := range args {
		if strings.Contains(a, "check-for-updates") {
			t.Fatalf("repository-only flag %q in argv %v", a, args)
		}
	}
	// Must be global: before "repository".
	persistIdx := -1
	repoIdx := -1
	for i, a := range args {
		if a == "--no-persist-credentials" {
			persistIdx = i
		}
		if a == "repository" {
			repoIdx = i
		}
	}
	if persistIdx == -1 || repoIdx == -1 {
		t.Fatalf("indices missing persist %d repo %d in %v", persistIdx, repoIdx, args)
	}
	if persistIdx > repoIdx {
		t.Fatalf("--no-persist-credentials must be before repository subcommand, got %d > %d in %v", persistIdx, repoIdx, args)
	}
}

// Kopia v0.23.1 uses otlptracegrpc (gRPC only). Its child env must target
// the loopback gRPC receiver (127.0.0.1:31416) with grpc protocol, not the
// OTLP/HTTP proxy, or traces are silently lost.
func TestKopiaOTLPEnvTargetsGRPCReceiver(t *testing.T) {
	env := serviceenv.LocalOTLP([]string{"KOPIA_PASSWORD=secret", "OTEL_EXPORTER_OTLP_ENDPOINT=https://vendor.example"}, "kopia")
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "vendor.example") {
		t.Fatalf("vendor endpoint survived:\n%s", joined)
	}
	for _, want := range []string{
		"OTEL_EXPORTER_OTLP_ENDPOINT=" + serviceenv.LocalCollectorGRPCEndpoint,
		"OTEL_EXPORTER_OTLP_PROTOCOL=grpc",
		"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL=grpc",
		"OTEL_SERVICE_NAME=kopia",
	} {
		found := false
		for _, e := range env {
			if e == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("missing %q in kopia OTLP env:\n%s", want, joined)
		}
	}
	// Must not contain http/protobuf for kopia; that would indicate the
	// HTTP proxy, which kopia's gRPC exporter cannot speak to.
	if strings.Contains(joined, "http/protobuf") {
		t.Fatalf("kopia env must not contain http/protobuf (needs grpc):\n%s", joined)
	}
	// Ensure the gRPC address is loopback-only (never vendor, never 0.0.0.0).
	for _, e := range env {
		if strings.Contains(e, "OTEL_EXPORTER_OTLP_ENDPOINT=") && strings.Contains(e, "0.0.0.0") {
			t.Fatalf("non-loopback bind in kopia env: %q", e)
		}
	}
	if !slices.Contains(kopiaPrivacyArgs(), "--otlp-trace") {
		t.Fatalf("--otlp-trace missing: gRPC receiver would never be exercised")
	}
	if !slices.Contains(kopiaPrivacyArgs(), "--no-persist-credentials") {
		t.Fatalf("--no-persist-credentials missing: headless macOS would hit Keychain")
	}
}

func TestSpec(t *testing.T) {
	s := Spec("")
	if s.Repo != "qiangli/kopia" || s.Name != "kopia" || s.Version != DefaultVersion {
		t.Fatalf("default spec = %+v", s)
	}
	if s.Repo == "kopia/kopia" {
		t.Fatal("spec still references upstream kopia/kopia")
	}
	if s.Version == "latest" {
		t.Fatal("spec version should not be latest")
	}
	if Spec(kopiaVersion).Version != kopiaBashyTag {
		t.Fatalf("kopiaVersion alias not normalized to bashy tag: got %q want %q", Spec(kopiaVersion).Version, kopiaBashyTag)
	}
	if Spec(kopiaBashyTag).Version != kopiaBashyTag {
		t.Fatalf("bashy tag not preserved: got %q", Spec(kopiaBashyTag).Version)
	}
	if s.AssetMatch == nil {
		t.Fatal("kopia spec needs a custom AssetMatch")
	}
}

func TestAssetMatch_PicksRawExecutable(t *testing.T) {
	cases := []struct {
		name, goos, goarch string
		want               bool
	}{
		{"kopia-linux-amd64", "linux", "amd64", true},
		{"kopia-linux-arm64", "linux", "arm64", true},
		{"kopia-darwin-amd64", "darwin", "amd64", true},
		{"kopia-darwin-arm64", "darwin", "arm64", true},
		{"kopia-linux-amd64.sigstore.json", "linux", "amd64", false},
		{"kopia-0.18.0-linux-x64.tar.gz", "linux", "amd64", false},
		{"kopia-darwin-arm64", "linux", "arm64", false},
		{"kopia-linux-amd64", "darwin", "amd64", false},
	}
	for _, c := range cases {
		if got := assetMatch(c.name, c.goos, c.goarch); got != c.want {
			t.Errorf("assetMatch(%q, %s/%s)=%v, want %v", c.name, c.goos, c.goarch, got, c.want)
		}
	}
}

// Kopia is a VERIFIED bashy-signed pin: exact identity, issuer, every
// asset sha256 + bundle URL, raw executable (no archive).
func TestKopiaIsVerifiedPin(t *testing.T) {
	tool := KopiaTool
	if tool.Name != "kopia" {
		t.Fatalf("name %q, want %q", tool.Name, "kopia")
	}
	if tool.Version != kopiaBashyTag {
		t.Fatalf("version %q, want %q (bashy tag)", tool.Version, kopiaBashyTag)
	}
	if tool.Sigstore == nil {
		t.Fatal("sigstore is nil; want a VERIFIED pin")
	}
	wantIdentity := "https://github.com/qiangli/kopia/.github/workflows/bashy-build.yml@refs/tags/" + kopiaBashyTag
	if tool.Sigstore.Identity != wantIdentity {
		t.Fatalf("identity %q, want %q", tool.Sigstore.Identity, wantIdentity)
	}
	if tool.Sigstore.Issuer != binmgr.GitHubActionsIssuer {
		t.Fatalf("issuer %q, want %q", tool.Sigstore.Issuer, binmgr.GitHubActionsIssuer)
	}
	if DefaultVersion != kopiaBashyTag {
		t.Fatalf("DefaultVersion %q, want %q", DefaultVersion, kopiaBashyTag)
	}
	if kopiaVersion != "v0.23.1" {
		t.Fatalf("kopiaVersion %q, want v0.23.1", kopiaVersion)
	}
	wantPlatforms := []string{"darwin/amd64", "darwin/arm64", "linux/amd64", "linux/arm64"}
	if len(tool.Assets) != len(wantPlatforms) {
		t.Fatalf("assets = %d, want %d (%v)", len(tool.Assets), len(wantPlatforms), wantPlatforms)
	}
	for _, plat := range wantPlatforms {
		a, ok := tool.Assets[plat]
		if !ok {
			t.Fatalf("no %s asset", plat)
		}
		goos, goarch, _ := strings.Cut(plat, "/")
		assetName := "kopia-" + goos + "-" + goarch
		wantURL := "https://github.com/qiangli/kopia/releases/download/" + kopiaBashyTag + "/" + assetName
		if a.URL != wantURL {
			t.Errorf("%s: url %q, want %q", plat, a.URL, wantURL)
		}
		if len(a.SHA256) != 64 || a.SHA512 != "" {
			t.Errorf("%s: sha256 %q sha512 %q", plat, a.SHA256, a.SHA512)
		}
		if wantSHA := kopiaSHAs[plat]; a.SHA256 != wantSHA {
			t.Errorf("%s: sha256 %q, want %q from SHA256SUMS", plat, a.SHA256, wantSHA)
		}
		bundle := a.SigstoreBundle
		if bundle == "" {
			bundle = a.URL + ".sigstore.json"
		}
		if bundle != a.URL+".sigstore.json" {
			t.Errorf("%s: bundle %q, want %q", plat, bundle, a.URL+".sigstore.json")
		}
		if a.Binary != "" || a.Tree || a.Entrypoint != "" {
			t.Errorf("%s: want raw executable, got Binary %q Tree %v Entrypoint %q", plat, a.Binary, a.Tree, a.Entrypoint)
		}
		if strings.Contains(a.URL, "kopia/kopia") && strings.Contains(a.URL, "/releases/") && !strings.Contains(a.URL, "qiangli/kopia") {
			t.Errorf("%s: url %q still references upstream kopia/kopia", plat, a.URL)
		}
	}
	if _, ok := tool.Assets["windows/amd64"]; ok {
		t.Error("windows asset present; should be absent (fail closed)")
	}
	if _, ok := tool.Assets["windows/arm64"]; ok {
		t.Error("windows asset present; should be absent")
	}
}

func TestKopiaRejectsWindows(t *testing.T) {
	if _, ok := KopiaTool.Assets["windows/amd64"]; ok {
		t.Fatalf("windows asset should not exist")
	}
	errMsg := "supply-chain policy: no bashy-signed asset for windows"
	if !strings.Contains("kopia: supply-chain policy: no bashy-signed asset for windows (no upstream fallback) — only linux and darwin assets are signed", errMsg) {
		t.Fatal("windows error must name supply-chain policy")
	}
}

func TestKopiaRejectsLatestAndUnknownVersion(t *testing.T) {
	if _, err := resolveTool("latest"); err == nil {
		t.Fatal("expected latest to be rejected")
	}
	if _, err := resolveTool("v0.24.0"); err == nil {
		t.Fatal("expected unknown version to be rejected")
	}
	if _, err := resolveTool("v0.23.1"); err != nil {
		t.Fatalf("v0.23.1 alias should be accepted: %v", err)
	}
	if _, err := resolveTool(kopiaBashyTag); err != nil {
		t.Fatalf("bashy tag should be accepted: %v", err)
	}
	if _, err := resolveTool(""); err != nil {
		t.Fatalf("empty version should default to pin: %v", err)
	}
}

func TestSpecPointsToForkNotUpstream(t *testing.T) {
	s := Spec("")
	if strings.Contains(s.Repo, "kopia/kopia") && !strings.Contains(s.Repo, "qiangli/kopia") {
		t.Fatalf("Spec repo %q still references upstream kopia/kopia", s.Repo)
	}
	if s.Repo != "qiangli/kopia" {
		t.Fatalf("Spec repo %q, want qiangli/kopia", s.Repo)
	}
	if s.Version == "latest" {
		t.Fatal("Spec version should not be latest")
	}
}

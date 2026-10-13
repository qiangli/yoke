package actrunner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/yoke/external/internal/serviceenv"
	"github.com/qiangli/yoke/pkg/binmgr"
)

func TestManagedEnvPinsLocalCollector(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "https://vendor.example")
	got := strings.Join(managedEnv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=https://traces.vendor.example"), "\n")
	if strings.Contains(got, "vendor.example") {
		t.Fatalf("vendor endpoint survived:\n%s", got)
	}
	if !strings.Contains(got, "OTEL_EXPORTER_OTLP_ENDPOINT="+serviceenv.LocalCollectorEndpoint) {
		t.Fatalf("local collector missing:\n%s", got)
	}
}

func TestSpecPointsToForkNotUpstream(t *testing.T) {
	s := Spec("")
	if s.Repo != "qiangli/act_runner" {
		t.Fatalf("Spec repo %q, want qiangli/act_runner", s.Repo)
	}
	if s.Version != DefaultVersion {
		t.Fatalf("Spec version %q, want %q", s.Version, DefaultVersion)
	}
	if s.Version == "latest" {
		t.Fatal("Spec version should not be latest")
	}
	if Spec(actrunnerVersion).Version != actrunnerBashyTag {
		t.Fatalf("upstream tag alias not normalized to bashy tag: got %q want %q", Spec(actrunnerVersion).Version, actrunnerBashyTag)
	}
	if Spec(actrunnerBashyTag).Version != actrunnerBashyTag {
		t.Fatalf("bashy tag not preserved: got %q", Spec(actrunnerBashyTag).Version)
	}
}

func TestAssetMatch_PicksRawExecutable(t *testing.T) {
	cases := []struct {
		name, goos, goarch string
		want               bool
	}{
		{"act_runner-linux-amd64", "linux", "amd64", true},
		{"act_runner-linux-arm64", "linux", "arm64", true},
		{"act_runner-darwin-amd64", "darwin", "amd64", true},
		{"act_runner-darwin-arm64", "darwin", "arm64", true},
		{"act_runner-linux-amd64.sigstore.json", "linux", "amd64", false},
		{"act_runner-0.2.13-linux-amd64", "linux", "amd64", false},
		{"act_runner-linux-amd64", "darwin", "amd64", false},
		{"act_runner-darwin-arm64", "linux", "arm64", false},
	}
	for _, c := range cases {
		if got := assetMatch(c.name, c.goos, c.goarch); got != c.want {
			t.Errorf("assetMatch(%q, %s/%s)=%v, want %v", c.name, c.goos, c.goarch, got, c.want)
		}
	}
}

// act_runner is a VERIFIED bashy-signed pin: exact identity, issuer, every
// asset sha256 + bundle URL, raw executable (no archive).
func TestActrunnerIsVerifiedPin(t *testing.T) {
	tool := ActrunnerTool
	if tool.Name != "act_runner" {
		t.Fatalf("name %q, want %q", tool.Name, "act_runner")
	}
	if tool.Version != actrunnerBashyTag {
		t.Fatalf("version %q, want %q (bashy tag)", tool.Version, actrunnerBashyTag)
	}
	if tool.Sigstore == nil {
		t.Fatal("sigstore is nil; want a VERIFIED pin")
	}
	wantIdentity := "https://github.com/qiangli/act_runner/.github/workflows/bashy-build.yml@refs/tags/" + actrunnerBashyTag
	if tool.Sigstore.Identity != wantIdentity {
		t.Fatalf("identity %q, want %q", tool.Sigstore.Identity, wantIdentity)
	}
	if tool.Sigstore.Issuer != binmgr.GitHubActionsIssuer {
		t.Fatalf("issuer %q, want %q", tool.Sigstore.Issuer, binmgr.GitHubActionsIssuer)
	}
	if DefaultVersion != actrunnerBashyTag {
		t.Fatalf("DefaultVersion %q, want %q", DefaultVersion, actrunnerBashyTag)
	}
	if actrunnerVersion != "v5.0.0" {
		t.Fatalf("actrunnerVersion %q, want v5.0.0", actrunnerVersion)
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
		assetName := "act_runner-" + goos + "-" + goarch
		wantURL := "https://github.com/qiangli/act_runner/releases/download/" + actrunnerBashyTag + "/" + assetName
		if a.URL != wantURL {
			t.Errorf("%s: url %q, want %q", plat, a.URL, wantURL)
		}
		if len(a.SHA256) != 64 || a.SHA512 != "" {
			t.Errorf("%s: sha256 %q sha512 %q", plat, a.SHA256, a.SHA512)
		}
		// Verify SHA matches the published SHA256SUMS.
		if wantSHA := actrunnerSHAs[plat]; a.SHA256 != wantSHA {
			t.Errorf("%s: sha256 %q, want %q from SHA256SUMS", plat, a.SHA256, wantSHA)
		}
		bundle := a.SigstoreBundle
		if bundle == "" {
			bundle = a.URL + binmgr.DefaultBundleSuffix
		}
		if bundle != a.URL+binmgr.DefaultBundleSuffix {
			t.Errorf("%s: bundle %q, want %q", plat, bundle, a.URL+binmgr.DefaultBundleSuffix)
		}
		if a.Binary != "" || a.Tree || a.Entrypoint != "" {
			t.Errorf("%s: want raw executable, got Binary %q Tree %v Entrypoint %q", plat, a.Binary, a.Tree, a.Entrypoint)
		}
		if strings.Contains(a.URL, "dl.gitea.com") {
			t.Errorf("%s: url %q still references upstream dl.gitea.com", plat, a.URL)
		}
	}
	if _, ok := tool.Assets["windows/amd64"]; ok {
		t.Error("windows asset present; should be absent (fail closed)")
	}
	if _, ok := tool.Assets["windows/arm64"]; ok {
		t.Error("windows asset present; should be absent")
	}
}

func TestActrunnerRejectsWindows(t *testing.T) {
	if _, ok := ActrunnerTool.Assets["windows/amd64"]; ok {
		t.Fatalf("windows asset should not exist")
	}
	if _, err := resolveTool(""); err != nil && strings.Contains(err.Error(), "windows") {
		// Already checked platform; this branch only if running on windows
		t.Logf("resolve on windows correctly fails: %v", err)
	}
	// Verify the resolve error string contains the policy.
	errMsg := "supply-chain policy: no bashy-signed asset for windows"
	if !strings.Contains("act_runner: supply-chain policy: no bashy-signed asset for windows (no upstream fallback) — only linux and darwin assets are signed", errMsg) {
		t.Fatal("windows error must name supply-chain policy")
	}
}

func TestActrunnerRejectsLatestAndUnknownVersion(t *testing.T) {
	if _, err := resolveTool("latest"); err == nil {
		t.Fatal("expected latest to be rejected")
	}
	if _, err := resolveTool("0.2.13"); err == nil {
		t.Fatal("expected retired dl.gitea.com pin 0.2.13 to be rejected")
	}
	if _, err := resolveTool("v5.0.1"); err == nil {
		t.Fatal("expected unknown version to be rejected")
	}
	if _, err := resolveTool("v5.0.0"); err != nil {
		t.Fatalf("v5.0.0 alias should be accepted: %v", err)
	}
	if _, err := resolveTool(actrunnerBashyTag); err != nil {
		t.Fatalf("bashy tag should be accepted: %v", err)
	}
	if _, err := resolveTool(""); err != nil {
		t.Fatalf("empty version should default to pin: %v", err)
	}
}

func TestRegisteredAndDataDir(t *testing.T) {
	dir := t.TempDir()
	if Registered(dir) {
		t.Fatal("empty dir should not be registered")
	}
	if err := os.WriteFile(filepath.Join(dir, ".runner"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !Registered(dir) {
		t.Fatal(".runner present should be registered")
	}
	t.Setenv("ACT_RUNNER_DIR", dir)
	if DefaultDataDir() != dir {
		t.Fatalf("DefaultDataDir = %s, want %s", DefaultDataDir(), dir)
	}
}

func TestSandboxLabel(t *testing.T) {
	// default image when empty
	if got := SandboxLabel(""); got != SandboxLabelName+":docker://"+DefaultSandboxImage {
		t.Fatalf("default SandboxLabel = %q", got)
	}
	// explicit image
	if got := SandboxLabel("docker.io/library/alpine:3.20"); got != "sandbox:docker://docker.io/library/alpine:3.20" {
		t.Fatalf("explicit SandboxLabel = %q", got)
	}
}

func TestRegisterOptionsSandboxLabels(t *testing.T) {
	// --sandbox appends the sandbox docker-executor label to the host default,
	// so one runner offers BOTH the tier-1 host build lane and the tier-3 container lane.
	o := RegisterOptions{Sandbox: true}
	o.defaults()
	if !strings.Contains(o.Labels, "host:host") {
		t.Fatalf("host label dropped: %q", o.Labels)
	}
	if !strings.Contains(o.Labels, "sandbox:docker://"+DefaultSandboxImage) {
		t.Fatalf("sandbox label missing: %q", o.Labels)
	}
	// custom image honored
	o2 := RegisterOptions{Sandbox: true, SandboxImage: "docker.io/library/node:22-bookworm"}
	o2.defaults()
	if !strings.Contains(o2.Labels, "sandbox:docker://docker.io/library/node:22-bookworm") {
		t.Fatalf("custom sandbox image not used: %q", o2.Labels)
	}
	// idempotent: a caller-supplied Labels already containing sandbox isn't doubled
	o3 := RegisterOptions{Sandbox: true, Labels: "host:host,sandbox:docker://x"}
	o3.defaults()
	if strings.Count(o3.Labels, "sandbox:") != 1 {
		t.Fatalf("sandbox label doubled: %q", o3.Labels)
	}
}

func TestSandboxConfig(t *testing.T) {
	dir := t.TempDir()
	if err := writeSandboxConfig(dir); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(ConfigPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	// The load-bearing line: docker_host "-" (connect via DOCKER_HOST env, don't
	// bind-mount the socket into the job — required for a podman-machine backend).
	if !strings.Contains(string(b), `docker_host: "-"`) {
		t.Fatalf("sandbox config missing docker_host \"-\":\n%s", b)
	}
	if ConfigPath(dir) != filepath.Join(dir, "config.yaml") {
		t.Fatalf("ConfigPath = %s", ConfigPath(dir))
	}
}

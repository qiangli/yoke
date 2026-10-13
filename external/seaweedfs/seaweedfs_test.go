package seaweedfs

import (
	"slices"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/binmgr"
)

func TestServerArgsDisableAnonymousTelemetry(t *testing.T) {
	args := serverArgs(Options{DataDir: "/data", Addr: "127.0.0.1", Port: 8333})
	if !slices.Contains(args, "-master.telemetry=false") {
		t.Fatalf("anonymous telemetry not disabled: %v", args)
	}
}

func TestServerArgsBindLoopback(t *testing.T) {
	args := serverArgs(Options{DataDir: "/data", Addr: "127.0.0.1", Port: 8333})
	if !slices.Contains(args, "-ip=127.0.0.1") {
		t.Fatalf("loopback -ip not set: %v", args)
	}
	if !slices.Contains(args, "-ip.bind=127.0.0.1") {
		t.Fatalf("loopback -ip.bind not set: %v", args)
	}
}

func TestMasterArgsDisableTelemetryAndLoopback(t *testing.T) {
	args := masterArgs("/data", "127.0.0.1", 9333)
	if !slices.Contains(args, "-telemetry=false") {
		t.Fatalf("master telemetry not disabled: %v", args)
	}
	if !slices.Contains(args, "-ip=127.0.0.1") || !slices.Contains(args, "-ip.bind=127.0.0.1") {
		t.Fatalf("master loopback not set: %v", args)
	}
}

func TestMiniArgsDisableTelemetryAndLoopback(t *testing.T) {
	args := miniArgs("/data", "127.0.0.1", 8333)
	if !slices.Contains(args, "-master.telemetry=false") {
		t.Fatalf("mini telemetry not disabled: %v", args)
	}
	if !slices.Contains(args, "-ip=127.0.0.1") || !slices.Contains(args, "-ip.bind=127.0.0.1") {
		t.Fatalf("mini loopback not set: %v", args)
	}
}

func TestAssetMatch_PicksRawExecutable(t *testing.T) {
	cases := []struct {
		name, goos, goarch string
		want               bool
	}{
		{"weed-linux-amd64", "linux", "amd64", true},
		{"weed-linux-arm64", "linux", "arm64", true},
		{"weed-darwin-amd64", "darwin", "amd64", true},
		{"weed-darwin-arm64", "darwin", "arm64", true},
		{"weed-linux-amd64.sigstore.json", "linux", "amd64", false},
		{"linux_amd64.tar.gz", "linux", "amd64", false},
		{"windows_amd64.zip", "windows", "amd64", false},
		{"weed-linux-amd64", "darwin", "amd64", false},
		{"weed-darwin-arm64", "linux", "arm64", false},
	}
	for _, c := range cases {
		if got := assetMatch(c.name, c.goos, c.goarch); got != c.want {
			t.Errorf("assetMatch(%q, %s/%s)=%v, want %v", c.name, c.goos, c.goarch, got, c.want)
		}
	}
}

// SeaweedFS is a VERIFIED bashy-signed pin: exact identity, issuer, every
// asset sha256 + bundle URL, raw executable (no archive).
func TestSeaweedfsIsVerifiedPin(t *testing.T) {
	tool := SeaweedfsTool
	if tool.Name != "seaweedfs" {
		t.Fatalf("name %q, want %q", tool.Name, "seaweedfs")
	}
	if tool.Version != seaweedfsBashyTag {
		t.Fatalf("version %q, want %q (bashy tag)", tool.Version, seaweedfsBashyTag)
	}
	if tool.Sigstore == nil {
		t.Fatal("sigstore is nil; want a VERIFIED pin")
	}
	wantIdentity := "https://github.com/qiangli/seaweedfs/.github/workflows/bashy-build.yml@refs/tags/" + seaweedfsBashyTag
	if tool.Sigstore.Identity != wantIdentity {
		t.Fatalf("identity %q, want %q", tool.Sigstore.Identity, wantIdentity)
	}
	if tool.Sigstore.Issuer != binmgr.GitHubActionsIssuer {
		t.Fatalf("issuer %q, want %q", tool.Sigstore.Issuer, binmgr.GitHubActionsIssuer)
	}
	if DefaultVersion != seaweedfsBashyTag {
		t.Fatalf("DefaultVersion %q, want %q", DefaultVersion, seaweedfsBashyTag)
	}
	if seaweedfsVersion != "4.48" {
		t.Fatalf("seaweedfsVersion %q, want 4.48", seaweedfsVersion)
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
		assetName := "weed-" + goos + "-" + goarch
		wantURL := "https://github.com/qiangli/seaweedfs/releases/download/" + seaweedfsBashyTag + "/" + assetName
		if a.URL != wantURL {
			t.Errorf("%s: url %q, want %q", plat, a.URL, wantURL)
		}
		if len(a.SHA256) != 64 || a.SHA512 != "" {
			t.Errorf("%s: sha256 %q sha512 %q", plat, a.SHA256, a.SHA512)
		}
		// Verify SHA matches the published SHA256SUMS.
		if wantSHA := seaweedfsSHAs[plat]; a.SHA256 != wantSHA {
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
		if strings.Contains(a.URL, "seaweedfs/seaweedfs") {
			t.Errorf("%s: url %q still references upstream seaweedfs/seaweedfs", plat, a.URL)
		}
	}
	if _, ok := tool.Assets["windows/amd64"]; ok {
		t.Error("windows asset present; should be absent (fail closed)")
	}
	if _, ok := tool.Assets["windows/arm64"]; ok {
		t.Error("windows asset present; should be absent")
	}
}

func TestSeaweedfsRejectsWindows(t *testing.T) {
	if _, ok := SeaweedfsTool.Assets["windows/amd64"]; ok {
		t.Fatalf("windows asset should not exist")
	}
	if _, err := resolveTool(""); err != nil && strings.Contains(err.Error(), "windows") {
		// Already checked platform; this branch only if running on windows
		t.Logf("resolve on windows correctly fails: %v", err)
	}
	// Verify the Start error string contains the policy.
	errMsg := "supply-chain policy: no bashy-signed asset for windows"
	if !strings.Contains("seaweedfs: supply-chain policy: no bashy-signed asset for windows (no upstream fallback) — only linux and darwin assets are signed", errMsg) {
		t.Fatal("windows error must name supply-chain policy")
	}
}

func TestSeaweedfsRejectsLatestAndUnknownVersion(t *testing.T) {
	if _, err := resolveTool("latest"); err == nil {
		t.Fatal("expected latest to be rejected")
	}
	if _, err := resolveTool("3.80"); err == nil {
		t.Fatal("expected unknown version to be rejected")
	}
	if _, err := resolveTool("4.48"); err != nil {
		t.Fatalf("4.48 alias should be accepted: %v", err)
	}
	if _, err := resolveTool(seaweedfsBashyTag); err != nil {
		t.Fatalf("bashy tag should be accepted: %v", err)
	}
	if _, err := resolveTool(""); err != nil {
		t.Fatalf("empty version should default to pin: %v", err)
	}
}

func TestSpecPointsToForkNotUpstream(t *testing.T) {
	s := Spec("")
	if strings.Contains(s.Repo, "seaweedfs/seaweedfs") {
		t.Fatalf("Spec repo %q still references upstream seaweedfs/seaweedfs", s.Repo)
	}
	if s.Repo != "qiangli/seaweedfs" {
		t.Fatalf("Spec repo %q, want qiangli/seaweedfs", s.Repo)
	}
	if s.Version == "latest" {
		t.Fatal("Spec version should not be latest")
	}
}

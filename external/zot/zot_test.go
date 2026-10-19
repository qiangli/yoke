package zot

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/binmgr"
)

func TestEnsureConfig_SeedsAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	cfg, err := ensureConfig(dir, "127.0.0.1", 5000)
	if err != nil {
		t.Fatalf("ensureConfig: %v", err)
	}
	if cfg != filepath.Join(dir, "config.json") {
		t.Fatalf("cfg path = %s", cfg)
	}
	b, _ := os.ReadFile(cfg)
	var parsed map[string]any
	if err := json.Unmarshal(b, &parsed); err != nil {
		t.Fatalf("config is not valid JSON: %v", err)
	}
	http := parsed["http"].(map[string]any)
	if http["address"] != "127.0.0.1" || http["port"] != "5000" {
		t.Fatalf("http config = %v", http)
	}
	if _, ok := parsed["storage"]; !ok {
		t.Fatal("config missing storage")
	}
	if ext, ok := parsed["extensions"].(map[string]any); !ok || len(ext) != 0 {
		t.Fatalf("extensions = %#v, want empty (optional outbound integrations off)", parsed["extensions"])
	}
	// same arguments: the file stays byte-identical and the mtime is untouched
	// (no rewrite at all — a rewrite would churn restarts and break -count>1).
	first := string(b)
	fiBefore, _ := os.Stat(cfg)
	if _, err := ensureConfig(dir, "127.0.0.1", 5000); err != nil {
		t.Fatal(err)
	}
	b2, _ := os.ReadFile(cfg)
	fiAfter, _ := os.Stat(cfg)
	if string(b2) != first {
		t.Fatal("ensureConfig rewrote an existing config with the same arguments")
	}
	if !fiAfter.ModTime().Equal(fiBefore.ModTime()) {
		t.Fatal("ensureConfig touched config.json mtime with the same arguments")
	}
	// changed arguments: ONLY the managed keys (http.address, http.port,
	// storage.rootDirectory) are rewritten; every other field is preserved.
	var before map[string]any
	if err := json.Unmarshal(b2, &before); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureConfig(dir, "127.0.0.1", 6000); err != nil {
		t.Fatal(err)
	}
	b3, _ := os.ReadFile(cfg)
	var after map[string]any
	if err := json.Unmarshal(b3, &after); err != nil {
		t.Fatal(err)
	}
	afterHTTP, _ := after["http"].(map[string]any)
	if afterHTTP == nil || afterHTTP["address"] != "127.0.0.1" || afterHTTP["port"] != "6000" {
		t.Fatalf("http config after port change = %#v", after["http"])
	}
	// simulate the rewrite on the pre-change document: only managed keys differ
	beforeHTTP, _ := before["http"].(map[string]any)
	beforeHTTP["port"] = "6000"
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("rewrite touched fields beyond the managed keys:\nbefore %#v\nafter  %#v", before, after)
	}
}

func TestEnsureConfig_ReappliesBindOnRestart(t *testing.T) {
	dir := t.TempDir()
	if _, err := ensureConfig(dir, "127.0.0.1", 5000); err != nil {
		t.Fatalf("first ensureConfig: %v", err)
	}
	// restart with a new bind and port: the managed http config must follow
	if _, err := ensureConfig(dir, "0.0.0.0", 5001); err != nil {
		t.Fatalf("second ensureConfig: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(b, &parsed); err != nil {
		t.Fatalf("config JSON: %v", err)
	}
	http, _ := parsed["http"].(map[string]any)
	if http == nil {
		t.Fatalf("config missing http section: %#v", parsed)
	}
	if http["address"] != "0.0.0.0" {
		t.Fatalf("restart kept stale http config: address = %#v, want 0.0.0.0", http["address"])
	}
	if http["port"] != "5001" {
		t.Fatalf("restart kept stale http config: port = %#v, want 5001", http["port"])
	}
}

func TestEnsureConfig_PreservesUserFields(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	if _, err := ensureConfig(dir, "127.0.0.1", 5000); err != nil {
		t.Fatalf("ensureConfig: %v", err)
	}
	// user-owned edits: a tweaked log level and an extensions entry
	b, _ := os.ReadFile(cfgPath)
	var user map[string]any
	if err := json.Unmarshal(b, &user); err != nil {
		t.Fatal(err)
	}
	user["log"].(map[string]any)["level"] = "debug"
	user["extensions"].(map[string]any)["metrics"] = map[string]any{"prometheus": map[string]any{"path": "/metrics"}}
	userDoc, err := json.MarshalIndent(user, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, userDoc, 0o644); err != nil {
		t.Fatal(err)
	}
	// a bind change rewrites only the managed keys; user fields survive
	if _, err := ensureConfig(dir, "0.0.0.0", 5000); err != nil {
		t.Fatalf("ensureConfig after user edits: %v", err)
	}
	b2, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var after map[string]any
	if err := json.Unmarshal(b2, &after); err != nil {
		t.Fatal(err)
	}
	logSec, _ := after["log"].(map[string]any)
	if logSec == nil || logSec["level"] != "debug" {
		t.Fatalf("user log.level lost after reconcile: %#v", after["log"])
	}
	ext, _ := after["extensions"].(map[string]any)
	if ext == nil {
		t.Fatalf("extensions section lost after reconcile: %#v", after["extensions"])
	}
	metrics, _ := ext["metrics"].(map[string]any)
	if metrics == nil {
		t.Fatalf("user extensions.metrics lost after reconcile: %#v", ext)
	}
	prom, _ := metrics["prometheus"].(map[string]any)
	if prom == nil || prom["path"] != "/metrics" {
		t.Fatalf("user extensions.metrics.prometheus lost after reconcile: %#v", metrics)
	}
	if http, _ := after["http"].(map[string]any); http == nil || http["address"] != "0.0.0.0" {
		t.Fatalf("managed http.address not reconciled: %#v", after["http"])
	}
}

func TestSpec(t *testing.T) {
	s := Spec("")
	if s.Repo != "qiangli/zot" || s.Name != "zot" || s.Version != DefaultVersion {
		t.Fatalf("default spec = %+v", s)
	}
	if s.Repo == "project-zot/zot" {
		t.Fatal("spec still references upstream project-zot/zot")
	}
	if s.Version == "latest" {
		t.Fatal("spec version should not be latest")
	}
	if Spec(zotVersion).Version != zotBashyTag {
		t.Fatalf("zotVersion alias not normalized: got %q want %q", Spec(zotVersion).Version, zotBashyTag)
	}
	if s.AssetMatch == nil {
		t.Fatal("zot spec needs a custom AssetMatch")
	}
}

func TestEnsureConfig_MinimalBuildHasNoExtensions(t *testing.T) {
	dir := t.TempDir()
	cfg, err := ensureConfig(dir, "127.0.0.1", 5000)
	if err != nil {
		t.Fatalf("ensureConfig: %v", err)
	}
	b, _ := os.ReadFile(cfg)
	var parsed map[string]any
	if err := json.Unmarshal(b, &parsed); err != nil {
		t.Fatalf("config JSON: %v", err)
	}
	ext, ok := parsed["extensions"].(map[string]any)
	if !ok {
		t.Fatalf("extensions missing or not a map: %#v", parsed["extensions"])
	}
	if len(ext) != 0 {
		t.Fatalf("extensions = %#v, want empty (minimal build: no search/sync/imagetrust/ui)", ext)
	}
	// Explicitly assert none of the minimal-excluded extensions are present.
	for _, key := range []string{"search", "sync", "imagetrust", "ui", "scrub", "metrics", "lint"} {
		if _, ok := ext[key]; ok {
			t.Fatalf("extensions contains %q, want absent for bashy-minimal", key)
		}
	}
	_ = cfg
}

// Zot is a VERIFIED bashy-signed minimal pin: exact identity, issuer, every
// asset sha256 + bundle URL, raw executable (no archive).
func TestZotIsVerifiedPin(t *testing.T) {
	tool := ZotTool
	if tool.Name != "zot" {
		t.Fatalf("name %q, want %q", tool.Name, "zot")
	}
	if tool.Version != zotBashyTag {
		t.Fatalf("version %q, want %q (bashy tag)", tool.Version, zotBashyTag)
	}
	if tool.Sigstore == nil {
		t.Fatal("sigstore is nil; want a VERIFIED pin")
	}
	wantIdentity := "https://github.com/qiangli/zot/.github/workflows/bashy-build.yml@refs/tags/" + zotBashyTag
	if tool.Sigstore.Identity != wantIdentity {
		t.Fatalf("identity %q, want %q", tool.Sigstore.Identity, wantIdentity)
	}
	if tool.Sigstore.Issuer != binmgr.GitHubActionsIssuer {
		t.Fatalf("issuer %q, want %q", tool.Sigstore.Issuer, binmgr.GitHubActionsIssuer)
	}
	if DefaultVersion != zotBashyTag {
		t.Fatalf("DefaultVersion %q, want %q", DefaultVersion, zotBashyTag)
	}
	if zotVersion != "v2.1.22" {
		t.Fatalf("zotVersion %q, want v2.1.22", zotVersion)
	}
	wantPlatforms := []string{"darwin/amd64", "darwin/arm64", "linux/amd64", "linux/arm64", "windows/amd64"}
	if len(tool.Assets) != len(wantPlatforms) {
		t.Fatalf("assets = %d, want %d (%v)", len(tool.Assets), len(wantPlatforms), wantPlatforms)
	}
	for _, plat := range wantPlatforms {
		a, ok := tool.Assets[plat]
		if !ok {
			t.Fatalf("no %s asset", plat)
		}
		goos, goarch, _ := strings.Cut(plat, "/")
		assetName := "zot-" + goos + "-" + goarch
		if goos == "windows" {
			assetName += ".exe"
		}
		wantURL := "https://github.com/qiangli/zot/releases/download/" + zotBashyTag + "/" + assetName
		if a.URL != wantURL {
			t.Errorf("%s: url %q, want %q", plat, a.URL, wantURL)
		}
		if len(a.SHA256) != 64 || a.SHA512 != "" {
			t.Errorf("%s: sha256 %q sha512 %q", plat, a.SHA256, a.SHA512)
		}
		if wantSHA := zotSHAs[plat]; a.SHA256 != wantSHA {
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
		if strings.Contains(a.URL, "project-zot/zot") {
			t.Errorf("%s: url %q still references upstream project-zot/zot", plat, a.URL)
		}
	}
	if got := tool.Assets["windows/amd64"].SHA256; got != "c373f8f85f5324624d6bb11620fe149fec5c2131bcf443234f5a2f10780e294c" {
		t.Errorf("windows SHA256 = %q, want published checksum", got)
	}
	if _, ok := tool.Assets["windows/arm64"]; ok {
		t.Error("windows asset present; should be absent")
	}
}

func TestZotRejectsLatestAndUnknownVersion(t *testing.T) {
	if _, err := resolveTool("latest"); err == nil {
		t.Fatal("expected latest to be rejected")
	}
	if _, err := resolveTool("v2.1.20"); err == nil {
		t.Fatal("expected unknown version to be rejected")
	}
	if _, err := resolveTool("v2.1.22"); err != nil {
		t.Fatalf("v2.1.22 alias should be accepted: %v", err)
	}
	if _, err := resolveTool(zotBashyTag); err != nil {
		t.Fatalf("bashy tag should be accepted: %v", err)
	}
	if _, err := resolveTool(""); err != nil {
		t.Fatalf("empty version should default to pin: %v", err)
	}
}

func TestZotSpecPointsToForkNotUpstream(t *testing.T) {
	s := Spec("")
	if strings.Contains(s.Repo, "project-zot/zot") {
		t.Fatalf("Spec repo %q still references upstream project-zot/zot", s.Repo)
	}
	if s.Repo != "qiangli/zot" {
		t.Fatalf("Spec repo %q, want qiangli/zot", s.Repo)
	}
	if s.Version == "latest" {
		t.Fatal("Spec version should not be latest")
	}
}

// The asset names below are the REAL ones from a zot release (verified against
// project-zot/zot v2.1.18). The previous version of this test asserted on
// invented names — "zot-minimal-linux-amd64" and "zot-exporter-linux-amd64",
// neither of which zot publishes — which is exactly why the bug shipped: the
// test passed while the resolver cached `zb`, the load-test benchmark tool.
func TestAssetMatch_PicksFullBuild(t *testing.T) {
	cases := []struct {
		goos, goarch, name string
		want               bool
	}{
		// the full build — the only acceptable match
		{"linux", "amd64", "zot-linux-amd64", true},
		{"windows", "amd64", "zot-windows-amd64.exe", true},
		{"windows", "amd64", "zot-windows-amd64", false},
		{"darwin", "arm64", "zot-darwin-arm64", true},

		// siblings that ALL carry the os/arch tokens and must NOT match
		{"darwin", "arm64", "zb-darwin-arm64", false},  // benchmark — the shipped bug
		{"darwin", "arm64", "zli-darwin-arm64", false}, // CLI
		{"darwin", "arm64", "zxp-darwin-arm64", false}, // exporter (named zxp, not "exporter")
		{"darwin", "arm64", "zot-darwin-arm64-debug", false},
		{"darwin", "arm64", "zot-darwin-arm64-minimal", false},
		{"linux", "amd64", "zb-linux-amd64", false},
		{"linux", "amd64", "zli-linux-amd64", false},
		{"linux", "amd64", "zxp-linux-amd64", false},
		{"linux", "amd64", "zot-linux-amd64-minimal", false},

		// wrong platform never matches
		{"linux", "amd64", "zot-darwin-arm64", false},
		{"darwin", "arm64", "zot-linux-amd64", false},
	}
	for _, c := range cases {
		if got := assetMatch(c.name, c.goos, c.goarch); got != c.want {
			t.Errorf("assetMatch(%q, %s/%s) = %v, want %v", c.name, c.goos, c.goarch, got, c.want)
		}
	}
}

// Exactly one asset from a real release listing may match a given platform —
// otherwise resolution is order-dependent and can silently cache the wrong tool.
func TestAssetMatch_ExactlyOneWinnerPerPlatform(t *testing.T) {
	release := []string{
		"zb-darwin-arm64", "zli-darwin-arm64", "zot-darwin-arm64",
		"zot-darwin-arm64-debug", "zot-darwin-arm64-minimal", "zxp-darwin-arm64",
		"zb-linux-amd64", "zli-linux-amd64", "zot-linux-amd64",
		"zot-linux-amd64-debug", "zot-linux-amd64-minimal", "zxp-linux-amd64",
	}
	for _, p := range []struct{ goos, goarch string }{{"darwin", "arm64"}, {"linux", "amd64"}} {
		var hits []string
		for _, a := range release {
			if assetMatch(a, p.goos, p.goarch) {
				hits = append(hits, a)
			}
		}
		if len(hits) != 1 {
			t.Errorf("%s/%s matched %d assets %v, want exactly 1", p.goos, p.goarch, len(hits), hits)
		}
	}
}

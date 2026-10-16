package stack

import (
	"slices"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/binmgr"
)

func TestVictoriaStoreArgsDoNotReportRemotely(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"logs", victoriaLogsArgs(9428, "/data/logs", "")},
		{"metrics", victoriaMetricsArgs(8428, "/data/metrics", "")},
		{"traces", victoriaTracesArgs(10428, "/data/traces", "")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, arg := range tt.args {
				if strings.HasPrefix(arg, "-pushmetrics.url") {
					t.Fatalf("remote metrics reporting enabled: %v", tt.args)
				}
			}
		})
	}
	if !slices.Contains(tests[1].args, "-selfScrapeInterval=0") {
		t.Fatalf("VictoriaMetrics self-scrape not explicitly disabled: %v", tests[1].args)
	}
}

func TestVictoriaStoreEnvPinsLocalCollector(t *testing.T) {
	if !slices.Contains(localOTLPEnv("victoria-logs"), "OTEL_EXPORTER_OTLP_ENDPOINT="+localCollectorEndpoint) {
		t.Fatalf("local collector missing: %v", localOTLPEnv("victoria-logs"))
	}
}

// Each store is a VERIFIED bashy-signed pin: exact identity, issuer, every
// asset sha256 + bundle URL, raw executable (no archive).
func TestVictoriaStoresAreVerifiedPins(t *testing.T) {
	cases := []struct {
		name       string
		tool       binmgr.Tool
		bashyTag   string
		version    string
		windowsSHA string
	}{
		{"victoria-metrics", victoriaMetricsTool, victoriaMetricsBashyTag, victoriaMetricsVersion, "4fa0cca9de0eb898d5158d73dbfada99540d2ec20c9bace91020a259020930f3"},
		{"victoria-logs", victoriaLogsTool, victoriaLogsBashyTag, victoriaLogsVersion, "7880d57a5e9daa7172905dbb30bced97afd59266ec5b663d89386b8fbe8d4a87"},
		{"victoria-traces", victoriaTracesTool, victoriaTracesBashyTag, victoriaTracesVersion, "ed3373a067adae2d1a0f4d1c5f3a30798b43d8db1b2a3d134a9200a74ebbf27c"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tool := tc.tool
			if tool.Name != tc.name {
				t.Fatalf("name %q, want %q", tool.Name, tc.name)
			}
			if tool.Version != tc.bashyTag {
				t.Fatalf("version %q, want %q (bashy tag)", tool.Version, tc.bashyTag)
			}
			if tool.Sigstore == nil {
				t.Fatal("sigstore is nil; want a VERIFIED pin")
			}
			wantIdentity := "https://github.com/qiangli/" + tc.name + "/.github/workflows/bashy-build.yml@refs/tags/" + tc.bashyTag
			if tool.Sigstore.Identity != wantIdentity {
				t.Fatalf("identity %q, want %q", tool.Sigstore.Identity, wantIdentity)
			}
			if tool.Sigstore.Issuer != binmgr.GitHubActionsIssuer {
				t.Fatalf("issuer %q, want %q", tool.Sigstore.Issuer, binmgr.GitHubActionsIssuer)
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
				assetName := tc.name + "-" + goos + "-" + goarch
				if goos == "windows" {
					assetName += ".exe"
				}
				wantURL := "https://github.com/qiangli/" + tc.name + "/releases/download/" + tc.bashyTag + "/" + assetName
				if a.URL != wantURL {
					t.Errorf("%s: url %q, want %q", plat, a.URL, wantURL)
				}
				if len(a.SHA256) != 64 || a.SHA512 != "" {
					t.Errorf("%s: sha256 %q sha512 %q", plat, a.SHA256, a.SHA512)
				}
				// Bundle is asset URL + .sigstore.json (default suffix).
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
				// Ensure no upstream VictoriaMetrics/* path remains.
				if strings.Contains(a.URL, "VictoriaMetrics/") {
					t.Errorf("%s: url %q still references upstream VictoriaMetrics/*", plat, a.URL)
				}
			}
			if got := tool.Assets["windows/amd64"].SHA256; got != tc.windowsSHA {
				t.Errorf("windows SHA256 = %q, want published checksum %q", got, tc.windowsSHA)
			}
			if _, ok := tool.Assets["windows/arm64"]; ok {
				t.Error("unsigned windows/arm64 asset present")
			}
			// Version constant is the upstream tag the bashy release was built from.
			if tc.version == "" {
				t.Error("version constant is empty")
			}
		})
	}
}

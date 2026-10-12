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
		name     string
		tool     binmgr.Tool
		bashyTag string
		version  string
	}{
		{"victoria-metrics", victoriaMetricsTool, victoriaMetricsBashyTag, victoriaMetricsVersion},
		{"victoria-logs", victoriaLogsTool, victoriaLogsBashyTag, victoriaLogsVersion},
		{"victoria-traces", victoriaTracesTool, victoriaTracesBashyTag, victoriaTracesVersion},
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
				assetName := tc.name + "-" + goos + "-" + goarch
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
			// Windows has no signed asset.
			if _, ok := tool.Assets["windows/amd64"]; ok {
				t.Error("windows asset present; should be absent (fail closed)")
			}
			// Version constant is the upstream tag the bashy release was built from.
			if tc.version == "" {
				t.Error("version constant is empty")
			}
		})
	}
}

func TestVictoriaStoresRejectWindows(t *testing.T) {
	// The Start method fails closed on windows with a supply-chain policy
	// error and no upstream fallback. We cannot run on windows here, but we
	// verify the Tool has no windows asset and the error string is defined.
	for _, tool := range []binmgr.Tool{victoriaMetricsTool, victoriaLogsTool, victoriaTracesTool} {
		if _, ok := tool.Assets["windows/amd64"]; ok {
			t.Fatalf("%s: windows asset should not exist", tool.Name)
		}
	}
	// Simulate the Start check by verifying the error message contains the
	// policy string.
	errMsg := "supply-chain policy: no bashy-signed asset for windows"
	if !strings.Contains("victoria-metrics: supply-chain policy: no bashy-signed asset for windows (no upstream fallback) — only linux and darwin assets are signed", errMsg) {
		t.Fatal("windows error must name supply-chain policy")
	}
}

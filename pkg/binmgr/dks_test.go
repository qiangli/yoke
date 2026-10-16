package binmgr

import (
	"context"
	"encoding/hex"
	"strings"
	"testing"
)

func TestDKSDeclarationsAndPins(t *testing.T) {
	cases := []struct{ name, repo, version string }{
		{"containerd-shim-runc-v2", "k3s", K3sVersion},
		{"k3s", "k3s", K3sVersion},
		{"runc", "runc", RuncVersion},
		{"cni-bandwidth", "cni-plugins", CNIPluginsVersion},
		{"cni-bridge", "cni-plugins", CNIPluginsVersion},
		{"cni-firewall", "cni-plugins", CNIPluginsVersion},
		{"cni-host-local", "cni-plugins", CNIPluginsVersion},
		{"cni-loopback", "cni-plugins", CNIPluginsVersion},
		{"cni-portmap", "cni-plugins", CNIPluginsVersion},
		{"metrics-server", "metrics-server", MetricsServerVersion},
		{"tailscale", "tailscale", TailscaleVersion},
		{"tailscaled", "tailscale", TailscaleVersion},
		{"coredns", "coredns", CoreDNSVersion},
		{"pause", "pause", PauseVersion},
	}
	if len(dksReleases) != len(cases) {
		t.Fatalf("declarations: got %d, want %d", len(dksReleases), len(cases))
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tool, err := DKSTool(tc.name)
			if err != nil {
				t.Fatal(err)
			}
			identity := "https://github.com/qiangli/" + tc.repo + "/.github/workflows/bashy-build.yml@refs/tags/" + tc.version
			if tool.Name != tc.name || tool.Version != tc.version || !tool.RequireCommittedPin ||
				tool.Sigstore == nil || tool.Sigstore.Identity != identity ||
				tool.Sigstore.Issuer != "https://token.actions.githubusercontent.com" {
				t.Fatalf("trust declaration: %+v", tool)
			}
			base := "https://github.com/qiangli/" + tc.repo + "/releases/download/" + tc.version + "/"
			if tool.SignedSums == nil || tool.SignedSums.URL != base+"SHA256SUMS" ||
				tool.SignedSums.BundleURL != base+"SHA256SUMS.sigstore.json" {
				t.Fatalf("signed sums: %+v", tool.SignedSums)
			}
			if len(tool.Assets) != 2 {
				t.Fatalf("assets: %+v", tool.Assets)
			}
			for _, arch := range []string{"amd64", "arm64"} {
				platform := "linux/" + arch
				asset, ok := tool.Assets[platform]
				url := base + tc.name + "-linux-" + arch
				if !ok || asset.URL != url || asset.SigstoreBundle != url+".sigstore.json" ||
					asset.Tree || asset.Binary != "" {
					t.Errorf("%s asset: %+v", platform, asset)
				}
				pin, ok := PinnedSHA256(tc.name, tc.version, platform)
				decoded, err := hex.DecodeString(pin)
				if !ok || err != nil || len(decoded) != 32 {
					t.Errorf("%s pin: %q, found %v", platform, pin, ok)
				}
			}
			for _, platform := range []string{"darwin/amd64", "darwin/arm64", "windows/amd64", "windows/arm64", "linux/386"} {
				if _, ok := tool.Assets[platform]; ok {
					t.Errorf("unexpected asset for %s", platform)
				}
				if _, ok := PinnedSHA256(tc.name, tc.version, platform); ok {
					t.Errorf("unexpected pin for %s", platform)
				}
			}
			// Exercise the pre-network refusal on every test host, including macOS.
			// A tag bump without reviewed replacement pins must never use TOFU.
			tool.Assets[Platform()] = tool.Assets["linux/amd64"]
			tool.Version += "-unpinned"
			if _, err := Ensure(context.Background(), tool); err == nil ||
				!strings.Contains(err.Error(), "no committed sha256") {
				t.Fatalf("missing pin must fail closed: %v", err)
			}
		})
	}
}

func TestDKSToolRejectsUnknownName(t *testing.T) {
	for _, name := range []string{"", "cni-plugins", "k3s/latest", "unknown"} {
		if _, err := DKSTool(name); err == nil {
			t.Errorf("accepted unknown executable %q", name)
		}
	}
}

func TestDKSSignedSumsExactInventory(t *testing.T) {
	counts := map[string]int{"k3s": 10, "runc": 8, "cni-plugins": 18, "metrics-server": 8, "tailscale": 10, "coredns": 8, "pause": 8}
	for name, release := range dksReleases {
		t.Run(name, func(t *testing.T) {
			tool, err := DKSTool(name)
			if err != nil {
				t.Fatal(err)
			}
			var rows []string
			for _, filename := range tool.SignedSums.RequiredNames {
				rows = append(rows, strings.Repeat("a", 64)+"  "+filename+"\n")
			}
			if len(rows) != counts[release.repo] {
				t.Fatalf("manifest rows: got %d, want %d", len(rows), counts[release.repo])
			}
			manifest := strings.Join(rows, "")
			if _, err := parseSignedSums(tool, []byte(manifest)); err != nil {
				t.Fatal(err)
			}
			for i := range rows {
				missing := strings.Join(rows[:i], "") + strings.Join(rows[i+1:], "")
				if _, err := parseSignedSums(tool, []byte(missing)); err == nil {
					t.Fatalf("accepted missing row %d", i)
				}
			}
			for _, bad := range []string{
				manifest + rows[0],
				strings.Join(rows[1:], "") + rows[1],
				strings.Join(rows[1:], "") + strings.Repeat("a", 64) + "  unlisted\n",
			} {
				if _, err := parseSignedSums(tool, []byte(bad)); err == nil {
					t.Fatal("accepted extra, duplicate, or unlisted row")
				}
			}
		})
	}
}

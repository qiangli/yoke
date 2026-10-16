package binmgr

import (
	"fmt"
	"strings"
)

// DKS runtime releases are built and signed by each fork's bashy-build.yml.
// Keep each tag here so companions and their pin keys move together. Updating
// a tag also requires reviewing and replacing all of that release's digests.
const (
	K3sVersion           = "bashy-v1.37.1+k3s1-7"
	RuncVersion          = "bashy-v1.5.2-2"
	CNIPluginsVersion    = "bashy-v1.5.1-2"
	MetricsServerVersion = "bashy-v0.9.0-2"
	TailscaleVersion     = "bashy-v1.104.1-1"
	CoreDNSVersion       = "bashy-v1.14.7-1"
	PauseVersion         = "bashy-v1.37.1-2"
)

var dksReleases = map[string]struct{ repo, version string }{
	"containerd-shim-runc-v2": {"k3s", K3sVersion},
	"k3s":                     {"k3s", K3sVersion},
	"runc":                    {"runc", RuncVersion},
	"cni-bandwidth":           {"cni-plugins", CNIPluginsVersion},
	"cni-bridge":              {"cni-plugins", CNIPluginsVersion},
	"cni-firewall":            {"cni-plugins", CNIPluginsVersion},
	"cni-host-local":          {"cni-plugins", CNIPluginsVersion},
	"cni-loopback":            {"cni-plugins", CNIPluginsVersion},
	"cni-portmap":             {"cni-plugins", CNIPluginsVersion},
	"metrics-server":          {"metrics-server", MetricsServerVersion},
	"tailscale":               {"tailscale", TailscaleVersion},
	"tailscaled":              {"tailscale", TailscaleVersion},
	"coredns":                 {"coredns", CoreDNSVersion},
	"pause":                   {"pause", PauseVersion},
}

// dksManifestNames is the exact file inventory reviewed in each pinned release's
// SHA256SUMS. Companions and build evidence are required, not optional rows.
func dksManifestNames(repo string) []string {
	versions := map[string]string{
		"k3s": "v1.37.1+k3s1", "runc": "v1.5.2", "cni-plugins": "v1.5.1",
		"metrics-server": "v0.9.0", "tailscale": "v1.104.1", "coredns": "v1.14.7", "pause": "v1.37.1",
	}
	names := []string{
		repo + "-no-tracking.env", repo + "-otel.env", repo + "-pin.txt",
		repo + "-" + versions[repo] + ".cdx.json",
		repo + "-" + versions[repo] + ".govulncheck.json", "vuln-ignore",
	}
	for name, release := range dksReleases {
		if release.repo == repo {
			for _, arch := range []string{"amd64", "arm64"} {
				names = append(names, name+"-linux-"+arch)
			}
		}
	}
	return names
}

// DKSTool declares a Linux runtime executable from the seven bashy-built DKS
// releases. CNI plugins use their release names (cni-bridge, cni-host-local,
// cni-loopback, cni-portmap, cni-bandwidth, cni-firewall). Each executable has
// its own committed digest, exact workflow identity and signed checksum row.
// Unknown names and unsupported platforms never fall back to upstream assets.
func DKSTool(name string) (Tool, error) {
	release, ok := dksReleases[name]
	if !ok {
		return Tool{}, fmt.Errorf("binmgr: unknown DKS tool %q", name)
	}
	base := "https://github.com/qiangli/" + release.repo + "/releases/download/" + release.version + "/"
	t := Tool{
		Name: name, Version: release.version,
		Assets:              make(map[string]Asset),
		RequireCommittedPin: true,
		Sigstore: &Sigstore{
			Identity: "https://github.com/qiangli/" + release.repo + "/.github/workflows/bashy-build.yml@refs/tags/" + release.version,
			Issuer:   GitHubActionsIssuer,
		},
		SignedSums: &SignedSums{URL: base + "SHA256SUMS", BundleURL: base + "SHA256SUMS" + DefaultBundleSuffix, RequiredNames: dksManifestNames(release.repo)},
	}
	for _, platform := range []string{"linux/amd64", "linux/arm64"} {
		url := base + name + "-" + strings.ReplaceAll(platform, "/", "-")
		t.Assets[platform] = Asset{URL: url, SigstoreBundle: url + DefaultBundleSuffix}
	}
	return t, nil
}

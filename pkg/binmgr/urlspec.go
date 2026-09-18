package binmgr

import (
	"context"
	"fmt"
	"strings"
)

// URLSpec locates a tool's binary at a templated download URL — for vendors that
// publish releases OUTSIDE GitHub (e.g. act_runner on dl.gitea.com). It is the
// non-GitHub sibling of GitHubSpec: same trust path (download → sha256-verify →
// cache via Ensure), just a different way to resolve the per-platform asset URL.
//
// URLTemplate (and the optional ChecksumURLTemplate) expand these tokens:
//
//	{version}  the resolved version (leading "v" preserved as given)
//	{goos}     runtime.GOOS, after OSAlias if set
//	{goarch}   runtime.GOARCH, after ArchAlias if set
//	{ext}      ".exe" on windows, "" elsewhere
//
// Example (act_runner):
//
//	URLTemplate: "https://dl.gitea.com/act_runner/{version}/act_runner-{version}-{goos}-{goarch}{ext}"
type URLSpec struct {
	// Name is the logical tool name — the cache key and cached binary name.
	Name string
	// Version is required (no "latest" auto-resolution — there's no API to query).
	Version string
	// URLTemplate is the download URL with {version}/{goos}/{goarch}/{ext} tokens.
	URLTemplate string
	// ChecksumURLTemplate (optional) is the sha256 sidecar URL template; when
	// empty, ResolveURL tries "<download-url>.sha256". The body may be a bare
	// digest or a "<digest>  <file>" line — the first 64-hex run is taken.
	ChecksumURLTemplate string
	// Member is the executable's path within a .tar.gz/.zip; "" = raw binary.
	Member string
	// ArchAlias maps runtime.GOARCH into the vendor's token (e.g. amd64→x86_64).
	ArchAlias func(goarch string) string
	// OSAlias maps runtime.GOOS into the vendor's token.
	OSAlias func(goos string) string
}

// ResolveURL turns a URLSpec into a Tool for the current platform, ready for
// Ensure/Start. It fetches and parses the checksum so the cache stays verified.
func ResolveURL(ctx context.Context, spec URLSpec) (Tool, error) {
	if spec.Name == "" || spec.Version == "" || spec.URLTemplate == "" {
		return Tool{}, fmt.Errorf("binmgr: url spec needs name, version, and url template")
	}
	goos, goarch := splitPlatform(Platform())
	if spec.OSAlias != nil {
		goos = spec.OSAlias(goos)
	}
	if spec.ArchAlias != nil {
		goarch = spec.ArchAlias(goarch)
	}
	url := expandTokens(spec.URLTemplate, spec.Version, goos, goarch)

	sha, err := resolveURLChecksum(ctx, spec, url)
	if err != nil {
		return Tool{}, err
	}
	return Tool{
		Name:    spec.Name,
		Version: spec.Version,
		Assets: map[string]Asset{
			Platform(): {URL: url, SHA256: sha, Binary: spec.Member},
		},
	}, nil
}

// ResolveURLPinned is ResolveURL for a caller that already HOLDS the digest —
// a registered command record, whose reviewed YAML is the trust root the way
// pins.go is for the compiled-in externals. It touches no network: the
// template is expanded for the current platform and the asset carries the
// given sha256, which Ensure then verifies the download against.
func ResolveURLPinned(spec URLSpec, sha256 string) Tool {
	goos, goarch := splitPlatform(Platform())
	if spec.OSAlias != nil {
		goos = spec.OSAlias(goos)
	}
	if spec.ArchAlias != nil {
		goarch = spec.ArchAlias(goarch)
	}
	return Tool{
		Name:    spec.Name,
		Version: spec.Version,
		Assets: map[string]Asset{
			Platform(): {URL: expandTokens(spec.URLTemplate, spec.Version, goos, goarch), SHA256: strings.ToLower(sha256), Binary: spec.Member},
		},
	}
}

func expandTokens(tmpl, version, goos, goarch string) string {
	ext := ""
	if goos == "windows" {
		ext = ".exe"
	}
	r := strings.NewReplacer(
		"{version}", version,
		"{goos}", goos,
		"{goarch}", goarch,
		"{ext}", ext,
	)
	return r.Replace(tmpl)
}

// resolveURLChecksum reads the sha256 sidecar (the template if given, else
// "<url>.sha256") and extracts the digest. Errors if none is found — the verify
// is the trust boundary and must not be silently skipped.
func resolveURLChecksum(ctx context.Context, spec URLSpec, downloadURL string) (string, error) {
	var checkURL string
	if spec.ChecksumURLTemplate != "" {
		goos, goarch := splitPlatform(Platform())
		if spec.OSAlias != nil {
			goos = spec.OSAlias(goos)
		}
		if spec.ArchAlias != nil {
			goarch = spec.ArchAlias(goarch)
		}
		checkURL = expandTokens(spec.ChecksumURLTemplate, spec.Version, goos, goarch)
	} else {
		checkURL = downloadURL + ".sha256"
	}
	body, err := httpGetBody(ctx, checkURL, "")
	if err != nil {
		return "", fmt.Errorf("binmgr: fetch checksum %s: %w", checkURL, err)
	}
	if h := hex64.FindString(string(body)); h != "" {
		return strings.ToLower(h), nil
	}
	return "", fmt.Errorf("binmgr: no sha256 in %s", checkURL)
}

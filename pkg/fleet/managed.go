package fleet

// A MANAGED INSTALL — operator decision D12 (2026-10-09, "bashy is all you
// need"): the agent CLIs the fleet drives are pinned, binmgr-managed tools, the
// same way bashy already manages go, node, uv, ollama and the rest. The tool
// recipe (pkg/fleet/baseline/tools/<tool>.yaml) names the vendor's OWN
// published artifact per platform — a GitHub release asset, a vendor download
// URL, an npm platform package tarball — with its version and digest, and the
// launcher resolves it on first use into the bashy cache. Nothing is vendored
// into this repo: download + exec of the vendor's artifact, under the vendor's
// licence, which the recipe records.
//
// Why a pin and not "whatever is on PATH": the fleet benchmarks tool:model
// bindings against each other, and a CLI that silently moved from one version
// to another makes every measurement incomparable (the Homebrew opencode
// 1.18.30 build that failed every run with "Unexpected server error" while
// exiting 0 is the story this shipped under). So a managed recipe is the ONLY
// binary a launch uses: when the pin cannot be resolved the launch fails with
// that reason, it never falls through to an unpinned PATH or Homebrew copy.
// A PATH/Homebrew binary stays available as an EXPLICIT override — the
// per-tool BASHY_TOOL_BINARY_<NAME> environment variable, or a `cli.binary:`
// path (with a separator) in an operator overlay — never as a fallback.
//
// Self-update is switched off through the recipe's `env:` (DISABLE_AUTOUPDATER,
// OPENCODE_DISABLE_AUTOUPDATE, …): a tool that updates itself in place
// un-pins itself, and a pinned cache path must stay the bytes its digest named.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/qiangli/yoke/pkg/binmgr"
)

// ToolManaged is a tool recipe's pinned install: the vendor's published
// artifact per platform, verified by digest and cached by binmgr.
type ToolManaged struct {
	// Source records WHERE the vendor publishes the artifact — informational,
	// for the operator reading the recipe: github-release, vendor-url, npm,
	// pypi. The URL in each asset is what is fetched.
	Source string `yaml:"source,omitempty" json:"source,omitempty" doc:"where the vendor publishes: github-release, vendor-url, npm, pypi"`
	// Version is the pinned release; {version} in asset URLs expands to it.
	// Never "latest": there is nothing to pin otherwise.
	Version string `yaml:"version" json:"version" doc:"pinned vendor version; {version} in asset urls expands to it"`
	// License is the vendor's licence for the artifact (an SPDX id, or the
	// vendor's terms when the binary is proprietary). Download + exec only;
	// nothing is vendored.
	License string `yaml:"license" json:"license" doc:"vendor licence of the downloaded artifact (SPDX id or terms); download+exec only"`
	// LicenseURL points at the licence text or terms.
	LicenseURL string `yaml:"license_url,omitempty" json:"license_url,omitempty" doc:"where the licence or terms are published"`
	// Env is KEY=VALUE pairs every launch of the managed binary sets — the
	// tool's own self-update switch, so the pin stays the pin.
	Env []string `yaml:"env,omitempty" json:"env,omitempty" doc:"KEY=VALUE set on every launch (the vendor's self-update switch)"`
	// Assets are keyed by goos/goarch ("darwin/arm64").
	Assets map[string]ToolManagedAsset `yaml:"assets" json:"assets" doc:"per-platform artifacts keyed goos/goarch"`
	// Notes is free text: how the digests were verified, what the artifact
	// contains, why a platform is missing.
	Notes string `yaml:"notes,omitempty" json:"notes,omitempty" doc:"provenance and verification notes"`
	// Service marks a pin whose binary is a server bashy or outpost STARTS
	// (a homeserver, a gateway). Service pins answer to the supply-chain
	// policy: only bashy-CI builds signed keylessly with Sigstore run, so a
	// service pin without Sigstore is UNVERIFIED and is refused at install
	// and at service enable/start.
	Service bool `yaml:"service,omitempty" json:"service,omitempty" doc:"the binary is a server bashy starts; requires sigstore"`
	// Sigstore is the keyless signing identity every asset must carry.
	// binmgr verifies the bundle over the downloaded bytes before install.
	Sigstore *ToolSigstore `yaml:"sigstore,omitempty" json:"sigstore,omitempty" doc:"keyless Sigstore identity every asset must be signed by"`
}

// ToolSigstore is a pin's keyless signing identity (cosign keyless, GitHub
// Actions OIDC). Identity and issuer are compared EXACTLY.
type ToolSigstore struct {
	Identity string `yaml:"identity" json:"identity" doc:"exact certificate SAN: https://github.com/OWNER/REPO/.github/workflows/FILE@refs/..."`
	Issuer   string `yaml:"issuer" json:"issuer" doc:"exact OIDC issuer: https://token.actions.githubusercontent.com"`
	// Bundle locates each asset's bundle: empty = asset URL + ".sigstore.json";
	// a suffix starting with "." is appended to the asset URL; an https URL
	// expands {version} and {asset} (the asset URL's basename).
	Bundle string `yaml:"bundle,omitempty" json:"bundle,omitempty" doc:"bundle URL ({version} {asset}) or asset-URL suffix; default .sigstore.json"`
}

// ErrUnsignedServiceBuild is the refusal for a service whose binary is not a
// verified bashy-signed build.
var ErrUnsignedServiceBuild = errors.New("bashy only runs bashy-signed server builds")

// Unverified reports a service pin without a Sigstore identity.
func (m *ToolManaged) Unverified() bool { return m != nil && m.Service && m.Sigstore == nil }

// ServiceBuildCheck refuses a tool that may not run as a server: a managed
// pin must exist and declare Sigstore. Nil means binmgr will only ever
// install a build signed by the pinned identity.
func (t Tool) ServiceBuildCheck() error {
	m := t.CLI.Managed
	switch {
	case m == nil:
		return fmt.Errorf("%w: tool %q has no managed pin to verify", ErrUnsignedServiceBuild, t.Name)
	case m.Sigstore == nil:
		return fmt.Errorf("%w: tool %q pin %s declares no sigstore identity (unverified)", ErrUnsignedServiceBuild, t.Name, m.Version)
	}
	return m.Validate(t.Name)
}

func (s *ToolSigstore) validate(bad func(string, ...any), assets int) {
	id := strings.TrimSpace(s.Identity)
	if !strings.HasPrefix(id, "https://github.com/") || !strings.Contains(id, "/.github/workflows/") || !strings.Contains(id, "@refs/") {
		bad("sigstore identity %q is not an exact GitHub Actions workflow ref (https://github.com/OWNER/REPO/.github/workflows/FILE@refs/...)", s.Identity)
	}
	if strings.TrimSpace(s.Issuer) != binmgr.GitHubActionsIssuer {
		bad("sigstore issuer %q, want %s", s.Issuer, binmgr.GitHubActionsIssuer)
	}
	b := strings.TrimSpace(s.Bundle)
	switch {
	case b == "", strings.HasPrefix(b, "."):
	case strings.HasPrefix(strings.ToLower(b), "https://"):
		if assets > 1 && !strings.Contains(b, "{asset}") {
			bad("sigstore bundle %q names one bundle for %d assets; use {asset} or a suffix", b, assets)
		}
	default:
		bad("sigstore bundle %q is neither an https URL nor an asset-URL suffix", b)
	}
}

// bundleFor renders the bundle location for one expanded asset URL.
func (s *ToolSigstore) bundleFor(assetURL, version string) string {
	b := strings.TrimSpace(s.Bundle)
	switch {
	case b == "":
		return assetURL + binmgr.DefaultBundleSuffix
	case strings.HasPrefix(b, "."):
		return assetURL + b
	}
	b = strings.ReplaceAll(b, "{version}", version)
	return strings.ReplaceAll(b, "{asset}", assetURL[strings.LastIndex(assetURL, "/")+1:])
}

// ToolManagedAsset is one platform's artifact.
type ToolManagedAsset struct {
	URL string `yaml:"url" json:"url" doc:"download URL; {version} expands to the pinned version"`
	// Exactly one digest is required. sha256 is preferred; sha512 is for
	// vendors that publish only sha512 (Antigravity).
	SHA256 string `yaml:"sha256,omitempty" json:"sha256,omitempty" doc:"hex sha256 of the downloaded file"`
	SHA512 string `yaml:"sha512,omitempty" json:"sha512,omitempty" doc:"hex sha512 of the downloaded file, when the vendor publishes no sha256"`
	// Member is the executable's path or basename inside an archive; empty
	// means the download is the raw binary.
	Member string `yaml:"member,omitempty" json:"member,omitempty" doc:"executable inside a .zip/.tar.gz; empty = raw binary"`
	// Tree unpacks the whole archive (a tool that needs its sibling files)
	// and runs Entrypoint inside it.
	Tree       bool   `yaml:"tree,omitempty" json:"tree,omitempty" doc:"unpack the whole archive instead of one member"`
	Entrypoint string `yaml:"entrypoint,omitempty" json:"entrypoint,omitempty" doc:"executable path inside the unpacked tree (tree only)"`
}

var (
	hexSHA256 = regexp.MustCompile(`^[0-9a-f]{64}$`)
	hexSHA512 = regexp.MustCompile(`^[0-9a-f]{128}$`)
	gooses    = []string{"darwin", "linux", "windows", "freebsd", "openbsd", "netbsd", "android"}
)

// ToolBinaryOverrideEnv is the per-tool environment variable that names an
// explicit binary to launch INSTEAD of the managed install — the operator's
// PATH or Homebrew copy, chosen out loud. BASHY_TOOL_BINARY_OPENCODE for
// opencode, BASHY_TOOL_BINARY_KIMI_CODE for kimi-code.
func ToolBinaryOverrideEnv(tool string) string {
	s := strings.ToUpper(strings.TrimSpace(tool))
	s = strings.NewReplacer("-", "_", ".", "_", "/", "_").Replace(s)
	return "BASHY_TOOL_BINARY_" + s
}

// IsManaged reports a recipe that pins a binmgr-managed install.
func (t Tool) IsManaged() bool { return t.CLI.Managed != nil }

// Validate checks a managed block is complete enough to be a pin: a version,
// a licence, and for every platform a URL plus exactly one well-formed digest.
// A recipe that fails here is reported by Catalog.Tools and REFUSED at launch;
// it never degrades to an unpinned binary.
func (m *ToolManaged) Validate(tool string) error {
	if m == nil {
		return nil
	}
	var errs []error
	bad := func(format string, a ...any) {
		errs = append(errs, fmt.Errorf("fleet: tool %q managed install: "+format, append([]any{tool}, a...)...))
	}
	v := strings.TrimSpace(m.Version)
	switch v {
	case "":
		bad("version is required")
	case "latest", "stable", "main", "master":
		bad("version %q is not a pin", v)
	}
	if strings.TrimSpace(m.License) == "" {
		bad("license is required (the vendor's licence or terms for the downloaded artifact)")
	}
	for _, kv := range m.Env {
		if !envKeyLine.MatchString(kv) {
			bad("env entry %q is not KEY=VALUE", kv)
		}
	}
	if len(m.Assets) == 0 {
		bad("no assets")
	}
	if m.Sigstore != nil {
		m.Sigstore.validate(bad, len(m.Assets))
	}
	keys := make([]string, 0, len(m.Assets))
	for k := range m.Assets {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		a := m.Assets[k]
		goos, goarch, ok := strings.Cut(k, "/")
		if !ok || !contains(gooses, goos) || !contains(goarches, goarch) {
			bad("asset key %q is not goos/goarch", k)
		}
		if strings.TrimSpace(a.URL) == "" {
			bad("asset %s has no url", k)
		} else if u := strings.ToLower(a.URL); !strings.HasPrefix(u, "https://") {
			bad("asset %s url must be https (%s)", k, a.URL)
		}
		s256, s512 := strings.ToLower(strings.TrimSpace(a.SHA256)), strings.ToLower(strings.TrimSpace(a.SHA512))
		switch {
		case s256 == "" && s512 == "":
			bad("asset %s has no digest; a pin without a digest is not a pin", k)
		case s256 != "" && s512 != "":
			bad("asset %s declares both sha256 and sha512; keep the one the vendor publishes", k)
		case s256 != "" && !hexSHA256.MatchString(s256):
			bad("asset %s sha256 is not 64 hex chars", k)
		case s512 != "" && !hexSHA512.MatchString(s512):
			bad("asset %s sha512 is not 128 hex chars", k)
		}
		if m.Sigstore != nil && s256 == "" {
			bad("asset %s is signed but has no sha256; the bundle digest is checked against it", k)
		}
		if a.Tree && strings.TrimSpace(a.Entrypoint) == "" {
			bad("asset %s is a tree without an entrypoint", k)
		}
		if !a.Tree && strings.TrimSpace(a.Entrypoint) != "" {
			bad("asset %s names an entrypoint but is not a tree (use member)", k)
		}
	}
	return errors.Join(errs...)
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// managedBinaryName is the cache key and on-disk basename of the managed
// executable: the recipe's bare binary name (kimi for kimi-code), else the
// tool name.
func (t Tool) managedBinaryName() string {
	b := strings.TrimSpace(t.CLI.Binary)
	if b != "" && !isPathLike(b) {
		return b
	}
	return t.Name
}

func isPathLike(s string) bool {
	return strings.ContainsRune(s, '/') || strings.ContainsRune(s, filepath.Separator)
}

// BinmgrTool renders the managed block as the binmgr declaration Ensure
// consumes, for every platform the recipe carries. Platform selection and
// the no-asset error happen in binmgr for the running platform.
func (t Tool) BinmgrTool() (binmgr.Tool, error) {
	m := t.CLI.Managed
	if m == nil {
		return binmgr.Tool{}, fmt.Errorf("fleet: tool %q has no managed install", t.Name)
	}
	if err := m.Validate(t.Name); err != nil {
		return binmgr.Tool{}, err
	}
	version := strings.TrimSpace(m.Version)
	out := binmgr.Tool{Name: t.managedBinaryName(), Version: version, Assets: map[string]binmgr.Asset{}}
	if s := m.Sigstore; s != nil {
		out.Sigstore = &binmgr.Sigstore{Identity: strings.TrimSpace(s.Identity), Issuer: strings.TrimSpace(s.Issuer)}
	}
	for k, a := range m.Assets {
		u := strings.ReplaceAll(strings.TrimSpace(a.URL), "{version}", version)
		var bundle string
		if m.Sigstore != nil {
			bundle = m.Sigstore.bundleFor(u, version)
		}
		out.Assets[k] = binmgr.Asset{
			SigstoreBundle: bundle,
			URL:            u,
			SHA256:         strings.ToLower(strings.TrimSpace(a.SHA256)),
			SHA512:         strings.ToLower(strings.TrimSpace(a.SHA512)),
			Binary:         strings.TrimSpace(a.Member),
			Tree:           a.Tree,
			Entrypoint:     strings.TrimSpace(a.Entrypoint),
		}
	}
	return out, nil
}

// ManagedOverride reports the explicit binary an operator chose INSTEAD of
// the managed install: the BASHY_TOOL_BINARY_<NAME> variable, or a
// `cli.binary:` that is a path rather than a bare name. getenv is os.Getenv
// in production. ("", false) means the managed install is in force.
func (t Tool) ManagedOverride(getenv func(string) string) (string, bool) {
	if getenv == nil {
		getenv = os.Getenv
	}
	if p := strings.TrimSpace(getenv(ToolBinaryOverrideEnv(t.Name))); p != "" {
		return p, true
	}
	if b := strings.TrimSpace(t.CLI.Binary); isPathLike(b) {
		return b, true
	}
	return "", false
}

// ManagedBinaryPath is the cache path the managed install resolves to on this
// platform — what a launcher puts in argv[0] — computed without network. It
// is the SAME path whether or not the pin is installed yet; Cached says which.
func (t Tool) ManagedBinaryPath() (string, error) {
	bt, err := t.BinmgrTool()
	if err != nil {
		return "", err
	}
	p, err := binmgr.InstallPath(bt)
	if err != nil {
		return "", fmt.Errorf("fleet: tool %q managed install %s: %w", t.Name, bt.Version, err)
	}
	return p, nil
}

// ManagedCached reports the installed pin, with no network.
func (t Tool) ManagedCached() (string, bool) {
	bt, err := t.BinmgrTool()
	if err != nil {
		return "", false
	}
	return binmgr.Cached(bt)
}

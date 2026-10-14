package binmgr

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// PrimaVersion is the immutable bashy CI release of the reviewed first-party
// source tag bashy-prima-0.1.2. Built and signed by the bashy-build workflow
// (CI run 38486571631, green on all five platforms). The eleven raw asset
// hashes are pinned in pins.go: the prima CLI and the prima server for every
// platform, plus the Windows OpenMP runtime the executables load.
const (
	PrimaName     = "prima"
	PrimaVersion  = "bashy-bashy-prima-0.1.2-13"
	PrimaRepo     = "qiangli/prima.cpp"
	PrimaIdentity = "https://github.com/qiangli/prima.cpp/.github/workflows/bashy-build.yml@refs/tags/" + PrimaVersion
)

// PrimaServerName is the pin namespace of the prima server executable. It
// shares the release, version, workflow identity and checksum manifest with
// the CLI; only the asset bytes (and their pins) differ. The server is the
// leader binary outpost runs for rank 0; the CLI is the worker binary.
const PrimaServerName = "prima-server"

// PrimaLibOMPName is the pin namespace of the Windows OpenMP runtime the
// prima executables load. It is verified exactly like any other release
// file: committed pin, per-file Sigstore bundle, signed SHA256SUMS row.
const PrimaLibOMPName = "prima-libomp"

// PrimaLibOMPFile is the release file name of the Windows OpenMP runtime.
const PrimaLibOMPFile = "libomp.dll"

var PrimaPlatforms = []string{"linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64", "windows/amd64"}

// primaCLIFile is the release file name of the prima CLI (the worker binary)
// for a platform.
func primaCLIFile(platform string) string {
	osName, arch, _ := strings.Cut(platform, "/")
	name := "prima-" + osName + "-" + arch
	if osName == "windows" {
		name += ".exe"
	}
	return name
}

// primaServerFile is the release file name of the prima server (the leader
// binary) for a platform.
func primaServerFile(platform string) string {
	osName, arch, _ := strings.Cut(platform, "/")
	name := "prima-server-" + osName + "-" + arch
	if osName == "windows" {
		name += ".exe"
	}
	return name
}

// primaReleaseBase is the download prefix for every file of the pinned Prima
// release.
func primaReleaseBase() string {
	return "https://github.com/" + PrimaRepo + "/releases/download/" + PrimaVersion + "/"
}

// PrimaTool declares raw executables and per-asset Sigstore bundles. SHA256
// pins must be committed in pins.go before any platform can be provisioned.
func PrimaTool() Tool {
	t := Tool{Name: PrimaName, Version: PrimaVersion, Assets: make(map[string]Asset),
		Sigstore:            &Sigstore{Identity: PrimaIdentity, Issuer: GitHubActionsIssuer},
		RequireCommittedPin: true}
	base := primaReleaseBase()
	t.SignedSums = &SignedSums{URL: base + "SHA256SUMS", BundleURL: base + "SHA256SUMS.sigstore.json"}
	for _, p := range PrimaPlatforms {
		name := primaCLIFile(p)
		t.Assets[p] = Asset{URL: base + name, SigstoreBundle: base + name + DefaultBundleSuffix}
	}
	return t
}

// EnsurePrima refuses missing committed pins before network or cache access.
func EnsurePrima(ctx context.Context) (string, error) { return Ensure(ctx, PrimaTool()) }

// primaCompanionTool is the verification declaration for one release file
// that installs into the prima version dir beside the CLI: the server
// executable (every platform) or the Windows OpenMP runtime. The pin
// namespace differs per file, so each resolves its own committed digest;
// identity, issuer and checksum manifest are the release's.
func primaCompanionTool(pinName, file string) Tool {
	base := primaReleaseBase()
	return Tool{Name: pinName, Version: PrimaVersion,
		Assets:              map[string]Asset{Platform(): {URL: base + file, SigstoreBundle: base + file + DefaultBundleSuffix}},
		Sigstore:            &Sigstore{Identity: PrimaIdentity, Issuer: GitHubActionsIssuer},
		SignedSums:          &SignedSums{URL: base + "SHA256SUMS", BundleURL: base + "SHA256SUMS.sigstore.json"},
		RequireCommittedPin: true}
}

// PrimaServerPath is where EnsurePrimaServer installs the server executable:
// the prima version dir, beside the CLI. It touches neither the network nor
// the filesystem. Errors exactly where the ensure would: no committed pin
// for this platform.
func PrimaServerPath() (string, error) {
	return primaCompanionPath(PrimaServerName, primaServerFile(Platform()))
}

// PrimaLibOMPPath is where EnsurePrimaLibOMP installs the Windows OpenMP
// runtime: the prima version dir, beside the two executables. Windows only.
func PrimaLibOMPPath() (string, error) {
	if Platform() != "windows/amd64" {
		return "", fmt.Errorf("binmgr: %s %s ships only for windows/amd64", PrimaLibOMPName, PrimaVersion)
	}
	return primaCompanionPath(PrimaLibOMPName, PrimaLibOMPFile)
}

func primaCompanionPath(pinName, file string) (string, error) {
	cli, err := InstallPath(PrimaTool())
	if err != nil {
		return "", err
	}
	if _, ok := pinnedSHA256(pinName, PrimaVersion, Platform()); !ok {
		return "", fmt.Errorf("binmgr: %s %s has no committed sha256 for %s", pinName, PrimaVersion, Platform())
	}
	return filepath.Join(filepath.Dir(cli), file), nil
}

// EnsurePrimaServer provisions the prima server executable (the leader
// binary) into the prima version dir, beside the CLI. Verification is the
// same fail-closed path as Ensure: committed pin, per-file Sigstore bundle
// with the exact release identity, and the signed SHA256SUMS row — before
// anything is installed, and re-checked on every cache hit.
func EnsurePrimaServer(ctx context.Context) (string, error) {
	decl := primaCompanionTool(PrimaServerName, primaServerFile(Platform()))
	asset, ok := decl.Assets[Platform()]
	if !ok || asset.URL == "" {
		return "", fmt.Errorf("binmgr: %s %s has no asset for %s", PrimaServerName, PrimaVersion, Platform())
	}
	pin, ok := pinnedSHA256(PrimaServerName, PrimaVersion, Platform())
	if !ok {
		return "", fmt.Errorf("binmgr: %s %s has no committed sha256 for %s", PrimaServerName, PrimaVersion, Platform())
	}
	dest, err := primaCompanionPath(PrimaServerName, primaServerFile(Platform()))
	if err != nil {
		return "", err
	}
	return installVerifiedCompanion(ctx, decl, asset, dest, pin, true)
}

// EnsurePrimaLibOMP provisions the Windows OpenMP runtime beside the prima
// executables, so the two .exe files and libomp.dll install side by side.
// Windows only; any other platform refuses fail-closed.
func EnsurePrimaLibOMP(ctx context.Context) (string, error) {
	if Platform() != "windows/amd64" {
		return "", fmt.Errorf("binmgr: %s %s ships only for windows/amd64", PrimaLibOMPName, PrimaVersion)
	}
	decl := primaCompanionTool(PrimaLibOMPName, PrimaLibOMPFile)
	asset := decl.Assets[Platform()]
	pin, ok := pinnedSHA256(PrimaLibOMPName, PrimaVersion, Platform())
	if !ok {
		return "", fmt.Errorf("binmgr: %s %s has no committed sha256 for %s", PrimaLibOMPName, PrimaVersion, Platform())
	}
	dest, err := primaCompanionPath(PrimaLibOMPName, PrimaLibOMPFile)
	if err != nil {
		return "", err
	}
	return installVerifiedCompanion(ctx, decl, asset, dest, pin, false)
}

// installVerifiedCompanion downloads one release asset, verifies it through
// the full fail-closed path, and installs it at dest. It mirrors Ensure's
// order: the committed pin is authoritative (release checksums are
// discarded), then the signed SHA256SUMS row, then the per-file Sigstore
// bundle, then the digest — before anything is installed, with the evidence
// cached for offline re-verification. A cache hit re-verifies the evidence
// (bundle, sums, cosign offline) and refuses on anything stale or tampered.
func installVerifiedCompanion(ctx context.Context, decl Tool, asset Asset, dest, pin string, executable bool) (string, error) {
	if strings.TrimSpace(pin) == "" {
		return "", fmt.Errorf("binmgr: %s %s has no committed sha256 for %s", decl.Name, decl.Version, Platform())
	}
	// The committed pin is authoritative: verify against OUR digest and
	// discard whatever the release resolved to.
	asset.SHA256, asset.SHA512, asset.MD5 = pin, "", ""
	dir := filepath.Dir(dest)
	present := isExecutable(dest)
	if !executable {
		fi, err := os.Stat(dest)
		present = err == nil && !fi.IsDir()
	}
	if present {
		if !signedCacheValid(ctx, decl, asset, dest) {
			return "", fmt.Errorf("%w: %s %s has an unverified or stale cache entry", ErrSigstore, decl.Name, decl.Version)
		}
		return dest, nil
	}
	if Offline() {
		return "", offlineMissing(decl.Name, decl.Version)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, ".dl-*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	sum, _, _, derr := download(ctx, asset.URL, tmp)
	_ = tmp.Close()
	if derr != nil {
		return "", derr
	}
	var sums, sumsBundle []byte
	sums, sumsBundle, err = verifySignedSums(ctx, decl, asset, sum)
	if err != nil {
		return "", err
	}
	var bundle []byte
	bundle, err = verifySigstore(ctx, decl, asset, tmpName, sum)
	if err != nil {
		return "", err
	}
	if want := strings.ToLower(strings.TrimSpace(pin)); want != sum {
		return "", fmt.Errorf("binmgr: %s %s sha256 mismatch: got %s, want %s", decl.Name, decl.Version, sum, want)
	}
	if err := os.Rename(tmpName, dest); err != nil {
		if cerr := copyFile(tmpName, dest); cerr != nil {
			return "", err
		}
	}
	if executable {
		if err := os.Chmod(dest, 0o755); err != nil {
			return "", err
		}
	} else if err := os.Chmod(dest, 0o644); err != nil {
		return "", err
	}
	if err := writeSignedCacheEvidence(asset, dest, tmpName, bundle); err != nil {
		return "", err
	}
	if err := writeSignedSumsEvidence(dest, sums, sumsBundle); err != nil {
		return "", err
	}
	return dest, nil
}

package binmgr

import (
	"context"
	"strings"
)

// PrimaVersion is the immutable bashy CI release of the reviewed first-party
// source tag bashy-prima-0.1.1. The four raw asset hashes are pinned in pins.go.
const (
	PrimaName     = "prima"
	PrimaVersion  = "bashy-bashy-prima-0.1.1-4"
	PrimaRepo     = "qiangli/prima.cpp"
	PrimaIdentity = "https://github.com/qiangli/prima.cpp/.github/workflows/bashy-build.yml@refs/tags/" + PrimaVersion
)

var PrimaPlatforms = []string{"linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"}

// PrimaTool declares raw executables and per-asset Sigstore bundles. SHA256
// pins must be committed in pins.go before any platform can be provisioned.
func PrimaTool() Tool {
	t := Tool{Name: PrimaName, Version: PrimaVersion, Assets: make(map[string]Asset),
		Sigstore:            &Sigstore{Identity: PrimaIdentity, Issuer: GitHubActionsIssuer},
		RequireCommittedPin: true}
	base := "https://github.com/" + PrimaRepo + "/releases/download/" + PrimaVersion + "/"
	t.SignedSums = &SignedSums{URL: base + "SHA256SUMS", BundleURL: base + "SHA256SUMS.sigstore.json"}
	for _, p := range PrimaPlatforms {
		os, arch, _ := strings.Cut(p, "/")
		name := "prima-" + os + "-" + arch
		t.Assets[p] = Asset{URL: base + name, SigstoreBundle: base + name + DefaultBundleSuffix}
	}
	return t
}

// EnsurePrima refuses missing committed pins before network or cache access.
func EnsurePrima(ctx context.Context) (string, error) { return Ensure(ctx, PrimaTool()) }

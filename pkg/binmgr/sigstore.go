package binmgr

// KEYLESS SIGSTORE VERIFICATION — the service supply-chain gate (Sprint 433,
// docs/bashy-service-supply-chain-policy.md). bashy and outpost never start a
// service built by a third party: a service binary must be built by bashy CI
// and signed keylessly with Sigstore (cosign keyless, GitHub Actions OIDC).
// When a Tool declares Sigstore, Ensure fetches the asset's bundle and
// verifies it over the DOWNLOADED BYTES before anything is decompressed,
// installed or executed:
//
//  1. here, in Go, with the standard library only: the bundle's leaf
//     certificate names EXACTLY the pinned identity (URI SAN) and EXACTLY the
//     pinned OIDC issuer (Fulcio extension), the bundle's artifact digest is
//     the pinned sha256 and the downloaded bytes' sha256, and the signature
//     verifies over those bytes with the certificate's key;
//  2. then by a binmgr-pinned cosign (`cosign verify-blob --bundle …
//     --certificate-identity … --certificate-oidc-issuer … --trusted-root …
//     --offline`), which checks the certificate chain to Fulcio and the
//     transparency-log inclusion against the trusted root EMBEDDED in this
//     package — no TUF fetch at verify time;
//  3. then the existing sha256 check.
//
// Any failure refuses the install and nothing is written to the cache.
//
// WHY EXEC COSIGN AND NOT LINK sigstore-go: bashy is permissive-only.
// sigstore-go (Apache-2.0) requires github.com/letsencrypt/boulder and
// github.com/hashicorp/go-retryablehttp, both MPL-2.0, so linking it would
// put copyleft modules in yoke's module graph. cosign is exec'd as a pinned
// external (Apache-2.0, download + exec, never linked).

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	_ "embed"
	"encoding/asn1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// GitHubActionsIssuer is the only OIDC issuer a bashy-signed build may carry:
// keyless signing from a bashy CI workflow on GitHub Actions.
const GitHubActionsIssuer = "https://token.actions.githubusercontent.com"

// DefaultBundleSuffix is appended to an asset URL when a pin names no bundle.
const DefaultBundleSuffix = ".sigstore.json"

// Sigstore is a tool's keyless signing identity: the exact certificate
// identity (URI SAN — a GitHub Actions workflow ref) and OIDC issuer that
// must have signed every asset.
type Sigstore struct {
	// Identity is compared EXACTLY (no regexp), e.g.
	// https://github.com/OWNER/REPO/.github/workflows/release.yml@refs/tags/v1.2.3
	Identity string `json:"identity"`
	// Issuer is compared exactly; GitHubActionsIssuer for bashy CI.
	Issuer string `json:"issuer"`
}

// ErrSigstore marks every keyless-verification refusal.
var ErrSigstore = errors.New("binmgr: sigstore verification failed")

// trustedRoot is the Sigstore public-good trusted root (Fulcio CAs, Rekor
// and CT log keys, TSAs), pinned in this repository: the TUF target
// trusted_root.json as shipped by github.com/sigstore/sigstore v1.10.8
// (Apache-2.0). Its digest is checked before every use, so the root changes
// only through a reviewed edit of both the file and trustedRootSHA256.
//
//go:embed sigstore/trusted_root.json
var trustedRoot []byte

const trustedRootSHA256 = "4364d7724c04cc912ce2a6c45ed2610e8d8d1c4dc857fb500292738d4d9c8d2c"

// TrustedRoot returns the pinned trusted root, refusing a root whose digest
// does not match its pin.
func TrustedRoot() ([]byte, error) {
	sum := sha256.Sum256(trustedRoot)
	if hex.EncodeToString(sum[:]) != trustedRootSHA256 {
		return nil, fmt.Errorf("%w: embedded trusted root does not match its pinned sha256", ErrSigstore)
	}
	return trustedRoot, nil
}

// cosign is binmgr's own pinned verifier. Its trust is the committed sha256
// pin (pinnedDigests, key "cosign@<version>/<goos>/<goarch>") plus the
// upstream signing identity recorded below; until a digest is committed for
// the running platform, signed installs are refused (fail closed) rather than
// trusting a cosign resolved from the network.
const (
	CosignVersion  = "v3.1.1"
	CosignIdentity = "https://github.com/sigstore/cosign/.github/workflows/release.yml@refs/tags/" + CosignVersion
)

// CosignTool is the binmgr declaration of the pinned cosign.
func CosignTool() Tool {
	t := Tool{Name: "cosign", Version: CosignVersion, Assets: map[string]Asset{}}
	for _, p := range []string{"darwin/amd64", "darwin/arm64", "linux/amd64", "linux/arm64", "windows/amd64"} {
		goos, goarch, _ := strings.Cut(p, "/")
		name := "cosign-" + goos + "-" + goarch
		if goos == "windows" {
			name += ".exe"
		}
		t.Assets[p] = Asset{URL: "https://github.com/sigstore/cosign/releases/download/" + CosignVersion + "/" + name}
	}
	return t
}

// cosignBinary resolves the pinned cosign; a seam for tests.
var cosignBinary func(ctx context.Context) (string, error)

func init() { cosignBinary = resolveCosign }

func resolveCosign(ctx context.Context) (string, error) {
	t := CosignTool()
	if _, ok := pinnedSHA256(t.Name, t.Version, Platform()); !ok {
		return "", fmt.Errorf("%w: no committed sha256 pin for cosign %s on %s (pkg/binmgr/pins.go); signed installs are refused until one lands", ErrSigstore, t.Version, Platform())
	}
	return Ensure(ctx, t)
}

// runCosign execs cosign; a seam for tests.
var runCosign = func(ctx context.Context, bin string, args []string) error {
	cmd := exec.CommandContext(ctx, bin, args...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(out.String()))
	}
	return nil
}

// CosignVerifyArgs is the exact verify-blob argv binmgr runs.
func CosignVerifyArgs(artifact, bundle, trustedRootPath string, s Sigstore) []string {
	return []string{"verify-blob",
		"--bundle", bundle,
		"--certificate-identity", s.Identity,
		"--certificate-oidc-issuer", s.Issuer,
		"--trusted-root", trustedRootPath,
		"--offline",
		artifact,
	}
}

// maxBundleSize caps a downloaded bundle (real ones are a few KiB).
const maxBundleSize = 1 << 20

// bundleURL is the asset's bundle location: its own, else URL+suffix.
func bundleURL(a Asset) string {
	if b := strings.TrimSpace(a.SigstoreBundle); b != "" {
		return b
	}
	return a.URL + DefaultBundleSuffix
}

// A signed cache entry needs a receipt from a completed bundle and cosign
// verification. Its binary digest also detects a changed executable on reuse.
type signedCacheReceipt struct {
	Pin, Identity, Issuer, URL, Bundle, BinarySHA256 string
}

func signedCacheReceiptPath(dest string) string { return dest + ".sigstore-verified.json" }

func signedCacheValid(t Tool, asset Asset, dest string) bool {
	if t.Sigstore == nil {
		return false
	}
	pin, ok := pinnedSHA256(t.Name, t.Version, Platform())
	if !ok {
		if t.RequireCommittedPin {
			return false
		}
		pin = strings.ToLower(strings.TrimSpace(asset.SHA256))
	}
	if pin == "" {
		return false
	}
	raw, err := os.ReadFile(signedCacheReceiptPath(dest))
	if err != nil {
		return false
	}
	var r signedCacheReceipt
	if json.Unmarshal(raw, &r) != nil || r.Pin != pin || r.Identity != t.Sigstore.Identity ||
		r.Issuer != t.Sigstore.Issuer || r.URL != asset.URL || r.Bundle != bundleURL(asset) {
		return false
	}
	data, err := os.ReadFile(dest)
	if err != nil {
		return false
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]) == r.BinarySHA256
}

func writeSignedCacheReceipt(t Tool, asset Asset, dest string) error {
	pin, ok := pinnedSHA256(t.Name, t.Version, Platform())
	if !ok {
		pin = strings.ToLower(strings.TrimSpace(asset.SHA256))
	}
	data, err := os.ReadFile(dest)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	r := signedCacheReceipt{pin, t.Sigstore.Identity, t.Sigstore.Issuer,
		asset.URL, bundleURL(asset), hex.EncodeToString(sum[:])}
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	path := signedCacheReceiptPath(dest)
	tmp, err := os.CreateTemp(filepath.Dir(path), ".receipt-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err = tmp.Write(raw); err == nil {
		err = tmp.Close()
	} else {
		_ = tmp.Close()
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// verifySigstore runs the whole keyless check for one downloaded artifact.
// sum is the downloaded bytes' sha256; asset.SHA256 is the pin.
func verifySigstore(ctx context.Context, t Tool, asset Asset, artifact, sum string) error {
	s := *t.Sigstore
	fail := func(format string, a ...any) error {
		return fmt.Errorf("%w: %s %s: "+format, append([]any{ErrSigstore, t.Name, t.Version}, a...)...)
	}
	if strings.TrimSpace(s.Identity) == "" || strings.TrimSpace(s.Issuer) == "" {
		return fail("identity and issuer are required")
	}
	pin := strings.ToLower(strings.TrimSpace(asset.SHA256))
	if pin == "" {
		return fail("a signed asset needs a pinned sha256")
	}
	dir := filepath.Dir(artifact)
	bf, err := os.CreateTemp(dir, ".bundle-*")
	if err != nil {
		return err
	}
	bundlePath := bf.Name()
	defer os.Remove(bundlePath)
	_, _, _, derr := download(ctx, bundleURL(asset), &limitWriter{w: bf, n: maxBundleSize})
	_ = bf.Close()
	if derr != nil {
		return fail("fetch bundle: %v", derr)
	}
	raw, err := os.ReadFile(bundlePath)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(artifact)
	if err != nil {
		return err
	}
	if err := checkBundle(raw, data, pin, sum, s); err != nil {
		return fail("%v", err)
	}
	root, err := TrustedRoot()
	if err != nil {
		return err
	}
	rootPath := filepath.Join(dir, ".trusted_root.json")
	if err := os.WriteFile(rootPath, root, 0o600); err != nil {
		return err
	}
	defer os.Remove(rootPath)
	bin, err := cosignBinary(ctx)
	if err != nil {
		return err
	}
	if err := runCosign(ctx, bin, CosignVerifyArgs(artifact, bundlePath, rootPath, s)); err != nil {
		return fail("cosign verify-blob: %v", err)
	}
	return nil
}

type limitWriter struct {
	w io.Writer
	n int64
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > l.n {
		return 0, errors.New("bundle exceeds size limit")
	}
	l.n -= int64(len(p))
	return l.w.Write(p)
}

// bundleJSON is the subset of a Sigstore bundle (v0.1–v0.3, JSON encoding)
// the Go-side check reads.
type bundleJSON struct {
	MediaType            string `json:"mediaType"`
	VerificationMaterial struct {
		Certificate *struct {
			RawBytes string `json:"rawBytes"`
		} `json:"certificate"`
		X509CertificateChain *struct {
			Certificates []struct {
				RawBytes string `json:"rawBytes"`
			} `json:"certificates"`
		} `json:"x509CertificateChain"`
	} `json:"verificationMaterial"`
	MessageSignature *struct {
		MessageDigest struct {
			Algorithm string `json:"algorithm"`
			Digest    string `json:"digest"`
		} `json:"messageDigest"`
		Signature string `json:"signature"`
	} `json:"messageSignature"`
	DSSEEnvelope json.RawMessage `json:"dsseEnvelope"`
}

// Fulcio certificate extensions carrying the OIDC issuer.
var (
	oidFulcioIssuerV1 = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 1}
	oidFulcioIssuerV2 = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 8}
)

// checkBundle is the Go-side half of the verification: exact identity, exact
// issuer, artifact digest == pin == downloaded bytes, signature valid over the
// bytes. Chain and transparency-log checks are cosign's.
func checkBundle(raw, artifact []byte, pin, sum string, s Sigstore) error {
	var b bundleJSON
	if err := json.Unmarshal(raw, &b); err != nil {
		return fmt.Errorf("bundle is not JSON: %v", err)
	}
	if !strings.HasPrefix(b.MediaType, "application/vnd.dev.sigstore.bundle") {
		return fmt.Errorf("bundle media type %q is not a Sigstore bundle", b.MediaType)
	}
	if b.MessageSignature == nil {
		if len(b.DSSEEnvelope) > 0 {
			return errors.New("bundle is a DSSE attestation; a blob signature is required")
		}
		return errors.New("bundle carries no message signature")
	}
	var certB64 string
	switch vm := b.VerificationMaterial; {
	case vm.Certificate != nil:
		certB64 = vm.Certificate.RawBytes
	case vm.X509CertificateChain != nil && len(vm.X509CertificateChain.Certificates) > 0:
		certB64 = vm.X509CertificateChain.Certificates[0].RawBytes
	default:
		return errors.New("bundle carries no certificate (a keyed signature is not a keyless bashy build)")
	}
	der, err := base64.StdEncoding.DecodeString(certB64)
	if err != nil {
		return fmt.Errorf("certificate encoding: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return fmt.Errorf("certificate: %v", err)
	}
	var ids []string
	for _, u := range cert.URIs {
		ids = append(ids, u.String())
	}
	ids = append(ids, cert.EmailAddresses...)
	if len(ids) != 1 || ids[0] != s.Identity {
		return fmt.Errorf("certificate identity %q, want exactly %q", strings.Join(ids, ","), s.Identity)
	}
	if iss := certIssuer(cert); iss != s.Issuer {
		return fmt.Errorf("certificate OIDC issuer %q, want exactly %q", iss, s.Issuer)
	}
	md := b.MessageSignature.MessageDigest
	if md.Algorithm != "SHA2_256" {
		return fmt.Errorf("bundle digest algorithm %q, want SHA2_256", md.Algorithm)
	}
	d, err := base64.StdEncoding.DecodeString(md.Digest)
	if err != nil {
		return fmt.Errorf("bundle digest encoding: %v", err)
	}
	if got := hex.EncodeToString(d); got != pin {
		return fmt.Errorf("bundle artifact digest %s != pinned sha256 %s", got, pin)
	}
	if sum != pin {
		return fmt.Errorf("downloaded sha256 %s != pinned sha256 %s", sum, pin)
	}
	sig, err := base64.StdEncoding.DecodeString(b.MessageSignature.Signature)
	if err != nil {
		return fmt.Errorf("signature encoding: %v", err)
	}
	h := sha256.Sum256(artifact)
	if !bytes.Equal(h[:], d) {
		return errors.New("artifact bytes do not match the bundle digest")
	}
	ok := false
	switch pub := cert.PublicKey.(type) {
	case *ecdsa.PublicKey:
		ok = ecdsa.VerifyASN1(pub, h[:], sig)
	case *rsa.PublicKey:
		ok = cert.CheckSignature(x509.SHA256WithRSA, artifact, sig) == nil
	case ed25519.PublicKey:
		ok = ed25519.Verify(pub, artifact, sig)
	}
	if !ok {
		return errors.New("signature does not verify over the artifact")
	}
	return nil
}

// certIssuer reads the Fulcio OIDC-issuer extension (v2 DER UTF8String,
// else v1 raw bytes).
func certIssuer(cert *x509.Certificate) string {
	var v1 string
	for _, e := range cert.Extensions {
		switch {
		case e.Id.Equal(oidFulcioIssuerV2):
			var s string
			if _, err := asn1.Unmarshal(e.Value, &s); err == nil {
				return s
			}
		case e.Id.Equal(oidFulcioIssuerV1):
			v1 = string(e.Value)
		}
	}
	return v1
}

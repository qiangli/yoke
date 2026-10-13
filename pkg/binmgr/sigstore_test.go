package binmgr

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

const (
	testIdentity = "https://github.com/bashy/builds/.github/workflows/release.yml@refs/tags/v1.0.0"
	testIssuer   = GitHubActionsIssuer
)

// signedBundle builds a Sigstore-style bundle (v0.3 JSON) over artifact with
// an in-test CA: a leaf whose URI SAN is identity and whose Fulcio v2
// extension is issuer, an ECDSA P-256 signature, and digestOf as the bundle's
// message digest.
func signedBundle(t *testing.T, artifact, digestOf []byte, identity, issuer string) []byte {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test-fulcio"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, _ := x509.ParseCertificate(caDER)
	leafKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	u, _ := url.Parse(identity)
	iss, _ := asn1.Marshal(issuer)
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), NotBefore: time.Now().Add(-time.Minute),
		NotAfter: time.Now().Add(10 * time.Minute), URIs: []*url.URL{u},
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning},
		ExtraExtensions: []pkix.Extension{{Id: oidFulcioIssuerV2, Value: iss}}}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(artifact)
	sig, err := ecdsa.SignASN1(rand.Reader, leafKey, h[:])
	if err != nil {
		t.Fatal(err)
	}
	d := sha256.Sum256(digestOf)
	b := map[string]any{
		"mediaType": "application/vnd.dev.sigstore.bundle.v0.3+json",
		"verificationMaterial": map[string]any{
			"certificate": map[string]any{"rawBytes": base64.StdEncoding.EncodeToString(leafDER)},
		},
		"messageSignature": map[string]any{
			"messageDigest": map[string]any{"algorithm": "SHA2_256", "digest": base64.StdEncoding.EncodeToString(d[:])},
			"signature":     base64.StdEncoding.EncodeToString(sig),
		},
	}
	out, _ := json.Marshal(b)
	return out
}

// fakeCosign replaces the pinned cosign with a recorder; err is what the
// "chain + tlog" half returns.
func fakeCosign(t *testing.T, err error) *[][]string {
	t.Helper()
	var calls [][]string
	oldBin, oldRun := cosignBinary, runCosign
	cosignBinary = func(context.Context) (string, error) { return "/pinned/cosign", nil }
	runCosign = func(_ context.Context, bin string, args []string) error {
		if bin != "/pinned/cosign" {
			t.Errorf("cosign bin %q", bin)
		}
		calls = append(calls, args)
		return err
	}
	t.Cleanup(func() { cosignBinary, runCosign = oldBin, oldRun })
	return &calls
}

type signedCase struct {
	served   []byte // bytes the release serves
	signed   []byte // bytes the bundle signs
	digestOf []byte // bytes the bundle's digest names
	pinned   []byte // bytes whose sha256 the pin names
	identity string
	issuer   string
}

func runSigned(t *testing.T, c signedCase, cosignErr error) (string, error, *[][]string) {
	t.Helper()
	bundle := signedBundle(t, c.signed, c.digestOf, c.identity, c.issuer)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/svc":
			_, _ = w.Write(c.served)
		case "/svc" + DefaultBundleSuffix:
			_, _ = w.Write(bundle)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("BASHY_BIN_CACHE", t.TempDir())
	calls := fakeCosign(t, cosignErr)
	tool := Tool{Name: "signedsvc", Version: "v1.0.0",
		Assets:   map[string]Asset{Platform(): {URL: srv.URL + "/svc", SHA256: sha256hex(c.pinned)}},
		Sigstore: &Sigstore{Identity: testIdentity, Issuer: testIssuer}}
	p, err := Ensure(context.Background(), tool)
	return p, err, calls
}

func TestEnsureSigstore(t *testing.T) {
	good := []byte("#!/bin/sh\necho signed\n")
	evil := []byte("#!/bin/sh\necho evil\n")
	base := signedCase{served: good, signed: good, digestOf: good, pinned: good, identity: testIdentity, issuer: testIssuer}

	t.Run("valid bundle identity issuer sha installs", func(t *testing.T) {
		p, err, calls := runSigned(t, base, nil)
		if err != nil {
			t.Fatal(err)
		}
		if b, _ := os.ReadFile(p); string(b) != string(good) {
			t.Fatalf("installed %q", b)
		}
		if len(*calls) != 1 {
			t.Fatalf("cosign calls %d", len(*calls))
		}
		args := (*calls)[0]
		for _, want := range [][]string{
			{"--certificate-identity", testIdentity},
			{"--certificate-oidc-issuer", testIssuer},
		} {
			i := slices.Index(args, want[0])
			if i < 0 || args[i+1] != want[1] {
				t.Errorf("cosign argv %v lacks %v", args, want)
			}
		}
		if args[0] != "verify-blob" || !slices.Contains(args, "--offline") || !slices.Contains(args, "--trusted-root") || !slices.Contains(args, "--bundle") {
			t.Errorf("cosign argv %v", args)
		}
	})

	refused := map[string]signedCase{}
	c := base
	c.identity = "https://github.com/someone-else/fork/.github/workflows/release.yml@refs/tags/v1.0.0"
	refused["wrong identity"] = c
	c = base
	c.identity = testIdentity + "x"
	refused["identity prefix only"] = c
	c = base
	c.issuer = "https://accounts.google.com"
	refused["wrong issuer"] = c
	c = base
	c.served = evil
	refused["tampered asset"] = c
	c = base
	c.served, c.signed, c.digestOf = evil, evil, evil
	refused["bundle digest != pinned sha"] = c
	c = base
	c.signed = evil
	refused["signature over other bytes"] = c
	for name, sc := range refused {
		t.Run(name, func(t *testing.T) {
			p, err, calls := runSigned(t, sc, nil)
			if !errors.Is(err, ErrSigstore) {
				t.Fatalf("want ErrSigstore, got path=%q err=%v", p, err)
			}
			if len(*calls) != 0 {
				t.Errorf("cosign ran after the Go-side check failed")
			}
			assertNothingInstalled(t)
		})
	}

	t.Run("cosign chain rejection refuses", func(t *testing.T) {
		_, err, _ := runSigned(t, base, errors.New("certificate not issued by the trusted root"))
		if !errors.Is(err, ErrSigstore) {
			t.Fatalf("got %v", err)
		}
		assertNothingInstalled(t)
	})

	t.Run("missing bundle refuses", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/svc" {
				_, _ = w.Write(good)
				return
			}
			http.NotFound(w, r)
		}))
		defer srv.Close()
		t.Setenv("BASHY_BIN_CACHE", t.TempDir())
		fakeCosign(t, nil)
		_, err := Ensure(context.Background(), Tool{Name: "signedsvc", Version: "v1.0.0",
			Assets:   map[string]Asset{Platform(): {URL: srv.URL + "/svc", SHA256: sha256hex(good)}},
			Sigstore: &Sigstore{Identity: testIdentity, Issuer: testIssuer}})
		if !errors.Is(err, ErrSigstore) {
			t.Fatalf("got %v", err)
		}
		assertNothingInstalled(t)
	})
}

func TestSignedCacheReverifiesOffline(t *testing.T) {
	good := []byte("#!/bin/sh\necho signed\n")
	bundle := signedBundle(t, good, good, testIdentity, testIssuer)
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path == "/svc"+DefaultBundleSuffix {
			_, _ = w.Write(bundle)
		} else if r.URL.Path == "/svc" {
			_, _ = w.Write(good)
		} else {
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	t.Setenv("BASHY_BIN_CACHE", t.TempDir())
	withPin(t, "signedsvc@v1.0.0/"+Platform(), sha256hex(good))
	calls := fakeCosign(t, nil)
	tool := Tool{Name: "signedsvc", Version: "v1.0.0",
		Assets:   map[string]Asset{Platform(): {URL: srv.URL + "/svc", SHA256: sha256hex(good)}},
		Sigstore: &Sigstore{Identity: testIdentity, Issuer: testIssuer}}
	dest, err := InstallPath(tool)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}
	// This was sufficient to bypass verification: the receipt is plain JSON
	// controlled by whoever can write the cached binary.
	if err := os.WriteFile(dest, good, 0o755); err != nil {
		t.Fatal(err)
	}
	receipt := map[string]string{"Pin": sha256hex(good), "Identity": testIdentity,
		"Issuer": testIssuer, "URL": srv.URL + "/svc", "Bundle": srv.URL + "/svc" + DefaultBundleSuffix,
		"BinarySHA256": sha256hex(good)}
	raw, _ := json.Marshal(receipt)
	if err := os.WriteFile(dest+".sigstore-verified.json", raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Ensure(context.Background(), tool); !errors.Is(err, ErrSigstore) {
		t.Fatalf("forged receipt admitted: %v", err)
	}
	if _, ok := Cached(tool); ok || requests != 0 || len(*calls) != 0 {
		t.Fatalf("forged receipt: cached=%v requests=%d cosign=%d", ok, requests, len(*calls))
	}
	if err := os.Remove(dest); err != nil {
		t.Fatal(err)
	}
	if _, err := Ensure(context.Background(), tool); err != nil {
		t.Fatal(err)
	}
	if requests != 2 || len(*calls) != 1 {
		t.Fatalf("install requests=%d cosign=%d", requests, len(*calls))
	}
	t.Setenv("BASHY_OFFLINE", "1")
	if _, err := Ensure(context.Background(), tool); err != nil {
		t.Fatalf("offline verified cache: %v", err)
	}
	if _, ok := Cached(tool); !ok || requests != 2 || len(*calls) != 3 {
		t.Fatalf("offline cache: cached=%v requests=%d cosign=%d", ok, requests, len(*calls))
	}
	if err := os.WriteFile(signedCacheBundlePath(dest), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Ensure(context.Background(), tool); !errors.Is(err, ErrSigstore) {
		t.Fatalf("tampered bundle admitted: %v", err)
	}
	if err := os.Remove(signedCacheBundlePath(dest)); err != nil {
		t.Fatal(err)
	}
	if _, err := Ensure(context.Background(), tool); !errors.Is(err, ErrSigstore) {
		t.Fatalf("missing bundle admitted: %v", err)
	}
	if err := os.WriteFile(signedCacheBundlePath(dest), bundle, 0o600); err != nil {
		t.Fatal(err)
	}
	// The Go-side check succeeds, then chain/transparency verification rejects.
	runCosign = func(context.Context, string, []string) error { return errors.New("transparency proof rejected") }
	if _, err := Ensure(context.Background(), tool); !errors.Is(err, ErrSigstore) {
		t.Fatalf("cosign rejection admitted: %v", err)
	}
}

// Without Sigstore the sha256-only path is unchanged: no bundle fetched, no
// cosign run.
func TestEnsureUnsignedToolUnchanged(t *testing.T) {
	payload := []byte("#!/bin/sh\necho plain\n")
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		_, _ = w.Write(payload)
	}))
	defer srv.Close()
	t.Setenv("BASHY_BIN_CACHE", t.TempDir())
	calls := fakeCosign(t, nil)
	if _, err := Ensure(context.Background(), toolFor(srv.URL+"/bin", sha256hex(payload), "")); err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || paths[0] != "/bin" || len(*calls) != 0 {
		t.Fatalf("requests %v cosign %d", paths, len(*calls))
	}
}

func TestTrustedRootPinned(t *testing.T) {
	if _, err := TrustedRoot(); err != nil {
		t.Fatal(err)
	}
}

// Real cosign is never resolved from the network without a committed pin.
func TestCosignRequiresCommittedPin(t *testing.T) {
	t.Setenv("BASHY_BIN_CACHE", t.TempDir())
	if _, ok := pinnedSHA256("cosign", CosignVersion, Platform()); ok {
		t.Skip("cosign pinned for this platform")
	}
	if _, err := resolveCosign(context.Background()); !errors.Is(err, ErrSigstore) {
		t.Fatalf("got %v", err)
	}
}

func TestCachedCosignMustMatchPin(t *testing.T) {
	if _, ok := pinnedSHA256("cosign", CosignVersion, Platform()); !ok {
		t.Skip("cosign not pinned on this platform")
	}
	t.Setenv("BASHY_BIN_CACHE", t.TempDir())
	dest, err := InstallPath(CosignTool())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, []byte("forged cosign"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveCosign(context.Background()); !errors.Is(err, ErrSigstore) {
		t.Fatalf("forged cached cosign admitted: %v", err)
	}
}

func assertNothingInstalled(t *testing.T) {
	t.Helper()
	tool := Tool{Name: "signedsvc", Version: "v1.0.0", Assets: map[string]Asset{Platform(): {URL: "https://x/svc"}}}
	if p, ok := Cached(tool); ok {
		t.Fatalf("refused install left %s in the cache", p)
	}
}

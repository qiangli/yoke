package binmgr

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// SignedSums names the release's signed SHA256SUMS and Sigstore bundle.
// The signature uses the Tool's exact workflow identity and OIDC issuer.
type SignedSums struct {
	URL       string `json:"url"`
	BundleURL string `json:"bundle_url"`
}

const maxSignedSumsSize = 1 << 20

func sumsCachePath(dest string) string       { return dest + ".SHA256SUMS" }
func sumsBundleCachePath(dest string) string { return dest + ".SHA256SUMS.sigstore.json" }

func signedSumsNames(t Tool) (map[string]bool, error) {
	if len(t.Assets) == 0 {
		return nil, fmt.Errorf("no declared assets")
	}
	want := make(map[string]bool, len(t.Assets))
	for _, asset := range t.Assets {
		u, err := url.Parse(asset.URL)
		if err != nil || u.Path == "" || u.RawQuery != "" || u.Fragment != "" {
			return nil, fmt.Errorf("invalid asset URL %q", asset.URL)
		}
		name := path.Base(u.Path)
		if name == "." || name == ".." || name == "" || strings.ContainsAny(name, "\\\r\n") || want[name] {
			return nil, fmt.Errorf("invalid or duplicate asset name %q", name)
		}
		want[name] = true
	}
	return want, nil
}

// parseSignedSums accepts exactly one conventional sha256sum row per declared
// asset. No paths, alternate spellings, comments, empty rows, or extra assets.
func parseSignedSums(t Tool, raw []byte) (map[string]string, error) {
	want, err := signedSumsNames(t)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 || len(raw) > maxSignedSumsSize || raw[len(raw)-1] != '\n' {
		return nil, fmt.Errorf("SHA256SUMS is empty, oversized, or lacks a final newline")
	}
	rows := strings.Split(string(raw[:len(raw)-1]), "\n")
	if len(rows) != len(want) {
		return nil, fmt.Errorf("SHA256SUMS has %d rows, want %d", len(rows), len(want))
	}
	got := make(map[string]string, len(rows))
	for _, row := range rows {
		if len(row) < 67 || row[64:66] != "  " {
			return nil, fmt.Errorf("malformed SHA256SUMS row")
		}
		digest, name := row[:64], row[66:]
		if len(name) == 0 || !want[name] || strings.ContainsAny(name, "/\\\r\n\x00") {
			return nil, fmt.Errorf("unlisted or unsafe SHA256SUMS asset %q", name)
		}
		if _, exists := got[name]; exists {
			return nil, fmt.Errorf("duplicate SHA256SUMS asset %q", name)
		}
		decoded, err := hex.DecodeString(digest)
		if err != nil || len(decoded) != sha256.Size || hex.EncodeToString(decoded) != digest {
			return nil, fmt.Errorf("malformed SHA256SUMS digest for %q", name)
		}
		got[name] = digest
	}
	return got, nil
}

func checkSignedSums(t Tool, asset Asset, raw, bundle []byte, binarySHA string) error {
	if t.SignedSums == nil || t.Sigstore == nil || !t.RequireCommittedPin {
		return fmt.Errorf("signed sums require Sigstore and a committed pin")
	}
	pin, ok := pinnedSHA256(t.Name, t.Version, Platform())
	if !ok {
		return fmt.Errorf("no committed sha256 for %s", Platform())
	}
	rows, err := parseSignedSums(t, raw)
	if err != nil {
		return err
	}
	u, err := url.Parse(asset.URL)
	if err != nil {
		return err
	}
	if rows[path.Base(u.Path)] != binarySHA || binarySHA != pin {
		return fmt.Errorf("SHA256SUMS asset row, downloaded sha256, and committed pin disagree")
	}
	h := sha256.Sum256(raw)
	if err := checkBundle(bundle, raw, hex.EncodeToString(h[:]), hex.EncodeToString(h[:]), *t.Sigstore); err != nil {
		return fmt.Errorf("SHA256SUMS bundle: %w", err)
	}
	return nil
}

func verifySignedSums(ctx context.Context, t Tool, asset Asset, binarySHA string) ([]byte, []byte, error) {
	fail := func(err error) ([]byte, []byte, error) {
		return nil, nil, fmt.Errorf("%w: %s %s signed SHA256SUMS: %v", ErrSigstore, t.Name, t.Version, err)
	}
	if t.SignedSums.URL == "" || t.SignedSums.BundleURL == "" {
		return fail(fmt.Errorf("missing URL or bundle URL"))
	}
	var sums, bundle bytes.Buffer
	if _, _, _, err := download(ctx, t.SignedSums.URL, &limitWriter{w: &sums, n: maxSignedSumsSize}); err != nil {
		return fail(err)
	}
	if _, _, _, err := download(ctx, t.SignedSums.BundleURL, &limitWriter{w: &bundle, n: maxBundleSize}); err != nil {
		return fail(err)
	}
	if err := checkSignedSums(t, asset, sums.Bytes(), bundle.Bytes(), binarySHA); err != nil {
		return fail(err)
	}
	dir, err := os.MkdirTemp("", "binmgr-sums-*")
	if err != nil {
		return fail(err)
	}
	defer os.RemoveAll(dir)
	artifact, bundlePath := filepath.Join(dir, "SHA256SUMS"), filepath.Join(dir, "SHA256SUMS.sigstore.json")
	if err := os.WriteFile(artifact, sums.Bytes(), 0o600); err != nil {
		return fail(err)
	}
	if err := os.WriteFile(bundlePath, bundle.Bytes(), 0o600); err != nil {
		return fail(err)
	}
	if err := verifyBundleCosign(ctx, artifact, bundlePath, *t.Sigstore); err != nil {
		return fail(err)
	}
	return sums.Bytes(), bundle.Bytes(), nil
}

func signedSumsCacheValid(ctx context.Context, t Tool, asset Asset, dest, binarySHA string) bool {
	sums, err := os.ReadFile(sumsCachePath(dest))
	if err != nil || len(sums) > maxSignedSumsSize {
		return false
	}
	bundle, err := os.ReadFile(sumsBundleCachePath(dest))
	if err != nil || len(bundle) > maxBundleSize {
		return false
	}
	if err := checkSignedSums(t, asset, sums, bundle, binarySHA); err != nil {
		return false
	}
	return verifyBundleCosign(ctx, sumsCachePath(dest), sumsBundleCachePath(dest), *t.Sigstore) == nil
}

func writeSignedSumsEvidence(dest string, sums, bundle []byte) error {
	for _, item := range []struct {
		path string
		data []byte
	}{{sumsCachePath(dest), sums}, {sumsBundleCachePath(dest), bundle}} {
		tmp, err := os.CreateTemp(filepath.Dir(dest), ".sums-*")
		if err != nil {
			return err
		}
		name := tmp.Name()
		if _, err = tmp.Write(item.data); err == nil {
			err = tmp.Close()
		} else {
			_ = tmp.Close()
		}
		if err != nil {
			_ = os.Remove(name)
			return err
		}
		if err = os.Rename(name, item.path); err != nil {
			_ = os.Remove(name)
			return err
		}
	}
	return nil
}

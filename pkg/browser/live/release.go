package live

// Release packing for the embedded Chrome extension (Sprint 512.3).
//
// The bytes packed are exactly the embedded tree `browser setup live`
// extracts, so the Web Store upload, the side-loaded folder and the hub's
// version floor all describe one artifact. Packing is reproducible: entries
// sorted, fixed timestamps and modes, so two runs of the same tree produce
// byte-identical archives and the published sha256 is checkable by anyone.

import (
	"archive/zip"
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"
)

// zipEpoch is the fixed modification time of every packed entry. 1980-01-01
// is the earliest time the zip format can carry, so no tool rewrites it.
var zipEpoch = time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)

// ExtensionVersion returns the version declared by the embedded
// manifest.json — the single source of the extension's version.
func ExtensionVersion() (string, error) {
	b, err := extensionFS.ReadFile("extension/manifest.json")
	if err != nil {
		return "", err
	}
	var m struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return "", fmt.Errorf("manifest.json: %w", err)
	}
	if m.Version == "" {
		return "", errors.New("manifest.json: no version")
	}
	return m.Version, nil
}

// CheckReleaseVersion refuses a release whose manifest version is below the
// hub floor: such an extension would fail every live call on install.
func CheckReleaseVersion() (string, error) {
	v, err := ExtensionVersion()
	if err != nil {
		return "", err
	}
	if versionLess(v, LiveExtensionMinVersion) {
		return v, fmt.Errorf("extension v%s is below the hub floor v%s", v, LiveExtensionMinVersion)
	}
	return v, nil
}

// PackExtensionZip returns a reproducible zip of the embedded extension,
// rooted at the extension directory (manifest.json at the top, as the
// Chrome Web Store requires). README.md is developer documentation and is
// not shipped.
func PackExtensionZip() ([]byte, error) {
	var names []string
	err := fs.WalkDir(extensionFS, "extension", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && p != "extension/README.md" {
			names = append(names, p)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, p := range names {
		data, err := extensionFS.ReadFile(p)
		if err != nil {
			return nil, err
		}
		h := &zip.FileHeader{
			Name:     strings.TrimPrefix(p, "extension/"),
			Method:   zip.Deflate,
			Modified: zipEpoch,
		}
		h.SetMode(0o644)
		w, err := zw.CreateHeader(h)
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(data); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// PackCRX3 wraps a packed zip in a CRX3 container signed with the RSA key
// in keyPEM (PKCS#1 or PKCS#8). The key is supplied by the caller from the
// secrets vault and never stored beside the source. It returns the CRX
// bytes and the extension ID the key determines.
func PackCRX3(zipBytes, keyPEM []byte) ([]byte, string, error) {
	key, err := parseRSAKey(keyPEM)
	if err != nil {
		return nil, "", err
	}
	pub, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(pub)
	crxID := sum[:16]
	signedData := protoBytes(1, crxID) // SignedData{crx_id}

	h := sha256.New()
	h.Write([]byte("CRX3 SignedData\x00"))
	var n [4]byte
	binary.LittleEndian.PutUint32(n[:], uint32(len(signedData)))
	h.Write(n[:])
	h.Write(signedData)
	h.Write(zipBytes)
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, h.Sum(nil))
	if err != nil {
		return nil, "", err
	}
	proof := append(protoBytes(1, pub), protoBytes(2, sig)...) // AsymmetricKeyProof
	header := append(protoBytes(2, proof), protoBytes(10000, signedData)...)

	var out bytes.Buffer
	out.WriteString("Cr24")
	binary.Write(&out, binary.LittleEndian, uint32(3))
	binary.Write(&out, binary.LittleEndian, uint32(len(header)))
	out.Write(header)
	out.Write(zipBytes)
	return out.Bytes(), extensionID(crxID), nil
}

// extensionID renders a CRX id the way Chrome shows it: each hex nibble
// mapped onto a–p.
func extensionID(id []byte) string {
	var b strings.Builder
	for _, c := range id {
		b.WriteByte('a' + c>>4)
		b.WriteByte('a' + c&0x0f)
	}
	return b.String()
}

func parseRSAKey(keyPEM []byte) (*rsa.PrivateKey, error) {
	blk, _ := pem.Decode(keyPEM)
	if blk == nil {
		return nil, errors.New("crx: signing key is not PEM")
	}
	if k, err := x509.ParsePKCS1PrivateKey(blk.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, fmt.Errorf("crx: signing key: %w", err)
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("crx: signing key is not RSA")
	}
	return rk, nil
}

// protoBytes encodes one length-delimited protobuf field.
func protoBytes(field int, v []byte) []byte {
	out := binary.AppendUvarint(nil, uint64(field)<<3|2)
	out = binary.AppendUvarint(out, uint64(len(v)))
	return append(out, v...)
}

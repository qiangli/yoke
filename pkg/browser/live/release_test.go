package live

import (
	"archive/zip"
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/binary"
	"encoding/pem"
	"testing"
)

func TestPackExtensionZipIsReproducible(t *testing.T) {
	a, err := PackExtensionZip()
	if err != nil {
		t.Fatal(err)
	}
	b, err := PackExtensionZip()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("two packs of the same tree differ")
	}
	zr, err := zip.NewReader(bytes.NewReader(a), int64(len(a)))
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, f := range zr.File {
		names[f.Name] = true
		if !f.Modified.Equal(zipEpoch) {
			t.Errorf("%s: mtime %v, want fixed epoch", f.Name, f.Modified)
		}
	}
	for _, want := range []string{"manifest.json", "background.js", "popup.html", "popup.js"} {
		if !names[want] {
			t.Errorf("zip lacks %s at its root (Web Store needs manifest.json at top)", want)
		}
	}
	if names["README.md"] {
		t.Error("developer README shipped in the release zip")
	}
}

func TestCheckReleaseVersion(t *testing.T) {
	v, err := CheckReleaseVersion()
	if err != nil {
		t.Fatal(err)
	}
	if v == "" {
		t.Fatal("empty version")
	}
}

func TestPackCRX3Header(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	z, _ := PackExtensionZip()
	crx, id, err := PackCRX3(z, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	if string(crx[:4]) != "Cr24" || binary.LittleEndian.Uint32(crx[4:8]) != 3 {
		t.Fatal("bad CRX3 magic/version")
	}
	hl := binary.LittleEndian.Uint32(crx[8:12])
	if !bytes.Equal(crx[12+hl:], z) {
		t.Fatal("CRX payload is not the release zip")
	}
	if len(id) != 32 {
		t.Fatalf("extension id %q, want 32 chars a-p", id)
	}
	if _, _, err := PackCRX3(z, []byte("not a key")); err == nil {
		t.Fatal("non-PEM key accepted")
	}
}

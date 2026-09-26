package python

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"testing"
)

// An .apk is several gzip members; the files are in the last one.
func TestApkFileReadsTheLastMember(t *testing.T) {
	member := func(files map[string]string) []byte {
		var tb bytes.Buffer
		tw := tar.NewWriter(&tb)
		for name, body := range files {
			tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg})
			tw.Write([]byte(body))
		}
		tw.Close()
		var gb bytes.Buffer
		zw := gzip.NewWriter(&gb)
		zw.Write(tb.Bytes())
		zw.Close()
		return gb.Bytes()
	}
	apk := append(append(member(map[string]string{".SIGN.RSA.x": "sig"}), member(map[string]string{".PKGINFO": "info"})...),
		member(map[string]string{"lib/ld-musl-aarch64.so.1": "LOADER"})...)
	got, err := apkFile(apk, "lib/ld-musl-aarch64.so.1")
	if err != nil || string(got) != "LOADER" {
		t.Fatalf("apkFile = %q, %v", got, err)
	}
	if _, err := apkFile(apk, "lib/missing"); err == nil {
		t.Fatal("a missing file must be an error")
	}
}

func TestFetchPinnedRejectsWrongChecksumShape(t *testing.T) {
	for arch, pin := range muslPackage {
		if len(pin.sha256) != 64 || pin.url == "" {
			t.Fatalf("%s pin malformed: %+v", arch, pin)
		}
	}
}

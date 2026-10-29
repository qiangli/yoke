package binmgr

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func TestChromeRuntimePackagesArePinnedAndLicensed(t *testing.T) {
	for _, arch := range []string{"amd64", "arm64"} {
		packages := chromeRuntimePackages[arch]
		if len(packages) != 3 {
			t.Fatalf("%s package count = %d", arch, len(packages))
		}
		for _, pkg := range packages {
			if pkg.URL == "" || len(pkg.SHA256) != 64 || pkg.License == "" || len(pkg.Provides) == 0 {
				t.Fatalf("unreviewable %s package: %#v", arch, pkg)
			}
		}
	}
}

func TestDebDataMember(t *testing.T) {
	payload := []byte("compressed tar")
	deb := []byte("!<arch>\n")
	deb = append(deb, arTestMember("debian-binary", []byte("2.0\n"))...)
	deb = append(deb, arTestMember("data.tar.zst", payload)...)
	got, name, err := debDataMember(deb)
	if err != nil {
		t.Fatal(err)
	}
	if name != "data.tar.zst" || !bytes.Equal(got, payload) {
		t.Fatalf("debDataMember = %q %q", name, got)
	}
}

func TestExtractDebLibraries(t *testing.T) {
	var tarBytes bytes.Buffer
	tw := tar.NewWriter(&tarBytes)
	body := []byte("shared-library")
	if err := tw.WriteHeader(&tar.Header{Name: "./usr/lib/x86_64-linux-gnu/libnss3.so", Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.WriteHeader(&tar.Header{Name: "./usr/lib/x86_64-linux-gnu/libalias.so", Mode: 0o777, Typeflag: tar.TypeSymlink, Linkname: "libnss3.so"}); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	var compressed bytes.Buffer
	zw, err := zstd.NewWriter(&compressed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := zw.Write(tarBytes.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	deb := append([]byte("!<arch>\n"), arTestMember("data.tar.zst", compressed.Bytes())...)
	packagePath := filepath.Join(t.TempDir(), "runtime.deb")
	if err := os.WriteFile(packagePath, deb, 0o644); err != nil {
		t.Fatal(err)
	}
	runtimeDir := t.TempDir()
	if err := extractDebLibraries(packagePath, runtimeDir); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(runtimeDir, "libalias.so"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("extracted library = %q", got)
	}
}

func arTestMember(name string, body []byte) []byte {
	header := []byte(name + "/")
	header = append(header, bytes.Repeat([]byte(" "), 16-len(header))...)
	header = append(header, []byte("0           0     0     100644  ")...)
	size := []byte(string(bytes.Repeat([]byte(" "), 10)))
	n := len(body)
	for i := 9; n > 0; i-- {
		size[i] = byte('0' + n%10)
		n /= 10
	}
	header = append(header, size...)
	header = append(header, '`', '\n')
	out := append(header, body...)
	if len(body)%2 != 0 {
		out = append(out, '\n')
	}
	return out
}

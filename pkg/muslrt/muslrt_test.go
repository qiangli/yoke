package muslrt

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

type entry struct{ name, body, link string }

// testApk builds an .apk: several gzip members, the files in the last one.
func testApk(entries ...entry) []byte {
	member := func(es []entry) []byte {
		var tb bytes.Buffer
		tw := tar.NewWriter(&tb)
		for _, e := range es {
			if e.link != "" {
				tw.WriteHeader(&tar.Header{Name: e.name, Linkname: e.link, Mode: 0o777, Typeflag: tar.TypeSymlink})
				continue
			}
			tw.WriteHeader(&tar.Header{Name: e.name, Mode: 0o755, Size: int64(len(e.body)), Typeflag: tar.TypeReg})
			tw.Write([]byte(e.body))
		}
		tw.Close()
		var gb bytes.Buffer
		zw := gzip.NewWriter(&gb)
		zw.Write(tb.Bytes())
		zw.Close()
		return gb.Bytes()
	}
	return append(append(member([]entry{{name: ".SIGN.RSA.x", body: "sig"}}), member([]entry{{name: ".PKGINFO", body: "info"}})...), member(entries)...)
}

func TestFileReadsTheLastMemberAndFollowsSymlinks(t *testing.T) {
	apk := testApk(entry{name: "usr/lib/libstdc++.so.6", link: "libstdc++.so.6.0.34"}, entry{name: "usr/lib/libstdc++.so.6.0.34", body: "CXX"},
		entry{name: "lib/ld-musl-aarch64.so.1", body: "LOADER"})
	if got, mode, err := File(apk, "usr/lib/libstdc++.so.6"); err != nil || string(got) != "CXX" || mode != 0o755 {
		t.Fatalf("File(symlink) = %q %v %v", got, mode, err)
	}
	if got, _, err := File(apk, "lib/ld-musl-aarch64.so.1"); err != nil || string(got) != "LOADER" {
		t.Fatalf("File = %q, %v", got, err)
	}
	if _, _, err := File(apk, "lib/missing"); err == nil {
		t.Fatal("a missing file must be an error")
	}
}

func TestEnsureLibsVerifiesInstallsAndIsOffline(t *testing.T) {
	apk := testApk(entry{name: "usr/lib/libzz-test.so.9", body: "ZZ"})
	sum := sha256.Sum256(apk)
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hits++; w.Write(apk) }))
	defer srv.Close()
	dir := t.TempDir()
	libs := []Lib{{Soname: "libzz-test.so.9", Member: "usr/lib/libzz-test.so.9", Pkg: Package{Name: "zz", URL: srv.URL, SHA256: hex.EncodeToString(sum[:])}}}
	used, err := EnsureLibs(context.Background(), dir, libs)
	if err != nil || !used || hits != 1 {
		t.Fatalf("EnsureLibs = %v, %v (hits %d)", used, err, hits)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "libzz-test.so.9")); string(got) != "ZZ" {
		t.Fatalf("installed %q", got)
	}
	srv.Close()
	if used, err := EnsureLibs(context.Background(), dir, libs); err != nil || !used {
		t.Fatalf("second EnsureLibs must be a cache hit: %v, %v", used, err)
	}
	bad := []Lib{{Soname: "libyy.so", Member: "usr/lib/libzz-test.so.9", Pkg: Package{URL: "http://127.0.0.1:1/x", SHA256: "00"}}}
	if _, err := EnsureLibs(context.Background(), t.TempDir(), bad); err == nil {
		t.Fatal("an unreachable or unverified package must fail")
	}
}

func TestPinsAreWellFormed(t *testing.T) {
	for _, m := range []map[string]Package{musl, LibStdCxx, LibGcc, LibSSL, LibCrypto, LibPslNative} {
		for _, goarch := range []string{"amd64", "arm64"} {
			p, ok := m[goarch]
			if !ok || len(p.SHA256) != 64 || p.URL == "" || p.License == "" {
				t.Fatalf("%s pin malformed: %+v", goarch, p)
			}
		}
	}
}

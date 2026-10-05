// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

// Package muslrt provisions the musl runtime pieces a Linux host without glibc
// needs before a separately downloaded toolchain can run there. The
// FROM-scratch bashy image is such a host: it has no libc at all. The pieces
// are musl's loader and, for toolchains that need them, shared libraries such
// as the GNU C++ runtime and OpenSSL. All of them come from pinned,
// sha256-verified Alpine packages. They are downloaded and executed as separate
// files, never linked into bashy (licence ruling:
// bashy/docs/fence-toolchain-licenses.md).
//
// The loader is the only file written outside bashy's cache: musl-linked
// executables name /lib/ld-musl-<arch>.so.1 as their ELF interpreter.
// Libraries go to a caller-chosen private directory, and only the child
// process that needs them sees that directory through LD_LIBRARY_PATH.
package muslrt

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"time"
)

// Package is one pinned Alpine package.
type Package struct {
	Name    string // e.g. "libstdc++-15.2.0-r5"
	URL     string
	SHA256  string
	License string // as recorded in the licence inventory
}

// alpine is the release every pin below comes from.
const alpine = "https://dl-cdn.alpinelinux.org/alpine/v3.24/"

func apk(repo, arch, name, sum, license string) Package {
	return Package{Name: name, URL: alpine + repo + "/" + arch + "/" + name + ".apk", SHA256: sum, License: license}
}

// musl is Alpine v3.24's musl-1.2.6-r2 per GOARCH: the loader, which is also
// musl's libc.
var musl = map[string]Package{
	"arm64": apk("main", "aarch64", "musl-1.2.6-r2", "5e9674b7f41152fe2119093b5cb4c13eaaadb19c2d5422b2d7267913e663ee6e", "MIT"),
	"amd64": apk("main", "x86_64", "musl-1.2.6-r2", "573712e2f49c15bfc20a2699f204acdfc74c772722b15e7353d768057fae0e71", "MIT"),
}

// Library pins per GOARCH. The GCC runtime is GPL-3.0-or-later WITH
// GCC-exception-3.1 (Alpine's index labels the whole gcc aport
// "GPL-2.0-or-later AND LGPL-2.1-or-later"); see the licence ruling.
var (
	LibStdCxx = map[string]Package{
		"arm64": apk("main", "aarch64", "libstdc++-15.2.0-r5", "2302e766d4e4926038ec166ecb85837ee884576115236ddb565e3a5fca4a11d7", "GPL-3.0-or-later WITH GCC-exception-3.1"),
		"amd64": apk("main", "x86_64", "libstdc++-15.2.0-r5", "14c987b556f5385a5db18376e788c75f37d85321b8dc1920d926ea7daac1d6f6", "GPL-3.0-or-later WITH GCC-exception-3.1"),
	}
	LibGcc = map[string]Package{
		"arm64": apk("main", "aarch64", "libgcc-15.2.0-r5", "369aaa6e9d099a737bad6dd3e6c2fe7bb1547ca26d22b94ee0411228f709b403", "GPL-3.0-or-later WITH GCC-exception-3.1"),
		"amd64": apk("main", "x86_64", "libgcc-15.2.0-r5", "393dcd32629f06d7d85409c272d142d0c082772d10b87ef55ee82f47de3be637", "GPL-3.0-or-later WITH GCC-exception-3.1"),
	}
	LibSSL = map[string]Package{
		"arm64": apk("main", "aarch64", "libssl3-3.5.9-r0", "20ac252b276d73f2c69c1d25f84537c7fba81caefc026c6be1094b394af2082e", "Apache-2.0"),
		"amd64": apk("main", "x86_64", "libssl3-3.5.9-r0", "05e3393fb95aa5751ca2f9d242f659f6cff82c1cc7767cc2df4a086f7ad01877", "Apache-2.0"),
	}
	LibCrypto = map[string]Package{
		"arm64": apk("main", "aarch64", "libcrypto3-3.5.9-r0", "2676a2b0b6e23ea2edccf3ee982b9842a665d52603d047de3d0a185dc316d983", "Apache-2.0"),
		"amd64": apk("main", "x86_64", "libcrypto3-3.5.9-r0", "6632d758d8f5e9ea3b650fe966f23bbf9a202f8b8dceecac93da135dec5e3689", "Apache-2.0"),
	}
	// LibPslNative is PowerShell's native shim, built from PowerShell-Native
	// source against musl by Alpine. Microsoft publishes no musl arm64 build.
	LibPslNative = map[string]Package{
		"arm64": apk("community", "aarch64", "libpsl-native-7.4.0-r2", "4afb4c28be3263926fc290354a7501eef790d1309fb10124d1bddb6c2d88a544", "MIT"),
		"amd64": apk("community", "x86_64", "libpsl-native-7.4.0-r2", "d91c111d748483359d4a60f2d90fef9dd0f0c17b41c8fdb3c466864e668386d6", "MIT"),
	}
)

// Arch is the musl/Alpine name of the running architecture.
func Arch() (string, error) {
	switch runtime.GOARCH {
	case "arm64":
		return "aarch64", nil
	case "amd64":
		return "x86_64", nil
	}
	return "", fmt.Errorf("musl: no musl runtime pinned for %s", runtime.GOARCH)
}

// LoaderPath is where musl-linked binaries expect their loader.
func LoaderPath() (string, error) {
	arch, err := Arch()
	if err != nil {
		return "", err
	}
	return "/lib/ld-musl-" + arch + ".so.1", nil
}

// glibcLoaders are the dynamic loaders a glibc system provides.
var glibcLoaders = []string{
	"/lib64/ld-linux-x86-64.so.2",
	"/lib/ld-linux-aarch64.so.1",
	"/lib/x86_64-linux-gnu/ld-linux-x86-64.so.2",
	"/lib/aarch64-linux-gnu/ld-linux-aarch64.so.1",
}

// HasGlibc reports whether this Linux host can run glibc-linked binaries.
func HasGlibc() bool {
	for _, p := range glibcLoaders {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

// Needed reports whether this host needs the musl path: Linux without glibc.
func Needed() bool { return runtime.GOOS == "linux" && !HasGlibc() }

// EnsureLoader installs musl's loader at /lib when this host needs it and it
// is missing. A no-op everywhere else. variant names the preloaded image that
// avoids the install (e.g. "bashy self image --with python") for the error a
// read-only root gets.
func EnsureLoader(ctx context.Context, tool, variant string) error {
	if !Needed() {
		return nil
	}
	loader, err := LoaderPath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(loader); err == nil {
		return nil
	}
	pin, ok := musl[runtime.GOARCH]
	if !ok {
		return fmt.Errorf("%s: no musl package pinned for %s", tool, runtime.GOARCH)
	}
	pkg, err := Fetch(ctx, pin)
	if err != nil {
		return fmt.Errorf("%s: musl loader: %w", tool, err)
	}
	name := filepath.Base(loader)
	body, _, err := File(pkg, "lib/"+name)
	if err != nil {
		return fmt.Errorf("%s: musl loader: %w", tool, err)
	}
	if err := InstallFile(loader, body, 0o755); err != nil {
		return fmt.Errorf("%s: this host has no libc and bashy cannot install musl's loader at %s (%v); "+
			"run with a writable root filesystem, or use the preloaded image (%s)", tool, loader, err, variant)
	}
	// libc.musl-<arch>.so.1 is the name musl-linked libraries ask for; musl's
	// loader is also its libc.
	arch, _ := Arch()
	link := "/lib/libc.musl-" + arch + ".so.1"
	if _, err := os.Lstat(link); errors.Is(err, os.ErrNotExist) {
		_ = os.Symlink(name, link)
	}
	fmt.Fprintf(os.Stderr, "note: installed musl's loader at %s (Alpine %s, MIT) — this host has no libc\n", loader, pin.Name)
	return nil
}

// systemLibDirs is musl's default library search path.
var systemLibDirs = []string{"/lib", "/usr/local/lib", "/usr/lib"}

// SystemHas reports whether musl's default search path already holds soname.
func SystemHas(soname string) bool {
	for _, dir := range systemLibDirs {
		if _, err := os.Stat(filepath.Join(dir, soname)); err == nil {
			return true
		}
	}
	return false
}

// Lib is one shared library to provision: soname is the file name it is
// installed under, member the path inside pkg (a symlink is followed).
type Lib struct {
	Soname string
	Member string
	Pkg    Package
}

// EnsureLibs installs every lib the system path lacks into dir and reports
// whether dir holds any of them (and so must be put on LD_LIBRARY_PATH). It
// is idempotent and offline once dir is populated.
func EnsureLibs(ctx context.Context, dir string, libs []Lib) (bool, error) {
	used := false
	fetched := map[string][]byte{}
	for _, lib := range libs {
		dest := filepath.Join(dir, lib.Soname)
		if _, err := os.Stat(dest); err == nil {
			used = true
			continue
		}
		if SystemHas(lib.Soname) {
			continue
		}
		pkg, ok := fetched[lib.Pkg.URL]
		if !ok {
			var err error
			if pkg, err = Fetch(ctx, lib.Pkg); err != nil {
				return false, err
			}
			fetched[lib.Pkg.URL] = pkg
		}
		body, mode, err := File(pkg, lib.Member)
		if err != nil {
			return false, fmt.Errorf("%s: %w", lib.Pkg.Name, err)
		}
		if err := InstallFile(dest, body, mode|0o444); err != nil {
			return false, err
		}
		used = true
	}
	return used, nil
}

// Fetch downloads a pinned package and returns it only when its sha256
// matches.
func Fetch(ctx context.Context, p Package) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.URL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: 2 * time.Minute}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: HTTP %d", p.URL, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != p.SHA256 {
		return nil, fmt.Errorf("%s: sha256 %s, want %s", p.URL, got, p.SHA256)
	}
	return data, nil
}

// File returns one file from an Alpine package, following a symlink inside
// the package (libstdc++.so.6 -> libstdc++.so.6.0.34). An .apk is a sequence
// of gzip members (signature, control, data); the files live in the last one.
func File(apk []byte, name string) ([]byte, os.FileMode, error) {
	br := bufio.NewReader(bytes.NewReader(apk))
	var last []byte
	for {
		zr, err := gzip.NewReader(br)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, 0, err
		}
		zr.Multistream(false)
		member, err := io.ReadAll(zr)
		if err != nil {
			return nil, 0, err
		}
		last = member
		if _, err := br.Peek(1); errors.Is(err, io.EOF) {
			break
		}
	}
	if last == nil {
		return nil, 0, errors.New("empty package")
	}
	for hops := 0; hops < 8; hops++ {
		hdr, body, err := tarEntry(last, name)
		if err != nil {
			return nil, 0, err
		}
		if hdr.Typeflag != tar.TypeSymlink {
			return body, os.FileMode(hdr.Mode).Perm(), nil
		}
		target := hdr.Linkname
		if !path.IsAbs(target) {
			target = path.Join(path.Dir(name), target)
		}
		name = path.Clean(target)
		if path.IsAbs(name) {
			name = name[1:]
		}
	}
	return nil, 0, fmt.Errorf("%s: too many symlinks", name)
}

func tarEntry(data []byte, name string) (*tar.Header, []byte, error) {
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, nil, fmt.Errorf("%s not in package", name)
		}
		if err != nil {
			return nil, nil, err
		}
		if hdr.Name != name {
			continue
		}
		switch hdr.Typeflag {
		case tar.TypeSymlink:
			return hdr, nil, nil
		case tar.TypeReg:
			body, err := io.ReadAll(io.LimitReader(tr, 16<<20))
			return hdr, body, err
		}
	}
}

// InstallFile writes data to path atomically (temp file + rename).
func InstallFile(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".bashy-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}

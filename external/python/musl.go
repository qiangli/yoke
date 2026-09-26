package python

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
	"path/filepath"
	"runtime"
	"time"
)

// On a Linux host without glibc — the FROM-scratch bashy image — the only
// CPython builds that can run are python-build-standalone's musl builds, and
// those are dynamically linked against musl: their ELF interpreter is
// /lib/ld-musl-<arch>.so.1. The host has no libc at all, so bashy provisions
// musl's loader (MIT) there from Alpine's pinned musl package, the same way it
// provisions every other toolchain: download, sha256-verify, install. When the
// root filesystem does not allow it (read-only, not root), the error names the
// way out: the preloaded python image variant.

// MuslUvVersion is the uv release used on hosts without glibc: UV_LIBC (the
// libc override that makes uv work where it cannot detect one) arrived in uv
// 0.7.22. glibc and macOS hosts keep DefaultVersion.
const MuslUvVersion = "0.12.19"

// muslPackage pins Alpine v3.24's musl-1.2.6-r2 per architecture.
var muslPackage = map[string]struct{ url, sha256 string }{
	"arm64": {"https://dl-cdn.alpinelinux.org/alpine/v3.24/main/aarch64/musl-1.2.6-r2.apk", "5e9674b7f41152fe2119093b5cb4c13eaaadb19c2d5422b2d7267913e663ee6e"},
	"amd64": {"https://dl-cdn.alpinelinux.org/alpine/v3.24/main/x86_64/musl-1.2.6-r2.apk", "573712e2f49c15bfc20a2699f204acdfc74c772722b15e7353d768057fae0e71"},
}

func muslArch() (string, error) {
	switch runtime.GOARCH {
	case "arm64":
		return "aarch64", nil
	case "amd64":
		return "x86_64", nil
	}
	return "", fmt.Errorf("python: no musl loader pinned for %s", runtime.GOARCH)
}

// muslLoaderPath is where musl-linked binaries expect their loader.
func muslLoaderPath() (string, error) {
	arch, err := muslArch()
	if err != nil {
		return "", err
	}
	return "/lib/ld-musl-" + arch + ".so.1", nil
}

// needsMusl reports whether this host needs the musl path: Linux without glibc.
func needsMusl() bool { return runtime.GOOS == "linux" && !hasGlibc() }

// uvEnv is the environment uv runs with: on a musl-path host, UV_LIBC names
// the libc uv cannot detect (no /bin/sh or loader to inspect).
func uvEnv(env []string) []string {
	if needsMusl() {
		// UV_PYTHON_INSTALL_BIN=0: no python3.x shim in ~/.local/bin (and no
		// PATH warning) — bashy resolves the managed interpreter itself.
		env = append(env, "UV_LIBC=musl", "UV_PYTHON_INSTALL_BIN=0")
	}
	return env
}

// EnsureMuslLoader installs musl's loader at /lib when this host needs it and
// it is missing. A no-op everywhere else.
func EnsureMuslLoader(ctx context.Context) error {
	if !needsMusl() {
		return nil
	}
	loader, err := muslLoaderPath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(loader); err == nil {
		return nil
	}
	pin, ok := muslPackage[runtime.GOARCH]
	if !ok {
		return fmt.Errorf("python: no musl package pinned for %s", runtime.GOARCH)
	}
	apk, err := fetchPinned(ctx, pin.url, pin.sha256)
	if err != nil {
		return fmt.Errorf("python: musl loader: %w", err)
	}
	name := filepath.Base(loader)
	body, err := apkFile(apk, "lib/"+name)
	if err != nil {
		return fmt.Errorf("python: musl loader: %w", err)
	}
	if err := installFile(loader, body, 0o755); err != nil {
		return fmt.Errorf("python: this host has no libc and bashy cannot install musl's loader at %s (%v); "+
			"run with a writable root filesystem, or use the preloaded python image (bashy self image --with python)", loader, err)
	}
	// libc.musl-<arch>.so.1 is the name musl-linked libraries ask for; musl's
	// loader is also its libc.
	arch, _ := muslArch()
	link := "/lib/libc.musl-" + arch + ".so.1"
	if _, err := os.Lstat(link); errors.Is(err, os.ErrNotExist) {
		_ = os.Symlink(name, link)
	}
	fmt.Fprintf(os.Stderr, "note: installed musl's loader at %s (Alpine musl-1.2.6-r2, MIT) — this host has no libc\n", loader)
	return nil
}

// fetchPinned downloads url and returns it only when its sha256 matches.
func fetchPinned(ctx context.Context, url, want string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: 2 * time.Minute}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != want {
		return nil, fmt.Errorf("%s: sha256 %s, want %s", url, got, want)
	}
	return data, nil
}

// apkFile returns one regular file from an Alpine package. An .apk is a
// sequence of gzip members (signature, control, data); the files live in the
// last one.
func apkFile(apk []byte, name string) ([]byte, error) {
	br := bufio.NewReader(bytes.NewReader(apk))
	var last []byte
	for {
		zr, err := gzip.NewReader(br)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		zr.Multistream(false)
		member, err := io.ReadAll(zr)
		if err != nil {
			return nil, err
		}
		last = member
		if _, err := br.Peek(1); errors.Is(err, io.EOF) {
			break
		}
	}
	if last == nil {
		return nil, errors.New("empty package")
	}
	tr := tar.NewReader(bytes.NewReader(last))
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%s not in package", name)
		}
		if err != nil {
			return nil, err
		}
		if hdr.Name == name && hdr.Typeflag == tar.TypeReg {
			return io.ReadAll(io.LimitReader(tr, 8<<20))
		}
	}
}

// installFile writes data to path atomically (temp file + rename).
func installFile(path string, data []byte, mode os.FileMode) error {
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

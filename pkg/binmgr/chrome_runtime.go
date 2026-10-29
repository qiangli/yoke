package binmgr

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// chromeRuntimePackage is a separately downloaded Linux runtime package. The
// package bytes and extracted libraries stay beside the managed browser; none
// of them are linked into or distributed with bashy.
type chromeRuntimePackage struct {
	Name     string
	URL      string
	SHA256   string
	License  string
	Provides []string
}

// Ubuntu 24.04 packages are used because Chrome for Testing is glibc-linked
// and Noble is the oldest currently supported Ubuntu release with glibc 2.39.
// NSS/NSPR are MPL-2.0; alsa-lib is LGPL-2.1-or-later. Keeping that metadata
// here makes the copyleft boundary reviewable next to the download pins.
var chromeRuntimePackages = map[string][]chromeRuntimePackage{
	"amd64": {
		{Name: "libnspr4_4.35-1.1build1_amd64.deb", URL: "https://archive.ubuntu.com/ubuntu/pool/main/n/nspr/libnspr4_4.35-1.1build1_amd64.deb", SHA256: "e579e72d091f6c7a13f5a756c31065b15aae5b81840d61b069355aa2283c07b4", License: "MPL-2.0", Provides: []string{"libnspr4.so", "libplc4.so", "libplds4.so"}},
		{Name: "libnss3_3.98-1build1_amd64.deb", URL: "https://archive.ubuntu.com/ubuntu/pool/main/n/nss/libnss3_3.98-1build1_amd64.deb", SHA256: "88247fe0db5cd4c273b7dd026d9ded4ff9ba828b62437d12a2f1c2abc29468d2", License: "MPL-2.0", Provides: []string{"libnss3.so", "libnssutil3.so"}},
		{Name: "libasound2t64_1.2.11-1build2_amd64.deb", URL: "https://archive.ubuntu.com/ubuntu/pool/main/a/alsa-lib/libasound2t64_1.2.11-1build2_amd64.deb", SHA256: "c2f0caa30869876791ba349bef4907d5cfec47b8cfd1ae9889a05dce0feb7c39", License: "LGPL-2.1-or-later", Provides: []string{"libasound.so.2"}},
	},
	"arm64": {
		{Name: "libnspr4_4.35-1.1build1_arm64.deb", URL: "https://ports.ubuntu.com/pool/main/n/nspr/libnspr4_4.35-1.1build1_arm64.deb", SHA256: "0bd5994126c41aa05aa380954a3cb2ae56a0a8ebe368a0e01a411fdcc0cb9c68", License: "MPL-2.0", Provides: []string{"libnspr4.so", "libplc4.so", "libplds4.so"}},
		{Name: "libnss3_3.98-1build1_arm64.deb", URL: "https://ports.ubuntu.com/pool/main/n/nss/libnss3_3.98-1build1_arm64.deb", SHA256: "8cf38786c8808bec1873758f71c8c634387677f16104ba4c5ce2485746c4ac96", License: "MPL-2.0", Provides: []string{"libnss3.so", "libnssutil3.so"}},
		{Name: "libasound2t64_1.2.11-1build2_arm64.deb", URL: "https://ports.ubuntu.com/pool/main/a/alsa-lib/libasound2t64_1.2.11-1build2_arm64.deb", SHA256: "793a0961cad1540bdc6217f14b544eb1b27eda952675f4d7d150f547f0c196d7", License: "LGPL-2.1-or-later", Provides: []string{"libasound.so.2"}},
	},
}

var chromeRuntimeSonames = map[string]bool{
	"libnspr4.so": true, "libnss3.so": true, "libnssutil3.so": true, "libasound.so.2": true,
}

// PrepareChromeRuntime provisions only the known shared libraries the resolved
// Linux browser cannot find. It returns a directory to prepend to the child
// process's LD_LIBRARY_PATH, or "" when the host already satisfies the ELF.
func PrepareChromeRuntime(ctx context.Context, browserPath string) (string, error) {
	if runtime.GOOS != "linux" {
		return "", nil
	}
	missing, err := missingChromeRuntimeLibraries(browserPath)
	if err != nil {
		// Installed browsers are often shell wrappers. Their package manager owns
		// their runtime, so only prepare direct ELF executables such as the
		// managed chrome-headless-shell.
		return "", nil
	}
	if len(missing) == 0 {
		return "", nil
	}
	packages, ok := chromeRuntimePackages[runtime.GOARCH]
	if !ok {
		return "", fmt.Errorf("binmgr: Chrome runtime is not pinned for linux/%s (missing %s)", runtime.GOARCH, strings.Join(missing, ", "))
	}
	cacheRoot, err := CacheDir()
	if err != nil {
		return "", err
	}
	runtimeDir := filepath.Join(cacheRoot, ChromeHeadlessShellTool().Name, ChromeHeadlessShellVersion, "runtime")
	for _, pkg := range packages {
		if !packageNeeded(pkg, missing) {
			continue
		}
		if err := ensureChromeRuntimePackage(ctx, runtimeDir, pkg); err != nil {
			return "", fmt.Errorf("binmgr: Chrome runtime %s (%s): %w", pkg.Name, pkg.License, err)
		}
	}
	for _, soname := range missing {
		if _, err := os.Stat(filepath.Join(runtimeDir, soname)); err != nil {
			return "", fmt.Errorf("binmgr: Chrome runtime package did not provide %s", soname)
		}
	}
	return runtimeDir, nil
}

func packageNeeded(pkg chromeRuntimePackage, missing []string) bool {
	for _, provided := range pkg.Provides {
		for _, soname := range missing {
			if provided == soname {
				return true
			}
		}
	}
	return false
}

func missingChromeRuntimeLibraries(browserPath string) ([]string, error) {
	f, err := elf.Open(browserPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	imports, err := f.ImportedLibraries()
	if err != nil {
		return nil, err
	}
	dirs := chromeLibraryDirs(browserPath)
	var missing []string
	for _, soname := range imports {
		if !chromeRuntimeSonames[soname] {
			continue
		}
		found := false
		for _, dir := range dirs {
			if _, err := os.Stat(filepath.Join(dir, soname)); err == nil {
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, soname)
		}
	}
	return missing, nil
}

func chromeLibraryDirs(browserPath string) []string {
	dirs := []string{filepath.Dir(browserPath)}
	dirs = append(dirs, filepath.SplitList(os.Getenv("LD_LIBRARY_PATH"))...)
	switch runtime.GOARCH {
	case "amd64":
		dirs = append(dirs, "/lib/x86_64-linux-gnu", "/usr/lib/x86_64-linux-gnu")
	case "arm64":
		dirs = append(dirs, "/lib/aarch64-linux-gnu", "/usr/lib/aarch64-linux-gnu")
	}
	return append(dirs, "/lib64", "/usr/lib64", "/lib", "/usr/lib", "/usr/local/lib")
}

func ensureChromeRuntimePackage(ctx context.Context, runtimeDir string, pkg chromeRuntimePackage) error {
	complete := true
	for _, soname := range pkg.Provides {
		if _, err := os.Stat(filepath.Join(runtimeDir, soname)); err != nil {
			complete = false
			break
		}
	}
	if complete {
		return nil
	}
	packagesDir := filepath.Join(runtimeDir, ".packages")
	if err := os.MkdirAll(packagesDir, 0o755); err != nil {
		return err
	}
	packagePath := filepath.Join(packagesDir, pkg.Name)
	if got, err := fileSHA256(packagePath); err != nil || got != pkg.SHA256 {
		tmp, err := os.CreateTemp(packagesDir, ".dl-*")
		if err != nil {
			return err
		}
		tmpName := tmp.Name()
		defer os.Remove(tmpName)
		got, _, _, err = download(ctx, pkg.URL, tmp)
		closeErr := tmp.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		if got != pkg.SHA256 {
			return fmt.Errorf("sha256 mismatch: got %s, want %s", got, pkg.SHA256)
		}
		if err := os.Rename(tmpName, packagePath); err != nil {
			return err
		}
	}
	return extractDebLibraries(packagePath, runtimeDir)
}

func fileSHA256(name string) (string, error) {
	f, err := os.Open(name)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func extractDebLibraries(packagePath, runtimeDir string) error {
	body, err := os.ReadFile(packagePath)
	if err != nil {
		return err
	}
	data, name, err := debDataMember(body)
	if err != nil {
		return err
	}
	var tarBody io.Reader = bytes.NewReader(data)
	var decoder *zstd.Decoder
	if strings.HasSuffix(name, ".zst") {
		decoder, err = zstd.NewReader(tarBody)
		if err != nil {
			return err
		}
		defer decoder.Close()
		tarBody = decoder
	} else {
		return fmt.Errorf("unsupported deb payload %s", name)
	}
	stage, err := os.MkdirTemp(runtimeDir, ".stage-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	tr := tar.NewReader(tarBody)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		base := filepath.Base(filepath.Clean(hdr.Name))
		if !strings.HasPrefix(base, "lib") || !strings.Contains(base, ".so") {
			continue
		}
		target := filepath.Join(stage, base)
		switch hdr.Typeflag {
		case tar.TypeReg:
			if err := writeTreeFile(target, io.LimitReader(tr, 64<<20), os.FileMode(hdr.Mode)); err != nil {
				return err
			}
		case tar.TypeSymlink:
			link := filepath.Base(hdr.Linkname)
			if link == "." || link == string(filepath.Separator) {
				return fmt.Errorf("unsafe library symlink %s -> %s", hdr.Name, hdr.Linkname)
			}
			if err := os.Symlink(link, target); err != nil {
				return err
			}
		}
	}
	entries, err := os.ReadDir(stage)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		src, dest := filepath.Join(stage, entry.Name()), filepath.Join(runtimeDir, entry.Name())
		if entry.Type()&os.ModeSymlink != 0 {
			link, err := os.Readlink(src)
			if err != nil {
				return err
			}
			_ = os.Remove(dest)
			if err := os.Symlink(link, dest); err != nil {
				return err
			}
			continue
		}
		if err := copyFile(src, dest); err != nil {
			return err
		}
	}
	return nil
}

func debDataMember(deb []byte) ([]byte, string, error) {
	if len(deb) < 8 || string(deb[:8]) != "!<arch>\n" {
		return nil, "", errors.New("invalid deb ar header")
	}
	for off := 8; off+60 <= len(deb); {
		hdr := deb[off : off+60]
		if string(hdr[58:60]) != "`\n" {
			return nil, "", errors.New("invalid deb member header")
		}
		name := strings.TrimSuffix(strings.TrimSpace(string(hdr[:16])), "/")
		size, err := strconv.Atoi(strings.TrimSpace(string(hdr[48:58])))
		if err != nil || size < 0 || off+60+size > len(deb) {
			return nil, "", errors.New("invalid deb member size")
		}
		start := off + 60
		if strings.HasPrefix(name, "data.tar.") {
			return deb[start : start+size], name, nil
		}
		off = start + size
		if off%2 != 0 {
			off++
		}
	}
	return nil, "", errors.New("deb has no data.tar payload")
}

// Package binmgr is the shared managed-external-binary mechanism for the dhnt
// ecosystem: resolve a (name, version, platform) tool spec → download from its
// own release → sha256-verify → cache → return the executable path. Both bashy
// (the user-facing "OS of binaries" host) and outpost (the lean mesh supervisor)
// call it IN-PROCESS — coreutils is the shared layer both already import — to run
// wrapped tools (loom/Gitea, Zot, SeaweedFS, Kopia, …) without compiling those
// heavy binaries into either. Each tool ships per-platform binaries + sha256 from
// its own CI; binmgr is the one trust/verify/version path for all of them.
//
// It complements external/podman's Resolve (which locates an already-present
// binary): binmgr is the download half. See docs/external-binary-builtins.md.
package binmgr

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Asset is one platform's download for a tool.
type Asset struct {
	// URL is the download URL (a raw binary, or a .tar.gz/.tgz/.zip archive).
	URL string `json:"url"`
	// SHA256 is the expected hex digest of the downloaded file (preferred).
	SHA256 string `json:"sha256"`
	// SHA512 is the expected hex sha512 digest — the strongest supported check,
	// used when SHA256 is empty. The integrity check some vendors publish (e.g.
	// Apache dist ships only .sha512).
	SHA512 string `json:"sha512,omitempty"`
	// MD5 is the expected hex md5 digest — a weak fallback for tools that publish
	// only .md5 sidecars (e.g. SeaweedFS). Used when SHA256/SHA512 are empty.
	// At least one of SHA256/SHA512/MD5 MUST be set: an asset with none is
	// REFUSED (fail-closed), never installed unverified.
	MD5 string `json:"md5,omitempty"`
	// Binary is the path to the executable WITHIN an archive (e.g.
	// "gitea/gitea"); empty means the download is itself the raw binary.
	Binary string `json:"binary,omitempty"`
	// Tree requests whole-archive ("recursive") extraction instead of pulling a
	// single Binary member: the entire .tar.gz/.zip tree is unpacked into the
	// tool's cache dir, preserving modes and symlinks. Use it for tools that need
	// their full layout to run (a Go toolchain's bin/+pkg/+src/, an SDK, …).
	Tree bool `json:"tree,omitempty"`
	// Entrypoint is the executable's slash-separated path within the extracted
	// tree (e.g. "go/bin/go"); required when Tree is set, ignored otherwise.
	Entrypoint string `json:"entrypoint,omitempty"`
}

// Tool is a managed external binary: a logical name, a version (the cache key),
// and per-platform assets keyed by "goos/goarch" (e.g. "linux/amd64").
type Tool struct {
	Name    string           `json:"name"`
	Version string           `json:"version"`
	Assets  map[string]Asset `json:"assets"`
}

// Platform returns the current "goos/goarch" key.
func Platform() string { return runtime.GOOS + "/" + runtime.GOARCH }

// CacheDir is the root for downloaded binaries. Override via $BASHY_BIN_CACHE;
// otherwise <UserCacheDir>/bashy/bin.
func CacheDir() (string, error) {
	if d := strings.TrimSpace(os.Getenv("BASHY_BIN_CACHE")); d != "" {
		return d, nil
	}
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "bashy", "bin"), nil
}

func binaryName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

// BinaryName is the on-disk basename Ensure caches a tool under (adds .exe on
// Windows). Exported so callers can build/inspect cache paths.
func BinaryName(name string) string { return binaryName(name) }

// CachedBinary returns an already-cached tool binary from a prior Ensure (newest
// version wins), or "" when none is cached. A hot-path caller uses it to skip
// version resolution + network entirely: Ensure lays tools out at
// <CacheDir>/<name>/<version>/<binaryName>. Both raw-binary and Tree entrypoints
// whose entrypoint basename equals the tool name are found.
func CachedBinary(name string) string {
	root, err := CacheDir()
	if err != nil {
		return ""
	}
	// This package writes managed tools two ways, and a lookup that knows only
	// one of them silently reports "not installed" for a binary sitting in the
	// cache:
	//
	//   Ensure           -> <root>/<name>/<version>/<binary>   (version-pinned)
	//   ProvisionManaged -> <root>/<binary>                    (latest-wins)
	//
	// podman, ollama and the other engine tools come from ProvisionManaged, so
	// globbing only the versioned form missed every one of them — callers then
	// fell back to $PATH, which made a service definition's PATH load-bearing
	// and cost a host its container runtime when that PATH was regenerated
	// without the usual package-manager prefixes.
	//
	// Both layouts are searched here so this stays the single answer to "where
	// is managed tool X"; newest mtime wins across both, so a freshly-pinned
	// version supersedes an older flat drop and vice versa. Callers must not
	// reconstruct either path themselves.
	candidates, _ := filepath.Glob(filepath.Join(root, name, "*", binaryName(name)))
	if flat := filepath.Join(root, binaryName(name)); flat != "" {
		candidates = append(candidates, flat)
	}
	best := ""
	var bestMod int64
	for _, m := range candidates {
		fi, err := os.Stat(m)
		if err != nil || fi.IsDir() || (runtime.GOOS != "windows" && fi.Mode()&0o111 == 0) {
			continue
		}
		if mt := fi.ModTime().UnixNano(); best == "" || mt > bestMod {
			best, bestMod = m, mt
		}
	}
	return best
}

// Ensure resolves the tool's asset for the current platform, downloading +
// sha256-verifying + caching it if not already present, and returns the path to
// the executable. Idempotent: a cache hit returns immediately with no network.
func Ensure(ctx context.Context, t Tool) (string, error) {
	if t.Name == "" || t.Version == "" {
		return "", fmt.Errorf("binmgr: tool name and version are required")
	}
	asset, ok := t.Assets[Platform()]
	if !ok || asset.URL == "" {
		return "", fmt.Errorf("binmgr: %s %s has no asset for %s", t.Name, t.Version, Platform())
	}
	root, err := CacheDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(root, t.Name, t.Version)
	dest := filepath.Join(dir, binaryName(t.Name))
	if asset.Tree {
		if asset.Entrypoint == "" {
			return "", fmt.Errorf("binmgr: %s %s: Tree asset needs an Entrypoint", t.Name, t.Version)
		}
		dest = filepath.Join(dir, filepath.FromSlash(asset.Entrypoint))
	}
	if isExecutable(dest) {
		return dest, nil // cache hit — no network
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}

	tmp, err := os.CreateTemp(dir, ".dl-*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	sum, sha512sum, md5sum, derr := download(ctx, asset.URL, tmp)
	_ = tmp.Close()
	if derr != nil {
		return "", derr
	}
	// A committed pin, when present, is authoritative: verify against OUR digest
	// (trust root = this repo's reviewed history) and discard whatever the release
	// resolved to, including a weaker md5. See pins.go for why this matters — the
	// release-resolved checksum protects only against transit corruption, not
	// against the release itself being tampered with.
	if pin, ok := pinnedSHA256(t.Name, t.Version, Platform()); ok {
		asset.SHA256, asset.SHA512, asset.MD5 = pin, "", ""
	}

	// Integrity is the supply-chain trust boundary — bashy is OSS and its
	// download-and-exec path is exactly what a malicious mirror/tampered release
	// would target. Verify against the strongest digest provided (sha256 >
	// sha512 > md5) and — critically — FAIL CLOSED: refuse to install a binary
	// for which no checksum was supplied at all, rather than silently trusting it.
	switch {
	case strings.TrimSpace(asset.SHA256) != "":
		if want := strings.ToLower(strings.TrimSpace(asset.SHA256)); want != sum {
			return "", fmt.Errorf("binmgr: %s %s sha256 mismatch: got %s, want %s", t.Name, t.Version, sum, want)
		}
	case strings.TrimSpace(asset.SHA512) != "":
		if want := strings.ToLower(strings.TrimSpace(asset.SHA512)); want != sha512sum {
			return "", fmt.Errorf("binmgr: %s %s sha512 mismatch: got %s, want %s", t.Name, t.Version, sha512sum, want)
		}
	case strings.TrimSpace(asset.MD5) != "":
		// MD5 is collision-broken: an attacker can craft a malicious artifact
		// with the same md5 as a benign one, so an md5-only check against a
		// release-supplied digest is close to no check at all. Refuse it by
		// default. The fix is to commit a sha256 pin (pins.go); the env override
		// exists only so a legitimately md5-only upstream is not bricked before a
		// pin lands, and it says clearly what it is trading away.
		if !weakChecksumAllowed() {
			return "", fmt.Errorf("binmgr: %s %s: only an MD5 checksum is available for %s, and MD5 is collision-broken — not a trustworthy integrity check. Commit a sha256 pin (pkg/binmgr/pins.go), or set BASHY_ALLOW_WEAK_CHECKSUM=1 to accept the weaker check", t.Name, t.Version, asset.URL)
		}
		if want := strings.ToLower(strings.TrimSpace(asset.MD5)); want != md5sum {
			return "", fmt.Errorf("binmgr: %s %s md5 mismatch: got %s, want %s", t.Name, t.Version, md5sum, want)
		}
		fmt.Fprintf(os.Stderr, "binmgr: warning: %s %s verified with MD5 only (collision-broken); commit a sha256 pin\n", t.Name, t.Version)
	default:
		return "", fmt.Errorf("binmgr: %s %s: refusing to install %s with NO checksum (sha256/sha512/md5) — supply one or the download cannot be trusted", t.Name, t.Version, asset.URL)
	}

	switch {
	case asset.Tree:
		// Whole-archive extraction into dir/ (preserves the archive's own
		// layout + modes); dest is dir/<entrypoint>.
		if err := extractTree(tmpName, asset.URL, dir); err != nil {
			return "", err
		}
		if !isExecutable(dest) {
			return "", fmt.Errorf("binmgr: %s %s: entrypoint missing after extract: %s", t.Name, t.Version, dest)
		}
		return dest, nil
	case asset.Binary != "":
		if err := extract(tmpName, asset.URL, asset.Binary, dest); err != nil {
			return "", err
		}
	default:
		if err := os.Rename(tmpName, dest); err != nil {
			// cross-device fallback
			if cerr := copyFile(tmpName, dest); cerr != nil {
				return "", err
			}
		}
	}
	if err := os.Chmod(dest, 0o755); err != nil {
		return "", err
	}
	return dest, nil
}

func download(ctx context.Context, url string, w io.Writer) (sha, sha512sum, md5sum string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", "", "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", "", fmt.Errorf("binmgr: GET %s: HTTP %d", url, resp.StatusCode)
	}
	sh := sha256.New()
	s5 := sha512.New()
	mh := md5.New()
	if _, err := io.Copy(io.MultiWriter(w, sh, s5, mh), resp.Body); err != nil {
		return "", "", "", err
	}
	return hex.EncodeToString(sh.Sum(nil)), hex.EncodeToString(s5.Sum(nil)), hex.EncodeToString(mh.Sum(nil)), nil
}

func extract(archivePath, url, member, dest string) error {
	switch {
	case strings.HasSuffix(url, ".zip"):
		return extractZip(archivePath, member, dest)
	case strings.HasSuffix(url, ".tar.gz"), strings.HasSuffix(url, ".tgz"):
		return extractTarGz(archivePath, member, dest)
	default:
		return fmt.Errorf("binmgr: archive member requested but %s is not a .zip/.tar.gz", url)
	}
}

func extractTarGz(archivePath, member, dest string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return fmt.Errorf("binmgr: %q not found in archive", member)
		}
		if err != nil {
			return err
		}
		if matchMember(hdr.Name, member) {
			return writeFrom(tr, dest)
		}
	}
}

func extractZip(archivePath, member, dest string) error {
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, zf := range zr.File {
		if matchMember(zf.Name, member) {
			rc, err := zf.Open()
			if err != nil {
				return err
			}
			defer rc.Close()
			return writeFrom(rc, dest)
		}
	}
	return fmt.Errorf("binmgr: %q not found in archive", member)
}

// matchMember matches an archive entry against the requested member by full
// cleaned path OR by basename — so a Member like "kopia" finds both a root-level
// "kopia" and a nested "kopia-0.18-linux-x64/kopia" without knowing the version.
// On Windows it also accepts the `.exe` the Member spec omits (e.g. Member "gh"
// matches "gh_2.62_windows_amd64/bin/gh.exe"), so cross-platform tools resolve.
func matchMember(entryName, member string) bool {
	if filepath.Clean(entryName) == filepath.Clean(member) || filepath.Base(entryName) == member {
		return true
	}
	return runtime.GOOS == "windows" && filepath.Base(entryName) == member+".exe"
}

func writeFrom(r io.Reader, dest string) error {
	tmp := dest + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, r); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dest)
}

// extractTree unpacks an entire .tar.gz/.zip into destDir, preserving the
// archive's directory layout, file modes, and symlinks. This is the "recursive"
// counterpart to the single-member extract, for tools that need their whole
// tree (e.g. a Go toolchain).
func extractTree(archivePath, url, destDir string) error {
	switch {
	case strings.HasSuffix(url, ".zip"):
		return extractTreeZip(archivePath, destDir)
	case strings.HasSuffix(url, ".tar.gz"), strings.HasSuffix(url, ".tgz"):
		return extractTreeTarGz(archivePath, destDir)
	default:
		return fmt.Errorf("binmgr: tree extraction needs a .zip/.tar.gz, got %s", url)
	}
}

func extractTreeTarGz(archivePath, destDir string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		target, err := safeJoinTree(destDir, hdr.Name)
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := writeTreeFile(target, tr, os.FileMode(hdr.Mode)); err != nil {
				return err
			}
		case tar.TypeSymlink:
			_ = os.MkdirAll(filepath.Dir(target), 0o755)
			_ = os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
		}
	}
}

func extractTreeZip(archivePath, destDir string) error {
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, zf := range zr.File {
		target, err := safeJoinTree(destDir, zf.Name)
		if err != nil {
			return err
		}
		if zf.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		rc, err := zf.Open()
		if err != nil {
			return err
		}
		err = writeTreeFile(target, rc, zf.Mode())
		rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func writeTreeFile(target string, r io.Reader, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode|0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, r); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// safeJoinTree guards against path traversal (zip-slip) in archive entries.
func safeJoinTree(destDir, name string) (string, error) {
	target := filepath.Join(destDir, name)
	clean := filepath.Clean(destDir)
	if target != clean && !strings.HasPrefix(target, clean+string(os.PathSeparator)) {
		return "", fmt.Errorf("binmgr: unsafe path in archive: %s", name)
	}
	return target, nil
}

func copyFile(src, dest string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	return writeFrom(in, dest)
}

func isExecutable(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	return info.Mode()&0o111 != 0
}

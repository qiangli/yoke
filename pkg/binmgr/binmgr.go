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

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

// Asset is one platform's download for a tool.
type Asset struct {
	// URL is the download URL (a raw binary, a single zstd-compressed binary
	// ".zst", or a .tar.gz/.tgz/.tar.xz/.tar.zst/.zip archive).
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
	// SigstoreBundle is where this asset's Sigstore bundle is published;
	// empty = URL + DefaultBundleSuffix. Read only when the Tool declares
	// Sigstore.
	SigstoreBundle string `json:"sigstore_bundle,omitempty"`
}

// Tool is a managed external binary: a logical name, a version (the cache key),
// and per-platform assets keyed by "goos/goarch" (e.g. "linux/amd64").
type Tool struct {
	// targetPlatform is set only on the private copy used by EnsureFor.
	targetPlatform string
	cachedPath     string
	cachedName     string
	Name           string           `json:"name"`
	Version        string           `json:"version"`
	Assets         map[string]Asset `json:"assets"`
	// Sigstore, when set, requires every download to carry a keyless
	// Sigstore bundle signed by exactly this identity and issuer, verified
	// before the digest check and before anything is unpacked (sigstore.go).
	Sigstore *Sigstore `json:"sigstore,omitempty"`
	// SignedSums requires a separately signed release checksum manifest. Its
	// asset row must agree with both the downloaded bytes and a committed pin.
	SignedSums *SignedSums `json:"signed_sums,omitempty"`
	// RequireCommittedPin refuses release-supplied checksums, including on cache hits.
	RequireCommittedPin bool `json:"require_committed_pin,omitempty"`
}

// Platform returns the current "goos/goarch" key.
func Platform() string { return runtime.GOOS + "/" + runtime.GOARCH }

func (t Tool) platform() string {
	if t.targetPlatform != "" {
		return t.targetPlatform
	}
	return Platform()
}

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
// whose entrypoint basename equals the tool name are found. Product executables
// extracted together by EnsureMembers are also discoverable.
func CachedBinary(name string) string {
	// Prima is admitted only through its pinned, signed Tool declaration.
	if name == PrimaName {
		return ""
	}
	root, err := CacheDir()
	if err != nil {
		return ""
	}
	// This package writes managed tools in several layouts; a lookup knowing only
	// one of them silently reports "not installed" for a binary sitting in the
	// cache:
	//
	//   Ensure           -> <root>/<name>/<version>/<binary>   (version-pinned)
	//   ProvisionManaged -> <root>/<binary>                    (latest-wins)
	//   EnsureMembers    -> <root>/bashy-archive/<version>/members-<hash>/<binary>
	//   engine tarball   -> <root>/<name>/<version>/<name>-<goos>-<goarch>/usr/local/bin/<binary>
	//
	// podman, ollama and the other engine tools come from ProvisionManaged, so
	// globbing only the versioned form missed every one of them — callers then
	// fell back to $PATH, which made a service definition's PATH load-bearing
	// and cost a host its container runtime when that PATH was regenerated
	// without the usual package-manager prefixes.
	//
	// These layouts are searched here so this stays the single answer to "where
	// is managed tool X"; newest mtime wins across them, so a freshly-pinned
	// version supersedes an older flat drop and vice versa. Callers must not
	// reconstruct either path themselves.
	candidates, _ := filepath.Glob(filepath.Join(root, name, "*", binaryName(name)))
	// The engine tarball layout bashy oci / bashy podman self-provisions (a
	// Tree extraction of the upstream engine tarball, e.g.
	// <root>/podman/v6.1.2/podman-linux-amd64/usr/local/bin/podman). No
	// shared resolver for that layout exists in this module — and
	// external/podman/engine imports this package, so reaching outward for
	// one would risk an import cycle — so the layout is matched by glob
	// instead, newest mtime winning below like the other layouts.
	engine, _ := filepath.Glob(filepath.Join(root, name, "*",
		fmt.Sprintf("%s-%s-%s", name, runtime.GOOS, runtime.GOARCH),
		"usr", "local", "bin", binaryName(name)))
	candidates = append(candidates, engine...)
	switch name {
	case "bashy", "outpost", "bash", "sh":
		paired, _ := filepath.Glob(filepath.Join(root, "bashy-archive", "*", "members-*", binaryName(name)))
		candidates = append(candidates, paired...)
	}

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

// InstallPath is where Ensure lays out t's executable for the current
// platform — <CacheDir>/<name>/<version>/<binary>, or the Tree entrypoint
// under that version dir. It touches neither the network nor the filesystem,
// so a launcher can name the pinned binary (argv[0]) before anything is
// downloaded and a listing can ask "is this pin installed" without fetching
// it. Errors exactly where Ensure would: no name/version, no asset for this
// platform, a Tree asset without an entrypoint.
func InstallPath(t Tool) (string, error) {
	_, dest, err := installLayout(t)
	return dest, err
}

// Cached reports the executable Ensure would return for t when it is already
// installed, with no network. ("", false) when the pin is absent or the asset
// cannot be resolved for this platform.
func Cached(t Tool) (string, bool) {
	dest, err := InstallPath(t)
	if err != nil || !isExecutable(dest) {
		return "", false
	}
	if t.Sigstore != nil && !signedCacheValid(context.Background(), t, t.Assets[Platform()], dest) {
		return "", false
	}
	return dest, true
}

func installLayout(t Tool) (dir, dest string, err error) {
	if t.Name == "" || t.Version == "" {
		return "", "", fmt.Errorf("binmgr: tool name and version are required")
	}
	asset, ok := t.Assets[t.platform()]
	if !ok || asset.URL == "" {
		return "", "", fmt.Errorf("binmgr: %s %s has no asset for %s", t.Name, t.Version, t.platform())
	}
	if t.RequireCommittedPin {
		if _, ok := pinnedSHA256(t.Name, t.Version, t.platform()); !ok {
			return "", "", fmt.Errorf("binmgr: %s %s has no committed sha256 for %s", t.Name, t.Version, t.platform())
		}
		if t.Sigstore == nil {
			return "", "", fmt.Errorf("binmgr: %s %s requires Sigstore verification", t.Name, t.Version)
		}
	}
	if t.SignedSums != nil && (t.Sigstore == nil || !t.RequireCommittedPin) {
		return "", "", fmt.Errorf("binmgr: %s %s signed sums require Sigstore and a committed pin", t.Name, t.Version)
	}
	root, err := CacheDir()
	if err != nil {
		return "", "", err
	}
	if t.platform() != Platform() {
		root = filepath.Join(root, ".targets", filepath.FromSlash(t.platform()))
	}
	dir = filepath.Join(root, t.Name, t.Version)
	name := t.Name
	if strings.HasPrefix(t.platform(), "windows/") {
		name += ".exe"
	}
	dest = filepath.Join(dir, name)
	if asset.Tree {
		if asset.Entrypoint == "" {
			return "", "", fmt.Errorf("binmgr: %s %s: Tree asset needs an Entrypoint", t.Name, t.Version)
		}
		dest = filepath.Join(dir, filepath.FromSlash(asset.Entrypoint))
	}
	return dir, dest, nil
}

// Ensure resolves the tool's asset for the current platform, downloading +
// sha256-verifying + caching it if not already present, and returns the path to
// the executable. Idempotent: a cache hit returns immediately with no network.
func Ensure(ctx context.Context, t Tool) (string, error) {
	t.targetPlatform = ""
	return ensure(ctx, t)
}

// EnsureFor resolves an explicit target platform using the same pins and
// verification as Ensure. Foreign artifacts live under
// <CacheDir>/.targets/<goos>/<goarch>/<name>/<version>, outside host lookups.
// It never executes the target binary; signature verification uses host cosign.
// A target equal to the host preserves Ensure's existing cache layout.
func EnsureFor(ctx context.Context, t Tool, goos, goarch string) (string, error) {
	for _, token := range []string{goos, goarch} {
		if token == "" || strings.IndexFunc(token, func(r rune) bool {
			return (r < 'a' || r > 'z') && (r < '0' || r > '9')
		}) >= 0 {
			return "", fmt.Errorf("binmgr: invalid target platform %q/%q", goos, goarch)
		}
	}
	t.targetPlatform = goos + "/" + goarch
	return ensure(ctx, t)
}

func ensure(ctx context.Context, t Tool) (string, error) {
	if t.platform() == Platform() && !t.RequireCommittedPin && t.Sigstore == nil && t.SignedSums == nil && t.cachedName == t.Name && t.Name != "" && t.cachedPath != "" && isExecutable(t.cachedPath) {
		return t.cachedPath, nil
	}
	dir, dest, err := installLayout(t)
	if err != nil {
		return "", err
	}
	asset := t.Assets[t.platform()]
	if isExecutable(dest) {
		if t.Sigstore != nil && !signedCacheValid(ctx, t, asset, dest) {
			return "", fmt.Errorf("%w: %s %s has an unverified or stale cache entry", ErrSigstore, t.Name, t.Version)
		}
		return dest, nil // cache hit — no network
	}
	if Offline() {
		return "", offlineMissing(t.Name, t.Version)
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
	if pin, ok := pinnedSHA256(t.Name, t.Version, t.platform()); ok {
		asset.SHA256, asset.SHA512, asset.MD5 = pin, "", ""
	}
	var sums, sumsBundle []byte
	if t.SignedSums != nil {
		sums, sumsBundle, err = verifySignedSums(ctx, t, asset, sum)
		if err != nil {
			return "", err
		}
	}

	// A signed tool is verified keylessly over the downloaded bytes FIRST —
	// exact identity, exact issuer, signature, bundle digest == pin — and a
	// failure refuses before any decompress/install (sigstore.go).
	var bundle []byte
	if t.Sigstore != nil {
		bundle, err = verifySigstore(ctx, t, asset, tmpName, sum)
		if err != nil {
			return "", err
		}
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
		if t.Sigstore != nil {
			if err := writeSignedCacheEvidence(asset, dest, tmpName, bundle); err != nil {
				return "", err
			}
		}
		if t.SignedSums != nil {
			if err := writeSignedSumsEvidence(dest, sums, sumsBundle); err != nil {
				return "", err
			}
		}
		return dest, nil
	case asset.Binary != "":
		if err := extract(tmpName, asset.URL, asset.Binary, dest); err != nil {
			return "", err
		}
	case isSingleZst(asset.URL):
		// A single executable compressed with zstd (tuwunel ships these): the
		// digest above covered the compressed download; decompress it into place.
		if err := decompressZst(tmpName, dest, maxDecompressedSize); err != nil {
			return "", fmt.Errorf("binmgr: %s %s: %w", t.Name, t.Version, err)
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
	if t.Sigstore != nil {
		if err := writeSignedCacheEvidence(asset, dest, tmpName, bundle); err != nil {
			return "", err
		}
	}
	if t.SignedSums != nil {
		if err := writeSignedSumsEvidence(dest, sums, sumsBundle); err != nil {
			return "", err
		}
	}
	return dest, nil
}

func download(ctx context.Context, url string, w io.Writer) (sha, sha512sum, md5sum string, err error) {
	if Offline() {
		return "", "", "", fmt.Errorf("binmgr: BASHY_OFFLINE=1 refuses download %s", url)
	}
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
	case isTarArchive(url):
		return extractTar(archivePath, url, member, dest)
	default:
		return fmt.Errorf("binmgr: archive member requested but %s is not a .zip/.tar.gz/.tar.xz/.tar.zst", url)
	}
}

func extractTar(archivePath, url, member, dest string) error {
	tr, closer, err := tarReader(archivePath, url)
	if err != nil {
		return err
	}
	defer closer.Close()
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

// maxDecompressedSize bounds a single-file .zst install so a hostile asset (a
// small zstd bomb) cannot fill the disk. A var so tests can lower it.
var maxDecompressedSize int64 = 512 << 20

// isSingleZst reports a single zstd-compressed file (".zst" but not a tarball).
func isSingleZst(url string) bool {
	return strings.HasSuffix(url, ".zst") && !strings.HasSuffix(url, ".tar.zst")
}

// decompressZst stream-decompresses src into dest (0755), refusing output
// larger than limit. It writes a temp file beside dest and renames it into
// place, so a corrupt, truncated or over-cap stream leaves nothing behind.
func decompressZst(src, dest string, limit int64) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	zr, err := zstd.NewReader(f)
	if err != nil {
		return fmt.Errorf("zstd: %w", err)
	}
	defer zr.Close()

	out, err := os.CreateTemp(filepath.Dir(dest), ".zst-*")
	if err != nil {
		return err
	}
	tmp := out.Name()
	ok := false
	defer func() {
		if !ok {
			_ = out.Close()
			_ = os.Remove(tmp)
		}
	}()
	n, err := io.Copy(out, io.LimitReader(zr, limit+1))
	if err != nil {
		return fmt.Errorf("zstd decompress: %w", err)
	}
	if n > limit {
		return fmt.Errorf("zstd decompress: output exceeds %d bytes, refusing", limit)
	}
	if err := out.Chmod(0o755); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, dest); err != nil {
		return err
	}
	ok = true
	return nil
}

// extractTree unpacks an entire .tar.gz/.zip into destDir, preserving the
// archive's directory layout, file modes, and symlinks. This is the "recursive"
// counterpart to the single-member extract, for tools that need their whole
// tree (e.g. a Go toolchain).
func extractTree(archivePath, url, destDir string) error {
	switch {
	case strings.HasSuffix(url, ".zip"):
		return extractTreeZip(archivePath, destDir)
	case isTarArchive(url):
		return extractTreeTar(archivePath, url, destDir)
	default:
		return fmt.Errorf("binmgr: tree extraction needs a .zip/.tar.gz/.tar.xz/.tar.zst, got %s", url)
	}
}

// isTarArchive reports a compressed tar this package can unpack.
func isTarArchive(url string) bool {
	for _, s := range []string{".tar.gz", ".tgz", ".tar.xz", ".tar.zst"} {
		if strings.HasSuffix(url, s) {
			return true
		}
	}
	return false
}

// tarReader opens a .tar.gz/.tgz, .tar.xz (Zig ships only .tar.xz on unix) or
// .tar.zst (the official linux ollama) archive. The returned closer releases
// the file (and the decoder).
func tarReader(archivePath, url string) (*tar.Reader, io.Closer, error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return nil, nil, err
	}
	if strings.HasSuffix(url, ".tar.zst") {
		zr, err := zstd.NewReader(f)
		if err != nil {
			f.Close()
			return nil, nil, err
		}
		return tar.NewReader(zr), closerFunc(func() error { zr.Close(); return f.Close() }), nil
	}
	if strings.HasSuffix(url, ".tar.xz") {
		xr, err := xz.NewReader(f)
		if err != nil {
			f.Close()
			return nil, nil, err
		}
		return tar.NewReader(xr), f, nil
	}
	gz, err := gzip.NewReader(f)
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	return tar.NewReader(gz), f, nil
}

func extractTreeTar(archivePath, url, destDir string) error {
	tr, closer, err := tarReader(archivePath, url)
	if err != nil {
		return err
	}
	defer closer.Close()
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

// IsExecutable reports whether path exists and is an executable file.
func IsExecutable(path string) bool { return isExecutable(path) }

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

type closerFunc func() error

func (c closerFunc) Close() error { return c() }

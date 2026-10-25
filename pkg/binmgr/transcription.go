package binmgr

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// S503.6 Review licenses and package verified transcription dependencies for six targets.
//
// Supply-chain audit summary:
//
// 1. yt-dlp (The Unlicense + ISC meriyah + MIT astring - Fully Permissive):
//    - Pinned version: 2026.10.21
//    - Standalone PyInstaller executables (yt-dlp.exe, yt-dlp_linux, yt-dlp_macos) bundle
//      mutagen (GPL-2.0-or-later). Under the repository's supply-chain policy, GPL-bundled
//      executables are strictly EXCLUDED.
//    - The admitted dependency path is the official pure-Python zipapp (yt-dlp),
//      whose full license closure consists of The Unlicense (core yt-dlp / public domain dedication),
//      ISC license (bundled meriyah JS parser), and MIT license (bundled astring JS generator).
//      All components are strictly permissive non-copyleft, satisfying the supply chain policy.
//    - Runs across all six release targets under Python 3.10+ (provisioned by bashy via uv / CPython 3.15,
//      PSF-2.0, yoke/external/python).
//    - JavaScript challenges (n-sig / player signatures) are resolved using
//      an admitted JavaScript runtime. Upstream yt-dlp defaults to and recommends
//      Deno (v2.9.8, MIT, DenoTool()), while also supporting Node.js (v22.23.2 LTS, MIT,
//      yoke/external/node).
//    - External JavaScript solver scripts (yt-dlp-ejs, 0.8.0, The Unlicense + ISC + MIT)
//      are bundled directly into the pure Python zipapp, satisfying `--no-remote-components`.
//      Standalone wheel and minified solver artifacts are pinned and cached for modular or
//      decoupled runtime environments.
//
// 2. whisper.cpp (MIT License - Permissive):
//    - Pinned stable release: v1.9.5 (official upstream stable release).
//    - Upstream's releases page explicitly labels b5454 a nightly build ("Nightly build: b5454").
//      Nightly builds are not admitted for stable tool pinning under supply chain policy.
//    - Upstream publishes NO standalone CLI prebuilt release archives for stable releases (v1.9.5
//      attaches 0 binary assets; macOS has never had prebuilt CLI assets upstream).
//    - Consequently, missing stable prebuilt CLI assets across all six targets (darwin/amd64,
//      darwin/arm64, linux/amd64, linux/arm64, windows/amd64, windows/arm64) are recorded as
//      explicit upstream blockers.
//    - Admitted resolution paths:
//      a) Pinned source build: upstream stable v1.9.5 source archive pinned with SHA-256 in pins.go
//         via EnsureWhisperCppSource.
//      b) First-party signed CI release: qiangli/bashy tagged at the WhisperCppVersion
//         release tag, with keyless Sigstore bundles from GitHub Actions.
//
// 3. Media Decoder / FFmpeg (LGPL-2.1-or-later / GPL-2.0-or-later):
//    - Strict permissive packaging CANNOT be satisfied: FFmpeg has no permissive license option.
//    - Packaging FFmpeg remains an unresolved blocker pending an explicit operator policy decision.
//    - Proposed interim path per target, pending operator decision: unbundled host discovery
//      on $PATH (Option A), plus native decoders where supported (afconvert on darwin; dr_wav for WAV).
//    - No copyleft FFmpeg executable is bundled, downloaded, or distributed via binmgr.
//
// 4. Models (OpenAI Whisper - MIT License):
//    - Pinned official GGML weights from Hugging Face (ggerganov/whisper.cpp) at immutable
//      revision 5359861c739e955e79d9a303bcbc70fb988958b1.
//    - Acquired intentionally by name, verified against committed SHA-256 digests,
//      and cached offline. Tampered downloads are rejected fail-closed.
//    - Cache concurrency is safe: synchronized in-flight downloads, unique temporary files,
//      and atomic replacement prevent data corruption and racing downloads.

const (
	YtDlpName    = "yt-dlp"
	YtDlpVersion = "2026.10.21"

	// YtDlpURL is the official upstream pure-Python zipapp (The Unlicense + ISC meriyah + MIT astring).
	// Excludes all PyInstaller standalone executables bundling GPL mutagen.
	YtDlpURL = "https://github.com/yt-dlp/yt-dlp/releases/download/2026.10.21/yt-dlp"

	// YtDlpSHA256 is the verified digest of the 2026.10.21 zipapp.
	YtDlpSHA256 = "b7af77f686e95f7845ce8d09d21957718febb8b48b30555adb9dd2292ad7bf14"

	WhisperCppName     = "whisper-cpp"
	WhisperCppVersion  = "whisper-cpp-v1.9.5-bashy.1"
	WhisperCppRepo     = "qiangli/bashy"
	WhisperCppIdentity = "https://github.com/qiangli/bashy/.github/workflows/s503-whisper-release.yml@refs/tags/" + WhisperCppVersion
)

// YtDlpPlatforms covers all six release targets because the pure Python zipapp
// is executed by the managed Python runtime (yoke/external/python).
var YtDlpPlatforms = []string{
	"linux/amd64", "linux/arm64",
	"darwin/amd64", "darwin/arm64",
	"windows/amd64", "windows/arm64",
}

// WhisperCppPlatforms lists the six release targets.
var WhisperCppPlatforms = []string{
	"linux/amd64", "linux/arm64",
	"darwin/amd64", "darwin/arm64",
	"windows/amd64", "windows/arm64",
}

func whisperCLIFile(platform string) string {
	osName, arch, _ := strings.Cut(platform, "/")
	name := "whisper-cli-" + osName + "-" + arch
	if osName == "windows" {
		name += ".exe"
	}
	return name
}

func whisperReleaseBase() string {
	return "https://github.com/" + WhisperCppRepo + "/releases/download/" + WhisperCppVersion + "/"
}

// YtDlpTool declares the verified permissive yt-dlp zipapp across all six targets.
func YtDlpTool() Tool {
	t := Tool{
		Name:    YtDlpName,
		Version: YtDlpVersion,
		Assets:  make(map[string]Asset, len(YtDlpPlatforms)),
	}
	for _, p := range YtDlpPlatforms {
		t.Assets[p] = Asset{
			URL:    YtDlpURL,
			SHA256: YtDlpSHA256,
		}
	}
	return t
}

// WhisperCppTool declares the verified first-party signed whisper.cpp CLI specification
// across all six release targets.
// Upstream stable release v1.9.5 publishes no prebuilt standalone CLI release archives
// (assets_count = 0) and nightly builds (b5454) are excluded by policy.
// This declaration models the first-party Bashy tagged release with keyless
// Sigstore bundles and a signed SHA256SUMS containing all six executables,
// the reviewed notice bundle, and the release manifest.
// Because prebuilt binaries have not yet been built and signed in pins.go, RequireCommittedPin
// enforces fail-closed rejection on missing pins across all six target platforms.
func WhisperCppTool() Tool {
	t := Tool{
		Name:                WhisperCppName,
		Version:             WhisperCppVersion,
		Assets:              make(map[string]Asset, len(WhisperCppPlatforms)),
		Sigstore:            &Sigstore{Identity: WhisperCppIdentity, Issuer: GitHubActionsIssuer},
		RequireCommittedPin: true,
	}
	base := whisperReleaseBase()
	t.SignedSums = &SignedSums{
		URL:       base + "SHA256SUMS",
		BundleURL: base + "SHA256SUMS.sigstore.json",
		RequiredNames: []string{"WHISPER-NOTICES.txt", "whisper-release-manifest.json", "whisper-build-evidence.tar.gz"},
	}
	for _, p := range WhisperCppPlatforms {
		name := whisperCLIFile(p)
		t.Assets[p] = Asset{
			URL:            base + name,
			SigstoreBundle: base + name + DefaultBundleSuffix,
		}
	}
	return t
}

// EnsureYtDlp provisions the pure-Python zipapp for the host platform.
func EnsureYtDlp(ctx context.Context) (string, error) {
	return Ensure(ctx, YtDlpTool())
}

// EnsureWhisperCpp attempts to provision the whisper.cpp CLI binary.
// Upstream stable release v1.9.5 publishes no prebuilt CLI release archives across
// any target platform (assets_count = 0). This function fails closed with an
// explicit blocker error describing the admitted resolution paths (pinned source build
// via EnsureWhisperCppSource or first-party signed CI release), rather than falling
// back to nightly builds.
func EnsureWhisperCpp(ctx context.Context) (string, error) {
	tool := WhisperCppTool()
	p := Platform()
	if _, ok := pinnedSHA256(tool.Name, tool.Version, p); !ok {
		return "", fmt.Errorf("binmgr: %s %s has no committed sha256 for %s (upstream stable release v1.9.5 publishes no precompiled CLI assets; nightly builds are excluded; build from pinned source via EnsureWhisperCppSource or await first-party signed release at %s)", tool.Name, tool.Version, p, WhisperCppRepo)
	}
	return Ensure(ctx, tool)
}

const (
	DenoName    = "deno"
	DenoVersion = "v2.9.8"
	DenoBaseURL = "https://github.com/denoland/deno/releases/download/" + DenoVersion + "/"
)

// DenoPlatforms lists all six target platforms supported by official upstream prebuilt archives.
var DenoPlatforms = []string{
	"linux/amd64", "linux/arm64",
	"darwin/amd64", "darwin/arm64",
	"windows/amd64", "windows/arm64",
}

func denoAssetFile(platform string) string {
	switch platform {
	case "darwin/arm64":
		return "deno-aarch64-apple-darwin.zip"
	case "darwin/amd64":
		return "deno-x86_64-apple-darwin.zip"
	case "linux/arm64":
		return "deno-aarch64-unknown-linux-gnu.zip"
	case "linux/amd64":
		return "deno-x86_64-unknown-linux-gnu.zip"
	case "windows/arm64":
		return "deno-aarch64-pc-windows-msvc.zip"
	case "windows/amd64":
		return "deno-x86_64-pc-windows-msvc.zip"
	default:
		return ""
	}
}

// DenoTool declares the official upstream Deno runtime across all six release targets.
// Deno is the recommended JavaScript engine for yt-dlp to solve dynamic YouTube player
// signature and throttling challenges via `--no-remote-components --js-runtimes deno:<path>`.
func DenoTool() Tool {
	t := Tool{
		Name:    DenoName,
		Version: DenoVersion,
		Assets:  make(map[string]Asset, len(DenoPlatforms)),
	}
	for _, p := range DenoPlatforms {
		file := denoAssetFile(p)
		binary := "deno"
		if strings.HasPrefix(p, "windows/") {
			binary = "deno.exe"
		}
		pin, _ := pinnedSHA256(DenoName, DenoVersion, p)
		t.Assets[p] = Asset{
			URL:    DenoBaseURL + file,
			SHA256: pin,
			Binary: binary,
		}
	}
	return t
}

// EnsureDeno provisions the official prebuilt Deno binary for the host platform,
// downloading, verifying against committed SHA-256 pin, unpacking from zip, and caching locally.
func EnsureDeno(ctx context.Context) (string, error) {
	return Ensure(ctx, DenoTool())
}

const (
	YtDlpEjsName    = "yt-dlp-ejs"
	YtDlpEjsVersion = "0.8.0"

	// YtDlpEjsWheelURL is the official upstream pure-Python wheel (The Unlicense + ISC meriyah + MIT astring).
	YtDlpEjsWheelURL    = "https://github.com/yt-dlp/ejs/releases/download/0.8.0/yt_dlp_ejs-0.8.0-py3-none-any.whl"
	YtDlpEjsWheelSHA256 = "79300e5fca7f937a1eeede11f0456862c1b41107ce1d726871e0207424f4bdb4"
	YtDlpEjsWheelSize   = 53443

	// YtDlpEjsSolverURL is the official bundled JavaScript solver script (The Unlicense + ISC meriyah + MIT astring).
	YtDlpEjsSolverURL    = "https://github.com/yt-dlp/ejs/releases/download/0.8.0/yt.solver.lib.min.js"
	YtDlpEjsSolverSHA256 = "c55987fe697e5b9ee18830163f7af85327e9bb5c3e674b969d38c8d205eaa577"
	YtDlpEjsSolverSize   = 151561
)

// YtDlpEjsPackageInfo describes the pinned upstream yt-dlp-ejs release artifacts.
type YtDlpEjsPackageInfo struct {
	Version      string
	WheelURL     string
	WheelSHA256  string
	WheelSize    int64
	SolverURL    string
	SolverSHA256 string
	SolverSize   int64
}

// YtDlpEjsPackage returns metadata for the pinned yt-dlp-ejs component artifacts.
func YtDlpEjsPackage() YtDlpEjsPackageInfo {
	return YtDlpEjsPackageInfo{
		Version:      YtDlpEjsVersion,
		WheelURL:     YtDlpEjsWheelURL,
		WheelSHA256:  YtDlpEjsWheelSHA256,
		WheelSize:    YtDlpEjsWheelSize,
		SolverURL:    YtDlpEjsSolverURL,
		SolverSHA256: YtDlpEjsSolverSHA256,
		SolverSize:   YtDlpEjsSolverSize,
	}
}

// YtDlpEjsPackageDir returns the directory where yt-dlp-ejs artifacts are cached.
func YtDlpEjsPackageDir() (string, error) {
	if d := strings.TrimSpace(os.Getenv("BASHY_BIN_CACHE")); d != "" {
		return filepath.Join(filepath.Dir(d), "packages", "yt-dlp-ejs"), nil
	}
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "bashy", "packages", "yt-dlp-ejs"), nil
}

// EnsureYtDlpEjsWheel downloads and verifies the official pure-Python wheel.
// Concurrency-safe, verifies exact size and SHA-256, and caches offline.
func EnsureYtDlpEjsWheel(ctx context.Context) (string, error) {
	info := YtDlpEjsPackage()
	return ensureEjsArtifact(ctx, "wheel", "yt_dlp_ejs-0.8.0-py3-none-any.whl", info.WheelURL, info.WheelSHA256, info.WheelSize)
}

// EnsureYtDlpEjsSolver downloads and verifies the official bundled minified JavaScript solver.
// Concurrency-safe, verifies exact size and SHA-256, and caches offline.
func EnsureYtDlpEjsSolver(ctx context.Context) (string, error) {
	info := YtDlpEjsPackage()
	return ensureEjsArtifact(ctx, "solver", "yt.solver.lib.min.js", info.SolverURL, info.SolverSHA256, info.SolverSize)
}

const (
	WhisperCppSourceURL    = "https://github.com/ggml-org/whisper.cpp/archive/refs/tags/v1.9.5.tar.gz"
	WhisperCppSourceSHA256 = "ff1a9053feb509ff9d7729703355541ae9690073a6b1c40eb692c962e0dc1720"
	WhisperCppSourceSize   = 9626464
)

// WhisperCppSourceArchiveInfo describes the pinned upstream v1.9.5 source tarball.
type WhisperCppSourceArchiveInfo struct {
	Version string
	URL     string
	SHA256  string
	Size    int64
}

// WhisperCppSourceArchive returns the metadata for the pinned stable v1.9.5 source archive.
func WhisperCppSourceArchive() WhisperCppSourceArchiveInfo {
	return WhisperCppSourceArchiveInfo{
		Version: WhisperCppVersion,
		URL:     WhisperCppSourceURL,
		SHA256:  WhisperCppSourceSHA256,
		Size:    WhisperCppSourceSize,
	}
}

// WhisperSourceDir returns the directory where source archives are cached.
func WhisperSourceDir() (string, error) {
	if d := strings.TrimSpace(os.Getenv("BASHY_BIN_CACHE")); d != "" {
		return filepath.Join(filepath.Dir(d), "sources", "whisper"), nil
	}
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "bashy", "sources", "whisper"), nil
}

// Concurrency control structures for media downloads and source archives.
var (
	transcriptionMu sync.Mutex
	modelInFlight   = make(map[string]*modelFlight)
	sourceInFlight  = make(map[string]*sourceFlight)
	ejsInFlight     = make(map[string]*ejsFlight)
)

type modelFlight struct {
	done chan struct{}
	path string
	err  error
}

type sourceFlight struct {
	done chan struct{}
	path string
	err  error
}

type ejsFlight struct {
	done chan struct{}
	path string
	err  error
}

// EnsureWhisperCppSource downloads and verifies the official upstream stable v1.9.5 source archive.
// Downloads with fail-closed SHA-256 verification and rejects tampered archives.
// Concurrency-safe: serialized in-flight requests, unique temp files, and atomic caching.
func EnsureWhisperCppSource(ctx context.Context) (string, error) {
	info := WhisperCppSourceArchive()
	dir, err := WhisperSourceDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create whisper source dir: %w", err)
	}

	targetFile := filepath.Join(dir, fmt.Sprintf("whisper.cpp-%s.tar.gz", info.Version))

	// Fast cache hit check
	if fi, err := os.Stat(targetFile); err == nil && fi.Size() == info.Size {
		if valid, _ := verifyFileSHA256(targetFile, info.SHA256); valid {
			return targetFile, nil
		}
	}

	// Concurrency synchronization: if a download for this version is already running, wait.
	transcriptionMu.Lock()
	if flight, ok := sourceInFlight[info.Version]; ok {
		transcriptionMu.Unlock()
		select {
		case <-flight.done:
			return flight.path, flight.err
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	flight := &sourceFlight{done: make(chan struct{})}
	sourceInFlight[info.Version] = flight
	transcriptionMu.Unlock()

	defer func() {
		transcriptionMu.Lock()
		delete(sourceInFlight, info.Version)
		close(flight.done)
		transcriptionMu.Unlock()
	}()

	// Re-check cache under flight ownership
	if fi, err := os.Stat(targetFile); err == nil && fi.Size() == info.Size {
		if valid, _ := verifyFileSHA256(targetFile, info.SHA256); valid {
			flight.path = targetFile
			return targetFile, nil
		}
	}

	tmp, err := os.CreateTemp(dir, fmt.Sprintf(".dl-whisper-source-%s-*", info.Version))
	if err != nil {
		flight.err = fmt.Errorf("create temp source file: %w", err)
		return "", flight.err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, info.URL, nil)
	if err != nil {
		flight.err = fmt.Errorf("create source request: %w", err)
		_ = tmp.Close()
		return "", flight.err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		flight.err = fmt.Errorf("download whisper source: %w", err)
		_ = tmp.Close()
		return "", flight.err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		flight.err = fmt.Errorf("download whisper source: http status %d", resp.StatusCode)
		_ = tmp.Close()
		return "", flight.err
	}

	hasher := sha256.New()
	mw := io.MultiWriter(tmp, hasher)

	copied, err := io.Copy(mw, resp.Body)
	closeErr := tmp.Close()
	if err != nil {
		flight.err = fmt.Errorf("stream whisper source: %w", err)
		return "", flight.err
	}
	if closeErr != nil {
		flight.err = fmt.Errorf("close whisper source: %w", closeErr)
		return "", flight.err
	}

	if copied != info.Size {
		flight.err = fmt.Errorf("download whisper source: expected %d bytes, got %d", info.Size, copied)
		return "", flight.err
	}

	actualSHA := hex.EncodeToString(hasher.Sum(nil))
	if !strings.EqualFold(actualSHA, info.SHA256) {
		flight.err = fmt.Errorf("rejected tampered whisper source %s: expected sha256 %s, got %s", info.Version, info.SHA256, actualSHA)
		return "", flight.err
	}

	if err := os.Rename(tmpName, targetFile); err != nil {
		if fi, serr := os.Stat(targetFile); serr == nil && fi.Size() == info.Size {
			if valid, _ := verifyFileSHA256(targetFile, info.SHA256); valid {
				flight.path = targetFile
				return targetFile, nil
			}
		}
		flight.err = fmt.Errorf("install whisper source: %w", err)
		return "", flight.err
	}

	flight.path = targetFile
	return targetFile, nil
}

// WhisperModelInfo describes a pinned OpenAI Whisper GGML model weight file.
type WhisperModelInfo struct {
	Name   string
	URL    string
	SHA256 string
	Size   int64
}

var (
	whisperModelsMu sync.RWMutex

	// WhisperModels catalogs the 10 GGML model files at the verified immutable
	// Hugging Face repository revision 5359861c739e955e79d9a303bcbc70fb988958b1.
	// Digests and sizes come from that revision's file metadata; tiny was also
	// independently streamed and hashed during Sprint 503 review.
	WhisperModels = map[string]WhisperModelInfo{
		"tiny": {
			Name:   "tiny",
			URL:    "https://huggingface.co/ggerganov/whisper.cpp/resolve/5359861c739e955e79d9a303bcbc70fb988958b1/ggml-tiny.bin",
			SHA256: "be07e048e1e599ad46341c8d2a135645097a538221678b7acdd1b1919c6e1b21",
			Size:   77691713,
		},
		"tiny.en": {
			Name:   "tiny.en",
			URL:    "https://huggingface.co/ggerganov/whisper.cpp/resolve/5359861c739e955e79d9a303bcbc70fb988958b1/ggml-tiny.en.bin",
			SHA256: "921e4cf8686fdd993dcd081a5da5b6c365bfde1162e72b08d75ac75289920b1f",
			Size:   77704715,
		},
		"base": {
			Name:   "base",
			URL:    "https://huggingface.co/ggerganov/whisper.cpp/resolve/5359861c739e955e79d9a303bcbc70fb988958b1/ggml-base.bin",
			SHA256: "60ed5bc3dd14eea856493d334349b405782ddcaf0028d4b5df4088345fba2efe",
			Size:   147951465,
		},
		"base.en": {
			Name:   "base.en",
			URL:    "https://huggingface.co/ggerganov/whisper.cpp/resolve/5359861c739e955e79d9a303bcbc70fb988958b1/ggml-base.en.bin",
			SHA256: "a03779c86df3323075f5e796cb2ce5029f00ec8869eee3fdfb897afe36c6d002",
			Size:   147964211,
		},
		"small": {
			Name:   "small",
			URL:    "https://huggingface.co/ggerganov/whisper.cpp/resolve/5359861c739e955e79d9a303bcbc70fb988958b1/ggml-small.bin",
			SHA256: "1be3a9b2063867b937e64e2ec7483364a79917e157fa98c5d94b5c1fffea987b",
			Size:   487601967,
		},
		"small.en": {
			Name:   "small.en",
			URL:    "https://huggingface.co/ggerganov/whisper.cpp/resolve/5359861c739e955e79d9a303bcbc70fb988958b1/ggml-small.en.bin",
			SHA256: "c6138d6d58ecc8322097e0f987c32f1be8bb0a18532a3f88f734d1bbf9c41e5d",
			Size:   487614201,
		},
		"medium": {
			Name:   "medium",
			URL:    "https://huggingface.co/ggerganov/whisper.cpp/resolve/5359861c739e955e79d9a303bcbc70fb988958b1/ggml-medium.bin",
			SHA256: "6c14d5adee5f86394037b4e4e8b59f1673b6cee10e3cf0b11bbdbee79c156208",
			Size:   1533763059,
		},
		"medium.en": {
			Name:   "medium.en",
			URL:    "https://huggingface.co/ggerganov/whisper.cpp/resolve/5359861c739e955e79d9a303bcbc70fb988958b1/ggml-medium.en.bin",
			SHA256: "cc37e93478338ec7700281a7ac30a10128929eb8f427dda2e865faa8f6da4356",
			Size:   1533774781,
		},
		"large-v3": {
			Name:   "large-v3",
			URL:    "https://huggingface.co/ggerganov/whisper.cpp/resolve/5359861c739e955e79d9a303bcbc70fb988958b1/ggml-large-v3.bin",
			SHA256: "64d182b440b98d5203c4f9bd541544d84c605196c4f7b845dfa11fb23594d1e2",
			Size:   3095033483,
		},
		"large-v3-turbo": {
			Name:   "large-v3-turbo",
			URL:    "https://huggingface.co/ggerganov/whisper.cpp/resolve/5359861c739e955e79d9a303bcbc70fb988958b1/ggml-large-v3-turbo.bin",
			SHA256: "1fc70f774d38eb169993ac391eea357ef47c88757ef72ee5943879b7e8e2bc69",
			Size:   1624555275,
		},
	}
)

// RegisterTestWhisperModel registers or overrides a model in WhisperModels under write lock,
// returning a cleanup function that restores original state.
func RegisterTestWhisperModel(name string, info WhisperModelInfo) func() {
	whisperModelsMu.Lock()
	defer whisperModelsMu.Unlock()
	orig, exists := WhisperModels[name]
	WhisperModels[name] = info
	return func() {
		whisperModelsMu.Lock()
		defer whisperModelsMu.Unlock()
		if exists {
			WhisperModels[name] = orig
		} else {
			delete(WhisperModels, name)
		}
	}
}

// WhisperModel resolves a model name to its catalog metadata.
func WhisperModel(name string) (WhisperModelInfo, error) {
	name = strings.TrimPrefix(name, "ggml-")
	name = strings.TrimSuffix(name, ".bin")
	whisperModelsMu.RLock()
	defer whisperModelsMu.RUnlock()
	info, ok := WhisperModels[name]
	if !ok {
		return WhisperModelInfo{}, fmt.Errorf("unknown whisper model: %q", name)
	}
	return info, nil
}

// WhisperModelDir returns the directory where whisper model weights are cached.
func WhisperModelDir() (string, error) {
	if d := strings.TrimSpace(os.Getenv("BASHY_BIN_CACHE")); d != "" {
		return filepath.Join(filepath.Dir(d), "models", "whisper"), nil
	}
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "bashy", "models", "whisper"), nil
}

func verifyFileSHA256(path, expectedSHA string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false, err
	}

	actual := hex.EncodeToString(h.Sum(nil))
	return strings.EqualFold(actual, expectedSHA), nil
}

// EnsureWhisperModel intentionally acquires and verifies a Whisper model.
// Downloads with fail-closed SHA-256 verification and rejects tampered artifacts.
// Concurrency-safe: serialized in-flight requests, unique temp files, and atomic caching.
func EnsureWhisperModel(ctx context.Context, name string) (string, error) {
	info, err := WhisperModel(name)
	if err != nil {
		return "", err
	}

	dir, err := WhisperModelDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create whisper model dir: %w", err)
	}

	targetFile := filepath.Join(dir, fmt.Sprintf("ggml-%s.bin", info.Name))

	// Fast cache hit check
	if fi, err := os.Stat(targetFile); err == nil && fi.Size() == info.Size {
		if valid, _ := verifyFileSHA256(targetFile, info.SHA256); valid {
			return targetFile, nil
		}
	}

	// Concurrency synchronization: if a download for this model is already in progress,
	// wait for it rather than duplicating network/disk work or colliding on temp files.
	transcriptionMu.Lock()
	if flight, ok := modelInFlight[info.Name]; ok {
		transcriptionMu.Unlock()
		select {
		case <-flight.done:
			return flight.path, flight.err
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	flight := &modelFlight{done: make(chan struct{})}
	modelInFlight[info.Name] = flight
	transcriptionMu.Unlock()

	defer func() {
		transcriptionMu.Lock()
		delete(modelInFlight, info.Name)
		close(flight.done)
		transcriptionMu.Unlock()
	}()

	// Re-check cache under flight ownership in case another process/flight finished
	if fi, err := os.Stat(targetFile); err == nil && fi.Size() == info.Size {
		if valid, _ := verifyFileSHA256(targetFile, info.SHA256); valid {
			flight.path = targetFile
			return targetFile, nil
		}
	}

	// Create unique temporary file in destination directory
	tmp, err := os.CreateTemp(dir, fmt.Sprintf(".dl-%s-*", info.Name))
	if err != nil {
		flight.err = fmt.Errorf("create temp model file: %w", err)
		return "", flight.err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, info.URL, nil)
	if err != nil {
		flight.err = fmt.Errorf("create model request: %w", err)
		_ = tmp.Close()
		return "", flight.err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		flight.err = fmt.Errorf("download whisper model: %w", err)
		_ = tmp.Close()
		return "", flight.err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		flight.err = fmt.Errorf("download whisper model: http status %d", resp.StatusCode)
		_ = tmp.Close()
		return "", flight.err
	}

	hasher := sha256.New()
	mw := io.MultiWriter(tmp, hasher)

	copied, err := io.Copy(mw, resp.Body)
	closeErr := tmp.Close()
	if err != nil {
		flight.err = fmt.Errorf("stream whisper model: %w", err)
		return "", flight.err
	}
	if closeErr != nil {
		flight.err = fmt.Errorf("close whisper model: %w", closeErr)
		return "", flight.err
	}

	if copied != info.Size {
		flight.err = fmt.Errorf("download whisper model: expected %d bytes, got %d", info.Size, copied)
		return "", flight.err
	}

	actualSHA := hex.EncodeToString(hasher.Sum(nil))
	if !strings.EqualFold(actualSHA, info.SHA256) {
		flight.err = fmt.Errorf("rejected tampered model %s: expected sha256 %s, got %s", info.Name, info.SHA256, actualSHA)
		return "", flight.err
	}

	// Atomic publish
	if err := os.Rename(tmpName, targetFile); err != nil {
		// Fallback for cross-device or Windows locked file replacement
		if fi, serr := os.Stat(targetFile); serr == nil && fi.Size() == info.Size {
			if valid, _ := verifyFileSHA256(targetFile, info.SHA256); valid {
				flight.path = targetFile
				return targetFile, nil
			}
		}
		flight.err = fmt.Errorf("install whisper model: %w", err)
		return "", flight.err
	}

	flight.path = targetFile
	return targetFile, nil
}

func ensureEjsArtifact(ctx context.Context, kind, filename, url, expectedSHA string, expectedSize int64) (string, error) {
	dir, err := YtDlpEjsPackageDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create ejs package dir: %w", err)
	}

	targetFile := filepath.Join(dir, filename)
	if fi, err := os.Stat(targetFile); err == nil && fi.Size() == expectedSize {
		if valid, _ := verifyFileSHA256(targetFile, expectedSHA); valid {
			return targetFile, nil
		}
	}

	flightKey := kind + ":" + filename
	transcriptionMu.Lock()
	if flight, ok := ejsInFlight[flightKey]; ok {
		transcriptionMu.Unlock()
		select {
		case <-flight.done:
			return flight.path, flight.err
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	flight := &ejsFlight{done: make(chan struct{})}
	ejsInFlight[flightKey] = flight
	transcriptionMu.Unlock()

	defer func() {
		transcriptionMu.Lock()
		delete(ejsInFlight, flightKey)
		close(flight.done)
		transcriptionMu.Unlock()
	}()

	if fi, err := os.Stat(targetFile); err == nil && fi.Size() == expectedSize {
		if valid, _ := verifyFileSHA256(targetFile, expectedSHA); valid {
			flight.path = targetFile
			return targetFile, nil
		}
	}

	tmp, err := os.CreateTemp(dir, fmt.Sprintf(".dl-ejs-%s-*", kind))
	if err != nil {
		flight.err = fmt.Errorf("create temp ejs file: %w", err)
		return "", flight.err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		flight.err = fmt.Errorf("create ejs request: %w", err)
		_ = tmp.Close()
		return "", flight.err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		flight.err = fmt.Errorf("download ejs %s: %w", kind, err)
		_ = tmp.Close()
		return "", flight.err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		flight.err = fmt.Errorf("download ejs %s: http status %d", kind, resp.StatusCode)
		_ = tmp.Close()
		return "", flight.err
	}

	hasher := sha256.New()
	mw := io.MultiWriter(tmp, hasher)

	copied, err := io.Copy(mw, resp.Body)
	closeErr := tmp.Close()
	if err != nil {
		flight.err = fmt.Errorf("stream ejs %s: %w", kind, err)
		return "", flight.err
	}
	if closeErr != nil {
		flight.err = fmt.Errorf("close ejs %s: %w", kind, closeErr)
		return "", flight.err
	}

	if copied != expectedSize {
		flight.err = fmt.Errorf("download ejs %s: expected %d bytes, got %d", kind, expectedSize, copied)
		return "", flight.err
	}

	actualSHA := hex.EncodeToString(hasher.Sum(nil))
	if !strings.EqualFold(actualSHA, expectedSHA) {
		flight.err = fmt.Errorf("rejected tampered ejs %s: expected sha256 %s, got %s", kind, expectedSHA, actualSHA)
		return "", flight.err
	}

	if err := os.Rename(tmpName, targetFile); err != nil {
		if fi, serr := os.Stat(targetFile); serr == nil && fi.Size() == expectedSize {
			if valid, _ := verifyFileSHA256(targetFile, expectedSHA); valid {
				flight.path = targetFile
				return targetFile, nil
			}
		}
		flight.err = fmt.Errorf("install ejs %s: %w", kind, err)
		return "", flight.err
	}

	flight.path = targetFile
	return targetFile, nil
}

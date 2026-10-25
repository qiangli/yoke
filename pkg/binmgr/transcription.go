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
//    - Runs across all six release targets under Python 3.9+ (provisioned by bashy via uv / CPython 3.15,
//      PSF-2.0, yoke/external/python).
//    - JavaScript challenges (n-sig / player signatures) are resolved by invoking
//      Node.js via `--js-runtimes node`, reusing the repository's existing Node.js
//      provisioning (v22.23.2 LTS, MIT, yoke/external/node).
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
//    - Admitted resolution path: a verified first-party CI build (qiangli/whisper.cpp tagged at v1.9.5
//      signed with Sigstore via bashy-build.yml), following the proven pattern in yoke/pkg/binmgr/prima.go.
//
// 3. Media Decoder / FFmpeg (LGPL-2.1-or-later / GPL-2.0-or-later):
//    - Strict permissive packaging CANNOT be satisfied: FFmpeg has no permissive license option.
//    - Packaging FFmpeg remains an unresolved blocker pending an explicit operator policy decision.
//    - Proposed interim path per target, pending operator decision: unbundled host discovery
//      on $PATH (Option A), plus native decoders where supported (afconvert on darwin; dr_wav for WAV).
//    - No copyleft FFmpeg executable is bundled, downloaded, or distributed via binmgr.
//
// 4. Models (OpenAI Whisper - MIT License):
//    - Pinned official GGML weights from Hugging Face (ggerganov/whisper.cpp).
//    - Acquired intentionally by name, verified against committed SHA-256 digests,
//      and cached offline. Tampered downloads are rejected fail-closed.

const (
	YtDlpName    = "yt-dlp"
	YtDlpVersion = "2026.10.21"

	// YtDlpURL is the official upstream pure-Python zipapp (The Unlicense + ISC meriyah + MIT astring).
	// Excludes all PyInstaller standalone executables bundling GPL mutagen.
	YtDlpURL = "https://github.com/yt-dlp/yt-dlp/releases/download/2026.10.21/yt-dlp"

	// YtDlpSHA256 is the verified digest of the 2026.10.21 zipapp.
	YtDlpSHA256 = "b7af77f686e95f7845ce8d09d21957718febb8b48b30555adb9dd2292ad7bf14"

	WhisperCppName    = "whisper-cpp"
	WhisperCppVersion = "v1.9.5"
)

// YtDlpPlatforms covers all six release targets because the pure Python zipapp
// is executed by the managed Python runtime (yoke/external/python).
var YtDlpPlatforms = []string{
	"linux/amd64", "linux/arm64",
	"darwin/amd64", "darwin/arm64",
	"windows/amd64", "windows/arm64",
}

// WhisperCppPlatforms lists the release targets. Upstream stable release v1.9.5 publishes
// no standalone prebuilt CLI archives (assets_count = 0); all 6 targets have missing
// stable assets upstream and are recorded as blockers.
var WhisperCppPlatforms = []string{
	"linux/amd64", "linux/arm64",
	"darwin/amd64", "darwin/arm64",
	"windows/amd64", "windows/arm64",
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

// WhisperCppTool declares the tool configuration for whisper.cpp.
// Because upstream stable release v1.9.5 publishes no prebuilt CLI release archives
// (and nightly build b5454 is excluded), Assets is empty; missing stable prebuilt
// assets are recorded as explicit upstream blockers.
func WhisperCppTool() Tool {
	return Tool{
		Name:    WhisperCppName,
		Version: WhisperCppVersion,
		Assets:  make(map[string]Asset),
	}
}

// WhisperModelInfo describes a pinned OpenAI Whisper GGML model weight file.
type WhisperModelInfo struct {
	Name   string
	URL    string
	SHA256 string
	Size   int64
}

// WhisperModels catalogs the 10 GGML model files at the verified immutable
// Hugging Face repository revision 5359861c739e955e79d9a303bcbc70fb988958b1.
// Digests and sizes come from that revision's file metadata; tiny was also
// independently streamed and hashed during Sprint 503 review.
var WhisperModels = map[string]WhisperModelInfo{
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

// WhisperModel resolves a model name to its catalog metadata.
func WhisperModel(name string) (WhisperModelInfo, error) {
	name = strings.TrimPrefix(name, "ggml-")
	name = strings.TrimSuffix(name, ".bin")
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

	// Check if already cached and valid
	if fi, err := os.Stat(targetFile); err == nil && fi.Size() == info.Size {
		if valid, _ := verifyFileSHA256(targetFile, info.SHA256); valid {
			return targetFile, nil
		}
		_ = os.Remove(targetFile)
	}

	// Download to temporary file
	tmpFile := targetFile + ".tmp"
	_ = os.Remove(tmpFile)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, info.URL, nil)
	if err != nil {
		return "", fmt.Errorf("create model request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("download whisper model: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download whisper model: http status %d", resp.StatusCode)
	}

	out, err := os.OpenFile(tmpFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return "", fmt.Errorf("create temp model file: %w", err)
	}

	hasher := sha256.New()
	mw := io.MultiWriter(out, hasher)

	copied, err := io.Copy(mw, resp.Body)
	_ = out.Close()
	if err != nil {
		_ = os.Remove(tmpFile)
		return "", fmt.Errorf("stream whisper model: %w", err)
	}

	if copied != info.Size {
		_ = os.Remove(tmpFile)
		return "", fmt.Errorf("download whisper model: expected %d bytes, got %d", info.Size, copied)
	}

	actualSHA := hex.EncodeToString(hasher.Sum(nil))
	if !strings.EqualFold(actualSHA, info.SHA256) {
		_ = os.Remove(tmpFile)
		return "", fmt.Errorf("rejected tampered model %s: expected sha256 %s, got %s", info.Name, info.SHA256, actualSHA)
	}

	if err := os.Rename(tmpFile, targetFile); err != nil {
		_ = os.Remove(tmpFile)
		return "", fmt.Errorf("install whisper model: %w", err)
	}

	return targetFile, nil
}

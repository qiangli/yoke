package runner_embed

// CUDA RUNNER (S546.1 slice 2, S451.4a release). The CUDA variant is a
// separate bashy-signed release of the same pinned runner source plus two
// backend libraries. It is too large to embed (~1.8 GB), so it is fetched
// with binmgr from the pinned release, every file verified against its
// Sigstore identity and reviewed sha256, and installed side by side in
// upstream's layout:
//
//	<root>/<version>/bin/ycode-runner
//	<root>/<version>/lib/ollama/libggml-cuda.so
//	<root>/<version>/lib/ollama/libggml-base.so.0
//
// Upstream ggml SILENTLY falls back to CPU when the CUDA backend cannot load.
// Upstream stays unpatched, so failing closed is this package's job: before
// start (layout + host runtime) and after start (the runner's own log must
// report a CUDA device, see cuda_run.go). The driver and CUDA runtime
// (libcuda, libcudart, libcublas) are HOST-provided and never bundled.

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/qiangli/yoke/pkg/binmgr"
)

// BackendEnv explicitly selects the runner backend: "cuda" or "cpu". Empty
// means auto: CUDA on a provisioned GPU worker, the embedded runner otherwise.
const BackendEnv = "BASHY_OLLAMA_BACKEND"

// GPUWorkerEnv marks a host created by a cloud GPU provision request (set in
// the service environment by the worker bootstrapper, see outpost settings).
const GPUWorkerEnv = "OUTPOST_GPU_WORKER_REQUEST_ID"

// Backend is the runner variant a launch uses.
type Backend string

const (
	// BackendDefault is the embedded runner (CPU on Linux, Metal on macOS).
	BackendDefault Backend = ""
	// BackendCPU is an explicit CPU selection: the embedded runner, even on a
	// GPU worker.
	BackendCPU Backend = "cpu"
	// BackendCUDA is the fetched CUDA runner; it never degrades to CPU.
	BackendCUDA Backend = "cuda"
)

// SelectBackend resolves the backend from getenv. An explicit BackendEnv
// wins; otherwise the GPU-worker marker requests CUDA. CUDA is offered on
// linux/amd64 only: a CUDA request anywhere else is an error, never a quiet
// CPU run.
func SelectBackend(getenv func(string) string) (Backend, error) {
	return selectBackend(getenv, hostGOOS, hostGOARCH)
}

// hostGOOS/hostGOARCH are vars so the dispatch tests run on every platform.
var hostGOOS, hostGOARCH = runtime.GOOS, runtime.GOARCH

func selectBackend(getenv func(string) string, goos, goarch string) (Backend, error) {
	b := BackendDefault
	switch v := strings.ToLower(strings.TrimSpace(getenv(BackendEnv))); v {
	case "":
		if strings.TrimSpace(getenv(GPUWorkerEnv)) != "" {
			b = BackendCUDA
		}
	case "cpu":
		return BackendCPU, nil
	case "cuda":
		b = BackendCUDA
	default:
		return "", fmt.Errorf("runner: %s=%q is not a backend (use cuda or cpu)", BackendEnv, v)
	}
	if b == BackendCUDA && goos+"/"+goarch != "linux/amd64" {
		return "", fmt.Errorf("runner: CUDA runner is published for linux/amd64 only, not %s/%s; set %s=cpu to run on CPU explicitly", goos, goarch, BackendEnv)
	}
	return b, nil
}

// cudaFile is one signed release asset of the CUDA runner.
type cudaFile struct {
	asset  string // release asset name
	rel    string // slash path inside the install layout
	sha256 string // reviewed pin of the final bytes
}

type cudaReleaseMetadata struct {
	version, identity, baseURL string
	files                      []cudaFile
}

// The CUDA release pins. The manager fills the sha256 values from the
// reviewed, puppy-proven, promoted release; until then a CUDA selection fails
// closed with a message naming the missing pins.
const (
	cudaReleaseVersion  = "bashy-v0.21.2-16"
	cudaReleaseIdentity = "https://github.com/qiangli/ollama/.github/workflows/bashy-build.yml@refs/tags/" + cudaReleaseVersion
	cudaReleaseBaseURL  = "https://github.com/qiangli/ollama/releases/download/" + cudaReleaseVersion + "/"

	cudaRunnerSHA256   = ""
	cudaLibCUDASHA256  = ""
	cudaLibBaseSHA256  = ""
	cudaLibCUDAName    = "libggml-cuda.so"
	cudaLibBaseName    = "libggml-base.so.0"
	cudaLibDirRel      = "lib/ollama"
	cudaRunnerRelPath  = "bin/" + runnerName
	cudaToolNamePrefix = "ollama-runner-cuda"
)

var cudaRelease = cudaReleaseMetadata{
	version:  cudaReleaseVersion,
	identity: cudaReleaseIdentity,
	baseURL:  cudaReleaseBaseURL,
	files: []cudaFile{
		{"runner-linux-amd64-cuda", cudaRunnerRelPath, cudaRunnerSHA256},
		{"runner-linux-amd64-cuda-libggml-cuda.so", cudaLibDirRel + "/" + cudaLibCUDAName, cudaLibCUDASHA256},
		{"runner-linux-amd64-cuda-libggml-base.so.0", cudaLibDirRel + "/" + cudaLibBaseName, cudaLibBaseSHA256},
	},
}

// Seams for tests: the fetcher (binmgr.Ensure), the verifier over installed
// bytes, the host-runtime probe, and the install root.
var (
	fetchCUDAAsset  = binmgr.Ensure
	verifyCUDAFile  = binmgr.VerifySignedBlob
	hostLdconfig    = ldconfigCache
	cudaInstallRoot = defaultCUDAInstallRoot
)

// ErrCUDAUnavailable marks every CUDA fail-closed refusal.
var ErrCUDAUnavailable = errors.New("CUDA runner unavailable")

// CUDALaunch is a verified, preflighted CUDA runner install.
type CUDALaunch struct {
	Runner string // bin/ycode-runner
	LibDir string // lib/ollama holding libggml-cuda.so + libggml-base.so.0
}

// Env returns env with the runner's library search pointed at LibDir. The
// in-process server sets OLLAMA_LIBRARY_PATH from ITS OWN executable, where
// no CUDA backend lives; left alone, ggml would load nothing and run on CPU.
func (l CUDALaunch) Env(env []string) []string {
	ld := l.LibDir
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, "LD_LIBRARY_PATH="); ok && v != "" {
			ld += string(filepath.ListSeparator) + v
		}
	}
	env = replaceEnv(env, "OLLAMA_LIBRARY_PATH", l.LibDir)
	return replaceEnv(env, "LD_LIBRARY_PATH", ld)
}

func replaceEnv(env []string, key, value string) []string {
	out := make([]string, 0, len(env)+1)
	for _, e := range env {
		if !strings.HasPrefix(e, key+"=") {
			out = append(out, e)
		}
	}
	return append(out, key+"="+value)
}

func cudaFail(format string, a ...any) error {
	return fmt.Errorf("runner: %w: "+format, append([]any{ErrCUDAUnavailable}, a...)...)
}

func cudaReleaseReady(r cudaReleaseMetadata) error {
	if r.version == "" || r.baseURL == "" || r.identity != "https://github.com/qiangli/ollama/.github/workflows/bashy-build.yml@refs/tags/"+r.version {
		return cudaFail("release identity for %q is not the bashy-build.yml tag workflow", r.version)
	}
	var missing []string
	for _, f := range r.files {
		if _, err := hex.DecodeString(f.sha256); err != nil || len(f.sha256) != 64 {
			missing = append(missing, f.asset)
		}
	}
	if len(missing) > 0 {
		return cudaFail("no reviewed sha256 pin for %s of release %s (pins are filled after the signed release is proven); set %s=cpu to run on CPU explicitly",
			strings.Join(missing, ", "), r.version, BackendEnv)
	}
	return nil
}

func defaultCUDAInstallRoot() (string, error) {
	root, err := binmgr.CacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, cudaToolNamePrefix+"-layout"), nil
}

// EnsureCUDA returns the verified CUDA runner install, fetching and
// installing it when the layout is absent or fails verification, then
// checks everything the runner needs BEFORE it starts. Every error wraps
// ErrCUDAUnavailable and names what is missing; there is no CPU fallback.
func EnsureCUDA(ctx context.Context) (CUDALaunch, error) {
	r := cudaRelease
	if err := cudaReleaseReady(r); err != nil {
		return CUDALaunch{}, err
	}
	root, err := cudaInstallRoot()
	if err != nil {
		return CUDALaunch{}, cudaFail("install root: %v", err)
	}
	dir := filepath.Join(root, r.version)
	if verr := verifyCUDALayout(ctx, r, dir); verr != nil {
		if err := installCUDALayout(ctx, r, dir); err != nil {
			if dirExists(dir) {
				// Name what is wrong with the install, not only the repair.
				return CUDALaunch{}, fmt.Errorf("%w (installed layout: %v)", err, verr)
			}
			return CUDALaunch{}, err
		}
		if err := verifyCUDALayout(ctx, r, dir); err != nil {
			return CUDALaunch{}, cudaFail("installed layout failed verification: %v", err)
		}
	}
	l := CUDALaunch{
		Runner: filepath.Join(dir, filepath.FromSlash(cudaRunnerRelPath)),
		LibDir: filepath.Join(dir, filepath.FromSlash(cudaLibDirRel)),
	}
	if err := PreflightCUDA(l); err != nil {
		return CUDALaunch{}, err
	}
	return l, nil
}

func bundlePath(p string) string { return p + binmgr.DefaultBundleSuffix }

// verifyCUDALayout checks every installed file against its pin and Sigstore
// identity. The bundle beside a file is evidence, never a trust root: the
// pin and identity are the committed constants above.
func verifyCUDALayout(ctx context.Context, r cudaReleaseMetadata, dir string) error {
	signer := binmgr.Sigstore{Identity: r.identity, Issuer: binmgr.GitHubActionsIssuer}
	for _, f := range r.files {
		p := filepath.Join(dir, filepath.FromSlash(f.rel))
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o022 != 0 {
			return fmt.Errorf("%s is not a private regular file", p)
		}
		bundle, err := os.ReadFile(bundlePath(p))
		if err != nil {
			return err
		}
		if err := verifyCUDAFile(ctx, p, bundle, f.sha256, signer); err != nil {
			return fmt.Errorf("%s: %w", f.asset, err)
		}
	}
	return nil
}

// installCUDALayout fetches every asset through binmgr (Sigstore + sha256
// verified over the downloaded bytes before it is cached) and places it in
// upstream's layout under its plain name. Files are hard-linked from the
// binmgr cache when possible (no second 1.8 GB copy), copied otherwise —
// never symlinked: the runner resolves lib/ollama from its real path and
// libggml-cuda.so finds libggml-base.so.0 through RUNPATH $ORIGIN.
func installCUDALayout(ctx context.Context, r cudaReleaseMetadata, dir string) error {
	signer := &binmgr.Sigstore{Identity: r.identity, Issuer: binmgr.GitHubActionsIssuer}
	for _, f := range r.files {
		tool := binmgr.Tool{
			Name:     cudaToolNamePrefix + strings.TrimPrefix(f.asset, "runner-linux-amd64-cuda"),
			Version:  r.version,
			Sigstore: signer,
			Assets: map[string]binmgr.Asset{"linux/amd64": {
				URL: r.baseURL + f.asset, SHA256: f.sha256,
			}},
		}
		cached, err := fetchCUDAAsset(ctx, tool)
		if err != nil {
			return cudaFail("fetch %s from release %s: %v", f.asset, r.version, err)
		}
		bundle, err := os.ReadFile(cached + ".sigstore-bundle.json")
		if err != nil {
			return cudaFail("fetched %s has no cached Sigstore bundle: %v", f.asset, err)
		}
		dest := filepath.Join(dir, filepath.FromSlash(f.rel))
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return cudaFail("create %s: %v", filepath.Dir(dest), err)
		}
		if err := placeFile(cached, dest); err != nil {
			return cudaFail("install %s: %v", f.asset, err)
		}
		if err := writeFileAtomic(bundlePath(dest), bundle, 0o644); err != nil {
			return cudaFail("install %s bundle: %v", f.asset, err)
		}
	}
	return nil
}

func placeFile(src, dest string) error {
	tmp := dest + ".tmp"
	_ = os.Remove(tmp)
	if err := os.Link(src, tmp); err != nil {
		if err := copyFile(src, tmp); err != nil {
			_ = os.Remove(tmp)
			return err
		}
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dest); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// copyFile streams src to dest; the CUDA backend is too large to buffer.
func copyFile(src, dest string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// hostCUDALibs are the host-provided CUDA runtime libraries, matched exactly
// as builds/ollama cuda-link-audit `host` matches them in `ldconfig -p`.
var hostCUDALibs = []string{"libcuda.so", "libcudart.so", "libcublas.so"}

// PreflightCUDA fails closed BEFORE start when the layout is incomplete or
// the host cannot provide the CUDA runtime.
func PreflightCUDA(l CUDALaunch) error {
	if !isExecutableFile(l.Runner) {
		return cudaFail("runner %s is missing or not executable", l.Runner)
	}
	cuda := filepath.Join(l.LibDir, cudaLibCUDAName)
	if !isRegularFile(cuda) {
		return cudaFail("CUDA backend %s is missing; without it ggml silently runs on CPU", cuda)
	}
	if base := filepath.Join(l.LibDir, cudaLibBaseName); !isRegularFile(base) {
		return cudaFail("%s is missing next to %s (libggml-cuda.so NEEDS it via RUNPATH $ORIGIN); reinstall the CUDA runner release", base, cuda)
	}
	text, err := hostLdconfig()
	if err != nil {
		return cudaFail("%v: no host CUDA runtime evidence; set %s=cpu to run on CPU explicitly", err, BackendEnv)
	}
	var missing []string
	for _, lib := range hostCUDALibs {
		if !strings.Contains(text, lib) {
			missing = append(missing, lib)
		}
	}
	if len(missing) > 0 {
		return cudaFail("host CUDA runtime missing %s (the NVIDIA driver and CUDA runtime are host-provided, never bundled: use a GPU host image with them installed, e.g. the AWS Deep Learning Base GPU AMI); set %s=cpu to run on CPU explicitly",
			strings.Join(missing, " "), BackendEnv)
	}
	return nil
}

// ldconfigCache returns `ldconfig -p`, finding ldconfig via PATH then
// /usr/sbin and /sbin (it is often outside a service's PATH).
func ldconfigCache() (string, error) {
	bin, err := exec.LookPath("ldconfig")
	if err != nil {
		bin = ""
		for _, c := range []string{"/usr/sbin/ldconfig", "/sbin/ldconfig"} {
			if isExecutableFile(c) {
				bin = c
				break
			}
		}
	}
	if bin == "" {
		return "", errors.New("ldconfig not found (PATH, /usr/sbin, /sbin)")
	}
	out, err := exec.Command(bin, "-p").Output()
	if err != nil {
		return "", fmt.Errorf("%s -p: %v", bin, err)
	}
	return string(out), nil
}

func dirExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

func isRegularFile(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.Mode().IsRegular()
}

func isExecutableFile(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0
}

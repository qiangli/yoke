package runner_embed

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/binmgr"
)

const fullLdconfig = `3 libs found in cache
	libcuda.so.1 (libc6,x86-64) => /usr/lib/x86_64-linux-gnu/libcuda.so.1
	libcudart.so.12 (libc6,x86-64) => /usr/local/cuda/lib64/libcudart.so.12
	libcublas.so.12 (libc6,x86-64) => /usr/local/cuda/lib64/libcublas.so.12
`

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestSelectBackend(t *testing.T) {
	cases := []struct {
		name       string
		env        map[string]string
		goos, arch string
		want       Backend
		wantErr    string
	}{
		{"default", nil, "linux", "amd64", BackendDefault, ""},
		{"gpu worker marker requests cuda", map[string]string{GPUWorkerEnv: "req-1"}, "linux", "amd64", BackendCUDA, ""},
		{"explicit cuda", map[string]string{BackendEnv: "CUDA"}, "linux", "amd64", BackendCUDA, ""},
		{"explicit cpu wins over marker", map[string]string{BackendEnv: "cpu", GPUWorkerEnv: "req-1"}, "linux", "amd64", BackendCPU, ""},
		{"cuda off linux/amd64 fails closed", map[string]string{BackendEnv: "cuda"}, "darwin", "arm64", "", "linux/amd64 only"},
		{"marker off linux/amd64 fails closed", map[string]string{GPUWorkerEnv: "req-1"}, "linux", "arm64", "", "linux/amd64 only"},
		{"unknown backend", map[string]string{BackendEnv: "rocm"}, "linux", "amd64", "", "not a backend"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := selectBackend(envOf(c.env), c.goos, c.arch)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want %q", err, c.wantErr)
				}
				return
			}
			if err != nil || got != c.want {
				t.Fatalf("got %q, %v; want %q", got, err, c.want)
			}
		})
	}
}

// fakeCUDARelease pins three fixture files, serves them from a fake binmgr
// fetcher, and verifies by sha256 + identity like the real verifier.
func fakeCUDARelease(t *testing.T, runner []byte) (root string, fetched *int) {
	t.Helper()
	oldRel, oldFetch, oldVerify, oldLd, oldRoot := cudaRelease, fetchCUDAAsset, verifyCUDAFile, hostLdconfig, cudaInstallRoot
	t.Cleanup(func() {
		cudaRelease, fetchCUDAAsset, verifyCUDAFile, hostLdconfig, cudaInstallRoot = oldRel, oldFetch, oldVerify, oldLd, oldRoot
	})
	const version = "bashy-v0.0.0-test"
	contents := map[string][]byte{
		"runner-linux-amd64-cuda":                   runner,
		"runner-linux-amd64-cuda-libggml-cuda.so":   []byte("libggml-cuda bytes"),
		"runner-linux-amd64-cuda-libggml-base.so.0": []byte("libggml-base bytes"),
	}
	rel := cudaReleaseMetadata{
		version:  version,
		identity: "https://github.com/qiangli/ollama/.github/workflows/bashy-build.yml@refs/tags/" + version,
		baseURL:  "https://example.invalid/" + version + "/",
	}
	for _, f := range cudaRelease.files {
		f.sha256 = sha256Hex(contents[f.asset])
		rel.files = append(rel.files, f)
	}
	cudaRelease = rel
	cache := t.TempDir()
	root = t.TempDir()
	cudaInstallRoot = func() (string, error) { return root, nil }
	n := 0
	fetched = &n
	fetchCUDAAsset = func(_ context.Context, tool binmgr.Tool) (string, error) {
		n++
		a := tool.Assets["linux/amd64"]
		if tool.Sigstore == nil || tool.Sigstore.Identity != rel.identity || tool.Version != version {
			return "", errors.New("fetch without the pinned Sigstore identity")
		}
		asset := strings.TrimPrefix(a.URL, rel.baseURL)
		data, ok := contents[asset]
		if !ok || sha256Hex(data) != a.SHA256 {
			return "", fmt.Errorf("unknown or mispinned asset %q", asset)
		}
		p := filepath.Join(cache, tool.Name)
		if err := os.WriteFile(p, data, 0o755); err != nil {
			return "", err
		}
		return p, os.WriteFile(p+".sigstore-bundle.json", []byte(`{"fixture":true}`), 0o644)
	}
	verifyCUDAFile = func(_ context.Context, p string, bundle []byte, pin string, s binmgr.Sigstore) error {
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if sha256Hex(data) != pin {
			return errors.New("wrong sha256")
		}
		if s.Identity != rel.identity || s.Issuer != binmgr.GitHubActionsIssuer || len(bundle) == 0 {
			return errors.New("wrong signer")
		}
		return nil
	}
	hostLdconfig = func() (string, error) { return fullLdconfig, nil }
	return root, fetched
}

func TestEnsureCUDAInstallsUpstreamLayoutAndReverifies(t *testing.T) {
	root, fetched := fakeCUDARelease(t, []byte("#!/bin/sh\n"))
	l, err := EnsureCUDA(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, cudaRelease.version)
	if l.Runner != filepath.Join(dir, "bin", runnerName) || l.LibDir != filepath.Join(dir, "lib", "ollama") {
		t.Fatalf("layout = %+v", l)
	}
	for _, name := range []string{"libggml-cuda.so", "libggml-base.so.0"} {
		info, err := os.Lstat(filepath.Join(l.LibDir, name))
		if err != nil || !info.Mode().IsRegular() {
			t.Fatalf("%s not installed as a regular file: %v", name, err)
		}
	}
	if *fetched != 3 {
		t.Fatalf("fetched %d assets, want 3", *fetched)
	}
	// A verified layout is reused without fetching.
	if _, err := EnsureCUDA(context.Background()); err != nil || *fetched != 3 {
		t.Fatalf("cache hit refetched (%d) or failed: %v", *fetched, err)
	}
	// A tampered library is never accepted: the layout is reinstalled.
	if err := os.WriteFile(filepath.Join(l.LibDir, "libggml-cuda.so"), []byte("tampered"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureCUDA(context.Background()); err != nil || *fetched != 6 {
		t.Fatalf("tampered layout not repaired (%d fetches): %v", *fetched, err)
	}
	got, _ := os.ReadFile(filepath.Join(l.LibDir, "libggml-cuda.so"))
	if string(got) != "libggml-cuda bytes" {
		t.Fatalf("libggml-cuda.so = %q", got)
	}
}

func TestEnsureCUDAWithoutPinsFailsClosed(t *testing.T) {
	_, fetched := fakeCUDARelease(t, []byte("runner"))
	cudaRelease.files[1].sha256 = ""
	_, err := EnsureCUDA(context.Background())
	if !errors.Is(err, ErrCUDAUnavailable) || !strings.Contains(err.Error(), "runner-linux-amd64-cuda-libggml-cuda.so") {
		t.Fatalf("err = %v", err)
	}
	if *fetched != 0 {
		t.Fatal("fetched without reviewed pins")
	}
	cudaRelease.files[1].sha256 = strings.Repeat("a", 64)
	cudaRelease.identity = "https://github.com/qiangli/ollama/.github/workflows/other.yml@refs/tags/" + cudaRelease.version
	if _, err := EnsureCUDA(context.Background()); !errors.Is(err, ErrCUDAUnavailable) {
		t.Fatalf("accepted a foreign workflow identity: %v", err)
	}
}

func TestCompiledCUDAPinsShape(t *testing.T) {
	r := cudaRelease
	if r.identity != "https://github.com/qiangli/ollama/.github/workflows/bashy-build.yml@refs/tags/"+r.version {
		t.Fatalf("identity %q is not the bashy-build tag workflow", r.identity)
	}
	if len(r.files) != 3 || r.files[0].rel != "bin/"+runnerName ||
		r.files[1].rel != "lib/ollama/libggml-cuda.so" || r.files[2].rel != "lib/ollama/libggml-base.so.0" {
		t.Fatalf("layout drifted from upstream's bin/ + lib/ollama/: %+v", r.files)
	}
}

// Operator red test: CUDA runner with libggml-base missing -> non-zero +
// message, and the runner never starts (so no CPU completion).
func TestDispatchCUDALibggmlBaseMissingFailsClosedBeforeStart(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	oldOS, oldArch := hostGOOS, hostGOARCH
	hostGOOS, hostGOARCH = "linux", "amd64"
	t.Cleanup(func() { hostGOOS, hostGOARCH = oldOS, oldArch })
	marker := filepath.Join(t.TempDir(), "ran")
	fakeCUDARelease(t, []byte("#!/bin/sh\ntouch "+marker+"\necho completion-on-cpu\n"))
	l, err := EnsureCUDA(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Drop libggml-base after install, and stop the repair path from
	// restoring it, as on a host where the release shipped without it.
	if err := os.Remove(filepath.Join(l.LibDir, "libggml-base.so.0")); err != nil {
		t.Fatal(err)
	}
	fetchCUDAAsset = func(context.Context, binmgr.Tool) (string, error) {
		return "", errors.New("release asset absent")
	}
	t.Setenv(BackendEnv, "cuda")
	var out, errb bytes.Buffer
	handled, code := DispatchCUDA(context.Background(), []string{"--port", "1"}, nil, &out, &errb)
	if !handled || code == 0 {
		t.Fatalf("handled=%v code=%d, want handled non-zero", handled, code)
	}
	if !strings.Contains(errb.String(), "libggml-base") {
		t.Fatalf("message does not name libggml-base: %q", errb.String())
	}
	if _, err := os.Stat(marker); err == nil || strings.Contains(out.String(), "completion") {
		t.Fatal("runner started without libggml-base")
	}
}

func TestPreflightCUDA(t *testing.T) {
	fakeCUDARelease(t, []byte("#!/bin/sh\n"))
	l, err := EnsureCUDA(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Run("libggml-base missing", func(t *testing.T) {
		base := filepath.Join(l.LibDir, "libggml-base.so.0")
		data, _ := os.ReadFile(base)
		os.Remove(base)
		defer os.WriteFile(base, data, 0o755)
		err := PreflightCUDA(l)
		if !errors.Is(err, ErrCUDAUnavailable) || !strings.Contains(err.Error(), "libggml-base.so.0 is missing next to") {
			t.Fatalf("err = %v", err)
		}
	})
	// Operator red test: host runtime missing names what is missing.
	t.Run("host runtime missing", func(t *testing.T) {
		hostLdconfig = func() (string, error) {
			return "\tlibcuda.so.1 (libc6,x86-64) => /usr/lib/libcuda.so.1\n", nil
		}
		err := PreflightCUDA(l)
		if !errors.Is(err, ErrCUDAUnavailable) || !strings.Contains(err.Error(), "libcudart.so libcublas.so") ||
			!strings.Contains(err.Error(), BackendEnv+"=cpu") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("no ldconfig", func(t *testing.T) {
		hostLdconfig = func() (string, error) { return "", errors.New("ldconfig not found (PATH, /usr/sbin, /sbin)") }
		if err := PreflightCUDA(l); !errors.Is(err, ErrCUDAUnavailable) || !strings.Contains(err.Error(), "ldconfig not found") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("ready", func(t *testing.T) {
		hostLdconfig = func() (string, error) { return fullLdconfig, nil }
		if err := PreflightCUDA(l); err != nil {
			t.Fatal(err)
		}
	})
}

func TestCUDALaunchEnvPointsLibrarySearchAtLayout(t *testing.T) {
	l := CUDALaunch{Runner: "/r/bin/ycode-runner", LibDir: "/r/lib/ollama"}
	env := l.Env([]string{"OLLAMA_LIBRARY_PATH=/usr/local/bin", "LD_LIBRARY_PATH=/usr/local/bin", "HOME=/h"})
	joined := strings.Join(env, "\n")
	for _, want := range []string{"OLLAMA_LIBRARY_PATH=/r/lib/ollama", "LD_LIBRARY_PATH=/r/lib/ollama" + string(filepath.ListSeparator) + "/usr/local/bin", "HOME=/h"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("env missing %q: %v", want, env)
		}
	}
	if strings.Contains(joined, "OLLAMA_LIBRARY_PATH=/usr/local/bin") {
		t.Fatalf("server's library path leaked through: %v", env)
	}
}

func TestCUDALogVerdict(t *testing.T) {
	for line, want := range map[string]string{
		"ggml_cuda_init: found 1 CUDA devices:\n": "ok",
		`time=2026-10-31T00:00:00Z level=INFO source=ggml.go:104 msg=system CPU.0.SSE3=1 CUDA.0.ARCHS=890 CUDA.0.USE_GRAPHS=1`: "ok",
		"ggml_cuda_init: found 0 CUDA devices:\n":                                         "bad",
		"ggml_cuda_init: failed to initialize CUDA: no CUDA-capable device is detected\n": "bad",
		`msg=system CPU.0.SSE3=1 CPU.0.AVX=1`:                                             "",
		"llama_model_loader: loaded meta data\n":                                          "",
	} {
		ok, bad := cudaLogVerdict(line)
		got := ""
		if ok {
			got = "ok"
		} else if bad != nil {
			got = "bad"
		}
		if got != want {
			t.Errorf("%q: got %q want %q", line, got, want)
		}
	}
}

func writeScript(t *testing.T, body string) CUDALaunch {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "runner")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return CUDALaunch{Runner: p, LibDir: dir}
}

func TestRunCUDAProvenDeviceServes(t *testing.T) {
	l := writeScript(t, `echo "ggml_cuda_init: found 1 CUDA devices:" >&2
[ "$OLLAMA_LIBRARY_PATH" = "`+"$(dirname \"$0\")"+`" ] || { echo bad-libpath; exit 3; }
echo "served $1"
`)
	var out, errb bytes.Buffer
	if code := RunCUDA(context.Background(), l, []string{"x"}, nil, &out, &errb, 5*time.Second); code != 0 {
		t.Fatalf("code %d, out %q err %q", code, out.String(), errb.String())
	}
	if out.String() != "served x\n" || !strings.Contains(errb.String(), "found 1 CUDA devices") {
		t.Fatalf("output not relayed: %q / %q", out.String(), errb.String())
	}
}

// Operator red test: startup log without a CUDA device -> the runner is
// stopped and nothing it would have served on CPU reaches the caller.
func TestRunCUDAStartupLogWithoutDeviceFailsClosed(t *testing.T) {
	l := writeScript(t, `echo "load_backend: loaded CPU backend" >&2
sleep 2
echo completion-on-cpu
`)
	var out, errb bytes.Buffer
	start := time.Now()
	code := RunCUDA(context.Background(), l, nil, nil, &out, &errb, 300*time.Millisecond)
	if code == 0 {
		t.Fatal("CPU-backed runner passed the CUDA gate")
	}
	if time.Since(start) > 1500*time.Millisecond {
		t.Fatal("runner was not stopped at the end of the start window")
	}
	if strings.Contains(out.String(), "completion-on-cpu") {
		t.Fatal("served a CPU completion on a CUDA selection")
	}
	if !strings.Contains(errb.String(), "no CUDA device within") || !strings.Contains(errb.String(), "refusing to serve on CPU") {
		t.Fatalf("message = %q", errb.String())
	}
}

func TestRunCUDAZeroDevicesAndEarlyExitFailClosed(t *testing.T) {
	l := writeScript(t, `echo "ggml_cuda_init: found 0 CUDA devices:" >&2
sleep 2
echo completion-on-cpu
`)
	var out, errb bytes.Buffer
	if code := RunCUDA(context.Background(), l, nil, nil, &out, &errb, 5*time.Second); code == 0 ||
		strings.Contains(out.String(), "completion") || !strings.Contains(errb.String(), "found 0 CUDA devices") {
		t.Fatalf("code %d out %q err %q", code, out.String(), errb.String())
	}
	l = writeScript(t, "exit 0\n")
	errb.Reset()
	if code := RunCUDA(context.Background(), l, nil, nil, &out, &errb, 5*time.Second); code == 0 ||
		!strings.Contains(errb.String(), "exited before reporting a CUDA device") {
		t.Fatalf("early exit: code %d err %q", code, errb.String())
	}
}

func TestDispatchCUDANotSelectedIsNotHandled(t *testing.T) {
	t.Setenv(BackendEnv, "cpu")
	t.Setenv(GPUWorkerEnv, "req-1")
	if handled, _ := DispatchCUDA(context.Background(), nil, nil, &bytes.Buffer{}, &bytes.Buffer{}); handled {
		t.Fatal("explicit CPU selection was handled as CUDA")
	}
}

func TestCUDAStartWindow(t *testing.T) {
	if d := CUDAStartWindow(envOf(nil)); d != DefaultCUDAStartWindow {
		t.Fatal(d)
	}
	if d := CUDAStartWindow(envOf(map[string]string{CUDAStartWindowEnv: "90s"})); d != 90*time.Second {
		t.Fatal(d)
	}
}

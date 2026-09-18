package whycmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/qiangli/coreutils/tool"
	"github.com/qiangli/yoke/pkg/binmgr"
)

func setSeams(t *testing.T, r func(context.Context, *tool.RunContext) (string, error), e func(context.Context, string, []string, string, []string, io.Reader, io.Writer, io.Writer) (int, int, error)) {
	t.Helper()
	prevR, prevE := resolveBinFn, execCmdFn
	resolveBinFn, execCmdFn = r, e
	t.Cleanup(func() {
		resolveBinFn, execCmdFn = prevR, prevE
	})
}

func TestWhy_HelpUsageAndSynopsis(t *testing.T) {
	if cmd.Name != "why" {
		t.Errorf("cmd.Name = %q, want %q", cmd.Name, "why")
	}
	if !strings.Contains(cmd.Synopsis, "witr v0.3.3") {
		t.Errorf("cmd.Synopsis does not mention witr v0.3.3: %q", cmd.Synopsis)
	}
	for _, expected := range []string{"why nginx", "--port 8080", "--pid 1234", "--file", "--container", "--json", "--tree"} {
		if !strings.Contains(cmd.Usage, expected) {
			t.Errorf("cmd.Usage missing example %q", expected)
		}
	}
}

func TestWhy_PassExactArgvAndFlags(t *testing.T) {
	var capturedBin string
	var capturedArgs []string

	setSeams(t,
		func(ctx context.Context, rc *tool.RunContext) (string, error) {
			return "/bin/fake-witr", nil
		},
		func(ctx context.Context, binPath string, args []string, dir string, env []string, stdin io.Reader, stdout, stderr io.Writer) (int, int, error) {
			capturedBin = binPath
			capturedArgs = append([]string{}, args...)
			return 0, 0, nil
		},
	)

	rc := &tool.RunContext{}
	testArgs := []string{"--pid", "1234", "--port", "8080", "--file", "/tmp/app.log", "--container", "c1", "--json", "--tree", "nginx"}
	code := run(rc, testArgs)
	if code != 0 {
		t.Fatalf("run returned %d, want 0", code)
	}
	if capturedBin != "/bin/fake-witr" {
		t.Errorf("capturedBin = %q, want %q", capturedBin, "/bin/fake-witr")
	}
	if len(capturedArgs) != len(testArgs) {
		t.Fatalf("capturedArgs len = %d, want %d", len(capturedArgs), len(testArgs))
	}
	for i, arg := range testArgs {
		if capturedArgs[i] != arg {
			t.Errorf("capturedArgs[%d] = %q, want %q", i, capturedArgs[i], arg)
		}
	}
}

func TestWhy_PreserveCwdEnvAndStdio(t *testing.T) {
	var capturedDir string
	var capturedEnv []string
	var capturedStdin string

	setSeams(t,
		func(ctx context.Context, rc *tool.RunContext) (string, error) {
			return "/bin/fake-witr", nil
		},
		func(ctx context.Context, binPath string, args []string, dir string, env []string, stdin io.Reader, stdout, stderr io.Writer) (int, int, error) {
			capturedDir = dir
			capturedEnv = append([]string{}, env...)
			if stdin != nil {
				b, _ := io.ReadAll(stdin)
				capturedStdin = string(b)
			}
			if stdout != nil {
				_, _ = stdout.Write([]byte("out ok"))
			}
			if stderr != nil {
				_, _ = stderr.Write([]byte("err ok"))
			}
			return 0, 0, nil
		},
	)

	outBuf := &bytes.Buffer{}
	errBuf := &bytes.Buffer{}
	rc := &tool.RunContext{
		Dir:   "/custom/dir",
		Env:   []string{"FOO=BAR", "BAZ=QUX"},
		Stdio: tool.Stdio{In: strings.NewReader("input data"), Out: outBuf, Err: errBuf},
	}

	code := run(rc, []string{"nginx"})
	if code != 0 {
		t.Fatalf("run returned %d, want 0", code)
	}
	if capturedDir != "/custom/dir" {
		t.Errorf("capturedDir = %q, want %q", capturedDir, "/custom/dir")
	}
	if len(capturedEnv) != 2 || capturedEnv[0] != "FOO=BAR" || capturedEnv[1] != "BAZ=QUX" {
		t.Errorf("capturedEnv = %v, want [FOO=BAR BAZ=QUX]", capturedEnv)
	}
	if capturedStdin != "input data" {
		t.Errorf("capturedStdin = %q, want %q", capturedStdin, "input data")
	}
	if outBuf.String() != "out ok" {
		t.Errorf("stdout = %q, want %q", outBuf.String(), "out ok")
	}
	if errBuf.String() != "err ok" {
		t.Errorf("stderr = %q, want %q", errBuf.String(), "err ok")
	}
}

func TestWhy_ExplicitEmptyEnv(t *testing.T) {
	var capturedEnv []string

	setSeams(t,
		func(ctx context.Context, rc *tool.RunContext) (string, error) {
			return "/bin/fake-witr", nil
		},
		func(ctx context.Context, binPath string, args []string, dir string, env []string, stdin io.Reader, stdout, stderr io.Writer) (int, int, error) {
			capturedEnv = env
			return 0, 0, nil
		},
	)

	// nil rc.Env represents explicit empty env
	rc := &tool.RunContext{Env: nil}
	code := run(rc, []string{"nginx"})
	if code != 0 {
		t.Fatalf("run returned %d, want 0", code)
	}
	if capturedEnv == nil {
		t.Fatal("capturedEnv is nil, expected non-nil empty slice []string{}")
	}
	if len(capturedEnv) != 0 {
		t.Fatalf("capturedEnv len = %d, want 0", len(capturedEnv))
	}
}

func TestWhy_ExitStatusAndSignalPreservation(t *testing.T) {
	testCases := []struct {
		name       string
		retCode    int
		retSig     int
		retErr     error
		wantStatus int
		wantSig    int
	}{
		{"success 0", 0, 0, nil, 0, 0},
		{"failure 1", 1, 0, nil, 1, 0},
		{"usage error 2", 2, 0, nil, 2, 0},
		{"arbitrary status 42", 42, 0, nil, 42, 0},
		{"terminated by SIGTERM 15", 143, 15, nil, 143, 15},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			setSeams(t,
				func(ctx context.Context, rc *tool.RunContext) (string, error) {
					return "/bin/fake-witr", nil
				},
				func(ctx context.Context, binPath string, args []string, dir string, env []string, stdin io.Reader, stdout, stderr io.Writer) (int, int, error) {
					return tc.retCode, tc.retSig, tc.retErr
				},
			)
			rc := &tool.RunContext{}
			code := run(rc, []string{"nginx"})
			if code != tc.wantStatus {
				t.Errorf("status = %d, want %d", code, tc.wantStatus)
			}
			if rc.ExitSignal != tc.wantSig {
				t.Errorf("ExitSignal = %d, want %d", rc.ExitSignal, tc.wantSig)
			}
		})
	}
}

func TestWhy_DistinguishResolveVsStartErrors(t *testing.T) {
	// 1. Resolve Error
	errBuf1 := &bytes.Buffer{}
	setSeams(t,
		func(ctx context.Context, rc *tool.RunContext) (string, error) {
			return "", errors.New("resolution failed: network down")
		},
		func(ctx context.Context, binPath string, args []string, dir string, env []string, stdin io.Reader, stdout, stderr io.Writer) (int, int, error) {
			return 0, 0, nil
		},
	)
	rc1 := &tool.RunContext{Stdio: tool.Stdio{Err: errBuf1}}
	code1 := run(rc1, []string{"nginx"})
	if code1 != 1 {
		t.Errorf("resolve error code = %d, want 1", code1)
	}
	if !strings.Contains(errBuf1.String(), "why: resolution failed: network down") {
		t.Errorf("resolve error msg = %q, want substring %q", errBuf1.String(), "resolution failed")
	}

	// 2. Start Error
	errBuf2 := &bytes.Buffer{}
	setSeams(t,
		func(ctx context.Context, rc *tool.RunContext) (string, error) {
			return "/bin/fake-witr", nil
		},
		func(ctx context.Context, binPath string, args []string, dir string, env []string, stdin io.Reader, stdout, stderr io.Writer) (int, int, error) {
			return 126, 0, errors.New("permission denied")
		},
	)
	rc2 := &tool.RunContext{Stdio: tool.Stdio{Err: errBuf2}}
	code2 := run(rc2, []string{"nginx"})
	if code2 != 126 {
		t.Errorf("start error code = %d, want 126", code2)
	}
	if !strings.Contains(errBuf2.String(), "why: permission denied") {
		t.Errorf("start error msg = %q, want substring %q", errBuf2.String(), "permission denied")
	}
}

func TestWhy_NilSafeDiagnostics(t *testing.T) {
	setSeams(t,
		func(ctx context.Context, rc *tool.RunContext) (string, error) {
			return "", errors.New("resolution error")
		},
		func(ctx context.Context, binPath string, args []string, dir string, env []string, stdin io.Reader, stdout, stderr io.Writer) (int, int, error) {
			return 0, 0, nil
		},
	)

	// rc.Err is nil — must not panic!
	rc := &tool.RunContext{Stdio: tool.Stdio{Err: nil}}
	code := run(rc, []string{"nginx"})
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
}

func TestWhy_VersionOverrideFailClosed(t *testing.T) {
	testCases := []struct {
		name    string
		env     []string
		wantOk  bool
		wantMsg string
	}{
		{"valid pinned v0.3.3", []string{"WHY_VERSION=v0.3.3"}, true, ""},
		{"valid pinned 0.3.3", []string{"WITR_VERSION=0.3.3"}, true, ""},
		{"unpinned version v0.3.4", []string{"WHY_VERSION=v0.3.4"}, false, "unsupported version override"},
		{"latest override", []string{"WITR_VERSION=latest"}, false, "unsupported version override"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			rc := &tool.RunContext{Env: tc.env}
			err := validateVersionOverride(rc)
			if tc.wantOk {
				if err != nil {
					t.Fatalf("unexpected validation error: %v", err)
				}
			} else {
				if err == nil {
					t.Fatal("expected validation error, got nil")
				}
				if !strings.Contains(err.Error(), tc.wantMsg) {
					t.Errorf("err = %q, want substring %q", err.Error(), tc.wantMsg)
				}
			}
		})
	}
}

func TestWhy_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	var receivedCtx context.Context
	setSeams(t,
		func(ctx context.Context, rc *tool.RunContext) (string, error) {
			receivedCtx = ctx
			return "", ctx.Err()
		},
		func(ctx context.Context, binPath string, args []string, dir string, env []string, stdin io.Reader, stdout, stderr io.Writer) (int, int, error) {
			return 0, 0, nil
		},
	)

	rc := &tool.RunContext{Ctx: ctx}
	code := run(rc, []string{"nginx"})
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if receivedCtx == nil || receivedCtx.Err() == nil {
		t.Errorf("expected cancelled context, got %v", receivedCtx)
	}
}

func TestWhy_CacheBehaviorSkipsResolution(t *testing.T) {
	tmpCache := t.TempDir()
	t.Setenv("BASHY_BIN_CACHE", tmpCache)

	witrDir := filepath.Join(tmpCache, UpstreamTool, PinnedVersion)
	if err := os.MkdirAll(witrDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fakeBin := filepath.Join(witrDir, binmgr.BinaryName(UpstreamTool))
	if err := os.WriteFile(fakeBin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	rc := &tool.RunContext{}
	resolved, err := defaultResolveBin(context.Background(), rc)
	if err != nil {
		t.Fatalf("defaultResolveBin failed on cache hit: %v", err)
	}
	if resolved != fakeBin {
		t.Errorf("resolved = %q, want %q", resolved, fakeBin)
	}
}

func TestWhy_PinnedTuples(t *testing.T) {
	expectedPins := map[string]string{
		"darwin/amd64":  "39934f6a8d6a0413c52324ccdbd3a0867371785b6c066005ea063a78279487ef",
		"darwin/arm64":  "d05b51825604d608da8757e549a1f5322549a350f8336c593429f3f2cd507927",
		"linux/amd64":   "08fc46e3f80a374476f71d0d6e6579477cd98c6df5cc59d98224adf948f5ebf5",
		"linux/arm64":   "dca2be6cf56a5274de0a036b83345d055b8e94f7f4fd23dc54dd102d7669e2d8",
		"windows/amd64": "1ae95a354fa7f767828ad7942497f3801e5299f8afad5844ec6d1819703a6b28",
		"windows/arm64": "e644a1e152437a0aff93c672660b363de690361ca90f35a792f88b361ca569e4",
		"freebsd/amd64": "0fcc966fc8adbdf901174c96901e15f4b202e8925bc2ce6d20daf11b9f12305c",
		"freebsd/arm64": "41ec530a07062797d3143a286c2d094643a3c4b7f1c2171bee81e6e0345b17f1",
	}

	for plat, wantSHA := range expectedPins {
		toolName, version := "witr", "v0.3.3"
		sha, ok := binmgr.PinnedSHA256(toolName, version, plat)
		if !ok {
			t.Errorf("pin missing for %s@%s/%s", toolName, version, plat)
		}
		if sha != wantSHA {
			t.Errorf("pin for %s@%s/%s = %q, want %q", toolName, version, plat, sha, wantSHA)
		}
	}
}

func TestWhy_DefaultExecCmd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping Unix exec test on Windows")
	}

	outBuf := &bytes.Buffer{}
	errBuf := &bytes.Buffer{}
	code, sig, err := defaultExecCmd(context.Background(), "/bin/sh", []string{"-c", "echo hello"}, t.TempDir(), []string{"ENV_VAR=1"}, nil, outBuf, errBuf)
	if err != nil {
		t.Fatalf("defaultExecCmd failed: %v", err)
	}
	if code != 0 {
		t.Errorf("code = %d, want 0", code)
	}
	if sig != 0 {
		t.Errorf("sig = %d, want 0", sig)
	}
	if strings.TrimSpace(outBuf.String()) != "hello" {
		t.Errorf("outBuf = %q, want %q", outBuf.String(), "hello")
	}
}

func TestWhy_DefaultExecCmd_NonZeroExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping Unix exec test on Windows")
	}

	code, _, err := defaultExecCmd(context.Background(), "/bin/sh", []string{"-c", "exit 42"}, t.TempDir(), nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("defaultExecCmd failed: %v", err)
	}
	if code != 42 {
		t.Errorf("code = %d, want 42", code)
	}
}

func TestWhy_DefaultExecCmd_StartError(t *testing.T) {
	code, _, err := defaultExecCmd(context.Background(), "/nonexistent_path_witr_test_12345", nil, "", nil, nil, nil, nil)
	if err == nil {
		t.Fatal("expected error for nonexistent binary, got nil")
	}
	if code != 126 {
		t.Errorf("code = %d, want 126 for start error", code)
	}
}

func TestWhy_HermeticAssetMatch(t *testing.T) {
	linuxSpec := buildSpec("linux", "amd64")
	if linuxSpec.AssetMatch("witr-0.3.3-linux-amd64.apk", "linux", "amd64") {
		t.Error("Linux matched .apk instead of raw binary")
	}
	if linuxSpec.AssetMatch("witr-0.3.3-linux-amd64.deb", "linux", "amd64") {
		t.Error("Linux matched .deb instead of raw binary")
	}
	if !linuxSpec.AssetMatch("witr-linux-amd64", "linux", "amd64") {
		t.Error("Linux failed to match raw binary")
	}

	windowsSpec := buildSpec("windows", "amd64")
	if windowsSpec.Member != "witr.exe" {
		t.Errorf("Windows member = %q, want witr.exe", windowsSpec.Member)
	}
	if !windowsSpec.AssetMatch("witr-windows-amd64.zip", "windows", "amd64") {
		t.Error("Windows failed to match .zip")
	}
}

func TestWhy_UnsupportedPlatform(t *testing.T) {
	// Backup and restore goos/goarch
	oldGoos, oldGoarch := goos, goarch
	goos, goarch = "plan9", "amd64"
	defer func() {
		goos, goarch = oldGoos, oldGoarch
	}()

	// The early check must fail with the specific error before any cache/network calls
	rc := &tool.RunContext{}
	_, err := defaultResolveBin(context.Background(), rc)
	if err == nil {
		t.Fatal("expected error on unsupported platform plan9/amd64, got nil")
	}
	expectedErr := "unsupported platform plan9/amd64: no pinned digest for witr@v0.3.3"
	if !strings.Contains(err.Error(), expectedErr) {
		t.Errorf("expected error %q, got: %q", expectedErr, err.Error())
	}
}

func TestWhy_InstallChecksumMismatch(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("bad binary content"))
	}))
	defer ts.Close()

	toolObj := binmgr.Tool{
		Name:    "witr",
		Version: "v0.3.3",
		Assets: map[string]binmgr.Asset{
			binmgr.Platform(): {URL: ts.URL, SHA256: "0000000000000000000000000000000000000000000000000000000000000000"},
		},
	}

	t.Setenv("BASHY_BIN_CACHE", t.TempDir())
	_, err := binmgr.Ensure(context.Background(), toolObj)
	if err == nil {
		t.Fatal("expected error for checksum mismatch")
	}
	if !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Errorf("expected sha256 mismatch error, got: %v", err)
	}
}

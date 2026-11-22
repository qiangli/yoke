package gotoolchain

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestPinGOROOTReplacesForeignGOROOT is the 54a667814e3c regression: a child
// of the managed toolchain must see exactly one GOROOT, the managed one, even
// when the parent env carries a foreign GOROOT (as `go test` exports).
func TestPinGOROOTReplacesForeignGOROOT(t *testing.T) {
	env := []string{"PATH=/usr/bin", "GOROOT=/foreign/go1.27.2", "HOME=/home/x"}
	got := PinGOROOT(env, "/managed/go1.27.4")
	var roots []string
	for _, kv := range got {
		if strings.HasPrefix(kv, "GOROOT=") {
			roots = append(roots, kv)
		}
	}
	if len(roots) != 1 || roots[0] != "GOROOT=/managed/go1.27.4" {
		t.Fatalf("PinGOROOT left GOROOT entries %v, want exactly [GOROOT=/managed/go1.27.4] (full env %v)", roots, got)
	}
	if len(got) != len(env) {
		t.Fatalf("PinGOROOT changed env size: got %v, want %v entries like %v", got, len(env), env)
	}
	// Empty goroot is a no-op, and the input is not mutated.
	if out := PinGOROOT(env, ""); len(out) != len(env) || out[1] != "GOROOT=/foreign/go1.27.2" {
		t.Fatalf("PinGOROOT(empty) = %v, want unchanged %v", out, env)
	}
	if env[1] != "GOROOT=/foreign/go1.27.2" {
		t.Fatalf("PinGOROOT mutated its input: %v", env)
	}
}

// TestDefaultVersionMatchesProductToolchain pins the stale-pin regression of
// story 389edc3a9dc8: the provisioned default must be the version the
// official build image pre-seeds, not an older patch.
func TestDefaultVersionMatchesProductToolchain(t *testing.T) {
	if DefaultVersion != "1.27.4" {
		t.Fatalf("DefaultVersion = %q, want 1.27.4 (the product go.mod toolchain / pre-seeded image version)", DefaultVersion)
	}
}

// TestEnsureEmptyVersionHonorsEnvVersion pins that an empty version request
// follows $BASHY_GO_VERSION (the front door's selection) before the default:
// a cache staged only under the env-named version is hit with no network.
func TestEnsureEmptyVersionHonorsEnvVersion(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go executable unavailable")
	}
	out, err := exec.Command(goBin, "env", "GOROOT").Output()
	if err != nil {
		t.Fatalf("go env GOROOT: %v", err)
	}
	realGoroot := strings.TrimSpace(string(out))
	const ver = "99.0.0-envtest"
	binCache := t.TempDir()
	stage := filepath.Join(binCache, "go", ver)
	if err := os.MkdirAll(stage, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realGoroot, filepath.Join(stage, "go")); err != nil {
		t.Skipf("symlink GOROOT unavailable: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stage, ".binmgr-tree-complete"), []byte("ok\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BASHY_BIN_CACHE", binCache)
	t.Setenv("BASHY_GO_VERSION", ver)
	gotBin, gotRoot, err := Ensure(context.Background(), "")
	if err != nil {
		t.Fatalf("Ensure(\"\") with BASHY_GO_VERSION=%s: %v", ver, err)
	}
	if want := filepath.Join(stage, "go", "bin", "go"); gotBin != want && filepath.Base(gotBin) != "go" {
		t.Fatalf("Ensure(\"\") goBin = %q, want under the env-version stage %q", gotBin, want)
	}
	if gotRoot == "" {
		t.Fatalf("Ensure(\"\") goroot empty (goBin %q)", gotBin)
	}
}

func TestWindowsNativePathForGOOS(t *testing.T) {
	tests := []struct {
		name string
		goos string
		in   string
		want string
	}{
		{"msys temp", "windows", "/c/Users/alice/AppData/Local/Temp", `C:\Users\alice\AppData\Local\Temp`},
		{"shellified go cache", "windows", `\c\Users\alice\go\pkg\mod`, `C:\Users\alice\go\pkg\mod`},
		{"drive path", "windows", `C:\Users\alice\AppData\Local\Temp`, `C:\Users\alice\AppData\Local\Temp`},
		{"other os unchanged", "linux", "/c/Users/alice/AppData/Local/Temp", "/c/Users/alice/AppData/Local/Temp"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := windowsNativePathForGOOS(tt.goos, tt.in); got != tt.want {
				t.Fatalf("windowsNativePathForGOOS(%q, %q) = %q, want %q", tt.goos, tt.in, got, tt.want)
			}
		})
	}
}

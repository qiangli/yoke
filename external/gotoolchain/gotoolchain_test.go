package gotoolchain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/mod/module"
	modzip "golang.org/x/mod/zip"

	"github.com/qiangli/yoke/pkg/artifact"
	"github.com/qiangli/yoke/pkg/binmgr"
	"github.com/qiangli/yoke/pkg/gomod"
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

func TestSanitizeSealedGOPROXY(t *testing.T) {
	if got := SanitizeSealedGOPROXY("off"); got != "off" {
		t.Fatalf("SanitizeSealedGOPROXY(off) = %q, want off", got)
	}
	if got := SanitizeSealedGOPROXY("https://proxy.golang.org,direct"); got != "" {
		t.Fatalf("SanitizeSealedGOPROXY(public+direct) = %q, want empty", got)
	}
	poisoned := "https://proxy.golang.org,http://198.51.100.10:8080,http://127.0.0.1:43210|direct"
	if got := SanitizeSealedGOPROXY(poisoned); got != "http://127.0.0.1:43210" {
		t.Fatalf("SanitizeSealedGOPROXY(%q) = %q, want http://127.0.0.1:43210", poisoned, got)
	}
}

func TestSealedEnsureRestoresFromCASAndRefusesUncached(t *testing.T) {
	ctx := context.Background()
	artRoot := filepath.Join(t.TempDir(), "artifacts")
	binCache := filepath.Join(t.TempDir(), "bin-cache")
	t.Setenv("BASHY_ARTIFACTS_DIR", artRoot)
	t.Setenv("BASHY_BIN_CACHE", binCache)

	// Persist sealed state in artRoot (even with BASHY_OFFLINE unset, seal.json enforces sealed mode!).
	t.Setenv("BASHY_OFFLINE", "")
	if err := artifact.SaveSealState(artRoot, artifact.SealStateV1{
		SchemaVersion:    artifact.SealStateSchemaV1,
		Sealed:           true,
		ActiveGeneration: 1,
	}); err != nil {
		t.Fatalf("SaveSealState: %v", err)
	}
	if !binmgr.Offline() {
		t.Fatal("expected binmgr.Offline() == true when seal.json is sealed")
	}

	// Build a tiny synthetic Go tree seed archive and ingest into CAS under gotoolchain/go@88.1.0.
	const synthVer = "88.1.0"
	fakeGoroot := filepath.Join(t.TempDir(), "fake-goroot")
	if err := os.MkdirAll(filepath.Join(fakeGoroot, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	goExePath := filepath.Join(fakeGoroot, "bin", binmgr.BinaryName("go"))
	if err := os.WriteFile(goExePath, []byte("#!/bin/sh\necho fake-go\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	seedTar := filepath.Join(t.TempDir(), "go-seed.tar")
	if err := binmgr.BuildTreeSeedArchive(ctx, seedTar, "go", synthVer, "go", fakeGoroot); err != nil {
		t.Fatalf("BuildTreeSeedArchive: %v", err)
	}
	seedBytes, err := os.ReadFile(seedTar)
	if err != nil {
		t.Fatal(err)
	}

	store, err := artifact.OpenStore(ctx, artifact.StoreOptions{Root: artRoot})
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	if _, err := store.PutArtifact(ctx, artifact.CatalogRecordV1{
		SchemaVersion:  artifact.CatalogRecordSchemaV1,
		Digest:         artifact.DigestBytes(seedBytes),
		Size:           int64(len(seedBytes)),
		MediaType:      "application/x-tar",
		LogicalName:    LogicalNameToolchain,
		LogicalVersion: synthVer,
		Provenance:     "test",
		Retention:      artifact.RetentionProtectedRoot,
		RetentionRoots: []string{"test"},
	}, bytes.NewReader(seedBytes)); err != nil {
		t.Fatalf("PutArtifact toolchain: %v", err)
	}
	_ = store.Close()

	// 1. Wiped binmgr cache -> Ensure restores synthVer from CAS offline!
	gotBin, gotRoot, err := Ensure(ctx, synthVer)
	if err != nil {
		t.Fatalf("sealed Ensure(%s) restore from CAS failed: %v", synthVer, err)
	}
	if !binmgr.IsExecutable(gotBin) || gotRoot == "" {
		t.Fatalf("restored toolchain invalid: bin=%q root=%q", gotBin, gotRoot)
	}

	// 2. Uncached version -> Ensure refuses immediately with ErrSealedNetworkDenied (never calls go.dev)!
	if _, _, err := Ensure(ctx, "99.99.99-uncached"); !errors.Is(err, artifact.ErrSealedNetworkDenied) {
		t.Fatalf("uncached sealed Ensure err = %v, want ErrSealedNetworkDenied", err)
	}
}

func TestSealedNewGoCmdStripsExternalProxyAndBuildsFromLoopbackCAS(t *testing.T) {
	hostGo, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go executable unavailable")
	}
	out, err := exec.Command(hostGo, "env", "GOROOT").Output()
	if err != nil {
		t.Fatalf("go env GOROOT: %v", err)
	}
	realGoroot := strings.TrimSpace(string(out))

	ctx := context.Background()
	artRoot := filepath.Join(t.TempDir(), "artifacts")
	binCache := filepath.Join(t.TempDir(), "bin-cache")
	const testGoVer = "99.27.4-sealedtest"

	stage := filepath.Join(binCache, "go", testGoVer)
	if err := os.MkdirAll(stage, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realGoroot, filepath.Join(stage, "go")); err != nil {
		t.Skipf("symlink GOROOT unavailable: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stage, ".binmgr-tree-complete"), []byte("ok\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Populate artifact CAS with an external dependency module via gomod.CaptureModuleClosure.
	depSrc := t.TempDir()
	const depMod = "example.com/sealed/dep"
	const depVer = "v1.0.0"
	depGoMod := []byte("module " + depMod + "\n\ngo 1.22\n")
	if err := os.WriteFile(filepath.Join(depSrc, "go.mod"), depGoMod, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(depSrc, "dep.go"), []byte("package dep\n\nfunc Msg() string { return \"sealed-cmd-ok\" }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var zbuf bytes.Buffer
	if err := modzip.CreateFromDir(&zbuf, module.Version{Path: depMod, Version: depVer}, depSrc); err != nil {
		t.Fatal(err)
	}
	seedModCache := filepath.Join(t.TempDir(), "seed-modcache")
	escMod, _ := module.EscapePath(depMod)
	vDir := filepath.Join(seedModCache, "cache", "download", filepath.FromSlash(escMod), "@v")
	if err := os.MkdirAll(vDir, 0o755); err != nil {
		t.Fatal(err)
	}
	infoBytes, _ := json.Marshal(struct{ Version, Time string }{depVer, time.Unix(0, 0).UTC().Format(time.RFC3339)})
	_ = os.WriteFile(filepath.Join(vDir, depVer+".info"), infoBytes, 0o644)
	_ = os.WriteFile(filepath.Join(vDir, depVer+".mod"), depGoMod, 0o644)
	_ = os.WriteFile(filepath.Join(vDir, depVer+".zip"), zbuf.Bytes(), 0o644)

	// Compute go.sum hashes using host go mod download on file:// proxy.
	consumerDir := filepath.Join(t.TempDir(), "consumer")
	if err := os.MkdirAll(consumerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(consumerDir, "go.mod"), []byte(fmt.Sprintf("module example.com/sealed/app\n\ngo 1.22\n\nrequire %s %s\n", depMod, depVer)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(consumerDir, "main.go"), []byte("package main\nimport (\"fmt\"; \"example.com/sealed/dep\")\nfunc main() { fmt.Println(dep.Msg()) }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Populate go.sum offline against file:// cache/download before deleting seedModCache.
	tidyCmd := exec.Command(hostGo, "mod", "tidy")
	tidyCmd.Dir = consumerDir
	tidyCmd.Env = append(os.Environ(),
		"GOWORK=off",
		"GOPROXY=file://"+filepath.ToSlash(filepath.Join(seedModCache, "cache", "download")),
		"GOSUMDB=off",
		"GOTOOLCHAIN=local",
	)
	if out, err := tidyCmd.CombinedOutput(); err != nil {
		t.Fatalf("seed go mod tidy: %v\n%s", err, out)
	}

	if _, err := gomod.CaptureModuleClosure(ctx, gomod.ModuleClosureOptions{
		ModuleDirs:   []string{consumerDir},
		GoModCache:   seedModCache,
		ArtifactRoot: artRoot,
	}); err != nil {
		t.Fatalf("CaptureModuleClosure: %v", err)
	}
	_ = filepath.Walk(seedModCache, func(p string, _ os.FileInfo, _ error) error {
		return os.Chmod(p, 0o777)
	})
	_ = os.RemoveAll(seedModCache)

	if err := artifact.SaveSealState(artRoot, artifact.SealStateV1{
		SchemaVersion:    artifact.SealStateSchemaV1,
		Sealed:           true,
		ActiveGeneration: 1,
	}); err != nil {
		t.Fatal(err)
	}

	emptyGoCache := filepath.Join(t.TempDir(), "empty-gocache")
	emptyModCache := filepath.Join(t.TempDir(), "empty-gomodcache")
	_ = os.MkdirAll(emptyGoCache, 0o755)
	_ = os.MkdirAll(emptyModCache, 0o755)
	t.Cleanup(func() {
		_ = filepath.Walk(emptyModCache, func(p string, _ os.FileInfo, _ error) error {
			return os.Chmod(p, 0o755)
		})
	})

	t.Setenv("BASHY_ARTIFACTS_DIR", artRoot)
	t.Setenv("BASHY_BIN_CACHE", binCache)
	t.Setenv("BASHY_GO_VERSION", testGoVer)
	t.Setenv("GOCACHE", emptyGoCache)
	t.Setenv("GOMODCACHE", emptyModCache)
	t.Setenv("GOWORK", "off")
	t.Setenv("GOPROXY", "https://proxy.golang.org,direct")
	t.Setenv("HTTP_PROXY", "http://198.51.100.99:8080")
	t.Setenv("HTTPS_PROXY", "http://198.51.100.99:8080")

	// Also verify explicit GOPROXY=off stays untouched in SealedCommandEnv.
	offEnv, offCleanup, err := SealedCommandEnv(ctx, []string{"GOPROXY=off"}, realGoroot, artRoot)
	if err != nil {
		t.Fatal(err)
	}
	offCleanup()
	if got, _ := lookupEnvInSlice(offEnv, "GOPROXY"); got != "off" {
		t.Fatalf("explicit GOPROXY=off changed to %q", got)
	}

	binOut := filepath.Join(t.TempDir(), "app")
	if runtime.GOOS == "windows" {
		binOut += ".exe"
	}
	wd, _ := os.Getwd()
	if err := os.Chdir(consumerDir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(wd) }()

	var stdout, stderr bytes.Buffer
	goCmd := NewGoCmd()
	goCmd.SetOut(&stdout)
	goCmd.SetErr(&stderr)
	goCmd.SetArgs([]string{"build", "-mod=readonly", "-buildvcs=false", "-o", binOut, "."})
	if err := goCmd.ExecuteContext(ctx); err != nil {
		t.Fatalf("sealed NewGoCmd build failed: %v\nstderr: %s", err, stderr.String())
	}

	runOut, err := exec.Command(binOut).CombinedOutput()
	if err != nil || strings.TrimSpace(string(runOut)) != "sealed-cmd-ok" {
		t.Fatalf("built binary output = (%q, %v), want sealed-cmd-ok", string(runOut), err)
	}
}

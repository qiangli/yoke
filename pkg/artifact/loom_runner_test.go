// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package artifact_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	ygit "github.com/qiangli/yoke/git"
	"github.com/qiangli/yoke/pkg/artifact"
	"github.com/qiangli/yoke/pkg/gomod"
)

func TestWorkflowHelpersCaptureWipeAndMaterializePreservesExecutableBits(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	artRoot := filepath.Join(tmp, "artifacts")

	store, err := artifact.OpenStore(ctx, artifact.StoreOptions{Root: artRoot})
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	// Create synthetic workflow actions + bashy-build scripts + image.lock + commits.txt.
	srcHelperDir := filepath.Join(tmp, "helpers-src")
	actionsDir := filepath.Join(srcHelperDir, ".gitea", "actions", "verify-gate")
	buildScriptsDir := filepath.Join(srcHelperDir, "tools", "bashy-build")
	runnerBinDir := filepath.Join(srcHelperDir, "runner-hooks")

	for _, d := range []string{actionsDir, buildScriptsDir, runnerBinDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	actionYAML := []byte("name: verify-gate\nruns:\n  using: composite\n  steps:\n    - run: ./run-gate.sh\n      shell: sh\n")
	actionScript := []byte("#!/bin/sh\nset -eu\necho gate-ok\n")
	runnerScript := []byte("#!/bin/sh\nset -eu\necho custom-runner-ready\n")
	lockContent := []byte(strings.Join([]string{
		"bashy_version=v1.0.0-rc.40",
		"apt_snapshot=20260821T190000Z",
		"go_version=1.27.4",
		"go_sha256_amd64=" + strings.Repeat("a", 64),
		"go_sha256_arm64=" + strings.Repeat("b", 64),
		"base_image=localhost/bashy-base:v1.0.0-rc.40",
		"base_digest=sha256:" + strings.Repeat("c", 64),
		"image=localhost/bashy-build:v1.0.0-rc.40-go1.27.4-111111111111",
		"image_digest=sha256:" + strings.Repeat("d", 64),
		"source_bashy_commit=c379daea083e6aa7b05fe06135099ac0148c384a",
		"",
	}, "\n"))
	commitsContent := []byte("bashy_commit=c379daea083e6aa7b05fe06135099ac0148c384a\nyoke_commit=b7ca202e247eb7ca202e247eb7ca202e247eb7ca\n")

	if err := os.WriteFile(filepath.Join(actionsDir, "action.yml"), actionYAML, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(actionsDir, "run-gate.sh"), actionScript, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runnerBinDir, "dispatch.sh"), runnerScript, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(buildScriptsDir, "image.lock"), lockContent, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcHelperDir, "commits.txt"), commitsContent, 0o644); err != nil {
		t.Fatal(err)
	}

	capRes, err := artifact.CaptureWorkflowHelpers(ctx, store, artifact.WorkflowHelperCaptureOptions{
		Name:      "gate-runner-helpers",
		Version:   "v1.0.0",
		SourceDir: srcHelperDir,
	})
	if err != nil {
		t.Fatalf("CaptureWorkflowHelpers: %v", err)
	}
	if capRes.Root.Kind != artifact.WorkflowHelperKind || capRes.Root.MediaType != artifact.MediaTypeWorkflowHelper {
		t.Fatalf("unexpected root descriptor: %+v", capRes.Root)
	}
	if capRes.Metadata.BuildDigest != "sha256:"+strings.Repeat("d", 64) {
		t.Fatalf("BuildDigest = %q", capRes.Metadata.BuildDigest)
	}
	if capRes.Metadata.SourceCommits["yoke"] != "b7ca202e247eb7ca202e247eb7ca202e247eb7ca" {
		t.Fatalf("SourceCommits = %+v", capRes.Metadata.SourceCommits)
	}

	// Verify RetentionProtectedRoot blocks GC eviction!
	rec, err := store.GetRecord(ctx, capRes.Digest)
	if err != nil {
		t.Fatalf("GetRecord: %v", err)
	}
	if canEvict, _ := artifact.CanEvict(rec, nil, nil, nil, 1, time.Now(), nil); canEvict {
		t.Fatal("protected workflow-helper record must NEVER be evictable")
	}

	// Completely wipe the original source directory!
	if err := os.RemoveAll(srcHelperDir); err != nil {
		t.Fatal(err)
	}

	// Materialize into a brand-new runner workspace directory with BASHY_OFFLINE=1.
	t.Setenv("BASHY_OFFLINE", "1")
	restoreDir := filepath.Join(tmp, "restored-workspace")
	matRes, err := artifact.MaterializeWorkflowHelpers(ctx, store, artifact.WorkflowHelperMaterializeOptions{
		Name:    "gate-runner-helpers",
		Version: "v1.0.0",
		DestDir: restoreDir,
	})
	if err != nil {
		t.Fatalf("MaterializeWorkflowHelpers: %v", err)
	}
	if !matRes.VerifiedArchive || matRes.FileCount != 5 {
		t.Fatalf("unexpected materialize result: %+v", matRes)
	}

	// Assert byte-for-byte restoration AND executable vs non-executable bits.
	checkRestored := func(rel string, want []byte, wantExec bool) {
		t.Helper()
		p := filepath.Join(restoreDir, filepath.FromSlash(rel))
		got, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read restored %s: %v", rel, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("restored %s content mismatch", rel)
		}
		if runtime.GOOS != "windows" {
			fi, err := os.Stat(p)
			if err != nil {
				t.Fatal(err)
			}
			isExec := fi.Mode().Perm()&0o111 != 0
			if isExec != wantExec {
				t.Fatalf("%s executable=%t (mode %o), want %t", rel, isExec, fi.Mode().Perm(), wantExec)
			}
		}
	}

	checkRestored(".gitea/actions/verify-gate/action.yml", actionYAML, false)
	checkRestored(".gitea/actions/verify-gate/run-gate.sh", actionScript, true)
	checkRestored("runner-hooks/dispatch.sh", runnerScript, true)
	checkRestored("tools/bashy-build/image.lock", lockContent, false)
	checkRestored("commits.txt", commitsContent, false)

	// Negative check: wrong expected digest pin fails closed without writing to target dir.
	badDest := filepath.Join(tmp, "should-not-exist")
	_, err = artifact.MaterializeWorkflowHelpers(ctx, store, artifact.WorkflowHelperMaterializeOptions{
		Name:    "gate-runner-helpers",
		Version: "v1.0.0",
		Digest:  "sha256:" + strings.Repeat("f", 64),
		DestDir: badDest,
	})
	if !errors.Is(err, artifact.ErrCorruptPayload) {
		t.Fatalf("expected ErrCorruptPayload on wrong digest pin, got: %v", err)
	}
	if _, statErr := os.Stat(badDest); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("corrupt digest pin created destination directory: %v", statErr)
	}
}

const selfHostMiniDriverSource = `package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"example.com/bashy/publishedsib"
)

var (
	seedGeneration = "candidate"
	nondetStamp    = "fixed"
)

func main() {
	if len(os.Args) >= 2 && os.Args[1] == "--version" {
		fmt.Printf("bashy-mini-selfhost gen=%s stamp=%s sib=%s\n", seedGeneration, nondetStamp, publishedsib.Banner())
		return
	}
	if len(os.Args) >= 2 && os.Args[1] == "go" {
		if os.Getenv("BASHY_OFFLINE") != "1" {
			fmt.Fprintln(os.Stderr, "expected BASHY_OFFLINE=1")
			os.Exit(2)
		}
		if os.Getenv("GOTOOLCHAIN") != "local" || os.Getenv("GOSUMDB") != "off" {
			fmt.Fprintf(os.Stderr, "unexpected GOTOOLCHAIN=%q GOSUMDB=%q\n", os.Getenv("GOTOOLCHAIN"), os.Getenv("GOSUMDB"))
			os.Exit(2)
		}
		if os.Getenv("HTTP_PROXY") != "" || os.Getenv("HTTPS_PROXY") != "" {
			fmt.Fprintf(os.Stderr, "external HTTP_PROXY/HTTPS_PROXY leaked into sealed build\n")
			os.Exit(2)
		}
		goproxy := os.Getenv("GOPROXY")
		if !strings.HasPrefix(goproxy, "http://127.0.0.1:") {
			fmt.Fprintf(os.Stderr, "expected loopback GOPROXY, got %q\n", goproxy)
			os.Exit(2)
		}
		// Verify make is shadowed on PATH by the failing stub (exit 127).
		if err := exec.Command("make", "--version").Run(); err == nil {
			fmt.Fprintln(os.Stderr, "make succeeded; expected failing no-make stub on PATH")
			os.Exit(2)
		}
		goroot := os.Getenv("BASHY_TEST_SEALED_GOROOT")
		if goroot == "" {
			fmt.Fprintln(os.Stderr, "missing BASHY_TEST_SEALED_GOROOT")
			os.Exit(2)
		}
		goBinName := "go"
		if os.PathSeparator == '\\' {
			goBinName = "go.exe"
		}
		goBin := filepath.Join(goroot, "bin", goBinName)
		c := exec.Command(goBin, os.Args[2:]...)
		c.Stdin = os.Stdin
		c.Stdout = os.Stdout
		c.Stderr = os.Stderr
		c.Env = append(os.Environ(), "GOROOT="+goroot)
		if err := c.Run(); err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				os.Exit(ee.ExitCode())
			}
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	fmt.Fprintf(os.Stderr, "unsupported args: %v\n", os.Args[1:])
	os.Exit(2)
}
`

func TestRunSelfHostFixedPointEndToEndAndNondeterministicRejection(t *testing.T) {
	hostGo, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not on PATH")
	}
	gorootOut, err := exec.Command(hostGo, "env", "GOROOT").Output()
	if err != nil {
		t.Fatalf("go env GOROOT: %v", err)
	}
	sealedGoroot := strings.TrimSpace(string(gorootOut))
	t.Setenv("BASHY_TEST_SEALED_GOROOT", sealedGoroot)
	// Poison external proxies in parent env to verify RunSelfHostFixedPoint strips them.
	t.Setenv("HTTP_PROXY", "http://198.51.100.1:9999")
	t.Setenv("HTTPS_PROXY", "http://198.51.100.1:9999")

	ctx := context.Background()
	tmp := t.TempDir()
	artRoot := filepath.Join(tmp, "artifacts")
	loomDir := filepath.Join(tmp, "loom", "modules.git")

	// 1. Create and publish a sibling internal Go module into Loom + CAS.
	sibDir := filepath.Join(tmp, "sib-src")
	if _, err := ygit.Init(ygit.InitOptions{Path: sibDir, Quiet: true}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sibDir, "go.mod"), []byte("module example.com/bashy/publishedsib\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sibDir, "sib.go"), []byte("package publishedsib\n\nfunc Banner() string { return \"loom-sealed-ok\" }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ygit.Add(ygit.AddOptions{RepoPath: sibDir, All: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := ygit.Commit(ygit.CommitOptions{
		RepoPath:    sibDir,
		Message:     "publish v1.0.0",
		AuthorName:  "tester",
		AuthorEmail: "tester@local.invalid",
	}); err != nil {
		t.Fatal(err)
	}

	sibPub, err := gomod.PublishArtifact(ctx, gomod.PublishOptions{
		Dir:          sibDir,
		Version:      "v1.0.0",
		ArtifactRoot: artRoot,
		LoomRepoDir:  loomDir,
	})
	if err != nil {
		t.Fatalf("PublishArtifact: %v", err)
	}

	// Evict the derived .zip payload from CAS before running fixed-point so Stage 0
	// proves Loom-backed .zip reconstruction under zero external network access!
	{
		st, err := artifact.OpenStore(ctx, artifact.StoreOptions{Root: artRoot})
		if err != nil {
			t.Fatal(err)
		}
		items, err := st.DryRunPrune(ctx, 0)
		if err != nil {
			_ = st.Close()
			t.Fatal(err)
		}
		if _, err := st.ApplyPrune(ctx, items); err != nil {
			_ = st.Close()
			t.Fatal(err)
		}
		_ = st.Close()
	}

	// 2. Create the candidate self-hosting source checkout depending on example.com/bashy/publishedsib@v1.0.0.
	srcDir := filepath.Join(tmp, "bashy-src")
	cmdDir := filepath.Join(srcDir, "cmd", "bashy")
	if err := os.MkdirAll(cmdDir, 0o755); err != nil {
		t.Fatal(err)
	}
	goModContent := "module example.com/bashy/minidriver\n\ngo 1.22\n\nrequire example.com/bashy/publishedsib v1.0.0\n"
	goSumContent := "example.com/bashy/publishedsib v1.0.0 " + sibPub.Hash + "\n" +
		"example.com/bashy/publishedsib v1.0.0/go.mod " + sibPub.GoModSum + "\n"
	if err := os.WriteFile(filepath.Join(srcDir, "go.mod"), []byte(goModContent), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "go.sum"), []byte(goSumContent), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cmdDir, "main.go"), []byte(selfHostMiniDriverSource), 0o644); err != nil {
		t.Fatal(err)
	}

	lockPath := filepath.Join(tmp, "image.lock")
	lockText := strings.Join([]string{
		"bashy_version=v1.0.0-rc.41",
		"apt_snapshot=20260821T190000Z",
		"go_version=1.27.4",
		"go_sha256_amd64=" + strings.Repeat("1", 64),
		"go_sha256_arm64=" + strings.Repeat("2", 64),
		"base_image=localhost/bashy-base:v1.0.0-rc.41",
		"base_digest=sha256:" + strings.Repeat("3", 64),
		"image=localhost/bashy-build:v1.0.0-rc.41-go1.27.4-abcdef012345",
		"image_digest=sha256:" + strings.Repeat("4", 64),
		"source_bashy_commit=fedcba9876543210fedcba9876543210fedcba98",
		"",
	}, "\n")
	if err := os.WriteFile(lockPath, []byte(lockText), 0o644); err != nil {
		t.Fatal(err)
	}

	// Build PrevBashyExe (the "previous release" binary, intentionally stamped differently
	// from the candidate so Stage 0 != PrevBashyExe while Stage 1 == Stage 2!).
	prevExe := filepath.Join(tmp, "prev-bashy", filepath.Base(hostGo)+"-prev")
	if runtime.GOOS == "windows" {
		prevExe += ".exe"
	}
	if err := os.MkdirAll(filepath.Dir(prevExe), 0o755); err != nil {
		t.Fatal(err)
	}
	{
		srv, err := gomod.StartProxyServer(ctx, gomod.ProxyServerOptions{
			Addr:         "127.0.0.1:0",
			ArtifactRoot: artRoot,
			LoomRepoDir:  loomDir,
			Sealed:       true,
		})
		if err != nil {
			t.Fatalf("StartProxyServer for seed build: %v", err)
		}
		seedGoCache := filepath.Join(tmp, "seed-gocache")
		seedModCache := filepath.Join(tmp, "seed-gomodcache")
		c := exec.CommandContext(ctx, hostGo, "build", "-trimpath", "-ldflags", "-s -w -X main.seedGeneration=previous-release", "-o", prevExe, "./cmd/bashy")
		c.Dir = srcDir
		c.Env = append(os.Environ(),
			"GOPROXY="+srv.URL(),
			"GOSUMDB=off",
			"GOTOOLCHAIN=local",
			"CGO_ENABLED=0",
			"GOWORK=off",
			"GOFLAGS=-mod=readonly -buildvcs=false",
			"GOCACHE="+seedGoCache,
			"GOMODCACHE="+seedModCache,
		)
		out, buildErr := c.CombinedOutput()
		_ = srv.Close()
		_ = filepath.WalkDir(seedModCache, func(p string, _ os.DirEntry, _ error) error {
			_ = os.Chmod(p, 0o700)
			return nil
		})
		if buildErr != nil {
			t.Fatalf("build prevExe failed: %v\n%s", buildErr, string(out))
		}
	}

	// Re-evict reconstructed .zip again so Stage 0 of RunSelfHostFixedPoint itself exercises Loom regeneration!
	{
		st, err := artifact.OpenStore(ctx, artifact.StoreOptions{Root: artRoot})
		if err != nil {
			t.Fatal(err)
		}
		items, _ := st.DryRunPrune(ctx, 0)
		_, _ = st.ApplyPrune(ctx, items)
		_ = st.Close()
	}

	// 3. Run the 3-stage offline self-hosting fixed-point gate!
	verifiedOut := filepath.Join(tmp, "out", "bashy-verified")
	if runtime.GOOS == "windows" {
		verifiedOut += ".exe"
	}
	report, err := artifact.RunSelfHostFixedPoint(ctx, artifact.FixedPointOptions{
		PrevBashyExe:  prevExe,
		SourceDir:     srcDir,
		TargetPackage: "./cmd/bashy",
		ArtifactRoot:  artRoot,
		LoomDir:       loomDir,
		BuildLockPath: lockPath,
		OutputBinary:  verifiedOut,
	})
	if err != nil {
		t.Fatalf("RunSelfHostFixedPoint failed: %v", err)
	}
	if report.SchemaVersion != artifact.FixedPointSchemaV1 {
		t.Fatalf("SchemaVersion = %q, want %q", report.SchemaVersion, artifact.FixedPointSchemaV1)
	}
	if !report.FixedPointMatch {
		t.Fatalf("expected FixedPointMatch == true, got %+v", report)
	}
	if report.Candidate1SHA256 != report.Candidate2SHA256 || !artifact.ValidDigest(report.Candidate1SHA256) {
		t.Fatalf("candidate_1 (%s) != candidate_2 (%s)", report.Candidate1SHA256, report.Candidate2SHA256)
	}
	if len(report.Stages) != 3 {
		t.Fatalf("len(Stages) = %d, want 3", len(report.Stages))
	}
	for i, st := range report.Stages {
		if !st.GoCacheEmpty || !st.ModCacheEmpty {
			t.Fatalf("stage %d caches did not start empty: %+v", i, st)
		}
		if st.WorkspaceBytes == 0 {
			t.Fatalf("stage %d WorkspaceBytes == 0", i)
		}
	}
	if report.PeakWorkspaceBytes == 0 {
		t.Fatal("expected non-zero PeakWorkspaceBytes")
	}
	if report.Version != "v1.0.0-rc.41" || report.BuildID != "fedcba9" {
		t.Fatalf("unexpected version/build_id from image.lock: %s / %s", report.Version, report.BuildID)
	}
	if !strings.Contains(report.SmokeOutput, "sib=loom-sealed-ok") {
		t.Fatalf("unexpected SmokeOutput: %q", report.SmokeOutput)
	}
	if _, err := os.Stat(verifiedOut); err != nil {
		t.Fatalf("OutputBinary not written: %v", err)
	}

	// 4. Negative test: inject nondeterministic wall-clock/stage bytes via tools/productgo/main.go
	// and verify RunSelfHostFixedPoint FAILS CLOSED with FixedPointMatch == false!
	nondetOverlayDir := filepath.Join(srcDir, "tools", "productgo")
	if err := os.MkdirAll(nondetOverlayDir, 0o755); err != nil {
		t.Fatal(err)
	}
	nondetProductGo := `package main

import (
	"fmt"
	"os"
	"os/exec"
	"time"
)

func main() {
	// args: GOOS GOARCH CGO_ENABLED build -trimpath -ldflags <ldflags> -o <out> <pkg>
	args := os.Args[4:]
	stamp := fmt.Sprintf("wall-%d-stage-%s", time.Now().UnixNano(), os.Getenv("BASHY_FIXEDPOINT_STAGE"))
	injected := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == "-ldflags" && i+1 < len(args) {
			injected = append(injected, "-ldflags", args[i+1]+" -X main.nondetStamp="+stamp)
			i++
			continue
		}
		injected = append(injected, args[i])
	}
	exe := os.Getenv("BASHY_EXE")
	c := exec.Command(exe, append([]string{"go"}, injected...)...)
	c.Stdin = os.Stdin
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	c.Env = os.Environ()
	if err := c.Run(); err != nil {
		os.Exit(1)
	}
}
`
	if err := os.WriteFile(filepath.Join(nondetOverlayDir, "main.go"), []byte(nondetProductGo), 0o644); err != nil {
		t.Fatal(err)
	}

	badReport, badErr := artifact.RunSelfHostFixedPoint(ctx, artifact.FixedPointOptions{
		PrevBashyExe:  prevExe,
		SourceDir:     srcDir,
		TargetPackage: "./cmd/bashy",
		ArtifactRoot:  artRoot,
		LoomDir:       loomDir,
		BuildLockPath: lockPath,
	})
	if !errors.Is(badErr, artifact.ErrFixedPointMismatch) {
		t.Fatalf("expected ErrFixedPointMismatch on nondeterministic build, got: %v", badErr)
	}
	if badReport == nil || badReport.FixedPointMatch {
		t.Fatalf("expected non-nil report with FixedPointMatch == false, got: %+v", badReport)
	}
	if !badReport.ProductOverlayUsed {
		t.Fatal("expected ProductOverlayUsed == true when tools/productgo/main.go exists")
	}
	if badReport.Candidate1SHA256 == badReport.Candidate2SHA256 {
		t.Fatalf("expected candidate_1 and candidate_2 hashes to differ, both were %s", badReport.Candidate1SHA256)
	}
}

// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package gomod

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"golang.org/x/mod/module"
	modzip "golang.org/x/mod/zip"

	"github.com/qiangli/coreutils/pkg/weavecli"
	coregit "github.com/qiangli/yoke/git"
	"github.com/qiangli/yoke/pkg/artifact"
)

func initProducerGitRepo(t *testing.T, dir, modPath, greeting string) string {
	t.Helper()
	if _, err := coregit.Init(coregit.InitOptions{Path: dir, Quiet: true}); err != nil {
		t.Fatalf("git init: %v", err)
	}
	write(t, filepath.Join(dir, "go.mod"), "module "+modPath+"\n\ngo 1.22\n")
	write(t, filepath.Join(dir, "greeter.go"), fmt.Sprintf("package greeter\n\nfunc Greet() string { return %q }\n", greeting))
	if _, err := coregit.Add(coregit.AddOptions{RepoPath: dir, All: true}); err != nil {
		t.Fatalf("git add: %v", err)
	}
	if _, err := coregit.Commit(coregit.CommitOptions{
		RepoPath:    dir,
		Message:     "initial greeter",
		AuthorName:  "tester",
		AuthorEmail: "tester@example.invalid",
	}); err != nil {
		t.Fatalf("git commit: %v", err)
	}
	sha, err := coregit.ResolveCommit(dir, "HEAD")
	if err != nil {
		t.Fatalf("resolve HEAD: %v", err)
	}
	return sha
}

func makeWritableTempDir(t *testing.T, prefix string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), prefix)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = filepath.Walk(dir, func(p string, _ os.FileInfo, _ error) error {
			return os.Chmod(p, 0o777)
		})
	})
	return dir
}

func buildAndRunConsumerOffline(t *testing.T, consumerDir, proxyURL string) string {
	t.Helper()
	modCache := makeWritableTempDir(t, "gomodcache")
	goCache := makeWritableTempDir(t, "gocache")

	binName := "consumer-app"
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}
	binPath := filepath.Join(t.TempDir(), binName)

	env := append(os.Environ(),
		"GOMODCACHE="+modCache,
		"GOCACHE="+goCache,
		"GOWORK=off",
		"GOFLAGS=-mod=readonly -buildvcs=false",
	)
	env = append(env, SealedBuildEnv(proxyURL, os.Getenv)...)

	buildCmd := exec.Command("go", "build", "-o", binPath, ".")
	buildCmd.Dir = consumerDir
	buildCmd.Env = env
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("offline go build failed: %v\n%s", err, out)
	}

	runCmd := exec.Command(binPath)
	out, err := runCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("run compiled consumer failed: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestS2OfflineConsumerBuildEvictionRegenerationAndSingleflight(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH")
	}

	ctx := context.Background()
	artRoot := filepath.Join(t.TempDir(), "artifacts")
	loomDir := filepath.Join(artRoot, "loom", "modules.git")

	store, err := artifact.OpenStore(ctx, artifact.StoreOptions{Root: artRoot})
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	// 1. Create producer git repo, publish via PublishArtifact, then delete working tree completely!
	producerDir := filepath.Join(t.TempDir(), "greeter-src")
	const modPath = "example.com/offline/greeter"
	commitSHA := initProducerGitRepo(t, producerDir, modPath, "hello from sealed offline proxy")

	pub, err := PublishArtifact(ctx, PublishOptions{
		Dir:          producerDir,
		ArtifactRoot: artRoot,
		LoomRepoDir:  loomDir,
		Backend:      store,
	})
	if err != nil {
		t.Fatalf("PublishArtifact: %v", err)
	}
	if !pub.Published || pub.Commit != commitSHA || !module.IsPseudoVersion(pub.Version) {
		t.Fatalf("unexpected publication: %+v", pub)
	}
	if !strings.HasPrefix(pub.LoomRef, "refs/bashy/published/example.com/offline/greeter/") {
		t.Fatalf("unexpected LoomRef: %s", pub.LoomRef)
	}

	if err := os.RemoveAll(producerDir); err != nil {
		t.Fatalf("RemoveAll producerDir: %v", err)
	}
	if _, err := os.Stat(producerDir); !os.IsNotExist(err) {
		t.Fatalf("producer working tree still exists: %v", err)
	}

	var regenCount atomic.Int64
	srv, err := StartProxyServer(ctx, ProxyServerOptions{
		Addr:        "127.0.0.1:0",
		LoomRepoDir: loomDir,
		Backend:     store,
		Sealed:      true,
		OnRegenerate: func(m, v string) {
			if m == modPath && v == pub.Version {
				regenCount.Add(1)
			}
		},
	})
	if err != nil {
		t.Fatalf("StartProxyServer: %v", err)
	}
	defer srv.Close()

	// 2. Create separate clean-cache consumer module with go.sum and build offline.
	consumerDir := filepath.Join(t.TempDir(), "consumer")
	write(t, filepath.Join(consumerDir, "go.mod"), fmt.Sprintf("module example.com/offline/consumer\n\ngo 1.22\n\nrequire %s %s\n", modPath, pub.Version))
	goSum := fmt.Sprintf("%s %s %s\n%s %s/go.mod %s\n",
		modPath, pub.Version, pub.Hash,
		modPath, pub.Version, pub.GoModSum,
	)
	write(t, filepath.Join(consumerDir, "go.sum"), goSum)
	write(t, filepath.Join(consumerDir, "main.go"), `package main

import (
	"fmt"
	"example.com/offline/greeter"
)

func main() {
	fmt.Println(greeter.Greet())
}
`)

	gotGreeting := buildAndRunConsumerOffline(t, consumerDir, srv.URL())
	if gotGreeting != "hello from sealed offline proxy" {
		t.Fatalf("consumer output = %q, want %q", gotGreeting, "hello from sealed offline proxy")
	}
	if regenCount.Load() != 0 {
		t.Fatalf("unexpected regeneration on warm CAS: %d", regenCount.Load())
	}

	// 3a. Verify an active read lease over pub.ZipDigest prevents DryRunPrune/ApplyPrune from evicting .zip.
	if err := store.WithReadLease(ctx, 0, "", time.Minute, []string{pub.ZipDigest}, func(lctx context.Context) error {
		leasedItems, err := store.DryRunPrune(lctx, 0)
		if err != nil {
			return err
		}
		if len(leasedItems) != 0 {
			return fmt.Errorf("expected 0 evictable items while .zip is under active read lease, got %+v", leasedItems)
		}
		return nil
	}); err != nil {
		t.Fatalf("WithReadLease prune guard: %v", err)
	}

	// 3b. Evict reconstructible .zip via DryRunPrune + ApplyPrune; verify .info/.mod/PublicationRecord/LoomRef stay intact.
	items, err := store.DryRunPrune(ctx, 0)
	if err != nil {
		t.Fatalf("DryRunPrune: %v", err)
	}
	if len(items) != 1 || items[0].ID != pub.ZipDigest {
		t.Fatalf("DryRunPrune items = %+v, want only zip digest %s", items, pub.ZipDigest)
	}
	counts, err := store.ApplyPrune(ctx, items)
	if err != nil {
		t.Fatalf("ApplyPrune: %v", err)
	}
	if counts.Removed != 1 {
		t.Fatalf("ApplyPrune counts = %+v, want Removed=1", counts)
	}

	zipSt, err := store.GetRecordStatus(ctx, pub.ZipDigest)
	if err != nil || zipSt.PayloadPresent {
		t.Fatalf("expected evicted zip PayloadPresent=false, got st=%+v err=%v", zipSt, err)
	}
	for _, protectedDgst := range []string{pub.InfoDigest, pub.ModDigest, pub.SourceDigest} {
		st, err := store.GetRecordStatus(ctx, protectedDgst)
		if err != nil || !st.PayloadPresent {
			t.Fatalf("protected artifact %s was evicted: st=%+v err=%v", protectedDgst, st, err)
		}
	}
	if _, err := store.GetPublication(ctx, modPath, pub.Version); err != nil {
		t.Fatalf("PublicationRecord missing after prune: %v", err)
	}
	loomRepo, err := gogit.PlainOpen(loomDir)
	if err != nil {
		t.Fatalf("open bare loom repo: %v", err)
	}
	if ref, err := loomRepo.Reference(plumbing.ReferenceName(pub.LoomRef), true); err != nil || ref.Hash().String() != pub.Commit {
		t.Fatalf("loom ref %s after prune = (%v, %v), want %s", pub.LoomRef, ref, err, pub.Commit)
	}

	// 4. Second clean-cache consumer build regenerates .zip from Loom commit offline!
	gotGreeting2 := buildAndRunConsumerOffline(t, consumerDir, srv.URL())
	if gotGreeting2 != "hello from sealed offline proxy" {
		t.Fatalf("second consumer output = %q", gotGreeting2)
	}
	if regenCount.Load() != 1 {
		t.Fatalf("OnRegenerate count = %d, want 1", regenCount.Load())
	}
	zipStAfter, err := store.GetRecordStatus(ctx, pub.ZipDigest)
	if err != nil || !zipStAfter.PayloadPresent {
		t.Fatalf("expected regenerated zip PayloadPresent=true, got %+v (%v)", zipStAfter, err)
	}
	restoredZip, err := store.FetchVerified(ctx, pub.ZipDigest)
	if err != nil {
		t.Fatalf("FetchVerified restored zip: %v", err)
	}
	if gotH1, err := hashZipBytes(restoredZip); err != nil || gotH1 != pub.Hash {
		t.Fatalf("restored zip h1 = (%q, %v), want %q", gotH1, err, pub.Hash)
	}

	// 5. Concurrent single-flight regeneration: evict .zip again and fire 16 concurrent GETs.
	items2, err := store.DryRunPrune(ctx, 0)
	if err != nil || len(items2) != 1 {
		t.Fatalf("second DryRunPrune = (%+v, %v)", items2, err)
	}
	if _, err := store.ApplyPrune(ctx, items2); err != nil {
		t.Fatalf("second ApplyPrune: %v", err)
	}
	regenCount.Store(0)

	// Add a brief pause inside OnRegenerate on a dedicated server to guarantee all 16 callers overlap in-flight.
	var sfRegenCount atomic.Int64
	sfSrv, err := StartProxyServer(ctx, ProxyServerOptions{
		Addr:        "127.0.0.1:0",
		LoomRepoDir: loomDir,
		Backend:     store,
		Sealed:      true,
		OnRegenerate: func(m, v string) {
			sfRegenCount.Add(1)
			time.Sleep(50 * time.Millisecond)
		},
	})
	if err != nil {
		t.Fatalf("StartProxyServer sfSrv: %v", err)
	}
	defer sfSrv.Close()

	escMod, _ := module.EscapePath(modPath)
	escVer, _ := module.EscapeVersion(pub.Version)
	zipURL := fmt.Sprintf("%s/%s/@v/%s.zip", sfSrv.URL(), escMod, escVer)

	const concurrency = 16
	startBarrier := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, concurrency)

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-startBarrier
			resp, err := http.Get(zipURL)
			if err != nil {
				errs <- fmt.Errorf("req %d: %w", idx, err)
				return
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				errs <- fmt.Errorf("req %d read body: %w", idx, err)
				return
			}
			if resp.StatusCode != http.StatusOK {
				errs <- fmt.Errorf("req %d status=%d body=%q", idx, resp.StatusCode, string(body))
				return
			}
			if d := artifact.DigestBytes(body); d != pub.ZipDigest {
				errs <- fmt.Errorf("req %d digest=%s != %s", idx, d, pub.ZipDigest)
				return
			}
			if !bytes.Equal(body, restoredZip) {
				errs <- fmt.Errorf("req %d bytes mismatch", idx)
				return
			}
		}(i)
	}
	close(startBarrier)
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Fatal(err)
	}
	if sfRegenCount.Load() != 1 {
		t.Fatalf("concurrent singleflight OnRegenerate count = %d, want 1", sfRegenCount.Load())
	}
}

func TestS2ImmutableOverwriteRefusalAndIdempotentRepublish(t *testing.T) {
	ctx := context.Background()
	artRoot := filepath.Join(t.TempDir(), "artifacts")
	loomDir := filepath.Join(artRoot, "loom", "modules.git")

	store, err := artifact.OpenStore(ctx, artifact.StoreOptions{Root: artRoot})
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	producerDir := filepath.Join(t.TempDir(), "greeter")
	const modPath = "example.com/offline/greeter"
	initProducerGitRepo(t, producerDir, modPath, "v1")

	pub1, err := PublishArtifact(ctx, PublishOptions{
		Dir:         producerDir,
		Version:     "v0.1.0",
		LoomRepoDir: loomDir,
		Backend:     store,
	})
	if err != nil {
		t.Fatalf("initial PublishArtifact v0.1.0: %v", err)
	}
	if !pub1.Published {
		t.Fatal("expected Published=true on first publish")
	}

	// Re-publishing identical unchanged commit at v0.1.0 must succeed idempotently with Published=false.
	pubAgain, err := PublishArtifact(ctx, PublishOptions{
		Dir:         producerDir,
		Version:     "v0.1.0",
		LoomRepoDir: loomDir,
		Backend:     store,
	})
	if err != nil {
		t.Fatalf("idempotent republish v0.1.0: %v", err)
	}
	if pubAgain.Published || pubAgain.ZipDigest != pub1.ZipDigest || pubAgain.Hash != pub1.Hash {
		t.Fatalf("expected idempotent no-op, got %+v", pubAgain)
	}

	// Uncommitted dirty edit on tracked file with explicit version must be refused before publication.
	write(t, filepath.Join(producerDir, "greeter.go"), "package greeter\n\nfunc Greet() string { return \"dirty\" }\n")
	if _, err := PublishArtifact(ctx, PublishOptions{
		Dir:         producerDir,
		Version:     "v0.2.0",
		LoomRepoDir: loomDir,
		Backend:     store,
	}); ExitCodeOf(err) != weavecli.ExitPrecondFail {
		t.Fatalf("dirty worktree publish err = %v (code %d), want ExitPrecondFail", err, ExitCodeOf(err))
	}

	// Commit the modified greeter.go and attempt overwrite of immutable v0.1.0 -> must refuse with ErrImmutablePublicationConflict!
	if _, err := coregit.Add(coregit.AddOptions{RepoPath: producerDir, All: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := coregit.Commit(coregit.CommitOptions{
		RepoPath:    producerDir,
		Message:     "change greeting",
		AuthorName:  "tester",
		AuthorEmail: "tester@example.invalid",
	}); err != nil {
		t.Fatal(err)
	}

	_, conflictErr := PublishArtifact(ctx, PublishOptions{
		Dir:         producerDir,
		Version:     "v0.1.0",
		LoomRepoDir: loomDir,
		Backend:     store,
	})
	if !errors.Is(conflictErr, artifact.ErrImmutablePublicationConflict) {
		t.Fatalf("overwrite conflict err = %v, want errors.Is(..., ErrImmutablePublicationConflict)", conflictErr)
	}
	if ExitCodeOf(conflictErr) != weavecli.ExitPrecondFail {
		t.Fatalf("overwrite ExitCodeOf = %d, want %d (ExitPrecondFail)", ExitCodeOf(conflictErr), weavecli.ExitPrecondFail)
	}

	// Publishing the new commit under a new version v0.2.0 succeeds.
	pub2, err := PublishArtifact(ctx, PublishOptions{
		Dir:         producerDir,
		Version:     "v0.2.0",
		LoomRepoDir: loomDir,
		Backend:     store,
	})
	if err != nil || !pub2.Published {
		t.Fatalf("PublishArtifact v0.2.0: %+v %v", pub2, err)
	}
}

type countingFakeFetcher struct {
	calls   atomic.Int64
	modules map[string]fakeModuleBundle
}

type fakeModuleBundle struct {
	version string
	when    time.Time
	gomod   []byte
	zipData []byte
}

func (f *countingFakeFetcher) Query(ctx context.Context, modPath, query string) (string, time.Time, error) {
	f.calls.Add(1)
	if b, ok := f.modules[modPath+"@"+query]; ok {
		return b.version, b.when, nil
	}
	return "", time.Time{}, os.ErrNotExist
}

func (f *countingFakeFetcher) List(ctx context.Context, modPath string) ([]string, error) {
	f.calls.Add(1)
	var out []string
	prefix := modPath + "@"
	for k, b := range f.modules {
		if strings.HasPrefix(k, prefix) {
			out = append(out, b.version)
		}
	}
	if len(out) == 0 {
		return nil, os.ErrNotExist
	}
	return out, nil
}

func (f *countingFakeFetcher) Download(ctx context.Context, modPath, version string) (io.ReadSeekCloser, io.ReadSeekCloser, io.ReadSeekCloser, error) {
	f.calls.Add(1)
	b, ok := f.modules[modPath+"@"+version]
	if !ok {
		return nil, nil, nil, os.ErrNotExist
	}
	infoBytes, _ := json.Marshal(struct {
		Version string
		Time    string
	}{b.version, b.when.UTC().Format(time.RFC3339)})
	return bytesReadSeekCloser{bytes.NewReader(infoBytes)},
		bytesReadSeekCloser{bytes.NewReader(b.gomod)},
		bytesReadSeekCloser{bytes.NewReader(b.zipData)},
		nil
}

func TestS2MissingRecoveryInputsHardFailureWithZeroUpstreamFallback(t *testing.T) {
	ctx := context.Background()
	artRoot := filepath.Join(t.TempDir(), "artifacts")
	loomDir := filepath.Join(artRoot, "loom", "modules.git")

	store, err := artifact.OpenStore(ctx, artifact.StoreOptions{Root: artRoot})
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	// Publish a non-git module directory with explicit version to also exercise deterministic commit synthesis.
	nonGitDir := filepath.Join(t.TempDir(), "nongit-mod")
	const modPath = "example.com/internal/broken"
	write(t, filepath.Join(nonGitDir, "go.mod"), "module "+modPath+"\n\ngo 1.22\n")
	write(t, filepath.Join(nonGitDir, "broken.go"), "package broken\n\nconst X = 42\n")

	pub, err := PublishArtifact(ctx, PublishOptions{
		Dir:         nonGitDir,
		Version:     "v0.1.0",
		LoomRepoDir: loomDir,
		Backend:     store,
	})
	if err != nil || !pub.Published {
		t.Fatalf("PublishArtifact non-git: %+v %v", pub, err)
	}
	// Verify idempotent republish on synthesized non-git dir too.
	again, err := PublishArtifact(ctx, PublishOptions{
		Dir:         nonGitDir,
		Version:     "v0.1.0",
		LoomRepoDir: loomDir,
		Backend:     store,
	})
	if err != nil || again.Published {
		t.Fatalf("expected non-git republish to be idempotent, got %+v %v", again, err)
	}

	// Evict .zip via prune.
	items, err := store.DryRunPrune(ctx, 0)
	if err != nil || len(items) != 1 {
		t.Fatalf("DryRunPrune: %+v %v", items, err)
	}
	if _, err := store.ApplyPrune(ctx, items); err != nil {
		t.Fatalf("ApplyPrune: %v", err)
	}

	// Corrupt/remove the immutable Loom ref in the bare repository!
	loomRepo, err := gogit.PlainOpen(loomDir)
	if err != nil {
		t.Fatalf("PlainOpen loom: %v", err)
	}
	if err := loomRepo.Storer.RemoveReference(plumbing.ReferenceName(pub.LoomRef)); err != nil {
		t.Fatalf("RemoveReference: %v", err)
	}

	fakeUpstream := &countingFakeFetcher{modules: map[string]fakeModuleBundle{}}
	// Populate fakeUpstream WITH a fake bundle for the exact same module@version to prove
	// the proxy NEVER falls back to upstream even if Sealed==false!
	var fakeZip bytes.Buffer
	zw := zip.NewWriter(&fakeZip)
	fw, _ := zw.Create(modPath + "@v0.1.0/go.mod")
	_, _ = fw.Write([]byte("module " + modPath + "\n"))
	_ = zw.Close()
	fakeUpstream.modules[modPath+"@v0.1.0"] = fakeModuleBundle{
		version: "v0.1.0",
		when:    time.Unix(0, 0).UTC(),
		gomod:   []byte("module " + modPath + "\n"),
		zipData: fakeZip.Bytes(),
	}

	for _, tc := range []struct {
		name   string
		sealed bool
	}{
		{name: "sealed", sealed: true},
		{name: "unsealed_no_fallback_for_internal", sealed: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeUpstream.calls.Store(0)
			srv, err := StartProxyServer(ctx, ProxyServerOptions{
				Addr:        "127.0.0.1:0",
				LoomRepoDir: loomDir,
				Backend:     store,
				Sealed:      tc.sealed,
				Fetcher:     fakeUpstream,
			})
			if err != nil {
				t.Fatalf("StartProxyServer: %v", err)
			}
			defer srv.Close()

			resp, err := http.Get(srv.URL() + "/example.com/internal/broken/@v/v0.1.0.zip")
			if err != nil {
				t.Fatalf("GET zip: %v", err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()

			if resp.StatusCode == http.StatusOK {
				t.Fatalf("expected hard failure status != 200, got 200 (%d bytes)", len(body))
			}
			if !strings.Contains(string(body), artifact.ErrMissingRecoveryInputs.Error()) {
				t.Fatalf("expected diagnostic %q in body, got status=%d body=%q",
					artifact.ErrMissingRecoveryInputs.Error(), resp.StatusCode, string(body))
			}
			if gotCalls := fakeUpstream.calls.Load(); gotCalls != 0 {
				t.Fatalf("upstream Fetcher call count = %d, want 0", gotCalls)
			}
		})
	}
}

func TestS2PublicUpstreamWarmupThenSealedOfflineAndBindGuard(t *testing.T) {
	ctx := context.Background()
	artRoot := filepath.Join(t.TempDir(), "artifacts")
	loomDir := filepath.Join(artRoot, "loom", "modules.git")

	store, err := artifact.OpenStore(ctx, artifact.StoreOptions{Root: artRoot})
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	// Non-loopback bind without AllowMeshBind must fail closed with ErrNonLoopbackBind.
	if _, err := StartProxyServer(ctx, ProxyServerOptions{
		Addr:        "0.0.0.0:0",
		LoomRepoDir: loomDir,
		Backend:     store,
		Sealed:      true,
	}); !errors.Is(err, artifact.ErrNonLoopbackBind) {
		t.Fatalf("expected ErrNonLoopbackBind on 0.0.0.0:0, got %v", err)
	}

	// Build a valid canonical zip for a simulated public dependency.
	pubSrc := t.TempDir()
	const pubMod = "example.com/public/dep"
	pubGoMod := []byte("module " + pubMod + "\n\ngo 1.22\n")
	write(t, filepath.Join(pubSrc, "go.mod"), string(pubGoMod))
	write(t, filepath.Join(pubSrc, "dep.go"), "package dep\n\nconst Answer = 7\n")
	var pubZip bytes.Buffer
	if err := modzip.CreateFromDir(&pubZip, module.Version{Path: pubMod, Version: "v1.0.0"}, pubSrc); err != nil {
		t.Fatalf("CreateFromDir: %v", err)
	}

	fakeUpstream := &countingFakeFetcher{
		modules: map[string]fakeModuleBundle{
			pubMod + "@v1.0.0": {
				version: "v1.0.0",
				when:    time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
				gomod:   pubGoMod,
				zipData: pubZip.Bytes(),
			},
		},
	}

	// Warm public module in unsealed mode.
	warmSrv, err := StartProxyServer(ctx, ProxyServerOptions{
		Addr:        "127.0.0.1:0",
		LoomRepoDir: loomDir,
		Backend:     store,
		Sealed:      false,
		Fetcher:     fakeUpstream,
	})
	if err != nil {
		t.Fatalf("StartProxyServer warm: %v", err)
	}
	resp, err := http.Get(warmSrv.URL() + "/example.com/public/dep/@v/v1.0.0.zip")
	if err != nil {
		t.Fatal(err)
	}
	warmBytes, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	_ = warmSrv.Close()
	if resp.StatusCode != http.StatusOK || !bytes.Equal(warmBytes, pubZip.Bytes()) {
		t.Fatalf("warm download failed: status=%d", resp.StatusCode)
	}
	if fakeUpstream.calls.Load() != 1 {
		t.Fatalf("expected 1 upstream download call during warmup, got %d", fakeUpstream.calls.Load())
	}

	// Now start in Sealed: true mode and verify cached public module serves offline while uncached fails with ErrSealedNetworkDenied!
	sealedSrv, err := StartProxyServer(ctx, ProxyServerOptions{
		Addr:        "127.0.0.1:0",
		LoomRepoDir: loomDir,
		Backend:     store,
		Sealed:      true,
		Fetcher:     fakeUpstream,
	})
	if err != nil {
		t.Fatalf("StartProxyServer sealed: %v", err)
	}
	defer sealedSrv.Close()

	for _, ep := range []string{
		"/example.com/public/dep/@v/list",
		"/example.com/public/dep/@v/v1.0.0.info",
		"/example.com/public/dep/@v/v1.0.0.mod",
		"/example.com/public/dep/@v/v1.0.0.zip",
	} {
		r, err := http.Get(sealedSrv.URL() + ep)
		if err != nil {
			t.Fatalf("sealed GET %s: %v", ep, err)
		}
		_, _ = io.ReadAll(r.Body)
		r.Body.Close()
		if r.StatusCode != http.StatusOK {
			t.Fatalf("sealed GET %s status = %d, want 200", ep, r.StatusCode)
		}
	}

	// Uncached module in sealed mode must be denied without hitting fakeUpstream!
	beforeCalls := fakeUpstream.calls.Load()
	missResp, err := http.Get(sealedSrv.URL() + "/example.com/public/uncached/@v/v1.0.0.info")
	if err != nil {
		t.Fatal(err)
	}
	missBody, _ := io.ReadAll(missResp.Body)
	missResp.Body.Close()
	if missResp.StatusCode == http.StatusOK || !strings.Contains(string(missBody), artifact.ErrSealedNetworkDenied.Error()) {
		t.Fatalf("expected sealed denial, got status=%d body=%q", missResp.StatusCode, string(missBody))
	}
	if fakeUpstream.calls.Load() != beforeCalls {
		t.Fatal("sealed server called upstream fetcher")
	}
}

func TestS2ModProxyCLI(t *testing.T) {
	artRoot := filepath.Join(t.TempDir(), "artifacts")
	loomDir := filepath.Join(artRoot, "loom", "modules.git")

	producerDir := filepath.Join(t.TempDir(), "cli-greeter")
	initProducerGitRepo(t, producerDir, "example.com/cli/greeter", "from cli")

	// 1. mod proxy publish --catalog --json
	var pubOut, pubErr bytes.Buffer
	code := Run([]string{
		"proxy", "publish", producerDir,
		"--version", "v0.1.0",
		"--catalog",
		"--artifact-root", artRoot,
		"--loom-dir", loomDir,
		"--json",
	}, &pubOut, &pubErr)
	if code != weavecli.ExitOK {
		t.Fatalf("mod proxy publish exit=%d stderr=%s stdout=%s", code, pubErr.String(), pubOut.String())
	}
	var pubEnv struct {
		Status string              `json:"status"`
		Result ArtifactPublication `json:"result"`
	}
	if err := json.Unmarshal(pubOut.Bytes(), &pubEnv); err != nil || pubEnv.Status != "ok" || !pubEnv.Result.Published {
		t.Fatalf("unmarshal publish --json: %v (%s)", err, pubOut.String())
	}

	// 2. mod proxy status --catalog --json
	var stOut, stErr bytes.Buffer
	code = Run([]string{
		"proxy", "status",
		"--catalog",
		"--artifact-root", artRoot,
		"--json",
	}, &stOut, &stErr)
	if code != weavecli.ExitOK {
		t.Fatalf("mod proxy status exit=%d stderr=%s", code, stErr.String())
	}
	var stEnv struct {
		Status string `json:"status"`
		Result struct {
			Modules []CatalogModuleStatus `json:"modules"`
		} `json:"result"`
	}
	if err := json.Unmarshal(stOut.Bytes(), &stEnv); err != nil || stEnv.Status != "ok" || len(stEnv.Result.Modules) != 1 {
		t.Fatalf("unmarshal status --json: %v (%s)", err, stOut.String())
	}
	if got := stEnv.Result.Modules[0]; got.Module != "example.com/cli/greeter" || len(got.Publications) != 1 || !got.Publications[0].ZipPresent {
		t.Fatalf("unexpected status module: %+v", got)
	}

	// 3. mod proxy serve startup, HTTP fetch, and context cancellation shutdown.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pr, pw := io.Pipe()
	var serveErrBuf bytes.Buffer
	serveDone := make(chan error, 1)

	cmd := NewModCmd()
	cmd.SetOut(pw)
	cmd.SetErr(&serveErrBuf)
	cmd.SetArgs([]string{
		"proxy", "serve",
		"--addr", "127.0.0.1:0",
		"--artifact-root", artRoot,
		"--loom-dir", loomDir,
		"--sealed",
	})

	go func() {
		err := cmd.ExecuteContext(ctx)
		_ = pw.Close()
		serveDone <- err
	}()

	lineCh := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(pr)
		if sc.Scan() {
			lineCh <- strings.TrimSpace(sc.Text())
		}
	}()

	var listenURL string
	select {
	case listenURL = <-lineCh:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatalf("timed out waiting for mod proxy serve URL (stderr=%s)", serveErrBuf.String())
	}
	if !strings.HasPrefix(listenURL, "http://127.0.0.1:") {
		cancel()
		t.Fatalf("unexpected listen URL %q", listenURL)
	}

	resp, err := http.Get(listenURL + "/example.com/cli/greeter/@v/v0.1.0.info")
	if err != nil {
		cancel()
		t.Fatalf("GET .info from CLI server: %v", err)
	}
	infoBytes, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(infoBytes), `"Version":"v0.1.0"`) {
		cancel()
		t.Fatalf("unexpected .info response status=%d body=%s", resp.StatusCode, infoBytes)
	}

	cancel()
	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatalf("mod proxy serve clean shutdown returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for mod proxy serve shutdown")
	}
}

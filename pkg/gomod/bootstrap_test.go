// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package gomod

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/mod/module"
	modzip "golang.org/x/mod/zip"

	"github.com/qiangli/yoke/pkg/artifact"
)

func stageFakeCachedModule(t *testing.T, modCache, modPath, version, goSource string) (zipH1, modH1 string) {
	t.Helper()
	srcDir := t.TempDir()
	gomodBytes := []byte("module " + modPath + "\n\ngo 1.22\n")
	write(t, filepath.Join(srcDir, "go.mod"), string(gomodBytes))
	write(t, filepath.Join(srcDir, "lib.go"), goSource)

	var zbuf bytes.Buffer
	if err := modzip.CreateFromDir(&zbuf, module.Version{Path: modPath, Version: version}, srcDir); err != nil {
		t.Fatalf("CreateFromDir: %v", err)
	}
	zipBytes := zbuf.Bytes()
	var err error
	zipH1, err = hashZipBytes(zipBytes)
	if err != nil {
		t.Fatal(err)
	}
	modH1, err = HashGoModBytes(gomodBytes)
	if err != nil {
		t.Fatal(err)
	}

	escMod, err := module.EscapePath(modPath)
	if err != nil {
		t.Fatal(err)
	}
	escVer, err := module.EscapeVersion(version)
	if err != nil {
		t.Fatal(err)
	}
	vDir := filepath.Join(modCache, "cache", "download", filepath.FromSlash(escMod), "@v")
	if err := os.MkdirAll(vDir, 0o755); err != nil {
		t.Fatal(err)
	}
	infoBytes, _ := json.Marshal(struct {
		Version string
		Time    string
	}{Version: version, Time: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)})

	write(t, filepath.Join(vDir, escVer+".info"), string(infoBytes))
	write(t, filepath.Join(vDir, escVer+".mod"), string(gomodBytes))
	if err := os.WriteFile(filepath.Join(vDir, escVer+".zip"), zipBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	return zipH1, modH1
}

func TestCaptureModuleClosureOfflineBuildAndRetention(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH")
	}

	ctx := context.Background()
	t.Setenv("BASHY_OFFLINE", "")

	artRoot := filepath.Join(t.TempDir(), "artifacts")
	loomDir := filepath.Join(artRoot, "loom", "modules.git")
	store, err := artifact.OpenStore(ctx, artifact.StoreOptions{Root: artRoot})
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	// 1. Prepare a local sibling git module to publish + an external cached module in GOMODCACHE.
	siblingDir := filepath.Join(t.TempDir(), "sibling")
	const siblingMod = "example.com/local/sibling"
	initProducerGitRepo(t, siblingDir, siblingMod, "sibling-ok")

	seedModCache := filepath.Join(t.TempDir(), "seed-gomodcache")
	const extMod = "example.com/external/util"
	const extVer = "v1.2.3"
	extZipH1, extModH1 := stageFakeCachedModule(t, seedModCache, extMod, extVer, "package util\n\nfunc Tag() string { return \"ext-ok\" }\n")

	// Publish sibling once to learn its pseudo-version & hashes for consumer go.mod/go.sum, then CaptureModuleClosure idempotently.
	sibPub, err := PublishArtifact(ctx, PublishOptions{
		Dir:          siblingDir,
		ArtifactRoot: artRoot,
		LoomRepoDir:  loomDir,
		Backend:      store,
	})
	if err != nil {
		t.Fatalf("PublishArtifact sibling: %v", err)
	}

	consumerDir := filepath.Join(t.TempDir(), "consumer")
	write(t, filepath.Join(consumerDir, "go.mod"), fmt.Sprintf(
		"module example.com/app/consumer\n\ngo 1.22\n\nrequire (\n\t%s %s\n\t%s %s\n)\n",
		siblingMod, sibPub.Version, extMod, extVer,
	))
	write(t, filepath.Join(consumerDir, "go.sum"), fmt.Sprintf(
		"%s %s %s\n%s %s/go.mod %s\n%s %s %s\n%s %s/go.mod %s\n",
		siblingMod, sibPub.Version, sibPub.Hash,
		siblingMod, sibPub.Version, sibPub.GoModSum,
		extMod, extVer, extZipH1,
		extMod, extVer, extModH1,
	))
	write(t, filepath.Join(consumerDir, "main.go"), `package main

import (
	"fmt"
	"example.com/external/util"
	"example.com/local/sibling"
)

func main() {
	fmt.Println(greeter.Greet() + ":" + util.Tag())
}
`)

	closureRes, err := CaptureModuleClosure(ctx, ModuleClosureOptions{
		ModuleDirs:   []string{consumerDir},
		PublishDirs:  []string{siblingDir},
		GoModCache:   seedModCache,
		ArtifactRoot: artRoot,
		LoomRepoDir:  loomDir,
		Backend:      store,
	})
	if err != nil {
		t.Fatalf("CaptureModuleClosure: %v", err)
	}
	if closureRes.Ingested != 1 || len(closureRes.Published) != 1 || len(closureRes.Roots) != 2 {
		t.Fatalf("unexpected ModuleClosureResult: %+v", closureRes)
	}

	// Delete both original sibling working tree and seed GOMODCACHE!
	if err := os.RemoveAll(siblingDir); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(seedModCache); err != nil {
		t.Fatal(err)
	}

	// Prune store: external module zip MUST remain protected, while published sibling zip evicts and reconstructs from Loom!
	pruneItems, err := store.DryRunPrune(ctx, 0)
	if err != nil {
		t.Fatalf("DryRunPrune: %v", err)
	}
	if len(pruneItems) != 1 || pruneItems[0].ID != sibPub.ZipDigest {
		t.Fatalf("expected only reconstructible sibling zip to be evictable, got %+v", pruneItems)
	}
	if _, err := store.ApplyPrune(ctx, pruneItems); err != nil {
		t.Fatalf("ApplyPrune: %v", err)
	}

	srv, err := StartProxyServer(ctx, ProxyServerOptions{
		Addr:         "127.0.0.1:0",
		ArtifactRoot: artRoot,
		LoomRepoDir:  loomDir,
		Backend:      store,
		Sealed:       true,
	})
	if err != nil {
		t.Fatalf("StartProxyServer: %v", err)
	}
	defer srv.Close()

	gotOut := buildAndRunConsumerOffline(t, consumerDir, srv.URL())
	if gotOut != "sibling-ok:ext-ok" {
		t.Fatalf("offline consumer output = %q, want %q", gotOut, "sibling-ok:ext-ok")
	}
}

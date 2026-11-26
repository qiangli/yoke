// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package artifact

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5/memfs"
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"
	"golang.org/x/mod/module"
	modzip "golang.org/x/mod/zip"
)

func seedTestLoomPublication(t *testing.T, ctx context.Context, store *Store, loomDir, modPath, version string) PublicationRecord {
	t.Helper()
	memStore := memory.NewStorage()
	fsys := memfs.New()
	repo, err := gogit.Init(memStore, fsys)
	if err != nil {
		t.Fatalf("gogit.Init: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}

	gomodBytes := []byte("module " + modPath + "\n\ngo 1.27\n")
	srcCode := []byte("package lib\n\nconst Greeting = \"portable-loom-ok\"\n")
	for name, body := range map[string][]byte{"go.mod": gomodBytes, "lib.go": srcCode} {
		f, err := fsys.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.Write(body)
		_ = f.Close()
		if _, err := wt.Add(name); err != nil {
			t.Fatal(err)
		}
	}
	sig := &object.Signature{Name: "test", Email: "test@local.invalid", When: time.Unix(0, 0).UTC()}
	commitHash, err := wt.Commit("publish", &gogit.CommitOptions{Author: sig, Committer: sig})
	if err != nil {
		t.Fatal(err)
	}

	bareLoom, err := openOrInitBareRepo(loomDir)
	if err != nil {
		t.Fatalf("openOrInitBareRepo: %v", err)
	}
	if err := copyGitCommitClosure(repo.Storer, bareLoom.Storer, commitHash); err != nil {
		t.Fatalf("copyGitCommitClosure: %v", err)
	}
	loomRef, err := LoomPublicationRef(modPath, version)
	if err != nil {
		t.Fatal(err)
	}
	if err := bareLoom.Storer.SetReference(plumbing.NewHashReference(plumbing.ReferenceName(loomRef), commitHash)); err != nil {
		t.Fatal(err)
	}

	// Build canonical zip from tmp dir.
	tmpMod := t.TempDir()
	_ = os.WriteFile(filepath.Join(tmpMod, "go.mod"), gomodBytes, 0o644)
	_ = os.WriteFile(filepath.Join(tmpMod, "lib.go"), srcCode, 0o644)
	var zbuf bytes.Buffer
	if err := modzip.CreateFromDir(&zbuf, module.Version{Path: modPath, Version: version}, tmpMod); err != nil {
		t.Fatalf("CreateFromDir: %v", err)
	}
	zipBytes := zbuf.Bytes()
	infoBytes := []byte(`{"Version":"` + version + `","Time":"1970-01-01T00:00:00Z"}`)
	srcAnchor := []byte(`{"loom_ref":"` + loomRef + `","commit":"` + commitHash.String() + `"}`)

	srcDgst := DigestBytes(srcAnchor)
	infoDgst := DigestBytes(infoBytes)
	modDgst := DigestBytes(gomodBytes)
	zipDgst := DigestBytes(zipBytes)

	for _, item := range []struct {
		rec  CatalogRecordV1
		data []byte
	}{
		{
			data: srcAnchor,
			rec: CatalogRecordV1{
				SchemaVersion:  CatalogRecordSchemaV1,
				Digest:         srcDgst,
				Size:           int64(len(srcAnchor)),
				MediaType:      "application/json",
				LogicalName:    "gomod-source/" + modPath,
				LogicalVersion: version,
				Provenance:     "loom:" + loomRef,
				Retention:      RetentionProtectedRoot,
				RetentionRoots: []string{"loom:" + loomRef},
			},
		},
		{
			data: infoBytes,
			rec: CatalogRecordV1{
				SchemaVersion:  CatalogRecordSchemaV1,
				Digest:         infoDgst,
				Size:           int64(len(infoBytes)),
				MediaType:      "application/json",
				LogicalName:    "gomod-info/" + modPath,
				LogicalVersion: version,
				Dependencies:   []string{srcDgst},
				Provenance:     "loom:" + loomRef,
				Retention:      RetentionProtectedRoot,
				RetentionRoots: []string{"gomod-pub:" + modPath + "@" + version},
			},
		},
		{
			data: gomodBytes,
			rec: CatalogRecordV1{
				SchemaVersion:  CatalogRecordSchemaV1,
				Digest:         modDgst,
				Size:           int64(len(gomodBytes)),
				MediaType:      "text/plain; charset=utf-8",
				LogicalName:    "gomod-mod/" + modPath,
				LogicalVersion: version,
				Dependencies:   []string{srcDgst},
				Provenance:     "loom:" + loomRef,
				Retention:      RetentionProtectedRoot,
				RetentionRoots: []string{"gomod-pub:" + modPath + "@" + version},
			},
		},
		{
			data: zipBytes,
			rec: CatalogRecordV1{
				SchemaVersion:  CatalogRecordSchemaV1,
				Digest:         zipDgst,
				Size:           int64(len(zipBytes)),
				MediaType:      "application/zip",
				LogicalName:    "gomod-zip/" + modPath,
				LogicalVersion: version,
				Provenance:     "loom:" + loomRef,
				Retention:      RetentionReconstructible,
				Recipe: &RegenerationRecipe{
					Kind:           RecipeGoModZipV1,
					InputDigests:   []string{srcDgst},
					LoomRef:        loomRef,
					Commit:         commitHash.String(),
					ExpectedDigest: zipDgst,
					ExpectedGoSum:  "h1:1234567890abcdef=",
				},
			},
		},
	} {
		if _, err := store.PutArtifact(ctx, item.rec, bytes.NewReader(item.data)); err != nil {
			t.Fatalf("PutArtifact %s: %v", item.rec.LogicalName, err)
		}
	}

	pub := PublicationRecord{
		SchemaVersion: PublicationSchemaV1,
		Module:        modPath,
		Version:       version,
		Commit:        commitHash.String(),
		LoomRef:       loomRef,
		InfoDigest:    infoDgst,
		ModDigest:     modDgst,
		ZipDigest:     zipDgst,
		GoSumH1:       "h1:1234567890abcdef=",
	}
	if _, err := store.PublishModule(ctx, pub); err != nil {
		t.Fatalf("PublishModule: %v", err)
	}
	return pub
}

func TestPortableBundleExportImportLoomAndMaterializeRoundTrip(t *testing.T) {
	ctx := context.Background()
	t.Setenv("BASHY_OFFLINE", "")

	srcRoot := filepath.Join(t.TempDir(), "src-artifacts")
	srcLoom := filepath.Join(srcRoot, "loom", "modules.git")

	store, err := OpenStore(ctx, StoreOptions{Root: srcRoot})
	if err != nil {
		t.Fatalf("OpenStore src: %v", err)
	}
	prof := SelfHostProfileV1(CurrentPlatform())
	pub := seedTestLoomPublication(t, ctx, store, srcLoom, "example.com/portable/lib", "v0.1.0")
	m1, digests := seedSelfHostManifest(t, ctx, store, 1, prof, "gen1")

	// Evict reconstructible .zip before export to prove Loom-backed recovery survives bundle export/import!
	pruneItems, err := store.DryRunPrune(ctx, 0)
	if err != nil || len(pruneItems) != 1 || pruneItems[0].ID != pub.ZipDigest {
		t.Fatalf("DryRunPrune = (%+v, %v), want zip %s", pruneItems, err, pub.ZipDigest)
	}
	if _, err := store.ApplyPrune(ctx, pruneItems); err != nil {
		t.Fatalf("ApplyPrune: %v", err)
	}
	_ = store.Close()

	if _, err := Seal(ctx, SealOptions{
		Root:         srcRoot,
		LoomDir:      srcLoom,
		Profile:      &prof,
		DiskFreeFunc: func(string) (uint64, error) { return 50 << 30, nil },
	}); err != nil {
		t.Fatalf("Seal src: %v", err)
	}

	bundlePath := filepath.Join(t.TempDir(), "recovery-bundle.tar")
	expRes, err := ExportBundle(ctx, ExportOptions{
		Root:       srcRoot,
		LoomDir:    srcLoom,
		OutputPath: bundlePath,
	})
	if err != nil {
		t.Fatalf("ExportBundle: %v", err)
	}
	if expRes.Generation != 1 || expRes.LoomRefCount != 1 || !ValidDigest(expRes.BundleSHA256) {
		t.Fatalf("unexpected ExportResult: %+v", expRes)
	}

	// Import into a completely clean destination directory with Seal: true and Materialize hook.
	dstRoot := filepath.Join(t.TempDir(), "dst-artifacts")
	dstLoom := filepath.Join(dstRoot, "loom", "modules.git")
	materialized := false

	impRes, err := ImportBundle(ctx, ImportOptions{
		Root:         dstRoot,
		LoomDir:      dstLoom,
		BundlePath:   bundlePath,
		Seal:         true,
		DiskFreeFunc: func(string) (uint64, error) { return 50 << 30, nil },
		Materialize: func(ctx context.Context, b Backend, m ManifestV1) error {
			data, err := b.FetchVerified(ctx, digests["gotoolchain"])
			if err != nil {
				return err
			}
			if string(data) != "go-sdk-gen1" {
				t.Fatalf("materialized toolchain payload = %q", string(data))
			}
			materialized = true
			return nil
		},
	})
	if err != nil {
		t.Fatalf("ImportBundle clean: %v", err)
	}
	if !materialized || !impRes.Sealed || impRes.ImportedGeneration != 1 || impRes.BaselineGeneration != 1 || impRes.Publications != 1 {
		t.Fatalf("unexpected ImportResult: %+v (materialized=%v)", impRes, materialized)
	}

	// Verify closure & Loom ref on clean destination.
	verRep, err := VerifyClosure(ctx, VerifyOptions{
		Root:          dstRoot,
		LoomDir:       dstLoom,
		VerifyDigests: true,
		DiskFreeFunc:  func(string) (uint64, error) { return 50 << 30, nil },
	})
	if err != nil || !verRep.Complete || len(verRep.Missing) != 0 {
		t.Fatalf("VerifyClosure after import = (%+v, %v)", verRep, err)
	}
	_ = m1

	// Re-importing bundle on top of sealed gen 1 must assign additive generation 2 while keeping BaselineGeneration == 1!
	impRes2, err := ImportBundle(ctx, ImportOptions{
		Root:         dstRoot,
		LoomDir:      dstLoom,
		BundlePath:   bundlePath,
		Seal:         true,
		DiskFreeFunc: func(string) (uint64, error) { return 50 << 30, nil },
	})
	if err != nil {
		t.Fatalf("second ImportBundle: %v", err)
	}
	if impRes2.ImportedGeneration != 2 || impRes2.BaselineGeneration != 1 {
		t.Fatalf("additive ImportBundle = %+v, want ImportedGeneration=2 BaselineGeneration=1", impRes2)
	}
}

func writeCustomTar(t *testing.T, outPath string, writeFn func(tw *tar.Writer)) {
	t.Helper()
	f, err := os.Create(outPath)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(f)
	writeFn(tw)
	_ = tw.Close()
	_ = f.Close()
}

func TestImportBundleAdversarialArchivesLeaveActiveGenerationIntact(t *testing.T) {
	ctx := context.Background()
	t.Setenv("BASHY_OFFLINE", "")

	// Build a valid baseline store (sealed at generation 1) AND export a valid bundle to mutate.
	liveRoot := filepath.Join(t.TempDir(), "live-artifacts")
	store, err := OpenStore(ctx, StoreOptions{Root: liveRoot})
	if err != nil {
		t.Fatal(err)
	}
	prof := SelfHostProfileV1(CurrentPlatform())
	_, liveDigests := seedSelfHostManifest(t, ctx, store, 1, prof, "live-gen1")
	_ = store.Close()

	origSeal, err := Seal(ctx, SealOptions{
		Root:         liveRoot,
		Profile:      &prof,
		DiskFreeFunc: func(string) (uint64, error) { return 50 << 30, nil },
	})
	if err != nil {
		t.Fatalf("Seal live baseline: %v", err)
	}

	validBundle := filepath.Join(t.TempDir(), "valid.tar")
	if _, err := ExportBundle(ctx, ExportOptions{
		Root:       liveRoot,
		OutputPath: validBundle,
	}); err != nil {
		t.Fatalf("ExportBundle valid: %v", err)
	}
	validRaw, err := os.ReadFile(validBundle)
	if err != nil {
		t.Fatal(err)
	}

	// Read metadata from valid.tar for crafting tampered variants.
	var baseMeta BundleMetadataV1
	{
		tr := tar.NewReader(bytes.NewReader(validRaw))
		_, _ = tr.Next()
		mb, _ := io.ReadAll(tr)
		if err := json.Unmarshal(mb, &baseMeta); err != nil {
			t.Fatal(err)
		}
	}

	// 1. Tampered blob payload inside tar.
	tamperedTar := filepath.Join(t.TempDir(), "tampered.tar")
	writeCustomTar(t, tamperedTar, func(tw *tar.Writer) {
		tr := tar.NewReader(bytes.NewReader(validRaw))
		corruptedOne := false
		for {
			hdr, err := tr.Next()
			if err == io.EOF {
				break
			}
			body, _ := io.ReadAll(tr)
			if !corruptedOne && strings.HasPrefix(hdr.Name, "oci/blobs/sha256/") && len(body) > 0 {
				body[0] ^= 0xff
				corruptedOne = true
			}
			_ = tw.WriteHeader(hdr)
			_, _ = tw.Write(body)
		}
	})

	// 2. Path traversal (../evil).
	traversalTar := filepath.Join(t.TempDir(), "traversal.tar")
	writeCustomTar(t, traversalTar, func(tw *tar.Writer) {
		badMeta := baseMeta
		badMeta.Entries = []BundleTarEntry{{Path: "../evil", Size: 4, SHA256: strings.Repeat("a", 64)}}
		mb, _ := json.Marshal(badMeta)
		_ = tw.WriteHeader(&tar.Header{Name: bundleMetadataName, Typeflag: tar.TypeReg, Mode: 0o600, Size: int64(len(mb))})
		_, _ = tw.Write(mb)
		_ = tw.WriteHeader(&tar.Header{Name: "../evil", Typeflag: tar.TypeReg, Mode: 0o600, Size: 4})
		_, _ = tw.Write([]byte("evil"))
	})

	// 3. Escaping symlink.
	symlinkTar := filepath.Join(t.TempDir(), "symlink.tar")
	writeCustomTar(t, symlinkTar, func(tw *tar.Writer) {
		mb, _ := json.Marshal(baseMeta)
		_ = tw.WriteHeader(&tar.Header{Name: bundleMetadataName, Typeflag: tar.TypeReg, Mode: 0o600, Size: int64(len(mb))})
		_, _ = tw.Write(mb)
		_ = tw.WriteHeader(&tar.Header{Name: "oci/escape-link", Typeflag: tar.TypeSymlink, Linkname: "../../outside", Mode: 0o777})
	})

	// 4. Truncated tar archive (chopped mid-archive).
	truncatedTar := filepath.Join(t.TempDir(), "truncated.tar")
	if err := os.WriteFile(truncatedTar, validRaw[:len(validRaw)/2], 0o600); err != nil {
		t.Fatal(err)
	}

	// 5. Foreign platform mismatch (linux/amd64 vs darwin/arm64 or windows/amd64).
	foreignPlatform := "linux/amd64"
	if CurrentPlatform() == "linux/amd64" {
		foreignPlatform = "darwin/arm64"
	}

	cases := []struct {
		name           string
		bundle         string
		targetPlatform string
		wantErr        error
	}{
		{name: "tampered_blob_digest", bundle: tamperedTar, wantErr: ErrCorruptPayload},
		{name: "path_traversal", bundle: traversalTar},
		{name: "escaping_symlink", bundle: symlinkTar},
		{name: "truncated_tar", bundle: truncatedTar, wantErr: ErrCorruptPayload},
		{name: "wrong_platform", bundle: validBundle, targetPlatform: foreignPlatform, wantErr: ErrPlatformMismatch},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ImportBundle(ctx, ImportOptions{
				Root:           liveRoot,
				BundlePath:     tc.bundle,
				Seal:           true,
				TargetPlatform: tc.targetPlatform,
				DiskFreeFunc:   func(string) (uint64, error) { return 50 << 30, nil },
			})
			if err == nil {
				t.Fatalf("%s unexpectedly succeeded", tc.name)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("%s err = %v, want %v", tc.name, err, tc.wantErr)
			}

			// Verify existing ActiveGeneration, seal.json, and CAS blobs are 100% untouched!
			gotSeal, ok, sErr := LoadSealState(liveRoot)
			if sErr != nil || !ok || gotSeal.ActiveGeneration != 1 || gotSeal.BaselineGeneration != 1 || gotSeal.ManifestDigest != origSeal.ManifestDigest {
				t.Fatalf("after %s: seal.json mutated: got=%+v ok=%v err=%v", tc.name, gotSeal, ok, sErr)
			}
			roStore, oErr := OpenStore(ctx, StoreOptions{Root: liveRoot, ReadOnly: true})
			if oErr != nil {
				t.Fatalf("after %s: OpenStore read-only: %v", tc.name, oErr)
			}
			manifests, _ := roStore.ListManifests(ctx)
			if len(manifests) != 1 || manifests[0].Generation != 1 {
				_ = roStore.Close()
				t.Fatalf("after %s: manifests mutated: %+v", tc.name, manifests)
			}
			if _, fErr := roStore.FetchVerified(ctx, liveDigests["source"]); fErr != nil {
				_ = roStore.Close()
				t.Fatalf("after %s: live source blob corrupted: %v", tc.name, fErr)
			}
			_ = roStore.Close()
		})
	}
}

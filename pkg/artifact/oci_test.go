// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package artifact_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/qiangli/yoke/pkg/artifact"
	"github.com/qiangli/yoke/pkg/prune"
)

type syntheticOCIImage struct {
	layoutDir      string
	archivePath    string
	manifestDigest string
	configDigest   string
	layerDigests   []string
}

func makeLayerTarGzip(t *testing.T, files map[string]string) ([]byte, string) {
	t.Helper()
	var rawTar bytes.Buffer
	tw := tar.NewWriter(&rawTar)
	fixedTime := time.Unix(0, 0).UTC()
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{
			Name:     name,
			Typeflag: tar.TypeReg,
			Mode:     0o644,
			Size:     int64(len(content)),
			ModTime:  fixedTime,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	diffID := artifact.DigestBytes(rawTar.Bytes())

	var gzBuf bytes.Buffer
	gw := gzip.NewWriter(&gzBuf)
	gw.ModTime = fixedTime
	if _, err := gw.Write(rawTar.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return gzBuf.Bytes(), diffID
}

func buildSyntheticOCIImage(t *testing.T, refName, arch string, layerFiles []map[string]string, extraLabel string) syntheticOCIImage {
	t.Helper()
	layoutDir := filepath.Join(t.TempDir(), "oci-layout-src")
	blobsDir := filepath.Join(layoutDir, "blobs", "sha256")
	if err := os.MkdirAll(blobsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	writeBlob := func(data []byte) string {
		dgst := artifact.DigestBytes(data)
		hexPart := strings.TrimPrefix(dgst, "sha256:")
		if err := os.WriteFile(filepath.Join(blobsDir, hexPart), data, 0o644); err != nil {
			t.Fatal(err)
		}
		return dgst
	}

	var layerDescs []ocispec.Descriptor
	var layerDigests []string
	var diffIDs []digest.Digest
	for _, lf := range layerFiles {
		gzBytes, diffID := makeLayerTarGzip(t, lf)
		lDigest := writeBlob(gzBytes)
		layerDigests = append(layerDigests, lDigest)
		diffIDs = append(diffIDs, digest.Digest(diffID))
		layerDescs = append(layerDescs, ocispec.Descriptor{
			MediaType: ocispec.MediaTypeImageLayerGzip,
			Digest:    digest.Digest(lDigest),
			Size:      int64(len(gzBytes)),
		})
	}

	imgCfg := ocispec.Image{
		Platform: ocispec.Platform{
			OS:           "linux",
			Architecture: arch,
		},
		Config: ocispec.ImageConfig{
			Entrypoint: []string{"/bashy"},
			WorkingDir: "/work",
			Labels: map[string]string{
				"io.dhnt.test.extra": extraLabel,
			},
		},
		RootFS: ocispec.RootFS{
			Type:    "layers",
			DiffIDs: diffIDs,
		},
	}
	cfgBytes, err := json.Marshal(imgCfg)
	if err != nil {
		t.Fatal(err)
	}
	cfgDigest := writeBlob(cfgBytes)

	manifest := ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Config: ocispec.Descriptor{
			MediaType: ocispec.MediaTypeImageConfig,
			Digest:    digest.Digest(cfgDigest),
			Size:      int64(len(cfgBytes)),
		},
		Layers: layerDescs,
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	manifestDigest := writeBlob(manifestBytes)

	layoutJSON, _ := json.Marshal(ocispec.ImageLayout{Version: ocispec.ImageLayoutVersion})
	if err := os.WriteFile(filepath.Join(layoutDir, ocispec.ImageLayoutFile), layoutJSON, 0o644); err != nil {
		t.Fatal(err)
	}

	annotations := map[string]string{}
	if refName != "" {
		annotations[ocispec.AnnotationRefName] = refName
	}
	idx := ocispec.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{
			{
				MediaType:   ocispec.MediaTypeImageManifest,
				Digest:      digest.Digest(manifestDigest),
				Size:        int64(len(manifestBytes)),
				Annotations: annotations,
			},
		},
	}
	idxBytes, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(layoutDir, ocispec.ImageIndexFile), idxBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	// Also pack into an oci-archive .tar file.
	archivePath := filepath.Join(t.TempDir(), "image.oci.tar")
	outFile, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(outFile)
	err = filepath.Walk(layoutDir, func(p string, info os.FileInfo, walkErr error) error {
		if walkErr != nil || info.IsDir() {
			return walkErr
		}
		rel, err := filepath.Rel(layoutDir, p)
		if err != nil {
			return err
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(&tar.Header{
			Name:     filepath.ToSlash(rel),
			Typeflag: tar.TypeReg,
			Mode:     0o644,
			Size:     int64(len(raw)),
			ModTime:  time.Unix(0, 0).UTC(),
		}); err != nil {
			return err
		}
		_, err = tw.Write(raw)
		return err
	})
	if err != nil {
		_ = outFile.Close()
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		_ = outFile.Close()
		t.Fatal(err)
	}
	if err := outFile.Close(); err != nil {
		t.Fatal(err)
	}

	return syntheticOCIImage{
		layoutDir:      layoutDir,
		archivePath:    archivePath,
		manifestDigest: manifestDigest,
		configDigest:   cfgDigest,
		layerDigests:   layerDigests,
	}
}

func TestOCIIngestExportMultiLayerRoundTrip(t *testing.T) {
	ctx := context.Background()
	storeRoot := t.TempDir()
	store, err := artifact.OpenStore(ctx, artifact.StoreOptions{Root: storeRoot})
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	synth := buildSyntheticOCIImage(t, "localhost/bashy-base:v1.0.0-rc.40", "amd64", []map[string]string{
		{"etc/passwd": "root:x:0:0:root:/tmp/bashy:/bashy\n", "bashy": "static-bashy-elf"},
		{"opt/bashy/toolchain.txt": "go1.27.4-locked\n"},
		{"work/README.md": "offline-sandbox-layer-3\n"},
	}, "baseline")

	ingested, err := artifact.IngestOCIArchive(ctx, store, synth.archivePath, artifact.OCIIngestOptions{
		LogicalName: "bashy-base",
		Version:     "v1.0.0-rc.40",
	})
	if err != nil {
		t.Fatalf("IngestOCIArchive: %v", err)
	}

	if ingested.ManifestDigest != synth.manifestDigest {
		t.Fatalf("ManifestDigest = %s, want %s", ingested.ManifestDigest, synth.manifestDigest)
	}
	if ingested.ConfigDigest != synth.configDigest {
		t.Fatalf("ConfigDigest = %s, want %s", ingested.ConfigDigest, synth.configDigest)
	}
	if len(ingested.LayerDigests) != 3 {
		t.Fatalf("LayerDigests = %v, want 3 layers", ingested.LayerDigests)
	}
	if ingested.Root.Kind != "oci-image" || ingested.Root.Name != "oci/bashy-base" || ingested.Root.Platform != "linux/amd64" {
		t.Fatalf("unexpected RootDescriptor: %+v", ingested.Root)
	}

	// Verify manifest, config, and every layer are stored as RetentionProtectedRoot in CAS.
	for _, dgst := range append([]string{synth.manifestDigest, synth.configDigest}, synth.layerDigests...) {
		st, err := store.GetRecordStatus(ctx, dgst)
		if err != nil {
			t.Fatalf("GetRecordStatus(%s): %v", dgst, err)
		}
		if !st.PayloadPresent || st.Record.Retention != artifact.RetentionProtectedRoot {
			t.Fatalf("blob %s status = (present=%t, retention=%s), want protected present",
				dgst, st.PayloadPresent, st.Record.Retention)
		}
	}

	// Simulate Podman engine storage wipe by deleting original layout & archive.
	if err := os.RemoveAll(synth.layoutDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(synth.archivePath); err != nil {
		t.Fatal(err)
	}

	// Export oci-archive directly from CAS by pinned repo@digest reference.
	restoredTar := filepath.Join(t.TempDir(), "restored-bashy-base.oci.tar")
	expRes, err := artifact.ExportOCIArchive(ctx, store, "localhost/bashy-base@"+synth.manifestDigest, restoredTar)
	if err != nil {
		t.Fatalf("ExportOCIArchive: %v", err)
	}
	if expRes.ManifestDigest != synth.manifestDigest {
		t.Fatalf("exported ManifestDigest = %s, want %s", expRes.ManifestDigest, synth.manifestDigest)
	}

	// Re-ingest the exported tar into a brand-new verification store to prove the
	// reconstructed oci-archive is self-contained and has identical manifest + layer digests!
	verifyStore, err := artifact.OpenStore(ctx, artifact.StoreOptions{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer verifyStore.Close()

	reIngested, err := artifact.IngestOCIArchive(ctx, verifyStore, restoredTar, artifact.OCIIngestOptions{
		LogicalName: "bashy-base-restored",
	})
	if err != nil {
		t.Fatalf("re-ingest exported archive: %v", err)
	}
	if reIngested.ManifestDigest != synth.manifestDigest || reIngested.ConfigDigest != synth.configDigest {
		t.Fatalf("re-ingested digests (%s, %s) != (%s, %s)",
			reIngested.ManifestDigest, reIngested.ConfigDigest, synth.manifestDigest, synth.configDigest)
	}
	for i := range synth.layerDigests {
		if reIngested.LayerDigests[i] != synth.layerDigests[i] {
			t.Fatalf("layer[%d] digest = %s, want %s", i, reIngested.LayerDigests[i], synth.layerDigests[i])
		}
	}
}

func TestOCIPrunePreservesBaselineHelpersAptAndActiveReadLease(t *testing.T) {
	ctx := context.Background()
	store, err := artifact.OpenStore(ctx, artifact.StoreOptions{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// 1. Ingest protected baseline OCI image.
	baseImg := buildSyntheticOCIImage(t, "localhost/bashy-build:v1.0.0-rc.40", "arm64", []map[string]string{
		{"bashy": "protected-bashy"},
		{"usr/local/go/bin/go": "protected-go"},
	}, "build-baseline")
	baseRes, err := artifact.IngestOCIArchive(ctx, store, baseImg.archivePath, artifact.OCIIngestOptions{
		LogicalName: "bashy-build",
		Version:     "v1.0.0-rc.40",
	})
	if err != nil {
		t.Fatalf("IngestOCIArchive base: %v", err)
	}

	// 2. Capture Podman VM helpers + apt snapshot archive as protected roots.
	helperSrc := t.TempDir()
	gvproxyPath := filepath.Join(helperSrc, "gvproxy")
	vfkitPath := filepath.Join(helperSrc, "vfkit")
	_ = os.WriteFile(gvproxyPath, []byte("gvproxy-binary"), 0o755)
	_ = os.WriteFile(vfkitPath, []byte("vfkit-binary"), 0o755)

	aptSrc := t.TempDir()
	_ = os.WriteFile(filepath.Join(aptSrc, "Packages.gz"), []byte("apt-packages-index"), 0o644)
	_ = os.WriteFile(filepath.Join(aptSrc, "build-essential_12.10_arm64.deb"), []byte("deb-payload"), 0o644)

	helperRes, err := artifact.CaptureVMAndHelperAssets(ctx, store, artifact.HelperCaptureOptions{
		Platform: "darwin/arm64",
		Version:  "v6.1.2",
		HelperPaths: map[string]string{
			"gvproxy": gvproxyPath,
			"vfkit":   vfkitPath,
		},
		AptArchivePath: aptSrc,
		AptSnapshot:    "20260821T190000Z",
	})
	if err != nil {
		t.Fatalf("CaptureVMAndHelperAssets: %v", err)
	}
	if helperRes.HelperRoot == nil || helperRes.AptRoot == nil {
		t.Fatalf("expected both HelperRoot and AptRoot, got %+v", helperRes)
	}

	// 3. Also register two disposable scratch images:
	//    - scratchIdle: unleased -> SHOULD be pruned
	//    - scratchLeased: actively being exported via ExportOCIArchiveWithOptions -> MUST NOT be pruned!
	scratchIdle := buildSyntheticOCIImage(t, "localhost/sandbox-idle:tmp", "arm64", []map[string]string{
		{"work/idle.txt": "disposable-idle-layer"},
	}, "idle")
	idleRes, err := artifact.RegisterGeneratedOCIImage(ctx, store, artifact.GeneratedOCIImageOptions{
		ArchivePath:   scratchIdle.archivePath,
		LogicalName:   "sandbox-idle",
		Deterministic: false,
		Retention:     artifact.RetentionDisposable,
	})
	if err != nil {
		t.Fatalf("RegisterGeneratedOCIImage idle: %v", err)
	}

	scratchLeased := buildSyntheticOCIImage(t, "localhost/sandbox-leased:tmp", "arm64", []map[string]string{
		{"work/leased.txt": "disposable-leased-layer"},
	}, "leased")
	leasedRes, err := artifact.RegisterGeneratedOCIImage(ctx, store, artifact.GeneratedOCIImageOptions{
		ArchivePath:   scratchLeased.archivePath,
		LogicalName:   "sandbox-leased",
		Deterministic: false,
		Retention:     artifact.RetentionDisposable,
	})
	if err != nil {
		t.Fatalf("RegisterGeneratedOCIImage leased: %v", err)
	}

	// Gather pre-lease candidate list that includes scratchLeased blobs AND try to force-pass
	// protected baseline/helper/apt items into ApplyPrune to verify ApplyPrune re-checks everything!
	preItems, err := store.DryRunPrune(ctx, 0)
	if err != nil {
		t.Fatalf("DryRunPrune: %v", err)
	}
	forcedItems := append([]prune.Item{
		{Noun: "artifact", ID: baseRes.ManifestDigest},
		{Noun: "artifact", ID: baseRes.LayerDigests[0]},
		{Noun: "artifact", ID: helperRes.HelperRoot.Digest},
		{Noun: "artifact", ID: helperRes.AptRoot.Digest},
	}, preItems...)

	var pruneCountsDuringExport prune.Counts
	exportedLeasedTar := filepath.Join(t.TempDir(), "leased-export.oci.tar")
	_, err = artifact.ExportOCIArchiveWithOptions(ctx, store, artifact.OCIExportOptions{
		RefOrDigest: leasedRes.ManifestDigest,
		OutputPath:  exportedLeasedTar,
		OnLeaseAcquired: func(leaseCtx context.Context) error {
			// Run ApplyPrune right while ExportOCIArchive holds the active read lease!
			var pErr error
			pruneCountsDuringExport, pErr = store.ApplyPrune(leaseCtx, forcedItems)
			return pErr
		},
	})
	if err != nil {
		t.Fatalf("ExportOCIArchiveWithOptions with concurrent ApplyPrune: %v", err)
	}

	// scratchIdle (manifest + config + 1 layer = 3 blobs) should be evicted.
	if pruneCountsDuringExport.Removed != 3 {
		t.Fatalf("ApplyPrune removed %d blobs during active lease, want 3 (only unleased scratchIdle)", pruneCountsDuringExport.Removed)
	}
	idleSt, err := store.GetRecordStatus(ctx, idleRes.ManifestDigest)
	if err != nil || idleSt.PayloadPresent {
		t.Fatalf("expected unleased scratchIdle manifest evicted, got st=%+v err=%v", idleSt, err)
	}

	// Protected baseline OCI image, podman-vm helpers, apt-archive, AND leased scratch image must all remain intact!
	for _, mustExist := range []string{
		baseRes.ManifestDigest,
		baseRes.ConfigDigest,
		baseRes.LayerDigests[0],
		baseRes.LayerDigests[1],
		helperRes.HelperRoot.Digest,
		helperRes.AptRoot.Digest,
		leasedRes.ManifestDigest,
		leasedRes.ConfigDigest,
		leasedRes.LayerDigests[0],
	} {
		st, err := store.GetRecordStatus(ctx, mustExist)
		if err != nil || !st.PayloadPresent {
			t.Fatalf("blob %s was unexpectedly evicted (st=%+v, err=%v)", mustExist, st, err)
		}
	}

	// Verify RestoreVMAndHelperAssets restores both helpers and apt archive from CAS cleanly.
	restoreHelpersDir := filepath.Join(t.TempDir(), "restored-helpers")
	restoreAptDir := filepath.Join(t.TempDir(), "restored-apt")
	restoredAssets, err := artifact.RestoreVMAndHelperAssets(ctx, store, artifact.HelperRestoreOptions{
		Version:       "v6.1.2",
		HelperDestDir: restoreHelpersDir,
		AptSnapshot:   "20260821T190000Z",
		AptDestDir:    restoreAptDir,
	})
	if err != nil {
		t.Fatalf("RestoreVMAndHelperAssets: %v", err)
	}
	if len(restoredAssets.HelperFiles) != 2 || len(restoredAssets.AptFiles) != 2 {
		t.Fatalf("unexpected restored asset counts: %+v", restoredAssets)
	}
}

func TestOCIGeneratedNondeterministicVsReproduciblePolicy(t *testing.T) {
	ctx := context.Background()
	store, err := artifact.OpenStore(ctx, artifact.StoreOptions{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	baseImg := buildSyntheticOCIImage(t, "localhost/bashy-base:v1.0.0", "amd64", []map[string]string{
		{"bashy": "base-layer"},
	}, "base")
	baseRes, err := artifact.IngestOCIArchive(ctx, store, baseImg.archivePath, artifact.OCIIngestOptions{
		LogicalName: "bashy-base",
		Version:     "v1.0.0",
	})
	if err != nil {
		t.Fatal(err)
	}

	ctxBytes := []byte("FROM localhost/bashy-base:v1.0.0\nCOPY app /app\n")
	ctxDigest := artifact.DigestBytes(ctxBytes)
	if _, err := store.PutArtifact(ctx, artifact.CatalogRecordV1{
		SchemaVersion:  artifact.CatalogRecordSchemaV1,
		Digest:         ctxDigest,
		Size:           int64(len(ctxBytes)),
		MediaType:      "text/plain; charset=utf-8",
		LogicalName:    "oci-context/my-sandbox",
		LogicalVersion: "v1",
		Provenance:     "test",
		Retention:      artifact.RetentionProtectedRoot,
		RetentionRoots: []string{"context:my-sandbox"},
	}, bytes.NewReader(ctxBytes)); err != nil {
		t.Fatal(err)
	}

	genBuild1 := buildSyntheticOCIImage(t, "localhost/my-sandbox:build", "amd64", []map[string]string{
		{"bashy": "base-layer"},
		{"app": "build-timestamp-1"},
	}, "ts-1")

	// 1. Nondeterministic image with RetentionReconstructible MUST fail with ErrNondeterministicImageEviction!
	_, err = artifact.RegisterGeneratedOCIImage(ctx, store, artifact.GeneratedOCIImageOptions{
		ArchivePath:    genBuild1.archivePath,
		LogicalName:    "my-sandbox",
		Deterministic:  false,
		ExpectedDigest: genBuild1.manifestDigest,
		InputDigests:   []string{baseRes.ManifestDigest, ctxDigest},
		Retention:      artifact.RetentionReconstructible,
	})
	if !errors.Is(err, artifact.ErrNondeterministicImageEviction) {
		t.Fatalf("Deterministic=false + RetentionReconstructible err = %v, want ErrNondeterministicImageEviction", err)
	}

	// 2. Nondeterministic image with RetentionProtectedRoot retains all layers safely without a recipe.
	protGen, err := artifact.RegisterGeneratedOCIImage(ctx, store, artifact.GeneratedOCIImageOptions{
		ArchivePath:   genBuild1.archivePath,
		LogicalName:   "my-sandbox-protected",
		Version:       "v1",
		Deterministic: false,
		Retention:     artifact.RetentionProtectedRoot,
	})
	if err != nil {
		t.Fatalf("RegisterGeneratedOCIImage protected: %v", err)
	}
	protRec, err := store.GetRecord(ctx, protGen.ManifestDigest)
	if err != nil || protRec.Retention != artifact.RetentionProtectedRoot || protRec.Recipe != nil {
		t.Fatalf("unexpected protected generated record: %+v (err=%v)", protRec, err)
	}

	// 3. Nondeterministic disposable scratch rebuilds issue a NEW digest identity without claiming old digest recovery.
	scratch1 := buildSyntheticOCIImage(t, "localhost/my-scratch:latest", "amd64", []map[string]string{
		{"tmp/run": "run-1-at-t1"},
	}, "run-1")
	sRes1, err := artifact.RegisterGeneratedOCIImage(ctx, store, artifact.GeneratedOCIImageOptions{
		ArchivePath:   scratch1.archivePath,
		LogicalName:   "my-scratch",
		Deterministic: false,
		Retention:     artifact.RetentionDisposable,
	})
	if err != nil {
		t.Fatalf("scratch build 1: %v", err)
	}
	// Evict scratch build 1 via prune.
	items, err := store.DryRunPrune(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApplyPrune(ctx, items); err != nil {
		t.Fatal(err)
	}
	// Attempting to export old evicted scratch digest fails cleanly (never claims deterministic recovery).
	if _, err := artifact.ExportOCIArchive(ctx, store, sRes1.ManifestDigest, filepath.Join(t.TempDir(), "out.tar")); !errors.Is(err, artifact.ErrPayloadEvicted) {
		t.Fatalf("export evicted nondeterministic scratch = %v, want ErrPayloadEvicted", err)
	}

	// Rebuild scratch image -> produces new digest identity cleanly!
	scratch2 := buildSyntheticOCIImage(t, "localhost/my-scratch:latest", "amd64", []map[string]string{
		{"tmp/run": "run-2-at-t2"},
	}, "run-2")
	sRes2, err := artifact.RegisterGeneratedOCIImage(ctx, store, artifact.GeneratedOCIImageOptions{
		ArchivePath:   scratch2.archivePath,
		LogicalName:   "my-scratch",
		Deterministic: false,
		Retention:     artifact.RetentionDisposable,
	})
	if err != nil {
		t.Fatalf("scratch build 2: %v", err)
	}
	if sRes2.ManifestDigest == sRes1.ManifestDigest || sRes2.Root.Version == sRes1.Root.Version {
		t.Fatalf("expected new digest identity on nondeterministic scratch rebuild, got %s vs %s",
			sRes2.ManifestDigest, sRes1.ManifestDigest)
	}

	// 4. Deterministic image with all retained protected inputs succeeds as RetentionReconstructible (RecipeOCIDerivedV1).
	detImg := buildSyntheticOCIImage(t, "localhost/my-det:v1", "amd64", []map[string]string{
		{"bashy": "base-layer"},
		{"det": "canonical-epoch-0"},
	}, "deterministic")
	detRes, err := artifact.RegisterGeneratedOCIImage(ctx, store, artifact.GeneratedOCIImageOptions{
		ArchivePath:    detImg.archivePath,
		LogicalName:    "my-det",
		Version:        "v1",
		Deterministic:  true,
		ExpectedDigest: detImg.manifestDigest,
		InputDigests:   []string{baseRes.ManifestDigest, ctxDigest},
		Retention:      artifact.RetentionReconstructible,
	})
	if err != nil {
		t.Fatalf("RegisterGeneratedOCIImage deterministic: %v", err)
	}
	detRec, err := store.GetRecord(ctx, detRes.ManifestDigest)
	if err != nil || detRec.Retention != artifact.RetentionReconstructible || detRec.Recipe == nil || detRec.Recipe.Kind != artifact.RecipeOCIDerivedV1 {
		t.Fatalf("unexpected deterministic record: %+v (err=%v)", detRec, err)
	}
	// Shared base layer must remain RetentionProtectedRoot (never downgraded by derived image)!
	sharedLayerSt, err := store.GetRecordStatus(ctx, baseRes.LayerDigests[0])
	if err != nil || sharedLayerSt.Record.Retention != artifact.RetentionProtectedRoot {
		t.Fatalf("shared base layer retention = %+v, want RetentionProtectedRoot", sharedLayerSt)
	}
}

func TestParseBuildImageLockAndCheckSandboxPackAllowed(t *testing.T) {
	lockContent := strings.Join([]string{
		"# Official build image lock fixture",
		"bashy_version=v1.0.0-rc.40",
		"bashy_sha256=43c33b1022f22e407f3d40bcb4b47b693836540ade9c6ed1307fc14406b14720",
		"previous_version=v1.0.0-rc.40",
		"built_with_digest=localhost/bashy-build:v1.0.0-rc.40-go1.27.4-f9db3280fcdf@sha256:f9db3280fcdf20084ad6297b989b913091c820648a7615b7f2d3fa1e79fb0b3a",
		"apt_snapshot=20260821T190000Z",
		"go_version=1.27.4",
		"go_sha256_amd64=7226736c2339719cda28fbf3a54d71aa63bcefe376e98cef00034268dab24840",
		"go_sha256_arm64=3392582988a273a87ab5043ee1b2c9c17a2c3c00580bf9326c47a2e0aaaeac92",
		"base_image=localhost/bashy-base:v1.0.0-rc.40",
		"base_digest=sha256:cfd66860c047f167a62b32627e576cf65c277c01686e23c809473b6593de02d5",
		"image=localhost/bashy-build:v1.0.0-rc.40-go1.27.4-1171628e07d3",
		"image_digest=sha256:1171628e07d3e7490458d9b0b484e689fee56c4a739e3f8ad577188eceece2ff",
		"source_bashy_commit=c379daea083e6aa7b05fe06135099ac0148c384a",
		"source_coreutils_commit=c5832f99ccf69ced4dc2e64caa85f0cab5d46632",
	}, "\n") + "\n"

	lockPath := filepath.Join(t.TempDir(), "image.lock")
	if err := os.WriteFile(lockPath, []byte(lockContent), 0o644); err != nil {
		t.Fatal(err)
	}

	lock, err := artifact.ParseBuildImageLock(lockPath)
	if err != nil {
		t.Fatalf("ParseBuildImageLock: %v", err)
	}
	if lock.BashyVersion != "v1.0.0-rc.40" || lock.GoVersion != "1.27.4" || lock.AptSnapshot != "20260821T190000Z" {
		t.Fatalf("unexpected parsed lock fields: %+v", lock)
	}
	if lock.BaseDigest != "sha256:cfd66860c047f167a62b32627e576cf65c277c01686e23c809473b6593de02d5" ||
		lock.ImageDigest != "sha256:1171628e07d3e7490458d9b0b484e689fee56c4a739e3f8ad577188eceece2ff" {
		t.Fatalf("unexpected parsed image digests: %+v", lock)
	}
	if lock.SourceCommits["bashy"] != "c379daea083e6aa7b05fe06135099ac0148c384a" {
		t.Fatalf("unexpected source commits: %+v", lock.SourceCommits)
	}

	// CheckSandboxPackAllowed enforcement:
	prof := artifact.SelfHostProfileV1("linux/amd64")
	for _, allowed := range []string{"go", "bashsharp"} {
		if err := artifact.CheckSandboxPackAllowed(prof, true, allowed); err != nil {
			t.Fatalf("mandatory pack %q refused: %v", allowed, err)
		}
	}
	for _, denied := range []string{"python", "rust"} {
		err := artifact.CheckSandboxPackAllowed(prof, true, denied)
		if !errors.Is(err, artifact.ErrSealedNetworkDenied) {
			t.Fatalf("unconfigured pack %q in sealed mode = %v, want ErrSealedNetworkDenied", denied, err)
		}
		if !strings.Contains(err.Error(), "import a verified pack instead of fetching online") {
			t.Fatalf("unexpected error guidance: %v", err)
		}
	}
	// Unsealed mode allows on-demand acquisition.
	if err := artifact.CheckSandboxPackAllowed(prof, false, "python"); err != nil {
		t.Fatalf("unsealed CheckSandboxPackAllowed(python) = %v, want nil", err)
	}
	// Configured optional python pack is allowed in sealed mode and captured/verified in CAS.
	profWithPy := prof
	profWithPy.Languages = []string{"go", "bashsharp", "python"}
	if err := artifact.CheckSandboxPackAllowed(profWithPy, true, "python"); err != nil {
		t.Fatalf("configured python pack in sealed mode = %v, want nil", err)
	}

	ctx := context.Background()
	store, err := artifact.OpenStore(ctx, artifact.StoreOptions{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	lpRoots, err := artifact.CaptureLanguagePacks(ctx, store, artifact.LanguagePackCaptureOptions{
		Profile:  profWithPy,
		Version:  "gen-1",
		Platform: "linux/amd64",
	})
	if err != nil {
		t.Fatalf("CaptureLanguagePacks: %v", err)
	}
	if len(lpRoots) != 3 {
		t.Fatalf("CaptureLanguagePacks roots = %d, want 3 (go, bashsharp, python)", len(lpRoots))
	}
	verified, missing, err := artifact.VerifyLanguagePacks(ctx, store, profWithPy, "gen-1")
	if err != nil || len(missing) != 0 || len(verified) != 3 {
		t.Fatalf("VerifyLanguagePacks = (%v, %v, %v), want 3 verified and 0 missing", verified, missing, err)
	}
}

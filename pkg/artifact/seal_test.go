// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package artifact

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func putTestRoot(t *testing.T, ctx context.Context, store *Store, kind, name, ver string, payload []byte, deps ...string) RootDescriptor {
	t.Helper()
	dgst := DigestBytes(payload)
	rec := CatalogRecordV1{
		SchemaVersion:  CatalogRecordSchemaV1,
		Digest:         dgst,
		Size:           int64(len(payload)),
		MediaType:      "application/octet-stream",
		LogicalName:    name,
		LogicalVersion: ver,
		Dependencies:   deps,
		Provenance:     "test-bootstrap",
		Retention:      RetentionProtectedRoot,
		RetentionRoots: []string{"test:" + kind + ":" + name},
	}
	if _, err := store.PutArtifact(ctx, rec, bytes.NewReader(payload)); err != nil {
		t.Fatalf("PutArtifact(%s): %v", name, err)
	}
	return RootDescriptor{
		Kind:      kind,
		Name:      name,
		Version:   ver,
		MediaType: "application/octet-stream",
		Digest:    dgst,
		Size:      int64(len(payload)),
		Deps:      deps,
	}
}

func seedSelfHostManifest(t *testing.T, ctx context.Context, store *Store, gen uint64, prof ProfileV1, suffix string) (ManifestV1, map[string]string) {
	t.Helper()
	profBytes, err := prof.CanonicalBytes()
	if err != nil {
		t.Fatalf("CanonicalBytes: %v", err)
	}
	profDigest := DigestBytes(profBytes)
	if _, err := store.PutArtifact(ctx, CatalogRecordV1{
		SchemaVersion:  CatalogRecordSchemaV1,
		Digest:         profDigest,
		Size:           int64(len(profBytes)),
		MediaType:      "application/json",
		LogicalName:    "profile/" + prof.Name,
		LogicalVersion: suffix,
		Provenance:     "test-bootstrap",
		Retention:      RetentionProtectedRoot,
		RetentionRoots: []string{"test:profile"},
	}, bytes.NewReader(profBytes)); err != nil {
		t.Fatalf("PutArtifact profile: %v", err)
	}

	tcRoot := putTestRoot(t, ctx, store, "gotoolchain", "gotoolchain/go", "1.27.4", []byte("go-sdk-"+suffix))
	ovRoot := putTestRoot(t, ctx, store, "gooverlay", "gooverlay/productgo", "1.27.4", []byte("overlay-"+suffix))
	srcRoot := putTestRoot(t, ctx, store, "source", "source/bashy", suffix, []byte("source-"+suffix))
	modRoot := putTestRoot(t, ctx, store, "gomod", "gomod-zip/example.com/dep", "v1.0.0", []byte("modzip-"+suffix))
	lpGo := putTestRoot(t, ctx, store, "lang-pack", "go", suffix, []byte("lp-go-"+suffix), tcRoot.Digest, ovRoot.Digest)
	lpBsh := putTestRoot(t, ctx, store, "lang-pack", "bashsharp", suffix, []byte("lp-bsh-"+suffix), srcRoot.Digest)

	m := ManifestV1{
		SchemaVersion:       ManifestSchemaV1,
		Generation:          gen,
		CreatedAt:           "2026-11-26T12:00:00Z",
		Platform:            prof.Platform,
		ProfileName:         prof.Name,
		ProfileDigest:       profDigest,
		GoToolchainVersion:  "1.27.4",
		ProductOverlaySHA:   ovRoot.Digest,
		RebuildReserveBytes: prof.Capacity.RebuildReserveBytes,
		Roots:               []RootDescriptor{tcRoot, ovRoot, srcRoot, modRoot, lpGo, lpBsh},
	}
	if err := store.PutManifest(ctx, m); err != nil {
		t.Fatalf("PutManifest gen %d: %v", gen, err)
	}
	return m, map[string]string{
		"gotoolchain": tcRoot.Digest,
		"source":      srcRoot.Digest,
		"overlay":     ovRoot.Digest,
	}
}

func TestSelfHostProfileAndCapacityReserve(t *testing.T) {
	platform := CurrentPlatform()
	selfProf := SelfHostProfileV1(platform)
	if err := selfProf.Validate(); err != nil {
		t.Fatalf("SelfHostProfileV1.Validate: %v", err)
	}
	defProf := DefaultProfileV1(platform)
	if err := defProf.Validate(); err != nil {
		t.Fatalf("DefaultProfileV1.Validate: %v", err)
	}

	// Live OS DiskFreeBytes check on nonexistent child path walks up to TempDir.
	nonexistent := filepath.Join(t.TempDir(), "nested", "future", "artifacts")
	free, err := DiskFreeBytes(nonexistent)
	if err != nil || free == 0 {
		t.Fatalf("DiskFreeBytes(%s) = (%d, %v), want >0", nonexistent, free, err)
	}

	// Simulated capacity shortfall fails with ErrInsufficientCapacity.
	_, err = CheckCapacityReserve(nonexistent, 500<<20, 2<<30, func(string) (uint64, error) {
		return 1 << 30, nil
	})
	if !errors.Is(err, ErrInsufficientCapacity) {
		t.Fatalf("CheckCapacityReserve shortfall = %v, want ErrInsufficientCapacity", err)
	}

	// Sufficient capacity succeeds.
	gotFree, err := CheckCapacityReserve(nonexistent, 500<<20, 2<<30, func(string) (uint64, error) {
		return 10 << 30, nil
	})
	if err != nil || gotFree != 10<<30 {
		t.Fatalf("CheckCapacityReserve sufficient = (%d, %v)", gotFree, err)
	}
}

func TestSealRefusesIncompleteProfileMissingBlobsAndLowDisk(t *testing.T) {
	ctx := context.Background()
	t.Setenv("BASHY_OFFLINE", "")

	// 1. Uninitialized root: InspectStatus does not create catalog.db, enumerates all missing capabilities, and Seal refuses.
	emptyRoot := filepath.Join(t.TempDir(), "empty-artifacts")
	stEmpty, err := InspectStatus(ctx, InspectStatusOptions{
		Root:         emptyRoot,
		ProfileSpec:  "default",
		DiskFreeFunc: func(string) (uint64, error) { return 50 << 30, nil },
	})
	if err != nil {
		t.Fatalf("InspectStatus empty: %v", err)
	}
	if stEmpty.ClosureComplete || len(stEmpty.MissingArtifacts) < 6 {
		t.Fatalf("expected incomplete closure with skeleton missing items, got %+v", stEmpty)
	}
	if _, statErr := os.Stat(filepath.Join(emptyRoot, "catalog.db")); !os.IsNotExist(statErr) {
		t.Fatalf("InspectStatus must not create catalog.db on empty root: %v", statErr)
	}
	if _, err := Seal(ctx, SealOptions{
		Root:         emptyRoot,
		ProfileSpec:  "default",
		DiskFreeFunc: func(string) (uint64, error) { return 50 << 30, nil },
	}); !errors.Is(err, ErrIncompleteClosure) {
		t.Fatalf("Seal on empty root = %v, want ErrIncompleteClosure", err)
	}

	// 2. Seed self-host closure (no Ollama model roots) and verify DefaultProfileV1 refuses Seal and enumerates missing models!
	root := filepath.Join(t.TempDir(), "artifacts")
	store, err := OpenStore(ctx, StoreOptions{Root: root})
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	selfProf := SelfHostProfileV1(CurrentPlatform())
	_, digests := seedSelfHostManifest(t, ctx, store, 1, selfProf, "gen1")
	_ = store.Close()

	defProf := DefaultProfileV1(CurrentPlatform())
	stDefault, err := InspectStatus(ctx, InspectStatusOptions{
		Root:         root,
		Profile:      &defProf,
		DiskFreeFunc: func(string) (uint64, error) { return 50 << 30, nil },
	})
	if err != nil {
		t.Fatalf("InspectStatus(DefaultProfileV1): %v", err)
	}
	if stDefault.ClosureComplete || len(stDefault.MissingArtifacts) != 2 {
		t.Fatalf("expected 2 missing required ollama models, got %+v", stDefault.MissingArtifacts)
	}
	_, sealErr := Seal(ctx, SealOptions{
		Root:         root,
		Profile:      &defProf,
		DiskFreeFunc: func(string) (uint64, error) { return 50 << 30, nil },
	})
	if !errors.Is(sealErr, ErrIncompleteClosure) || !strings.Contains(sealErr.Error(), "qwen3.5:4b") || !strings.Contains(sealErr.Error(), "qwen3-embedding:0.6b") {
		t.Fatalf("Seal(DefaultProfileV1) = %v, want ErrIncompleteClosure naming both missing models", sealErr)
	}
	if IsSealed(root) {
		t.Fatal("failed Seal must not leave root sealed")
	}

	// 3. Low disk space refuses Seal with ErrInsufficientCapacity even when self-host closure is complete.
	_, capErr := Seal(ctx, SealOptions{
		Root:         root,
		Profile:      &selfProf,
		DiskFreeFunc: func(string) (uint64, error) { return 100 << 20, nil }, // 100 MiB < 2 GiB reserve
	})
	if !errors.Is(capErr, ErrInsufficientCapacity) {
		t.Fatalf("Seal with low disk = %v, want ErrInsufficientCapacity", capErr)
	}

	// 4. Missing/corrupted CAS blob refuses Seal with ErrIncompleteClosure.
	blobHex := strings.TrimPrefix(digests["source"], "sha256:")
	blobPath := filepath.Join(root, "cas", "blobs", "sha256", blobHex)
	origBlob, err := os.ReadFile(blobPath)
	if err != nil {
		t.Fatalf("read CAS blob: %v", err)
	}
	if err := os.Chmod(blobPath, 0o600); err != nil {
		t.Fatalf("chmod CAS blob writable: %v", err)
	}
	if err := os.WriteFile(blobPath, []byte("corrupted-source-bytes"), 0o600); err != nil {
		t.Fatalf("corrupt CAS blob: %v", err)
	}
	if _, err := Seal(ctx, SealOptions{
		Root:         root,
		Profile:      &selfProf,
		DiskFreeFunc: func(string) (uint64, error) { return 50 << 30, nil },
	}); !errors.Is(err, ErrIncompleteClosure) {
		t.Fatalf("Seal with corrupted blob = %v, want ErrIncompleteClosure", err)
	}
	// Restore intact CAS blob.
	if err := os.WriteFile(blobPath, origBlob, 0o444); err != nil {
		t.Fatalf("restore CAS blob: %v", err)
	}

	// 5. Healthy self-host closure + reserve seals cleanly.
	sealSt, err := Seal(ctx, SealOptions{
		Root:         root,
		Profile:      &selfProf,
		DiskFreeFunc: func(string) (uint64, error) { return 50 << 30, nil },
	})
	if err != nil {
		t.Fatalf("Seal(SelfHostProfileV1): %v", err)
	}
	if !sealSt.Sealed || sealSt.ActiveGeneration != 1 || sealSt.BaselineGeneration != 1 {
		t.Fatalf("unexpected SealStateV1: %+v", sealSt)
	}
	if !IsSealed(root) {
		t.Fatal("expected IsSealed(root) == true after Seal")
	}
}

func TestAdditiveGenerationPreservesBaselineAndPruneProtection(t *testing.T) {
	ctx := context.Background()
	t.Setenv("BASHY_OFFLINE", "")
	root := filepath.Join(t.TempDir(), "artifacts")

	store, err := OpenStore(ctx, StoreOptions{Root: root})
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	prof := SelfHostProfileV1(CurrentPlatform())
	_, gen1Digests := seedSelfHostManifest(t, ctx, store, 1, prof, "gen1")
	_ = store.Close()

	st1, err := Seal(ctx, SealOptions{
		Root:         root,
		Profile:      &prof,
		DiskFreeFunc: func(string) (uint64, error) { return 50 << 30, nil },
	})
	if err != nil {
		t.Fatalf("Seal gen 1: %v", err)
	}
	if st1.BaselineGeneration != 1 || st1.ActiveGeneration != 1 {
		t.Fatalf("gen 1 seal state = %+v", st1)
	}

	// Add generation 2 with new blobs + a disposable scratch record.
	store2, err := OpenStore(ctx, StoreOptions{Root: root})
	if err != nil {
		t.Fatalf("OpenStore gen 2: %v", err)
	}
	_, gen2Digests := seedSelfHostManifest(t, ctx, store2, 2, prof, "gen2")
	scratchPayload := []byte("disposable-scratch-item")
	scratchRec := makeScratchRecord("scratch/tmp", "v1", scratchPayload)
	if _, err := store2.PutArtifact(ctx, scratchRec, bytes.NewReader(scratchPayload)); err != nil {
		t.Fatalf("PutArtifact scratch: %v", err)
	}
	_ = store2.Close()

	st2, err := Seal(ctx, SealOptions{
		Root:         root,
		Profile:      &prof,
		DiskFreeFunc: func(string) (uint64, error) { return 50 << 30, nil },
	})
	if err != nil {
		t.Fatalf("Seal gen 2: %v", err)
	}
	if st2.BaselineGeneration != 1 || st2.ActiveGeneration != 2 {
		t.Fatalf("expected BaselineGeneration=1 and ActiveGeneration=2, got %+v", st2)
	}

	// ApplyPrune must remove scratch while preserving BOTH gen 1 and gen 2 protected roots!
	store3, err := OpenStore(ctx, StoreOptions{Root: root})
	if err != nil {
		t.Fatalf("OpenStore prune: %v", err)
	}
	defer store3.Close()

	items, err := store3.DryRunPrune(ctx, 0)
	if err != nil {
		t.Fatalf("DryRunPrune: %v", err)
	}
	if len(items) != 1 || items[0].ID != scratchRec.Digest {
		t.Fatalf("DryRunPrune = %+v, want only scratch %s", items, scratchRec.Digest)
	}
	counts, err := store3.ApplyPrune(ctx, items)
	if err != nil || counts.Removed != 1 {
		t.Fatalf("ApplyPrune = (%+v, %v)", counts, err)
	}

	for label, dgst := range map[string]string{
		"gen1-toolchain": gen1Digests["gotoolchain"],
		"gen1-source":    gen1Digests["source"],
		"gen2-toolchain": gen2Digests["gotoolchain"],
		"gen2-source":    gen2Digests["source"],
	} {
		if _, err := store3.FetchVerified(ctx, dgst); err != nil {
			t.Fatalf("%s (%s) was not preserved across prune: %v", label, dgst, err)
		}
	}
}

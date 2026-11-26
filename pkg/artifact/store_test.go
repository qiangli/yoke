// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package artifact

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/prune"
)

func makeProtectedRecord(name, version string, payload []byte, deps ...string) CatalogRecordV1 {
	return CatalogRecordV1{
		SchemaVersion:  CatalogRecordSchemaV1,
		Digest:         DigestBytes(payload),
		Size:           int64(len(payload)),
		MediaType:      "application/octet-stream",
		LogicalName:    name,
		LogicalVersion: version,
		Dependencies:   deps,
		Provenance:     "bootstrap",
		Retention:      RetentionProtectedRoot,
		RetentionRoots: []string{"gen-1"},
	}
}

func makeReconstructibleRecord(name, version string, payload []byte, inputDigest string) CatalogRecordV1 {
	dgst := DigestBytes(payload)
	return CatalogRecordV1{
		SchemaVersion:  CatalogRecordSchemaV1,
		Digest:         dgst,
		Size:           int64(len(payload)),
		MediaType:      "application/zip",
		LogicalName:    name,
		LogicalVersion: version,
		Provenance:     "gomod-proxy",
		Retention:      RetentionReconstructible,
		Recipe: &RegenerationRecipe{
			Kind:           RecipeGoModZipV1,
			InputDigests:   []string{inputDigest},
			LoomRef:        "refs/bashy/published/example.com/acme/pkg/v1.0.0",
			Commit:         "0123456789abcdef0123456789abcdef01234567",
			ExpectedDigest: dgst,
			ExpectedGoSum:  "h1:0123456789abcdef=",
		},
	}
}

func makeScratchRecord(name, version string, payload []byte) CatalogRecordV1 {
	return CatalogRecordV1{
		SchemaVersion:  CatalogRecordSchemaV1,
		Digest:         DigestBytes(payload),
		Size:           int64(len(payload)),
		MediaType:      "application/octet-stream",
		LogicalName:    name,
		LogicalVersion: version,
		Provenance:     "build-scratch",
		Retention:      RetentionScratch,
	}
}

func TestDefaultRootPrecedence(t *testing.T) {
	tmp := t.TempDir()
	explicitDir := filepath.Join(tmp, "custom-artifacts")
	bashyHome := filepath.Join(tmp, "bashy-home")

	t.Setenv("BASHY_ARTIFACTS_DIR", explicitDir)
	t.Setenv("BASHY_HOME", bashyHome)
	if got := DefaultRoot(); got != explicitDir {
		t.Fatalf("DefaultRoot with BASHY_ARTIFACTS_DIR = %q, want %q", got, explicitDir)
	}

	t.Setenv("BASHY_ARTIFACTS_DIR", "")
	if got := DefaultRoot(); got != filepath.Join(bashyHome, "artifacts") {
		t.Fatalf("DefaultRoot with BASHY_HOME = %q, want %q", got, filepath.Join(bashyHome, "artifacts"))
	}
}

func TestStoreEpochIncrementAndStaleOrRecycledLeaseLoss(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()

	var mu sync.Mutex
	currentBirth := "birth-alpha"
	aliveFn := func(pid int, startID string) bool {
		mu.Lock()
		defer mu.Unlock()
		return pid == 4242 && startID == currentBirth
	}

	s1, err := OpenStore(ctx, StoreOptions{Root: root, Alive: aliveFn})
	if err != nil {
		t.Fatalf("OpenStore s1: %v", err)
	}
	if s1.ServiceEpoch() != 1 {
		t.Fatalf("s1 epoch = %d, want 1", s1.ServiceEpoch())
	}

	srcBytes := []byte("loom-source-tree-v1")
	srcRec := makeProtectedRecord("source/acme", "v1.0.0", srcBytes)
	if _, err := s1.PutArtifact(ctx, srcRec, bytes.NewReader(srcBytes)); err != nil {
		t.Fatalf("PutArtifact src: %v", err)
	}

	zipBytes := []byte("derived-module-zip-v1")
	zipRec := makeReconstructibleRecord("gomod/acme", "v1.0.0", zipBytes, srcRec.Digest)
	if _, err := s1.PutArtifact(ctx, zipRec, bytes.NewReader(zipBytes)); err != nil {
		t.Fatalf("PutArtifact zip: %v", err)
	}

	lease1, err := s1.AcquireLease(ctx, 4242, "birth-alpha", 10*time.Minute, []string{zipRec.Digest})
	if err != nil {
		t.Fatalf("AcquireLease: %v", err)
	}
	if _, err := s1.RenewLease(ctx, lease1.ID, 15*time.Minute); err != nil {
		t.Fatalf("RenewLease: %v", err)
	}

	// Active lease blocks DryRunPrune and ApplyPrune.
	if items, err := s1.DryRunPrune(ctx, 0); err != nil || len(items) != 0 {
		t.Fatalf("DryRunPrune under active lease = (%v, %v), want empty", items, err)
	}
	counts, err := s1.ApplyPrune(ctx, []prune.Item{{Noun: "artifact", ID: zipRec.Digest, Size: zipRec.Size}})
	if err != nil || counts.Removed != 0 {
		t.Fatalf("ApplyPrune under active lease = (%+v, %v), want 0 removed", counts, err)
	}

	// Simulate crash by closing s1 WITHOUT releasing lease1, then reopening s2.
	if err := s1.Close(); err != nil {
		t.Fatalf("s1.Close: %v", err)
	}

	s2, err := OpenStore(ctx, StoreOptions{Root: root, Alive: aliveFn})
	if err != nil {
		t.Fatalf("OpenStore s2: %v", err)
	}
	defer s2.Close()

	if s2.ServiceEpoch() != 2 {
		t.Fatalf("s2 epoch = %d, want 2", s2.ServiceEpoch())
	}
	active, err := s2.ListActiveLeases(ctx)
	if err != nil || len(active) != 0 {
		t.Fatalf("stale epoch lease persisted as active: %v (err=%v)", active, err)
	}

	// Acquire a new lease in epoch 2, then simulate PID recycling (birth-alpha -> birth-recycled).
	if _, err := s2.AcquireLease(ctx, 4242, "birth-alpha", 10*time.Minute, []string{zipRec.Digest}); err != nil {
		t.Fatalf("s2 AcquireLease: %v", err)
	}
	if items, err := s2.DryRunPrune(ctx, 0); err != nil || len(items) != 0 {
		t.Fatalf("expected active epoch-2 lease to block prune, got %v (%v)", items, err)
	}

	mu.Lock()
	currentBirth = "birth-recycled"
	mu.Unlock()

	items, err := s2.DryRunPrune(ctx, 0)
	if err != nil || len(items) != 1 || items[0].ID != zipRec.Digest {
		t.Fatalf("recycled PID lease must lose authority; DryRunPrune = (%v, %v)", items, err)
	}
	counts, err = s2.ApplyPrune(ctx, items)
	if err != nil || counts.Removed != 1 {
		t.Fatalf("ApplyPrune after PID recycle = (%+v, %v), want 1 removed", counts, err)
	}

	// Verify CAS payload was reclaimed while CatalogRecordV1 + Recipe stay durable!
	has, err := s2.HasBlob(ctx, zipRec.Digest)
	if err != nil || has {
		t.Fatalf("HasBlob(zip) = (%v, %v), want false", has, err)
	}
	st, err := s2.GetRecordStatus(ctx, zipRec.Digest)
	if err != nil {
		t.Fatalf("GetRecordStatus(zip): %v", err)
	}
	if st.PayloadPresent || st.EvictedAt == nil {
		t.Fatalf("expected PayloadPresent=false and EvictedAt!=nil, got %+v", st)
	}
	if st.Record.Recipe == nil || st.Record.Recipe.ExpectedDigest != zipRec.Digest {
		t.Fatalf("expected durable recipe preserved after prune, got %+v", st.Record)
	}
	if _, err := s2.FetchVerified(ctx, zipRec.Digest); !errors.Is(err, ErrPayloadEvicted) {
		t.Fatalf("FetchVerified on evicted payload = %v, want ErrPayloadEvicted", err)
	}
	// Protected input root must still verify cleanly!
	gotSrc, err := s2.FetchVerified(ctx, srcRec.Digest)
	if err != nil || !bytes.Equal(gotSrc, srcBytes) {
		t.Fatalf("FetchVerified(src) = (%q, %v)", gotSrc, err)
	}
}

func TestConcurrentPutFetchLeaseAndPruneRace(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(ctx, StoreOptions{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	// Seed transitive dependency chain: root -> dep1 -> dep2 (dep2 initially marked scratch,
	// yet reachable from protected root so it MUST NEVER be evicted!).
	dep2Bytes := []byte("transitive-leaf-library")
	dep2Rec := makeScratchRecord("deps/leaf", "v1.0.0", dep2Bytes)
	if _, err := store.PutArtifact(ctx, dep2Rec, bytes.NewReader(dep2Bytes)); err != nil {
		t.Fatalf("PutArtifact dep2: %v", err)
	}

	dep1Bytes := []byte("transitive-mid-library")
	dep1Rec := CatalogRecordV1{
		SchemaVersion:  CatalogRecordSchemaV1,
		Digest:         DigestBytes(dep1Bytes),
		Size:           int64(len(dep1Bytes)),
		MediaType:      "application/octet-stream",
		LogicalName:    "deps/mid",
		LogicalVersion: "v1.0.0",
		Dependencies:   []string{dep2Rec.Digest},
		Provenance:     "bootstrap",
		Retention:      RetentionScratch,
	}
	if _, err := store.PutArtifact(ctx, dep1Rec, bytes.NewReader(dep1Bytes)); err != nil {
		t.Fatalf("PutArtifact dep1: %v", err)
	}

	rootBytes := []byte("protected-toolchain-root")
	rootRec := makeProtectedRecord("toolchain/go", "1.27.4", rootBytes, dep1Rec.Digest)
	if _, err := store.PutArtifact(ctx, rootRec, bytes.NewReader(rootBytes)); err != nil {
		t.Fatalf("PutArtifact root: %v", err)
	}

	// Pre-populate 6 reconstructible artifacts held under active read leases during concurrent prune.
	const numLeased = 6
	leasedPayloads := make([][]byte, numLeased)
	leasedDigests := make([]string, numLeased)
	for i := range numLeased {
		p := []byte(fmt.Sprintf("leased-reconstructible-zip-%d", i))
		rec := makeReconstructibleRecord(fmt.Sprintf("gomod/leased-%d", i), "v1.0.0", p, rootRec.Digest)
		if _, err := store.PutArtifact(ctx, rec, bytes.NewReader(p)); err != nil {
			t.Fatalf("PutArtifact leased %d: %v", i, err)
		}
		leasedPayloads[i] = p
		leasedDigests[i] = rec.Digest
	}

	var wg sync.WaitGroup
	errCh := make(chan error, 64)
	stopPrune := make(chan struct{})

	// Concurrent pruners.
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stopPrune:
					return
				default:
				}
				items, err := store.DryRunPrune(ctx, 0)
				if err != nil {
					errCh <- fmt.Errorf("DryRunPrune: %w", err)
					return
				}
				if _, err := store.ApplyPrune(ctx, items); err != nil {
					errCh <- fmt.Errorf("ApplyPrune: %w", err)
					return
				}
			}
		}()
	}

	// Concurrent writers adding both protected and disposable scratch artifacts.
	var writerWG sync.WaitGroup
	protectedExtras := make([]string, 8)
	scratchExtras := make([]string, 8)
	for i := range 8 {
		writerWG.Add(1)
		go func(idx int) {
			defer writerWG.Done()
			pb := []byte(fmt.Sprintf("concurrent-protected-%d", idx))
			pr := makeProtectedRecord(fmt.Sprintf("protected/item-%d", idx), "v1.0.0", pb)
			if _, err := store.PutArtifact(ctx, pr, bytes.NewReader(pb)); err != nil {
				errCh <- fmt.Errorf("PutArtifact protected %d: %w", idx, err)
				return
			}
			protectedExtras[idx] = pr.Digest

			sb := []byte(fmt.Sprintf("concurrent-scratch-%d", idx))
			sr := makeScratchRecord(fmt.Sprintf("scratch/item-%d", idx), "v1.0.0", sb)
			if _, err := store.PutArtifact(ctx, sr, bytes.NewReader(sb)); err != nil {
				errCh <- fmt.Errorf("PutArtifact scratch %d: %w", idx, err)
				return
			}
			scratchExtras[idx] = sr.Digest
		}(i)
	}

	// Concurrent leased readers repeatedly verifying leased reconstructible payloads.
	for i := range numLeased {
		writerWG.Add(1)
		go func(idx int) {
			defer writerWG.Done()
			dgst := leasedDigests[idx]
			want := leasedPayloads[idx]
			err := store.WithReadLease(ctx, 0, "", 5*time.Minute, []string{dgst}, func(lctx context.Context) error {
				for range 12 {
					got, err := store.FetchVerified(lctx, dgst)
					if err != nil {
						return fmt.Errorf("leased FetchVerified(%s): %w", dgst, err)
					}
					if !bytes.Equal(got, want) {
						return fmt.Errorf("leased content mismatch for %s", dgst)
					}
				}
				return nil
			})
			if err != nil {
				errCh <- err
			}
		}(i)
	}

	writerWG.Wait()
	close(stopPrune)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}

	// Now all read leases have been released; run final prune pass and verify invariants!
	items, err := store.DryRunPrune(ctx, 0)
	if err != nil {
		t.Fatalf("final DryRunPrune: %v", err)
	}
	if _, err := store.ApplyPrune(ctx, items); err != nil {
		t.Fatalf("final ApplyPrune: %v", err)
	}

	// 100% of protected roots and transitive dependencies must still exist and verify!
	for _, dgst := range append([]string{rootRec.Digest, dep1Rec.Digest, dep2Rec.Digest}, protectedExtras...) {
		if _, err := store.FetchVerified(ctx, dgst); err != nil {
			t.Fatalf("protected/transitive digest %s lost or corrupted: %v", dgst, err)
		}
	}

	// All leased reconstructible artifacts (now unleased) and scratch artifacts must be evicted from CAS,
	// while reconstructible CatalogRecordV1 + Recipe remain durable!
	for _, dgst := range leasedDigests {
		has, err := store.HasBlob(ctx, dgst)
		if err != nil || has {
			t.Fatalf("expected unleased reconstructible blob %s reclaimed, has=%v err=%v", dgst, has, err)
		}
		st, err := store.GetRecordStatus(ctx, dgst)
		if err != nil || st.PayloadPresent || st.Record.Recipe == nil {
			t.Fatalf("expected durable evicted record+recipe for %s, got (%+v, %v)", dgst, st, err)
		}
	}
	for _, dgst := range scratchExtras {
		has, err := store.HasBlob(ctx, dgst)
		if err != nil || has {
			t.Fatalf("expected scratch blob %s reclaimed, has=%v err=%v", dgst, has, err)
		}
	}
}

func TestSharedDigestProtectedAgainstScratchEviction(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(ctx, StoreOptions{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	sharedPayload := []byte("shared-content-addressed-payload")
	prot := makeProtectedRecord("core/protected", "v1.0.0", sharedPayload)
	if _, err := store.PutArtifact(ctx, prot, bytes.NewReader(sharedPayload)); err != nil {
		t.Fatalf("PutArtifact prot: %v", err)
	}
	scratch := makeScratchRecord("tmp/projection", "v1.0.0", sharedPayload)
	if _, err := store.PutArtifact(ctx, scratch, bytes.NewReader(sharedPayload)); err != nil {
		t.Fatalf("PutArtifact scratch: %v", err)
	}

	items, err := store.DryRunPrune(ctx, 0)
	if err != nil || len(items) != 0 {
		t.Fatalf("shared protected digest must never be candidate for prune, got %v (%v)", items, err)
	}
	got, err := store.FetchVerified(ctx, prot.Digest)
	if err != nil || !bytes.Equal(got, sharedPayload) {
		t.Fatalf("FetchVerified shared = (%q, %v)", got, err)
	}
}

func TestCrashRecoveryOrphanBlobAndInterruptedEvictJournal(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()

	// Part A: Crash right after ORAS CAS write + SaveIndex, BEFORE bbolt catalog commit.
	errSimulatedCrash := errors.New("simulated crash")
	orphanBytes := []byte("orphan-uncommitted-payload")
	orphanRec := makeProtectedRecord("uncommitted/pkg", "v0.1.0", orphanBytes)

	s1, err := OpenStore(ctx, StoreOptions{
		Root: root,
		AfterCASWriteHook: func(dgst string) error {
			if dgst == orphanRec.Digest {
				return errSimulatedCrash
			}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("OpenStore s1: %v", err)
	}

	// Commit one valid root and one reconstructible artifact first.
	validRootBytes := []byte("durable-protected-root")
	validRoot := makeProtectedRecord("durable/root", "v1.0.0", validRootBytes)
	if _, err := s1.PutArtifact(ctx, validRoot, bytes.NewReader(validRootBytes)); err != nil {
		t.Fatalf("PutArtifact validRoot: %v", err)
	}
	evictBytes := []byte("reconstructible-to-interrupt-evict")
	evictRec := makeReconstructibleRecord("gomod/interrupted", "v1.0.0", evictBytes, validRoot.Digest)
	if _, err := s1.PutArtifact(ctx, evictRec, bytes.NewReader(evictBytes)); err != nil {
		t.Fatalf("PutArtifact evictRec: %v", err)
	}

	// Attempt PutArtifact for orphanRec -> crashes after CAS write, before bbolt commit!
	if _, err := s1.PutArtifact(ctx, orphanRec, bytes.NewReader(orphanBytes)); !errors.Is(err, errSimulatedCrash) {
		t.Fatalf("PutArtifact orphan error = %v, want errSimulatedCrash", err)
	}
	// Catalog MUST NOT expose incomplete metadata!
	if _, err := s1.GetRecord(ctx, orphanRec.Digest); !errors.Is(err, ErrNotFound) {
		t.Fatalf("uncommitted record leaked into catalog: %v", err)
	}
	orphanFile := filepath.Join(root, "cas", "blobs", "sha256", strings.TrimPrefix(orphanRec.Digest, "sha256:"))
	if _, err := os.Stat(orphanFile); err != nil {
		t.Fatalf("expected orphan CAS file on disk before reconcile: %v", err)
	}

	// Part B: Simulate crash during ApplyPrune AFTER evict journal entry commits, BEFORE CAS delete!
	s1.opts.DeleteBlobHook = func(dgst string) error {
		if dgst == evictRec.Digest {
			return errSimulatedCrash
		}
		return nil
	}
	items, err := s1.DryRunPrune(ctx, 0)
	if err != nil || len(items) != 1 {
		t.Fatalf("DryRunPrune = (%v, %v), want 1 item", items, err)
	}
	if _, err := s1.ApplyPrune(ctx, items); !errors.Is(err, errSimulatedCrash) {
		t.Fatalf("ApplyPrune error = %v, want errSimulatedCrash", err)
	}
	evictFile := filepath.Join(root, "cas", "blobs", "sha256", strings.TrimPrefix(evictRec.Digest, "sha256:"))
	if _, err := os.Stat(evictFile); err != nil {
		t.Fatalf("expected evict CAS file still on disk before reopen: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("s1.Close: %v", err)
	}

	// Reopen store: startup Reconcile must delete orphan CAS blob AND finish pending evict journal!
	s2, err := OpenStore(ctx, StoreOptions{Root: root})
	if err != nil {
		t.Fatalf("OpenStore s2: %v", err)
	}
	defer s2.Close()

	if _, err := os.Stat(orphanFile); !os.IsNotExist(err) {
		t.Fatalf("expected orphan CAS blob cleaned by startup Reconcile, stat err=%v", err)
	}
	if _, err := os.Stat(evictFile); !os.IsNotExist(err) {
		t.Fatalf("expected interrupted evict CAS blob removed by startup Reconcile, stat err=%v", err)
	}
	st, err := s2.GetRecordStatus(ctx, evictRec.Digest)
	if err != nil || st.PayloadPresent || st.Record.Recipe == nil {
		t.Fatalf("expected evictRec marked evicted with intact recipe after recovery, got (%+v, %v)", st, err)
	}
	gotValid, err := s2.FetchVerified(ctx, validRoot.Digest)
	if err != nil || !bytes.Equal(gotValid, validRootBytes) {
		t.Fatalf("validRoot after recovery = (%q, %v)", gotValid, err)
	}
}

func TestBitRotTruncationAndMissingCASFileDetection(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := OpenStore(ctx, StoreOptions{Root: root})
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	flipBytes := []byte("immutable-protected-model-weights-shard-01")
	flipRec := makeProtectedRecord("models/qwen", "v1.0.0", flipBytes)
	if _, err := store.PutArtifact(ctx, flipRec, bytes.NewReader(flipBytes)); err != nil {
		t.Fatalf("PutArtifact flipRec: %v", err)
	}

	truncBytes := []byte("immutable-protected-toolchain-archive")
	truncRec := makeProtectedRecord("toolchain/archive", "v1.0.0", truncBytes)
	if _, err := store.PutArtifact(ctx, truncRec, bytes.NewReader(truncBytes)); err != nil {
		t.Fatalf("PutArtifact truncRec: %v", err)
	}

	missBytes := []byte("immutable-protected-overlay-bundle")
	missRec := makeProtectedRecord("overlay/bundle", "v1.0.0", missBytes)
	if _, err := store.PutArtifact(ctx, missRec, bytes.NewReader(missBytes)); err != nil {
		t.Fatalf("PutArtifact missRec: %v", err)
	}

	blobFile := func(dgst string) string {
		return filepath.Join(root, "cas", "blobs", "sha256", strings.TrimPrefix(dgst, "sha256:"))
	}

	// 1. Flip 1 byte in flipRec's CAS blob file on disk.
	p1 := blobFile(flipRec.Digest)
	if err := os.Chmod(p1, 0o600); err != nil {
		t.Fatalf("chmod flip blob: %v", err)
	}
	corrupted := bytes.Clone(flipBytes)
	corrupted[len(corrupted)/2] ^= 0xff
	if err := os.WriteFile(p1, corrupted, 0o444); err != nil {
		t.Fatalf("write flipped blob: %v", err)
	}
	if _, err := store.FetchVerified(ctx, flipRec.Digest); !errors.Is(err, ErrCorruptPayload) {
		t.Fatalf("FetchVerified on 1-byte tampered blob = %v, want ErrCorruptPayload", err)
	}

	// 2. Truncate truncRec's CAS blob file on disk.
	p2 := blobFile(truncRec.Digest)
	if err := os.Chmod(p2, 0o600); err != nil {
		t.Fatalf("chmod trunc blob: %v", err)
	}
	if err := os.WriteFile(p2, truncBytes[:5], 0o444); err != nil {
		t.Fatalf("truncate blob: %v", err)
	}
	if _, err := store.FetchVerified(ctx, truncRec.Digest); !errors.Is(err, ErrCorruptPayload) {
		t.Fatalf("FetchVerified on truncated blob = %v, want ErrCorruptPayload", err)
	}

	// 3. Delete missRec's CAS blob file behind the store's back.
	p3 := blobFile(missRec.Digest)
	if err := os.Chmod(p3, 0o600); err != nil {
		t.Fatalf("chmod miss blob: %v", err)
	}
	if err := os.Remove(p3); err != nil {
		t.Fatalf("remove miss blob: %v", err)
	}
	if _, err := store.FetchVerified(ctx, missRec.Digest); !errors.Is(err, ErrCorruptPayload) {
		t.Fatalf("FetchVerified on missing CAS blob = %v, want ErrCorruptPayload", err)
	}
}

func TestDiskFullENOSPCLeavesCatalogClean(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()

	var rejectWrites bool
	store, err := OpenStore(ctx, StoreOptions{
		Root: root,
		DiskSpaceCheck: func(_ context.Context, _ string, _ int64) error {
			if rejectWrites {
				return syscall.ENOSPC
			}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	baseBytes := []byte("baseline-protected-artifact")
	baseRec := makeProtectedRecord("baseline/core", "v1.0.0", baseBytes)
	if _, err := store.PutArtifact(ctx, baseRec, bytes.NewReader(baseBytes)); err != nil {
		t.Fatalf("PutArtifact base: %v", err)
	}

	rejectWrites = true
	bigBytes := bytes.Repeat([]byte("x"), 4096)
	bigRec := makeProtectedRecord("growth/blocked", "v1.0.0", bigBytes)
	if _, err := store.PutArtifact(ctx, bigRec, bytes.NewReader(bigBytes)); !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("PutArtifact under ENOSPC = %v, want ENOSPC", err)
	}

	// Failed write must not leave committed dangling bbolt records or logical mappings.
	if _, err := store.GetRecord(ctx, bigRec.Digest); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected no committed record after ENOSPC, got %v", err)
	}
	if _, err := store.LookupLogical(ctx, bigRec.LogicalName, bigRec.LogicalVersion); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected no logical mapping after ENOSPC, got %v", err)
	}

	// Existing protected artifact must remain intact and readable.
	got, err := store.FetchVerified(ctx, baseRec.Digest)
	if err != nil || !bytes.Equal(got, baseBytes) {
		t.Fatalf("FetchVerified baseline after ENOSPC = (%q, %v)", got, err)
	}
}

func TestWindowsOpenFileSemanticsAndCleanFlockRelease(t *testing.T) {
	ctx := context.Background()
	manualDir, err := os.MkdirTemp("", "s654-winlock-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	defer os.RemoveAll(manualDir)

	var simulateSharingLock bool
	store, err := OpenStore(ctx, StoreOptions{
		Root: manualDir,
		DeleteBlobHook: func(dgst string) error {
			if simulateSharingLock {
				return &os.PathError{Op: "remove", Path: dgst, Err: ErrSharingViolation}
			}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	srcBytes := []byte("retained-source-commit")
	srcRec := makeProtectedRecord("source/mod", "v1.0.0", srcBytes)
	if _, err := store.PutArtifact(ctx, srcRec, bytes.NewReader(srcBytes)); err != nil {
		t.Fatalf("PutArtifact src: %v", err)
	}

	zipBytes := bytes.Repeat([]byte("streamed-zip-chunk-"), 256)
	zipRec := makeReconstructibleRecord("gomod/mod", "v1.0.0", zipBytes, srcRec.Digest)
	if _, err := store.PutArtifact(ctx, zipRec, bytes.NewReader(zipBytes)); err != nil {
		t.Fatalf("PutArtifact zip: %v", err)
	}

	candidates, err := store.DryRunPrune(ctx, 0)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("DryRunPrune = (%v, %v), want 1 candidate", candidates, err)
	}

	// 1. Real concurrent open Fetch stream (OpenBlob) during ApplyPrune:
	// deletion must be skipped cleanly while stream handle is open!
	rc, err := store.OpenBlob(ctx, zipRec.Digest)
	if err != nil {
		t.Fatalf("OpenBlob: %v", err)
	}
	half := make([]byte, len(zipBytes)/2)
	if _, err := io.ReadFull(rc, half); err != nil {
		_ = rc.Close()
		t.Fatalf("ReadFull first half: %v", err)
	}

	counts, err := store.ApplyPrune(ctx, candidates)
	if err != nil || counts.Removed != 0 {
		_ = rc.Close()
		t.Fatalf("ApplyPrune while reader open = (%+v, %v), want 0 removed and nil err", counts, err)
	}
	rest, err := io.ReadAll(rc)
	if err != nil {
		_ = rc.Close()
		t.Fatalf("ReadAll second half: %v", err)
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("rc.Close: %v", err)
	}
	if !bytes.Equal(append(half, rest...), zipBytes) {
		t.Fatal("open stream bytes corrupted during concurrent prune")
	}

	// 2. Simulated Windows ERROR_SHARING_VIOLATION / os.ErrPermission on delete:
	// ApplyPrune must skip cleanly without corrupting bbolt or marking PayloadPresent=false!
	simulateSharingLock = true
	counts, err = store.ApplyPrune(ctx, candidates)
	if err != nil || counts.Removed != 0 {
		t.Fatalf("ApplyPrune under sharing violation = (%+v, %v), want 0 removed and nil err", counts, err)
	}
	st, err := store.GetRecordStatus(ctx, zipRec.Digest)
	if err != nil || !st.PayloadPresent {
		t.Fatalf("catalog must keep PayloadPresent=true when delete deferred: (%+v, %v)", st, err)
	}
	if got, err := store.FetchVerified(ctx, zipRec.Digest); err != nil || !bytes.Equal(got, zipBytes) {
		t.Fatalf("FetchVerified after deferred delete failed: %v", err)
	}

	// 3. Clear sharing lock -> ApplyPrune now deletes cleanly, and Store.Close() releases bbolt flock
	// so os.RemoveAll succeeds cleanly on Windows too!
	simulateSharingLock = false
	counts, err = store.ApplyPrune(ctx, candidates)
	if err != nil || counts.Removed != 1 {
		t.Fatalf("ApplyPrune after lock release = (%+v, %v), want 1 removed", counts, err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("store.Close: %v", err)
	}
	if err := os.RemoveAll(manualDir); err != nil {
		t.Fatalf("RemoveAll after Store.Close failed (leaked open handle): %v", err)
	}
}

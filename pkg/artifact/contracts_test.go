// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package artifact

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestDefaultProfileAndManifestDeterminism(t *testing.T) {
	prof := DefaultProfileV1("linux/amd64")
	if err := prof.Validate(); err != nil {
		t.Fatalf("DefaultProfileV1 validate: %v", err)
	}
	profDigest, err := prof.Digest()
	if err != nil {
		t.Fatalf("prof.Digest: %v", err)
	}

	rootA := RootDescriptor{
		Kind:      "gotoolchain",
		Name:      "go",
		Version:   "1.27.4",
		Platform:  "linux/amd64",
		MediaType: "application/vnd.oci.image.layer.v1.tar+gzip",
		Digest:    DigestBytes([]byte("go1.27.4-sdk")),
		Size:      72_000_000,
	}
	rootB := RootDescriptor{
		Kind:      "source",
		Name:      "yoke",
		Version:   "17bf1a1dda58",
		MediaType: "application/vnd.bashy.git.bundle.v1",
		Digest:    DigestBytes([]byte("yoke-tree")),
		Size:      4_200_000,
		Deps:      []string{rootA.Digest},
	}

	m1 := ManifestV1{
		SchemaVersion:       ManifestSchemaV1,
		Generation:          1,
		CreatedAt:           "2026-11-26T09:00:00Z",
		Platform:            "linux/amd64",
		ProfileName:         prof.Name,
		ProfileDigest:       profDigest,
		GoToolchainVersion:  "1.27.4",
		ProductOverlaySHA:   DigestBytes([]byte("productgo-overlay")),
		BuildImageDigest:    "sha256:1171628e07d3e7490458d9b0b484e689fee56c4a739e3f8ad577188eceece2ff",
		RebuildReserveBytes: 8 << 30,
		Roots:               []RootDescriptor{rootB, rootA},
	}
	m2 := m1
	m2.Roots = []RootDescriptor{rootA, rootB}

	d1, err := m1.Digest()
	if err != nil {
		t.Fatalf("m1.Digest: %v", err)
	}
	d2, err := m2.Digest()
	if err != nil {
		t.Fatalf("m2.Digest: %v", err)
	}
	if d1 != d2 {
		t.Fatalf("manifest canonical digest depended on root order: %s != %s", d1, d2)
	}
}

func TestProfileRejectsIncompleteOrUnsafeConfigs(t *testing.T) {
	base := DefaultProfileV1("darwin/arm64")

	noBashSharp := base
	noBashSharp.Languages = []string{"go"}
	if err := noBashSharp.Validate(); err == nil {
		t.Fatal("expected missing bashsharp language pack to fail validation")
	}

	cloudFloor := base
	cloudFloor.OfflineFloor = TierCloud
	if err := cloudFloor.Validate(); err == nil {
		t.Fatal("expected cloud offline_floor to fail validation")
	}

	traversal := base
	traversal.Name = "../escape"
	if err := traversal.Validate(); err == nil {
		t.Fatal("expected path traversal profile name to fail validation")
	}
}

func TestCanEvictHonorsProtectedRootsRecipesAndLeases(t *testing.T) {
	now := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	srcDigest := DigestBytes([]byte("loom-commit-tree"))
	zipDigest := DigestBytes([]byte("canonical-module-zip"))

	protectedRec := CatalogRecordV1{
		SchemaVersion:  CatalogRecordSchemaV1,
		Digest:         srcDigest,
		Size:           1024,
		MediaType:      "application/vnd.bashy.git.tree.v1",
		LogicalName:    "github.com/qiangli/yoke",
		LogicalVersion: "v0.0.0-20261126092849-17bf1a1dda58",
		Provenance:     "loom:refs/bashy/published/github.com/qiangli/yoke/v0.0.0-20261126092849-17bf1a1dda58",
		Retention:      RetentionProtectedRoot,
		RetentionRoots: []string{"baseline-gen-1"},
	}
	if ok, _ := CanEvict(protectedRec, nil, nil, nil, 1, now, nil); ok {
		t.Fatal("protected root must never be evictable")
	}

	zipRec := CatalogRecordV1{
		SchemaVersion:  CatalogRecordSchemaV1,
		Digest:         zipDigest,
		Size:           4096,
		MediaType:      "application/zip",
		LogicalName:    "gomod/github.com/qiangli/yoke",
		LogicalVersion: "v0.0.0-20261126092849-17bf1a1dda58",
		Provenance:     "gomod-proxy",
		Retention:      RetentionReconstructible,
		Recipe: &RegenerationRecipe{
			Kind:           RecipeGoModZipV1,
			InputDigests:   []string{srcDigest},
			LoomRef:        "refs/bashy/published/github.com/qiangli/yoke/v0.0.0-20261126092849-17bf1a1dda58",
			Commit:         "17bf1a1dda5817bf1a1dda5817bf1a1dda5817bf",
			ExpectedDigest: zipDigest,
			ExpectedGoSum:  "h1:abcdefghijklmnop=",
		},
	}
	if err := zipRec.Validate(); err != nil {
		t.Fatalf("zipRec.Validate: %v", err)
	}

	// Missing retained source input -> must NOT evict.
	if ok, _ := CanEvict(zipRec, nil, map[string]bool{}, nil, 1, now, nil); ok {
		t.Fatal("must refuse eviction when recipe input is not retained")
	}

	// Retained source input, but active lease holds zipDigest -> must NOT evict.
	activeLease := Lease{
		ID:            "lease-1",
		HolderPID:     4242,
		HolderStartID: "start-a",
		ServiceEpoch:  7,
		Digests:       []string{zipDigest},
		ExpiresAt:     now.Add(5 * time.Minute),
	}
	alive := func(pid int, startID string) bool { return pid == 4242 && startID == "start-a" }
	if ok, _ := CanEvict(zipRec, map[string]bool{srcDigest: true}, nil, []Lease{activeLease}, 7, now, alive); ok {
		t.Fatal("must refuse eviction while active lease holds blob")
	}

	// Stale epoch OR recycled PID loses lease authority -> evictable!
	if ok, _ := CanEvict(zipRec, map[string]bool{srcDigest: true}, nil, []Lease{activeLease}, 8, now, alive); !ok {
		t.Fatal("stale service epoch lease should not block eviction")
	}
	deadPID := func(pid int, startID string) bool { return startID == "recycled-different-proc" }
	if ok, _ := CanEvict(zipRec, map[string]bool{srcDigest: true}, nil, []Lease{activeLease}, 7, now, deadPID); !ok {
		t.Fatal("recycled PID lease should not block eviction")
	}
}

func TestImmutablePublicationDetection(t *testing.T) {
	ref, err := LoomPublicationRef("github.com/qiangli/BashSharp", "v0.1.0")
	if err != nil {
		t.Fatalf("LoomPublicationRef: %v", err)
	}
	if !strings.Contains(ref, "!bash!sharp") {
		t.Fatalf("expected module path escaping in %q", ref)
	}
	p1 := PublicationRecord{
		SchemaVersion: PublicationSchemaV1,
		Module:        "github.com/qiangli/BashSharp",
		Version:       "v0.1.0",
		Commit:        "0123456789abcdef0123456789abcdef01234567",
		LoomRef:       ref,
		InfoDigest:    DigestBytes([]byte("info")),
		ModDigest:     DigestBytes([]byte("mod")),
		ZipDigest:     DigestBytes([]byte("zip1")),
		GoSumH1:       "h1:111111111111=",
	}
	if err := p1.Validate(); err != nil {
		t.Fatalf("p1.Validate: %v", err)
	}
	idem, err := CheckImmutablePublication(p1, p1)
	if err != nil || !idem {
		t.Fatalf("identical publication should be idempotent, got idem=%v err=%v", idem, err)
	}
	p2 := p1
	p2.ZipDigest = DigestBytes([]byte("zip2"))
	if _, err := CheckImmutablePublication(p1, p2); !errors.Is(err, ErrImmutablePublicationConflict) {
		t.Fatalf("expected ErrImmutablePublicationConflict, got %v", err)
	}
}

func TestSelectTierExcludesSealedCloudAndPreservesConfinement(t *testing.T) {
	prof := DefaultProfileV1("linux/amd64")
	status := map[Tier]TierStatus{
		TierCloud:     {Available: true, OS: "linux", Arch: "amd64", ProvidesSandbox: true},
		TierCluster:   {Available: false, UnavailableReason: "k3s offline"},
		TierSphere:    {Available: false, UnavailableReason: "no peers"},
		TierSandbox:   {Available: false, UnavailableReason: "podman storage offline"},
		TierWorkplace: {Available: true, OS: "linux", Arch: "amd64", ProvidesSandbox: false},
		TierUserland:  {Available: true, OS: "linux", Arch: "amd64", ProvidesSandbox: false},
	}

	// Sandboxed workload in sealed mode MUST NOT fall through to cloud OR unconstrained workplace/userland!
	_, err := SelectTier(prof, true, WorkloadRequirements{RequireSandbox: true}, status)
	if !errors.Is(err, ErrConfinementViolation) {
		t.Fatalf("expected ErrConfinementViolation when sandbox unavailable, got %v", err)
	}

	// Unconfined build/script workload falls back cleanly past unavailable tiers to workplace.
	got, err := SelectTier(prof, true, WorkloadRequirements{TargetOS: "linux"}, status)
	if err != nil || got != TierWorkplace {
		t.Fatalf("SelectTier unconfined = (%q, %v), want (%q, nil)", got, err, TierWorkplace)
	}

	// Floor enforcement: if OfflineFloor is TierSandbox and sandbox is down, fail with ErrOfflineFloorUnavailable.
	strictProf := prof
	strictProf.OfflineFloor = TierSandbox
	_, err = SelectTier(strictProf, true, WorkloadRequirements{}, status)
	if !errors.Is(err, ErrOfflineFloorUnavailable) {
		t.Fatalf("expected ErrOfflineFloorUnavailable when floor=sandbox is down, got %v", err)
	}
}

func TestApprovedReuseComponentsPermissiveOnly(t *testing.T) {
	comps := ApprovedReuseComponents()
	if len(comps) < 5 {
		t.Fatalf("expected >=5 reuse entries, got %d", len(comps))
	}
	for _, c := range comps {
		switch c.License {
		case "MIT", "Apache-2.0", "BSD-3-Clause", "BSD-2-Clause":
		default:
			t.Fatalf("non-permissive license %q on %s", c.License, c.Module)
		}
	}
}

// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package artifact_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/artifact"
	"github.com/qiangli/yoke/pkg/prune"
	"github.com/qiangli/yoke/pkg/prune/rods"
)

func TestArtifactPrunerDryRunDoesNotCreateOrBumpEpoch(t *testing.T) {
	ctx := context.Background()
	emptyRoot := filepath.Join(t.TempDir(), "nonexistent-artifacts")
	p := artifact.Pruner{Root: emptyRoot}

	items, err := p.DryRun(ctx, prune.Options{})
	if err != nil || len(items) != 0 {
		t.Fatalf("DryRun on missing root = (%v, %v), want empty", items, err)
	}
	if _, err := os.Stat(emptyRoot); !os.IsNotExist(err) {
		t.Fatalf("DryRun must not create missing root directory, stat err=%v", err)
	}

	// Create a store (epoch 1), close it, run DryRun, then reopen to verify DryRun did not bump epoch.
	root := t.TempDir()
	s1, err := artifact.OpenStore(ctx, artifact.StoreOptions{Root: root})
	if err != nil {
		t.Fatalf("OpenStore s1: %v", err)
	}
	if s1.ServiceEpoch() != 1 {
		t.Fatalf("s1 epoch = %d, want 1", s1.ServiceEpoch())
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("s1.Close: %v", err)
	}

	if _, err := (artifact.Pruner{Root: root}).DryRun(ctx, prune.Options{}); err != nil {
		t.Fatalf("DryRun on existing store: %v", err)
	}

	s2, err := artifact.OpenStore(ctx, artifact.StoreOptions{Root: root})
	if err != nil {
		t.Fatalf("OpenStore s2: %v", err)
	}
	defer s2.Close()
	if s2.ServiceEpoch() != 2 {
		t.Fatalf("DryRun mutated service_epoch: s2 epoch = %d, want 2", s2.ServiceEpoch())
	}
}

func TestRodsRegistryAndArtifactPruneCommandOlderAndApply(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	artRoot := filepath.Join(home, "artifacts")
	t.Setenv("HOME", home)
	t.Setenv("BASHY_HOME", filepath.Join(home, ".bashy"))
	t.Setenv("BASHY_ARTIFACTS_DIR", artRoot)

	baseTime := time.Now().UTC()
	store, err := artifact.OpenStore(ctx, artifact.StoreOptions{Root: artRoot})
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	protBytes := []byte("protected-root-payload")
	protRec := artifact.CatalogRecordV1{
		SchemaVersion:  artifact.CatalogRecordSchemaV1,
		Digest:         artifact.DigestBytes(protBytes),
		Size:           int64(len(protBytes)),
		MediaType:      "application/octet-stream",
		LogicalName:    "protected/root",
		LogicalVersion: "v1.0.0",
		Provenance:     "bootstrap",
		Retention:      artifact.RetentionProtectedRoot,
		RetentionRoots: []string{"gen-1"},
		CreatedAt:      baseTime.Add(-48 * time.Hour),
		LastAccessedAt: baseTime.Add(-48 * time.Hour),
	}
	if _, err := store.PutArtifact(ctx, protRec, bytes.NewReader(protBytes)); err != nil {
		t.Fatalf("PutArtifact prot: %v", err)
	}

	oldScratchBytes := []byte("stale-scratch-48h-old")
	oldScratch := artifact.CatalogRecordV1{
		SchemaVersion:  artifact.CatalogRecordSchemaV1,
		Digest:         artifact.DigestBytes(oldScratchBytes),
		Size:           int64(len(oldScratchBytes)),
		MediaType:      "application/octet-stream",
		LogicalName:    "scratch/old",
		LogicalVersion: "v1.0.0",
		Provenance:     "build",
		Retention:      artifact.RetentionScratch,
		CreatedAt:      baseTime.Add(-48 * time.Hour),
		LastAccessedAt: baseTime.Add(-48 * time.Hour),
	}
	if _, err := store.PutArtifact(ctx, oldScratch, bytes.NewReader(oldScratchBytes)); err != nil {
		t.Fatalf("PutArtifact oldScratch: %v", err)
	}

	recentScratchBytes := []byte("fresh-scratch-1m-old")
	recentScratch := artifact.CatalogRecordV1{
		SchemaVersion:  artifact.CatalogRecordSchemaV1,
		Digest:         artifact.DigestBytes(recentScratchBytes),
		Size:           int64(len(recentScratchBytes)),
		MediaType:      "application/octet-stream",
		LogicalName:    "scratch/recent",
		LogicalVersion: "v1.0.0",
		Provenance:     "build",
		Retention:      artifact.RetentionScratch,
		CreatedAt:      baseTime.Add(-1 * time.Minute),
		LastAccessedAt: baseTime.Add(-1 * time.Minute),
	}
	if _, err := store.PutArtifact(ctx, recentScratch, bytes.NewReader(recentScratchBytes)); err != nil {
		t.Fatalf("PutArtifact recentScratch: %v", err)
	}

	// Keep loopback service running on store so rods.Registry() and NewArtifactPruneCommand()
	// exercise live service routing transparently!
	srv, err := artifact.StartServer(ctx, artifact.ServerConfig{
		Root:                 artRoot,
		Addr:                 "127.0.0.1:0",
		Store:                store,
		CloseStoreOnShutdown: true,
	})
	if err != nil {
		_ = store.Close()
		t.Fatalf("StartServer: %v", err)
	}
	defer srv.Close()

	// 1. Verify rods.Registry().Overview lists "artifact" with 2 evictable items (old + recent scratch).
	rows := rods.Registry().Overview(ctx)
	foundArtifact := false
	for _, row := range rows {
		if row.Noun == "artifact" {
			foundArtifact = true
			if row.Error != "" {
				t.Fatalf("overview artifact error: %s", row.Error)
			}
			if row.Count != 2 {
				t.Fatalf("overview artifact count = %d, want 2", row.Count)
			}
		}
	}
	if !foundArtifact {
		t.Fatal("artifact noun missing from rods.Registry()")
	}

	// 2. Run NewArtifactPruneCommand with --older 24h --json (dry-run): only oldScratch qualifies!
	cmd := rods.NewArtifactPruneCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--older", "24h", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("prune dry-run execute: %v", err)
	}
	var dryRes prune.Result
	if err := json.Unmarshal(out.Bytes(), &dryRes); err != nil {
		t.Fatalf("unmarshal dry-run: %v (%s)", err, out.String())
	}
	if dryRes.Applied || len(dryRes.Items) != 1 || dryRes.Items[0].ID != oldScratch.Digest {
		t.Fatalf("dry-run --older 24h = %+v", dryRes)
	}

	// 3. Run NewArtifactPruneCommand with --older 24h --apply --json: removes oldScratch, keeps recentScratch & protRec!
	cmd = rods.NewArtifactPruneCommand()
	out.Reset()
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--older", "24h", "--apply", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("prune apply execute: %v", err)
	}
	var applyRes prune.Result
	if err := json.Unmarshal(out.Bytes(), &applyRes); err != nil {
		t.Fatalf("unmarshal apply: %v (%s)", err, out.String())
	}
	if !applyRes.Applied || applyRes.Counts.Removed != 1 || !strings.Contains(out.String(), oldScratch.Digest) {
		t.Fatalf("apply --older 24h = %+v", applyRes)
	}

	client := srv.NewClient()
	defer client.Close()
	if _, err := client.FetchVerified(ctx, recentScratch.Digest); err != nil {
		t.Fatalf("recentScratch should remain intact after --older 24h prune: %v", err)
	}
	if _, err := client.FetchVerified(ctx, protRec.Digest); err != nil {
		t.Fatalf("protRec should remain intact after prune: %v", err)
	}
	if _, err := client.FetchVerified(ctx, oldScratch.Digest); err == nil {
		t.Fatal("oldScratch should have been pruned")
	}
}

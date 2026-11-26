// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package artifact

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestServerRejectsNonLoopbackUnlessAllowed(t *testing.T) {
	ctx := context.Background()
	_, err := StartServer(ctx, ServerConfig{
		Root: t.TempDir(),
		Addr: "0.0.0.0:0",
	})
	if !errors.Is(err, ErrNonLoopbackBind) {
		t.Fatalf("StartServer(0.0.0.0:0) = %v, want ErrNonLoopbackBind", err)
	}
}

func TestLoopbackServiceAndClientRoundTrip(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()

	srv, err := StartServer(ctx, ServerConfig{
		Root: root,
		Addr: "127.0.0.1:0",
	})
	if err != nil {
		t.Fatalf("StartServer: %v", err)
	}
	defer srv.Close()

	if runtime.GOOS != "windows" {
		st, err := os.Stat(filepath.Join(root, serviceTokenFile))
		if err != nil {
			t.Fatalf("stat service.token: %v", err)
		}
		if perm := st.Mode().Perm(); perm != 0o600 {
			t.Fatalf("service.token perm = %o, want 0600", perm)
		}
	}

	// 1. Auth enforcement: missing token and wrong token -> 401 ErrUnauthorized.
	noAuthClient := NewClient(srv.Addr(), "")
	if _, err := noAuthClient.Status(ctx); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("missing token Status err = %v, want ErrUnauthorized", err)
	}
	wrongAuthClient := NewClient(srv.Addr(), "deadbeef")
	if _, err := wrongAuthClient.Status(ctx); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("wrong token Status err = %v, want ErrUnauthorized", err)
	}

	// 2. ConnectOrOpen must connect via HTTP *Client while server is live (without bbolt lock conflict!).
	backend, err := ConnectOrOpen(ctx, root)
	if err != nil {
		t.Fatalf("ConnectOrOpen (live server): %v", err)
	}
	defer backend.Close()
	if _, isClient := backend.(*Client); !isClient {
		t.Fatalf("ConnectOrOpen returned %T, want *Client while server is live", backend)
	}

	// 3. PutBlob (chunked unknown length) + PutArtifact + FetchVerified + LookupLogical.
	chunkedData := []byte("chunked-stream-blob-payload")
	chunkedDigest := DigestBytes(chunkedData)
	if n, err := backend.PutBlob(ctx, chunkedDigest, -1, "application/octet-stream", io.NopCloser(bytes.NewReader(chunkedData))); err != nil || n != int64(len(chunkedData)) {
		t.Fatalf("PutBlob chunked = (%d, %v)", n, err)
	}
	chunkedRec := makeProtectedRecord("raw/chunked", "v1.0.0", chunkedData)
	if _, err := backend.PutRecord(ctx, chunkedRec); err != nil {
		t.Fatalf("PutRecord chunked: %v", err)
	}
	gotChunked, err := backend.FetchVerified(ctx, chunkedDigest)
	if err != nil || !bytes.Equal(gotChunked, chunkedData) {
		t.Fatalf("FetchVerified chunked = (%q, %v)", gotChunked, err)
	}
	lookedUp, err := backend.LookupLogical(ctx, "raw/chunked", "v1.0.0")
	if err != nil || lookedUp.Record.Digest != chunkedDigest {
		t.Fatalf("LookupLogical = (%+v, %v)", lookedUp, err)
	}

	// 4. Immutable module publication + conflict -> 409 ErrImmutablePublicationConflict!
	infoBytes := []byte(`{"Version":"v1.0.0","Time":"2026-11-26T00:00:00Z"}`)
	modBytes := []byte("module example.com/acme/lib\n\ngo 1.27\n")
	zip1Bytes := []byte("canonical-zip-bytes-v1")
	zip2Bytes := []byte("divergent-zip-bytes-v1")
	for _, b := range [][]byte{infoBytes, modBytes, zip1Bytes, zip2Bytes} {
		if _, err := backend.PutBlob(ctx, DigestBytes(b), int64(len(b)), "application/octet-stream", bytes.NewReader(b)); err != nil {
			t.Fatalf("PutBlob pub asset: %v", err)
		}
	}
	loomRef, err := LoomPublicationRef("example.com/acme/lib", "v1.0.0")
	if err != nil {
		t.Fatalf("LoomPublicationRef: %v", err)
	}
	pub1 := PublicationRecord{
		SchemaVersion: PublicationSchemaV1,
		Module:        "example.com/acme/lib",
		Version:       "v1.0.0",
		Commit:        "0123456789abcdef0123456789abcdef01234567",
		LoomRef:       loomRef,
		InfoDigest:    DigestBytes(infoBytes),
		ModDigest:     DigestBytes(modBytes),
		ZipDigest:     DigestBytes(zip1Bytes),
		GoSumH1:       "h1:111111111111=",
	}
	idem, err := backend.PublishModule(ctx, pub1)
	if err != nil || idem {
		t.Fatalf("first PublishModule = (idem=%v, err=%v), want (false, nil)", idem, err)
	}
	idem, err = backend.PublishModule(ctx, pub1)
	if err != nil || !idem {
		t.Fatalf("idempotent PublishModule = (idem=%v, err=%v), want (true, nil)", idem, err)
	}
	pubConflict := pub1
	pubConflict.ZipDigest = DigestBytes(zip2Bytes)
	pubConflict.GoSumH1 = "h1:222222222222="
	if _, err := backend.PublishModule(ctx, pubConflict); !errors.Is(err, ErrImmutablePublicationConflict) {
		t.Fatalf("conflicting PublishModule = %v, want ErrImmutablePublicationConflict", err)
	}
	gotPub, err := backend.GetPublication(ctx, "example.com/acme/lib", "v1.0.0")
	if err != nil || gotPub.ZipDigest != pub1.ZipDigest {
		t.Fatalf("GetPublication = (%+v, %v)", gotPub, err)
	}

	// 5. ManifestV1 put/get/list round-trip.
	prof := DefaultProfileV1("linux/amd64")
	profBytes, err := prof.CanonicalBytes()
	if err != nil {
		t.Fatalf("prof.CanonicalBytes: %v", err)
	}
	if _, err := backend.PutBlob(ctx, DigestBytes(profBytes), int64(len(profBytes)), "application/json", bytes.NewReader(profBytes)); err != nil {
		t.Fatalf("PutBlob prof: %v", err)
	}
	overlayBytes := []byte("productgo-overlay-v1")
	if _, err := backend.PutBlob(ctx, DigestBytes(overlayBytes), int64(len(overlayBytes)), "application/octet-stream", bytes.NewReader(overlayBytes)); err != nil {
		t.Fatalf("PutBlob overlay: %v", err)
	}
	manifest := ManifestV1{
		SchemaVersion:       ManifestSchemaV1,
		Generation:          1,
		CreatedAt:           "2026-11-26T10:00:00Z",
		Platform:            "linux/amd64",
		ProfileName:         prof.Name,
		ProfileDigest:       DigestBytes(profBytes),
		GoToolchainVersion:  "1.27.4",
		ProductOverlaySHA:   DigestBytes(overlayBytes),
		RebuildReserveBytes: 8 << 30,
		Roots: []RootDescriptor{{
			Kind:      "gotoolchain",
			Name:      "raw/chunked",
			Version:   "v1.0.0",
			MediaType: "application/octet-stream",
			Digest:    chunkedDigest,
			Size:      int64(len(chunkedData)),
		}},
	}
	if err := backend.PutManifest(ctx, manifest); err != nil {
		t.Fatalf("PutManifest: %v", err)
	}
	gotManifest, err := backend.GetManifest(ctx, 1)
	if err != nil || gotManifest.Generation != 1 {
		t.Fatalf("GetManifest(1) = (%+v, %v)", gotManifest, err)
	}
	manifests, err := backend.ListManifests(ctx)
	if err != nil || len(manifests) != 1 {
		t.Fatalf("ListManifests = (%v, %v)", manifests, err)
	}

	// 6. Lease + Prune over HTTP client.
	scratchData := []byte("evictable-scratch-via-http")
	scratchRec := makeScratchRecord("scratch/http", "v1.0.0", scratchData)
	if _, err := backend.PutArtifact(ctx, scratchRec, bytes.NewReader(scratchData)); err != nil {
		t.Fatalf("PutArtifact scratch: %v", err)
	}

	pid, startID := CurrentProcessIdentity(ctx)
	lease, err := backend.AcquireLease(ctx, pid, startID, 5*time.Minute, []string{scratchRec.Digest})
	if err != nil {
		t.Fatalf("AcquireLease: %v", err)
	}
	if items, err := backend.DryRunPrune(ctx, 0); err != nil || len(items) != 0 {
		t.Fatalf("DryRunPrune while leased = (%v, %v), want empty", items, err)
	}
	if err := backend.ReleaseLease(ctx, lease.ID); err != nil {
		t.Fatalf("ReleaseLease: %v", err)
	}

	items, err := backend.DryRunPrune(ctx, 0)
	if err != nil || len(items) != 1 || items[0].ID != scratchRec.Digest {
		t.Fatalf("DryRunPrune after ReleaseLease = (%v, %v)", items, err)
	}
	counts, err := backend.ApplyPrune(ctx, items)
	if err != nil || counts.Removed != 1 {
		t.Fatalf("ApplyPrune = (%+v, %v)", counts, err)
	}

	st, err := backend.Status(ctx)
	if err != nil || st.ManifestCount != 1 || st.PublicationCount != 1 || st.EvictedCount != 1 {
		t.Fatalf("Status = (%+v, %v)", st, err)
	}

	// 7. Shut down Server and verify ConnectOrOpen transparently falls back to direct *Store!
	if err := srv.Close(); err != nil {
		t.Fatalf("srv.Close: %v", err)
	}
	direct, err := ConnectOrOpen(ctx, root)
	if err != nil {
		t.Fatalf("ConnectOrOpen after shutdown: %v", err)
	}
	defer direct.Close()
	if _, isStore := direct.(*Store); !isStore {
		t.Fatalf("ConnectOrOpen after shutdown returned %T, want *Store", direct)
	}
}

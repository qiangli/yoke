// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package artifact

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/opencontainers/go-digest"
	bolt "go.etcd.io/bbolt"
)

// JournalOp names a crash-recoverable multi-step CAS + catalog transition.
type JournalOp string

const (
	JournalOpPut   JournalOp = "put"
	JournalOpEvict JournalOp = "evict"
)

// JournalEntry records an in-flight multi-step transition across ORAS CAS and bbolt.
type JournalEntry struct {
	Op           JournalOp `json:"op"`
	Digest       string    `json:"digest"`
	ServiceEpoch uint64    `json:"service_epoch"`
	StartedAt    time.Time `json:"started_at"`
	ExpiresAt    time.Time `json:"expires_at,omitempty"`
}

func journalKey(op JournalOp, dgst string) []byte {
	return []byte(string(op) + ":" + dgst)
}

func putJournalTx(tx *bolt.Tx, entry JournalEntry) error {
	b := tx.Bucket(bucketJournal)
	if b == nil {
		return errors.New("artifact: missing journal bucket")
	}
	raw, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	return b.Put(journalKey(entry.Op, entry.Digest), raw)
}

func deleteJournalTx(tx *bolt.Tx, op JournalOp, dgst string) error {
	b := tx.Bucket(bucketJournal)
	if b == nil {
		return nil
	}
	return b.Delete(journalKey(op, dgst))
}

func loadJournalEntriesTx(tx *bolt.Tx) ([]JournalEntry, error) {
	b := tx.Bucket(bucketJournal)
	if b == nil {
		return nil, nil
	}
	var out []JournalEntry
	err := b.ForEach(func(_, v []byte) error {
		var e JournalEntry
		if err := json.Unmarshal(v, &e); err != nil {
			return err
		}
		out = append(out, e)
		return nil
	})
	return out, err
}

// Reconcile replays pending evict journal transitions, rolls back stale put
// staging transitions, removes leftover ingest temp files, and deletes any
// unreferenced orphan CAS blobs not tracked in records, manifests, or publications.
func (s *Store) Reconcile(ctx context.Context) error {
	if err := s.enterOp(); err != nil {
		return err
	}
	defer s.exitOp()
	if s.readOnly {
		return errors.New("artifact: cannot reconcile read-only store")
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	return s.reconcileLocked(ctx)
}

func (s *Store) reconcileLocked(ctx context.Context) error {
	now := s.now()

	var snap catalogSnapshot
	var entries []JournalEntry
	if err := s.db.View(func(tx *bolt.Tx) error {
		var err error
		snap, err = s.readCatalogSnapshotTx(tx, now)
		if err != nil {
			return err
		}
		entries, err = loadJournalEntriesTx(tx)
		return err
	}); err != nil {
		return err
	}

	// 1. Replay pending evict transitions first so interrupted evictions converge.
	for _, je := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if je.Op != JournalOpEvict {
			continue
		}
		recEntry, ok := snap.records[je.Digest]
		if !ok || !recEntry.PayloadPresent {
			// Record absent or already marked evicted; ensure CAS blob is gone if unreferenced.
			if !snap.protectedClosure[je.Digest] && !snap.hasActiveLease(je.Digest) && !s.hasOpenReaders(je.Digest) {
				if err := s.deleteCASBlobLocked(ctx, je.Digest); err != nil && !isSharingOrLockError(err) {
					return err
				}
			}
			if err := s.db.Update(func(tx *bolt.Tx) error {
				return deleteJournalTx(tx, JournalOpEvict, je.Digest)
			}); err != nil {
				return err
			}
			continue
		}

		canEvict, _ := s.canEvictSnapshot(recEntry.Record, snap, now)
		if !canEvict || s.hasOpenReaders(je.Digest) {
			// Protected or newly leased; abort stale eviction journal entry.
			if err := s.db.Update(func(tx *bolt.Tx) error {
				return deleteJournalTx(tx, JournalOpEvict, je.Digest)
			}); err != nil {
				return err
			}
			continue
		}

		if err := s.deleteCASBlobLocked(ctx, je.Digest); err != nil {
			if isSharingOrLockError(err) {
				// File still held open on host; defer without corrupting catalog.
				_ = s.db.Update(func(tx *bolt.Tx) error {
					return deleteJournalTx(tx, JournalOpEvict, je.Digest)
				})
				continue
			}
			return err
		}

		evictedAt := now
		if err := s.db.Update(func(tx *bolt.Tx) error {
			rb := tx.Bucket(bucketRecords)
			if raw := rb.Get([]byte(je.Digest)); raw != nil {
				var cur RecordStatus
				if err := json.Unmarshal(raw, &cur); err == nil {
					cur.PayloadPresent = false
					cur.EvictedAt = &evictedAt
					if updated, err := json.Marshal(cur); err == nil {
						if err := rb.Put([]byte(je.Digest), updated); err != nil {
							return err
						}
					}
				}
			}
			return deleteJournalTx(tx, JournalOpEvict, je.Digest)
		}); err != nil {
			return err
		}

		// Refresh snapshot after completing eviction so subsequent checks see updated state.
		if err := s.db.View(func(tx *bolt.Tx) error {
			var err error
			snap, err = s.readCatalogSnapshotTx(tx, now)
			return err
		}); err != nil {
			return err
		}
	}

	// 2. Clean stale put staging entries (from prior service epochs or expired TTLs),
	// while preserving active current-epoch staged uploads.
	activeStaged := make(map[string]bool)
	for _, je := range entries {
		if je.Op != JournalOpPut {
			continue
		}
		if snap.referencedDigests[je.Digest] {
			_ = s.db.Update(func(tx *bolt.Tx) error {
				return deleteJournalTx(tx, JournalOpPut, je.Digest)
			})
			continue
		}
		if je.ServiceEpoch == s.epoch && (je.ExpiresAt.IsZero() || now.Before(je.ExpiresAt)) {
			activeStaged[je.Digest] = true
			continue
		}
		_ = s.db.Update(func(tx *bolt.Tx) error {
			return deleteJournalTx(tx, JournalOpPut, je.Digest)
		})
	}

	// 3. Remove leftover temporary ingest files from interrupted writes only when
	// no current-epoch staged upload is in flight ( startup Reconcile has none ).
	if len(activeStaged) == 0 {
		ingestDir := filepath.Join(s.casDir, "ingest")
		if dirEntries, err := os.ReadDir(ingestDir); err == nil {
			for _, de := range dirEntries {
				if !de.IsDir() {
					_ = os.Remove(filepath.Join(ingestDir, de.Name()))
				}
			}
		}
	}

	// 4. Scan CAS sha256 blobs directory and delete any unreferenced orphan blobs.
	sha256Dir := filepath.Join(s.casDir, "blobs", "sha256")
	blobFiles, err := os.ReadDir(sha256Dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, bf := range blobFiles {
		if err := ctx.Err(); err != nil {
			return err
		}
		if bf.IsDir() {
			continue
		}
		dgst := "sha256:" + bf.Name()
		if _, err := digest.Parse(dgst); err != nil || !ValidDigest(dgst) {
			continue
		}
		if snap.referencedDigests[dgst] || activeStaged[dgst] || s.hasOpenReaders(dgst) {
			continue
		}
		if err := s.deleteCASBlobLocked(ctx, dgst); err != nil && !isSharingOrLockError(err) {
			return fmt.Errorf("artifact: reconcile orphan %s: %w", dgst, err)
		}
	}

	return s.cas.SaveIndex()
}

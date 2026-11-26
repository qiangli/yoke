// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package artifact

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	bolt "go.etcd.io/bbolt"
	berrors "go.etcd.io/bbolt/errors"
	"oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/errdef"

	"github.com/qiangli/yoke/pkg/prune"
)

var (
	bucketMeta         = []byte("meta")
	bucketRecords      = []byte("records")
	bucketLogical      = []byte("logical")
	bucketPublications = []byte("publications")
	bucketManifests    = []byte("manifests")
	bucketLeases       = []byte("leases")
	bucketJournal      = []byte("journal")

	metaKeyServiceEpoch = []byte("service_epoch")
)

const (
	defaultOpenTimeout = 3 * time.Second
	defaultStagingTTL  = 15 * time.Minute
	defaultMediaType   = "application/octet-stream"
)

// DefaultRoot resolves the managed artifact root directory without creating it:
// $BASHY_ARTIFACTS_DIR if non-empty, else $BASHY_HOME/artifacts (~/.bashy/artifacts fallback).
func DefaultRoot() string {
	if p := strings.TrimSpace(os.Getenv("BASHY_ARTIFACTS_DIR")); p != "" {
		return p
	}
	if p := strings.TrimSpace(os.Getenv("BASHY_HOME")); p != "" {
		return filepath.Join(p, "artifacts")
	}
	h, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(h) == "" {
		return filepath.Join(".bashy", "artifacts")
	}
	return filepath.Join(h, ".bashy", "artifacts")
}

// AliveFunc reports whether holder PID with optional birth marker startID is still live.
type AliveFunc func(pid int, startID string) bool

// CurrentProcessIdentity returns this process's PID and native birth marker when available.
func CurrentProcessIdentity(ctx context.Context) (int, string) {
	pid := os.Getpid()
	if err := ctx.Err(); err != nil {
		return pid, ""
	}
	startID, err := lookupProcessStartID(pid)
	if err != nil {
		return pid, ""
	}
	return pid, startID
}

// DefaultAliveFunc checks holder liveness via native OS process birth identity,
// comparing StartID when non-empty and falling back safely if birth identity is unavailable.
func DefaultAliveFunc(pid int, startID string) bool {
	if pid <= 0 {
		return false
	}
	gotStartID, err := lookupProcessStartID(pid)
	if err == nil && gotStartID != "" {
		if startID != "" {
			return gotStartID == startID
		}
		return true
	}
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	return processExists(pid)
}

// RecordStatus pairs a durable CatalogRecordV1 with its current CAS payload state.
type RecordStatus struct {
	Record         CatalogRecordV1 `json:"record"`
	PayloadPresent bool            `json:"payload_present"`
	EvictedAt      *time.Time      `json:"evicted_at,omitempty"`
}

// StoreStatus summarizes catalog and lease authority state.
type StoreStatus struct {
	ServiceEpoch     uint64 `json:"service_epoch"`
	PID              int    `json:"pid"`
	RecordCount      int    `json:"record_count"`
	EvictedCount     int    `json:"evicted_count"`
	PublicationCount int    `json:"publication_count"`
	ManifestCount    int    `json:"manifest_count"`
	ActiveLeaseCount int    `json:"active_lease_count"`
	LatestGeneration uint64 `json:"latest_generation"`
}

// Backend is the uniform interface implemented by both a local *Store and an HTTP *Client.
type Backend interface {
	PutBlob(ctx context.Context, dgst string, size int64, mediaType string, r io.Reader) (int64, error)
	PutRecord(ctx context.Context, rec CatalogRecordV1) (RecordStatus, error)
	PutArtifact(ctx context.Context, rec CatalogRecordV1, r io.Reader) (RecordStatus, error)
	GetRecord(ctx context.Context, dgst string) (CatalogRecordV1, error)
	GetRecordStatus(ctx context.Context, dgst string) (RecordStatus, error)
	LookupLogical(ctx context.Context, name, version string) (RecordStatus, error)
	OpenBlob(ctx context.Context, dgst string) (io.ReadCloser, error)
	FetchVerified(ctx context.Context, dgst string) ([]byte, error)
	PublishModule(ctx context.Context, pub PublicationRecord) (bool, error)
	GetPublication(ctx context.Context, modPath, version string) (PublicationRecord, error)
	ListPublications(ctx context.Context) ([]PublicationRecord, error)
	PutManifest(ctx context.Context, m ManifestV1) error
	GetManifest(ctx context.Context, generation uint64) (ManifestV1, error)
	ListManifests(ctx context.Context) ([]ManifestV1, error)
	AcquireLease(ctx context.Context, holderPID int, holderStartID string, ttl time.Duration, digests []string) (Lease, error)
	RenewLease(ctx context.Context, leaseID string, ttl time.Duration) (Lease, error)
	ReleaseLease(ctx context.Context, leaseID string) error
	ListActiveLeases(ctx context.Context) ([]Lease, error)
	WithReadLease(ctx context.Context, holderPID int, holderStartID string, ttl time.Duration, digests []string, fn func(context.Context) error) error
	DryRunPrune(ctx context.Context, older time.Duration) ([]prune.Item, error)
	ApplyPrune(ctx context.Context, items []prune.Item) (prune.Counts, error)
	Status(ctx context.Context) (StoreStatus, error)
	Close() error
}

// StoreOptions configures OpenStore and provides deterministic hooks for fault-injection tests.
type StoreOptions struct {
	Root        string
	ReadOnly    bool
	OpenTimeout time.Duration
	StagingTTL  time.Duration
	Now         func() time.Time
	Alive       AliveFunc

	// Fault-injection and admission hooks.
	BeforeWrite       func(ctx context.Context, size int64) error
	DiskSpaceCheck    func(ctx context.Context, root string, neededBytes int64) error
	AfterCASWriteHook func(digest string) error
	DeleteBlobHook    func(digest string) error
}

// Store manages the ORAS filesystem CAS (<root>/cas) and bbolt catalog (<root>/catalog.db)
// under single-writer serialization.
type Store struct {
	root       string
	casDir     string
	dbPath     string
	readOnly   bool
	stagingTTL time.Duration
	opts       StoreOptions

	cas   *oci.Store
	roCAS *oci.ReadOnlyStore
	db    *bolt.DB
	epoch uint64

	writeMu sync.Mutex

	lifeMu   sync.Mutex
	inFlight sync.WaitGroup
	closed   bool

	readersMu   sync.Mutex
	openReaders map[string]int
	lastAccess  map[string]time.Time
}

var _ Backend = (*Store)(nil)

// OpenStore opens (or initializes) the artifact store at opts.Root (or DefaultRoot()).
// Every writable open increments the persisted service_epoch and runs crash recovery Reconcile.
func OpenStore(ctx context.Context, opts StoreOptions) (*Store, error) {
	root := strings.TrimSpace(opts.Root)
	if root == "" {
		root = DefaultRoot()
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("artifact: resolve root %q: %w", root, err)
	}
	casDir := filepath.Join(rootAbs, "cas")
	dbPath := filepath.Join(rootAbs, "catalog.db")

	wait := opts.OpenTimeout
	if wait <= 0 {
		wait = defaultOpenTimeout
	}
	stagingTTL := opts.StagingTTL
	if stagingTTL <= 0 {
		stagingTTL = defaultStagingTTL
	}

	if !opts.ReadOnly {
		if err := os.MkdirAll(casDir, 0o700); err != nil {
			return nil, fmt.Errorf("artifact: create cas dir: %w", err)
		}
	} else {
		if _, err := os.Stat(dbPath); err != nil {
			if os.IsNotExist(err) {
				return nil, fmt.Errorf("%w: %s", ErrNotFound, dbPath)
			}
			return nil, err
		}
	}

	var casStore *oci.Store
	var roCASStore *oci.ReadOnlyStore
	if !opts.ReadOnly {
		casStore, err = oci.NewWithContext(ctx, casDir)
		if err != nil {
			return nil, fmt.Errorf("artifact: open oras oci store: %w", err)
		}
		casStore.AutoSaveIndex = true
		casStore.AutoGC = false
	} else {
		if _, statErr := os.Stat(filepath.Join(casDir, ocispec.ImageLayoutFile)); statErr == nil {
			roCASStore, err = oci.NewFromFS(ctx, os.DirFS(casDir))
			if err != nil {
				return nil, fmt.Errorf("artifact: open read-only oras oci store: %w", err)
			}
		}
	}

	db, err := openBoltBounded(ctx, dbPath, opts.ReadOnly, wait)
	if err != nil {
		return nil, err
	}

	s := &Store{
		root:        rootAbs,
		casDir:      casDir,
		dbPath:      dbPath,
		readOnly:    opts.ReadOnly,
		stagingTTL:  stagingTTL,
		opts:        opts,
		cas:         casStore,
		roCAS:       roCASStore,
		db:          db,
		openReaders: make(map[string]int),
		lastAccess:  make(map[string]time.Time),
	}

	if opts.ReadOnly {
		_ = db.View(func(tx *bolt.Tx) error {
			if mb := tx.Bucket(bucketMeta); mb != nil {
				if raw := mb.Get(metaKeyServiceEpoch); len(raw) == 8 {
					s.epoch = binary.BigEndian.Uint64(raw)
				}
			}
			return nil
		})
		return s, nil
	}

	if err := db.Update(func(tx *bolt.Tx) error {
		for _, name := range [][]byte{
			bucketMeta,
			bucketRecords,
			bucketLogical,
			bucketPublications,
			bucketManifests,
			bucketLeases,
			bucketJournal,
		} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		mb := tx.Bucket(bucketMeta)
		var prev uint64
		if raw := mb.Get(metaKeyServiceEpoch); len(raw) == 8 {
			prev = binary.BigEndian.Uint64(raw)
		}
		s.epoch = prev + 1
		var buf [8]byte
		binary.BigEndian.PutUint64(buf[:], s.epoch)
		return mb.Put(metaKeyServiceEpoch, buf[:])
	}); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("artifact: initialize catalog: %w", err)
	}

	if err := s.reconcileLocked(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("artifact: startup reconcile: %w", err)
	}

	return s, nil
}

func openBoltBounded(ctx context.Context, path string, readOnly bool, wait time.Duration) (*bolt.DB, error) {
	deadline := time.Now().Add(wait)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		db, err := bolt.Open(path, 0o600, &bolt.Options{
			Timeout:  50 * time.Millisecond,
			ReadOnly: readOnly,
		})
		if err == nil {
			return db, nil
		}
		if !errors.Is(err, berrors.ErrTimeout) {
			return nil, fmt.Errorf("artifact: open catalog %s: %w", path, err)
		}
		if !time.Now().Before(deadline) {
			return nil, fmt.Errorf("%w: %s: %v", ErrStoreBusy, path, err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func (s *Store) Root() string         { return s.root }
func (s *Store) ServiceEpoch() uint64 { return s.epoch }

func (s *Store) now() time.Time {
	if s.opts.Now != nil {
		return s.opts.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Store) alive(pid int, startID string) bool {
	if s.opts.Alive != nil {
		return s.opts.Alive(pid, startID)
	}
	return DefaultAliveFunc(pid, startID)
}

func (s *Store) enterOp() error {
	s.lifeMu.Lock()
	defer s.lifeMu.Unlock()
	if s.closed {
		return errors.New("artifact: store is closed")
	}
	s.inFlight.Add(1)
	return nil
}

func (s *Store) exitOp() {
	s.inFlight.Done()
}

// Close waits for in-flight operations, flushes access timestamps if writable,
// and releases the bbolt file lock cleanly (essential on Windows before temp-dir removal).
func (s *Store) Close() error {
	s.lifeMu.Lock()
	if s.closed {
		s.lifeMu.Unlock()
		return nil
	}
	s.closed = true
	s.lifeMu.Unlock()

	s.inFlight.Wait()

	if !s.readOnly && s.db != nil {
		s.flushLastAccessBestEffort()
	}
	if s.db != nil {
		return s.db.Close()
	}
	return nil
}

func (s *Store) flushLastAccessBestEffort() {
	s.readersMu.Lock()
	if len(s.lastAccess) == 0 {
		s.readersMu.Unlock()
		return
	}
	pending := make(map[string]time.Time, len(s.lastAccess))
	for k, v := range s.lastAccess {
		pending[k] = v
	}
	s.lastAccess = make(map[string]time.Time)
	s.readersMu.Unlock()

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_ = s.db.Update(func(tx *bolt.Tx) error {
		rb := tx.Bucket(bucketRecords)
		if rb == nil {
			return nil
		}
		for dgst, ts := range pending {
			raw := rb.Get([]byte(dgst))
			if raw == nil {
				continue
			}
			var st RecordStatus
			if err := json.Unmarshal(raw, &st); err != nil {
				continue
			}
			if ts.After(st.Record.LastAccessedAt) {
				st.Record.LastAccessedAt = ts
				if b, err := json.Marshal(st); err == nil {
					_ = rb.Put([]byte(dgst), b)
				}
			}
		}
		return nil
	})
}

func (s *Store) checkWriteAllowed(ctx context.Context, size int64) error {
	if s.readOnly {
		return errors.New("artifact: store opened read-only")
	}
	if s.opts.BeforeWrite != nil {
		if err := s.opts.BeforeWrite(ctx, size); err != nil {
			return err
		}
	}
	if s.opts.DiskSpaceCheck != nil {
		if err := s.opts.DiskSpaceCheck(ctx, s.root, size); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) casExists(ctx context.Context, dgst string) (bool, error) {
	desc := ocispec.Descriptor{MediaType: defaultMediaType, Digest: digest.Digest(dgst)}
	if s.cas != nil {
		return s.cas.Exists(ctx, desc)
	}
	if s.roCAS != nil {
		return s.roCAS.Exists(ctx, desc)
	}
	return false, nil
}

func (s *Store) casResolve(ctx context.Context, dgst string) (ocispec.Descriptor, error) {
	if s.cas != nil {
		return s.cas.Resolve(ctx, dgst)
	}
	if s.roCAS != nil {
		return s.roCAS.Resolve(ctx, dgst)
	}
	return ocispec.Descriptor{}, errdef.ErrNotFound
}

func (s *Store) casFetch(ctx context.Context, desc ocispec.Descriptor) (io.ReadCloser, error) {
	if s.cas != nil {
		return s.cas.Fetch(ctx, desc)
	}
	if s.roCAS != nil {
		return s.roCAS.Fetch(ctx, desc)
	}
	return nil, errdef.ErrNotFound
}

// writeAndVerifyCAS writes r into ORAS CAS, verifies SHA-256 and byte length,
// tags and persists index.json BEFORE any bbolt catalog commit.
func (s *Store) writeAndVerifyCAS(ctx context.Context, dgst string, expectedSize int64, mediaType string, r io.Reader) (int64, error) {
	if !ValidDigest(dgst) {
		return 0, fmt.Errorf("artifact: invalid digest %q", dgst)
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if strings.TrimSpace(mediaType) == "" {
		mediaType = defaultMediaType
	}
	checkSize := expectedSize
	if checkSize < 0 {
		checkSize = 0
	}
	if err := s.checkWriteAllowed(ctx, checkSize); err != nil {
		return 0, err
	}

	// Unknown size (e.g. chunked HTTP stream): stage in cas/ingest, verify digest, then push.
	if expectedSize < 0 {
		ingestDir := filepath.Join(s.casDir, "ingest")
		if err := os.MkdirAll(ingestDir, 0o700); err != nil {
			return 0, err
		}
		tmp, err := os.CreateTemp(ingestDir, "stage_*")
		if err != nil {
			return 0, err
		}
		tmpName := tmp.Name()
		defer func() {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}()

		h := sha256.New()
		n, copyErr := io.Copy(io.MultiWriter(tmp, h), r)
		if copyErr != nil {
			return 0, copyErr
		}
		if err := s.checkWriteAllowed(ctx, n); err != nil {
			return 0, err
		}
		gotDigest := "sha256:" + hex.EncodeToString(h.Sum(nil))
		if gotDigest != dgst {
			return 0, fmt.Errorf("%w: uploaded %s != expected %s", ErrCorruptPayload, gotDigest, dgst)
		}
		if _, err := tmp.Seek(0, io.SeekStart); err != nil {
			return 0, err
		}
		return s.pushVerifiedStreamToCAS(ctx, dgst, n, mediaType, tmp)
	}

	return s.pushVerifiedStreamToCAS(ctx, dgst, expectedSize, mediaType, r)
}

type errTrackingReader struct {
	r   io.Reader
	n   int64
	err error
}

func (er *errTrackingReader) Read(p []byte) (int, error) {
	n, err := er.r.Read(p)
	er.n += int64(n)
	if err != nil && err != io.EOF {
		er.err = err
	}
	return n, err
}

func (s *Store) pushVerifiedStreamToCAS(ctx context.Context, dgst string, expectedSize int64, mediaType string, r io.Reader) (int64, error) {
	desc := ocispec.Descriptor{
		MediaType: mediaType,
		Digest:    digest.Digest(dgst),
		Size:      expectedSize,
	}

	exists, err := s.cas.Exists(ctx, desc)
	if err != nil {
		return 0, err
	}
	if exists {
		// Verify incoming stream matches digest and size, and verify on-disk CAS blob is intact.
		var buf bytes.Buffer
		h := sha256.New()
		// Stream without buffering huge payloads unless needed to heal a corrupted CAS file.
		n, err := io.Copy(h, r)
		if err != nil {
			return 0, err
		}
		got := "sha256:" + hex.EncodeToString(h.Sum(nil))
		if n != expectedSize || got != dgst {
			return 0, fmt.Errorf("%w: uploaded (%s, %d bytes) != expected (%s, %d bytes)",
				ErrCorruptPayload, got, n, dgst, expectedSize)
		}
		_ = buf
		if err := s.verifyExistingCASBlob(ctx, dgst, expectedSize); err != nil {
			return 0, err
		}
	} else {
		tr := &errTrackingReader{r: r}
		if pushErr := s.cas.Push(ctx, desc, tr); pushErr != nil {
			if errors.Is(pushErr, errdef.ErrAlreadyExists) {
				if err := s.verifyExistingCASBlob(ctx, dgst, expectedSize); err != nil {
					return 0, err
				}
			} else if tr.err != nil {
				return 0, tr.err
			} else {
				return 0, fmt.Errorf("%w: %v", ErrCorruptPayload, pushErr)
			}
		}
	}

	if err := s.cas.Tag(ctx, desc, desc.Digest.String()); err != nil {
		return 0, fmt.Errorf("artifact: tag cas blob %s: %w", dgst, err)
	}
	if err := s.cas.SaveIndex(); err != nil {
		return 0, fmt.Errorf("artifact: save cas index: %w", err)
	}
	if s.opts.AfterCASWriteHook != nil {
		if err := s.opts.AfterCASWriteHook(dgst); err != nil {
			return 0, err
		}
	}
	return expectedSize, nil
}

func (s *Store) verifyExistingCASBlob(ctx context.Context, dgst string, expectedSize int64) error {
	desc := ocispec.Descriptor{MediaType: defaultMediaType, Digest: digest.Digest(dgst), Size: expectedSize}
	rc, err := s.casFetch(ctx, desc)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrCorruptPayload, err)
	}
	defer rc.Close()
	h := sha256.New()
	n, err := io.Copy(h, rc)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrCorruptPayload, err)
	}
	if expectedSize >= 0 && n != expectedSize {
		return fmt.Errorf("%w: %s size %d != %d", ErrCorruptPayload, dgst, n, expectedSize)
	}
	if got := "sha256:" + hex.EncodeToString(h.Sum(nil)); got != dgst {
		return fmt.Errorf("%w: %s hashed to %s", ErrCorruptPayload, dgst, got)
	}
	return nil
}

func (s *Store) deleteCASBlobLocked(ctx context.Context, dgst string) error {
	if s.opts.DeleteBlobHook != nil {
		if err := s.opts.DeleteBlobHook(dgst); err != nil {
			return err
		}
	}
	desc, err := s.cas.Resolve(ctx, dgst)
	if err != nil {
		if errors.Is(err, errdef.ErrNotFound) {
			return nil
		}
		return err
	}
	if delErr := s.cas.Delete(ctx, desc); delErr != nil {
		if errors.Is(delErr, errdef.ErrNotFound) {
			return s.cas.SaveIndex()
		}
		// Restore index tag if physical delete failed (e.g. Windows sharing lock).
		if ok, _ := s.cas.Exists(ctx, desc); ok {
			_ = s.cas.Tag(ctx, desc, desc.Digest.String())
			_ = s.cas.SaveIndex()
		}
		return delErr
	}
	return s.cas.SaveIndex()
}

// PutBlob writes and SHA-256-verifies a raw payload into ORAS CAS before recording
// a staged put journal entry (or restoring PayloadPresent on an existing evicted record).
func (s *Store) PutBlob(ctx context.Context, dgst string, size int64, mediaType string, r io.Reader) (int64, error) {
	if err := s.enterOp(); err != nil {
		return 0, err
	}
	defer s.exitOp()

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	written, err := s.writeAndVerifyCAS(ctx, dgst, size, mediaType, r)
	if err != nil {
		return 0, err
	}

	now := s.now()
	if err := s.db.Update(func(tx *bolt.Tx) error {
		rb := tx.Bucket(bucketRecords)
		if raw := rb.Get([]byte(dgst)); raw != nil {
			var st RecordStatus
			if err := json.Unmarshal(raw, &st); err != nil {
				return err
			}
			st.PayloadPresent = true
			st.EvictedAt = nil
			st.Record.LastAccessedAt = now
			updated, err := json.Marshal(st)
			if err != nil {
				return err
			}
			if err := rb.Put([]byte(dgst), updated); err != nil {
				return err
			}
			return deleteJournalTx(tx, JournalOpPut, dgst)
		}
		return putJournalTx(tx, JournalEntry{
			Op:           JournalOpPut,
			Digest:       dgst,
			ServiceEpoch: s.epoch,
			StartedAt:    now,
			ExpiresAt:    now.Add(s.stagingTTL),
		})
	}); err != nil {
		return 0, err
	}

	return written, nil
}

// PutArtifact writes and SHA-256-verifies r into ORAS CAS (including SaveIndex)
// BEFORE committing rec into the bbolt catalog.
func (s *Store) PutArtifact(ctx context.Context, rec CatalogRecordV1, r io.Reader) (RecordStatus, error) {
	if err := s.enterOp(); err != nil {
		return RecordStatus{}, err
	}
	defer s.exitOp()

	now := s.now()
	if rec.SchemaVersion == "" {
		rec.SchemaVersion = CatalogRecordSchemaV1
	}
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = now
	}
	if rec.LastAccessedAt.IsZero() {
		rec.LastAccessedAt = rec.CreatedAt
	}
	if err := rec.Validate(); err != nil {
		return RecordStatus{}, err
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	if _, err := s.writeAndVerifyCAS(ctx, rec.Digest, rec.Size, rec.MediaType, r); err != nil {
		return RecordStatus{}, err
	}

	var out RecordStatus
	if err := s.db.Update(func(tx *bolt.Tx) error {
		var err error
		out, err = putRecordTx(tx, rec, true)
		if err != nil {
			return err
		}
		return deleteJournalTx(tx, JournalOpPut, rec.Digest)
	}); err != nil {
		return RecordStatus{}, err
	}
	return out, nil
}

// PutRecord commits a CatalogRecordV1 referencing a blob already verified in ORAS CAS.
func (s *Store) PutRecord(ctx context.Context, rec CatalogRecordV1) (RecordStatus, error) {
	if err := s.enterOp(); err != nil {
		return RecordStatus{}, err
	}
	defer s.exitOp()
	if s.readOnly {
		return RecordStatus{}, errors.New("artifact: store opened read-only")
	}

	now := s.now()
	if rec.SchemaVersion == "" {
		rec.SchemaVersion = CatalogRecordSchemaV1
	}
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = now
	}
	if rec.LastAccessedAt.IsZero() {
		rec.LastAccessedAt = rec.CreatedAt
	}
	if err := rec.Validate(); err != nil {
		return RecordStatus{}, err
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	if err := s.verifyExistingCASBlob(ctx, rec.Digest, rec.Size); err != nil {
		return RecordStatus{}, fmt.Errorf("artifact: cannot commit record before verified CAS blob: %w", err)
	}

	var out RecordStatus
	if err := s.db.Update(func(tx *bolt.Tx) error {
		var err error
		out, err = putRecordTx(tx, rec, true)
		if err != nil {
			return err
		}
		return deleteJournalTx(tx, JournalOpPut, rec.Digest)
	}); err != nil {
		return RecordStatus{}, err
	}
	return out, nil
}

func retentionRank(r RetentionClass) int {
	switch r {
	case RetentionProtectedRoot:
		return 3
	case RetentionReconstructible:
		return 2
	case RetentionScratch:
		return 1
	default:
		return 0
	}
}

func unionSortedStrings(a, b []string) []string {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(a)+len(b))
	var out []string
	for _, s := range a {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, s := range b {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	slices.Sort(out)
	return out
}

func logicalKey(name, version string) []byte {
	return []byte(name + "@" + version)
}

func putRecordTx(tx *bolt.Tx, rec CatalogRecordV1, payloadPresent bool) (RecordStatus, error) {
	rb := tx.Bucket(bucketRecords)
	lb := tx.Bucket(bucketLogical)
	if rb == nil || lb == nil {
		return RecordStatus{}, errors.New("artifact: catalog buckets not initialized")
	}

	merged := rec
	if existingRaw := rb.Get([]byte(rec.Digest)); existingRaw != nil {
		var prev RecordStatus
		if err := json.Unmarshal(existingRaw, &prev); err != nil {
			return RecordStatus{}, err
		}
		// Never downgrade retention or drop dependency/root edges on shared CAS digests.
		if retentionRank(prev.Record.Retention) > retentionRank(merged.Retention) {
			merged.Retention = prev.Record.Retention
		}
		merged.RetentionRoots = unionSortedStrings(prev.Record.RetentionRoots, merged.RetentionRoots)
		merged.Dependencies = unionSortedStrings(prev.Record.Dependencies, merged.Dependencies)
		if merged.Recipe == nil && prev.Record.Recipe != nil {
			merged.Recipe = prev.Record.Recipe
		}
		if !prev.Record.CreatedAt.IsZero() && prev.Record.CreatedAt.Before(merged.CreatedAt) {
			merged.CreatedAt = prev.Record.CreatedAt
		}
	}

	st := RecordStatus{
		Record:         merged,
		PayloadPresent: payloadPresent,
	}
	raw, err := json.Marshal(st)
	if err != nil {
		return RecordStatus{}, err
	}
	if err := rb.Put([]byte(merged.Digest), raw); err != nil {
		return RecordStatus{}, err
	}
	if err := lb.Put(logicalKey(rec.LogicalName, rec.LogicalVersion), []byte(merged.Digest)); err != nil {
		return RecordStatus{}, err
	}
	return st, nil
}

// GetRecordStatus returns the catalog record and payload presence state for dgst.
func (s *Store) GetRecordStatus(ctx context.Context, dgst string) (RecordStatus, error) {
	if err := s.enterOp(); err != nil {
		return RecordStatus{}, err
	}
	defer s.exitOp()
	if err := ctx.Err(); err != nil {
		return RecordStatus{}, err
	}
	if !ValidDigest(dgst) {
		return RecordStatus{}, fmt.Errorf("artifact: invalid digest %q", dgst)
	}

	var st RecordStatus
	err := s.db.View(func(tx *bolt.Tx) error {
		rb := tx.Bucket(bucketRecords)
		if rb == nil {
			return fmt.Errorf("%w: %s", ErrNotFound, dgst)
		}
		raw := rb.Get([]byte(dgst))
		if raw == nil {
			return fmt.Errorf("%w: %s", ErrNotFound, dgst)
		}
		return json.Unmarshal(raw, &st)
	})
	if err != nil {
		return RecordStatus{}, err
	}
	s.readersMu.Lock()
	if ts, ok := s.lastAccess[dgst]; ok && ts.After(st.Record.LastAccessedAt) {
		st.Record.LastAccessedAt = ts
	}
	s.readersMu.Unlock()
	return st, nil
}

// GetRecord returns the CatalogRecordV1 for dgst (even if its reconstructible payload was evicted).
func (s *Store) GetRecord(ctx context.Context, dgst string) (CatalogRecordV1, error) {
	st, err := s.GetRecordStatus(ctx, dgst)
	if err != nil {
		return CatalogRecordV1{}, err
	}
	return st.Record, nil
}

// LookupLogical resolves <name>@<version> to its RecordStatus.
func (s *Store) LookupLogical(ctx context.Context, name, version string) (RecordStatus, error) {
	if err := s.enterOp(); err != nil {
		return RecordStatus{}, err
	}
	defer s.exitOp()
	if err := ctx.Err(); err != nil {
		return RecordStatus{}, err
	}

	var dgst string
	if err := s.db.View(func(tx *bolt.Tx) error {
		lb := tx.Bucket(bucketLogical)
		if lb == nil {
			return fmt.Errorf("%w: %s@%s", ErrNotFound, name, version)
		}
		raw := lb.Get(logicalKey(name, version))
		if raw == nil {
			return fmt.Errorf("%w: %s@%s", ErrNotFound, name, version)
		}
		dgst = string(raw)
		return nil
	}); err != nil {
		return RecordStatus{}, err
	}
	return s.GetRecordStatus(ctx, dgst)
}

// ListRecords returns all catalog records sorted deterministically by digest.
func (s *Store) ListRecords(ctx context.Context) ([]RecordStatus, error) {
	if err := s.enterOp(); err != nil {
		return nil, err
	}
	defer s.exitOp()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var out []RecordStatus
	err := s.db.View(func(tx *bolt.Tx) error {
		rb := tx.Bucket(bucketRecords)
		if rb == nil {
			return nil
		}
		return rb.ForEach(func(_, v []byte) error {
			var st RecordStatus
			if err := json.Unmarshal(v, &st); err != nil {
				return err
			}
			s.readersMu.Lock()
			if ts, ok := s.lastAccess[st.Record.Digest]; ok && ts.After(st.Record.LastAccessedAt) {
				st.Record.LastAccessedAt = ts
			}
			s.readersMu.Unlock()
			out = append(out, st)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(out, func(a, b RecordStatus) int {
		return strings.Compare(a.Record.Digest, b.Record.Digest)
	})
	return out, nil
}

// putEvictedRecord records metadata for an evicted reconstructible artifact during
// portable bundle import without requiring the evicted payload blob in CAS.
func (s *Store) putEvictedRecord(ctx context.Context, rec CatalogRecordV1) (RecordStatus, error) {
	if err := s.enterOp(); err != nil {
		return RecordStatus{}, err
	}
	defer s.exitOp()
	if err := ctx.Err(); err != nil {
		return RecordStatus{}, err
	}
	if s.readOnly {
		return RecordStatus{}, errors.New("artifact: store opened read-only")
	}
	now := s.now()
	if rec.SchemaVersion == "" {
		rec.SchemaVersion = CatalogRecordSchemaV1
	}
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = now
	}
	if rec.LastAccessedAt.IsZero() {
		rec.LastAccessedAt = rec.CreatedAt
	}
	if err := rec.Validate(); err != nil {
		return RecordStatus{}, err
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	present, err := s.casExists(ctx, rec.Digest)
	if err != nil {
		return RecordStatus{}, err
	}
	var out RecordStatus
	if err := s.db.Update(func(tx *bolt.Tx) error {
		var err error
		out, err = putRecordTx(tx, rec, present)
		if err != nil {
			return err
		}
		if !present {
			evAt := now
			out.EvictedAt = &evAt
			raw, err := json.Marshal(out)
			if err != nil {
				return err
			}
			return tx.Bucket(bucketRecords).Put([]byte(out.Record.Digest), raw)
		}
		return nil
	}); err != nil {
		return RecordStatus{}, err
	}
	return out, nil
}

// HasBlob reports whether dgst currently exists physically in ORAS CAS.
func (s *Store) HasBlob(ctx context.Context, dgst string) (bool, error) {
	if err := s.enterOp(); err != nil {
		return false, err
	}
	defer s.exitOp()
	if !ValidDigest(dgst) {
		return false, fmt.Errorf("artifact: invalid digest %q", dgst)
	}
	return s.casExists(ctx, dgst)
}

func (s *Store) addOpenReader(dgst string) {
	s.readersMu.Lock()
	s.openReaders[dgst]++
	s.lastAccess[dgst] = s.now()
	s.readersMu.Unlock()
}

func (s *Store) releaseOpenReader(dgst string) {
	s.readersMu.Lock()
	if n := s.openReaders[dgst]; n <= 1 {
		delete(s.openReaders, dgst)
	} else {
		s.openReaders[dgst] = n - 1
	}
	s.readersMu.Unlock()
}

func (s *Store) hasOpenReaders(dgst string) bool {
	s.readersMu.Lock()
	n := s.openReaders[dgst]
	s.readersMu.Unlock()
	return n > 0
}

type verifyingReadCloser struct {
	rc           io.ReadCloser
	dgst         string
	expectedSize int64
	hasher       hash.Hash
	bytesRead    int64
	verifiedEOF  bool
	closeOnce    sync.Once
	onClose      func()
	closeErr     error
}

func (v *verifyingReadCloser) Read(p []byte) (int, error) {
	n, err := v.rc.Read(p)
	if n > 0 {
		_, _ = v.hasher.Write(p[:n])
		v.bytesRead += int64(n)
		if v.expectedSize >= 0 && v.bytesRead > v.expectedSize {
			return 0, fmt.Errorf("%w: %s exceeded expected size %d", ErrCorruptPayload, v.dgst, v.expectedSize)
		}
	}
	if err == io.EOF {
		if !v.verifiedEOF {
			if v.expectedSize >= 0 && v.bytesRead != v.expectedSize {
				return 0, fmt.Errorf("%w: %s truncated (%d != %d bytes)", ErrCorruptPayload, v.dgst, v.bytesRead, v.expectedSize)
			}
			got := "sha256:" + hex.EncodeToString(v.hasher.Sum(nil))
			if got != v.dgst {
				return 0, fmt.Errorf("%w: %s hashed to %s", ErrCorruptPayload, v.dgst, got)
			}
			v.verifiedEOF = true
		}
		return n, io.EOF
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return 0, fmt.Errorf("%w: %v", ErrCorruptPayload, err)
	}
	return n, err
}

func (v *verifyingReadCloser) Close() error {
	v.closeOnce.Do(func() {
		v.closeErr = v.rc.Close()
		if v.onClose != nil {
			v.onClose()
		}
	})
	return v.closeErr
}

// OpenBlob opens a streaming reader for dgst that verifies byte length and SHA-256
// at EOF, returning ErrCorruptPayload on mismatch, truncation, or unexpectedly missing CAS file.
func (s *Store) OpenBlob(ctx context.Context, dgst string) (io.ReadCloser, error) {
	rc, _, _, err := s.openBlobWithMeta(ctx, dgst)
	return rc, err
}

func (s *Store) openBlobWithMeta(ctx context.Context, dgst string) (io.ReadCloser, int64, string, error) {
	if err := s.enterOp(); err != nil {
		return nil, 0, "", err
	}
	if err := ctx.Err(); err != nil {
		s.exitOp()
		return nil, 0, "", err
	}
	if !ValidDigest(dgst) {
		s.exitOp()
		return nil, 0, "", fmt.Errorf("artifact: invalid digest %q", dgst)
	}

	s.addOpenReader(dgst)
	cleanupOnErr := func() {
		s.releaseOpenReader(dgst)
		s.exitOp()
	}

	expectedSize := int64(-1)
	mediaType := defaultMediaType
	expectedPresent := false
	knownEvicted := false

	if err := s.db.View(func(tx *bolt.Tx) error {
		if rb := tx.Bucket(bucketRecords); rb != nil {
			if raw := rb.Get([]byte(dgst)); raw != nil {
				var st RecordStatus
				if err := json.Unmarshal(raw, &st); err != nil {
					return err
				}
				expectedSize = st.Record.Size
				if st.Record.MediaType != "" {
					mediaType = st.Record.MediaType
				}
				if st.PayloadPresent {
					expectedPresent = true
				} else {
					knownEvicted = true
				}
			}
		}
		if !expectedPresent && !knownEvicted {
			snap, err := s.readCatalogSnapshotTx(tx, s.now())
			if err == nil && snap.referencedDigests[dgst] {
				expectedPresent = true
			}
		}
		return nil
	}); err != nil {
		cleanupOnErr()
		return nil, 0, "", err
	}

	// Check if OCI index knows about this digest even if catalog record wasn't added yet.
	if resolved, err := s.casResolve(ctx, dgst); err == nil {
		if expectedSize < 0 && resolved.Size >= 0 {
			expectedSize = resolved.Size
		}
		if mediaType == defaultMediaType && resolved.MediaType != "" {
			mediaType = resolved.MediaType
		}
		if !knownEvicted {
			expectedPresent = true
		}
	}

	desc := ocispec.Descriptor{
		MediaType: mediaType,
		Digest:    digest.Digest(dgst),
		Size:      expectedSize,
	}
	rc, err := s.casFetch(ctx, desc)
	if err != nil {
		cleanupOnErr()
		if errors.Is(err, errdef.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
			if expectedPresent {
				return nil, 0, "", fmt.Errorf("%w: missing CAS file for %s", ErrCorruptPayload, dgst)
			}
			if knownEvicted {
				return nil, 0, "", fmt.Errorf("%w: %s", ErrPayloadEvicted, dgst)
			}
			return nil, 0, "", fmt.Errorf("%w: %s", ErrNotFound, dgst)
		}
		return nil, 0, "", err
	}

	vrc := &verifyingReadCloser{
		rc:           rc,
		dgst:         dgst,
		expectedSize: expectedSize,
		hasher:       sha256.New(),
		onClose: func() {
			s.releaseOpenReader(dgst)
			s.exitOp()
		},
	}
	return vrc, expectedSize, mediaType, nil
}

// FetchVerified reads the entire blob for dgst and verifies its SHA-256 digest.
func (s *Store) FetchVerified(ctx context.Context, dgst string) ([]byte, error) {
	rc, err := s.OpenBlob(ctx, dgst)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, err
	}
	if DigestBytes(data) != dgst {
		return nil, fmt.Errorf("%w: %s digest mismatch", ErrCorruptPayload, dgst)
	}
	return data, nil
}

// PublishModule records an immutable module publication, enforcing CheckImmutablePublication.
func (s *Store) PublishModule(ctx context.Context, pub PublicationRecord) (bool, error) {
	if err := s.enterOp(); err != nil {
		return false, err
	}
	defer s.exitOp()
	if s.readOnly {
		return false, errors.New("artifact: store opened read-only")
	}
	if pub.SchemaVersion == "" {
		pub.SchemaVersion = PublicationSchemaV1
	}
	if pub.PublishedAt.IsZero() {
		pub.PublishedAt = s.now()
	}
	if err := pub.Validate(); err != nil {
		return false, err
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	key := []byte(pub.Module + "@" + pub.Version)
	var idempotent bool
	err := s.db.Update(func(tx *bolt.Tx) error {
		pb := tx.Bucket(bucketPublications)
		if existingRaw := pb.Get(key); existingRaw != nil {
			var existing PublicationRecord
			if err := json.Unmarshal(existingRaw, &existing); err != nil {
				return err
			}
			idem, err := CheckImmutablePublication(existing, pub)
			if err != nil {
				return err
			}
			idempotent = idem
			return nil
		}
		// Require .info and .mod in CAS, and either .zip in CAS or a valid reconstructible .zip record.
		rb := tx.Bucket(bucketRecords)
		for _, d := range []string{pub.InfoDigest, pub.ModDigest, pub.ZipDigest} {
			ok, err := s.cas.Exists(ctx, ocispec.Descriptor{MediaType: defaultMediaType, Digest: digest.Digest(d)})
			if err != nil {
				return err
			}
			if !ok && d == pub.ZipDigest && rb != nil {
				if rawRec := rb.Get([]byte(d)); rawRec != nil {
					var st RecordStatus
					if json.Unmarshal(rawRec, &st) == nil && st.Record.Retention == RetentionReconstructible && st.Record.Recipe != nil {
						inputsOK := true
						for _, inD := range st.Record.Recipe.InputDigests {
							inPresent, inErr := s.cas.Exists(ctx, ocispec.Descriptor{MediaType: defaultMediaType, Digest: digest.Digest(inD)})
							if inErr != nil || !inPresent {
								inputsOK = false
								break
							}
						}
						if inputsOK {
							ok = true
						}
					}
				}
			}
			if !ok {
				return fmt.Errorf("artifact: publication blob %s missing from CAS: %w", d, ErrNotFound)
			}
			_ = deleteJournalTx(tx, JournalOpPut, d)
		}
		raw, err := json.Marshal(pub)
		if err != nil {
			return err
		}
		return pb.Put(key, raw)
	})
	return idempotent, err
}

func (s *Store) GetPublication(ctx context.Context, modPath, version string) (PublicationRecord, error) {
	if err := s.enterOp(); err != nil {
		return PublicationRecord{}, err
	}
	defer s.exitOp()
	if err := ctx.Err(); err != nil {
		return PublicationRecord{}, err
	}

	key := []byte(modPath + "@" + version)
	var pub PublicationRecord
	err := s.db.View(func(tx *bolt.Tx) error {
		pb := tx.Bucket(bucketPublications)
		if pb == nil {
			return fmt.Errorf("%w: %s@%s", ErrNotFound, modPath, version)
		}
		raw := pb.Get(key)
		if raw == nil {
			return fmt.Errorf("%w: %s@%s", ErrNotFound, modPath, version)
		}
		return json.Unmarshal(raw, &pub)
	})
	return pub, err
}

func (s *Store) ListPublications(ctx context.Context) ([]PublicationRecord, error) {
	if err := s.enterOp(); err != nil {
		return nil, err
	}
	defer s.exitOp()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var out []PublicationRecord
	err := s.db.View(func(tx *bolt.Tx) error {
		pb := tx.Bucket(bucketPublications)
		if pb == nil {
			return nil
		}
		return pb.ForEach(func(_, v []byte) error {
			var pub PublicationRecord
			if err := json.Unmarshal(v, &pub); err != nil {
				return err
			}
			out = append(out, pub)
			return nil
		})
	})
	return out, err
}

func manifestKey(gen uint64) []byte {
	return []byte(fmt.Sprintf("%020d", gen))
}

// PutManifest commits an additive protected ManifestV1 generation after verifying
// all root descriptors exist in ORAS CAS. Baseline generations are never retired.
func (s *Store) PutManifest(ctx context.Context, m ManifestV1) error {
	if err := s.enterOp(); err != nil {
		return err
	}
	defer s.exitOp()
	if s.readOnly {
		return errors.New("artifact: store opened read-only")
	}
	if err := m.Validate(); err != nil {
		return err
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	for _, r := range m.Roots {
		if err := s.verifyExistingCASBlob(ctx, r.Digest, r.Size); err != nil {
			return fmt.Errorf("artifact: manifest root %s@%s (%s) missing or corrupt in CAS: %w", r.Name, r.Version, r.Digest, err)
		}
		for _, dep := range r.Deps {
			ok, err := s.casExists(ctx, dep)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("artifact: manifest root dependency %s missing in CAS: %w", dep, ErrNotFound)
			}
		}
	}

	canonical, err := m.CanonicalBytes()
	if err != nil {
		return err
	}
	newDigest := DigestBytes(canonical)

	return s.db.Update(func(tx *bolt.Tx) error {
		mb := tx.Bucket(bucketManifests)
		key := manifestKey(m.Generation)
		if existingRaw := mb.Get(key); existingRaw != nil {
			var existing ManifestV1
			if err := json.Unmarshal(existingRaw, &existing); err != nil {
				return err
			}
			existingDigest, err := existing.Digest()
			if err != nil {
				return err
			}
			if existingDigest == newDigest {
				return nil
			}
			return fmt.Errorf("artifact: manifest generation %d already exists with digest %s", m.Generation, existingDigest)
		}
		genRootTag := fmt.Sprintf("manifest:gen-%d", m.Generation)
		rb := tx.Bucket(bucketRecords)
		pinRecordRoot := func(dgst string) {
			if rb == nil {
				return
			}
			raw := rb.Get([]byte(dgst))
			if raw == nil {
				return
			}
			var st RecordStatus
			if json.Unmarshal(raw, &st) != nil {
				return
			}
			st.Record.Retention = RetentionProtectedRoot
			st.Record.RetentionRoots = unionSortedStrings(st.Record.RetentionRoots, []string{genRootTag})
			if b, err := json.Marshal(st); err == nil {
				_ = rb.Put([]byte(dgst), b)
			}
		}
		for _, r := range m.Roots {
			_ = deleteJournalTx(tx, JournalOpPut, r.Digest)
			pinRecordRoot(r.Digest)
			for _, dep := range r.Deps {
				_ = deleteJournalTx(tx, JournalOpPut, dep)
				pinRecordRoot(dep)
			}
		}
		return mb.Put(key, canonical)
	})
}

func (s *Store) GetManifest(ctx context.Context, generation uint64) (ManifestV1, error) {
	if err := s.enterOp(); err != nil {
		return ManifestV1{}, err
	}
	defer s.exitOp()
	if err := ctx.Err(); err != nil {
		return ManifestV1{}, err
	}

	var m ManifestV1
	err := s.db.View(func(tx *bolt.Tx) error {
		mb := tx.Bucket(bucketManifests)
		if mb == nil {
			return fmt.Errorf("%w: manifest generation %d", ErrNotFound, generation)
		}
		raw := mb.Get(manifestKey(generation))
		if raw == nil {
			return fmt.Errorf("%w: manifest generation %d", ErrNotFound, generation)
		}
		return json.Unmarshal(raw, &m)
	})
	return m, err
}

func (s *Store) LatestManifest(ctx context.Context) (ManifestV1, error) {
	if err := s.enterOp(); err != nil {
		return ManifestV1{}, err
	}
	defer s.exitOp()
	if err := ctx.Err(); err != nil {
		return ManifestV1{}, err
	}

	var m ManifestV1
	err := s.db.View(func(tx *bolt.Tx) error {
		mb := tx.Bucket(bucketManifests)
		if mb == nil {
			return fmt.Errorf("%w: no manifests", ErrNotFound)
		}
		k, v := mb.Cursor().Last()
		if k == nil {
			return fmt.Errorf("%w: no manifests", ErrNotFound)
		}
		return json.Unmarshal(v, &m)
	})
	return m, err
}

func (s *Store) ListManifests(ctx context.Context) ([]ManifestV1, error) {
	if err := s.enterOp(); err != nil {
		return nil, err
	}
	defer s.exitOp()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var out []ManifestV1
	err := s.db.View(func(tx *bolt.Tx) error {
		mb := tx.Bucket(bucketManifests)
		if mb == nil {
			return nil
		}
		return mb.ForEach(func(_, v []byte) error {
			var m ManifestV1
			if err := json.Unmarshal(v, &m); err != nil {
				return err
			}
			out = append(out, m)
			return nil
		})
	})
	return out, err
}

func randomHex(numBytes int) (string, error) {
	b := make([]byte, numBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// AcquireLease persists a read/build lease bound to the current service_epoch and holder identity.
func (s *Store) AcquireLease(ctx context.Context, holderPID int, holderStartID string, ttl time.Duration, digests []string) (Lease, error) {
	if err := s.enterOp(); err != nil {
		return Lease{}, err
	}
	defer s.exitOp()
	if s.readOnly {
		return Lease{}, errors.New("artifact: store opened read-only")
	}
	if ttl <= 0 {
		return Lease{}, errors.New("artifact: lease ttl must be > 0")
	}
	if len(digests) == 0 {
		return Lease{}, errors.New("artifact: lease requires at least one digest")
	}
	dedup := make([]string, 0, len(digests))
	seen := make(map[string]bool, len(digests))
	for _, d := range digests {
		if !ValidDigest(d) {
			return Lease{}, fmt.Errorf("artifact: invalid lease digest %q", d)
		}
		if !seen[d] {
			seen[d] = true
			dedup = append(dedup, d)
		}
	}
	if holderPID < 0 {
		return Lease{}, errors.New("artifact: invalid holder pid")
	}

	idHex, err := randomHex(12)
	if err != nil {
		return Lease{}, err
	}
	now := s.now()
	lease := Lease{
		ID:            "lease-" + idHex,
		HolderPID:     holderPID,
		HolderStartID: holderStartID,
		ServiceEpoch:  s.epoch,
		Digests:       dedup,
		ExpiresAt:     now.Add(ttl),
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	if err := s.db.Update(func(tx *bolt.Tx) error {
		lb := tx.Bucket(bucketLeases)
		// Opportunistically garbage-collect dead/stale leases on write.
		_ = pruneInactiveLeasesTx(tx, now, s.epoch, s.alive)
		raw, err := json.Marshal(lease)
		if err != nil {
			return err
		}
		return lb.Put([]byte(lease.ID), raw)
	}); err != nil {
		return Lease{}, err
	}
	return lease, nil
}

// RenewLease extends an active lease's ExpiresAt by ttl from now.
func (s *Store) RenewLease(ctx context.Context, leaseID string, ttl time.Duration) (Lease, error) {
	if err := s.enterOp(); err != nil {
		return Lease{}, err
	}
	defer s.exitOp()
	if s.readOnly {
		return Lease{}, errors.New("artifact: store opened read-only")
	}
	if ttl <= 0 {
		return Lease{}, errors.New("artifact: lease ttl must be > 0")
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	now := s.now()
	var updated Lease
	err := s.db.Update(func(tx *bolt.Tx) error {
		lb := tx.Bucket(bucketLeases)
		raw := lb.Get([]byte(leaseID))
		if raw == nil {
			return fmt.Errorf("%w: lease %s", ErrNotFound, leaseID)
		}
		if err := json.Unmarshal(raw, &updated); err != nil {
			return err
		}
		if !updated.ActiveAt(now, s.epoch, s.alive) {
			_ = lb.Delete([]byte(leaseID))
			return fmt.Errorf("%w: lease %s expired or lost authority", ErrNotFound, leaseID)
		}
		updated.ExpiresAt = now.Add(ttl)
		b, err := json.Marshal(updated)
		if err != nil {
			return err
		}
		return lb.Put([]byte(leaseID), b)
	})
	return updated, err
}

// ReleaseLease removes leaseID from the catalog (idempotent if already absent).
func (s *Store) ReleaseLease(ctx context.Context, leaseID string) error {
	if err := s.enterOp(); err != nil {
		return err
	}
	defer s.exitOp()
	if s.readOnly {
		return errors.New("artifact: store opened read-only")
	}
	if strings.TrimSpace(leaseID) == "" {
		return nil
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	return s.db.Update(func(tx *bolt.Tx) error {
		lb := tx.Bucket(bucketLeases)
		if lb == nil {
			return nil
		}
		return lb.Delete([]byte(leaseID))
	})
}

// ListActiveLeases returns all leases holding authority at now under the current service_epoch.
func (s *Store) ListActiveLeases(ctx context.Context) ([]Lease, error) {
	if err := s.enterOp(); err != nil {
		return nil, err
	}
	defer s.exitOp()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	now := s.now()
	var active []Lease
	err := s.db.View(func(tx *bolt.Tx) error {
		var err error
		active, err = listActiveLeasesTx(tx, now, s.epoch, s.alive)
		return err
	})
	return active, err
}

func listActiveLeasesTx(tx *bolt.Tx, now time.Time, epoch uint64, alive AliveFunc) ([]Lease, error) {
	lb := tx.Bucket(bucketLeases)
	if lb == nil {
		return nil, nil
	}
	var out []Lease
	err := lb.ForEach(func(_, v []byte) error {
		var l Lease
		if err := json.Unmarshal(v, &l); err != nil {
			return err
		}
		if l.ActiveAt(now, epoch, alive) {
			out = append(out, l)
		}
		return nil
	})
	return out, err
}

func pruneInactiveLeasesTx(tx *bolt.Tx, now time.Time, epoch uint64, alive AliveFunc) error {
	lb := tx.Bucket(bucketLeases)
	if lb == nil {
		return nil
	}
	var staleKeys [][]byte
	_ = lb.ForEach(func(k, v []byte) error {
		var l Lease
		if err := json.Unmarshal(v, &l); err != nil || !l.ActiveAt(now, epoch, alive) {
			staleKeys = append(staleKeys, bytes.Clone(k))
		}
		return nil
	})
	for _, k := range staleKeys {
		if err := lb.Delete(k); err != nil {
			return err
		}
	}
	return nil
}

// WithReadLease acquires a lease over digests before invoking fn and releases it on return.
func (s *Store) WithReadLease(ctx context.Context, holderPID int, holderStartID string, ttl time.Duration, digests []string, fn func(context.Context) error) error {
	if holderPID <= 0 {
		holderPID, holderStartID = CurrentProcessIdentity(ctx)
	}
	lease, err := s.AcquireLease(ctx, holderPID, holderStartID, ttl, digests)
	if err != nil {
		return err
	}
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.ReleaseLease(releaseCtx, lease.ID)
	}()
	return fn(ctx)
}

type catalogSnapshot struct {
	records           map[string]RecordStatus
	protectedClosure  map[string]bool
	retainedDigests   map[string]bool
	referencedDigests map[string]bool
	activeLeases      []Lease
}

func (cs catalogSnapshot) hasActiveLease(dgst string) bool {
	for _, l := range cs.activeLeases {
		if slices.Contains(l.Digests, dgst) {
			return true
		}
	}
	return false
}

func (s *Store) readCatalogSnapshotTx(tx *bolt.Tx, now time.Time) (catalogSnapshot, error) {
	snap := catalogSnapshot{
		records:           make(map[string]RecordStatus),
		protectedClosure:  make(map[string]bool),
		retainedDigests:   make(map[string]bool),
		referencedDigests: make(map[string]bool),
	}

	ctx := context.Background()

	// 1. Load records and determine physically present blobs.
	if rb := tx.Bucket(bucketRecords); rb != nil {
		if err := rb.ForEach(func(_, v []byte) error {
			var st RecordStatus
			if err := json.Unmarshal(v, &st); err != nil {
				return err
			}
			s.readersMu.Lock()
			if ts, ok := s.lastAccess[st.Record.Digest]; ok && ts.After(st.Record.LastAccessedAt) {
				st.Record.LastAccessedAt = ts
			}
			s.readersMu.Unlock()

			snap.records[st.Record.Digest] = st
			if st.PayloadPresent {
				snap.referencedDigests[st.Record.Digest] = true
				if ok, err := s.casExists(ctx, st.Record.Digest); err == nil && ok {
					snap.retainedDigests[st.Record.Digest] = true
				}
			}
			return nil
		}); err != nil {
			return snap, err
		}
	}

	var queue []string
	enqueueProtected := func(d string) {
		if ValidDigest(d) && !snap.protectedClosure[d] {
			snap.protectedClosure[d] = true
			queue = append(queue, d)
		}
	}

	// 2. Seed protectedClosure from all ManifestV1 generations (additive; never auto-retired).
	if mb := tx.Bucket(bucketManifests); mb != nil {
		if err := mb.ForEach(func(_, v []byte) error {
			var m ManifestV1
			if err := json.Unmarshal(v, &m); err != nil {
				return err
			}
			for _, d := range []string{m.ProfileDigest, m.ProductOverlaySHA, m.BuildImageDigest} {
				if ValidDigest(d) {
					snap.referencedDigests[d] = true
					enqueueProtected(d)
					if ok, err := s.casExists(ctx, d); err == nil && ok {
						snap.retainedDigests[d] = true
					}
				}
			}
			for _, r := range m.Roots {
				snap.referencedDigests[r.Digest] = true
				enqueueProtected(r.Digest)
				if ok, err := s.casExists(ctx, r.Digest); err == nil && ok {
					snap.retainedDigests[r.Digest] = true
				}
				for _, dep := range r.Deps {
					snap.referencedDigests[dep] = true
					enqueueProtected(dep)
					if ok, err := s.casExists(ctx, dep); err == nil && ok {
						snap.retainedDigests[dep] = true
					}
				}
			}
			return nil
		}); err != nil {
			return snap, err
		}
	}

	// 3. Seed protectedClosure from PublicationRecord non-evictable .info/.mod blobs
	// (and .zip if no reconstructible/scratch record governs .zip).
	if pb := tx.Bucket(bucketPublications); pb != nil {
		if err := pb.ForEach(func(_, v []byte) error {
			var pub PublicationRecord
			if err := json.Unmarshal(v, &pub); err != nil {
				return err
			}
			for _, d := range []string{pub.InfoDigest, pub.ModDigest} {
				snap.referencedDigests[d] = true
				enqueueProtected(d)
				if ok, err := s.casExists(ctx, d); err == nil && ok {
					snap.retainedDigests[d] = true
				}
			}
			if recSt, hasRec := snap.records[pub.ZipDigest]; !hasRec {
				snap.referencedDigests[pub.ZipDigest] = true
				enqueueProtected(pub.ZipDigest)
				if ok, err := s.casExists(ctx, pub.ZipDigest); err == nil && ok {
					snap.retainedDigests[pub.ZipDigest] = true
				}
			} else if recSt.PayloadPresent {
				snap.referencedDigests[pub.ZipDigest] = true
			}
			return nil
		}); err != nil {
			return snap, err
		}
	}

	// 4. Seed protectedClosure from protected CatalogRecordV1 entries and retained recipe inputs.
	for dgst, st := range snap.records {
		rec := st.Record
		if rec.Retention == RetentionProtectedRoot || len(rec.RetentionRoots) > 0 {
			enqueueProtected(dgst)
		}
		if rec.Retention == RetentionReconstructible && rec.Recipe != nil {
			for _, in := range rec.Recipe.InputDigests {
				// Only mark physically retained inputs as protected so missing inputs
				// still fail CanEvict's retained-input check while present inputs are never pruned!
				if snap.retainedDigests[in] {
					enqueueProtected(in)
				} else if ok, err := s.casExists(ctx, in); err == nil && ok {
					snap.retainedDigests[in] = true
					snap.referencedDigests[in] = true
					enqueueProtected(in)
				}
			}
		}
	}

	// 5. BFS expansion across CatalogRecordV1.Dependencies.
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if st, ok := snap.records[cur]; ok {
			for _, dep := range st.Record.Dependencies {
				snap.referencedDigests[dep] = true
				enqueueProtected(dep)
				if !snap.retainedDigests[dep] {
					if ok, err := s.casExists(ctx, dep); err == nil && ok {
						snap.retainedDigests[dep] = true
					}
				}
			}
		}
	}

	// 6. Active leases in current epoch.
	if s.epoch > 0 {
		leases, err := listActiveLeasesTx(tx, now, s.epoch, s.alive)
		if err != nil {
			return snap, err
		}
		snap.activeLeases = leases
	}

	return snap, nil
}

func (s *Store) canEvictSnapshot(rec CatalogRecordV1, snap catalogSnapshot, now time.Time) (bool, string) {
	if rec.Retention == RetentionReconstructible && rec.Recipe != nil {
		for _, in := range rec.Recipe.InputDigests {
			if !snap.retainedDigests[in] {
				return false, "missing retained recipe input " + in
			}
		}
	}
	return CanEvict(rec, snap.protectedClosure, snap.retainedDigests, snap.activeLeases, s.epoch, now, s.alive)
}

// DryRunPrune returns eligible evictable CAS payload items without mutating CAS or catalog.
func (s *Store) DryRunPrune(ctx context.Context, older time.Duration) ([]prune.Item, error) {
	if err := s.enterOp(); err != nil {
		return nil, err
	}
	defer s.exitOp()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	now := s.now()
	var snap catalogSnapshot
	if err := s.db.View(func(tx *bolt.Tx) error {
		var err error
		snap, err = s.readCatalogSnapshotTx(tx, now)
		return err
	}); err != nil {
		return nil, err
	}

	var digests []string
	for dgst := range snap.records {
		digests = append(digests, dgst)
	}
	slices.Sort(digests)

	items := make([]prune.Item, 0)
	for _, dgst := range digests {
		st := snap.records[dgst]
		if !st.PayloadPresent {
			continue
		}
		if older > 0 {
			ref := st.Record.LastAccessedAt
			if ref.IsZero() {
				ref = st.Record.CreatedAt
			}
			if !ref.IsZero() && now.Sub(ref) < older {
				continue
			}
		}
		ok, reason := s.canEvictSnapshot(st.Record, snap, now)
		if !ok {
			continue
		}
		items = append(items, prune.Item{
			Noun:   "artifact",
			ID:     dgst,
			Reason: fmt.Sprintf("%s (%s@%s)", reason, st.Record.LogicalName, st.Record.LogicalVersion),
			Size:   st.Record.Size,
		})
	}
	return items, nil
}

// ApplyPrune re-verifies protectedClosure, retainedDigests, active leases, and open readers
// inside the serialized writer path immediately before deleting each CAS blob.
func (s *Store) ApplyPrune(ctx context.Context, items []prune.Item) (prune.Counts, error) {
	if err := s.enterOp(); err != nil {
		return prune.Counts{}, err
	}
	defer s.exitOp()
	if s.readOnly {
		return prune.Counts{}, errors.New("artifact: store opened read-only")
	}

	for _, item := range items {
		if item.Noun != "" && item.Noun != "artifact" {
			return prune.Counts{}, fmt.Errorf("artifact prune: foreign item noun %q (%s)", item.Noun, item.ID)
		}
	}
	if len(items) == 0 {
		return prune.Counts{}, nil
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	var counts prune.Counts
	for _, item := range items {
		if err := ctx.Err(); err != nil {
			return counts, err
		}
		dgst := item.ID
		if !ValidDigest(dgst) {
			continue
		}
		// Never delete a CAS blob while an in-process Fetch reader is still open.
		if s.hasOpenReaders(dgst) {
			continue
		}

		now := s.now()
		var eligible bool
		var blobSize int64

		// Step 1: Recheck closure, leases, and recipe inputs under bbolt transaction,
		// and journal pending evict BEFORE touching CAS on disk.
		if err := s.db.Update(func(tx *bolt.Tx) error {
			_ = pruneInactiveLeasesTx(tx, now, s.epoch, s.alive)
			snap, err := s.readCatalogSnapshotTx(tx, now)
			if err != nil {
				return err
			}
			st, ok := snap.records[dgst]
			if !ok || !st.PayloadPresent {
				return nil
			}
			canEvict, _ := s.canEvictSnapshot(st.Record, snap, now)
			if !canEvict || s.hasOpenReaders(dgst) {
				return nil
			}
			eligible = true
			blobSize = st.Record.Size
			return putJournalTx(tx, JournalEntry{
				Op:           JournalOpEvict,
				Digest:       dgst,
				ServiceEpoch: s.epoch,
				StartedAt:    now,
			})
		}); err != nil {
			return counts, err
		}
		if !eligible {
			continue
		}

		// Step 2: Delete physical CAS blob (outside bbolt tx, serialized by writeMu).
		if err := s.deleteCASBlobLocked(ctx, dgst); err != nil {
			if isSharingOrLockError(err) {
				// Open-file / sharing contention (e.g. Windows NTFS): roll back pending
				// journal entry and keep PayloadPresent=true so catalog stays consistent.
				_ = s.db.Update(func(tx *bolt.Tx) error {
					return deleteJournalTx(tx, JournalOpEvict, dgst)
				})
				continue
			}
			// Non-sharing failure (e.g. crash/I/O fault): leave evict journal entry
			// in bbolt so startup Reconcile completes the eviction cleanly on reopen.
			return counts, err
		}

		// Step 3: Mark payload evicted in records while preserving CatalogRecordV1 + Recipe,
		// and clear the completed evict journal entry.
		evictedAt := now
		if err := s.db.Update(func(tx *bolt.Tx) error {
			rb := tx.Bucket(bucketRecords)
			if raw := rb.Get([]byte(dgst)); raw != nil {
				var st RecordStatus
				if err := json.Unmarshal(raw, &st); err != nil {
					return err
				}
				st.PayloadPresent = false
				st.EvictedAt = &evictedAt
				updated, err := json.Marshal(st)
				if err != nil {
					return err
				}
				if err := rb.Put([]byte(dgst), updated); err != nil {
					return err
				}
			}
			return deleteJournalTx(tx, JournalOpEvict, dgst)
		}); err != nil {
			return counts, err
		}

		counts.Removed++
		counts.Bytes += blobSize
	}
	return counts, nil
}

// Status returns a point-in-time summary of the store.
func (s *Store) Status(ctx context.Context) (StoreStatus, error) {
	if err := s.enterOp(); err != nil {
		return StoreStatus{}, err
	}
	defer s.exitOp()
	if err := ctx.Err(); err != nil {
		return StoreStatus{}, err
	}

	now := s.now()
	out := StoreStatus{
		ServiceEpoch: s.epoch,
		PID:          os.Getpid(),
	}
	err := s.db.View(func(tx *bolt.Tx) error {
		if rb := tx.Bucket(bucketRecords); rb != nil {
			_ = rb.ForEach(func(_, v []byte) error {
				out.RecordCount++
				var st RecordStatus
				if json.Unmarshal(v, &st) == nil && !st.PayloadPresent {
					out.EvictedCount++
				}
				return nil
			})
		}
		if pb := tx.Bucket(bucketPublications); pb != nil {
			out.PublicationCount = pb.Stats().KeyN
		}
		if mb := tx.Bucket(bucketManifests); mb != nil {
			out.ManifestCount = mb.Stats().KeyN
			if k, v := mb.Cursor().Last(); k != nil {
				var m ManifestV1
				if json.Unmarshal(v, &m) == nil {
					out.LatestGeneration = m.Generation
				}
			}
		}
		if s.epoch > 0 {
			leases, err := listActiveLeasesTx(tx, now, s.epoch, s.alive)
			if err != nil {
				return err
			}
			out.ActiveLeaseCount = len(leases)
		}
		return nil
	})
	return out, err
}

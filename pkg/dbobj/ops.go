package dbobj

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/qiangli/yoke/external/rclone"
	"github.com/qiangli/yoke/pkg/binmgr"
	"github.com/qiangli/yoke/pkg/dblifecycle"
	"github.com/qiangli/yoke/pkg/dbstore"
	"github.com/qiangli/yoke/pkg/dbstore/sqlitedb"
)

func init() {
	allOps := []string{
		OpBucketCreate, OpBucketAdd,
		OpBucketDrop, OpBucketRm,
		OpBucketList,
		OpList, OpStat, OpGet, OpPut, OpDelete, OpRm,
		OpTransfer, OpExport, OpImport,
	}

	for _, name := range allOps {
		name := name
		dblifecycle.RegisterCategoryOp(name, dblifecycle.CategoryOp{
			Access: func(raw json.RawMessage) (dblifecycle.Access, error) {
				in, err := decodeInput(raw)
				if err != nil {
					return 0, err
				}
				if _, _, err := in.bounds(); err != nil {
					return 0, err
				}
				switch name {
				case OpBucketDrop, OpBucketRm:
					return dblifecycle.AccessExclusive, nil
				case OpBucketCreate, OpBucketAdd, OpPut, OpDelete, OpRm:
					return dblifecycle.AccessWrite, nil
				case OpImport:
					if in.Replace {
						return dblifecycle.AccessExclusive, nil
					}
					return dblifecycle.AccessWrite, nil
				case OpTransfer:
					if in.Delete || in.Replace {
						return dblifecycle.AccessExclusive, nil
					}
					return dblifecycle.AccessWrite, nil
				default:
					return dblifecycle.AccessRead, nil
				}
			},
			Spec: func(mode dbstore.Mode, a dblifecycle.Access) dblifecycle.Operation {
				return Spec(name, mode, a)
			},
			SpecInput: func(raw json.RawMessage, mode dbstore.Mode, a dblifecycle.Access) dblifecycle.Operation {
				s := Spec(name, mode, a)
				if name == OpTransfer {
					in, err := decodeInput(raw)
					if err == nil {
						isRemote := in.Remote != "" || strings.Contains(in.Destination, ":")
						var eff []string
						for _, e := range s.Effects {
							if e != "net.egress" {
								eff = append(eff, e)
							}
						}
						if isRemote {
							eff = append(eff, "net.egress")
						}
						s.Effects = eff
					}
				}
				return s
			},
			Run: func(ctx context.Context, h dbstore.Handle, raw json.RawMessage, a dblifecycle.Access) (dblifecycle.Outcome, error) {
				in, err := decodeInput(raw)
				if err != nil {
					return dblifecycle.Outcome{}, err
				}
				return dispatch(ctx, h, name, in)
			},
		})
	}
}

// normalizeObjectRelPath validates and returns a clean, relative slash-separated path for an object key.
// It refuses absolute paths, leading slashes or backslashes, and path traversal ("..").
func normalizeObjectRelPath(key string) (string, error) {
	if filepath.IsAbs(key) || strings.HasPrefix(key, "/") || strings.HasPrefix(key, "\\") {
		return "", fmt.Errorf("%w: object key %q cannot be an absolute path", dbstore.ErrInvalidInput, key)
	}
	clean := filepath.Clean(filepath.FromSlash(key))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: object key %q attempts path traversal outside destination", dbstore.ErrInvalidInput, key)
	}
	for _, seg := range strings.Split(filepath.ToSlash(clean), "/") {
		if seg == ".." {
			return "", fmt.Errorf("%w: object key %q attempts path traversal outside destination", dbstore.ErrInvalidInput, key)
		}
	}
	return filepath.ToSlash(clean), nil
}

// safeJoin ensures that resolving key relative to baseDir remains strictly inside baseDir.
// It checks lexical containment, and if baseDir exists, verifies that no existing component
// along the destination path is a symlink pointing outside baseDir.
func safeJoin(baseDir, key string) (string, error) {
	norm, err := normalizeObjectRelPath(key)
	if err != nil {
		return "", err
	}
	targetPath := filepath.Join(baseDir, filepath.FromSlash(norm))
	rel, err := filepath.Rel(baseDir, targetPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: object key %q escapes destination directory", dbstore.ErrInvalidInput, key)
	}

	// Verify that existing destination components do not contain symlinks escaping baseDir
	if baseEval, err := filepath.EvalSymlinks(baseDir); err == nil {
		curr := targetPath
		for curr != "" && curr != baseDir && curr != filepath.Dir(curr) {
			if fi, err := os.Lstat(curr); err == nil {
				if fi.Mode()&os.ModeSymlink != 0 {
					evalCurr, err := filepath.EvalSymlinks(curr)
					if err != nil {
						return "", fmt.Errorf("%w: resolve symlink %q: %v", dbstore.ErrInvalidInput, curr, err)
					}
					relEval, err := filepath.Rel(baseEval, evalCurr)
					if err != nil || relEval == ".." || strings.HasPrefix(relEval, ".."+string(filepath.Separator)) || filepath.IsAbs(relEval) {
						return "", fmt.Errorf("%w: destination path component %q is a symlink pointing outside destination directory", dbstore.ErrInvalidInput, curr)
					}
				}
			}
			curr = filepath.Dir(curr)
		}
	}

	return targetPath, nil
}

type parsedRecord struct {
	info     ObjectInfo
	data     []byte
	blobPath string
}

// objectStore abstracts object storage operations across SQLite and KV engines.
type objectStore interface {
	createBucket(ctx context.Context, bucket string) error
	dropBucket(ctx context.Context, bucket string) error
	listBuckets(ctx context.Context) ([]BucketInfo, error)
	listObjects(ctx context.Context, bucket, prefix, start string, limit int) ([]ObjectInfo, bool, error)
	statObject(ctx context.Context, bucket, key string) (*ObjectInfo, error)
	getObject(ctx context.Context, bucket, key string) (*ObjectInfo, []byte, error)
	putObject(ctx context.Context, obj ObjectInfo, data []byte, ifRevision string, ifAbsent bool) (*ObjectInfo, error)
	deleteObject(ctx context.Context, bucket, key, ifRevision string) (bool, error)
	importAtomic(ctx context.Context, replace bool, buckets []bucketDef, records []parsedRecord, onConflict string) (int64, error)
}

func getStore(h dbstore.Handle) (objectStore, error) {
	switch d := h.(type) {
	case *sqlitedb.DB:
		return newSQLiteStore(d), nil
	case dbstore.KV:
		return newKVStore(d), nil
	default:
		return nil, fmt.Errorf("%w: object category requires sqlite or kv/bbolt engine", dbstore.ErrUnsupported)
	}
}

// --- SQLite Object Store ---

type sqliteStore struct {
	db *sqlitedb.DB
}

func newSQLiteStore(db *sqlitedb.DB) *sqliteStore {
	return &sqliteStore{db: db}
}

func (s *sqliteStore) ensureSchema(ctx context.Context) error {
	schema := `
CREATE TABLE IF NOT EXISTS _bashy_obj_buckets (
    name TEXT PRIMARY KEY,
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS _bashy_objects (
    bucket TEXT NOT NULL,
    key TEXT NOT NULL,
    content_type TEXT NOT NULL,
    size INTEGER NOT NULL,
    sha256 TEXT NOT NULL,
    metadata TEXT NOT NULL,
    revision INTEGER NOT NULL,
    updated_at TEXT NOT NULL,
    data BLOB NOT NULL,
    PRIMARY KEY (bucket, key)
);
CREATE INDEX IF NOT EXISTS _bashy_objects_bucket_key ON _bashy_objects(bucket, key);
`
	_, err := s.db.SQL().ExecContext(ctx, schema)
	return err
}

func (s *sqliteStore) hasSchema(ctx context.Context) (bool, error) {
	var n int
	err := s.db.SQL().QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_schema WHERE type = 'table' AND name = '_bashy_obj_buckets'").Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (s *sqliteStore) createBucket(ctx context.Context, bucket string) error {
	if err := checkBucketName(bucket); err != nil {
		return err
	}
	if err := s.ensureSchema(ctx); err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.db.SQL().ExecContext(ctx, "INSERT OR IGNORE INTO _bashy_obj_buckets (name, created_at) VALUES (?, ?)", bucket, now)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: bucket %q already exists", dbstore.ErrExists, bucket)
	}
	return nil
}

func (s *sqliteStore) dropBucket(ctx context.Context, bucket string) error {
	if err := checkBucketName(bucket); err != nil {
		return err
	}
	has, err := s.hasSchema(ctx)
	if err != nil {
		return err
	}
	if !has {
		return fmt.Errorf("%w: bucket %q not found", dbstore.ErrNotFound, bucket)
	}
	tx, err := s.db.SQL().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var exists int
	err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM _bashy_obj_buckets WHERE name = ?", bucket).Scan(&exists)
	if err != nil {
		return err
	}
	if exists == 0 {
		return fmt.Errorf("%w: bucket %q not found", dbstore.ErrNotFound, bucket)
	}

	if _, err := tx.ExecContext(ctx, "DELETE FROM _bashy_objects WHERE bucket = ?", bucket); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM _bashy_obj_buckets WHERE name = ?", bucket); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *sqliteStore) listBuckets(ctx context.Context) ([]BucketInfo, error) {
	has, err := s.hasSchema(ctx)
	if err != nil {
		return nil, err
	}
	if !has {
		return []BucketInfo{}, nil
	}
	query := `
SELECT b.name, b.created_at,
       (SELECT COUNT(*) FROM _bashy_objects WHERE bucket = b.name),
       (SELECT COALESCE(SUM(size), 0) FROM _bashy_objects WHERE bucket = b.name)
FROM _bashy_obj_buckets b
ORDER BY b.name ASC
`
	rows, err := s.db.SQL().QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []BucketInfo
	for rows.Next() {
		var bi BucketInfo
		if err := rows.Scan(&bi.Name, &bi.CreatedAt, &bi.ObjectCount, &bi.TotalBytes); err != nil {
			return nil, err
		}
		list = append(list, bi)
	}
	if list == nil {
		list = []BucketInfo{}
	}
	return list, rows.Err()
}

func (s *sqliteStore) listObjects(ctx context.Context, bucket, prefix, start string, limit int) ([]ObjectInfo, bool, error) {
	has, err := s.hasSchema(ctx)
	if err != nil {
		return nil, false, err
	}
	if !has {
		return nil, false, fmt.Errorf("%w: bucket %q not found", dbstore.ErrNotFound, bucket)
	}
	var exists int
	err = s.db.SQL().QueryRowContext(ctx, "SELECT COUNT(*) FROM _bashy_obj_buckets WHERE name = ?", bucket).Scan(&exists)
	if err != nil {
		return nil, false, err
	}
	if exists == 0 {
		return nil, false, fmt.Errorf("%w: bucket %q not found", dbstore.ErrNotFound, bucket)
	}

	query := "SELECT key, content_type, size, sha256, metadata, revision, updated_at FROM _bashy_objects WHERE bucket = ?"
	args := []any{bucket}

	if prefix != "" {
		query += " AND key LIKE ? || '%'"
		args = append(args, prefix)
	}
	if start != "" {
		query += " AND key > ?"
		args = append(args, start)
	}
	query += " ORDER BY key ASC LIMIT ?"
	args = append(args, limit+1)

	rows, err := s.db.SQL().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()

	var objs []ObjectInfo
	for rows.Next() {
		var obj ObjectInfo
		var metaJSON string
		obj.Bucket = bucket
		if err := rows.Scan(&obj.Key, &obj.ContentType, &obj.Size, &obj.SHA256, &metaJSON, &obj.Revision, &obj.UpdatedAt); err != nil {
			return nil, false, err
		}
		if metaJSON != "" && metaJSON != "{}" {
			_ = json.Unmarshal([]byte(metaJSON), &obj.Metadata)
		}
		objs = append(objs, obj)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}

	truncated := false
	if len(objs) > limit {
		truncated = true
		objs = objs[:limit]
	}
	return objs, truncated, nil
}

func (s *sqliteStore) statObject(ctx context.Context, bucket, key string) (*ObjectInfo, error) {
	has, err := s.hasSchema(ctx)
	if err != nil {
		return nil, err
	}
	if !has {
		return nil, fmt.Errorf("%w: bucket %q not found", dbstore.ErrNotFound, bucket)
	}
	query := "SELECT content_type, size, sha256, metadata, revision, updated_at FROM _bashy_objects WHERE bucket = ? AND key = ?"
	var obj ObjectInfo
	var metaJSON string
	obj.Bucket = bucket
	obj.Key = key
	err = s.db.SQL().QueryRowContext(ctx, query, bucket, key).Scan(
		&obj.ContentType, &obj.Size, &obj.SHA256, &metaJSON, &obj.Revision, &obj.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: object %q in bucket %q not found", dbstore.ErrNotFound, key, bucket)
	}
	if err != nil {
		return nil, err
	}
	if metaJSON != "" && metaJSON != "{}" {
		_ = json.Unmarshal([]byte(metaJSON), &obj.Metadata)
	}
	return &obj, nil
}

func (s *sqliteStore) getObject(ctx context.Context, bucket, key string) (*ObjectInfo, []byte, error) {
	has, err := s.hasSchema(ctx)
	if err != nil {
		return nil, nil, err
	}
	if !has {
		return nil, nil, fmt.Errorf("%w: bucket %q not found", dbstore.ErrNotFound, bucket)
	}
	query := "SELECT content_type, size, sha256, metadata, revision, updated_at, data FROM _bashy_objects WHERE bucket = ? AND key = ?"
	var obj ObjectInfo
	var metaJSON string
	var data []byte
	obj.Bucket = bucket
	obj.Key = key
	err = s.db.SQL().QueryRowContext(ctx, query, bucket, key).Scan(
		&obj.ContentType, &obj.Size, &obj.SHA256, &metaJSON, &obj.Revision, &obj.UpdatedAt, &data,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, fmt.Errorf("%w: object %q in bucket %q not found", dbstore.ErrNotFound, key, bucket)
	}
	if err != nil {
		return nil, nil, err
	}
	if metaJSON != "" && metaJSON != "{}" {
		_ = json.Unmarshal([]byte(metaJSON), &obj.Metadata)
	}
	return &obj, data, nil
}

func (s *sqliteStore) putObject(ctx context.Context, obj ObjectInfo, data []byte, ifRevision string, ifAbsent bool) (*ObjectInfo, error) {
	if err := checkBucketName(obj.Bucket); err != nil {
		return nil, err
	}
	if err := checkObjectKey(obj.Key); err != nil {
		return nil, err
	}
	has, err := s.hasSchema(ctx)
	if err != nil {
		return nil, err
	}
	if !has {
		return nil, fmt.Errorf("%w: bucket %q not found", dbstore.ErrNotFound, obj.Bucket)
	}

	tx, err := s.db.SQL().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var bucketExists int
	err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM _bashy_obj_buckets WHERE name = ?", obj.Bucket).Scan(&bucketExists)
	if err != nil {
		return nil, err
	}
	if bucketExists == 0 {
		return nil, fmt.Errorf("%w: bucket %q not found", dbstore.ErrNotFound, obj.Bucket)
	}

	var curRevision int64
	var exists bool
	err = tx.QueryRowContext(ctx, "SELECT revision FROM _bashy_objects WHERE bucket = ? AND key = ?", obj.Bucket, obj.Key).Scan(&curRevision)
	if err == nil {
		exists = true
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	if exists {
		if ifAbsent {
			return nil, fmt.Errorf("%w: object %q already exists and if_absent requested", dbstore.ErrConflict, obj.Key)
		}
		if ifRevision != "" && strconv.FormatInt(curRevision, 10) != ifRevision {
			return nil, fmt.Errorf("%w: revision mismatch on key %s (expected %s, actual %d)", dbstore.ErrConflict, obj.Key, ifRevision, curRevision)
		}
		obj.Revision = curRevision + 1
	} else {
		if ifRevision != "" {
			return nil, fmt.Errorf("%w: revision condition %q specified for absent key %s", dbstore.ErrConflict, ifRevision, obj.Key)
		}
		obj.Revision = 1
	}

	obj.Size = int64(len(data))
	obj.SHA256 = hex.EncodeToString(sha256Sum(data))
	obj.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	if obj.ContentType == "" {
		obj.ContentType = "application/octet-stream"
	}

	metaJSON := "{}"
	if len(obj.Metadata) > 0 {
		b, _ := json.Marshal(obj.Metadata)
		metaJSON = string(b)
	}

	upsert := `
INSERT INTO _bashy_objects (bucket, key, content_type, size, sha256, metadata, revision, updated_at, data)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(bucket, key) DO UPDATE SET
  content_type = excluded.content_type,
  size = excluded.size,
  sha256 = excluded.sha256,
  metadata = excluded.metadata,
  revision = excluded.revision,
  updated_at = excluded.updated_at,
  data = excluded.data
`
	_, err = tx.ExecContext(ctx, upsert, obj.Bucket, obj.Key, obj.ContentType, obj.Size, obj.SHA256, metaJSON, obj.Revision, obj.UpdatedAt, data)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &obj, nil
}

func (s *sqliteStore) deleteObject(ctx context.Context, bucket, key, ifRevision string) (bool, error) {
	has, err := s.hasSchema(ctx)
	if err != nil {
		return false, err
	}
	if !has {
		return false, fmt.Errorf("%w: bucket %q not found", dbstore.ErrNotFound, bucket)
	}
	tx, err := s.db.SQL().BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var curRevision int64
	err = tx.QueryRowContext(ctx, "SELECT revision FROM _bashy_objects WHERE bucket = ? AND key = ?", bucket, key).Scan(&curRevision)
	if errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("%w: object %q in bucket %q not found", dbstore.ErrNotFound, key, bucket)
	}
	if err != nil {
		return false, err
	}

	if ifRevision != "" && strconv.FormatInt(curRevision, 10) != ifRevision {
		return false, fmt.Errorf("%w: revision mismatch on key %s (expected %s, actual %d)", dbstore.ErrConflict, key, ifRevision, curRevision)
	}

	_, err = tx.ExecContext(ctx, "DELETE FROM _bashy_objects WHERE bucket = ? AND key = ?", bucket, key)
	if err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func (s *sqliteStore) importAtomic(ctx context.Context, replace bool, buckets []bucketDef, records []parsedRecord, onConflict string) (int64, error) {
	if err := s.ensureSchema(ctx); err != nil {
		return 0, err
	}
	tx, err := s.db.SQL().BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	if replace {
		if _, err := tx.ExecContext(ctx, "DELETE FROM _bashy_objects"); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM _bashy_obj_buckets"); err != nil {
			return 0, err
		}
	}

	for _, b := range buckets {
		if err := checkBucketName(b.Name); err != nil {
			return 0, err
		}
		now := b.CreatedAt
		if now == "" {
			now = time.Now().UTC().Format(time.RFC3339)
		}
		if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO _bashy_obj_buckets (name, created_at) VALUES (?, ?)", b.Name, now); err != nil {
			return 0, err
		}
	}

	var imported int64
	for _, r := range records {
		var existingRevision int64
		err := tx.QueryRowContext(ctx, "SELECT revision FROM _bashy_objects WHERE bucket = ? AND key = ?", r.info.Bucket, r.info.Key).Scan(&existingRevision)
		exists := err == nil
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}

		if exists {
			switch onConflict {
			case "error":
				return 0, fmt.Errorf("%w: object %q in bucket %q already exists", dbstore.ErrConflict, r.info.Key, r.info.Bucket)
			case "skip":
				continue
			case "replace":
				// proceed
			}
		}

		metaJSON := "{}"
		if len(r.info.Metadata) > 0 {
			b, _ := json.Marshal(r.info.Metadata)
			metaJSON = string(b)
		}

		upsert := `
INSERT INTO _bashy_objects (bucket, key, content_type, size, sha256, metadata, revision, updated_at, data)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(bucket, key) DO UPDATE SET
  content_type = excluded.content_type,
  size = excluded.size,
  sha256 = excluded.sha256,
  metadata = excluded.metadata,
  revision = excluded.revision,
  updated_at = excluded.updated_at,
  data = excluded.data
`
		rev := r.info.Revision
		if rev <= 0 {
			rev = existingRevision + 1
		}
		if _, err := tx.ExecContext(ctx, upsert, r.info.Bucket, r.info.Key, r.info.ContentType, r.info.Size, r.info.SHA256, metaJSON, rev, r.info.UpdatedAt, r.data); err != nil {
			return 0, err
		}
		imported++
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return imported, nil
}

// --- KV Object Store (bbolt and MemKV) ---

const (
	bucketCatalogName = "_bashy_obj_buckets"
	bucketPrefix      = "obj:"
)

type kvStore struct {
	kv dbstore.KV
}

func newKVStore(kv dbstore.KV) *kvStore {
	return &kvStore{kv: kv}
}

func (k *kvStore) createBucket(ctx context.Context, bucket string) error {
	if err := checkBucketName(bucket); err != nil {
		return err
	}
	return k.kv.Update(ctx, func(tx dbstore.KVTx) error {
		_ = tx.CreateBucket([]byte(bucketCatalogName))
		val, found, err := tx.Get([]byte(bucketCatalogName), []byte(bucket))
		if err != nil {
			return err
		}
		if found && val != nil {
			return fmt.Errorf("%w: bucket %q already exists", dbstore.ErrExists, bucket)
		}
		now := time.Now().UTC().Format(time.RFC3339)
		if err := tx.Put([]byte(bucketCatalogName), []byte(bucket), []byte(now)); err != nil {
			return err
		}
		return tx.CreateBucket([]byte(bucketPrefix + bucket))
	})
}

func (k *kvStore) dropBucket(ctx context.Context, bucket string) error {
	if err := checkBucketName(bucket); err != nil {
		return err
	}
	return k.kv.Update(ctx, func(tx dbstore.KVTx) error {
		_, found, err := tx.Get([]byte(bucketCatalogName), []byte(bucket))
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("%w: bucket %q not found", dbstore.ErrNotFound, bucket)
		}
		if err := tx.Delete([]byte(bucketCatalogName), []byte(bucket)); err != nil {
			return err
		}
		return tx.DeleteBucket([]byte(bucketPrefix + bucket))
	})
}

func (k *kvStore) listBuckets(ctx context.Context) ([]BucketInfo, error) {
	var list []BucketInfo
	err := k.kv.View(ctx, func(tx dbstore.KVTx) error {
		return tx.Scan([]byte(bucketCatalogName), nil, func(kBytes, vBytes []byte) (bool, error) {
			bName := string(kBytes)
			createdAt := string(vBytes)

			var count, totalBytes int64
			targetBkt := []byte(bucketPrefix + bName)
			_ = tx.Scan(targetBkt, []byte("m:"), func(key, val []byte) (bool, error) {
				if !bytes.HasPrefix(key, []byte("m:")) {
					return false, nil
				}
				var info ObjectInfo
				if err := json.Unmarshal(val, &info); err == nil {
					count++
					totalBytes += info.Size
				}
				return true, nil
			})

			list = append(list, BucketInfo{
				Name:        bName,
				CreatedAt:   createdAt,
				ObjectCount: count,
				TotalBytes:  totalBytes,
			})
			return true, nil
		})
	})
	if err != nil {
		return nil, err
	}
	if list == nil {
		list = []BucketInfo{}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	return list, nil
}

func (k *kvStore) listObjects(ctx context.Context, bucket, prefix, start string, limit int) ([]ObjectInfo, bool, error) {
	var objs []ObjectInfo
	truncated := false
	err := k.kv.View(ctx, func(tx dbstore.KVTx) error {
		_, found, err := tx.Get([]byte(bucketCatalogName), []byte(bucket))
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("%w: bucket %q not found", dbstore.ErrNotFound, bucket)
		}

		targetBkt := []byte(bucketPrefix + bucket)
		startKey := []byte("m:")
		if start != "" {
			startKey = []byte("m:" + start + "\x00")
		}

		return tx.Scan(targetBkt, startKey, func(key, val []byte) (bool, error) {
			if !bytes.HasPrefix(key, []byte("m:")) {
				return false, nil
			}
			objKey := string(key[2:])
			if prefix != "" && !strings.HasPrefix(objKey, prefix) {
				return true, nil
			}

			if len(objs) >= limit {
				truncated = true
				return false, nil
			}

			var info ObjectInfo
			if err := json.Unmarshal(val, &info); err != nil {
				return true, nil
			}
			info.Bucket = bucket
			objs = append(objs, info)
			return true, nil
		})
	})
	return objs, truncated, err
}

func (k *kvStore) statObject(ctx context.Context, bucket, key string) (*ObjectInfo, error) {
	var obj ObjectInfo
	err := k.kv.View(ctx, func(tx dbstore.KVTx) error {
		_, found, err := tx.Get([]byte(bucketCatalogName), []byte(bucket))
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("%w: bucket %q not found", dbstore.ErrNotFound, bucket)
		}

		val, found, err := tx.Get([]byte(bucketPrefix+bucket), []byte("m:"+key))
		if err != nil {
			return err
		}
		if !found || val == nil {
			return fmt.Errorf("%w: object %q in bucket %q not found", dbstore.ErrNotFound, key, bucket)
		}
		if err := json.Unmarshal(val, &obj); err != nil {
			return fmt.Errorf("%w: corrupt object metadata: %v", dbstore.ErrIntegrity, err)
		}
		obj.Bucket = bucket
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &obj, nil
}

func (k *kvStore) getObject(ctx context.Context, bucket, key string) (*ObjectInfo, []byte, error) {
	var obj ObjectInfo
	var data []byte
	err := k.kv.View(ctx, func(tx dbstore.KVTx) error {
		_, found, err := tx.Get([]byte(bucketCatalogName), []byte(bucket))
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("%w: bucket %q not found", dbstore.ErrNotFound, bucket)
		}

		mVal, found, err := tx.Get([]byte(bucketPrefix+bucket), []byte("m:"+key))
		if err != nil {
			return err
		}
		if !found || mVal == nil {
			return fmt.Errorf("%w: object %q in bucket %q not found", dbstore.ErrNotFound, key, bucket)
		}
		if err := json.Unmarshal(mVal, &obj); err != nil {
			return fmt.Errorf("%w: corrupt object metadata: %v", dbstore.ErrIntegrity, err)
		}
		obj.Bucket = bucket

		dVal, found, err := tx.Get([]byte(bucketPrefix+bucket), []byte("d:"+key))
		if err != nil {
			return err
		}
		if !found {
			data = []byte{}
		} else {
			data = make([]byte, len(dVal))
			copy(data, dVal)
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return &obj, data, nil
}

func (k *kvStore) putObject(ctx context.Context, obj ObjectInfo, data []byte, ifRevision string, ifAbsent bool) (*ObjectInfo, error) {
	if err := checkBucketName(obj.Bucket); err != nil {
		return nil, err
	}
	if err := checkObjectKey(obj.Key); err != nil {
		return nil, err
	}

	err := k.kv.Update(ctx, func(tx dbstore.KVTx) error {
		_, found, err := tx.Get([]byte(bucketCatalogName), []byte(obj.Bucket))
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("%w: bucket %q not found", dbstore.ErrNotFound, obj.Bucket)
		}

		bktName := []byte(bucketPrefix + obj.Bucket)
		mVal, exists, err := tx.Get(bktName, []byte("m:"+obj.Key))
		if err != nil {
			return err
		}

		if exists && mVal != nil {
			if ifAbsent {
				return fmt.Errorf("%w: object %q already exists and if_absent requested", dbstore.ErrConflict, obj.Key)
			}
			var existing ObjectInfo
			if err := json.Unmarshal(mVal, &existing); err == nil {
				if ifRevision != "" && strconv.FormatInt(existing.Revision, 10) != ifRevision {
					return fmt.Errorf("%w: revision mismatch on key %s (expected %s, actual %d)", dbstore.ErrConflict, obj.Key, ifRevision, existing.Revision)
				}
				obj.Revision = existing.Revision + 1
			} else {
				obj.Revision = 1
			}
		} else {
			if ifRevision != "" {
				return fmt.Errorf("%w: revision condition %q specified for absent key %s", dbstore.ErrConflict, ifRevision, obj.Key)
			}
			obj.Revision = 1
		}

		obj.Size = int64(len(data))
		obj.SHA256 = hex.EncodeToString(sha256Sum(data))
		obj.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		if obj.ContentType == "" {
			obj.ContentType = "application/octet-stream"
		}

		mBytes, err := json.Marshal(obj)
		if err != nil {
			return err
		}

		if err := tx.Put(bktName, []byte("m:"+obj.Key), mBytes); err != nil {
			return err
		}
		return tx.Put(bktName, []byte("d:"+obj.Key), data)
	})
	if err != nil {
		return nil, err
	}
	return &obj, nil
}

func (k *kvStore) deleteObject(ctx context.Context, bucket, key, ifRevision string) (bool, error) {
	err := k.kv.Update(ctx, func(tx dbstore.KVTx) error {
		_, found, err := tx.Get([]byte(bucketCatalogName), []byte(bucket))
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("%w: bucket %q not found", dbstore.ErrNotFound, bucket)
		}

		bktName := []byte(bucketPrefix + bucket)
		mVal, exists, err := tx.Get(bktName, []byte("m:"+key))
		if err != nil {
			return err
		}
		if !exists || mVal == nil {
			return fmt.Errorf("%w: object %q in bucket %q not found", dbstore.ErrNotFound, key, bucket)
		}

		if ifRevision != "" {
			var existing ObjectInfo
			if err := json.Unmarshal(mVal, &existing); err == nil {
				if strconv.FormatInt(existing.Revision, 10) != ifRevision {
					return fmt.Errorf("%w: revision mismatch on key %s (expected %s, actual %d)", dbstore.ErrConflict, key, ifRevision, existing.Revision)
				}
			}
		}

		if err := tx.Delete(bktName, []byte("m:"+key)); err != nil {
			return err
		}
		return tx.Delete(bktName, []byte("d:"+key))
	})
	if err != nil {
		return false, err
	}
	return true, nil
}

func (k *kvStore) importAtomic(ctx context.Context, replace bool, buckets []bucketDef, records []parsedRecord, onConflict string) (int64, error) {
	var imported int64
	err := k.kv.Update(ctx, func(tx dbstore.KVTx) error {
		if err := tx.CreateBucket([]byte(bucketCatalogName)); err != nil && !errors.Is(err, dbstore.ErrExists) {
			return err
		}

		if replace {
			var toDelete []string
			if err := tx.Scan([]byte(bucketCatalogName), nil, func(kBytes, _ []byte) (bool, error) {
				toDelete = append(toDelete, string(kBytes))
				return true, nil
			}); err != nil && !errors.Is(err, dbstore.ErrNotFound) {
				return err
			}
			for _, bName := range toDelete {
				if err := tx.DeleteBucket([]byte(bucketPrefix + bName)); err != nil && !errors.Is(err, dbstore.ErrNotFound) {
					return err
				}
			}
			if err := tx.DeleteBucket([]byte(bucketCatalogName)); err != nil && !errors.Is(err, dbstore.ErrNotFound) {
				return err
			}
			if err := tx.CreateBucket([]byte(bucketCatalogName)); err != nil && !errors.Is(err, dbstore.ErrExists) {
				return err
			}
		}

		for _, b := range buckets {
			if err := checkBucketName(b.Name); err != nil {
				return err
			}
			// Preserve existing bucket metadata when not replacing
			existingVal, exists, err := tx.Get([]byte(bucketCatalogName), []byte(b.Name))
			if err != nil && !errors.Is(err, dbstore.ErrNotFound) {
				return err
			}
			if !exists || len(existingVal) == 0 {
				now := b.CreatedAt
				if now == "" {
					now = time.Now().UTC().Format(time.RFC3339)
				}
				if err := tx.Put([]byte(bucketCatalogName), []byte(b.Name), []byte(now)); err != nil {
					return err
				}
			}
			if err := tx.CreateBucket([]byte(bucketPrefix + b.Name)); err != nil && !errors.Is(err, dbstore.ErrExists) {
				return err
			}
		}

		for _, r := range records {
			bktName := []byte(bucketPrefix + r.info.Bucket)
			existingVal, exists, err := tx.Get([]byte(bucketCatalogName), []byte(r.info.Bucket))
			if err != nil && !errors.Is(err, dbstore.ErrNotFound) {
				return err
			}
			if !exists || len(existingVal) == 0 {
				now := r.info.UpdatedAt
				if now == "" {
					now = time.Now().UTC().Format(time.RFC3339)
				}
				if err := tx.Put([]byte(bucketCatalogName), []byte(r.info.Bucket), []byte(now)); err != nil {
					return err
				}
			}
			if err := tx.CreateBucket(bktName); err != nil && !errors.Is(err, dbstore.ErrExists) {
				return err
			}

			mVal, exists, err := tx.Get(bktName, []byte("m:"+r.info.Key))
			if err != nil && !errors.Is(err, dbstore.ErrNotFound) {
				return err
			}

			if exists && mVal != nil {
				switch onConflict {
				case "error":
					return fmt.Errorf("%w: object %q in bucket %q already exists", dbstore.ErrConflict, r.info.Key, r.info.Bucket)
				case "skip":
					continue
				case "replace":
					// proceed
				}
			}

			mBytes, err := json.Marshal(r.info)
			if err != nil {
				return err
			}
			if err := tx.Put(bktName, []byte("m:"+r.info.Key), mBytes); err != nil {
				return err
			}
			if err := tx.Put(bktName, []byte("d:"+r.info.Key), r.data); err != nil {
				return err
			}
			imported++
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return imported, nil
}

func sha256Sum(b []byte) []byte {
	h := sha256.Sum256(b)
	return h[:]
}

// --- Operation Dispatchers ---

func dispatch(ctx context.Context, h dbstore.Handle, name string, in Input) (dblifecycle.Outcome, error) {
	store, err := getStore(h)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}

	switch name {
	case OpBucketCreate, OpBucketAdd:
		return runBucketCreate(ctx, store, in)
	case OpBucketDrop, OpBucketRm:
		return runBucketDrop(ctx, store, in)
	case OpBucketList:
		return runBucketList(ctx, store, in)
	case OpList:
		return runList(ctx, store, h.Identity().ID, in)
	case OpStat:
		return runStat(ctx, store, in)
	case OpGet:
		return runGet(ctx, store, in)
	case OpPut:
		return runPut(ctx, store, in)
	case OpDelete, OpRm:
		return runDelete(ctx, store, in)
	case OpTransfer:
		return runTransfer(ctx, store, h, in)
	case OpExport:
		return runExport(ctx, store, h, in)
	case OpImport:
		return runImport(ctx, store, h, in)
	default:
		return dblifecycle.Outcome{}, fmt.Errorf("%w: unknown op %s", dbstore.ErrUnsupported, name)
	}
}

func runBucketCreate(ctx context.Context, store objectStore, in Input) (dblifecycle.Outcome, error) {
	if in.Bucket == "" {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: bucket name required", dbstore.ErrInvalidInput)
	}
	if err := store.createBucket(ctx, in.Bucket); err != nil {
		return dblifecycle.Outcome{}, err
	}
	return dblifecycle.Outcome{Result: map[string]any{"created": true, "bucket": in.Bucket}}, nil
}

func runBucketDrop(ctx context.Context, store objectStore, in Input) (dblifecycle.Outcome, error) {
	if in.Bucket == "" {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: bucket name required", dbstore.ErrInvalidInput)
	}
	if err := store.dropBucket(ctx, in.Bucket); err != nil {
		return dblifecycle.Outcome{}, err
	}
	return dblifecycle.Outcome{Result: map[string]any{"dropped": true, "bucket": in.Bucket}}, nil
}

func runBucketList(ctx context.Context, store objectStore, in Input) (dblifecycle.Outcome, error) {
	list, err := store.listBuckets(ctx)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	return dblifecycle.Outcome{Result: map[string]any{"buckets": list}}, nil
}

func runList(ctx context.Context, store objectStore, targetID string, in Input) (dblifecycle.Outcome, error) {
	if in.Bucket == "" {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: bucket name required", dbstore.ErrInvalidInput)
	}
	limit, _, err := in.bounds()
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	start := in.Start
	if in.Since != "" {
		s, ok, err := decodeCursor(in.Since)
		if err != nil {
			return dblifecycle.Outcome{}, err
		}
		if ok {
			start = s
		}
	}

	objs, truncated, err := store.listObjects(ctx, in.Bucket, in.Prefix, start, limit)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}

	var totalBytes int
	var lastKey string
	for _, o := range objs {
		totalBytes += int(o.Size)
		lastKey = o.Key
	}

	pg := newPage(targetID, in.Bucket, lastKey, len(objs), limit, totalBytes, truncated)
	return dblifecycle.Outcome{
		Result: map[string]any{
			"bucket":  in.Bucket,
			"objects": objs,
		},
		Page: pg,
	}, nil
}

func runStat(ctx context.Context, store objectStore, in Input) (dblifecycle.Outcome, error) {
	if in.Bucket == "" || in.Key == "" {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: bucket and key required", dbstore.ErrInvalidInput)
	}
	obj, err := store.statObject(ctx, in.Bucket, in.Key)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	return dblifecycle.Outcome{Result: map[string]any{"object": obj}}, nil
}

func runGet(ctx context.Context, store objectStore, in Input) (dblifecycle.Outcome, error) {
	if in.Bucket == "" || in.Key == "" {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: bucket and key required", dbstore.ErrInvalidInput)
	}
	_, maxBytes, err := in.bounds()
	if err != nil {
		return dblifecycle.Outcome{}, err
	}

	obj, data, err := store.getObject(ctx, in.Bucket, in.Key)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}

	if in.Out != "" {
		if in.Out == "-" {
			// standard output requested
			return dblifecycle.Outcome{
				Result: map[string]any{
					"object": obj,
					"bytes":  len(data),
					"out":    "-",
				},
			}, nil
		}
		if err := os.WriteFile(in.Out, data, 0o644); err != nil {
			return dblifecycle.Outcome{}, fmt.Errorf("%w: write --out: %v", dbstore.ErrBackend, err)
		}
		return dblifecycle.Outcome{
			Result: map[string]any{
				"object":     obj,
				"written_to": in.Out,
				"bytes":      len(data),
			},
		}, nil
	}

	// If no --out, check if payload exceeds maxBytes
	if len(data) > maxBytes {
		return dblifecycle.Outcome{
			Result: map[string]any{
				"object":    obj,
				"locator":   fmt.Sprintf("dbobj:%s/%s", in.Bucket, in.Key),
				"truncated": true,
				"size":      obj.Size,
				"sha256":    obj.SHA256,
			},
			Warnings: []string{fmt.Sprintf("data length %d exceeds max_bytes %d; use --out to write to file", len(data), maxBytes)},
		}, nil
	}

	val := Encode(data)
	return dblifecycle.Outcome{
		Result: map[string]any{
			"object": obj,
			"value":  val,
		},
	}, nil
}

func runPut(ctx context.Context, store objectStore, in Input) (dblifecycle.Outcome, error) {
	if in.Bucket == "" || in.Key == "" {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: bucket and key required", dbstore.ErrInvalidInput)
	}

	var data []byte
	var err error
	if in.In != "" {
		data, err = os.ReadFile(in.In)
		if err != nil {
			return dblifecycle.Outcome{}, fmt.Errorf("%w: read --in file %q: %v", dbstore.ErrInvalidInput, in.In, err)
		}
	} else if in.Value != nil {
		data, err = in.Value.Bytes()
		if err != nil {
			return dblifecycle.Outcome{}, err
		}
	} else {
		data = []byte{}
	}

	calcSHA := hex.EncodeToString(sha256Sum(data))
	if in.Checksum != "" && in.Checksum != calcSHA {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: checksum mismatch (expected %s, got %s)", dbstore.ErrIntegrity, in.Checksum, calcSHA)
	}

	objInfo := ObjectInfo{
		Bucket:      in.Bucket,
		Key:         in.Key,
		ContentType: in.ContentType,
		Metadata:    in.Metadata,
	}

	res, err := store.putObject(ctx, objInfo, data, in.IfRevision, in.IfAbsent)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}

	return dblifecycle.Outcome{
		Result: map[string]any{
			"put":    true,
			"object": res,
		},
	}, nil
}

func runDelete(ctx context.Context, store objectStore, in Input) (dblifecycle.Outcome, error) {
	if in.Bucket == "" || in.Key == "" {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: bucket and key required", dbstore.ErrInvalidInput)
	}
	deleted, err := store.deleteObject(ctx, in.Bucket, in.Key, in.IfRevision)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	return dblifecycle.Outcome{
		Result: map[string]any{
			"deleted": deleted,
			"bucket":  in.Bucket,
			"key":     in.Key,
		},
	}, nil
}

// TransferRunner executes an external transfer command (rclone). Can be overridden in tests.
var rcloneRunner = func(ctx context.Context, args []string) ([]byte, error) {
	bin, err := rclone.Path(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("rclone binary not available: %w", err)
	}
	cmd := binmgr.Command(ctx, bin, args...)
	cmd.Env = rclone.ChildEnv(os.Environ())
	return cmd.CombinedOutput()
}

func runTransfer(ctx context.Context, store objectStore, h dbstore.Handle, in Input) (dblifecycle.Outcome, error) {
	if in.Bucket == "" {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: transfer requires --bucket", dbstore.ErrInvalidInput)
	}
	if in.Destination == "" && in.Remote == "" {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: transfer requires --destination or --remote", dbstore.ErrInvalidInput)
	}

	dest := in.Destination
	if dest == "" {
		dest = in.Remote
	}

	// Verify bucket exists
	limit := MaxLimit
	if in.Limit > 0 && in.Limit < MaxLimit {
		limit = in.Limit
	}
	objs, truncated, err := store.listObjects(ctx, in.Bucket, in.Prefix, "", limit)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	if truncated {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: bucket %q has more than %d objects; partial transfer refused", dbstore.ErrLimitExceeded, in.Bucket, limit)
	}

	// Preflight: detect normalized key collisions across objects before any writes
	normToKey := make(map[string]string)
	keyToNorm := make(map[string]string)
	for _, o := range objs {
		norm, err := normalizeObjectRelPath(o.Key)
		if err != nil {
			return dblifecycle.Outcome{}, err
		}
		if prevKey, exists := normToKey[norm]; exists && prevKey != o.Key {
			return dblifecycle.Outcome{}, fmt.Errorf("%w: normalized key collision between %q and %q (both normalize to %q)", dbstore.ErrInvalidInput, prevKey, o.Key, norm)
		}
		normToKey[norm] = o.Key
		keyToNorm[o.Key] = norm
	}

	// Remote transfer via rclone: if remote target contains ":" or remote flag given
	if in.Remote != "" || strings.Contains(dest, ":") {
		// Destructive control check
		if (in.Delete || in.Replace) && !in.Replace {
			return dblifecycle.Outcome{}, fmt.Errorf("%w: destructive transfer requires --replace", dbstore.ErrConfirmationRequired)
		}

		// Stage local directory for rclone transfer
		tmpDir, err := os.MkdirTemp("", "bashy-dbobj-transfer-*")
		if err != nil {
			return dblifecycle.Outcome{}, fmt.Errorf("%w: temp dir: %v", dbstore.ErrBackend, err)
		}
		defer os.RemoveAll(tmpDir)

		root, err := os.OpenRoot(tmpDir)
		if err != nil {
			return dblifecycle.Outcome{}, fmt.Errorf("%w: open root temp dir: %v", dbstore.ErrBackend, err)
		}
		defer root.Close()

		var totalBytes int64
		h := sha256.New()
		for _, o := range objs {
			_, data, err := store.getObject(ctx, in.Bucket, o.Key)
			if err != nil {
				return dblifecycle.Outcome{}, err
			}
			norm := keyToNorm[o.Key]
			if norm == "" {
				norm, err = normalizeObjectRelPath(o.Key)
				if err != nil {
					return dblifecycle.Outcome{}, err
				}
			}
			relPath := filepath.FromSlash(norm)
			dir := filepath.Dir(relPath)
			if dir != "." && dir != "" {
				if err := root.MkdirAll(dir, 0o755); err != nil {
					return dblifecycle.Outcome{}, fmt.Errorf("%w: mkdir %s: %v", dbstore.ErrBackend, dir, err)
				}
			}
			f, err := root.OpenFile(relPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
			if err != nil {
				return dblifecycle.Outcome{}, fmt.Errorf("%w: open file %s: %v", dbstore.ErrBackend, norm, err)
			}
			if _, err := f.Write(data); err != nil {
				f.Close()
				return dblifecycle.Outcome{}, fmt.Errorf("%w: write file %s: %v", dbstore.ErrBackend, norm, err)
			}
			if err := f.Close(); err != nil {
				return dblifecycle.Outcome{}, fmt.Errorf("%w: close file %s: %v", dbstore.ErrBackend, norm, err)
			}
			totalBytes += int64(len(data))
			h.Write(data)
		}
		calcSHA := hex.EncodeToString(h.Sum(nil))

		if in.Checksum != "" && in.Checksum != calcSHA {
			return dblifecycle.Outcome{}, fmt.Errorf("%w: transfer checksum mismatch (expected %s, got %s)", dbstore.ErrIntegrity, in.Checksum, calcSHA)
		}

		args := []string{"copy", tmpDir, dest}
		if in.Delete {
			args = []string{"sync", tmpDir, dest}
		}
		out, err := rcloneRunner(ctx, args)
		if err != nil {
			return dblifecycle.Outcome{}, fmt.Errorf("%w: rclone transfer: %v (output: %s)", dbstore.ErrBackend, err, string(out))
		}

		return dblifecycle.Outcome{
			Result: map[string]any{
				"transfer":    true,
				"bucket":      in.Bucket,
				"destination": dest,
				"remote":      true,
				"objects":     len(objs),
				"bytes":       totalBytes,
				"sha256":      calcSHA,
			},
		}, nil
	}

	// Local directory transfer / backup
	if fi, err := os.Stat(dest); err == nil {
		if !fi.IsDir() {
			return dblifecycle.Outcome{}, fmt.Errorf("%w: destination %q is not a directory", dbstore.ErrInvalidInput, dest)
		}
		ents, _ := os.ReadDir(dest)
		if len(ents) > 0 && !in.Replace {
			return dblifecycle.Outcome{}, fmt.Errorf("%w: destination %q is non-empty; pass --replace to overwrite", dbstore.ErrConfirmationRequired, dest)
		}
	} else if os.IsNotExist(err) {
		if err := os.MkdirAll(dest, 0o755); err != nil {
			return dblifecycle.Outcome{}, fmt.Errorf("%w: create destination dir: %v", dbstore.ErrBackend, err)
		}
	} else {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: stat destination %q: %v", dbstore.ErrBackend, dest, err)
	}

	// Preflight symlink escape check for each object before writing
	for _, o := range objs {
		if _, err := safeJoin(dest, o.Key); err != nil {
			return dblifecycle.Outcome{}, err
		}
	}

	// Open confined destination root
	root, err := os.OpenRoot(dest)
	if err != nil {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: open destination root %q: %v", dbstore.ErrBackend, dest, err)
	}
	defer root.Close()

	var totalBytes int64
	hSha := sha256.New()
	for _, o := range objs {
		_, data, err := store.getObject(ctx, in.Bucket, o.Key)
		if err != nil {
			return dblifecycle.Outcome{}, err
		}
		norm := keyToNorm[o.Key]
		if norm == "" {
			norm, err = normalizeObjectRelPath(o.Key)
			if err != nil {
				return dblifecycle.Outcome{}, err
			}
		}
		relPath := filepath.FromSlash(norm)
		dir := filepath.Dir(relPath)
		if dir != "." && dir != "" {
			if err := root.MkdirAll(dir, 0o755); err != nil {
				return dblifecycle.Outcome{}, fmt.Errorf("%w: mkdir %s: %v", dbstore.ErrBackend, dir, err)
			}
		}
		f, err := root.OpenFile(relPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
		if err != nil {
			return dblifecycle.Outcome{}, fmt.Errorf("%w: open file %s: %v", dbstore.ErrBackend, norm, err)
		}
		if _, err := f.Write(data); err != nil {
			f.Close()
			return dblifecycle.Outcome{}, fmt.Errorf("%w: write file %s: %v", dbstore.ErrBackend, norm, err)
		}
		if err := f.Close(); err != nil {
			return dblifecycle.Outcome{}, fmt.Errorf("%w: close file %s: %v", dbstore.ErrBackend, norm, err)
		}
		totalBytes += int64(len(data))
		hSha.Write(data)
	}

	calcSHA := hex.EncodeToString(hSha.Sum(nil))
	if in.Checksum != "" && in.Checksum != calcSHA {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: transfer checksum mismatch (expected %s, got %s)", dbstore.ErrIntegrity, in.Checksum, calcSHA)
	}

	return dblifecycle.Outcome{
		Result: map[string]any{
			"transfer":    true,
			"bucket":      in.Bucket,
			"destination": dest,
			"remote":      false,
			"objects":     len(objs),
			"bytes":       totalBytes,
			"sha256":      calcSHA,
		},
	}, nil
}

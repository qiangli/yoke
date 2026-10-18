package dbobj

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/dblifecycle"
	"github.com/qiangli/yoke/pkg/dbstore"
)

// Interchange constants per contract §8.
const (
	ExportSchema   = "bashy-db-export-v1"
	FormatVersion  = "1.0"
	Category       = "obj"
	ManifestName   = "manifest.json"
	MaxRecordBytes = 16 << 20
	maxManifest    = 16 << 20
)

type manifest struct {
	SchemaVersion string         `json:"schema_version"`
	FormatVersion string         `json:"format_version"`
	Category      string         `json:"category"`
	Engine        string         `json:"engine"`
	Source        manifestSource `json:"source"`
	Snapshot      snapshot       `json:"snapshot"`
	SchemaDef     schemaDef      `json:"schema_def"`
	Files         []recordFile   `json:"files"`
	Blobs         []blobFile     `json:"blobs"`
	Derived       []any          `json:"derived"`
}

type manifestSource struct {
	ID   string `json:"id"`
	Mode string `json:"mode"`
}

type snapshot struct {
	Revision   *string `json:"revision"`
	Consistent bool    `json:"consistent"`
	Method     string  `json:"method"`
}

type schemaDef struct {
	Buckets []bucketDef `json:"buckets"`
}

type bucketDef struct {
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
}

type recordFile struct {
	Path    string `json:"path"`
	Bucket  string `json:"bucket"`
	Records int64  `json:"records"`
	Bytes   int64  `json:"bytes"`
	SHA256  string `json:"sha256"`
}

type blobFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type objectRecord struct {
	Bucket      string            `json:"bucket"`
	Key         string            `json:"key"`
	ContentType string            `json:"content_type"`
	Size        int64             `json:"size"`
	SHA256      string            `json:"sha256"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	Revision    int64             `json:"revision"`
	UpdatedAt   string            `json:"updated_at"`
	BlobPath    string            `json:"blob_path"`
}

func runExport(ctx context.Context, store objectStore, h dbstore.Handle, in Input) (dblifecycle.Outcome, error) {
	if in.Out == "" || in.Out == "-" || !filepath.IsAbs(in.Out) {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: export writes a new directory; give --out DIR (must be absolute)", dbstore.ErrInvalidInput)
	}

	if err := os.Mkdir(in.Out, 0o755); err != nil {
		if os.IsExist(err) {
			return dblifecycle.Outcome{}, fmt.Errorf("%w: export target directory %s already exists", dbstore.ErrExists, in.Out)
		}
		return dblifecycle.Outcome{}, fmt.Errorf("%w: create export dir: %v", dbstore.ErrBackend, err)
	}

	done := false
	defer func() {
		if !done {
			_ = os.RemoveAll(in.Out)
		}
	}()

	blobsDir := filepath.Join(in.Out, "blobs")
	if err := os.Mkdir(blobsDir, 0o755); err != nil {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: create blobs dir: %v", dbstore.ErrBackend, err)
	}

	id := h.Identity()
	buckets, err := store.listBuckets(ctx)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}

	m := manifest{
		SchemaVersion: ExportSchema,
		FormatVersion: FormatVersion,
		Category:      Category,
		Engine:        id.Engine,
		Source: manifestSource{
			ID:   id.ID,
			Mode: string(id.Mode),
		},
		Snapshot: snapshot{
			Consistent: true,
			Method:     "read-txn",
		},
		SchemaDef: schemaDef{
			Buckets: make([]bucketDef, len(buckets)),
		},
		Files:   []recordFile{},
		Blobs:   []blobFile{},
		Derived: []any{},
	}

	seenBlobs := make(map[string]bool)
	var totalRecords int64

	for i, b := range buckets {
		m.SchemaDef.Buckets[i] = bucketDef{Name: b.Name, CreatedAt: b.CreatedAt}

		rfPath := fmt.Sprintf("objects-%03d.jsonl", i)
		absRfPath := filepath.Join(in.Out, rfPath)

		rfFile, err := os.Create(absRfPath)
		if err != nil {
			return dblifecycle.Outcome{}, fmt.Errorf("%w: create record file %s: %v", dbstore.ErrBackend, rfPath, err)
		}

		rfWriter := bufio.NewWriter(rfFile)
		rfHasher := sha256.New()
		multiWriter := io.MultiWriter(rfWriter, rfHasher)

		limit := MaxLimit
		if in.Limit > 0 && in.Limit < MaxLimit {
			limit = in.Limit
		}
		objs, truncated, err := store.listObjects(ctx, b.Name, "", "", limit)
		if err != nil {
			rfFile.Close()
			return dblifecycle.Outcome{}, err
		}
		if truncated {
			rfFile.Close()
			return dblifecycle.Outcome{}, fmt.Errorf("%w: bucket %q has more than %d objects; partial export refused", dbstore.ErrLimitExceeded, b.Name, limit)
		}

		var recCount int64
		var byteCount int64

		for _, o := range objs {
			_, data, err := store.getObject(ctx, b.Name, o.Key)
			if err != nil {
				rfFile.Close()
				return dblifecycle.Outcome{}, err
			}

			blobRelPath := filepath.Join("blobs", o.SHA256)
			blobAbsPath := filepath.Join(in.Out, blobRelPath)

			if !seenBlobs[o.SHA256] {
				if err := os.WriteFile(blobAbsPath, data, 0o644); err != nil {
					rfFile.Close()
					return dblifecycle.Outcome{}, fmt.Errorf("%w: write blob %s: %v", dbstore.ErrBackend, blobRelPath, err)
				}
				m.Blobs = append(m.Blobs, blobFile{
					Path:   blobRelPath,
					Size:   int64(len(data)),
					SHA256: o.SHA256,
				})
				seenBlobs[o.SHA256] = true
			}

			rec := objectRecord{
				Bucket:      o.Bucket,
				Key:         o.Key,
				ContentType: o.ContentType,
				Size:        o.Size,
				SHA256:      o.SHA256,
				Metadata:    o.Metadata,
				Revision:    o.Revision,
				UpdatedAt:   o.UpdatedAt,
				BlobPath:    blobRelPath,
			}

			lineBytes, err := json.Marshal(rec)
			if err != nil {
				rfFile.Close()
				return dblifecycle.Outcome{}, err
			}
			lineBytes = append(lineBytes, '\n')

			n, err := multiWriter.Write(lineBytes)
			if err != nil {
				rfFile.Close()
				return dblifecycle.Outcome{}, err
			}
			recCount++
			byteCount += int64(n)
		}

		if err := rfWriter.Flush(); err != nil {
			rfFile.Close()
			return dblifecycle.Outcome{}, err
		}
		if err := rfFile.Close(); err != nil {
			return dblifecycle.Outcome{}, err
		}

		m.Files = append(m.Files, recordFile{
			Path:    rfPath,
			Bucket:  b.Name,
			Records: recCount,
			Bytes:   byteCount,
			SHA256:  hex.EncodeToString(rfHasher.Sum(nil)),
		})
		totalRecords += recCount
	}

	manifestBytes, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	if err := os.WriteFile(filepath.Join(in.Out, ManifestName), manifestBytes, 0o644); err != nil {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: write %s: %v", dbstore.ErrBackend, ManifestName, err)
	}

	done = true
	return dblifecycle.Outcome{
		Result: map[string]any{
			"exported": true,
			"out":      in.Out,
			"buckets":  len(buckets),
			"records":  totalRecords,
			"blobs":    len(m.Blobs),
		},
	}, nil
}

func runImport(ctx context.Context, store objectStore, h dbstore.Handle, in Input) (dblifecycle.Outcome, error) {
	if in.In == "" || in.In == "-" || !filepath.IsAbs(in.In) {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: import takes --in DIR (must be absolute path)", dbstore.ErrInvalidInput)
	}

	fi, err := os.Stat(in.In)
	if err != nil {
		if os.IsNotExist(err) {
			return dblifecycle.Outcome{}, fmt.Errorf("%w: import directory %s not found", dbstore.ErrNotFound, in.In)
		}
		return dblifecycle.Outcome{}, fmt.Errorf("%w: stat --in: %v", dbstore.ErrBackend, err)
	}
	if !fi.IsDir() {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: --in %s must be a directory", dbstore.ErrInvalidInput, in.In)
	}

	manifestPath := filepath.Join(in.In, ManifestName)
	rawManifest, err := os.ReadFile(manifestPath)
	if err != nil {
		if os.IsNotExist(err) {
			return dblifecycle.Outcome{}, fmt.Errorf("%w: manifest %s not found", dbstore.ErrNotFound, manifestPath)
		}
		return dblifecycle.Outcome{}, fmt.Errorf("%w: read manifest: %v", dbstore.ErrBackend, err)
	}
	if len(rawManifest) > maxManifest {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: manifest exceeds maximum %d bytes", dbstore.ErrLimitExceeded, maxManifest)
	}

	var m manifest
	dec := json.NewDecoder(bytes.NewReader(rawManifest))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: decode manifest: %v", dbstore.ErrVersionUnsupported, err)
	}

	if m.SchemaVersion != ExportSchema {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: unsupported schema version %q", dbstore.ErrVersionUnsupported, m.SchemaVersion)
	}

	majorVer := strings.Split(m.FormatVersion, ".")[0]
	if majorVer != "1" {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: unsupported format major version %q", dbstore.ErrVersionUnsupported, m.FormatVersion)
	}

	if m.Category != Category && m.Category != "objects" {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: import category %q (expected %s)", dbstore.ErrInvalidInput, m.Category, Category)
	}

	onConflict := in.OnConflict
	if onConflict == "" {
		onConflict = "error"
	}
	switch onConflict {
	case "error", "skip", "replace":
	default:
		return dblifecycle.Outcome{}, fmt.Errorf("%w: on_conflict must be error, skip, or replace", dbstore.ErrInvalidInput)
	}

	// Step 1: Pre-validation of all files and blobs with path safety and integrity checks
	for _, rf := range m.Files {
		filePath, err := safeJoin(in.In, rf.Path)
		if err != nil {
			return dblifecycle.Outcome{}, fmt.Errorf("%w: record file path safety: %v", dbstore.ErrInvalidInput, err)
		}
		data, err := os.ReadFile(filePath)
		if err != nil {
			return dblifecycle.Outcome{}, fmt.Errorf("%w: missing record file %s: %v", dbstore.ErrNotFound, rf.Path, err)
		}
		calcSHA := hex.EncodeToString(sha256Sum(data))
		if calcSHA != rf.SHA256 {
			return dblifecycle.Outcome{}, fmt.Errorf("%w: record file %s checksum mismatch (expected %s, got %s)", dbstore.ErrIntegrity, rf.Path, rf.SHA256, calcSHA)
		}
	}

	blobMap := make(map[string][]byte, len(m.Blobs))
	for _, bf := range m.Blobs {
		blobPath, err := safeJoin(in.In, bf.Path)
		if err != nil {
			return dblifecycle.Outcome{}, fmt.Errorf("%w: blob file path safety: %v", dbstore.ErrInvalidInput, err)
		}
		data, err := os.ReadFile(blobPath)
		if err != nil {
			return dblifecycle.Outcome{}, fmt.Errorf("%w: missing blob file %s: %v", dbstore.ErrNotFound, bf.Path, err)
		}
		if int64(len(data)) != bf.Size {
			return dblifecycle.Outcome{}, fmt.Errorf("%w: blob file %s size mismatch (expected %d, got %d)", dbstore.ErrIntegrity, bf.Path, bf.Size, len(data))
		}
		calcSHA := hex.EncodeToString(sha256Sum(data))
		if calcSHA != bf.SHA256 {
			return dblifecycle.Outcome{}, fmt.Errorf("%w: blob file %s checksum mismatch (expected %s, got %s)", dbstore.ErrIntegrity, bf.Path, bf.SHA256, calcSHA)
		}
		blobMap[bf.Path] = data
		blobMap[bf.SHA256] = data
	}

	// Validate bucket definitions
	for _, b := range m.SchemaDef.Buckets {
		if err := checkBucketName(b.Name); err != nil {
			return dblifecycle.Outcome{}, err
		}
	}

	// Step 2: Validate and parse all records across all record files before any mutation
	var records []parsedRecord
	type bktKey struct{ bucket, key string }
	seenKeys := make(map[bktKey]bool)

	for _, rf := range m.Files {
		filePath, _ := safeJoin(in.In, rf.Path)
		f, err := os.Open(filePath)
		if err != nil {
			return dblifecycle.Outcome{}, err
		}

		scanner := bufio.NewScanner(f)
		buf := make([]byte, 64*1024)
		scanner.Buffer(buf, MaxRecordBytes)

		for scanner.Scan() {
			line := bytes.TrimSpace(scanner.Bytes())
			if len(line) == 0 {
				continue
			}

			var rec objectRecord
			decRec := json.NewDecoder(bytes.NewReader(line))
			decRec.DisallowUnknownFields()
			if err := decRec.Decode(&rec); err != nil {
				f.Close()
				return dblifecycle.Outcome{}, fmt.Errorf("%w: parse record in %s: %v", dbstore.ErrIntegrity, rf.Path, err)
			}

			if err := checkBucketName(rec.Bucket); err != nil {
				f.Close()
				return dblifecycle.Outcome{}, err
			}
			if err := checkObjectKey(rec.Key); err != nil {
				f.Close()
				return dblifecycle.Outcome{}, err
			}

			if rec.BlobPath != "" {
				if _, err := safeJoin(in.In, rec.BlobPath); err != nil {
					f.Close()
					return dblifecycle.Outcome{}, fmt.Errorf("%w: record blob path safety %q: %v", dbstore.ErrInvalidInput, rec.BlobPath, err)
				}
			}

			blobData, found := blobMap[rec.BlobPath]
			if !found {
				blobData, found = blobMap[rec.SHA256]
			}
			if !found {
				f.Close()
				return dblifecycle.Outcome{}, fmt.Errorf("%w: missing blob %s for object %q", dbstore.ErrNotFound, rec.BlobPath, rec.Key)
			}

			if int64(len(blobData)) != rec.Size {
				f.Close()
				return dblifecycle.Outcome{}, fmt.Errorf("%w: object %q blob size mismatch (expected %d, got %d)", dbstore.ErrIntegrity, rec.Key, rec.Size, len(blobData))
			}
			calcSHA := hex.EncodeToString(sha256Sum(blobData))
			if calcSHA != rec.SHA256 {
				f.Close()
				return dblifecycle.Outcome{}, fmt.Errorf("%w: object %q blob checksum mismatch (expected %s, got %s)", dbstore.ErrIntegrity, rec.Key, rec.SHA256, calcSHA)
			}

			k := bktKey{bucket: rec.Bucket, key: rec.Key}
			if seenKeys[k] && onConflict == "error" {
				f.Close()
				return dblifecycle.Outcome{}, fmt.Errorf("%w: duplicate object %q in bucket %q in import", dbstore.ErrConflict, rec.Key, rec.Bucket)
			}
			seenKeys[k] = true

			if rec.ContentType == "" {
				rec.ContentType = "application/octet-stream"
			}
			if rec.UpdatedAt == "" {
				rec.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
			}

			records = append(records, parsedRecord{
				info: ObjectInfo{
					Bucket:      rec.Bucket,
					Key:         rec.Key,
					ContentType: rec.ContentType,
					Size:        rec.Size,
					SHA256:      rec.SHA256,
					Metadata:    rec.Metadata,
					Revision:    rec.Revision,
					UpdatedAt:   rec.UpdatedAt,
				},
				data:     blobData,
				blobPath: rec.BlobPath,
			})
		}

		if err := scanner.Err(); err != nil {
			f.Close()
			return dblifecycle.Outcome{}, fmt.Errorf("%w: scan %s: %v", dbstore.ErrBackend, rf.Path, err)
		}
		f.Close()
	}

	// Step 3: All validations passed! Execute ONE atomic transaction on target store.
	totalImported, err := store.importAtomic(ctx, in.Replace, m.SchemaDef.Buckets, records, onConflict)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}

	return dblifecycle.Outcome{
		Result: map[string]any{
			"imported": true,
			"in":       in.In,
			"buckets":  len(m.SchemaDef.Buckets),
			"records":  totalImported,
			"blobs":    len(m.Blobs),
		},
	}, nil
}

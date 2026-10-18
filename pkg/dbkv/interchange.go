package dbkv

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/qiangli/yoke/pkg/dblifecycle"
	"github.com/qiangli/yoke/pkg/dbstore"
)

// Interchange format: a directory holding manifest.json and one JSONL file
// per bucket (bucket-N.jsonl, empty buckets included). Each line is a JSON
// array ["<base64 key>","<base64 value>"]; base64 is lossless for binary.
// The manifest names every bucket (UTF-8), record count, byte size and
// sha256 so an import validates the whole export before changing anything.
const (
	ExportSchema   = "bashy-db-export-v1"
	FormatVersion  = "1.0"
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
	Capabilities  map[string]any `json:"capabilities"`
	Files         []recordFile   `json:"files"`
	Blobs         []any          `json:"blobs"`
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

type recordFile struct {
	Path    string `json:"path"`
	Bucket  string `json:"bucket"`
	Records int64  `json:"records"`
	Bytes   int64  `json:"bytes"`
	SHA256  string `json:"sha256"`
}

func runExport(ctx context.Context, kv dbstore.KV, h dbstore.Handle, in Input) (dblifecycle.Outcome, error) {
	if in.Bucket != "" || in.Key != nil || in.Value != nil || len(in.Ops) != 0 || in.In != "" || in.OnConflict != "" || in.Replace || in.Limit != 0 || in.MaxBytes != 0 || in.Since != "" {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: export takes --out DIR", dbstore.ErrInvalidInput)
	}
	if in.Out == "" || in.Out == "-" || !filepath.IsAbs(in.Out) {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: export writes a new directory; give --out DIR", dbstore.ErrInvalidInput)
	}
	// The directory must be new: an existing path is never merged into.
	if err := os.Mkdir(in.Out, 0o755); err != nil {
		if os.IsExist(err) {
			return dblifecycle.Outcome{}, fmt.Errorf("%w: %s", dbstore.ErrExists, in.Out)
		}
		return dblifecycle.Outcome{}, dbstore.Backend("export", err)
	}
	done := false
	defer func() {
		if !done {
			_ = os.RemoveAll(in.Out)
		}
	}()
	id := h.Identity()
	m := manifest{SchemaVersion: ExportSchema, FormatVersion: FormatVersion, Category: "kv", Engine: id.Engine,
		Source:       manifestSource{ID: id.ID, Mode: string(id.Mode)},
		Snapshot:     snapshot{Consistent: true, Method: "read-txn"},
		Capabilities: map[string]any{"ttl": false},
		Files:        []recordFile{}, Blobs: []any{}, Derived: []any{}}
	// One read transaction is the snapshot for every bucket.
	err := kv.View(ctx, func(tx dbstore.KVTx) error {
		buckets, err := tx.Buckets()
		if err != nil {
			return err
		}
		for i, b := range buckets {
			if !isName(b) {
				return fmt.Errorf("%w: bucket %q is not a UTF-8 name of 1..%d bytes; v1 cannot export it", dbstore.ErrUnsupported, b, MaxBucketName)
			}
			rf := recordFile{Path: "bucket-" + strconv.Itoa(i+1) + ".jsonl", Bucket: string(b)}
			f, err := os.OpenFile(filepath.Join(in.Out, rf.Path), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
			if err != nil {
				return dbstore.Backend("export", err)
			}
			sum := sha256.New()
			w := bufio.NewWriter(io.MultiWriter(f, sum))
			err = tx.Scan(b, nil, func(k, v []byte) (bool, error) {
				if err := ctx.Err(); err != nil {
					return false, dbstore.FromContext(err)
				}
				line, _ := json.Marshal([2]string{base64.StdEncoding.EncodeToString(k), base64.StdEncoding.EncodeToString(v)})
				line = append(line, '\n')
				if _, err := w.Write(line); err != nil {
					return false, err
				}
				rf.Records++
				rf.Bytes += int64(len(line))
				return true, nil
			})
			if err == nil {
				err = w.Flush()
			}
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return dbstore.Backend("export", err)
			}
			rf.SHA256 = hex.EncodeToString(sum.Sum(nil))
			m.Files = append(m.Files, rf)
		}
		return nil
	})
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(filepath.Join(in.Out, ManifestName), append(b, '\n'), 0o644); err != nil {
		return dblifecycle.Outcome{}, dbstore.Backend("export", err)
	}
	done = true
	var total int64
	for _, f := range m.Files {
		total += f.Records
	}
	return dblifecycle.Outcome{Result: map[string]any{
		"out": in.Out, "format_version": FormatVersion, "buckets": len(m.Files), "records": total,
		"snapshot": m.Snapshot, "manifest": ManifestName,
	}}, nil
}

func isName(b []byte) bool {
	_, err := bucketName(string(b))
	return err == nil
}

func readManifest(dir string) (manifest, error) {
	var m manifest
	f, err := os.Open(filepath.Join(dir, ManifestName))
	if err != nil {
		return m, fmt.Errorf("%w: %v", dbstore.ErrMalformedInput, err)
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxManifest+1))
	if err != nil || len(b) > maxManifest {
		return m, fmt.Errorf("%w: manifest unreadable or over %d bytes", dbstore.ErrMalformedInput, maxManifest)
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, fmt.Errorf("%w: manifest: %v", dbstore.ErrMalformedInput, err)
	}
	if m.SchemaVersion != ExportSchema {
		return m, fmt.Errorf("%w: manifest schema_version %q (want %s)", dbstore.ErrVersionUnsupported, m.SchemaVersion, ExportSchema)
	}
	if major, _, _ := strings.Cut(m.FormatVersion, "."); major != "1" {
		return m, fmt.Errorf("%w: format_version %q (this build reads 1.x)", dbstore.ErrVersionUnsupported, m.FormatVersion)
	}
	if m.Category != "kv" {
		return m, fmt.Errorf("%w: export category %q is not kv", dbstore.ErrUnsupported, m.Category)
	}
	seenPath, seenBucket := map[string]bool{}, map[string]bool{}
	for _, f := range m.Files {
		if f.Path == "" || f.Path != filepath.Base(f.Path) || f.Path == ManifestName || seenPath[f.Path] || seenBucket[f.Bucket] || !isName([]byte(f.Bucket)) {
			return m, fmt.Errorf("%w: manifest file entry %q (bucket %q)", dbstore.ErrMalformedInput, f.Path, f.Bucket)
		}
		seenPath[f.Path], seenBucket[f.Bucket] = true, true
	}
	return m, nil
}

// eachRecord streams one bucket file, verifying size, checksum and count
// against the manifest, and calls fn per decoded record.
func eachRecord(ctx context.Context, dir string, rf recordFile, fn func(k, v []byte) error) error {
	f, err := os.Open(filepath.Join(dir, rf.Path))
	if err != nil {
		return fmt.Errorf("%w: %v", dbstore.ErrMalformedInput, err)
	}
	defer f.Close()
	sum := sha256.New()
	var size int64
	r := bufio.NewReader(io.TeeReader(f, io.MultiWriter(sum, countWriter{&size})))
	var n int64
	for {
		if err := ctx.Err(); err != nil {
			return dbstore.FromContext(err)
		}
		line, err := readLine(r)
		if len(line) > 0 {
			var rec [2]string
			dec := json.NewDecoder(bytes.NewReader(line))
			dec.DisallowUnknownFields()
			if e := dec.Decode(&rec); e != nil {
				return fmt.Errorf("%w: %s record %d: %v", dbstore.ErrMalformedInput, rf.Path, n+1, e)
			}
			k, e1 := base64.StdEncoding.DecodeString(rec[0])
			v, e2 := base64.StdEncoding.DecodeString(rec[1])
			if e1 != nil || e2 != nil || len(k) == 0 {
				return fmt.Errorf("%w: %s record %d: key/value must be base64 and the key non-empty", dbstore.ErrMalformedInput, rf.Path, n+1)
			}
			n++
			if fn != nil {
				if e := fn(k, v); e != nil {
					return e
				}
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}
	if n != rf.Records || size != rf.Bytes || hex.EncodeToString(sum.Sum(nil)) != rf.SHA256 {
		return fmt.Errorf("%w: %s does not match the manifest (records, bytes or sha256)", dbstore.ErrIntegrity, rf.Path)
	}
	return nil
}

type countWriter struct{ n *int64 }

func (c countWriter) Write(p []byte) (int, error) { *c.n += int64(len(p)); return len(p), nil }

// readLine returns one line without its newline, failing past MaxRecordBytes.
func readLine(r *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		chunk, err := r.ReadSlice('\n')
		line = append(line, chunk...)
		if len(line) > MaxRecordBytes {
			return nil, fmt.Errorf("%w: record over %d bytes", dbstore.ErrLimitExceeded, MaxRecordBytes)
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		return bytes.TrimSuffix(line, []byte("\n")), err
	}
}

func runImport(ctx context.Context, kv dbstore.KV, _ dbstore.Handle, in Input) (dblifecycle.Outcome, error) {
	if in.Bucket != "" || in.Key != nil || in.Value != nil || len(in.Ops) != 0 || in.Out != "" || in.Limit != 0 || in.MaxBytes != 0 || in.Since != "" {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: import takes --in DIR, --on-conflict and --replace", dbstore.ErrInvalidInput)
	}
	if in.In == "" || in.In == "-" || !filepath.IsAbs(in.In) {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: import reads an export directory; give --in DIR", dbstore.ErrInvalidInput)
	}
	policy := in.OnConflict
	switch policy {
	case "":
		policy = "error"
	case "error", "skip", "replace":
	default:
		return dblifecycle.Outcome{}, fmt.Errorf("%w: --on-conflict error|skip|replace", dbstore.ErrInvalidInput)
	}
	if in.Replace && policy != "error" {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: --replace empties the target first; --on-conflict does not apply", dbstore.ErrInvalidInput)
	}
	// Validation first: manifest, checksums and every record, before any change.
	m, err := readManifest(in.In)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	for _, f := range m.Files {
		if err := eachRecord(ctx, in.In, f, nil); err != nil {
			return dblifecycle.Outcome{}, err
		}
	}
	applied := map[string]int64{}
	var total, skipped int64
	err = kv.Update(ctx, func(tx dbstore.KVTx) error {
		clear(applied)
		total, skipped = 0, 0
		existing, err := tx.Buckets()
		if err != nil {
			return err
		}
		if in.Replace {
			for _, b := range existing {
				if err := tx.DeleteBucket(b); err != nil {
					return err
				}
			}
			existing = nil
		}
		have := map[string]bool{}
		for _, b := range existing {
			have[string(b)] = true
		}
		for _, f := range m.Files {
			b := []byte(f.Bucket)
			if !have[f.Bucket] {
				if err := tx.CreateBucket(b); err != nil {
					return err
				}
			}
			err := eachRecord(ctx, in.In, f, func(k, v []byte) error {
				cur, found, err := tx.Get(b, k)
				if err != nil {
					return err
				}
				if found && !bytes.Equal(cur, v) {
					switch policy {
					case "error":
						return fmt.Errorf("%w: bucket %q key %s already holds a different value", dbstore.ErrConflict, f.Bucket, show(k))
					case "skip":
						skipped++
						return nil
					}
				}
				applied[f.Bucket]++
				total++
				return tx.Put(b, k, v)
			})
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return dblifecycle.Outcome{}, fmt.Errorf("%w (rolled back; the target is unchanged)", err)
	}
	mode := "merge"
	if in.Replace {
		mode = "replace"
	}
	return dblifecycle.Outcome{Result: map[string]any{
		"in": in.In, "mode": mode, "on_conflict": policy, "atomic": true, "format_version": m.FormatVersion,
		"source": m.Source, "buckets": len(m.Files), "records_applied": total, "records_skipped": skipped, "applied": applied,
	}}, nil
}

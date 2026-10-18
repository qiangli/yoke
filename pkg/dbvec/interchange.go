package dbvec

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/qiangli/yoke/pkg/dblifecycle"
	"github.com/qiangli/yoke/pkg/dbstore"
	"github.com/qiangli/yoke/pkg/dbstore/vecdb"
)

// Interchange format (contract §8): a directory holding manifest.json and
// one JSONL record file per collection. Each record line is
// {"id":…,"revision":N,"vector":{"t":"vec","dtype":"f32","b64":…},
// "metadata":{…}}: the embedding bit-exact, the metadata text as stored.
// Collection dimension, metric and model identity are in schema_def. vec0
// holds source embeddings, not a derived index, so derived is empty.
const (
	ExportSchema   = "bashy-db-export-v1"
	FormatVersion  = "1.0"
	Category       = "vectors"
	ManifestName   = "manifest.json"
	MaxRecordBytes = MaxMetadataBytes + MaxIDBytes + (MaxDimension*4*4)/3 + 4096
	maxManifest    = 16 << 20
)

type manifest struct {
	SchemaVersion string         `json:"schema_version"`
	FormatVersion string         `json:"format_version"`
	Category      string         `json:"category"`
	Engine        string         `json:"engine"`
	EngineVersion string         `json:"engine_version"`
	Source        manifestSource `json:"source"`
	Snapshot      snapshot       `json:"snapshot"`
	SchemaDef     schemaDef      `json:"schema_def"`
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

type schemaDef struct {
	VecFormat   string       `json:"vec_format"`
	VecVersion  string       `json:"sqlite_vec_version"`
	Collections []Collection `json:"collections"`
}

type recordFile struct {
	Path       string `json:"path"`
	Collection string `json:"collection"`
	Records    int64  `json:"records"`
	Bytes      int64  `json:"bytes"`
	SHA256     string `json:"sha256"`
}

type record struct {
	ID       string          `json:"id"`
	Revision int64           `json:"revision"`
	Vector   json.RawMessage `json:"vector"`
	Metadata json.RawMessage `json:"metadata"`
}

// loaded is a validated record ready to insert.
type loaded struct {
	id  string
	rev int64
	vec []float32
	doc string
}

func runExport(ctx context.Context, h dbstore.Handle, in Input) (dblifecycle.Outcome, error) {
	if err := in.only(OpExport, "out"); err != nil {
		return dblifecycle.Outcome{}, err
	}
	if in.Out == "" || in.Out == "-" || !filepath.IsAbs(in.Out) {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: export writes a new directory; give --out DIR", dbstore.ErrInvalidInput)
	}
	// A read transaction is the snapshot: every collection is read at one
	// point, and WAL readers never block writers.
	t, err := begin(ctx, h, false)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	defer t.end()
	id := h.Identity()
	m := manifest{SchemaVersion: ExportSchema, FormatVersion: FormatVersion, Category: Category, Engine: "sqlite",
		Source: manifestSource{ID: id.ID, Mode: string(id.Mode)}, Snapshot: snapshot{Consistent: true, Method: "read-txn"},
		SchemaDef: schemaDef{VecFormat: vecFormat}, Files: []recordFile{}, Blobs: []any{}, Derived: []any{}}
	if err := t.conn.QueryRowContext(ctx, "select sqlite_version(), vec_version()").Scan(&m.EngineVersion, &m.SchemaDef.VecVersion); err != nil {
		return dblifecycle.Outcome{}, t.classify(err)
	}
	cols, err := t.collections()
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	m.SchemaDef.Collections = cols
	if err := os.Mkdir(in.Out, 0o700); err != nil {
		if errors.Is(err, os.ErrExist) {
			return dblifecycle.Outcome{}, fmt.Errorf("%w: export directory %s", dbstore.ErrExists, in.Out)
		}
		return dblifecycle.Outcome{}, fmt.Errorf("%w: %v", dbstore.ErrInvalidInput, err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.RemoveAll(in.Out) // only the directory this export created
		}
	}()
	var records int64
	for i, c := range cols {
		f, err := exportCollection(ctx, t, in.Out, fmt.Sprintf("records-%06d.jsonl", i), c.Name)
		if err != nil {
			return dblifecycle.Outcome{}, err
		}
		m.Files = append(m.Files, f)
		records += f.Records
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	if err := writeFile(filepath.Join(in.Out, ManifestName), append(b, '\n')); err != nil {
		return dblifecycle.Outcome{}, err
	}
	ok = true
	return dblifecycle.Outcome{Result: map[string]any{
		"out": in.Out, "manifest": ManifestName, "schema_version": ExportSchema, "format_version": FormatVersion,
		"category": Category, "snapshot": m.Snapshot, "collections": len(cols), "records": records, "files": m.Files,
	}}, nil
}

func exportCollection(ctx context.Context, t *tx, dir, name, collection string) (recordFile, error) {
	rf := recordFile{Path: name, Collection: collection}
	rows, err := t.conn.QueryContext(ctx, "select m.id, m.revision, m.doc, v.emb from "+items(collection)+" m join "+vectors(collection)+" v on v.id = m.id order by m.id")
	if err != nil {
		return rf, t.classify(err)
	}
	defer rows.Close()
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return rf, err
	}
	defer f.Close()
	sum := sha256.New()
	w := bufio.NewWriterSize(io.MultiWriter(f, sum), 64<<10)
	for n := 0; rows.Next(); n++ {
		if n%256 == 0 {
			if err := ctx.Err(); err != nil {
				return rf, dbstore.FromContext(err)
			}
		}
		var r record
		var doc string
		var blob []byte
		if err := rows.Scan(&r.ID, &r.Revision, &doc, &blob); err != nil {
			return rf, t.classify(err)
		}
		v, err := vecdb.ParseF32(blob)
		if err != nil {
			return rf, fmt.Errorf("%w: collection %q vector %q: %v", dbstore.ErrIntegrity, collection, r.ID, err)
		}
		vb, _ := json.Marshal(encodeVector(v))
		r.Vector, r.Metadata = vb, json.RawMessage(doc)
		var line bytes.Buffer
		enc := json.NewEncoder(&line)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(r); err != nil {
			return rf, fmt.Errorf("%w: collection %q vector %q: %v", dbstore.ErrIntegrity, collection, r.ID, err)
		}
		// The export must stay importable: a line the reader would refuse
		// fails here instead of producing an export that cannot load.
		if line.Len() > MaxRecordBytes {
			return rf, fmt.Errorf("%w: collection %q vector %q encodes to over %d bytes", dbstore.ErrLimitExceeded, collection, r.ID, MaxRecordBytes)
		}
		if _, err := w.Write(line.Bytes()); err != nil {
			return rf, err
		}
		rf.Records++
		rf.Bytes += int64(line.Len())
	}
	if err := rows.Err(); err != nil {
		return rf, t.classify(err)
	}
	if err := w.Flush(); err != nil {
		return rf, err
	}
	if err := f.Sync(); err != nil {
		return rf, err
	}
	rf.SHA256 = hex.EncodeToString(sum.Sum(nil))
	return rf, f.Close()
}

func writeFile(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

var formatVersion = regexp.MustCompile(`^([0-9]+)\.([0-9]+)$`)
var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// readManifest validates the whole manifest before anything else runs.
// Unknown fields are tolerated: a later minor version may add them.
func readManifest(dir string) (manifest, error) {
	var m manifest
	p := filepath.Join(dir, ManifestName)
	info, err := os.Lstat(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return m, fmt.Errorf("%w: %s has no %s", dbstore.ErrNotFound, dir, ManifestName)
		}
		return m, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxManifest {
		return m, fmt.Errorf("%w: %s must be a regular file under %d bytes", dbstore.ErrMalformedInput, ManifestName, maxManifest)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, fmt.Errorf("%w: %s: %v", dbstore.ErrMalformedInput, ManifestName, err)
	}
	if m.SchemaVersion != ExportSchema {
		if strings.HasPrefix(m.SchemaVersion, "bashy-db-export-v") {
			return m, fmt.Errorf("%w: %s (this reader is %s)", dbstore.ErrVersionUnsupported, m.SchemaVersion, ExportSchema)
		}
		return m, fmt.Errorf("%w: schema_version %q is not %s", dbstore.ErrMalformedInput, m.SchemaVersion, ExportSchema)
	}
	fv := formatVersion.FindStringSubmatch(m.FormatVersion)
	if fv == nil {
		return m, fmt.Errorf("%w: format_version %q", dbstore.ErrMalformedInput, m.FormatVersion)
	}
	if fv[1] != "1" {
		return m, fmt.Errorf("%w: format_version %s (this reader accepts 1.x)", dbstore.ErrVersionUnsupported, m.FormatVersion)
	}
	if m.Category != Category || m.Engine != "sqlite" {
		return m, fmt.Errorf("%w: a %s/%s export cannot be imported as sqlite vectors", dbstore.ErrUnsupported, m.Engine, m.Category)
	}
	if m.SchemaDef.VecFormat != vecFormat {
		return m, fmt.Errorf("%w: vec_format %q (this reader is %q)", dbstore.ErrVersionUnsupported, m.SchemaDef.VecFormat, vecFormat)
	}
	if len(m.Blobs) != 0 || len(m.Derived) != 0 {
		return m, fmt.Errorf("%w: vector exports carry no blob files or derived indexes", dbstore.ErrMalformedInput)
	}
	cols := map[string]bool{}
	for _, c := range m.SchemaDef.Collections {
		if err := c.validate(); err != nil || cols[c.Name] {
			return m, fmt.Errorf("%w: collection %q: %v", dbstore.ErrMalformedInput, c.Name, err)
		}
		cols[c.Name] = true
	}
	files, paths := map[string]bool{}, map[string]bool{}
	for _, f := range m.Files {
		if !cols[f.Collection] || files[f.Collection] {
			return m, fmt.Errorf("%w: record file for collection %q", dbstore.ErrMalformedInput, f.Collection)
		}
		files[f.Collection] = true
		if !filepath.IsLocal(f.Path) || strings.ContainsAny(f.Path, `/\`) || f.Path == ManifestName || paths[f.Path] {
			return m, fmt.Errorf("%w: record file path %q", dbstore.ErrMalformedInput, f.Path)
		}
		paths[f.Path] = true
		if f.Records < 0 || f.Bytes < 0 || !sha256Hex.MatchString(f.SHA256) {
			return m, fmt.Errorf("%w: record file %q metadata", dbstore.ErrMalformedInput, f.Path)
		}
	}
	for c := range cols {
		if !files[c] {
			return m, fmt.Errorf("%w: collection %q has no record file", dbstore.ErrMalformedInput, c)
		}
	}
	return m, nil
}

// eachRecord streams one record file, decoding and validating every record
// against its collection (dimension, finiteness, metadata object), and
// verifies its record count, byte length and sha256 at the end. Memory is
// bounded by one record.
func eachRecord(ctx context.Context, dir string, f recordFile, c Collection, fn func(n int64, r loaded) error) error {
	p := filepath.Join(dir, f.Path)
	info, err := os.Lstat(p)
	if err != nil {
		return fmt.Errorf("%w: record file %s: %v", dbstore.ErrMalformedInput, f.Path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: record file %s is not a regular file", dbstore.ErrMalformedInput, f.Path)
	}
	file, err := os.Open(p)
	if err != nil {
		return err
	}
	defer file.Close()
	sum := sha256.New()
	rd := bufio.NewReaderSize(io.TeeReader(file, sum), 64<<10)
	var n, size int64
	prev := ""
	for {
		if n%256 == 0 {
			if err := ctx.Err(); err != nil {
				return dbstore.FromContext(err)
			}
		}
		line, err := readLine(rd)
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("%s record %d: %w", f.Path, n+1, err)
		}
		size += int64(len(line))
		n++
		var r record
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&r); err != nil {
			return fmt.Errorf("%w: %s record %d: %v", dbstore.ErrMalformedInput, f.Path, n, err)
		}
		if _, err := dec.Token(); err != io.EOF {
			return fmt.Errorf("%w: %s record %d has data after the record", dbstore.ErrMalformedInput, f.Path, n)
		}
		// Records are in strictly ascending id order (as exported), which
		// refuses a duplicate id with memory bounded by one record.
		if checkID(r.ID) != nil || r.Revision < 1 || (n > 1 && r.ID <= prev) {
			return fmt.Errorf("%w: %s record %d needs an id after the previous record's and a revision >= 1", dbstore.ErrMalformedInput, f.Path, n)
		}
		prev = r.ID
		// Exported vectors are always tagged: an untagged array would be a
		// rounding, not a lossless record.
		if t := bytes.TrimLeft(r.Vector, " \t"); len(t) == 0 || t[0] != '{' {
			return fmt.Errorf("%w: %s record %d vector must be tagged f32", dbstore.ErrMalformedInput, f.Path, n)
		}
		vec, err := parseVector(r.Vector, c)
		if err != nil {
			return fmt.Errorf("%w: %s record %d: %v", dbstore.ErrMalformedInput, f.Path, n, err)
		}
		if len(r.Metadata) == 0 {
			return fmt.Errorf("%w: %s record %d has no metadata object", dbstore.ErrMalformedInput, f.Path, n)
		}
		doc, err := metadata(r.Metadata)
		if err != nil {
			return fmt.Errorf("%w: %s record %d: %v", dbstore.ErrMalformedInput, f.Path, n, err)
		}
		if fn != nil {
			if err := fn(n, loaded{id: r.ID, rev: r.Revision, vec: vec, doc: doc}); err != nil {
				return err
			}
		}
	}
	if n != f.Records || size != f.Bytes {
		return fmt.Errorf("%w: %s has %d records/%d bytes, manifest says %d/%d", dbstore.ErrIntegrity, f.Path, n, size, f.Records, f.Bytes)
	}
	if got := hex.EncodeToString(sum.Sum(nil)); got != f.SHA256 {
		return fmt.Errorf("%w: %s sha256 %s, manifest says %s", dbstore.ErrIntegrity, f.Path, got, f.SHA256)
	}
	return nil
}

// readLine returns one newline-terminated line (newline included) of at
// most MaxRecordBytes. A final line without a newline is malformed.
func readLine(r *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		chunk, err := r.ReadSlice('\n')
		line = append(line, chunk...)
		if len(line) > MaxRecordBytes {
			return nil, fmt.Errorf("%w: record over %d bytes", dbstore.ErrLimitExceeded, MaxRecordBytes)
		}
		switch {
		case err == nil:
			return line, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case err == io.EOF && len(line) == 0:
			return nil, io.EOF
		case err == io.EOF:
			return nil, fmt.Errorf("%w: last record has no newline", dbstore.ErrMalformedInput)
		default:
			return nil, err
		}
	}
}

// runImport loads an export as one transaction. Merge (the default) needs
// every exported collection to exist with the same dimension, metric and
// model, and applies --on-conflict per same-ID vector; --replace (exclusive)
// removes every vector collection of the target and recreates the exported
// ones. Other tables in the database are never touched.
func runImport(ctx context.Context, h dbstore.Handle, in Input) (dblifecycle.Outcome, error) {
	if err := in.only(OpImport, "in", "on_conflict", "replace"); err != nil {
		return dblifecycle.Outcome{}, err
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
		return dblifecycle.Outcome{}, fmt.Errorf("%w: --replace empties the vector collections first; --on-conflict does not apply", dbstore.ErrInvalidInput)
	}
	// Validation first: the manifest and every record, before any change.
	m, err := readManifest(in.In)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	defs := map[string]Collection{}
	for _, c := range m.SchemaDef.Collections {
		defs[c.Name] = c
	}
	for _, f := range m.Files {
		if err := eachRecord(ctx, in.In, f, defs[f.Collection], nil); err != nil {
			return dblifecycle.Outcome{}, err
		}
	}
	t, err := begin(ctx, h, true)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	defer t.end()
	if in.Replace {
		cols, err := t.collections()
		if err != nil {
			return dblifecycle.Outcome{}, err
		}
		for _, c := range cols {
			if err := t.rmCollection(c.Name); err != nil {
				return dblifecycle.Outcome{}, err
			}
		}
		for _, c := range m.SchemaDef.Collections {
			if err := t.addCollection(c); err != nil {
				return dblifecycle.Outcome{}, err
			}
		}
	} else {
		for _, c := range m.SchemaDef.Collections {
			have, err := t.collection(c.Name)
			if err != nil {
				return dblifecycle.Outcome{}, fmt.Errorf("%w: merge needs collection %q to exist (add it, or use --replace --yes)", dbstore.ErrConflict, c.Name)
			}
			if have != c {
				return dblifecycle.Outcome{}, fmt.Errorf("%w: collection %q is %d/%s/%q, the export is %d/%s/%q; merge needs the same dimension, metric and model (or --replace --yes)",
					dbstore.ErrConflict, c.Name, have.Dimension, have.Metric, have.Model, c.Dimension, c.Metric, c.Model)
			}
		}
	}
	applied := map[string]int64{}
	var total int64
	for _, f := range m.Files {
		n, err := loadFile(ctx, t, in.In, f, defs[f.Collection], policy)
		if err != nil {
			return dblifecycle.Outcome{}, fmt.Errorf("%w (rolled back; the target is unchanged)", err)
		}
		applied[f.Collection] = n
		total += n
	}
	if err := t.commit(); err != nil {
		return dblifecycle.Outcome{}, err
	}
	mode := "merge"
	if in.Replace {
		mode = "replace"
	}
	return dblifecycle.Outcome{Result: map[string]any{
		"in": in.In, "mode": mode, "on_conflict": policy, "atomic": true, "format_version": m.FormatVersion,
		"source": m.Source, "collections": len(m.SchemaDef.Collections), "records_applied": total, "applied": applied,
	}}, nil
}

// loadFile re-streams a validated record file into its collection. The
// checksum is verified again at the end, inside the transaction, so a file
// changed after validation rolls the whole import back. A record keeps its
// exported revision when inserted; a replaced vector moves strictly past
// both revisions so a holder of either sees a conflict.
func loadFile(ctx context.Context, t *tx, dir string, f recordFile, c Collection, policy string) (int64, error) {
	var applied int64
	err := eachRecord(ctx, dir, f, c, func(n int64, r loaded) error {
		cur, ok, err := t.revision(c.Name, r.id)
		if err != nil {
			return err
		}
		switch {
		case !ok:
			err = t.insert(c.Name, r.id, r.rev, r.vec, r.doc)
		case policy == "skip":
			return nil
		case policy == "replace":
			rev := max(cur, r.rev)
			if rev == math.MaxInt64 {
				return fmt.Errorf("%w: collection %q record %d: vector %q has no revision after %d", dbstore.ErrLimitExceeded, c.Name, n, r.id, rev)
			}
			err = t.update(c.Name, r.id, rev+1, r.vec, r.doc)
		default:
			return fmt.Errorf("%w: collection %q record %d: vector %q already exists (--on-conflict error)", dbstore.ErrConflict, c.Name, n, r.id)
		}
		if err != nil {
			return fmt.Errorf("collection %q record %d: %w", c.Name, n, err)
		}
		applied++
		return nil
	})
	return applied, err
}

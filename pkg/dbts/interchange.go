package dbts

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/qiangli/yoke/pkg/dblifecycle"
	"github.com/qiangli/yoke/pkg/dbstore"
)

// Interchange format (contract §8): a directory holding manifest.json, with
// series definitions inline in schema_def and one JSONL record file per
// points chunk. Each points line carries the series identity with it, so a
// file is self-describing and chunks stay independent.
const (
	ExportSchema   = "bashy-db-export-v1"
	FormatVersion  = "1.0"
	ManifestName   = "manifest.json"
	MaxRecordBytes = 16 << 20
	maxManifest    = 16 << 20
	pointsPerFile  = 50000
)

type manifest struct {
	SchemaVersion string         `json:"schema_version"`
	FormatVersion string         `json:"format_version"`
	Category      string         `json:"category"`
	Engine        string         `json:"engine"`
	EngineVersion string         `json:"engine_version"`
	Source        manifestSource `json:"source"`
	Snapshot      snapshot       `json:"snapshot"`
	SchemaDef     tsSchemaDef    `json:"schema_def"`
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

type tsSchemaDef struct {
	Series []seriesDef `json:"series"`
}

type seriesDef struct {
	Name        string            `json:"name"`
	Labels      map[string]string `json:"labels,omitempty"`
	RetentionNs *int64            `json:"retention_ns,omitempty"`
}

type recordFile struct {
	Path    string `json:"path"`
	Kind    string `json:"kind"`
	Records int64  `json:"records"`
	Bytes   int64  `json:"bytes"`
	SHA256  string `json:"sha256"`
}

// maxImportFileBytes bounds one staged record file. Exports chunk at
// pointsPerFile records, so ordinary files are a few megabytes; anything
// larger is refused explicitly rather than staged without bound. Only one
// file is staged at a time, so import memory stays bounded by this cap,
// never by the dataset size, and nothing spills an in-memory database to
// disk: staging is private import bytes, not database storage.
const maxImportFileBytes = 64 << 20

// stagedPoint is one record decoded from staged file bytes, ready to
// apply with no further parsing.
type stagedPoint struct {
	series string
	labels string // canonical labels JSON
	ts     int64
	val    sampleValue
}

type pointRecord struct {
	Series string            `json:"series"`
	Labels map[string]string `json:"labels,omitempty"`
	Ts     json.RawMessage   `json:"ts"`
	Value  json.RawMessage   `json:"value"`
}

func runExport(ctx context.Context, h dbstore.Handle, in Input) (dblifecycle.Outcome, error) {
	if in.Out == "" || in.Out == "-" || !filepath.IsAbs(in.Out) {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: export writes a new directory; give --out DIR", dbstore.ErrInvalidInput)
	}
	t, err := begin(ctx, h, false)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	defer t.close()
	// begin already opened the read transaction: it is the snapshot, so
	// every series and point below is read at one point.
	ok, err := t.hasSchema()
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	id := h.Identity()
	m := manifest{SchemaVersion: ExportSchema, FormatVersion: FormatVersion, Category: "ts", Engine: "sqlite",
		Source:    manifestSource{ID: id.ID, Mode: string(id.Mode)},
		Snapshot:  snapshot{Consistent: true, Method: "read-txn"},
		SchemaDef: tsSchemaDef{Series: []seriesDef{}}, Files: []recordFile{}, Blobs: []any{}, Derived: []any{}}
	if err := t.queryRow("select sqlite_version()").Scan(&m.EngineVersion); err != nil {
		return dblifecycle.Outcome{}, t.classify(err)
	}
	if err := os.Mkdir(in.Out, 0o700); err != nil {
		if os.IsExist(err) {
			return dblifecycle.Outcome{}, fmt.Errorf("%w: export directory %s exists", dbstore.ErrExists, in.Out)
		}
		return dblifecycle.Outcome{}, fmt.Errorf("%w: export directory: %v", dbstore.ErrBackend, err)
	}
	remove := true
	defer func() {
		if remove {
			_ = os.RemoveAll(in.Out)
		}
	}()
	if ok {
		if err := writeExportFiles(ctx, t, in.Out, &m); err != nil {
			return dblifecycle.Outcome{}, err
		}
	}
	mb, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	if err := os.WriteFile(filepath.Join(in.Out, ManifestName), append(mb, '\n'), 0o600); err != nil {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: manifest: %v", dbstore.ErrBackend, err)
	}
	remove = false
	var points int64
	for _, f := range m.Files {
		points += f.Records
	}
	return dblifecycle.Outcome{Result: map[string]any{
		"out": in.Out, "series": len(m.SchemaDef.Series), "points": points, "files": len(m.Files),
	}}, nil
}

func writeExportFiles(ctx context.Context, t *tx, dir string, m *manifest) error {
	rows, err := t.query("select s.id, s.name, s.labels, s.retention_ns from " + tableSeries + " s order by s.name, s.labels")
	if err != nil {
		return err
	}
	defer rows.Close()
	type series struct {
		id        int64
		name      string
		labels    string
		retention *int64
	}
	var all []series
	for rows.Next() {
		var s series
		if err := rows.Scan(&s.id, &s.name, &s.labels, &s.retention); err != nil {
			return t.classify(err)
		}
		all = append(all, s)
	}
	if err := rows.Err(); err != nil {
		return t.classify(err)
	}
	var w *fileWriter
	flush := func() error {
		if w == nil {
			return nil
		}
		rec, err := w.close()
		if err != nil {
			return err
		}
		m.Files = append(m.Files, *rec)
		w = nil
		return nil
	}
	for _, s := range all {
		var lm map[string]string
		if s.labels != "" && s.labels != "{}" {
			if err := json.Unmarshal([]byte(s.labels), &lm); err != nil {
				return t.classify(fmt.Errorf("series %q labels: %v", s.name, err))
			}
		}
		m.SchemaDef.Series = append(m.SchemaDef.Series, seriesDef{Name: s.name, Labels: lm, RetentionNs: s.retention})
		prows, err := t.query("select p.ts, p.value_kind, p.value_int, p.value from "+tablePoints+" p where p.series_id = ? order by p.ts", s.id)
		if err != nil {
			return err
		}
		for prows.Next() {
			var ts int64
			var kind int
			var ival sql.NullInt64
			var fval float64
			if err := prows.Scan(&ts, &kind, &ival, &fval); err != nil {
				prows.Close()
				return t.classify(err)
			}
			v := sampleValue{fval: fval}
			if kind == valueKindInt && ival.Valid {
				v = sampleValue{isInt: true, ival: ival.Int64, fval: fval}
			}
			if w == nil {
				var err error
				w, err = newFileWriter(dir, len(m.Files))
				if err != nil {
					prows.Close()
					return err
				}
			}
			tsb, _ := json.Marshal(encodeTS(ts))
			vb, _ := json.Marshal(encodeSampleValue(v))
			lb, _ := json.Marshal(lm)
			line := `{"series":` + strconv.Quote(s.name) + `,"labels":` + string(orEmptyObj(lb)) +
				`,"ts":` + string(tsb) + `,"value":` + string(vb) + "}\n"
			if err := w.write(line); err != nil {
				prows.Close()
				return err
			}
			if w.records >= pointsPerFile {
				if err := flush(); err != nil {
					prows.Close()
					return err
				}
			}
			if err := ctx.Err(); err != nil {
				prows.Close()
				return dbstore.FromContext(err)
			}
		}
		cerr := prows.Err()
		prows.Close()
		if cerr != nil {
			return t.classify(cerr)
		}
	}
	return flush()
}

func orEmptyObj(b []byte) []byte {
	if len(b) == 0 || string(b) == "null" {
		return []byte("{}")
	}
	return b
}

type fileWriter struct {
	f       *os.File
	path    string
	name    string
	records int64
	bytes   int64
	h       hash.Hash
	w       *bufio.Writer
}

func newFileWriter(dir string, n int) (*fileWriter, error) {
	name := fmt.Sprintf("points-%06d.jsonl", n+1)
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return nil, fmt.Errorf("%w: export file %s exists", dbstore.ErrExists, name)
		}
		return nil, fmt.Errorf("%w: export file: %v", dbstore.ErrBackend, err)
	}
	h := sha256.New()
	return &fileWriter{f: f, path: filepath.Join(dir, name), name: name, h: h, w: bufio.NewWriter(io.MultiWriter(f, h))}, nil
}

func (w *fileWriter) write(line string) error {
	if len(line) > MaxRecordBytes {
		return fmt.Errorf("%w: record over %d bytes", dbstore.ErrLimitExceeded, MaxRecordBytes)
	}
	n, err := w.w.WriteString(line)
	if err != nil {
		return fmt.Errorf("%w: export file: %v", dbstore.ErrBackend, err)
	}
	w.records++
	w.bytes += int64(n)
	return nil
}

func (w *fileWriter) close() (*recordFile, error) {
	if err := w.w.Flush(); err != nil {
		w.f.Close()
		return nil, fmt.Errorf("%w: export file: %v", dbstore.ErrBackend, err)
	}
	if err := w.f.Close(); err != nil {
		return nil, fmt.Errorf("%w: export file: %v", dbstore.ErrBackend, err)
	}
	return &recordFile{Path: w.name, Kind: "points", Records: w.records, Bytes: w.bytes,
		SHA256: hex.EncodeToString(w.h.Sum(nil))}, nil
}

func runImport(ctx context.Context, h dbstore.Handle, in Input) (dblifecycle.Outcome, error) {
	if in.In == "" || in.In == "-" || !filepath.IsAbs(in.In) {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: import reads a directory; give --in DIR", dbstore.ErrInvalidInput)
	}
	onConflict := in.OnConflict
	if onConflict == "" {
		onConflict = "error"
	}
	m, err := readManifest(in.In)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	known := map[string]bool{}
	for _, sd := range m.SchemaDef.Series {
		// readManifest already validated every name and label set.
		labels, _ := canonicalLabels(sd.Labels)
		known[sd.Name+"\x00"+labels] = true
	}
	// The write transaction opens before staging: every destructive step
	// below (replace deletes, series registration, point writes) and every
	// staged apply share one transaction, so a late failure rolls back
	// and the target is unchanged. Staging reads each file once into
	// bounded private bytes and applies those same bytes, so caller files
	// cannot change between validation and apply.
	t, err := begin(ctx, h, true)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	defer t.close()
	if err := t.ensureSchema(); err != nil {
		return dblifecycle.Outcome{}, err
	}
	if in.Replace {
		if _, err := t.exec("delete from " + tablePoints); err != nil {
			return dblifecycle.Outcome{}, err
		}
		if _, err := t.exec("delete from " + tableSeries); err != nil {
			return dblifecycle.Outcome{}, err
		}
	}
	ids := map[string]int64{}
	ingested, skipped, replaced := 0, 0, 0
	for _, sd := range m.SchemaDef.Series {
		labels, err := canonicalLabels(sd.Labels)
		if err != nil {
			return dblifecycle.Outcome{}, fmt.Errorf("manifest series %q: %w", sd.Name, err)
		}
		key := sd.Name + "\x00" + labels
		id, err := t.seriesID(sd.Name, labels, true)
		if err != nil {
			return dblifecycle.Outcome{}, err
		}
		ids[key] = id
		if in.Replace && sd.RetentionNs != nil {
			if _, err := t.exec("update "+tableSeries+" set retention_ns = ? where id = ?", *sd.RetentionNs, id); err != nil {
				return dblifecycle.Outcome{}, err
			}
		}
	}
	for _, f := range m.Files {
		staged, err := stageFile(ctx, in.In, f, known)
		if err != nil {
			return dblifecycle.Outcome{}, err
		}
		n, err := applyStaged(ctx, t, staged, ids, onConflict)
		if err != nil {
			return dblifecycle.Outcome{}, err
		}
		ingested += n.ingested
		skipped += n.skipped
		replaced += n.replaced
	}
	if err := t.commit(); err != nil {
		return dblifecycle.Outcome{}, err
	}
	res := map[string]any{"series": len(m.SchemaDef.Series), "ingested": ingested}
	switch onConflict {
	case "skip":
		res["skipped"] = skipped
	case "replace":
		res["replaced"] = replaced
	}
	return dblifecycle.Outcome{Result: res}, nil
}

func readManifest(dir string) (*manifest, error) {
	b, err := os.ReadFile(filepath.Join(dir, ManifestName))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: no %s in %s", dbstore.ErrNotFound, ManifestName, dir)
		}
		return nil, fmt.Errorf("%w: manifest: %v", dbstore.ErrBackend, err)
	}
	if len(b) > maxManifest {
		return nil, fmt.Errorf("%w: manifest over %d bytes", dbstore.ErrMalformedInput, maxManifest)
	}
	var m manifest
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("%w: manifest: %v", dbstore.ErrMalformedInput, err)
	}
	if m.SchemaVersion != ExportSchema {
		return nil, fmt.Errorf("%w: export schema %q (want %s)", dbstore.ErrVersionUnsupported, m.SchemaVersion, ExportSchema)
	}
	major := strings.SplitN(m.FormatVersion, ".", 2)[0]
	if major != "1" {
		return nil, fmt.Errorf("%w: format version %q", dbstore.ErrVersionUnsupported, m.FormatVersion)
	}
	if m.Category != "ts" {
		return nil, fmt.Errorf("%w: category %q is not a time-series export", dbstore.ErrInvalidInput, m.Category)
	}
	if m.Engine != "sqlite" {
		return nil, fmt.Errorf("%w: engine %q (only sqlite time-series interchange is proven)", dbstore.ErrInvalidInput, m.Engine)
	}
	names := map[string]bool{}
	for _, sd := range m.SchemaDef.Series {
		if err := checkSeriesName(sd.Name); err != nil {
			return nil, fmt.Errorf("manifest series: %w", err)
		}
		if _, err := canonicalLabels(sd.Labels); err != nil {
			return nil, fmt.Errorf("manifest series %q: %w", sd.Name, err)
		}
		if sd.RetentionNs != nil && *sd.RetentionNs <= 0 {
			return nil, fmt.Errorf("%w: manifest series %q retention", dbstore.ErrInvalidInput, sd.Name)
		}
	}
	for _, f := range m.Files {
		if f.Kind != "points" {
			return nil, fmt.Errorf("%w: file kind %q", dbstore.ErrInvalidInput, f.Kind)
		}
		if names[f.Path] || f.Path == "" || f.Path != filepath.Base(f.Path) || strings.HasPrefix(f.Path, ".") {
			return nil, fmt.Errorf("%w: file path %q", dbstore.ErrMalformedInput, f.Path)
		}
		names[f.Path] = true
		if f.Records < 0 || f.Bytes < 0 {
			return nil, fmt.Errorf("%w: file %q counts", dbstore.ErrMalformedInput, f.Path)
		}
	}
	return &m, nil
}

type counts struct {
	ingested, skipped, replaced int
}

// stageFile reads one record file ONCE into bounded private bytes and
// validates those exact bytes: size, sha256, counts and strict decoding
// of every record. The caller applies the returned points; the file is
// never re-read, so caller files cannot change between validation and
// apply. Only one file is staged at a time, within maxImportFileBytes.
func stageFile(ctx context.Context, dir string, f recordFile, known map[string]bool) ([]stagedPoint, error) {
	if err := ctx.Err(); err != nil {
		return nil, dbstore.FromContext(err)
	}
	if f.Bytes < 0 || f.Bytes > maxImportFileBytes {
		return nil, fmt.Errorf("%w: %s declares %d bytes, over the %d-byte staged file limit", dbstore.ErrLimitExceeded, f.Path, f.Bytes, maxImportFileBytes)
	}
	// Lstat before open: refuse FIFOs/sockets/devices so a crafted
	// manifest path cannot block the importer on open or read.
	if fi, err := os.Lstat(filepath.Join(dir, f.Path)); err != nil {
		return nil, fmt.Errorf("%w: records file %s: %v", dbstore.ErrNotFound, f.Path, err)
	} else if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: records file %s is not a regular file", dbstore.ErrMalformedInput, f.Path)
	}
	fh, err := os.Open(filepath.Join(dir, f.Path))
	if err != nil {
		return nil, fmt.Errorf("%w: records file %s: %v", dbstore.ErrNotFound, f.Path, err)
	}
	defer fh.Close()
	raw, err := io.ReadAll(io.LimitReader(fh, maxImportFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", dbstore.ErrMalformedInput, f.Path, err)
	}
	if int64(len(raw)) > maxImportFileBytes {
		return nil, fmt.Errorf("%w: %s holds over the %d-byte staged file limit", dbstore.ErrLimitExceeded, f.Path, maxImportFileBytes)
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != strings.ToLower(f.SHA256) {
		return nil, fmt.Errorf("%w: %s checksum", dbstore.ErrIntegrity, f.Path)
	}
	var out []stagedPoint
	var records, nbytes int64
	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		if err := ctx.Err(); err != nil {
			return nil, dbstore.FromContext(err)
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if len(line) > MaxRecordBytes {
			return nil, fmt.Errorf("%w: %s record %d over %d bytes", dbstore.ErrLimitExceeded, f.Path, records+1, MaxRecordBytes)
		}
		// Every record line carries its newline in the byte count, as the
		// exporter writes them; a final line without one fails the count
		// check below instead of importing silently.
		nbytes += int64(len(line)) + 1
		var rec pointRecord
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&rec); err != nil {
			return nil, fmt.Errorf("%w: %s record %d: %v", dbstore.ErrMalformedInput, f.Path, records+1, err)
		}
		// Strict one-value-per-line: a second JSON value or trailing
		// garbage on the line is malformed, not a silent truncation.
		var extra json.RawMessage
		if err := dec.Decode(&extra); err != io.EOF {
			if err == nil {
				err = fmt.Errorf("trailing data after record")
			}
			return nil, fmt.Errorf("%w: %s record %d: %v", dbstore.ErrMalformedInput, f.Path, records+1, err)
		}
		if err := checkSeriesName(rec.Series); err != nil {
			return nil, fmt.Errorf("%s record %d: %w", f.Path, records+1, err)
		}
		labels, err := canonicalLabels(rec.Labels)
		if err != nil {
			return nil, fmt.Errorf("%s record %d: %w", f.Path, records+1, err)
		}
		if !known[rec.Series+"\x00"+labels] {
			return nil, fmt.Errorf("%w: %s record %d names series %q outside schema_def", dbstore.ErrMalformedInput, f.Path, records+1, rec.Series)
		}
		ts, err := parseTS("ts", rec.Ts)
		if err != nil {
			return nil, fmt.Errorf("%s record %d: %w", f.Path, records+1, err)
		}
		v, err := parseSampleValue("value", rec.Value)
		if err != nil {
			return nil, fmt.Errorf("%s record %d: %w", f.Path, records+1, err)
		}
		out = append(out, stagedPoint{series: rec.Series, labels: labels, ts: ts, val: v})
		records++
	}
	if records != f.Records || nbytes != f.Bytes {
		return nil, fmt.Errorf("%w: %s declares %d records/%d bytes, holds %d/%d", dbstore.ErrMalformedInput, f.Path, f.Records, f.Bytes, records, nbytes)
	}
	return out, nil
}

// applyStaged writes staged points inside the import transaction. The
// points were decoded from the staged bytes by stageFile, so no parsing
// remains to fail or ignore here; every error aborts before commit and
// leaves the target unchanged.
func applyStaged(ctx context.Context, t *tx, staged []stagedPoint, ids map[string]int64, onConflict string) (counts, error) {
	var c counts
	for _, s := range staged {
		id, ok := ids[s.series+"\x00"+s.labels]
		if !ok {
			return c, fmt.Errorf("%w: staged point names series %q outside schema_def", dbstore.ErrMalformedInput, s.series)
		}
		if err := ctx.Err(); err != nil {
			return c, dbstore.FromContext(err)
		}
		switch onConflict {
		case "replace":
			var n int
			if err := t.queryRow("select count(*) from "+tablePoints+" where series_id = ? and ts = ?", id, s.ts).Scan(&n); err != nil {
				return c, t.classify(err)
			}
			if _, err := t.exec("insert or replace into "+tablePoints+"(series_id, ts, value, value_kind, value_int) values (?, ?, ?, ?, ?)", id, s.ts, s.val.fval, kindOf(s.val), intOf(s.val)); err != nil {
				return c, err
			}
			if n == 0 {
				c.ingested++
			} else {
				c.replaced++
			}
		case "skip":
			r, err := t.exec("insert or ignore into "+tablePoints+"(series_id, ts, value, value_kind, value_int) values (?, ?, ?, ?, ?)", id, s.ts, s.val.fval, kindOf(s.val), intOf(s.val))
			if err != nil {
				return c, err
			}
			if n, _ := r.RowsAffected(); n == 0 {
				c.skipped++
			} else {
				c.ingested++
			}
		default:
			if _, err := t.exec("insert into "+tablePoints+"(series_id, ts, value, value_kind, value_int) values (?, ?, ?, ?, ?)", id, s.ts, s.val.fval, kindOf(s.val), intOf(s.val)); err != nil {
				if dbstore.Code(err) == "conflict" {
					return c, fmt.Errorf("%w: import point %q at ts %d already exists (on_conflict=error)", err, s.series, s.ts)
				}
				return c, err
			}
			c.ingested++
		}
	}
	return c, nil
}

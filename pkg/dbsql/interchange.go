package dbsql

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/qiangli/yoke/pkg/dblifecycle"
	"github.com/qiangli/yoke/pkg/dbstore"
)

// Interchange format (contract §8): a directory holding manifest.json and
// one JSONL record file per table. Each record line is a JSON array of
// §8.3 values: the rowid first when the file declares one, then the stored
// (non-generated) columns in manifest order.
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
	Objects       []object   `json:"objects"`
	UserVersion   int64      `json:"user_version"`
	ApplicationID int64      `json:"application_id"`
	Sequences     []sequence `json:"sequences"`
}

type object struct {
	Type  string `json:"type"`
	Name  string `json:"name"`
	Table string `json:"tbl_name"`
	SQL   string `json:"sql"`
}

type sequence struct {
	Name string          `json:"name"`
	Seq  json.RawMessage `json:"seq"`
}

type recordFile struct {
	Path    string   `json:"path"`
	Table   string   `json:"table"`
	Rowid   string   `json:"rowid,omitempty"`
	Columns []string `json:"columns"`
	Records int64    `json:"records"`
	Bytes   int64    `json:"bytes"`
	SHA256  string   `json:"sha256"`
}

func quote(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }

func runExport(ctx context.Context, h dbstore.Handle, in Input) (dblifecycle.Outcome, error) {
	db, err := handleDB(h)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	if in.SQL != "" || len(in.Params) != 0 || len(in.Statements) != 0 || in.In != "" || in.OnConflict != "" || in.Replace || in.Limit != 0 || in.MaxBytes != 0 || in.Since != "" {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: export takes --out DIR", dbstore.ErrInvalidInput)
	}
	if in.Out == "" || in.Out == "-" || !filepath.IsAbs(in.Out) {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: export writes a new directory; give --out DIR", dbstore.ErrInvalidInput)
	}
	s, err := begin(ctx, db, true, h)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	defer s.end()
	// A read transaction is the snapshot: WAL readers never block writers
	// or other readers, and every table below is read at one point.
	if _, err := s.conn.ExecContext(ctx, "begin"); err != nil {
		return dblifecycle.Outcome{}, s.classify(ctx, err)
	}
	objs, err := userObjects(ctx, s)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	id := h.Identity()
	m := manifest{SchemaVersion: ExportSchema, FormatVersion: FormatVersion, Category: "sql", Engine: "sqlite",
		Source: manifestSource{ID: id.ID, Mode: string(id.Mode)}, Snapshot: snapshot{Consistent: true, Method: "read-txn"},
		Files: []recordFile{}, Blobs: []any{}, Derived: []any{}}
	if err := s.conn.QueryRowContext(ctx, "select sqlite_version()").Scan(&m.EngineVersion); err != nil {
		return dblifecycle.Outcome{}, s.classify(ctx, err)
	}
	var tables []schemaObject
	for _, o := range objs {
		if o.SQL == nil {
			continue
		}
		m.SchemaDef.Objects = append(m.SchemaDef.Objects, object{Type: o.Type, Name: o.Name, Table: o.Table, SQL: *o.SQL})
		if o.Type != "table" {
			continue
		}
		kind, err := tableInfo(ctx, s, &o)
		if err != nil {
			return dblifecycle.Outcome{}, err
		}
		if kind != "table" {
			return dblifecycle.Outcome{}, fmt.Errorf("%w: %s table %q has no logical export in v1 (virtual tables are not exported)", dbstore.ErrUnsupported, kind, o.Name)
		}
		tables = append(tables, o)
	}
	if m.SchemaDef.Objects == nil {
		m.SchemaDef.Objects = []object{}
	}
	if m.SchemaDef.UserVersion, err = s.scalar(ctx, "pragma user_version"); err != nil {
		return dblifecycle.Outcome{}, err
	}
	if m.SchemaDef.ApplicationID, err = s.scalar(ctx, "pragma application_id"); err != nil {
		return dblifecycle.Outcome{}, err
	}
	if m.SchemaDef.Sequences, err = readSequences(ctx, s); err != nil {
		return dblifecycle.Outcome{}, err
	}
	var warnings []string
	if n, err := s.scalar(ctx, "select count(*) from sqlite_schema where name like 'sqlite!_stat%' escape '!'"); err != nil {
		return dblifecycle.Outcome{}, err
	} else if n > 0 {
		m.Derived = append(m.Derived, map[string]any{"kind": "sqlite-stat", "rebuild_from": "ANALYZE"})
		warnings = append(warnings, "sqlite_stat* planner statistics are derived and not exported; run ANALYZE after import")
	}
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
	for i, t := range tables {
		f, err := exportTable(ctx, s, in.Out, fmt.Sprintf("records-%06d.jsonl", i), t)
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
		"snapshot": m.Snapshot, "tables": len(tables), "objects": len(m.SchemaDef.Objects), "records": records, "files": m.Files,
	}, Warnings: warnings}, nil
}

func readSequences(ctx context.Context, s *session) ([]sequence, error) {
	seqs := []sequence{}
	n, err := s.scalar(ctx, "select count(*) from sqlite_schema where name = 'sqlite_sequence'")
	if err != nil || n == 0 {
		return seqs, err
	}
	rows, err := s.conn.QueryContext(ctx, "select name, +seq from sqlite_sequence order by rowid")
	if err != nil {
		return nil, s.classify(ctx, err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var seq int64
		if err := rows.Scan(&name, &seq); err != nil {
			return nil, s.classify(ctx, err)
		}
		b, _ := json.Marshal(tagged{T: "int", V: strconv.FormatInt(seq, 10)})
		seqs = append(seqs, sequence{Name: name, Seq: b})
	}
	return seqs, s.classify(ctx, rows.Err())
}

// rowidAlias picks a spelling of the implicit rowid that no column shadows.
func rowidAlias(t schemaObject) (string, error) {
	if t.Rowid == nil || !*t.Rowid {
		return "", nil
	}
	for _, alias := range []string{"rowid", "_rowid_", "oid"} {
		shadowed := false
		for _, c := range t.Columns {
			if strings.EqualFold(c["name"].(string), alias) {
				shadowed = true
			}
		}
		if !shadowed {
			return alias, nil
		}
	}
	return "", fmt.Errorf("%w: table %q shadows every rowid alias; its rowids cannot be exported", dbstore.ErrUnsupported, t.Name)
}

func storedColumns(t schemaObject) []string {
	var cols []string
	for _, c := range t.Columns {
		if c["generated"] == nil && c["hidden"] == nil {
			cols = append(cols, c["name"].(string))
		}
	}
	return cols
}

func exportTable(ctx context.Context, s *session, dir, name string, t schemaObject) (recordFile, error) {
	alias, err := rowidAlias(t)
	if err != nil {
		return recordFile{}, err
	}
	rf := recordFile{Path: name, Table: t.Name, Rowid: alias, Columns: storedColumns(t)}
	// Unary plus drops the declared type, so the driver returns each
	// value's own storage class (no DATE parsing) — exactly what is stored.
	exprs := make([]string, 0, len(rf.Columns)+1)
	if alias != "" {
		exprs = append(exprs, alias)
	}
	for _, c := range rf.Columns {
		exprs = append(exprs, "+"+quote(c))
	}
	q := "select " + strings.Join(exprs, ", ") + " from " + quote(t.Name)
	if alias != "" {
		q += " order by " + alias
	}
	rows, err := s.conn.QueryContext(ctx, q)
	if err != nil {
		return rf, s.classify(ctx, err)
	}
	defer rows.Close()
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return rf, err
	}
	defer f.Close()
	sum := sha256.New()
	w := bufio.NewWriterSize(io.MultiWriter(f, sum), 64<<10)
	values := make([]any, len(exprs))
	ptrs := make([]any, len(exprs))
	for i := range values {
		ptrs[i] = &values[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return rf, s.classify(ctx, err)
		}
		rec := make([]any, len(values))
		for i, v := range values {
			if rec[i], _, err = encodeValue(v); err != nil {
				return rf, fmt.Errorf("table %q record %d: %w", t.Name, rf.Records+1, err)
			}
		}
		line, err := json.Marshal(rec)
		if err != nil {
			return rf, err
		}
		if len(line)+1 > MaxRecordBytes {
			return rf, fmt.Errorf("%w: table %q record %d is over %d bytes", dbstore.ErrLimitExceeded, t.Name, rf.Records+1, MaxRecordBytes)
		}
		if _, err := w.Write(append(line, '\n')); err != nil {
			return rf, err
		}
		rf.Records++
		rf.Bytes += int64(len(line) + 1)
	}
	if err := rows.Err(); err != nil {
		return rf, s.classify(ctx, err)
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
	if m.Category != "sql" || m.Engine != "sqlite" {
		return m, fmt.Errorf("%w: a %s/%s export cannot be imported into sqlite sql", dbstore.ErrUnsupported, m.Engine, m.Category)
	}
	if len(m.Blobs) != 0 {
		return m, fmt.Errorf("%w: sql exports carry no blob files", dbstore.ErrMalformedInput)
	}
	if m.SchemaDef.UserVersion < math.MinInt32 || m.SchemaDef.UserVersion > math.MaxInt32 || m.SchemaDef.ApplicationID < math.MinInt32 || m.SchemaDef.ApplicationID > math.MaxInt32 {
		return m, fmt.Errorf("%w: user_version/application_id are 32-bit", dbstore.ErrMalformedInput)
	}
	kinds := map[string]string{}
	for _, o := range m.SchemaDef.Objects {
		switch o.Type {
		case "table", "index", "view", "trigger":
		default:
			return m, fmt.Errorf("%w: object type %q", dbstore.ErrMalformedInput, o.Type)
		}
		if o.Name == "" || strings.TrimSpace(o.SQL) == "" || strings.HasPrefix(strings.ToLower(o.Name), "sqlite_") {
			return m, fmt.Errorf("%w: object %q needs a name and sql", dbstore.ErrMalformedInput, o.Name)
		}
		key := strings.ToLower(o.Name)
		if _, dup := kinds[key]; dup {
			return m, fmt.Errorf("%w: object %q appears twice", dbstore.ErrMalformedInput, o.Name)
		}
		kinds[key] = o.Type
	}
	for _, o := range m.SchemaDef.Objects {
		if t := kinds[strings.ToLower(o.Table)]; (o.Type == "index" && t != "table") || (o.Type == "trigger" && t != "table" && t != "view") {
			return m, fmt.Errorf("%w: %s %q refers to unknown table %q", dbstore.ErrMalformedInput, o.Type, o.Name, o.Table)
		}
	}
	files := map[string]bool{}
	paths := map[string]bool{}
	for _, f := range m.Files {
		if kinds[strings.ToLower(f.Table)] != "table" || files[strings.ToLower(f.Table)] {
			return m, fmt.Errorf("%w: record file for table %q", dbstore.ErrMalformedInput, f.Table)
		}
		files[strings.ToLower(f.Table)] = true
		if !filepath.IsLocal(f.Path) || strings.ContainsAny(f.Path, `/\`) || f.Path == ManifestName || paths[f.Path] {
			return m, fmt.Errorf("%w: record file path %q", dbstore.ErrMalformedInput, f.Path)
		}
		paths[f.Path] = true
		if f.Rowid != "" && f.Rowid != "rowid" && f.Rowid != "_rowid_" && f.Rowid != "oid" {
			return m, fmt.Errorf("%w: rowid alias %q", dbstore.ErrMalformedInput, f.Rowid)
		}
		seen := map[string]bool{}
		for _, c := range f.Columns {
			if c == "" || seen[strings.ToLower(c)] {
				return m, fmt.Errorf("%w: columns of %q", dbstore.ErrMalformedInput, f.Table)
			}
			seen[strings.ToLower(c)] = true
		}
		if len(f.Columns) == 0 || f.Records < 0 || f.Bytes < 0 || !sha256Hex.MatchString(f.SHA256) {
			return m, fmt.Errorf("%w: record file %q metadata", dbstore.ErrMalformedInput, f.Path)
		}
	}
	for key, kind := range kinds {
		if kind == "table" && !files[key] {
			return m, fmt.Errorf("%w: table %q has no record file", dbstore.ErrMalformedInput, key)
		}
	}
	for _, sq := range m.SchemaDef.Sequences {
		v, err := decodeValue(sq.Seq)
		if _, isInt := v.(int64); err != nil || !isInt || kinds[strings.ToLower(sq.Name)] != "table" {
			return m, fmt.Errorf("%w: sequence for %q", dbstore.ErrMalformedInput, sq.Name)
		}
	}
	return m, nil
}

// eachRecord streams one record file, decoding every value strictly, and
// verifies its record count, byte length and sha256 at the end. Memory is
// bounded by one record (MaxRecordBytes).
func eachRecord(ctx context.Context, dir string, f recordFile, fn func(n int64, values []any) error) error {
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
	var sum hash.Hash = sha256.New()
	r := bufio.NewReaderSize(io.TeeReader(file, sum), 64<<10)
	width := len(f.Columns)
	if f.Rowid != "" {
		width++
	}
	var n, size int64
	for {
		if n%1024 == 0 {
			if err := ctx.Err(); err != nil {
				return dbstore.FromContext(err)
			}
		}
		line, err := readLine(r)
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("%s record %d: %w", f.Path, n+1, err)
		}
		size += int64(len(line))
		n++
		var raw []json.RawMessage
		if err := json.Unmarshal(line, &raw); err != nil || len(raw) != width {
			return fmt.Errorf("%w: %s record %d is not a %d-value array", dbstore.ErrMalformedInput, f.Path, n, width)
		}
		values := make([]any, width)
		for i, item := range raw {
			if values[i], err = decodeValue(item); err != nil {
				return fmt.Errorf("%s record %d value %d: %w", f.Path, n, i+1, err)
			}
		}
		if _, isInt := values[0].(int64); f.Rowid != "" && !isInt {
			return fmt.Errorf("%w: %s record %d rowid is not an int", dbstore.ErrMalformedInput, f.Path, n)
		}
		if fn != nil {
			if err := fn(n, values); err != nil {
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

func runImport(ctx context.Context, h dbstore.Handle, in Input) (dblifecycle.Outcome, error) {
	db, err := handleDB(h)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	if in.SQL != "" || len(in.Params) != 0 || len(in.Statements) != 0 || in.Out != "" || in.Limit != 0 || in.MaxBytes != 0 || in.Since != "" {
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
	// Validation first: the manifest and every record, before any change.
	m, err := readManifest(in.In)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	for _, f := range m.Files {
		if err := eachRecord(ctx, in.In, f, nil); err != nil {
			return dblifecycle.Outcome{}, err
		}
	}
	s, err := begin(ctx, db, false, h)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	defer s.end()
	applied := map[string]int64{}
	var total int64
	_, _, err = s.atomic(ctx, in.Replace, func() error {
		if _, err := s.conn.ExecContext(ctx, "pragma defer_foreign_keys=on"); err != nil {
			return s.classify(ctx, err)
		}
		if in.Replace {
			if err := dropAll(ctx, s); err != nil {
				return err
			}
			if err := createObjects(ctx, s, m, false); err != nil {
				return err
			}
		} else if err := sameSchema(ctx, s, m); err != nil {
			return err
		}
		for _, f := range m.Files {
			n, err := loadFile(ctx, s, in.In, f, policy)
			if err != nil {
				return err
			}
			applied[f.Table] = n
			total += n
		}
		if !in.Replace {
			return mergeSequences(ctx, s, m)
		}
		// Triggers come last so loading the rows does not fire them.
		if err := createObjects(ctx, s, m, true); err != nil {
			return err
		}
		return finishReplace(ctx, s, m)
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
		"source": m.Source, "tables": len(m.Files), "records_applied": total, "applied": applied,
	}}, nil
}

func dropAll(ctx context.Context, s *session) error {
	objs, err := userObjects(ctx, s)
	if err != nil {
		return err
	}
	for _, kind := range []string{"trigger", "view", "table"} {
		for i := len(objs) - 1; i >= 0; i-- {
			if objs[i].Type != kind {
				continue
			}
			if _, err := s.conn.ExecContext(ctx, "drop "+kind+" "+quote(objs[i].Name)); err != nil {
				return s.classify(ctx, err)
			}
		}
	}
	if n, err := s.scalar(ctx, "select count(*) from sqlite_schema where name = 'sqlite_sequence'"); err != nil {
		return err
	} else if n > 0 {
		if _, err := s.conn.ExecContext(ctx, "delete from sqlite_sequence"); err != nil {
			return s.classify(ctx, err)
		}
	}
	return nil
}

// createObjects runs the exported definitions in source order, either every
// non-trigger object or only the triggers. A definition
// that changed any row (a smuggled second statement) is malformed input;
// the final schema comparison catches a smuggled extra object.
func createObjects(ctx context.Context, s *session, m manifest, triggers bool) error {
	for _, o := range m.SchemaDef.Objects {
		if (o.Type == "trigger") != triggers {
			continue
		}
		before, err := s.scalar(ctx, "select total_changes()")
		if err != nil {
			return err
		}
		if _, err := s.conn.ExecContext(ctx, o.SQL); err != nil {
			return fmt.Errorf("create %s %q: %w", o.Type, o.Name, s.classify(ctx, err))
		}
		after, err := s.scalar(ctx, "select total_changes()")
		if err != nil {
			return err
		}
		if after != before {
			return fmt.Errorf("%w: definition of %s %q changed data", dbstore.ErrMalformedInput, o.Type, o.Name)
		}
	}
	return nil
}

func objectSet(objs []object) map[object]bool {
	set := map[object]bool{}
	for _, o := range objs {
		set[o] = true
	}
	return set
}

func targetObjects(ctx context.Context, s *session) ([]object, error) {
	objs, err := userObjects(ctx, s)
	if err != nil {
		return nil, err
	}
	var out []object
	for _, o := range objs {
		if o.SQL != nil {
			out = append(out, object{Type: o.Type, Name: o.Name, Table: o.Table, SQL: *o.SQL})
		}
	}
	return out, nil
}

// sameSchema is the merge precondition: every exported object exists in
// the target with the identical definition. Creating or altering schema is
// --replace or `db sql schema apply`, both exclusive.
func sameSchema(ctx context.Context, s *session, m manifest) error {
	objs, err := targetObjects(ctx, s)
	if err != nil {
		return err
	}
	have := objectSet(objs)
	for _, o := range m.SchemaDef.Objects {
		if !have[o] {
			return fmt.Errorf("%w: target has no identical %s %q; merge needs the same schema (use --replace --yes, or db sql schema apply first)", dbstore.ErrConflict, o.Type, o.Name)
		}
	}
	return nil
}

func finishReplace(ctx context.Context, s *session, m manifest) error {
	objs, err := targetObjects(ctx, s)
	if err != nil {
		return err
	}
	want, have := objectSet(m.SchemaDef.Objects), objectSet(objs)
	if len(objs) != len(m.SchemaDef.Objects) || len(want) != len(have) {
		return fmt.Errorf("%w: the imported definitions created objects the manifest does not declare", dbstore.ErrMalformedInput)
	}
	for o := range want {
		if !have[o] {
			return fmt.Errorf("%w: imported %s %q does not match its definition", dbstore.ErrMalformedInput, o.Type, o.Name)
		}
	}
	for _, f := range m.Files {
		n, err := s.scalar(ctx, "select count(*) from "+quote(f.Table))
		if err != nil {
			return err
		}
		if n != f.Records {
			return fmt.Errorf("%w: table %q has %d rows after import, manifest says %d", dbstore.ErrMalformedInput, f.Table, n, f.Records)
		}
	}
	// Loading explicit ids into AUTOINCREMENT tables already wrote sequence
	// rows; the exported values replace them exactly.
	if len(m.SchemaDef.Sequences) > 0 {
		if _, err := s.conn.ExecContext(ctx, "delete from sqlite_sequence"); err != nil {
			return s.classify(ctx, err)
		}
	}
	for _, sq := range m.SchemaDef.Sequences {
		v, _ := decodeValue(sq.Seq)
		if _, err := s.conn.ExecContext(ctx, "insert into sqlite_sequence(name, seq) values (?, ?)", sq.Name, v); err != nil {
			return s.classify(ctx, err)
		}
	}
	// Integers validated as 32-bit above; pragma values cannot be bound.
	for _, p := range []struct {
		name string
		v    int64
	}{{"user_version", m.SchemaDef.UserVersion}, {"application_id", m.SchemaDef.ApplicationID}} {
		if _, err := s.conn.ExecContext(ctx, "pragma "+p.name+" = "+strconv.FormatInt(p.v, 10)); err != nil {
			return s.classify(ctx, err)
		}
	}
	return nil
}

func mergeSequences(ctx context.Context, s *session, m manifest) error {
	for _, sq := range m.SchemaDef.Sequences {
		v, _ := decodeValue(sq.Seq)
		r, err := s.conn.ExecContext(ctx, "update sqlite_sequence set seq = max(seq, ?) where name = ?", v, sq.Name)
		if err != nil {
			return s.classify(ctx, err)
		}
		if n, _ := r.RowsAffected(); n == 0 {
			if _, err := s.conn.ExecContext(ctx, "insert into sqlite_sequence(name, seq) values (?, ?)", sq.Name, v); err != nil {
				return s.classify(ctx, err)
			}
		}
	}
	return nil
}

// loadFile re-streams a validated record file into its table. Conflicts
// are uniqueness conflicts only (upsert), so skip and replace never hide a
// NOT NULL, CHECK or foreign-key violation. The checksum is verified again
// at the end, inside the transaction, so a file changed after validation
// rolls the whole import back.
func loadFile(ctx context.Context, s *session, dir string, f recordFile, policy string) (int64, error) {
	cols := make([]string, 0, len(f.Columns)+1)
	if f.Rowid != "" {
		cols = append(cols, f.Rowid)
	}
	for _, c := range f.Columns {
		cols = append(cols, quote(c))
	}
	q := "insert into " + quote(f.Table) + " (" + strings.Join(cols, ", ") + ") values (" + strings.TrimSuffix(strings.Repeat("?, ", len(cols)), ", ") + ")"
	switch policy {
	case "skip":
		q += " on conflict do nothing"
	case "replace":
		sets := make([]string, 0, len(f.Columns))
		for _, c := range f.Columns {
			sets = append(sets, quote(c)+" = excluded."+quote(c))
		}
		q += " on conflict do update set " + strings.Join(sets, ", ")
	}
	stmt, err := s.conn.PrepareContext(ctx, q)
	if err != nil {
		return 0, fmt.Errorf("table %q: %w", f.Table, s.classify(ctx, err))
	}
	defer stmt.Close()
	var applied int64
	err = eachRecord(ctx, dir, f, func(n int64, values []any) error {
		r, err := stmt.ExecContext(ctx, values...)
		if err != nil {
			return fmt.Errorf("table %q record %d: %w", f.Table, n, s.classify(ctx, err))
		}
		k, _ := r.RowsAffected()
		applied += k
		return nil
	})
	return applied, err
}

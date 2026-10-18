// Package dbdoc is the Sprint 453 JSON document category (story
// d51543b72755) over the sqlitedb adapter: `db doc get|find|put|patch|rm|
// batch`, `db doc collection create|drop|list`, `db doc index add|rm|list`
// and the document logical interchange `db doc export|import`.
//
// A collection is an explicit SQLite table created by `collection create`;
// nothing is pre-created. Each document is a JSON object stored as the text
// the caller gave (re-encoded with numbers verbatim, so nested types and
// big or decimal numbers survive), under a stable caller-chosen or generated
// ID and a per-document revision that every write increments. Every
// operation registers with dblifecycle, so CLI, MCP and owner sessions reach
// it through one claim guard, owner admission, request-id replay and
// hold-epoch check. Bashy writes all SQL itself: filter values are bound
// parameters, and field paths are checked against a strict grammar before
// they become the literal JSON path an expression index can match. Every
// schema change (collection create/drop, index add/rm, import --replace) is
// exclusive.
package dbdoc

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/qiangli/yoke/pkg/dbstore"
	"github.com/qiangli/yoke/pkg/dbstore/sqlitedb"
)

// Operation names, as registered with dblifecycle.
const (
	OpGet              = "doc.get"
	OpFind             = "doc.find"
	OpPut              = "doc.put"
	OpPatch            = "doc.patch"
	OpRm               = "doc.rm"
	OpBatch            = "doc.batch"
	OpCollectionCreate = "doc.collection.create"
	OpCollectionDrop   = "doc.collection.drop"
	OpCollectionList   = "doc.collection.list"
	OpIndexAdd         = "doc.index.add"
	OpIndexRm          = "doc.index.rm"
	OpIndexList        = "doc.index.list"
	OpExport           = "doc.export"
	OpImport           = "doc.import"
)

// Bounds declared for the document category (contract §6).
const (
	DefaultLimit    = 100
	MaxLimit        = 10000
	DefaultMaxBytes = 256 << 10
	MaxMaxBytes     = 8 << 20
	MaxDocBytes     = 4 << 20
	MaxIDBytes      = 512
	MaxBatch        = 1000
	MaxIndexFields  = 8
	maxFilterNodes  = 64
)

// Input is the typed input every doc operation accepts, from CLI flags, an
// --input JSON document or the MCP/owner request.
type Input struct {
	Collection string          `json:"collection,omitempty"`
	ID         string          `json:"id,omitempty"`
	Doc        json.RawMessage `json:"doc,omitempty"`
	Patch      json.RawMessage `json:"patch,omitempty"`
	IfRevision *int64          `json:"if_revision,omitempty"`
	IfAbsent   bool            `json:"if_absent,omitempty"`
	Ops        []BatchOp       `json:"ops,omitempty"`
	Filter     json.RawMessage `json:"filter,omitempty"`
	Params     json.RawMessage `json:"params,omitempty"`
	Index      string          `json:"index,omitempty"`
	Fields     []string        `json:"fields,omitempty"`
	Unique     bool            `json:"unique,omitempty"`
	Limit      int             `json:"limit,omitempty"`
	MaxBytes   int             `json:"max_bytes,omitempty"`
	Since      string          `json:"since,omitempty"`
	Out        string          `json:"out,omitempty"`
	In         string          `json:"in,omitempty"`
	OnConflict string          `json:"on_conflict,omitempty"`
	Replace    bool            `json:"replace,omitempty"`
}

// BatchOp is one entry of an atomic batch: put, patch or rm.
type BatchOp struct {
	Op         string          `json:"op"`
	ID         string          `json:"id,omitempty"`
	Doc        json.RawMessage `json:"doc,omitempty"`
	Patch      json.RawMessage `json:"patch,omitempty"`
	IfRevision *int64          `json:"if_revision,omitempty"`
	IfAbsent   bool            `json:"if_absent,omitempty"`
}

// DecodeInput strictly decodes a typed input document.
func DecodeInput(raw json.RawMessage) (Input, error) {
	var in Input
	if len(raw) == 0 {
		return in, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return in, fmt.Errorf("%w: doc input: %v", dbstore.ErrInvalidInput, err)
	}
	if dec.More() {
		return in, fmt.Errorf("%w: doc input has trailing data", dbstore.ErrInvalidInput)
	}
	return in, nil
}

func (in Input) bounds() (limit, maxBytes int, err error) {
	limit, maxBytes = in.Limit, in.MaxBytes
	if limit == 0 {
		limit = DefaultLimit
	}
	if maxBytes == 0 {
		maxBytes = DefaultMaxBytes
	}
	if limit < 0 || limit > MaxLimit || maxBytes < 0 || maxBytes > MaxMaxBytes {
		return 0, 0, fmt.Errorf("%w: --limit 1..%d and --max-bytes 1..%d", dbstore.ErrInvalidInput, MaxLimit, MaxMaxBytes)
	}
	return limit, maxBytes, nil
}

// fields lists the input fields an operation accepts; any other set field is
// invalid_input, never silently ignored.
func (in Input) only(op string, allowed ...string) error {
	set := map[string]bool{
		"collection": in.Collection != "", "id": in.ID != "", "doc": len(in.Doc) != 0, "patch": len(in.Patch) != 0,
		"if_revision": in.IfRevision != nil, "if_absent": in.IfAbsent, "ops": len(in.Ops) != 0, "filter": len(in.Filter) != 0,
		"params": len(in.Params) != 0, "index": in.Index != "", "fields": len(in.Fields) != 0, "unique": in.Unique,
		"limit": in.Limit != 0, "max_bytes": in.MaxBytes != 0, "since": in.Since != "", "out": in.Out != "",
		"in": in.In != "", "on_conflict": in.OnConflict != "", "replace": in.Replace,
	}
	for _, a := range allowed {
		delete(set, a)
	}
	for name, on := range set {
		if on {
			return fmt.Errorf("%w: %s does not take %s", dbstore.ErrInvalidInput, op, name)
		}
	}
	return nil
}

var nameRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

// checkName validates a collection or index name. Names are lowercase so
// SQLite's case-insensitive identifiers cannot alias two collections, and
// "__" is reserved as the collection/index separator.
func checkName(kind, name string) error {
	if !nameRE.MatchString(name) || strings.Contains(name, "__") {
		return fmt.Errorf("%w: %s name %q (want [a-z][a-z0-9_]{0,62}, no \"__\")", dbstore.ErrInvalidInput, kind, name)
	}
	return nil
}

func checkID(id string) error {
	if id == "" || len(id) > MaxIDBytes || !utf8.ValidString(id) {
		return fmt.Errorf("%w: document id must be 1..%d bytes of UTF-8 text", dbstore.ErrInvalidInput, MaxIDBytes)
	}
	for _, r := range id {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w: document id has a control character", dbstore.ErrInvalidInput)
		}
	}
	return nil
}

func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// Metadata tables. They are created by the first `collection create`, never
// earlier, and are the only record of which tables are collections.
const (
	metaCollections = "bashy_doc_collections"
	metaIndexes     = "bashy_doc_indexes"
	tablePrefix     = "bashy_doc_c_"
	indexPrefix     = "bashy_doc_i_"
	docFormat       = "1"
)

func table(collection string) string { return `"` + tablePrefix + collection + `"` }
func indexName(collection, index string) string {
	return `"` + indexPrefix + collection + "__" + index + `"`
}

func handleDB(h dbstore.Handle) (*sql.DB, error) {
	db, ok := h.(*sqlitedb.DB)
	if !ok {
		return nil, fmt.Errorf("%w: doc category needs the sqlite engine", dbstore.ErrUnsupported)
	}
	return db.SQL(), nil
}

// tx is one connection running one transaction for one operation.
type tx struct {
	conn *sql.Conn
	ctx  context.Context
	done bool
}

// begin starts a read transaction (one snapshot) or an IMMEDIATE write
// transaction (the writer lock up front, so a contested write is busy at
// begin rather than failing half way).
func begin(ctx context.Context, h dbstore.Handle, write bool) (*tx, error) {
	db, err := handleDB(h)
	if err != nil {
		return nil, err
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, sqlitedb.ClassifyCtx(ctx, err)
	}
	stmt := "begin"
	if write {
		stmt = "begin immediate"
	}
	if _, err := conn.ExecContext(ctx, stmt); err != nil {
		conn.Close()
		return nil, sqlitedb.ClassifyCtx(ctx, err)
	}
	return &tx{conn: conn, ctx: ctx}, nil
}

func (t *tx) classify(err error) error {
	if err == nil {
		return nil
	}
	if dbstore.Code(err) != "backend_error" {
		return err
	}
	return sqlitedb.ClassifyCtx(t.ctx, err)
}

func (t *tx) exec(q string, args ...any) (sql.Result, error) {
	r, err := t.conn.ExecContext(t.ctx, q, args...)
	return r, t.classify(err)
}

func (t *tx) commit() error {
	_, err := t.conn.ExecContext(t.ctx, "commit")
	if err == nil {
		t.done = true
	}
	return t.classify(err)
}

// end rolls back anything not committed and returns the connection.
func (t *tx) end() {
	if !t.done {
		if _, err := t.conn.ExecContext(context.Background(), "rollback"); err != nil && !strings.Contains(err.Error(), "no transaction is active") {
			_ = t.conn.Raw(func(any) error { return driver.ErrBadConn })
		}
	}
	_ = t.conn.Close()
}

// hasMeta reports whether any collection was ever created in the target.
func (t *tx) hasMeta() (bool, error) {
	var n int
	err := t.conn.QueryRowContext(t.ctx, "select count(*) from sqlite_schema where type = 'table' and name = ?", metaCollections).Scan(&n)
	return n > 0, t.classify(err)
}

// collection checks an explicit collection exists; it is never created here.
func (t *tx) collection(name string) error {
	if err := checkName("collection", name); err != nil {
		return err
	}
	ok, err := t.hasMeta()
	if err != nil {
		return err
	}
	if ok {
		var n int
		if err := t.conn.QueryRowContext(t.ctx, "select count(*) from "+metaCollections+" where name = ?", name).Scan(&n); err != nil {
			return t.classify(err)
		}
		ok = n > 0
	}
	if !ok {
		return fmt.Errorf("%w: collection %q (create it with db doc collection create)", dbstore.ErrNotFound, name)
	}
	return nil
}

// collections lists the target's collections in name order.
func (t *tx) collections() ([]string, error) {
	ok, err := t.hasMeta()
	if err != nil || !ok {
		return []string{}, err
	}
	rows, err := t.conn.QueryContext(t.ctx, "select name from "+metaCollections+" order by name")
	if err != nil {
		return nil, t.classify(err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, t.classify(err)
		}
		out = append(out, n)
	}
	return out, t.classify(rows.Err())
}

// IndexDef is a declared expression index over one or more field paths.
type IndexDef struct {
	Name   string   `json:"name"`
	Fields []string `json:"fields"`
	Unique bool     `json:"unique"`
}

func (t *tx) indexes(collection string) ([]IndexDef, error) {
	rows, err := t.conn.QueryContext(t.ctx, "select name, fields, uniq from "+metaIndexes+" where collection = ? order by name", collection)
	if err != nil {
		return nil, t.classify(err)
	}
	defer rows.Close()
	out := []IndexDef{}
	for rows.Next() {
		var d IndexDef
		var fields string
		if err := rows.Scan(&d.Name, &fields, &d.Unique); err != nil {
			return nil, t.classify(err)
		}
		if err := json.Unmarshal([]byte(fields), &d.Fields); err != nil {
			return nil, fmt.Errorf("%w: index %q metadata: %v", dbstore.ErrIntegrity, d.Name, err)
		}
		out = append(out, d)
	}
	return out, t.classify(rows.Err())
}

func (t *tx) createCollection(name string) error {
	if err := checkName("collection", name); err != nil {
		return err
	}
	if _, err := t.exec("create table if not exists " + metaCollections + " (name text primary key not null, format text not null) strict"); err != nil {
		return err
	}
	if _, err := t.exec("create table if not exists " + metaIndexes + " (collection text not null, name text not null, fields text not null, uniq integer not null, primary key (collection, name)) strict"); err != nil {
		return err
	}
	var n int
	if err := t.conn.QueryRowContext(t.ctx, "select count(*) from sqlite_schema where lower(name) = ?", tablePrefix+name).Scan(&n); err != nil {
		return t.classify(err)
	}
	if _, err := t.exec("insert into "+metaCollections+" (name, format) values (?, ?)", name, docFormat); err != nil {
		if dbstore.Code(err) == "conflict" {
			return fmt.Errorf("%w: collection %q", dbstore.ErrExists, name)
		}
		return err
	}
	if n > 0 {
		return fmt.Errorf("%w: table %s exists outside the document category", dbstore.ErrExists, table(name))
	}
	_, err := t.exec("create table " + table(name) + " (id text primary key not null, revision integer not null check (revision > 0), doc text not null check (json_valid(doc) and json_type(doc) = 'object')) strict")
	return err
}

func (t *tx) dropCollection(name string) error {
	if err := t.collection(name); err != nil {
		return err
	}
	if _, err := t.exec("drop table " + table(name)); err != nil {
		return err
	}
	if _, err := t.exec("delete from "+metaIndexes+" where collection = ?", name); err != nil {
		return err
	}
	_, err := t.exec("delete from "+metaCollections+" where name = ?", name)
	return err
}

func (t *tx) addIndex(collection string, d IndexDef) error {
	if err := checkName("index", d.Name); err != nil {
		return err
	}
	if len(d.Fields) == 0 || len(d.Fields) > MaxIndexFields {
		return fmt.Errorf("%w: an index takes 1..%d --field paths", dbstore.ErrInvalidInput, MaxIndexFields)
	}
	exprs := make([]string, len(d.Fields))
	for i, f := range d.Fields {
		p, err := jsonPath(f)
		if err != nil {
			return err
		}
		exprs[i] = extract(p)
	}
	b, _ := json.Marshal(d.Fields)
	if _, err := t.exec("insert into "+metaIndexes+" (collection, name, fields, uniq) values (?, ?, ?, ?)", collection, d.Name, string(b), d.Unique); err != nil {
		if dbstore.Code(err) == "conflict" {
			return fmt.Errorf("%w: index %q on collection %q", dbstore.ErrExists, d.Name, collection)
		}
		return err
	}
	q := "create index "
	if d.Unique {
		q = "create unique index "
	}
	if _, err := t.exec(q + indexName(collection, d.Name) + " on " + table(collection) + " (" + strings.Join(exprs, ", ") + ")"); err != nil {
		if dbstore.Code(err) == "conflict" {
			return fmt.Errorf("%w: existing documents violate unique index %q: %v", dbstore.ErrConflict, d.Name, err)
		}
		return err
	}
	return nil
}

func (t *tx) rmIndex(collection, name string) error {
	if err := checkName("index", name); err != nil {
		return err
	}
	r, err := t.exec("delete from "+metaIndexes+" where collection = ? and name = ?", collection, name)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: index %q on collection %q", dbstore.ErrNotFound, name, collection)
	}
	_, err = t.exec("drop index " + indexName(collection, name))
	return err
}

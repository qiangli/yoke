// Package dbvec is the Sprint 453 vector category (story 7e9e0ba8dc34) over
// the sqlitedb adapter and sqlite-vec's vec0 (pkg/dbstore/vecdb): `db vec
// put|get|rm|batch|search`, `db vec collection add|rm|list` and the vector
// logical interchange `db vec export|import`.
//
// A collection is created only by `collection add`, which persists its
// dimension, metric and model identity; nothing is pre-created. Each item is
// an ID, a per-item revision every write increments, a float32 embedding
// stored losslessly in a vec0 table (which also enforces the dimension) and a
// JSON metadata object in a companion table. Search ranks exactly — no ANN
// index — with sqlite-vec's distance function for the collection's metric,
// over the items a metadata filter (the db doc grammar, bound parameters
// only) admits. Vectors are always supplied by the caller: this package makes
// no network call and generates no embedding. Every operation registers with
// dblifecycle, so CLI, MCP and owner sessions reach it through one claim
// guard, owner admission, request-id replay and hold-epoch check; every
// schema change (collection add/rm, import --replace) is exclusive.
package dbvec

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"database/sql/driver"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/qiangli/yoke/pkg/dbdoc"
	"github.com/qiangli/yoke/pkg/dbstore"
	"github.com/qiangli/yoke/pkg/dbstore/sqlitedb"
	"github.com/qiangli/yoke/pkg/dbstore/vecdb"
)

// Operation names, as registered with dblifecycle.
const (
	OpPut            = "vec.put"
	OpGet            = "vec.get"
	OpRm             = "vec.rm"
	OpBatch          = "vec.batch"
	OpSearch         = "vec.search"
	OpCollectionAdd  = "vec.collection.add"
	OpCollectionRm   = "vec.collection.rm"
	OpCollectionList = "vec.collection.list"
	OpExport         = "vec.export"
	OpImport         = "vec.import"
)

// Bounds declared for the vector category (contract §6). MaxDimension is
// sqlite-vec's own vec0 limit.
const (
	DefaultLimit     = 10
	MaxLimit         = 1000
	DefaultMaxBytes  = 256 << 10
	MaxMaxBytes      = 8 << 20
	MaxDimension     = 8192
	MaxMetadataBytes = 64 << 10
	MaxIDBytes       = 512
	MaxModelBytes    = 256
	MaxBatch         = 1000
)

// Metrics are the vec0 distance metrics a collection may declare.
var Metrics = []string{"cosine", "l2", "l1"}

// Input is the typed input every vec operation accepts, from CLI flags, an
// --input JSON document or the MCP/owner request.
type Input struct {
	Collection     string          `json:"collection,omitempty"`
	Dimension      int             `json:"dimension,omitempty"`
	Metric         string          `json:"metric,omitempty"`
	Model          string          `json:"model,omitempty"`
	ID             string          `json:"id,omitempty"`
	Vector         json.RawMessage `json:"vector,omitempty"`
	Metadata       json.RawMessage `json:"metadata,omitempty"`
	IfRevision     *int64          `json:"if_revision,omitempty"`
	IfAbsent       bool            `json:"if_absent,omitempty"`
	Ops            []BatchOp       `json:"ops,omitempty"`
	Filter         json.RawMessage `json:"filter,omitempty"`
	Params         json.RawMessage `json:"params,omitempty"`
	IncludeVectors bool            `json:"include_vectors,omitempty"`
	Limit          int             `json:"limit,omitempty"`
	MaxBytes       int             `json:"max_bytes,omitempty"`
	Since          string          `json:"since,omitempty"`
	Out            string          `json:"out,omitempty"`
	In             string          `json:"in,omitempty"`
	OnConflict     string          `json:"on_conflict,omitempty"`
	Replace        bool            `json:"replace,omitempty"`
}

// BatchOp is one entry of an atomic batch: put or rm.
type BatchOp struct {
	Op         string          `json:"op"`
	ID         string          `json:"id,omitempty"`
	Vector     json.RawMessage `json:"vector,omitempty"`
	Metadata   json.RawMessage `json:"metadata,omitempty"`
	Model      string          `json:"model,omitempty"`
	IfRevision *int64          `json:"if_revision,omitempty"`
	IfAbsent   bool            `json:"if_absent,omitempty"`
}

// DecodeInput strictly decodes a typed input document. An unknown field (an
// unconfigured embedding request among them) is invalid_input, never ignored.
func DecodeInput(raw json.RawMessage) (Input, error) {
	var in Input
	if len(raw) == 0 {
		return in, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return in, fmt.Errorf("%w: vec input: %v", dbstore.ErrInvalidInput, err)
	}
	if dec.More() {
		return in, fmt.Errorf("%w: vec input has trailing data", dbstore.ErrInvalidInput)
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

// only lists the input fields an operation accepts; any other set field is
// invalid_input, never silently ignored.
func (in Input) only(op string, allowed ...string) error {
	set := map[string]bool{
		"collection": in.Collection != "", "dimension": in.Dimension != 0, "metric": in.Metric != "", "model": in.Model != "",
		"id": in.ID != "", "vector": len(in.Vector) != 0, "metadata": len(in.Metadata) != 0, "if_revision": in.IfRevision != nil,
		"if_absent": in.IfAbsent, "ops": len(in.Ops) != 0, "filter": len(in.Filter) != 0, "params": len(in.Params) != 0,
		"include_vectors": in.IncludeVectors, "limit": in.Limit != 0, "max_bytes": in.MaxBytes != 0, "since": in.Since != "",
		"out": in.Out != "", "in": in.In != "", "on_conflict": in.OnConflict != "", "replace": in.Replace,
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

// checkName validates a collection name. Names are lowercase so SQLite's
// case-insensitive identifiers cannot alias two collections, and "__" is
// reserved: it separates the name from the vec0 table suffix, so no
// collection can collide with another's vec0 shadow tables.
func checkName(name string) error {
	if !nameRE.MatchString(name) || strings.Contains(name, "__") {
		return fmt.Errorf("%w: collection name %q (want [a-z][a-z0-9_]{0,62}, no \"__\")", dbstore.ErrInvalidInput, name)
	}
	return nil
}

func checkID(id string) error {
	if id == "" || len(id) > MaxIDBytes || !utf8.ValidString(id) {
		return fmt.Errorf("%w: vector id must be 1..%d bytes of UTF-8 text", dbstore.ErrInvalidInput, MaxIDBytes)
	}
	for _, r := range id {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w: vector id has a control character", dbstore.ErrInvalidInput)
		}
	}
	return nil
}

func checkModel(model string) error {
	if model == "" || len(model) > MaxModelBytes || !utf8.ValidString(model) || strings.TrimSpace(model) != model {
		return fmt.Errorf("%w: --model is the embedding model identity, 1..%d bytes without surrounding space", dbstore.ErrInvalidInput, MaxModelBytes)
	}
	for _, r := range model {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w: model has a control character", dbstore.ErrInvalidInput)
		}
	}
	return nil
}

func checkMetric(metric string) error {
	for _, m := range Metrics {
		if metric == m {
			return nil
		}
	}
	return fmt.Errorf("%w: --metric %q (want %s)", dbstore.ErrInvalidInput, metric, strings.Join(Metrics, "|"))
}

func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// Collection is a collection's persisted definition.
type Collection struct {
	Name      string `json:"name"`
	Dimension int    `json:"dimension"`
	Metric    string `json:"metric"`
	Model     string `json:"model"`
	DType     string `json:"dtype"`
}

func (c Collection) validate() error {
	if err := checkName(c.Name); err != nil {
		return err
	}
	if c.Dimension < 1 || c.Dimension > MaxDimension {
		return fmt.Errorf("%w: --dimension 1..%d", dbstore.ErrInvalidInput, MaxDimension)
	}
	if err := checkMetric(c.Metric); err != nil {
		return err
	}
	if c.DType != "f32" {
		return fmt.Errorf("%w: dtype %q (collections store f32)", dbstore.ErrInvalidInput, c.DType)
	}
	return checkModel(c.Model)
}

// Vector is the tagged lossless form (contract §8.3): little-endian float32
// in base64.
type Vector struct {
	T     string `json:"t"`
	DType string `json:"dtype"`
	B64   string `json:"b64"`
}

func encodeVector(v []float32) Vector {
	return Vector{T: "vec", DType: "f32", B64: base64.StdEncoding.EncodeToString(vecdb.F32(v))}
}

// parseVector accepts the tagged f32 form (bit-exact) or a JSON array of
// numbers, each rounded to the nearest float32 — the collection's declared
// dtype. It refuses a wrong dimension, a value outside float32, NaN or ±Inf,
// and for cosine an all-zero vector (its distance is undefined; vec0 would
// store it and rank it NULL).
func parseVector(raw json.RawMessage, c Collection) ([]float32, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("%w: vector is required (supply the embedding; no provider is called)", dbstore.ErrInvalidInput)
	}
	var v []float32
	switch bytes.TrimLeft(raw, " \t\r\n")[0] {
	case '{':
		var tv Vector
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&tv); err != nil || dec.More() {
			return nil, fmt.Errorf("%w: vector wants {\"t\":\"vec\",\"dtype\":\"f32\",\"b64\":…} or a number array", dbstore.ErrInvalidInput)
		}
		if tv.T != "vec" || tv.DType != "f32" {
			return nil, fmt.Errorf("%w: vector t=%q dtype=%q (want vec/f32)", dbstore.ErrInvalidInput, tv.T, tv.DType)
		}
		b, err := base64.StdEncoding.Strict().DecodeString(tv.B64)
		if err != nil {
			return nil, fmt.Errorf("%w: vector b64: %v", dbstore.ErrInvalidInput, err)
		}
		if v, err = vecdb.ParseF32(b); err != nil {
			return nil, err
		}
	case '[':
		var vals []any
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if err := dec.Decode(&vals); err != nil || dec.More() {
			return nil, fmt.Errorf("%w: vector array must hold only numbers", dbstore.ErrInvalidInput)
		}
		v = make([]float32, len(vals))
		for i, val := range vals {
			n, ok := val.(json.Number)
			if !ok {
				return nil, fmt.Errorf("%w: vector[%d] is not a number", dbstore.ErrInvalidInput, i)
			}
			f, err := strconv.ParseFloat(n.String(), 32)
			if err != nil {
				return nil, fmt.Errorf("%w: vector[%d] %s is not a float32", dbstore.ErrInvalidInput, i, n)
			}
			v[i] = float32(f)
		}
	default:
		return nil, fmt.Errorf("%w: vector wants a tagged f32 vector or a number array", dbstore.ErrInvalidInput)
	}
	if len(v) != c.Dimension {
		return nil, fmt.Errorf("%w: vector has dimension %d; collection %q is %d", dbstore.ErrInvalidInput, len(v), c.Name, c.Dimension)
	}
	zero := true
	for i, f := range v {
		if math.IsNaN(float64(f)) || math.IsInf(float64(f), 0) {
			return nil, fmt.Errorf("%w: vector[%d] is not finite", dbstore.ErrInvalidInput, i)
		}
		zero = zero && f == 0
	}
	if zero && c.Metric == "cosine" {
		return nil, fmt.Errorf("%w: a zero vector has no cosine distance", dbstore.ErrInvalidInput)
	}
	return v, nil
}

// checkModelMatch refuses a vector the caller says came from another model.
func checkModelMatch(model string, c Collection) error {
	if model != "" && model != c.Model {
		return fmt.Errorf("%w: vector model %q, collection %q holds %q embeddings", dbstore.ErrInvalidInput, model, c.Name, c.Model)
	}
	return nil
}

// metadata canonicalizes an optional metadata object ({} when absent).
func metadata(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "{}", nil
	}
	if len(raw) > MaxMetadataBytes {
		return "", fmt.Errorf("%w: metadata is over %d bytes", dbstore.ErrLimitExceeded, MaxMetadataBytes)
	}
	b, err := dbdoc.CanonicalObject("metadata", raw)
	if err != nil {
		return "", err
	}
	if len(b) > MaxMetadataBytes {
		return "", fmt.Errorf("%w: metadata is over %d bytes", dbstore.ErrLimitExceeded, MaxMetadataBytes)
	}
	return string(b), nil
}

// Metadata tables. They are created by the first `collection add`, never
// earlier, and are the only record of which tables are collections.
const (
	metaCollections = "bashy_vec_collections"
	itemPrefix      = "bashy_vec_m_"
	vectorPrefix    = "bashy_vec_v_"
	vecFormat       = "1"
)

func items(collection string) string   { return `"` + itemPrefix + collection + `"` }
func vectors(collection string) string { return `"` + vectorPrefix + collection + `__e"` }

func handleDB(h dbstore.Handle) (*sql.DB, error) {
	db, ok := h.(*sqlitedb.DB)
	if !ok {
		return nil, fmt.Errorf("%w: vec category needs the sqlite engine", dbstore.ErrUnsupported)
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

func (t *tx) hasMeta() (bool, error) {
	var n int
	err := t.conn.QueryRowContext(t.ctx, "select count(*) from sqlite_schema where type = 'table' and name = ?", metaCollections).Scan(&n)
	return n > 0, t.classify(err)
}

// collection loads an explicit collection; it is never created here.
func (t *tx) collection(name string) (Collection, error) {
	c := Collection{Name: name}
	if err := checkName(name); err != nil {
		return c, err
	}
	ok, err := t.hasMeta()
	if err != nil {
		return c, err
	}
	if ok {
		err := t.conn.QueryRowContext(t.ctx, "select dimension, metric, model, dtype from "+metaCollections+" where name = ?", name).Scan(&c.Dimension, &c.Metric, &c.Model, &c.DType)
		if errors.Is(err, sql.ErrNoRows) {
			ok = false
		} else if err != nil {
			return c, t.classify(err)
		}
	}
	if !ok {
		return c, fmt.Errorf("%w: vector collection %q (add it with db vec collection add)", dbstore.ErrNotFound, name)
	}
	if err := c.validate(); err != nil {
		return c, fmt.Errorf("%w: collection %q metadata: %v", dbstore.ErrIntegrity, name, err)
	}
	return c, nil
}

// collections lists the target's collections in name order.
func (t *tx) collections() ([]Collection, error) {
	ok, err := t.hasMeta()
	if err != nil || !ok {
		return []Collection{}, err
	}
	rows, err := t.conn.QueryContext(t.ctx, "select name, dimension, metric, model, dtype from "+metaCollections+" order by name")
	if err != nil {
		return nil, t.classify(err)
	}
	defer rows.Close()
	out := []Collection{}
	for rows.Next() {
		var c Collection
		if err := rows.Scan(&c.Name, &c.Dimension, &c.Metric, &c.Model, &c.DType); err != nil {
			return nil, t.classify(err)
		}
		out = append(out, c)
	}
	return out, t.classify(rows.Err())
}

func (t *tx) addCollection(c Collection) error {
	if err := c.validate(); err != nil {
		return err
	}
	if _, err := t.exec("create table if not exists " + metaCollections + " (name text primary key not null, dimension integer not null, metric text not null, model text not null, dtype text not null, format text not null) strict"); err != nil {
		return err
	}
	var n int
	if err := t.conn.QueryRowContext(t.ctx, "select count(*) from sqlite_schema where lower(name) in (?, ?)", itemPrefix+c.Name, vectorPrefix+c.Name+"__e").Scan(&n); err != nil {
		return t.classify(err)
	}
	if _, err := t.exec("insert into "+metaCollections+" (name, dimension, metric, model, dtype, format) values (?, ?, ?, ?, ?, ?)", c.Name, c.Dimension, c.Metric, c.Model, c.DType, vecFormat); err != nil {
		if dbstore.Code(err) == "conflict" {
			return fmt.Errorf("%w: vector collection %q", dbstore.ErrExists, c.Name)
		}
		return err
	}
	if n > 0 {
		return fmt.Errorf("%w: tables for collection %q exist outside the vector category", dbstore.ErrExists, c.Name)
	}
	if _, err := t.exec("create table " + items(c.Name) + " (id text primary key not null, revision integer not null check (revision > 0), doc text not null check (json_valid(doc) and json_type(doc) = 'object')) strict"); err != nil {
		return err
	}
	// The dimension and metric are validated integers/enum values above.
	_, err := t.exec(fmt.Sprintf("create virtual table %s using vec0(id text primary key, emb float[%d] distance_metric=%s)", vectors(c.Name), c.Dimension, c.Metric))
	return err
}

func (t *tx) rmCollection(name string) error {
	if _, err := t.collection(name); err != nil {
		return err
	}
	if _, err := t.exec("drop table " + vectors(name)); err != nil {
		return err
	}
	if _, err := t.exec("drop table " + items(name)); err != nil {
		return err
	}
	_, err := t.exec("delete from "+metaCollections+" where name = ?", name)
	return err
}

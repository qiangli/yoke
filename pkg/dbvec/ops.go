package dbvec

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/qiangli/yoke/pkg/dbdoc"
	"github.com/qiangli/yoke/pkg/dblifecycle"
	"github.com/qiangli/yoke/pkg/dbstore"
	"github.com/qiangli/yoke/pkg/dbstore/vecdb"
)

func fixed(a dblifecycle.Access) func(Input) dblifecycle.Access {
	return func(Input) dblifecycle.Access { return a }
}

func init() {
	read, write, excl := fixed(dblifecycle.AccessRead), fixed(dblifecycle.AccessWrite), fixed(dblifecycle.AccessExclusive)
	register(OpPut, write, runPut)
	register(OpGet, read, runGet)
	register(OpRm, write, runRm)
	register(OpBatch, write, runBatch)
	register(OpSearch, read, runSearch)
	register(OpCollectionAdd, excl, runCollectionAdd)
	register(OpCollectionRm, excl, runCollectionRm)
	register(OpCollectionList, read, runCollectionList)
	register(OpExport, read, runExport)
	register(OpImport, func(in Input) dblifecycle.Access {
		if in.Replace {
			return dblifecycle.AccessExclusive
		}
		return dblifecycle.AccessWrite
	}, runImport)
}

func register(name string, access func(Input) dblifecycle.Access, run func(context.Context, dbstore.Handle, Input) (dblifecycle.Outcome, error)) {
	dblifecycle.RegisterCategoryOp(name, dblifecycle.CategoryOp{
		Access: func(raw json.RawMessage) (dblifecycle.Access, error) {
			in, err := DecodeInput(raw)
			if err != nil {
				return 0, err
			}
			// The filter is compiled before admission, so a dry run and the
			// owner both refuse a malformed one without touching the target.
			if _, _, err := dbdoc.CompileFilter(in.Filter, in.Params); err != nil {
				return 0, err
			}
			return access(in), nil
		},
		Spec: func(mode dbstore.Mode, a dblifecycle.Access) dblifecycle.Operation { return Spec(name, mode, a) },
		Run: func(ctx context.Context, h dbstore.Handle, raw json.RawMessage, a dblifecycle.Access) (dblifecycle.Outcome, error) {
			in, err := DecodeInput(raw)
			if err != nil {
				return dblifecycle.Outcome{}, err
			}
			if a != access(in) {
				return dblifecycle.Outcome{}, fmt.Errorf("%w: %s admitted with the wrong access", dbstore.ErrInvalidInput, name)
			}
			return run(ctx, h, in)
		},
	})
}

// Spec is the declaration shared by CLI, --explain, MCP and owner IPC.
func Spec(name string, mode dbstore.Mode, a dblifecycle.Access) dblifecycle.Operation {
	s := dblifecycle.Operation{Name: name, MutationClass: "read", Hold: "none", Atomic: "op", DryRun: "local-preview", Modes: []string{"memory", "persistent"}}
	fs := func(tokens ...string) []string {
		if mode == dbstore.Persistent {
			return tokens
		}
		var out []string
		for _, t := range tokens {
			if !strings.HasPrefix(t, "fs.") {
				out = append(out, t)
			}
		}
		return out
	}
	switch a {
	case dblifecycle.AccessRead:
		s.Effects = fs("fs.read", "db.read")
	case dblifecycle.AccessWrite:
		s.MutationClass, s.Hold = "write", "shared-writer"
		s.Effects = fs("fs.write", "db.write")
	case dblifecycle.AccessExclusive:
		s.MutationClass, s.Hold = "destructive", "exclusive"
		s.Effects = fs("fs.write", "db.write", "db.delete")
	}
	switch name {
	case OpExport:
		s.Effects = append(fs("fs.read", "db.read"), "fs.write")
	case OpImport:
		s.Effects = append([]string{"fs.read"}, s.Effects...)
	}
	return s
}

// cursor is the shared bashy-cursor-v1 envelope, bound to the target and
// the query. Its token is the last hit's exact distance bits and id.
type cursor struct {
	SchemaVersion string        `json:"schema_version"`
	Kind          string        `json:"kind"`
	Token         string        `json:"token"`
	Binding       cursorBinding `json:"binding"`
}

type cursorBinding struct {
	Provider string `json:"provider"`
	Target   string `json:"target"`
	Query    string `json:"query"`
}

type page struct {
	Returned  int     `json:"returned"`
	Limit     int     `json:"limit"`
	Truncated bool    `json:"truncated"`
	Bytes     int     `json:"bytes"`
	Cursor    *cursor `json:"cursor"`
}

const cursorWarning = "a cursor resumes after the last returned hit; pages are separate reads, not one snapshot"

func binding(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// position is where a page ends: a distance (search) and an id.
type position struct {
	dist float64
	id   string
}

func (p position) token() string {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], math.Float64bits(p.dist))
	return "d:" + hex.EncodeToString(b[:]) + ":" + hex.EncodeToString([]byte(p.id))
}

func resumeAfter(since, target, query string) (position, bool, error) {
	var p position
	if since == "" {
		return p, false, nil
	}
	var c cursor
	dec := json.NewDecoder(strings.NewReader(since))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil || c.SchemaVersion != "bashy-cursor-v1" || c.Kind != "pagination" {
		return p, false, fmt.Errorf("%w: --since wants a bashy-cursor-v1 pagination envelope", dbstore.ErrInvalidInput)
	}
	if c.Binding.Provider != "db" || c.Binding.Target != target || c.Binding.Query != query {
		return p, false, fmt.Errorf("%w: cursor is bound to another target or query; restart without --since", dbstore.ErrInvalidInput)
	}
	parts := strings.Split(c.Token, ":")
	if len(parts) != 3 || parts[0] != "d" {
		return p, false, fmt.Errorf("%w: cursor token", dbstore.ErrInvalidInput)
	}
	bits, err1 := hex.DecodeString(parts[1])
	id, err2 := hex.DecodeString(parts[2])
	if err1 != nil || err2 != nil || len(bits) != 8 {
		return p, false, fmt.Errorf("%w: cursor token", dbstore.ErrInvalidInput)
	}
	p.dist, p.id = math.Float64frombits(binary.BigEndian.Uint64(bits)), string(id)
	return p, true, nil
}

func newPage(target, query string, last position, returned, limit, bytes int, truncated bool) page {
	p := page{Returned: returned, Limit: limit, Truncated: truncated, Bytes: bytes}
	if truncated {
		p.Cursor = &cursor{SchemaVersion: "bashy-cursor-v1", Kind: "pagination", Token: last.token(),
			Binding: cursorBinding{Provider: "db", Target: target, Query: query}}
	}
	return p
}

func canonicalRaw(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	if b, err := dbdoc.CanonicalObject("value", raw); err == nil {
		return string(b)
	}
	return string(raw)
}

// Item is one stored vector as every read returns it.
type Item struct {
	ID       string          `json:"id"`
	Revision int64           `json:"revision"`
	Vector   *Vector         `json:"vector,omitempty"`
	Metadata json.RawMessage `json:"metadata"`
}

// Hit is one search result, nearest first.
type Hit struct {
	ID       string          `json:"id"`
	Revision int64           `json:"revision"`
	Distance float64         `json:"distance"`
	Vector   *Vector         `json:"vector,omitempty"`
	Metadata json.RawMessage `json:"metadata"`
}

func (t *tx) revision(collection, id string) (int64, bool, error) {
	var rev int64
	err := t.conn.QueryRowContext(t.ctx, "select revision from "+items(collection)+" where id = ?", id).Scan(&rev)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return rev, err == nil, t.classify(err)
}

func (t *tx) get(collection, id string) (*Item, error) {
	it := Item{ID: id}
	var doc string
	var blob []byte
	err := t.conn.QueryRowContext(t.ctx, "select m.revision, m.doc, v.emb from "+items(collection)+" m join "+vectors(collection)+" v on v.id = m.id where m.id = ?", id).Scan(&it.Revision, &doc, &blob)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, t.classify(err)
	}
	v, err := vecdb.ParseF32(blob)
	if err != nil {
		return nil, fmt.Errorf("%w: stored vector %q: %v", dbstore.ErrIntegrity, id, err)
	}
	ev := encodeVector(v)
	it.Vector, it.Metadata = &ev, json.RawMessage(doc)
	return &it, nil
}

func runGet(ctx context.Context, h dbstore.Handle, in Input) (dblifecycle.Outcome, error) {
	if err := in.only(OpGet, "collection", "id", "max_bytes"); err != nil {
		return dblifecycle.Outcome{}, err
	}
	_, maxBytes, err := in.bounds()
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	if err := checkID(in.ID); err != nil {
		return dblifecycle.Outcome{}, err
	}
	t, err := begin(ctx, h, false)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	defer t.end()
	if _, err := t.collection(in.Collection); err != nil {
		return dblifecycle.Outcome{}, err
	}
	it, err := t.get(in.Collection, in.ID)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	if it == nil {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: vector %q in collection %q", dbstore.ErrNotFound, in.ID, in.Collection)
	}
	if size := len(it.Metadata) + len(it.Vector.B64) + len(it.ID); size > maxBytes {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: vector %q is %d bytes, over --max-bytes %d", dbstore.ErrLimitExceeded, in.ID, size, maxBytes)
	}
	return dblifecycle.Outcome{Result: it}, nil
}

// write is one put/rm inside an open write transaction. Any failed
// precondition aborts the whole operation.
func (t *tx) write(c Collection, op BatchOp) (map[string]any, error) {
	if op.IfAbsent && op.IfRevision != nil {
		return nil, fmt.Errorf("%w: if_absent and if_revision are exclusive", dbstore.ErrInvalidInput)
	}
	if op.IfRevision != nil && *op.IfRevision < 1 {
		return nil, fmt.Errorf("%w: if_revision starts at 1", dbstore.ErrInvalidInput)
	}
	var vec []float32
	var doc string
	var err error
	switch op.Op {
	case "put":
		if err := checkModelMatch(op.Model, c); err != nil {
			return nil, err
		}
		if vec, err = parseVector(op.Vector, c); err != nil {
			return nil, err
		}
		if doc, err = metadata(op.Metadata); err != nil {
			return nil, err
		}
		if op.ID == "" {
			if op.IfRevision != nil {
				return nil, fmt.Errorf("%w: if_revision needs an id", dbstore.ErrInvalidInput)
			}
			if op.ID, err = newID(); err != nil {
				return nil, err
			}
		}
	case "rm":
		if op.IfAbsent || len(op.Vector) != 0 || len(op.Metadata) != 0 || op.Model != "" {
			return nil, fmt.Errorf("%w: rm takes id and optional if_revision", dbstore.ErrInvalidInput)
		}
	default:
		return nil, fmt.Errorf("%w: batch op %q (want put or rm)", dbstore.ErrInvalidInput, op.Op)
	}
	if err := checkID(op.ID); err != nil {
		return nil, err
	}
	cur, ok, err := t.revision(c.Name, op.ID)
	if err != nil {
		return nil, err
	}
	if !ok && op.Op == "rm" {
		return nil, fmt.Errorf("%w: vector %q in collection %q", dbstore.ErrNotFound, op.ID, c.Name)
	}
	if ok && op.IfAbsent {
		return nil, fmt.Errorf("%w: vector %q exists at revision %d (if_absent)", dbstore.ErrExists, op.ID, cur)
	}
	if op.IfRevision != nil && (!ok || cur != *op.IfRevision) {
		actual := "absent"
		if ok {
			actual = strconv.FormatInt(cur, 10)
		}
		return nil, fmt.Errorf("%w: revision mismatch on vector %q: expected_revision %d, actual_revision %s", dbstore.ErrConflict, op.ID, *op.IfRevision, actual)
	}
	if op.Op == "rm" {
		if err := t.remove(c.Name, op.ID); err != nil {
			return nil, err
		}
		return map[string]any{"id": op.ID, "deleted": true, "revision": cur}, nil
	}
	if !ok {
		if err := t.insert(c.Name, op.ID, 1, vec, doc); err != nil {
			return nil, err
		}
		return map[string]any{"id": op.ID, "revision": int64(1), "created": true}, nil
	}
	if cur == math.MaxInt64 {
		return nil, fmt.Errorf("%w: vector %q has no revision after %d", dbstore.ErrLimitExceeded, op.ID, cur)
	}
	if err := t.update(c.Name, op.ID, cur+1, vec, doc); err != nil {
		return nil, err
	}
	return map[string]any{"id": op.ID, "revision": cur + 1, "created": false}, nil
}

func (t *tx) insert(collection, id string, rev int64, vec []float32, doc string) error {
	if _, err := t.exec("insert into "+items(collection)+" (id, revision, doc) values (?, ?, ?)", id, rev, doc); err != nil {
		return err
	}
	_, err := t.exec("insert into "+vectors(collection)+" (id, emb) values (?, ?)", id, vecdb.F32(vec))
	return err
}

func (t *tx) update(collection, id string, rev int64, vec []float32, doc string) error {
	if _, err := t.exec("update "+items(collection)+" set revision = ?, doc = ? where id = ?", rev, doc, id); err != nil {
		return err
	}
	_, err := t.exec("update "+vectors(collection)+" set emb = ? where id = ?", vecdb.F32(vec), id)
	return err
}

func (t *tx) remove(collection, id string) error {
	if _, err := t.exec("delete from "+vectors(collection)+" where id = ?", id); err != nil {
		return err
	}
	_, err := t.exec("delete from "+items(collection)+" where id = ?", id)
	return err
}

// writeOps runs writes as one atomic transaction: all apply or none do.
func writeOps(ctx context.Context, h dbstore.Handle, collection string, ops []BatchOp) ([]map[string]any, error) {
	t, err := begin(ctx, h, true)
	if err != nil {
		return nil, err
	}
	defer t.end()
	c, err := t.collection(collection)
	if err != nil {
		return nil, err
	}
	results := make([]map[string]any, 0, len(ops))
	for i, op := range ops {
		if i%64 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, dbstore.FromContext(err)
			}
		}
		r, err := t.write(c, op)
		if err != nil {
			if len(ops) > 1 {
				err = fmt.Errorf("op %d: %w", i+1, err)
			}
			return nil, fmt.Errorf("%w (rolled back; nothing was applied)", err)
		}
		results = append(results, r)
	}
	if err := t.commit(); err != nil {
		return nil, err
	}
	return results, nil
}

func runPut(ctx context.Context, h dbstore.Handle, in Input) (dblifecycle.Outcome, error) {
	if err := in.only(OpPut, "collection", "id", "vector", "text", "endpoint", "api_key_env", "metadata", "model", "if_revision", "if_absent"); err != nil {
		return dblifecycle.Outcome{}, err
	}
	if err := embedInput(ctx, h, &in); err != nil {
		return dblifecycle.Outcome{}, err
	}
	res, err := writeOps(ctx, h, in.Collection, []BatchOp{{Op: "put", ID: in.ID, Vector: in.Vector, Metadata: in.Metadata, Model: in.Model, IfRevision: in.IfRevision, IfAbsent: in.IfAbsent}})
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	return dblifecycle.Outcome{Result: res[0]}, nil
}

func runRm(ctx context.Context, h dbstore.Handle, in Input) (dblifecycle.Outcome, error) {
	if err := in.only(OpRm, "collection", "id", "if_revision"); err != nil {
		return dblifecycle.Outcome{}, err
	}
	res, err := writeOps(ctx, h, in.Collection, []BatchOp{{Op: "rm", ID: in.ID, IfRevision: in.IfRevision}})
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	return dblifecycle.Outcome{Result: res[0]}, nil
}

func runBatch(ctx context.Context, h dbstore.Handle, in Input) (dblifecycle.Outcome, error) {
	if err := in.only(OpBatch, "collection", "ops"); err != nil {
		return dblifecycle.Outcome{}, err
	}
	if len(in.Ops) == 0 || len(in.Ops) > MaxBatch {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: batch takes 1..%d ops", dbstore.ErrInvalidInput, MaxBatch)
	}
	res, err := writeOps(ctx, h, in.Collection, in.Ops)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	return dblifecycle.Outcome{Result: map[string]any{"committed": true, "collection": in.Collection, "results": res}}, nil
}

// runSearch ranks the items a metadata filter admits by exact distance to
// the query vector, nearest first and then by id, so equal distances order
// deterministically and a cursor resumes exactly after the last hit.
func runSearch(ctx context.Context, h dbstore.Handle, in Input) (dblifecycle.Outcome, error) {
	if err := in.only(OpSearch, "collection", "vector", "text", "endpoint", "api_key_env", "model", "filter", "params", "include_vectors", "limit", "max_bytes", "since"); err != nil {
		return dblifecycle.Outcome{}, err
	}
	if err := embedInput(ctx, h, &in); err != nil {
		return dblifecycle.Outcome{}, err
	}
	limit, maxBytes, err := in.bounds()
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	where, args, err := dbdoc.CompileFilter(in.Filter, in.Params)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	t, err := begin(ctx, h, false)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	defer t.end()
	c, err := t.collection(in.Collection)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	if err := checkModelMatch(in.Model, c); err != nil {
		return dblifecycle.Outcome{}, err
	}
	q, err := parseVector(in.Vector, c)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	target := h.Identity().ID
	query := binding(OpSearch, in.Collection, hex.EncodeToString(vecdb.F32(q)), canonicalRaw(in.Filter), canonicalRaw(in.Params), strconv.FormatBool(in.IncludeVectors))
	after, resumed, err := resumeAfter(in.Since, target, query)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	// The metric is a validated enum, so the function name is fixed text;
	// the filter reads the metadata column through its alias doc.
	inner := "select m.id as id, m.revision as revision, m.doc as doc, v.emb as emb, vec_distance_" + c.Metric + "(v.emb, ?) as dist from " +
		items(c.Name) + " m join " + vectors(c.Name) + " v on v.id = m.id where " + where
	all := append([]any{vecdb.F32(q)}, args...)
	stmt := "select id, revision, doc, emb, dist from (" + inner + ")"
	if resumed {
		stmt += " where dist > ? or (dist = ? and id > ?)"
		all = append(all, after.dist, after.dist, after.id)
	}
	stmt += " order by dist, id limit ?"
	all = append(all, limit+1)
	rows, err := t.conn.QueryContext(ctx, stmt, all...)
	if err != nil {
		return dblifecycle.Outcome{}, t.classify(err)
	}
	defer rows.Close()
	hits := []Hit{}
	used, truncated := 0, false
	for rows.Next() {
		if len(hits) == limit {
			truncated = true
			break
		}
		var hit Hit
		var doc string
		var blob []byte
		var dist sql.NullFloat64
		if err := rows.Scan(&hit.ID, &hit.Revision, &doc, &blob, &dist); err != nil {
			return dblifecycle.Outcome{}, t.classify(err)
		}
		if !dist.Valid || math.IsNaN(dist.Float64) {
			return dblifecycle.Outcome{}, fmt.Errorf("%w: vector %q has no %s distance", dbstore.ErrIntegrity, hit.ID, c.Metric)
		}
		hit.Distance, hit.Metadata = dist.Float64, json.RawMessage(doc)
		size := len(doc) + len(hit.ID) + 48
		if in.IncludeVectors {
			v, err := vecdb.ParseF32(blob)
			if err != nil {
				return dblifecycle.Outcome{}, fmt.Errorf("%w: stored vector %q: %v", dbstore.ErrIntegrity, hit.ID, err)
			}
			ev := encodeVector(v)
			hit.Vector = &ev
			size += len(ev.B64)
		}
		if used+size > maxBytes {
			if len(hits) == 0 {
				return dblifecycle.Outcome{}, fmt.Errorf("%w: hit %q is %d bytes, over --max-bytes %d", dbstore.ErrLimitExceeded, hit.ID, size, maxBytes)
			}
			truncated = true
			break
		}
		hits = append(hits, hit)
		used += size
	}
	if err := rows.Err(); err != nil {
		return dblifecycle.Outcome{}, t.classify(err)
	}
	var last position
	if len(hits) > 0 {
		last = position{dist: hits[len(hits)-1].Distance, id: hits[len(hits)-1].ID}
	}
	res := dblifecycle.Outcome{Result: map[string]any{"collection": c.Name, "metric": c.Metric, "model": c.Model, "exact": true, "hits": hits},
		Page: newPage(target, query, last, len(hits), limit, used, truncated)}
	if truncated {
		res.Warnings = []string{cursorWarning}
	}
	return res, nil
}

// schemaOp runs one exclusive schema change as one transaction.
func schemaOp(ctx context.Context, h dbstore.Handle, fn func(*tx) error) error {
	t, err := begin(ctx, h, true)
	if err != nil {
		return err
	}
	defer t.end()
	if err := fn(t); err != nil {
		return err
	}
	return t.commit()
}

func runCollectionAdd(ctx context.Context, h dbstore.Handle, in Input) (dblifecycle.Outcome, error) {
	if err := in.only(OpCollectionAdd, "collection", "dimension", "metric", "model"); err != nil {
		return dblifecycle.Outcome{}, err
	}
	c := Collection{Name: in.Collection, Dimension: in.Dimension, Metric: in.Metric, Model: in.Model, DType: "f32"}
	if err := c.validate(); err != nil {
		return dblifecycle.Outcome{}, err
	}
	err := schemaOp(ctx, h, func(t *tx) error { return t.addCollection(c) })
	return dblifecycle.Outcome{Result: map[string]any{"collection": c, "created": true}}, err
}

func runCollectionRm(ctx context.Context, h dbstore.Handle, in Input) (dblifecycle.Outcome, error) {
	if err := in.only(OpCollectionRm, "collection"); err != nil {
		return dblifecycle.Outcome{}, err
	}
	var n int64
	err := schemaOp(ctx, h, func(t *tx) error {
		if _, err := t.collection(in.Collection); err != nil {
			return err
		}
		if err := t.conn.QueryRowContext(ctx, "select count(*) from "+items(in.Collection)).Scan(&n); err != nil {
			return t.classify(err)
		}
		return t.rmCollection(in.Collection)
	})
	return dblifecycle.Outcome{Result: map[string]any{"collection": in.Collection, "dropped": true, "vectors": n}}, err
}

func runCollectionList(ctx context.Context, h dbstore.Handle, in Input) (dblifecycle.Outcome, error) {
	if err := in.only(OpCollectionList, "limit", "since"); err != nil {
		return dblifecycle.Outcome{}, err
	}
	limit, _, err := in.bounds()
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	target, query := h.Identity().ID, binding(OpCollectionList)
	after, _, err := resumeAfter(in.Since, target, query)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	t, err := begin(ctx, h, false)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	defer t.end()
	cols, err := t.collections()
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	type entry struct {
		Collection
		Vectors int64 `json:"vectors"`
	}
	out := []entry{}
	truncated := false
	for _, c := range cols {
		if c.Name <= after.id {
			continue
		}
		if len(out) == limit {
			truncated = true
			break
		}
		e := entry{Collection: c}
		if err := t.conn.QueryRowContext(ctx, "select count(*) from "+items(c.Name)).Scan(&e.Vectors); err != nil {
			return dblifecycle.Outcome{}, t.classify(err)
		}
		out = append(out, e)
	}
	var last position
	if len(out) > 0 {
		last.id = out[len(out)-1].Name
	}
	return dblifecycle.Outcome{Result: map[string]any{"collections": out}, Page: newPage(target, query, last, len(out), limit, 0, truncated)}, nil
}

// Bounds of the explicit embedding request.
const (
	MaxTextBytes     = 64 << 10
	maxEmbedResponse = 4 << 20
	embedTimeout     = 60 * time.Second
)

var envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)

// embedInput turns text into the operation's vector. It runs inside the
// owner's Run, before any write transaction is open: the collection's model
// and dimension are read in a short read transaction, the endpoint is called
// with no database lock held, and the write re-checks the model, so a
// collection replaced meanwhile refuses the vector. Supplied vectors return
// at once — no network.
func embedInput(ctx context.Context, h dbstore.Handle, in *Input) error {
	if in.Text == "" {
		if in.Endpoint != "" || in.APIKeyEnv != "" {
			return fmt.Errorf("%w: --endpoint and --api-key-env embed --text; a supplied vector takes neither", dbstore.ErrInvalidInput)
		}
		return nil
	}
	if len(in.Vector) != 0 {
		return fmt.Errorf("%w: give a vector or text, not both", dbstore.ErrInvalidInput)
	}
	if in.Endpoint == "" {
		return fmt.Errorf("%w: text needs an explicit --endpoint (OpenAI-compatible base URL); there is no default provider", dbstore.ErrInvalidInput)
	}
	if len(in.Text) > MaxTextBytes || !utf8.ValidString(in.Text) {
		return fmt.Errorf("%w: text must be 1..%d bytes of UTF-8", dbstore.ErrLimitExceeded, MaxTextBytes)
	}
	u, err := embedURL(in.Endpoint)
	if err != nil {
		return err
	}
	key := ""
	if in.APIKeyEnv != "" {
		if !envNameRE.MatchString(in.APIKeyEnv) {
			return fmt.Errorf("%w: --api-key-env names an environment variable", dbstore.ErrInvalidInput)
		}
		if key = os.Getenv(in.APIKeyEnv); key == "" {
			return fmt.Errorf("%w: --api-key-env %s is unset or empty in the process running the operation", dbstore.ErrInvalidInput, in.APIKeyEnv)
		}
	}
	t, err := begin(ctx, h, false)
	if err != nil {
		return err
	}
	c, err := t.collection(in.Collection)
	t.end()
	if err != nil {
		return err
	}
	if err := checkModelMatch(in.Model, c); err != nil {
		return err
	}
	v, err := embed(ctx, u, key, c, in.Text)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(encodeVector(v))
	if err != nil {
		return err
	}
	in.Vector, in.Model, in.Text, in.Endpoint, in.APIKeyEnv = raw, c.Model, "", "", ""
	return nil
}

// embedURL validates an explicit OpenAI-compatible base URL (…/v1) and
// returns its embeddings URL. Credentials never travel in the URL.
func embedURL(endpoint string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Opaque != "" {
		return "", fmt.Errorf("%w: --endpoint wants an http(s) base URL such as http://127.0.0.1:11434/v1", dbstore.ErrInvalidInput)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("%w: --endpoint takes no userinfo, query or fragment (pass a key with --api-key-env)", dbstore.ErrInvalidInput)
	}
	return strings.TrimSuffix(u.String(), "/") + "/embeddings", nil
}

// embedClient refuses every redirect, so a bearer key is only ever sent to
// the endpoint the caller named.
var embedClient = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// embed posts one OpenAI /v1/embeddings request for the collection's model
// and returns the embedding, validated against the collection's dimension
// and metric exactly as a supplied vector is.
func embed(ctx context.Context, endpoint, key string, c Collection, text string) ([]float32, error) {
	ctx, cancel := context.WithTimeout(ctx, embedTimeout)
	defer cancel()
	body, err := json.Marshal(map[string]any{"model": c.Model, "input": text, "encoding_format": "float"})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%w: --endpoint: %v", dbstore.ErrInvalidInput, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := embedClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, dbstore.FromContext(ctx.Err())
		}
		return nil, fmt.Errorf("%w: embedding endpoint %s: %v", dbstore.ErrBackend, endpoint, errors.Unwrap(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: embedding endpoint %s answered %s (redirects are not followed)", dbstore.ErrBackend, endpoint, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxEmbedResponse+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, dbstore.FromContext(ctx.Err())
		}
		return nil, fmt.Errorf("%w: embedding endpoint %s: %v", dbstore.ErrBackend, endpoint, err)
	}
	if len(b) > maxEmbedResponse {
		return nil, fmt.Errorf("%w: embedding response is over %d bytes", dbstore.ErrLimitExceeded, maxEmbedResponse)
	}
	var out struct {
		Data []struct {
			Embedding json.RawMessage `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &out); err != nil || len(out.Data) != 1 || len(out.Data[0].Embedding) == 0 {
		return nil, fmt.Errorf("%w: embedding endpoint %s did not return one data[].embedding", dbstore.ErrBackend, endpoint)
	}
	if out.Data[0].Embedding[0] != '[' {
		return nil, fmt.Errorf("%w: embedding endpoint %s returned a non-float embedding", dbstore.ErrBackend, endpoint)
	}
	v, err := parseVector(out.Data[0].Embedding, c)
	if err != nil {
		return nil, fmt.Errorf("embedding from %s for model %q: %w", endpoint, c.Model, err)
	}
	return v, nil
}

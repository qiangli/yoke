package dbdoc

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/qiangli/yoke/pkg/dblifecycle"
	"github.com/qiangli/yoke/pkg/dbstore"
)

func fixed(a dblifecycle.Access) func(Input) dblifecycle.Access {
	return func(Input) dblifecycle.Access { return a }
}

func init() {
	read, write, excl := fixed(dblifecycle.AccessRead), fixed(dblifecycle.AccessWrite), fixed(dblifecycle.AccessExclusive)
	register(OpGet, read, runGet)
	register(OpFind, read, runFind)
	register(OpPut, write, runPut)
	register(OpPatch, write, runPatch)
	register(OpRm, write, runRm)
	register(OpBatch, write, runBatch)
	register(OpCollectionCreate, excl, runCollectionCreate)
	register(OpCollectionDrop, excl, runCollectionDrop)
	register(OpCollectionList, read, runCollectionList)
	register(OpIndexAdd, excl, runIndexAdd)
	register(OpIndexRm, excl, runIndexRm)
	register(OpIndexList, read, runIndexList)
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
			if _, _, err := compileFilter(in.Filter, in.Params); err != nil {
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

// cursor is the shared bashy-cursor-v1 envelope. The token is the last key
// returned (keyset pagination), bound to the target and the query.
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

const cursorWarning = "a cursor resumes after the last returned id; pages are separate reads, not one snapshot"

func binding(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func canonicalRaw(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	v, err := decodeJSON(raw)
	if err != nil {
		return string(raw)
	}
	b, _ := encodeJSON(v)
	return string(b)
}

// resumeAfter returns the key a cursor resumes after ("" for a first page).
func resumeAfter(since, target, query string) (string, bool, error) {
	if since == "" {
		return "", false, nil
	}
	var c cursor
	dec := json.NewDecoder(strings.NewReader(since))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil || c.SchemaVersion != "bashy-cursor-v1" || c.Kind != "pagination" {
		return "", false, fmt.Errorf("%w: --since wants a bashy-cursor-v1 pagination envelope", dbstore.ErrInvalidInput)
	}
	if c.Binding.Provider != "db" || c.Binding.Target != target || c.Binding.Query != query {
		return "", false, fmt.Errorf("%w: cursor is bound to another target or query; restart without --since", dbstore.ErrInvalidInput)
	}
	hexKey, ok := strings.CutPrefix(c.Token, "k:")
	key, err := hex.DecodeString(hexKey)
	if !ok || err != nil {
		return "", false, fmt.Errorf("%w: cursor token", dbstore.ErrInvalidInput)
	}
	return string(key), true, nil
}

func newPage(target, query, last string, returned, limit, bytes int, truncated bool) page {
	p := page{Returned: returned, Limit: limit, Truncated: truncated, Bytes: bytes}
	if truncated {
		p.Cursor = &cursor{SchemaVersion: "bashy-cursor-v1", Kind: "pagination", Token: "k:" + hex.EncodeToString([]byte(last)),
			Binding: cursorBinding{Provider: "db", Target: target, Query: query}}
	}
	return p
}

// Document is one stored document as every read returns it.
type Document struct {
	ID       string          `json:"id"`
	Revision int64           `json:"revision"`
	Doc      json.RawMessage `json:"doc"`
}

func (t *tx) get(collection, id string) (*Document, error) {
	d := Document{ID: id}
	var doc string
	err := t.conn.QueryRowContext(t.ctx, "select revision, doc from "+table(collection)+" where id = ?", id).Scan(&d.Revision, &doc)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, t.classify(err)
	}
	d.Doc = json.RawMessage(doc)
	return &d, nil
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
	if err := t.collection(in.Collection); err != nil {
		return dblifecycle.Outcome{}, err
	}
	d, err := t.get(in.Collection, in.ID)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	if d == nil {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: document %q in collection %q", dbstore.ErrNotFound, in.ID, in.Collection)
	}
	if len(d.Doc) > maxBytes {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: document %q is %d bytes, over --max-bytes %d", dbstore.ErrLimitExceeded, in.ID, len(d.Doc), maxBytes)
	}
	return dblifecycle.Outcome{Result: d}, nil
}

func runFind(ctx context.Context, h dbstore.Handle, in Input) (dblifecycle.Outcome, error) {
	if err := in.only(OpFind, "collection", "filter", "params", "limit", "max_bytes", "since"); err != nil {
		return dblifecycle.Outcome{}, err
	}
	limit, maxBytes, err := in.bounds()
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	where, args, err := compileFilter(in.Filter, in.Params)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	target := h.Identity().ID
	query := binding(OpFind, in.Collection, canonicalRaw(in.Filter), canonicalRaw(in.Params))
	after, resumed, err := resumeAfter(in.Since, target, query)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	t, err := begin(ctx, h, false)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	defer t.end()
	if err := t.collection(in.Collection); err != nil {
		return dblifecycle.Outcome{}, err
	}
	q := "select id, revision, doc from " + table(in.Collection) + " where " + where
	if resumed {
		q += " and id > ?"
		args = append(args, after)
	}
	q += " order by id limit ?"
	args = append(args, limit+1)
	rows, err := t.conn.QueryContext(ctx, q, args...)
	if err != nil {
		return dblifecycle.Outcome{}, t.classify(err)
	}
	defer rows.Close()
	out := []Document{}
	used, truncated := 0, false
	for rows.Next() {
		if len(out) == limit {
			truncated = true
			break
		}
		var d Document
		var doc string
		if err := rows.Scan(&d.ID, &d.Revision, &doc); err != nil {
			return dblifecycle.Outcome{}, t.classify(err)
		}
		d.Doc = json.RawMessage(doc)
		size := len(doc) + len(d.ID) + 32
		if used+size > maxBytes {
			if len(out) == 0 {
				return dblifecycle.Outcome{}, fmt.Errorf("%w: document %q is %d bytes, over --max-bytes %d", dbstore.ErrLimitExceeded, d.ID, size, maxBytes)
			}
			truncated = true
			break
		}
		out = append(out, d)
		used += size
	}
	if err := rows.Err(); err != nil {
		return dblifecycle.Outcome{}, t.classify(err)
	}
	last := ""
	if len(out) > 0 {
		last = out[len(out)-1].ID
	}
	res := dblifecycle.Outcome{Result: map[string]any{"collection": in.Collection, "documents": out},
		Page: newPage(target, query, last, len(out), limit, used, truncated)}
	if truncated {
		res.Warnings = []string{cursorWarning}
	}
	return res, nil
}

// write is one put/patch/rm inside an open write transaction. It returns the
// per-item result; any failed precondition aborts the whole operation.
func (t *tx) write(collection string, op BatchOp) (map[string]any, error) {
	if op.IfAbsent && op.IfRevision != nil {
		return nil, fmt.Errorf("%w: if_absent and if_revision are exclusive", dbstore.ErrInvalidInput)
	}
	if op.IfRevision != nil && *op.IfRevision < 1 {
		return nil, fmt.Errorf("%w: if_revision starts at 1", dbstore.ErrInvalidInput)
	}
	switch op.Op {
	case "put":
		if len(op.Patch) != 0 {
			return nil, fmt.Errorf("%w: put takes doc, not patch", dbstore.ErrInvalidInput)
		}
		if op.ID == "" {
			if op.IfRevision != nil {
				return nil, fmt.Errorf("%w: if_revision needs an id", dbstore.ErrInvalidInput)
			}
			id, err := newID()
			if err != nil {
				return nil, err
			}
			op.ID = id
		}
	case "patch":
		if op.IfAbsent || len(op.Doc) != 0 || len(op.Patch) == 0 {
			return nil, fmt.Errorf("%w: patch takes id, patch and optional if_revision", dbstore.ErrInvalidInput)
		}
	case "rm":
		if op.IfAbsent || len(op.Doc) != 0 || len(op.Patch) != 0 {
			return nil, fmt.Errorf("%w: rm takes id and optional if_revision", dbstore.ErrInvalidInput)
		}
	default:
		return nil, fmt.Errorf("%w: batch op %q (want put, patch or rm)", dbstore.ErrInvalidInput, op.Op)
	}
	if err := checkID(op.ID); err != nil {
		return nil, err
	}
	cur, err := t.get(collection, op.ID)
	if err != nil {
		return nil, err
	}
	if cur == nil && op.Op != "put" {
		return nil, fmt.Errorf("%w: document %q in collection %q", dbstore.ErrNotFound, op.ID, collection)
	}
	if cur != nil && op.IfAbsent {
		return nil, fmt.Errorf("%w: document %q exists at revision %d (if_absent)", dbstore.ErrExists, op.ID, cur.Revision)
	}
	if op.IfRevision != nil && (cur == nil || cur.Revision != *op.IfRevision) {
		actual := "absent"
		if cur != nil {
			actual = strconv.FormatInt(cur.Revision, 10)
		}
		return nil, fmt.Errorf("%w: revision mismatch on document %q: expected_revision %d, actual_revision %s", dbstore.ErrConflict, op.ID, *op.IfRevision, actual)
	}
	if op.Op == "rm" {
		if _, err := t.exec("delete from "+table(collection)+" where id = ?", op.ID); err != nil {
			return nil, err
		}
		return map[string]any{"id": op.ID, "deleted": true, "revision": cur.Revision}, nil
	}
	var doc any
	if op.Op == "put" {
		if doc, err = object("doc", op.Doc); err != nil {
			return nil, err
		}
	} else {
		patch, err := object("patch", op.Patch)
		if err != nil {
			return nil, err
		}
		base, err := decodeJSON(cur.Doc)
		if err != nil {
			return nil, fmt.Errorf("%w: stored document %q: %v", dbstore.ErrIntegrity, op.ID, err)
		}
		doc = mergePatch(base, patch)
	}
	text, err := encodeJSON(doc)
	if err != nil {
		return nil, err
	}
	if len(text) > MaxDocBytes {
		return nil, fmt.Errorf("%w: document %q would be over %d bytes", dbstore.ErrLimitExceeded, op.ID, MaxDocBytes)
	}
	if cur == nil {
		if _, err := t.exec("insert into "+table(collection)+" (id, revision, doc) values (?, 1, ?)", op.ID, string(text)); err != nil {
			return nil, uniqueConflict(err)
		}
		return map[string]any{"id": op.ID, "revision": int64(1), "created": true}, nil
	}
	if cur.Revision == math.MaxInt64 {
		return nil, fmt.Errorf("%w: document %q has no revision after %d", dbstore.ErrLimitExceeded, op.ID, cur.Revision)
	}
	if _, err := t.exec("update "+table(collection)+" set revision = ?, doc = ? where id = ?", cur.Revision+1, string(text), op.ID); err != nil {
		return nil, uniqueConflict(err)
	}
	return map[string]any{"id": op.ID, "revision": cur.Revision + 1, "created": false}, nil
}

func uniqueConflict(err error) error {
	if dbstore.Code(err) == "conflict" {
		return fmt.Errorf("%w (a unique index already holds this value)", err)
	}
	return err
}

// writeOps runs writes as one atomic transaction: all apply or none do.
func writeOps(ctx context.Context, h dbstore.Handle, collection string, ops []BatchOp) ([]map[string]any, error) {
	t, err := begin(ctx, h, true)
	if err != nil {
		return nil, err
	}
	defer t.end()
	if err := t.collection(collection); err != nil {
		return nil, err
	}
	results := make([]map[string]any, 0, len(ops))
	for i, op := range ops {
		if i%64 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, dbstore.FromContext(err)
			}
		}
		r, err := t.write(collection, op)
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

func single(op string, allowed ...string) func(context.Context, dbstore.Handle, Input) (dblifecycle.Outcome, error) {
	return func(ctx context.Context, h dbstore.Handle, in Input) (dblifecycle.Outcome, error) {
		if err := in.only("doc."+op, append([]string{"collection", "id"}, allowed...)...); err != nil {
			return dblifecycle.Outcome{}, err
		}
		res, err := writeOps(ctx, h, in.Collection, []BatchOp{{Op: op, ID: in.ID, Doc: in.Doc, Patch: in.Patch, IfRevision: in.IfRevision, IfAbsent: in.IfAbsent}})
		if err != nil {
			return dblifecycle.Outcome{}, err
		}
		return dblifecycle.Outcome{Result: res[0]}, nil
	}
}

var (
	runPut   = single("put", "doc", "if_revision", "if_absent")
	runPatch = single("patch", "patch", "if_revision")
	runRm    = single("rm", "if_revision")
)

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

func runCollectionCreate(ctx context.Context, h dbstore.Handle, in Input) (dblifecycle.Outcome, error) {
	if err := in.only(OpCollectionCreate, "collection"); err != nil {
		return dblifecycle.Outcome{}, err
	}
	err := schemaOp(ctx, h, func(t *tx) error { return t.createCollection(in.Collection) })
	return dblifecycle.Outcome{Result: map[string]any{"collection": in.Collection, "created": true}}, err
}

func runCollectionDrop(ctx context.Context, h dbstore.Handle, in Input) (dblifecycle.Outcome, error) {
	if err := in.only(OpCollectionDrop, "collection"); err != nil {
		return dblifecycle.Outcome{}, err
	}
	var n int64
	err := schemaOp(ctx, h, func(t *tx) error {
		if err := t.collection(in.Collection); err != nil {
			return err
		}
		if err := t.conn.QueryRowContext(ctx, "select count(*) from "+table(in.Collection)).Scan(&n); err != nil {
			return t.classify(err)
		}
		return t.dropCollection(in.Collection)
	})
	return dblifecycle.Outcome{Result: map[string]any{"collection": in.Collection, "dropped": true, "documents": n}}, err
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
	names, err := t.collections()
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	type entry struct {
		Name      string     `json:"name"`
		Documents int64      `json:"documents"`
		Indexes   []IndexDef `json:"indexes"`
	}
	out := []entry{}
	truncated := false
	for _, name := range names {
		if name <= after {
			continue
		}
		if len(out) == limit {
			truncated = true
			break
		}
		e := entry{Name: name}
		if err := t.conn.QueryRowContext(ctx, "select count(*) from "+table(name)).Scan(&e.Documents); err != nil {
			return dblifecycle.Outcome{}, t.classify(err)
		}
		if e.Indexes, err = t.indexes(name); err != nil {
			return dblifecycle.Outcome{}, err
		}
		out = append(out, e)
	}
	last := ""
	if len(out) > 0 {
		last = out[len(out)-1].Name
	}
	return dblifecycle.Outcome{Result: map[string]any{"collections": out}, Page: newPage(target, query, last, len(out), limit, 0, truncated)}, nil
}

func runIndexAdd(ctx context.Context, h dbstore.Handle, in Input) (dblifecycle.Outcome, error) {
	if err := in.only(OpIndexAdd, "collection", "index", "fields", "unique"); err != nil {
		return dblifecycle.Outcome{}, err
	}
	d := IndexDef{Name: in.Index, Fields: in.Fields, Unique: in.Unique}
	err := schemaOp(ctx, h, func(t *tx) error {
		if err := t.collection(in.Collection); err != nil {
			return err
		}
		return t.addIndex(in.Collection, d)
	})
	return dblifecycle.Outcome{Result: map[string]any{"collection": in.Collection, "index": d, "created": true}}, err
}

func runIndexRm(ctx context.Context, h dbstore.Handle, in Input) (dblifecycle.Outcome, error) {
	if err := in.only(OpIndexRm, "collection", "index"); err != nil {
		return dblifecycle.Outcome{}, err
	}
	err := schemaOp(ctx, h, func(t *tx) error {
		if err := t.collection(in.Collection); err != nil {
			return err
		}
		return t.rmIndex(in.Collection, in.Index)
	})
	return dblifecycle.Outcome{Result: map[string]any{"collection": in.Collection, "index": in.Index, "dropped": true}}, err
}

func runIndexList(ctx context.Context, h dbstore.Handle, in Input) (dblifecycle.Outcome, error) {
	if err := in.only(OpIndexList, "collection"); err != nil {
		return dblifecycle.Outcome{}, err
	}
	t, err := begin(ctx, h, false)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	defer t.end()
	if err := t.collection(in.Collection); err != nil {
		return dblifecycle.Outcome{}, err
	}
	defs, err := t.indexes(in.Collection)
	return dblifecycle.Outcome{Result: map[string]any{"collection": in.Collection, "indexes": defs}}, err
}

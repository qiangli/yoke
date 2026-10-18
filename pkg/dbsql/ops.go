package dbsql

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/qiangli/yoke/pkg/dblifecycle"
	"github.com/qiangli/yoke/pkg/dbstore"
)

func init() {
	register(OpQuery, func(Input) dblifecycle.Access { return dblifecycle.AccessRead }, runQuery)
	register(OpSchemaShow, func(Input) dblifecycle.Access { return dblifecycle.AccessRead }, runSchemaShow)
	register(OpExec, func(Input) dblifecycle.Access { return dblifecycle.AccessWrite }, func(ctx context.Context, h dbstore.Handle, in Input) (dblifecycle.Outcome, error) {
		return runExec(ctx, h, in, false)
	})
	register(OpSchemaApply, func(Input) dblifecycle.Access { return dblifecycle.AccessExclusive }, func(ctx context.Context, h dbstore.Handle, in Input) (dblifecycle.Outcome, error) {
		return runExec(ctx, h, in, true)
	})
	register(OpExport, func(Input) dblifecycle.Access { return dblifecycle.AccessRead }, runExport)
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
			in, err := decodeInput(raw)
			if err != nil {
				return 0, err
			}
			// Typed values are validated before admission, so a dry run
			// and the owner both refuse them without touching the target.
			if _, err := decodeParams(in.Params); err != nil {
				return 0, err
			}
			for i, st := range in.Statements {
				if _, err := decodeParams(st.Params); err != nil {
					return 0, fmt.Errorf("statement %d: %w", i+1, err)
				}
			}
			return access(in), nil
		},
		Spec: func(mode dbstore.Mode, a dblifecycle.Access) dblifecycle.Operation { return Spec(name, mode, a) },
		Run: func(ctx context.Context, h dbstore.Handle, raw json.RawMessage, a dblifecycle.Access) (dblifecycle.Outcome, error) {
			in, err := decodeInput(raw)
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

// cursor is the shared bashy-cursor-v1 envelope (common-options.md; the
// joint decision spells its discriminator schema_version). SQLite exposes
// no revision, so the token is an offset bound to target and query only.
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

func queryBinding(op string, in Input) string {
	params := []byte(in.Params)
	var v any
	if json.Unmarshal(params, &v) == nil {
		params, _ = json.Marshal(v)
	}
	sum := sha256.Sum256([]byte(op + "\x00" + in.SQL + "\x00" + string(params)))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func resumeOffset(since, target, query string) (int, error) {
	if since == "" {
		return 0, nil
	}
	var c cursor
	dec := json.NewDecoder(strings.NewReader(since))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil || c.SchemaVersion != "bashy-cursor-v1" || c.Kind != "pagination" {
		return 0, fmt.Errorf("%w: --since wants a bashy-cursor-v1 pagination envelope", dbstore.ErrInvalidInput)
	}
	if c.Binding.Provider != "db" || c.Binding.Target != target || c.Binding.Query != query {
		return 0, fmt.Errorf("%w: cursor is bound to another target or query; restart without --since", dbstore.ErrInvalidInput)
	}
	n, err := strconv.Atoi(strings.TrimPrefix(c.Token, "o:"))
	if err != nil || n < 0 || !strings.HasPrefix(c.Token, "o:") {
		return 0, fmt.Errorf("%w: cursor token", dbstore.ErrInvalidInput)
	}
	return n, nil
}

type page struct {
	Returned  int     `json:"returned"`
	Limit     int     `json:"limit"`
	Truncated bool    `json:"truncated"`
	Bytes     int     `json:"bytes"`
	Cursor    *cursor `json:"cursor"`
}

func newPage(target, query string, offset, returned, limit, bytes int, truncated bool) page {
	p := page{Returned: returned, Limit: limit, Truncated: truncated, Bytes: bytes}
	if truncated {
		p.Cursor = &cursor{SchemaVersion: "bashy-cursor-v1", Kind: "pagination", Token: "o:" + strconv.Itoa(offset+returned),
			Binding: cursorBinding{Provider: "db", Target: target, Query: query}}
	}
	return p
}

const cursorWarning = "a cursor resumes by re-running the query at the next offset; SQLite exposes no revision, so pages are not one snapshot"

type column struct {
	Name string  `json:"name"`
	Type *string `json:"type"`
}

func runQuery(ctx context.Context, h dbstore.Handle, in Input) (dblifecycle.Outcome, error) {
	db, err := handleDB(h)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	if strings.TrimSpace(in.SQL) == "" || len(in.Statements) != 0 || in.Out != "" || in.In != "" || in.OnConflict != "" || in.Replace {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: sql query takes --sql and optional params", dbstore.ErrInvalidInput)
	}
	limit, maxBytes, err := in.bounds()
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	args, err := decodeParams(in.Params)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	target, binding := h.Identity().ID, queryBinding(OpQuery, in)
	offset, err := resumeOffset(in.Since, target, binding)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	s, err := begin(ctx, db, true)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	defer s.end()
	// A read reports success only if it changed no row at all, temp tables
	// included; anything else is rolled back by end and refused here.
	changes, err := s.scalar(ctx, "select total_changes()")
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	rows, err := s.conn.QueryContext(ctx, in.SQL, args...)
	if err != nil {
		return dblifecycle.Outcome{}, s.classify(ctx, err)
	}
	defer rows.Close()
	types, err := rows.ColumnTypes()
	if err != nil {
		return dblifecycle.Outcome{}, s.classify(ctx, err)
	}
	columns := make([]column, len(types))
	for i, t := range types {
		columns[i].Name = t.Name()
		if d := t.DatabaseTypeName(); d != "" {
			columns[i].Type = &d
		}
	}
	values := make([]any, len(types))
	ptrs := make([]any, len(types))
	for i := range values {
		ptrs[i] = &values[i]
	}
	for skipped := 0; skipped < offset && rows.Next(); skipped++ {
	}
	out := [][]any{}
	used, truncated, parsedTime := 0, false, false
	for rows.Next() {
		if len(out) == limit {
			truncated = true
			break
		}
		if err := rows.Scan(ptrs...); err != nil {
			return dblifecycle.Outcome{}, s.classify(ctx, err)
		}
		row := make([]any, len(values))
		for i, v := range values {
			enc, ts, err := encodeValue(v)
			if err != nil {
				return dblifecycle.Outcome{}, fmt.Errorf("row %d column %q: %w", offset+len(out)+1, columns[i].Name, err)
			}
			row[i], parsedTime = enc, parsedTime || ts
		}
		b, _ := json.Marshal(row)
		if used+len(b) > maxBytes {
			if len(out) == 0 {
				return dblifecycle.Outcome{}, fmt.Errorf("%w: row %d is %d bytes, over --max-bytes %d", dbstore.ErrLimitExceeded, offset+1, len(b), maxBytes)
			}
			truncated = true
			break
		}
		out = append(out, row)
		used += len(b)
	}
	if err := rows.Err(); err != nil {
		return dblifecycle.Outcome{}, s.classify(ctx, err)
	}
	if err := ctx.Err(); err != nil {
		return dblifecycle.Outcome{}, dbstore.FromContext(err)
	}
	rows.Close()
	if after, err := s.scalar(ctx, "select total_changes()"); err != nil {
		return dblifecycle.Outcome{}, err
	} else if after != changes {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: the read changed rows; the change was rolled back", dbstore.ErrReadOnly)
	}
	res := dblifecycle.Outcome{Result: map[string]any{"columns": columns, "rows": out}, Page: newPage(target, binding, offset, len(out), limit, used, truncated)}
	if truncated {
		res.Warnings = append(res.Warnings, cursorWarning)
	}
	if parsedTime {
		res.Warnings = append(res.Warnings, "the driver parsed DATE/DATETIME/TIMESTAMP text into ts values; select CAST(col AS TEXT) or +col for the stored text")
	}
	return res, nil
}

func runExec(ctx context.Context, h dbstore.Handle, in Input, allowSchema bool) (dblifecycle.Outcome, error) {
	db, err := handleDB(h)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	name := OpExec
	if allowSchema {
		name = OpSchemaApply
	}
	if in.Limit != 0 || in.MaxBytes != 0 || in.Since != "" || in.Out != "" || in.In != "" || in.OnConflict != "" || in.Replace {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: %s takes --sql/params or a statements batch", dbstore.ErrInvalidInput, name)
	}
	stmts := in.Statements
	if strings.TrimSpace(in.SQL) != "" {
		if len(stmts) != 0 {
			return dblifecycle.Outcome{}, fmt.Errorf("%w: give --sql or statements, not both", dbstore.ErrInvalidInput)
		}
		stmts = []Statement{{SQL: in.SQL, Params: in.Params}}
	} else if len(in.Params) != 0 {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: params need --sql", dbstore.ErrInvalidInput)
	}
	if len(stmts) == 0 {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: %s needs --sql or statements", dbstore.ErrInvalidInput, name)
	}
	// Every parameter is validated before the transaction starts.
	args := make([][]any, len(stmts))
	for i, st := range stmts {
		if strings.TrimSpace(st.SQL) == "" {
			return dblifecycle.Outcome{}, fmt.Errorf("%w: statement %d is empty", dbstore.ErrInvalidInput, i+1)
		}
		if args[i], err = decodeParams(st.Params); err != nil {
			return dblifecycle.Outcome{}, fmt.Errorf("statement %d: %w", i+1, err)
		}
	}
	s, err := begin(ctx, db, false)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	defer s.end()
	results := make([]map[string]any, 0, len(stmts))
	var total int64
	before, after, err := s.atomic(ctx, allowSchema, func() error {
		t0, err := s.scalar(ctx, "select total_changes()")
		if err != nil {
			return err
		}
		for i, st := range stmts {
			r, err := s.conn.ExecContext(ctx, st.SQL, args[i]...)
			if err != nil {
				return fmt.Errorf("statement %d: %w", i+1, s.classify(ctx, err))
			}
			n, _ := r.RowsAffected()
			id, _ := r.LastInsertId()
			results = append(results, map[string]any{"rows_affected": n, "last_insert_rowid": tagged{T: "int", V: strconv.FormatInt(id, 10)}})
		}
		t1, err := s.scalar(ctx, "select total_changes()")
		total = t1 - t0
		return err
	})
	if err != nil {
		return dblifecycle.Outcome{}, fmt.Errorf("%w (rolled back; nothing was applied)", err)
	}
	result := map[string]any{"committed": true, "statements": results, "total_changes": total}
	if allowSchema {
		result["schema_version"] = map[string]any{"before": before, "after": after}
	}
	return dblifecycle.Outcome{Result: result}, nil
}

type schemaObject struct {
	Type    string           `json:"type"`
	Name    string           `json:"name"`
	Table   string           `json:"tbl_name"`
	SQL     *string          `json:"sql"`
	Strict  *bool            `json:"strict,omitempty"`
	Rowid   *bool            `json:"rowid,omitempty"`
	Columns []map[string]any `json:"columns,omitempty"`
	Foreign []map[string]any `json:"foreign_keys,omitempty"`
}

// userObjects lists the schema in creation order, without SQLite's own
// internal objects (sqlite_sequence, sqlite_stat*, autoindexes).
func userObjects(ctx context.Context, s *session) ([]schemaObject, error) {
	rows, err := s.conn.QueryContext(ctx, "select type, name, tbl_name, sql from sqlite_schema where substr(name, 1, 7) <> 'sqlite_' order by rowid")
	if err != nil {
		return nil, s.classify(ctx, err)
	}
	var objs []schemaObject
	for rows.Next() {
		var o schemaObject
		if err := rows.Scan(&o.Type, &o.Name, &o.Table, &o.SQL); err != nil {
			rows.Close()
			return nil, s.classify(ctx, err)
		}
		objs = append(objs, o)
	}
	err = rows.Err()
	rows.Close()
	return objs, s.classify(ctx, err)
}

// tableInfo fills a table's columns, rowid/strict flags and foreign keys
// through the engine's own table-valued pragmas, with the name bound.
func tableInfo(ctx context.Context, s *session, o *schemaObject) (kind string, err error) {
	var wr, strict bool
	if err := s.conn.QueryRowContext(ctx, "select type, wr, strict from pragma_table_list(?) where schema = 'main'", o.Name).Scan(&kind, &wr, &strict); err != nil {
		return "", s.classify(ctx, err)
	}
	rowid := !wr
	o.Rowid, o.Strict = &rowid, &strict
	rows, err := s.conn.QueryContext(ctx, `select name, type, "notnull", dflt_value, pk, hidden from pragma_table_xinfo(?) order by cid`, o.Name)
	if err != nil {
		return "", s.classify(ctx, err)
	}
	for rows.Next() {
		var name, typ string
		var notNull bool
		var dflt *string
		var pk, hidden int64
		if err := rows.Scan(&name, &typ, &notNull, &dflt, &pk, &hidden); err != nil {
			rows.Close()
			return "", s.classify(ctx, err)
		}
		c := map[string]any{"name": name, "type": typ, "not_null": notNull, "default": dflt, "pk": pk}
		switch hidden {
		case 1:
			c["hidden"] = true
		case 2, 3:
			c["generated"] = true
		}
		o.Columns = append(o.Columns, c)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return "", s.classify(ctx, err)
	}
	rows.Close()
	fk, err := s.conn.QueryContext(ctx, `select id, seq, "table", "from", "to", on_update, on_delete from pragma_foreign_key_list(?) order by id, seq`, o.Name)
	if err != nil {
		return "", s.classify(ctx, err)
	}
	defer fk.Close()
	for fk.Next() {
		var id, seq int64
		var table, from, onUpdate, onDelete string
		var to *string
		if err := fk.Scan(&id, &seq, &table, &from, &to, &onUpdate, &onDelete); err != nil {
			return "", s.classify(ctx, err)
		}
		o.Foreign = append(o.Foreign, map[string]any{"id": id, "seq": seq, "table": table, "from": from, "to": to, "on_update": onUpdate, "on_delete": onDelete})
	}
	return kind, s.classify(ctx, fk.Err())
}

func runSchemaShow(ctx context.Context, h dbstore.Handle, in Input) (dblifecycle.Outcome, error) {
	db, err := handleDB(h)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	if in.SQL != "" || len(in.Params) != 0 || len(in.Statements) != 0 || in.Out != "" || in.In != "" || in.OnConflict != "" || in.Replace {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: sql schema show takes only bounds", dbstore.ErrInvalidInput)
	}
	limit, maxBytes, err := in.bounds()
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	target, binding := h.Identity().ID, queryBinding(OpSchemaShow, in)
	offset, err := resumeOffset(in.Since, target, binding)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	s, err := begin(ctx, db, true)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	defer s.end()
	// One read transaction: every object below comes from one snapshot.
	if _, err := s.conn.ExecContext(ctx, "begin"); err != nil {
		return dblifecycle.Outcome{}, s.classify(ctx, err)
	}
	objs, err := userObjects(ctx, s)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	userVersion, err := s.scalar(ctx, "pragma user_version")
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	schemaVersion, err := s.scalar(ctx, "pragma schema_version")
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	if offset > len(objs) {
		offset = len(objs)
	}
	out := []schemaObject{}
	used, truncated := 0, false
	for _, o := range objs[offset:] {
		if len(out) == limit {
			truncated = true
			break
		}
		if o.Type == "table" {
			if _, err := tableInfo(ctx, s, &o); err != nil {
				return dblifecycle.Outcome{}, err
			}
		}
		b, _ := json.Marshal(o)
		if used+len(b) > maxBytes {
			if len(out) == 0 {
				return dblifecycle.Outcome{}, fmt.Errorf("%w: schema object %q is %d bytes, over --max-bytes %d", dbstore.ErrLimitExceeded, o.Name, len(b), maxBytes)
			}
			truncated = true
			break
		}
		out = append(out, o)
		used += len(b)
	}
	res := dblifecycle.Outcome{
		Result: map[string]any{"objects": out, "user_version": userVersion, "schema_version": schemaVersion},
		Page:   newPage(target, binding, offset, len(out), limit, used, truncated),
	}
	if truncated {
		res.Warnings = []string{cursorWarning}
	}
	return res, nil
}

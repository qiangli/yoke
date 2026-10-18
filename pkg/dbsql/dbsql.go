// Package dbsql is the Sprint 453 SQL category (story af7f959dadcf) over the
// sqlitedb adapter: `db sql query|exec|schema show|schema apply` and the SQL
// logical interchange behind `db export|import`.
//
// Each operation registers with dblifecycle, so CLI, MCP and owner sessions
// reach it through one claim guard, owner admission, request-id replay and
// hold-epoch check. Read-only is enforced by the engine on the connection,
// never by reading SQL keywords: a read opens the file (or memdb) with
// mode=ro, sets query_only, and installs a commit hook that vetoes any write
// transaction, so a statement that turns query_only back off still cannot
// commit. Every connection runs with SQLITE_LIMIT_ATTACHED=0, so ATTACH and
// VACUUM INTO cannot reach a database outside the explicit target.
package dbsql

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"

	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/qiangli/yoke/pkg/dbstore"
	"github.com/qiangli/yoke/pkg/dbstore/sqlitedb"
)

// Operation names, as registered with dblifecycle.
const (
	OpQuery       = "sql.query"
	OpExec        = "sql.exec"
	OpSchemaShow  = "sql.schema.show"
	OpSchemaApply = "sql.schema.apply"
	OpExport      = "sql.export"
	OpImport      = "sql.import"
)

// Bounds declared for the sql category (contract §6).
const (
	DefaultLimit    = 100
	MaxLimit        = 10000
	DefaultMaxBytes = 256 << 10
	MaxMaxBytes     = 8 << 20
)

// Input is the typed input every sql operation accepts, from CLI flags, an
// --input JSON document or the MCP/owner request.
type Input struct {
	SQL        string          `json:"sql,omitempty"`
	Params     json.RawMessage `json:"params,omitempty"`
	Statements []Statement     `json:"statements,omitempty"`
	Limit      int             `json:"limit,omitempty"`
	MaxBytes   int             `json:"max_bytes,omitempty"`
	Since      string          `json:"since,omitempty"`
	Out        string          `json:"out,omitempty"`
	In         string          `json:"in,omitempty"`
	OnConflict string          `json:"on_conflict,omitempty"`
	Replace    bool            `json:"replace,omitempty"`
}

// Statement is one entry of an atomic exec batch.
type Statement struct {
	SQL    string          `json:"sql"`
	Params json.RawMessage `json:"params,omitempty"`
}

func decodeInput(raw json.RawMessage) (Input, error) {
	var in Input
	if len(raw) == 0 {
		return in, nil
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return in, fmt.Errorf("%w: sql input: %v", dbstore.ErrInvalidInput, err)
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

func handleDB(h dbstore.Handle) (*sql.DB, error) {
	db, ok := h.(*sqlitedb.DB)
	if !ok {
		return nil, fmt.Errorf("%w: sql category needs the sqlite engine", dbstore.ErrUnsupported)
	}
	return db.SQL(), nil
}

// session is one connection prepared for a category operation. Its commit
// hook admits a commit only when the operation itself allows it: never on a
// read, and only for the operation's own final COMMIT on a write. Its
// rollback hook records any transaction rollback, so a write operation whose
// script rolled back and reopened a transaction cannot commit only its tail.
type session struct {
	conn        *sql.Conn
	readOnly    bool
	allowCommit atomic.Bool
	vetoed      atomic.Bool
	rolledBack  atomic.Bool
	settings    [][2]string // connection pragmas as they were at begin
}

// connPragmas are the connection-scoped settings a statement can change that
// would otherwise outlive the operation on a pooled or held connection:
// read-only enforcement, temp spill, the native exclusive lock and the
// integrity checks. end restores every one that changed.
var connPragmas = []string{"query_only", "temp_store", "locking_mode", "foreign_keys",
	"recursive_triggers", "writable_schema", "ignore_check_constraints", "trusted_schema"}

type hookConn interface {
	RegisterCommitHook(sqlite.CommitHookFn)
	RegisterRollbackHook(sqlite.RollbackHookFn)
}

func begin(ctx context.Context, db *sql.DB, readOnly bool) (*session, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, sqlitedb.ClassifyCtx(ctx, err)
	}
	s := &session{conn: conn, readOnly: readOnly}
	if _, err := sqlite.Limit(conn, sqlite3.SQLITE_LIMIT_ATTACHED, 0); err != nil {
		conn.Close()
		return nil, sqlitedb.Classify(err)
	}
	for _, name := range connPragmas {
		var v string
		if err := conn.QueryRowContext(ctx, "pragma "+name).Scan(&v); err != nil {
			conn.Close()
			return nil, sqlitedb.ClassifyCtx(ctx, err)
		}
		s.settings = append(s.settings, [2]string{name, v})
	}
	err = conn.Raw(func(raw any) error {
		hc, ok := raw.(hookConn)
		if !ok {
			return fmt.Errorf("%w: sqlite commit hook", dbstore.ErrUnsupported)
		}
		hc.RegisterCommitHook(func() int32 {
			if s.allowCommit.Load() {
				return 0
			}
			s.vetoed.Store(true)
			return 1
		})
		hc.RegisterRollbackHook(func() { s.rolledBack.Store(true) })
		return nil
	})
	if err == nil && readOnly {
		_, err = conn.ExecContext(ctx, "pragma query_only=1")
	}
	if err != nil {
		s.discard()
		return nil, sqlitedb.ClassifyCtx(ctx, err)
	}
	return s, nil
}

// end rolls back anything the operation left open, restores the connection
// settings captured at begin and returns it to the handle's pool. A
// connection that still carries temporary objects or cannot be restored is
// discarded instead of reused. A restored locking_mode re-takes the native
// exclusive lock at once, so a held handle never stays unlocked.
func (s *session) end() {
	ctx := context.Background()
	if _, err := s.conn.ExecContext(ctx, "rollback"); err != nil && !strings.Contains(err.Error(), "no transaction is active") {
		s.discard()
		return
	}
	var temps int
	if err := s.conn.QueryRowContext(ctx, "select count(*) from temp.sqlite_schema").Scan(&temps); err != nil || temps != 0 {
		s.discard()
		return
	}
	relock := false
	for _, kv := range s.settings {
		var cur string
		if err := s.conn.QueryRowContext(ctx, "pragma "+kv[0]).Scan(&cur); err != nil {
			s.discard()
			return
		}
		if cur == kv[1] {
			continue
		}
		if _, err := s.conn.ExecContext(ctx, "pragma "+kv[0]+"="+kv[1]); err != nil {
			s.discard()
			return
		}
		relock = relock || kv[0] == "locking_mode" && kv[1] == "exclusive"
	}
	_ = s.conn.Raw(func(raw any) error {
		raw.(hookConn).RegisterCommitHook(nil)
		raw.(hookConn).RegisterRollbackHook(nil)
		return nil
	})
	if relock {
		if _, err := s.conn.ExecContext(ctx, "begin exclusive; commit"); err != nil {
			s.discard()
			return
		}
	}
	_ = s.conn.Close()
}

func (s *session) discard() {
	_ = s.conn.Raw(func(any) error { return driver.ErrBadConn })
	_ = s.conn.Close()
}

// classify reports an engine error with the contract code. A commit the
// hook vetoed is read_only on a read and an atomicity violation on a write.
func (s *session) classify(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	var se *sqlite.Error
	if errors.As(err, &se) && se.Code() == sqlite3.SQLITE_CONSTRAINT_ROWID {
		return fmt.Errorf("%w: %w", dbstore.ErrConflict, err)
	}
	if errors.As(err, &se) && se.Code() == sqlite3.SQLITE_CONSTRAINT_COMMITHOOK || s.vetoed.Load() {
		if s.readOnly {
			return fmt.Errorf("%w: the read connection refused to commit a write: %v", dbstore.ErrReadOnly, err)
		}
		return fmt.Errorf("%w: transaction control inside an atomic sql operation is refused; the whole operation is one transaction: %v", dbstore.ErrInvalidInput, err)
	}
	if msg := err.Error(); strings.Contains(msg, "no transaction is active") || strings.Contains(msg, "within a transaction") {
		return fmt.Errorf("%w: transaction control inside an atomic sql operation is refused; the whole operation is one transaction: %v", dbstore.ErrInvalidInput, err)
	}
	if strings.Contains(err.Error(), "too many attached databases") {
		return fmt.Errorf("%w: ATTACH and VACUUM are refused; a sql operation reaches only its explicit target: %v", dbstore.ErrUnsupported, err)
	}
	return sqlitedb.ClassifyCtx(ctx, err)
}

func (s *session) scalar(ctx context.Context, q string) (int64, error) {
	var n int64
	err := s.conn.QueryRowContext(ctx, q).Scan(&n)
	return n, s.classify(ctx, err)
}

// atomic runs fn inside one BEGIN IMMEDIATE transaction and commits only if
// fn succeeds, no rollback happened in between (the transaction committed is
// the one begun here), no temporary object is left behind and, unless
// allowSchema, the schema did not change: a schema change is a destructive
// `schema apply`, not an exec. The schema is compared by its stored
// definitions, not only by the caller-writable schema_version cookie.
func (s *session) atomic(ctx context.Context, allowSchema bool, fn func() error) (before, after int64, err error) {
	if _, err := s.conn.ExecContext(ctx, "begin immediate"); err != nil {
		return 0, 0, s.classify(ctx, err)
	}
	s.rolledBack.Store(false)
	if before, err = s.scalar(ctx, "pragma schema_version"); err != nil {
		return
	}
	defBefore, err := s.schemaDigest(ctx)
	if err != nil {
		return
	}
	if err = fn(); err != nil {
		return
	}
	if s.rolledBack.Load() {
		return before, 0, fmt.Errorf("%w: the transaction was rolled back inside the operation (ROLLBACK, OR ROLLBACK or a refused COMMIT); the whole operation is one transaction", dbstore.ErrInvalidInput)
	}
	if after, err = s.scalar(ctx, "pragma schema_version"); err != nil {
		return
	}
	defAfter, err := s.schemaDigest(ctx)
	if err != nil {
		return
	}
	if !allowSchema && (after != before || defAfter != defBefore) {
		return before, after, fmt.Errorf("%w: the statement changed the schema; schema changes need `db sql schema apply --yes` (exclusive hold)", dbstore.ErrInvalidInput)
	}
	temps, err := s.scalar(ctx, "select count(*) from temp.sqlite_schema")
	if err != nil {
		return
	}
	if temps != 0 {
		return before, after, fmt.Errorf("%w: temporary objects do not outlive one sql operation; drop them before it ends", dbstore.ErrInvalidInput)
	}
	s.allowCommit.Store(true)
	_, err = s.conn.ExecContext(ctx, "commit")
	s.allowCommit.Store(false)
	return before, after, s.classify(ctx, err)
}

// schemaDigest fingerprints the stored definitions of the main schema.
func (s *session) schemaDigest(ctx context.Context) (string, error) {
	rows, err := s.conn.QueryContext(ctx, "select type, name, tbl_name, rootpage, coalesce(sql, '') from main.sqlite_schema order by rowid")
	if err != nil {
		return "", s.classify(ctx, err)
	}
	defer rows.Close()
	h := sha256.New()
	for rows.Next() {
		var typ, name, tbl, def string
		var root int64
		if err := rows.Scan(&typ, &name, &tbl, &root, &def); err != nil {
			return "", s.classify(ctx, err)
		}
		fmt.Fprintf(h, "%q %q %q %d %q\n", typ, name, tbl, root, def)
	}
	if err := rows.Err(); err != nil {
		return "", s.classify(ctx, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

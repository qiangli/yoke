// Package sqlitedb is the dbstore adapter for SQLite through the pinned,
// CGO-free modernc.org/sqlite driver (the engine yoke already links).
//
// Both storage modes are native. Persistent targets are files opened in WAL
// mode, so independent processes share them directly: readers continue while
// writers serialize in the engine, bounded by busy_timeout. Memory targets use
// SQLite's own memdb VFS ("file:/<name>?vfs=memdb"), which keeps the database
// in this process's heap — never a temp file — and shares it across the
// connections of the one owner process that holds the MemSpace.
package sqlitedb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/qiangli/yoke/pkg/dbstore"
)

// Engine is the dbstore engine name.
const Engine = "sqlite"

// Adapter implements dbstore.Adapter.
type Adapter struct{}

var _ dbstore.Adapter = Adapter{}

func (Adapter) Engine() string { return Engine }

func (Adapter) Caps() dbstore.Caps {
	return dbstore.Summarize(dbstore.Caps{
		Engine:     Engine,
		Categories: []string{"sql", "documents", "vectors", "objects", "timeseries"},
		Modes: map[dbstore.Mode]dbstore.ModeCaps{
			dbstore.Persistent: {
				Supported: true, Via: "native",
				Sharing:   &dbstore.Sharing{NativeMultiprocess: true, OwnerRequiredForMultiprocess: false},
				Exclusion: &dbstore.Exclusion{Scopes: []string{"database"}, EnforcedBy: dbstore.EnforcedByNativeLock, NonBashyClients: dbstore.NonBashyBlocked},
			},
			dbstore.Memory: dbstore.MemoryModeCaps("native"),
		},
		Transactions: dbstore.Transactions{AtomicBatch: true, Isolation: "serializable"},
		Notes: []string{
			"persistent files use WAL; independent processes share them directly and writers serialize with busy_timeout",
			"memory databases live in the owner process heap (memdb VFS); other processes reach them only through that owner",
			"exclusive opens use locking_mode=EXCLUSIVE, which blocks other readers and writers until the handle closes",
			"an idle reader holds no file lock, so drop of a persistent file relies on the lifecycle db hold to exclude it",
			"a closed handle's pool rejects later statements, reported as lifetime_ended",
			"documents: db doc collections are tables created only by db doc collection create; db export|import of a sqlite target is the sql interchange, db doc export|import the document interchange",
			"vectors: db vec collections are sqlite-vec vec0 tables created only by db vec collection add (dimension, metric, model persisted); search is exact, never ANN; db vec export|import is the vector interchange",
			"objects: db obj buckets and revisioned objects with sha256 checksums; db obj export|import is the object interchange",
			"timeseries: db ts series are bashy_ts_series/bashy_ts_points tables created on first ingest; ts values keep their tagged type with exact int64 bits, described as SQL analytics, never full OLAP; db ts export|import is the time-series interchange",
		},
	})
}

// DB is an open SQLite database.
type DB struct {
	id        dbstore.Identity
	db        *sql.DB
	ro        bool
	exclusive bool
	guard     dbstore.Guard
	release   func()
}

func (d *DB) Identity() dbstore.Identity { return d.id }
func (d *DB) ReadOnly() bool             { return d.ro }
func (d *DB) Exclusive() bool            { return d.exclusive }

// SQL is the engine handle for the sql category operations.
func (d *DB) SQL() *sql.DB { return d.db }

// Close closes the handle's pool, then releases its MemSpace reference, once.
// The pool rejects every later statement, so a closed memory handle cannot
// reach the shared database past a later exclusive acquire.
func (d *DB) Close() error {
	return d.guard.Close(func() error {
		err := d.db.Close()
		if d.release != nil {
			d.release()
		}
		return err
	})
}

// memAnchor keeps a memdb database alive: SQLite frees it when its last
// connection closes, so the MemSpace holds one pinned connection.
type memAnchor struct {
	db   *sql.DB
	conn *sql.Conn
}

func (a *memAnchor) Close() error { return errors.Join(a.conn.Close(), a.db.Close()) }

func (Adapter) Create(ctx context.Context, t dbstore.Target, o dbstore.Options) (dbstore.Handle, error) {
	id, err := resolve(t, o, true)
	if err != nil {
		return nil, err
	}
	if id.Mode == dbstore.Memory {
		db, err := open(ctx, memDSN(id), o, false)
		if err != nil {
			return nil, err
		}
		conn, err := db.Conn(ctx)
		if err != nil {
			db.Close()
			return nil, Classify(err)
		}
		// A memdb name that already has content was created by someone else.
		var n int
		if err := conn.QueryRowContext(ctx, "select count(*) from sqlite_schema").Scan(&n); err != nil || n != 0 {
			conn.Close()
			db.Close()
			if err == nil {
				err = fmt.Errorf("%w: %s", dbstore.ErrExists, id.ID)
			}
			return nil, Classify(err)
		}
		if err := o.Mem.Put(id, &memAnchor{db: db, conn: conn}, o.Exclusive); err != nil {
			conn.Close()
			db.Close()
			return nil, err
		}
		return openMem(ctx, id, o, true)
	}
	// O_EXCL makes creation race-free; SQLite treats the empty file as a new database.
	f, err := os.OpenFile(id.Path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("%w: %s", dbstore.ErrExists, id.ID)
		}
		return nil, dbstore.Backend(Engine, err)
	}
	f.Close()
	db, err := open(ctx, fileDSN(id.Path, false), o, o.Exclusive)
	if err == nil {
		_, err = db.ExecContext(ctx, "pragma journal_mode=WAL")
		err = Classify(err)
	}
	if err != nil {
		if db != nil {
			db.Close()
		}
		os.Remove(id.Path)
		return nil, err
	}
	return &DB{id: id, db: db, exclusive: o.Exclusive}, nil
}

func (Adapter) Open(ctx context.Context, t dbstore.Target, o dbstore.Options) (dbstore.Handle, error) {
	id, err := resolve(t, o, false)
	if err != nil {
		return nil, err
	}
	if id.Mode == dbstore.Memory {
		return openMem(ctx, id, o, false)
	}
	if ok, err := dbstore.Exists(id.Path); err != nil {
		return nil, err
	} else if !ok {
		return nil, fmt.Errorf("%w: %s", dbstore.ErrNotFound, id.ID)
	}
	db, err := open(ctx, fileDSN(id.Path, o.ReadOnly), o, o.Exclusive)
	if err != nil {
		return nil, err
	}
	return &DB{id: id, db: db, ro: o.ReadOnly, exclusive: o.Exclusive}, nil
}

// Drop removes an idle database. A persistent file is removed while this
// process holds the exclusive lock (on Windows, right after releasing it,
// because an open file cannot be deleted there — the claim hold of the
// lifecycle layer covers that window).
func (a Adapter) Drop(ctx context.Context, t dbstore.Target, o dbstore.Options) error {
	id, err := resolve(t, o, false)
	if err != nil {
		return err
	}
	if id.Mode == dbstore.Memory {
		return o.Mem.Drop(id)
	}
	o.Exclusive, o.ReadOnly = true, false
	h, err := a.Open(ctx, t, o)
	if err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		h.Close()
		return removeFiles(id.Path)
	}
	err = removeFiles(id.Path)
	return errors.Join(err, h.Close())
}

func removeFiles(p string) error {
	var errs []error
	for _, f := range []string{p, p + "-wal", p + "-shm", p + "-journal"} {
		if err := os.Remove(f); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return dbstore.Backend(Engine, errors.Join(errs...))
}

func resolve(t dbstore.Target, o dbstore.Options, create bool) (dbstore.Identity, error) {
	if t.Engine == "" {
		t.Engine = Engine
	}
	if t.Engine != Engine {
		return dbstore.Identity{}, fmt.Errorf("%w: engine %q is not %s", dbstore.ErrInvalidInput, t.Engine, Engine)
	}
	id, err := dbstore.Resolve(t)
	if err != nil {
		return id, err
	}
	return id, o.Validate(id, create)
}

func openMem(ctx context.Context, id dbstore.Identity, o dbstore.Options, created bool) (dbstore.Handle, error) {
	if !created {
		if err := dbstore.Retry(ctx, o.WaitOrDefault(), func() error {
			_, err := o.Mem.Acquire(id, o.Exclusive)
			return err
		}); err != nil {
			return nil, err
		}
	}
	dsn := memDSN(id)
	if o.ReadOnly {
		dsn += "&mode=ro" // the engine refuses writes, not just query_only
	}
	db, err := open(ctx, dsn, o, false)
	if err != nil {
		o.Mem.Release(id)
		return nil, err
	}
	return &DB{id: id, db: db, ro: o.ReadOnly, exclusive: o.Exclusive, release: func() { o.Mem.Release(id) }}, nil
}

// open opens a pool and proves the connection with one statement, so a
// missing file, a lock wait or a bad path surfaces here and not on first use.
func open(ctx context.Context, dsn string, o dbstore.Options, exclusive bool) (*sql.DB, error) {
	q := url.Values{}
	q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", o.WaitOrDefault().Milliseconds()))
	if o.ReadOnly {
		q.Add("_pragma", "query_only(1)")
	}
	if exclusive {
		q.Add("_pragma", "locking_mode(EXCLUSIVE)")
		q.Set("_txlock", "exclusive")
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	db, err := sql.Open("sqlite", dsn+sep+q.Encode())
	if err != nil {
		return nil, Classify(err)
	}
	if exclusive {
		db.SetMaxOpenConns(1) // the lock belongs to one connection
		_, err = db.ExecContext(ctx, "begin exclusive; commit")
	} else {
		_, err = db.ExecContext(ctx, "select count(*) from sqlite_schema")
	}
	if err != nil {
		db.Close()
		return nil, Classify(err)
	}
	return db, nil
}

func fileDSN(p string, readOnly bool) string {
	slash := filepath.ToSlash(p)
	if !strings.HasPrefix(slash, "/") {
		slash = "/" + slash // Windows drive path: file:///C:/...
	}
	mode := "rw" // never "rwc": opening must not create
	if readOnly {
		mode = "ro"
	}
	return "file:" + (&url.URL{Path: slash}).EscapedPath() + "?mode=" + mode
}

// memDSN pins temp_store=MEMORY: sorts and temp tables of a memory database
// never spill to temporary files.
func memDSN(id dbstore.Identity) string {
	return "file:/" + url.PathEscape(id.Session) + "/" + url.PathEscape(id.Name) + "?vfs=memdb&_pragma=temp_store(2)"
}

// Classify maps a SQLite error to its contract code, keeping the engine
// message: BUSY/LOCKED -> busy, READONLY -> read_only, CANTOPEN -> not_found,
// UNIQUE/PRIMARYKEY constraint -> conflict, a closed pool -> lifetime_ended;
// context errors -> timeout/cancelled.
func Classify(err error) error {
	if err == nil {
		return nil
	}
	if c := dbstore.Code(err); c != "backend_error" {
		return dbstore.FromContext(err)
	}
	if err.Error() == "sql: database is closed" { // database/sql's unexported errDBClosed
		return fmt.Errorf("%w: %w", dbstore.ErrLifetimeEnded, err)
	}
	var se *sqlite.Error
	if errors.As(err, &se) {
		switch se.Code() {
		case sqlite3.SQLITE_CONSTRAINT_UNIQUE, sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY:
			return fmt.Errorf("%w: %w", dbstore.ErrConflict, err)
		}
		switch se.Code() & 0xff {
		case sqlite3.SQLITE_BUSY, sqlite3.SQLITE_LOCKED:
			return fmt.Errorf("%w: %w", dbstore.ErrBusy, err)
		case sqlite3.SQLITE_READONLY:
			return fmt.Errorf("%w: %w", dbstore.ErrReadOnly, err)
		case sqlite3.SQLITE_CANTOPEN:
			return fmt.Errorf("%w: %w", dbstore.ErrNotFound, err)
		}
	}
	return dbstore.Backend(Engine, err)
}

// ClassifyCtx is Classify for an error from a statement run under ctx: when
// ctx has ended, the engine's interrupt is reported as timeout or cancelled.
func ClassifyCtx(ctx context.Context, err error) error {
	if err != nil && ctx.Err() != nil {
		return fmt.Errorf("%w: %w", dbstore.FromContext(ctx.Err()), err)
	}
	return Classify(err)
}

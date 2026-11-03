package dbobj

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"strings"

	"github.com/qiangli/yoke/pkg/dbstore/sqlitedb"
)

// sqliteWriteTx owns a connection until Commit or Rollback. As in the doc,
// vec and ts categories, acquire the writer before reading preconditions:
// a deferred WAL snapshot cannot safely upgrade after another writer commits,
// and SQLite then returns BUSY without honoring the configured busy timeout.
type sqliteWriteTx struct {
	*sql.Conn
	ctx  context.Context
	done bool
}

func (s *sqliteStore) beginWrite(ctx context.Context) (*sqliteWriteTx, error) {
	conn, err := s.db.SQL().Conn(ctx)
	if err != nil {
		return nil, sqlitedb.ClassifyCtx(ctx, err)
	}
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		_ = conn.Close()
		return nil, sqlitedb.ClassifyCtx(ctx, err)
	}
	return &sqliteWriteTx{Conn: conn, ctx: ctx}, nil
}

func (tx *sqliteWriteTx) Commit() error {
	_, err := tx.ExecContext(tx.ctx, "COMMIT")
	if err == nil {
		tx.done = true
		_ = tx.Conn.Close()
	}
	return sqlitedb.ClassifyCtx(tx.ctx, err)
}

func (tx *sqliteWriteTx) Rollback() {
	if tx.done {
		return
	}
	// Cancellation must not leave an open transaction in the connection pool.
	if _, err := tx.ExecContext(context.Background(), "ROLLBACK"); err != nil && !strings.Contains(err.Error(), "no transaction is active") {
		_ = tx.Raw(func(any) error { return driver.ErrBadConn })
	}
	tx.done = true
	_ = tx.Conn.Close()
}

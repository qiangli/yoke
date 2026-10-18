// Package dblifecycle implements explicit database lifecycle operations over
// the existing dbstore adapters. Locator syntax and category data verbs live
// in their respective owning packages.
package dblifecycle

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/blevesearch/bleve/v2"
	"github.com/qiangli/bonsai/pkg/bonsai"

	bolt "go.etcd.io/bbolt"
	sqlite "modernc.org/sqlite"

	"github.com/qiangli/yoke/pkg/dbstore"
	"github.com/qiangli/yoke/pkg/dbstore/bboltdb"
	"github.com/qiangli/yoke/pkg/dbstore/blevedb"
	"github.com/qiangli/yoke/pkg/dbstore/bonsaidb"
	"github.com/qiangli/yoke/pkg/dbstore/sqlitedb"
	"github.com/qiangli/yoke/pkg/policy/coord"
	"github.com/qiangli/yoke/pkg/principal"
)

// Engine returns the installed adapter. Categories are separate verbs; vec
// uses a SQLite database and does not have an independent lifecycle.
func Engine(name string) (dbstore.Adapter, error) {
	switch name {
	case "sqlite":
		return sqlitedb.Adapter{}, nil
	case "bbolt":
		return bboltdb.Adapter{}, nil
	case "bleve":
		return blevedb.Adapter{}, nil
	case "bonsai":
		return bonsaidb.Adapter{}, nil
	default:
		return nil, fmt.Errorf("%w: engine %q", dbstore.ErrUnsupported, name)
	}
}

// Service binds all operations to one explicit owner memory space. A nil Mem
// means persistent-only direct access. Every operation checks the shared claim
// ledger before opening an adapter, including read-only operations.
type Service struct {
	Mem    *dbstore.MemSpace
	Holder principal.Ref
}

func (s Service) holder() principal.Ref {
	if s.Holder.Episode != "" {
		return s.Holder
	}
	return coord.Self()
}

func (s Service) options(id dbstore.Identity, wait time.Duration, exclusive bool) dbstore.Options {
	o := dbstore.Options{Wait: wait, Exclusive: exclusive}
	if id.Mode == dbstore.Memory {
		o.Mem = s.Mem
	}
	return o
}

func resolve(t dbstore.Target) (dbstore.Identity, dbstore.Adapter, error) {
	id, err := dbstore.Resolve(t)
	if err != nil {
		return id, nil, err
	}
	a, err := Engine(id.Engine)
	return id, a, err
}

func (s Service) guard(ctx context.Context, id dbstore.Identity) error {
	kind, name := id.ClaimUse()
	return coord.Guard(ctx, s.holder(), coord.Use{Kind: kind, Name: name})
}

// Create requires an explicit target. It never selects a default target or
// creates a parent directory.
func (s Service) Create(ctx context.Context, t dbstore.Target, wait time.Duration) (dbstore.Identity, error) {
	return s.CreateConfigured(ctx, t, nil, wait)
}

// CreateConfigured accepts an explicit Bleve mapping with the same claim
// admission as Create. The mapping is fixed for the lifetime of that index.
func (s Service) CreateConfigured(ctx context.Context, t dbstore.Target, input json.RawMessage, wait time.Duration) (dbstore.Identity, error) {
	id, a, err := resolve(t)
	if err != nil {
		return id, err
	}
	if err := s.guard(ctx, id); err != nil {
		return id, err
	}
	if len(input) != 0 {
		var cfg struct {
			Mapping json.RawMessage `json:"mapping"`
		}
		dec := json.NewDecoder(bytes.NewReader(input))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&cfg); err != nil {
			return id, fmt.Errorf("%w: create input: %v", dbstore.ErrInvalidInput, err)
		}
		var extra any
		if err := dec.Decode(&extra); err != io.EOF {
			return id, fmt.Errorf("%w: trailing create input", dbstore.ErrInvalidInput)
		}
		if len(cfg.Mapping) != 0 {
			if id.Engine != blevedb.Engine {
				return id, fmt.Errorf("%w: mapping applies only to bleve", dbstore.ErrInvalidInput)
			}
			m := bleve.NewIndexMapping()
			if err := json.Unmarshal(cfg.Mapping, m); err != nil {
				return id, fmt.Errorf("%w: mapping: %v", dbstore.ErrInvalidInput, err)
			}
			if err := m.Validate(); err != nil {
				return id, fmt.Errorf("%w: mapping: %v", dbstore.ErrInvalidInput, err)
			}
			a = blevedb.Adapter{Mapping: m}
		}
	}
	h, err := a.Create(ctx, id.Target, s.options(id, wait, false))
	if err != nil {
		return id, err
	}
	return id, h.Close()
}

// Open grants category implementations a checked handle. They must close it.
// This guard runs at every CLI and MCP entry through the common service.
func (s Service) Open(ctx context.Context, t dbstore.Target, readOnly bool, wait time.Duration) (dbstore.Handle, error) {
	id, a, err := resolve(t)
	if err != nil {
		return nil, err
	}
	if err := s.guard(ctx, id); err != nil {
		return nil, err
	}
	o := s.options(id, wait, false)
	o.ReadOnly = readOnly
	return a.Open(ctx, id.Target, o)
}

// Inspect checks that the target can be opened without changing it. SQLite
// additionally runs the engine's quick_check. Other engines are explicitly
// reported as open checks, not full integrity verification.
func (s Service) Inspect(ctx context.Context, t dbstore.Target, wait time.Duration) (dbstore.Identity, string, error) {
	h, err := s.Open(ctx, t, true, wait)
	if err != nil {
		return dbstore.Identity{}, "", err
	}
	defer h.Close()
	id := h.Identity()
	level, err := inspectHandle(ctx, h)
	return id, level, err
}

func inspectHandle(ctx context.Context, h dbstore.Handle) (string, error) {
	if db, ok := h.(*sqlitedb.DB); ok {
		var result string
		if err := db.SQL().QueryRowContext(ctx, "pragma quick_check").Scan(&result); err != nil {
			return "", sqlitedb.Classify(err)
		}
		if result != "ok" {
			return result, fmt.Errorf("%w: sqlite quick_check: %s", dbstore.ErrIntegrity, result)
		}
		return "quick_check", nil
	}
	return "open_only", nil
}

// Drop takes a whole-database claim before asking the adapter to remove data.
// A conflicting holder or a busy native backend leaves the target intact.
func (s Service) Drop(ctx context.Context, t dbstore.Target, wait time.Duration) (dbstore.Identity, error) {
	id, a, err := resolve(t)
	if err != nil {
		return id, err
	}
	// Missing targets leave even the claim ledger untouched.
	probe := s.options(id, wait, false)
	probe.ReadOnly = true
	h, err := a.Open(ctx, id.Target, probe)
	if err != nil {
		return id, err
	}
	if err := h.Close(); err != nil {
		return id, err
	}
	hold, err := acquireClaim(ctx, id, s.holder(), wait)
	if err != nil {
		return id, err
	}
	defer hold.release()
	if err := s.guard(ctx, id); err != nil {
		return id, err
	}
	return id, a.Drop(ctx, id.Target, s.options(id, wait, true))
}

// Backup produces a native snapshot for SQLite, bbolt, and persistent Bonsai.
// The destination is exclusive: a previous backup is never overwritten.
func (s Service) Backup(ctx context.Context, t dbstore.Target, out string, wait time.Duration) (dbstore.Identity, error) {
	// VACUUM INTO reads source content but SQLite requires a read-write
	// connection for the statement itself.
	h, err := s.Open(ctx, t, t.Engine != "sqlite", wait)
	if err != nil {
		return dbstore.Identity{}, err
	}
	defer h.Close()
	id := h.Identity()
	return id, backupHandle(ctx, h, out)
}

func backupHandle(ctx context.Context, h dbstore.Handle, out string) error {
	id := h.Identity()
	if out == "" || out == "-" {
		return fmt.Errorf("%w: backup requires a file path", dbstore.ErrInvalidInput)
	}
	if id.Engine == "bonsai" {
		if id.Mode != dbstore.Persistent {
			return fmt.Errorf("%w: bonsai memory native backup; use graph export", dbstore.ErrUnsupported)
		}
		if err := ctx.Err(); err != nil {
			return dbstore.FromContext(err)
		}
		// Bonsai writes a directory tree. Reserve the destination exclusively
		// so a concurrent backup can never replace an existing one.
		if err := os.Mkdir(out, 0o700); err != nil {
			if os.IsExist(err) {
				return dbstore.ErrExists
			}
			return err
		}
		complete := false
		defer func() {
			if !complete {
				_ = os.RemoveAll(out)
			}
		}()
		if _, err := h.(*bonsaidb.DB).Bonsai().BackupTo(ctx, bonsai.BackupOptions{Dir: out, Type: bonsai.BackupFull}); err != nil {
			return dbstore.Backend("bonsai", err)
		}
		complete = true
		return nil
	}
	if id.Engine != "sqlite" && id.Engine != "bbolt" {
		return fmt.Errorf("%w: %s native backup; use backend logical export when available", dbstore.ErrUnsupported, id.Engine)
	}
	p, err := filepath.Abs(out)
	if err != nil {
		return err
	}
	staged, publish, cleanup, err := stageBackup(p)
	if err != nil {
		return err
	}
	defer cleanup()
	if id.Engine == "sqlite" {
		// SQLite VACUUM INTO takes a transactionally consistent snapshot. The
		// staging path is private; publication never clobbers a competing file.
		if id.Mode == dbstore.Memory {
			err = sqliteBackupTo(ctx, h.(*sqlitedb.DB), staged)
		} else {
			_, err = h.(*sqlitedb.DB).SQL().ExecContext(ctx, "vacuum main into ?", staged)
		}
		if err != nil {
			return sqlitedb.Classify(err)
		}
		return publish()
	}
	bdb := h.(*bboltdb.DB).Bolt()
	if bdb == nil {
		return fmt.Errorf("%w: bbolt memory native snapshot; use logical export", dbstore.ErrUnsupported)
	}
	f, err := os.OpenFile(staged, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	err = bdb.View(func(tx *bolt.Tx) error {
		_, err := tx.WriteTo(f)
		return err
	})
	if err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return publish()
}

func sqliteBackupTo(ctx context.Context, db *sqlitedb.DB, path string) error {
	conn, err := db.SQL().Conn(ctx)
	if err != nil {
		return sqlitedb.Classify(err)
	}
	defer conn.Close()
	return sqlitedb.Classify(conn.Raw(func(raw any) error {
		backuper, ok := raw.(interface {
			NewBackup(string) (*sqlite.Backup, error)
		})
		if !ok {
			return fmt.Errorf("%w: sqlite native backup driver", dbstore.ErrUnsupported)
		}
		b, err := backuper.NewBackup(path)
		if err != nil {
			return err
		}
		finished := false
		defer func() {
			if !finished {
				_ = b.Finish()
			}
		}()
		for {
			if err := ctx.Err(); err != nil {
				return dbstore.FromContext(err)
			}
			more, err := b.Step(128)
			if err != nil {
				return err
			}
			if !more {
				break
			}
		}
		finished = true
		return b.Finish()
	}))
}

// stageBackup makes a private image beside the requested destination, then
// publishes it with a no-clobber hard link. A competing creator can win the
// final name, but its bytes are never removed or replaced by our cleanup.
func stageBackup(out string) (staged string, publish func() error, cleanup func(), err error) {
	dir, err := os.MkdirTemp(filepath.Dir(out), ".db-backup-*")
	if err != nil {
		return "", nil, nil, err
	}
	_ = os.Chmod(dir, 0o700)
	staged = filepath.Join(dir, "snapshot.db")
	cleanup = func() { _ = os.RemoveAll(dir) }
	publish = func() error {
		if err := os.Link(staged, out); err != nil {
			if os.IsExist(err) {
				return fmt.Errorf("%w: backup destination %s", dbstore.ErrExists, out)
			}
			return err
		}
		return nil
	}
	return staged, publish, cleanup, nil
}

// Restore replaces an explicitly existing SQLite target from a checked native
// snapshot. SQLite's backup API performs the copy through an exclusive target
// connection, so the target path is never exposed as a half-written file.
func (s Service) Restore(ctx context.Context, t dbstore.Target, in string, replace bool, wait time.Duration) (dbstore.Identity, error) {
	id, a, err := resolve(t)
	if err != nil {
		return id, err
	}
	if id.Engine == "bonsai" {
		return s.restoreBonsai(ctx, id, a, in, replace, wait)
	}
	if id.Engine != "sqlite" {
		return id, fmt.Errorf("%w: native restore for %s/%s", dbstore.ErrUnsupported, id.Engine, id.Mode)
	}
	probe := s.options(id, wait, false)
	probe.ReadOnly = true
	h, err := a.Open(ctx, id.Target, probe)
	if err != nil {
		return id, err
	}
	if err := h.Close(); err != nil {
		return id, err
	}
	if !replace {
		return id, fmt.Errorf("%w: restore requires --replace for an existing target", dbstore.ErrExists)
	}
	if in == "" || in == "-" {
		return id, fmt.Errorf("%w: restore requires a file path", dbstore.ErrInvalidInput)
	}
	input, err := filepath.Abs(in)
	if err != nil {
		return id, err
	}
	if id.Mode == dbstore.Persistent && input == id.Path {
		return id, fmt.Errorf("%w: backup and target are identical", dbstore.ErrInvalidInput)
	}
	// The adapter's read-only open refuses missing sources without creating a
	// file; quick_check validates the native image before replacing anything.
	source, err := (sqlitedb.Adapter{}).Open(ctx, dbstore.Target{Engine: "sqlite", Mode: dbstore.Persistent, Path: input}, dbstore.Options{ReadOnly: true, Wait: wait})
	if err != nil {
		return id, err
	}
	_, err = inspectHandle(ctx, source)
	_ = source.Close()
	if err != nil {
		return id, err
	}
	hold, err := acquireClaim(ctx, id, s.holder(), wait)
	if err != nil {
		return id, err
	}
	defer hold.release()
	if err := s.guard(ctx, id); err != nil {
		return id, err
	}
	dest, err := a.Open(ctx, id.Target, s.options(id, wait, true))
	if err != nil {
		return id, err
	}
	defer dest.Close()
	conn, err := dest.(*sqlitedb.DB).SQL().Conn(ctx)
	if err != nil {
		return id, sqlitedb.Classify(err)
	}
	defer conn.Close()
	err = conn.Raw(func(raw any) error {
		backuper, ok := raw.(interface {
			NewRestore(string) (*sqlite.Backup, error)
		})
		if !ok {
			return fmt.Errorf("%w: sqlite native restore driver", dbstore.ErrUnsupported)
		}
		b, err := backuper.NewRestore(input)
		if err != nil {
			return err
		}
		finished := false
		defer func() {
			if !finished {
				_ = b.Finish()
			}
		}()
		for {
			if err := ctx.Err(); err != nil {
				return dbstore.FromContext(err)
			}
			more, err := b.Step(128)
			if err != nil {
				return err
			}
			if !more {
				break
			}
		}
		finished = true
		return b.Finish()
	})
	return id, sqlitedb.Classify(err)
}

// restoreBonsai first replays the complete backup into a private disposable
// database. Bonsai's native restore mutates as it reads, so manifest parsing
// alone cannot guarantee that malformed backup bytes leave the target intact.
func (s Service) restoreBonsai(ctx context.Context, id dbstore.Identity, a dbstore.Adapter, in string, replace bool, wait time.Duration) (dbstore.Identity, error) {
	probe := s.options(id, wait, false)
	probe.ReadOnly = true
	h, err := a.Open(ctx, id.Target, probe)
	if err != nil {
		return id, err
	}
	if err = h.Close(); err != nil {
		return id, err
	}
	if !replace {
		return id, fmt.Errorf("%w: restore requires --replace", dbstore.ErrExists)
	}
	if id.Mode != dbstore.Persistent {
		return id, fmt.Errorf("%w: bonsai memory native restore; use graph import", dbstore.ErrUnsupported)
	}
	if in == "" || in == "-" {
		return id, fmt.Errorf("%w: restore requires a directory", dbstore.ErrInvalidInput)
	}
	input, err := filepath.Abs(in)
	if err != nil {
		return id, err
	}
	if input == id.Path {
		return id, fmt.Errorf("%w: backup and target are identical", dbstore.ErrInvalidInput)
	}
	info, err := os.Stat(input)
	if err != nil {
		return id, err
	}
	if !info.IsDir() {
		return id, fmt.Errorf("%w: bonsai backup must be a directory", dbstore.ErrInvalidInput)
	}
	stage, err := os.MkdirTemp("", "bonsai-restore-check-*")
	if err != nil {
		return id, err
	}
	defer os.RemoveAll(stage)
	checkTarget := dbstore.Target{Engine: "bonsai", Mode: dbstore.Persistent, Path: filepath.Join(stage, "db")}
	check, err := a.Create(ctx, checkTarget, dbstore.Options{Wait: wait})
	if err != nil {
		return id, err
	}
	err = check.(*bonsaidb.DB).Bonsai().RestoreFromManifest(ctx, input)
	closeErr := check.Close()
	if err != nil {
		return id, fmt.Errorf("%w: invalid bonsai backup: %v", dbstore.ErrIntegrity, err)
	}
	if closeErr != nil {
		return id, closeErr
	}
	if err := ctx.Err(); err != nil {
		return id, dbstore.FromContext(err)
	}
	hold, err := acquireClaim(ctx, id, s.holder(), wait)
	if err != nil {
		return id, err
	}
	defer hold.release()
	if err := s.guard(ctx, id); err != nil {
		return id, err
	}
	dest, err := a.Open(ctx, id.Target, s.options(id, wait, true))
	if err != nil {
		return id, err
	}
	defer dest.Close()
	b := dest.(*bonsaidb.DB).Bonsai()
	if err := b.DropAll(ctx); err != nil {
		return id, dbstore.Backend("bonsai", err)
	}
	if err := b.RestoreFromManifest(ctx, input); err != nil {
		return id, fmt.Errorf("%w: native bonsai restore failed after target reset: %v", dbstore.ErrIntegrity, err)
	}
	return id, nil
}

package dbobj

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/dbstore"
	"github.com/qiangli/yoke/pkg/dbstore/sqlitedb"
)

// A held independent writer makes the read-to-write upgrade failure
// deterministic: deferred transactions read successfully, then fail BUSY
// instead of waiting for the writer and checking its committed revision.
func TestSQLiteObjectWaitsBeforeReadingRevision(t *testing.T) {
	for _, conditional := range []bool{false, true} {
		t.Run(map[bool]string{false: "put", true: "revision"}[conditional], func(t *testing.T) {
			svc, mem := newTestService(t, dbstore.LifetimeInvocation)
			defer mem.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			target := dbstore.Target{Engine: "sqlite", Mode: dbstore.Persistent, Path: filepath.Join(t.TempDir(), "objects.db")}
			if _, err := svc.Create(ctx, target, time.Second); err != nil {
				t.Fatal(err)
			}
			mustRun(t, svc, target, OpBucketCreate, Input{Bucket: "bucket"}, false)
			mustRun(t, svc, target, OpPut, Input{Bucket: "bucket", Key: "key", Value: &Value{Text: strPtr("initial")}}, false)
			h, err := svc.Open(ctx, target, false, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer h.Close()
			db := h.(*sqlitedb.DB)
			writer, err := db.SQL().Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer writer.Close()
			if _, err := writer.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
				t.Fatal(err)
			}
			defer writer.ExecContext(context.Background(), "ROLLBACK")
			if _, err := writer.ExecContext(ctx, "UPDATE _bashy_objects SET revision=2 WHERE bucket='bucket' AND key='key'"); err != nil {
				t.Fatal(err)
			}
			started, done := make(chan struct{}), make(chan error, 1)
			go func() {
				close(started)
				revision := ""
				if conditional {
					revision = "1"
				}
				_, err := newSQLiteStore(db).putObject(ctx, ObjectInfo{Bucket: "bucket", Key: "key"}, []byte("replacement"), revision, false)
				done <- err
			}()
			<-started
			select {
			case err := <-done:
				t.Fatalf("write returned before independent writer released: %v", err)
			case <-time.After(100 * time.Millisecond):
			}
			if _, err := writer.ExecContext(ctx, "COMMIT"); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if conditional && !errors.Is(err, dbstore.ErrConflict) {
					t.Fatalf("stale revision: %v", err)
				}
				if !conditional && err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			obj, data, err := newSQLiteStore(db).getObject(ctx, "bucket", "key")
			if err != nil {
				t.Fatal(err)
			}
			wantData, wantRevision := "replacement", int64(3)
			if conditional {
				wantData, wantRevision = "initial", 2
			}
			if string(data) != wantData || obj.Revision != wantRevision {
				t.Fatalf("object=%+v data=%q", obj, data)
			}
		})
	}
}

func TestSQLiteObjectCanceledCommitReleasesWriter(t *testing.T) {
	svc, mem := newTestService(t, dbstore.LifetimeInvocation)
	defer mem.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	target := dbstore.Target{Engine: "sqlite", Mode: dbstore.Persistent, Path: filepath.Join(t.TempDir(), "objects.db")}
	if _, err := svc.Create(ctx, target, time.Second); err != nil {
		t.Fatal(err)
	}
	mustRun(t, svc, target, OpBucketCreate, Input{Bucket: "bucket"}, false)
	mustRun(t, svc, target, OpPut, Input{Bucket: "bucket", Key: "key", Value: &Value{Text: strPtr("initial")}}, false)
	h, err := svc.Open(ctx, target, false, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	db := h.(*sqlitedb.DB)
	db.SQL().SetMaxOpenConns(1)
	store := newSQLiteStore(db)
	operation, stop := context.WithCancel(ctx)
	defer stop()
	tx, err := store.beginWrite(operation)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(operation, "UPDATE _bashy_objects SET revision=99"); err != nil {
		t.Fatal(err)
	}
	stop()
	if err := tx.Commit(); err == nil {
		t.Fatal("canceled commit succeeded")
	}
	tx.Rollback()
	obj, _, err := store.getObject(ctx, "bucket", "key")
	if err != nil {
		t.Fatal(err)
	}
	if obj.Revision != 1 {
		t.Fatalf("canceled mutation persisted: %+v", obj)
	}
	obj, err = store.putObject(ctx, ObjectInfo{Bucket: "bucket", Key: "key"}, []byte("next"), "1", false)
	if err != nil || obj.Revision != 2 {
		t.Fatalf("writer not reusable: object=%+v err=%v", obj, err)
	}
}

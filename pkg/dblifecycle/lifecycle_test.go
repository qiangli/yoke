package dblifecycle

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/dgraph-io/dgo/v250/protos/api"
	"github.com/qiangli/bonsai/pkg/bonsai"
	"github.com/qiangli/yoke/pkg/dbstore"
	"github.com/qiangli/yoke/pkg/dbstore/bboltdb"
	"github.com/qiangli/yoke/pkg/dbstore/bonsaidb"
	"github.com/qiangli/yoke/pkg/dbstore/sqlitedb"
)

func TestSQLiteLifecycleBackupRestore(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	s := Service{}
	target := dbstore.Target{Engine: "sqlite", Mode: dbstore.Persistent, Path: filepath.Join(root, "main.db")}
	if _, _, err := s.Inspect(ctx, target, 0); !errors.Is(err, dbstore.ErrNotFound) {
		t.Fatalf("missing show = %v", err)
	}
	if _, err := s.Create(ctx, target, 0); err != nil {
		t.Fatal(err)
	}
	h, err := s.Open(ctx, target, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.(*sqlitedb.DB).SQL().ExecContext(ctx, "create table items(v text); insert into items values ('kept')")
	_ = h.Close()
	if err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(root, "backup.db")
	if _, err := s.Backup(ctx, target, backup, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Backup(ctx, target, backup, 0); !errors.Is(err, dbstore.ErrExists) {
		t.Fatalf("backup overwrite = %v", err)
	}
	restored := dbstore.Target{Engine: "sqlite", Mode: dbstore.Persistent, Path: filepath.Join(root, "restored.db")}
	if _, err := s.Restore(ctx, restored, backup, true, 0); !errors.Is(err, dbstore.ErrNotFound) {
		t.Fatalf("restore missing target = %v", err)
	}
	if _, err := s.Create(ctx, restored, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Restore(ctx, restored, backup, false, 0); !errors.Is(err, dbstore.ErrExists) {
		t.Fatalf("restore without replace = %v", err)
	}
	if _, err := s.Restore(ctx, restored, backup, true, 0); err != nil {
		t.Fatal(err)
	}
	h, err = s.Open(ctx, restored, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	var got string
	err = h.(*sqlitedb.DB).SQL().QueryRowContext(ctx, "select v from items").Scan(&got)
	_ = h.Close()
	if err != nil || got != "kept" {
		t.Fatalf("restored value = %q, %v", got, err)
	}
	if _, err := s.Drop(ctx, restored, 0); err != nil {
		t.Fatal(err)
	}
}

func TestBonsaiNativeBackupRestore(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	s := Service{}
	target := dbstore.Target{Engine: "bonsai", Mode: dbstore.Persistent, Path: filepath.Join(root, "graph")}
	if _, err := s.Create(ctx, target, 0); err != nil {
		t.Fatal(err)
	}
	h, err := s.Open(ctx, target, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	b := h.(*bonsaidb.DB).Bonsai()
	if err := b.Alter(ctx, "name: string @index(exact) ."); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Mutate(ctx, &api.Mutation{SetNquads: []byte(`_:a <name> "Alice" .`), CommitNow: true}); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(root, "native-backup")
	if _, err := s.Backup(ctx, target, backup, 0); err != nil {
		t.Fatal(err)
	}
	// A caller can change the source after staging. Native restore must use
	// the same private bytes that passed validation.
	staged, err := stageBonsaiBackup(ctx, backup, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manifestBytes, err := os.ReadFile(filepath.Join(staged, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest bonsai.MasterManifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatal(err)
	}
	backupFile := filepath.Join(backup, manifest.Manifests[0].Path, fmt.Sprintf("r%d-g1.backup", manifest.Manifests[0].ReadTs))
	stagedFile := filepath.Join(staged, manifest.Manifests[0].Path, fmt.Sprintf("r%d-g1.backup", manifest.Manifests[0].ReadTs))
	beforeStage, err := os.ReadFile(stagedFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(backupFile, []byte("changed source"), 0600); err != nil {
		t.Fatal(err)
	}
	afterStage, err := os.ReadFile(stagedFile)
	if err != nil || !bytes.Equal(beforeStage, afterStage) {
		t.Fatalf("staged backup changed with source: %v", err)
	}
	if err := os.WriteFile(backupFile, beforeStage, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Backup(ctx, target, backup, 0); !errors.Is(err, dbstore.ErrExists) {
		t.Fatalf("backup overwrite: %v", err)
	}
	if _, err := s.Restore(ctx, target, filepath.Join(root, "missing"), true, 0); err == nil {
		t.Fatal("missing backup accepted")
	}
	h, err = s.Open(ctx, target, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	b = h.(*bonsaidb.DB).Bonsai()
	if _, err := b.Mutate(ctx, &api.Mutation{SetNquads: []byte(`_:b <name> "Bob" .`), CommitNow: true}); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Restore(ctx, target, backup, false, 0); !errors.Is(err, dbstore.ErrExists) {
		t.Fatalf("restore without replace: %v", err)
	}
	broken := filepath.Join(root, "broken")
	if err := os.Mkdir(broken, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(broken, "manifest.json"), []byte(`{"manifests":[{"type":"full","backup_num":1,"backup_id":"x","path":"missing","read_ts":1}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Restore(ctx, target, broken, true, 0); !errors.Is(err, dbstore.ErrIntegrity) {
		t.Fatalf("invalid backup: %v", err)
	}
	lateBroken := filepath.Join(root, "late-broken")
	if err := os.Mkdir(lateBroken, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := stageBonsaiBackup(ctx, backup, lateBroken); err != nil {
		t.Fatal(err)
	}
	lateBroken = filepath.Join(lateBroken, "backup")
	lateFile := filepath.Join(lateBroken, manifest.Manifests[0].Path, fmt.Sprintf("r%d-g1.backup", manifest.Manifests[0].ReadTs))
	if err := os.WriteFile(lateFile, []byte("bad payload"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Restore(ctx, target, lateBroken, true, 0); !errors.Is(err, dbstore.ErrIntegrity) {
		t.Fatalf("malformed late payload: %v", err)
	}
	h, err = s.Open(ctx, target, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	before, err := h.(*bonsaidb.DB).Bonsai().Query(ctx, `{ q(func: has(name)) { name } }`)
	_ = h.Close()
	if err != nil || !bytes.Contains(before.Json, []byte("Bob")) {
		t.Fatalf("invalid restore changed target: %s %v", before.Json, err)
	}
	if _, err := s.Restore(ctx, target, backup, true, 0); err != nil {
		t.Fatal(err)
	}
	h, err = s.Open(ctx, target, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	result, err := h.(*bonsaidb.DB).Bonsai().Query(ctx, `{ q(func: has(name)) { name } }`)
	_ = h.Close()
	if err != nil || !bytes.Contains(result.Json, []byte("Alice")) || bytes.Contains(result.Json, []byte("Bob")) {
		t.Fatalf("restored graph: %s %v", result.Json, err)
	}
}

func TestStageBonsaiBackupRejectsEscapingReferences(t *testing.T) {
	for _, path := range []string{"../outside", "/outside", `..\outside`} {
		t.Run(path, func(t *testing.T) {
			input := t.TempDir()
			manifest := bonsai.MasterManifest{Manifests: []*bonsai.Manifest{{Path: path, ReadTs: 1}}}
			data, err := json.Marshal(manifest)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(input, "manifest.json"), data, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := stageBonsaiBackup(context.Background(), input, t.TempDir()); !errors.Is(err, dbstore.ErrInvalidInput) {
				t.Fatalf("path %q: %v", path, err)
			}
		})
	}
}

func TestBackupPublicationPreservesCompetingDestination(t *testing.T) {
	out := filepath.Join(t.TempDir(), "backup.db")
	staged, publish, cleanup, err := stageBackup(out)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if err := os.WriteFile(staged, []byte("ours"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out, []byte("theirs"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := publish(); !errors.Is(err, dbstore.ErrExists) {
		t.Fatalf("publication conflict = %v", err)
	}
	cleanup()
	b, err := os.ReadFile(out)
	if err != nil || string(b) != "theirs" {
		t.Fatalf("competing destination = %q %v", b, err)
	}
}

func TestSQLiteMemoryNativeBackupRestore(t *testing.T) {
	ctx := context.Background()
	mem, err := dbstore.NewMemSpace(dbstore.LifetimeOwner)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()
	s := Service{Mem: mem}
	source := dbstore.Target{Engine: "sqlite", Mode: dbstore.Memory, Session: mem.ID(), Name: "source"}
	dest := dbstore.Target{Engine: "sqlite", Mode: dbstore.Memory, Session: mem.ID(), Name: "restored"}
	if _, err := s.Create(ctx, source, 0); err != nil {
		t.Fatal(err)
	}
	h, err := s.Open(ctx, source, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.(*sqlitedb.DB).SQL().ExecContext(ctx, "create table data(v blob); insert into data values (x'00ff10')")
	_ = h.Close()
	if err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(t.TempDir(), "memory-copy.db")
	if _, err := s.Backup(ctx, source, backup, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(ctx, dest, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Restore(ctx, dest, backup, true, 0); err != nil {
		t.Fatal(err)
	}
	h, err = s.Open(ctx, dest, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	var got []byte
	err = h.(*sqlitedb.DB).SQL().QueryRowContext(ctx, "select v from data").Scan(&got)
	_ = h.Close()
	if err != nil || !bytes.Equal(got, []byte{0, 255, 16}) {
		t.Fatalf("memory restored blob = %x %v", got, err)
	}
	if err := mem.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Open(ctx, dest, true, 0); !errors.Is(err, dbstore.ErrLifetimeEnded) {
		t.Fatalf("ended memory = %v", err)
	}
}

func TestBboltNativeBackupReopens(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	s := Service{}
	target := dbstore.Target{Engine: "bbolt", Mode: dbstore.Persistent, Path: filepath.Join(root, "original.db")}
	if _, err := s.Create(ctx, target, 0); err != nil {
		t.Fatal(err)
	}
	h, err := s.Open(ctx, target, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	err = h.(*bboltdb.DB).Update(ctx, func(tx dbstore.KVTx) error {
		if err := tx.CreateBucket([]byte("records")); err != nil {
			return err
		}
		return tx.Put([]byte("records"), []byte("k"), []byte{0, 1, 255})
	})
	_ = h.Close()
	if err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(root, "copy.db")
	if _, err := s.Backup(ctx, target, backup, 0); err != nil {
		t.Fatal(err)
	}
	copyTarget := dbstore.Target{Engine: "bbolt", Mode: dbstore.Persistent, Path: backup}
	h, err = s.Open(ctx, copyTarget, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	var got []byte
	err = h.(*bboltdb.DB).View(ctx, func(tx dbstore.KVTx) error { v, _, err := tx.Get([]byte("records"), []byte("k")); got = v; return err })
	_ = h.Close()
	if err != nil || !bytes.Equal(got, []byte{0, 1, 255}) {
		t.Fatalf("bbolt backup value=%x %v", got, err)
	}
}

package loom

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

func backupCommit(t *testing.T, r *git.Repository, parents ...plumbing.Hash) plumbing.Hash {
	t.Helper()
	tree := &object.Tree{}
	obj := r.Storer.NewEncodedObject()
	if err := tree.Encode(obj); err != nil {
		t.Fatal(err)
	}
	th, err := r.Storer.SetEncodedObject(obj)
	if err != nil {
		t.Fatal(err)
	}
	c := &object.Commit{Author: object.Signature{Name: "backup test", Email: "test@example.com", When: time.Now()}, Committer: object.Signature{Name: "test", Email: "test@example.com", When: time.Now()}, Message: time.Now().String(), TreeHash: th, ParentHashes: parents}
	obj = r.Storer.NewEncodedObject()
	if err := c.Encode(obj); err != nil {
		t.Fatal(err)
	}
	h, err := r.Storer.SetEncodedObject(obj)
	if err != nil {
		t.Fatal(err)
	}
	return h
}
func backupSet(t *testing.T, r *git.Repository, name string, h plumbing.Hash) {
	t.Helper()
	if err := r.Storer.SetReference(plumbing.NewHashReference(plumbing.ReferenceName(name), h)); err != nil {
		t.Fatal(err)
	}
}
func TestBackupLocalNeverDelete(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source.git")
	src, err := git.PlainInit(source, true)
	if err != nil {
		t.Fatal(err)
	}
	b := BackupTarget{Name: "test", Backend: "local", Source: source, Target: filepath.Join(dir, "backup with spaces.git")}
	first := backupCommit(t, src)
	backupSet(t, src, "refs/heads/main", first)
	backupSet(t, src, "refs/heads/keep", first)
	backupSet(t, src, "refs/tags/v1", first)
	sync := func() {
		t.Helper()
		if err := syncBackup(context.Background(), b); err != nil {
			t.Fatal(err)
		}
	}
	sync()
	dst, err := git.PlainOpen(b.Target)
	if err != nil {
		t.Fatal(err)
	}
	check := func(name string, want plumbing.Hash) {
		t.Helper()
		r, e := dst.Reference(plumbing.ReferenceName(name), true)
		if e != nil || r.Hash() != want {
			t.Fatalf("%s: %v %v want %s", name, r, e, want)
		}
	}
	check("refs/heads/main", first)
	check("refs/tags/v1", first)
	next := backupCommit(t, src, first)
	backupSet(t, src, "refs/heads/main", next)
	sync()
	check("refs/heads/main", next)
	if err := src.Storer.RemoveReference("refs/heads/keep"); err != nil {
		t.Fatal(err)
	}
	sync()
	check("refs/heads/keep", first)
	fork := backupCommit(t, dst, first)
	backupSet(t, dst, "refs/heads/main", fork)
	backupSet(t, src, "refs/heads/a-new", next)
	err = syncBackup(context.Background(), b)
	if err == nil || !strings.Contains(err.Error(), "diverged ref refs/heads/main") {
		t.Fatalf("expected loud divergence: %v", err)
	}
	check("refs/heads/main", fork)
	if _, err := dst.Reference("refs/heads/a-new", true); err != plumbing.ErrReferenceNotFound {
		t.Fatalf("batch changed before failure: %v", err)
	}
	backupSet(t, dst, "refs/heads/main", next)
	backupSet(t, src, "refs/tags/v1", next)
	if err := syncBackup(context.Background(), b); err == nil || !strings.Contains(err.Error(), "refs/tags/v1") {
		t.Fatalf("changed tag accepted: %v", err)
	}
	check("refs/tags/v1", first)
}
func TestBackupCommandsPersistAndRemoveOnlyConfig(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source.git")
	r, err := git.PlainInit(source, true)
	if err != nil {
		t.Fatal(err)
	}
	h := backupCommit(t, r)
	backupSet(t, r, "refs/heads/main", h)
	target := filepath.Join(dir, "target.git")
	run := func(args ...string) string {
		t.Helper()
		c := newBackupCmd()
		var out bytes.Buffer
		c.SetOut(&out)
		c.SetArgs(append([]string{"--data", dir}, args...))
		if err := c.Execute(); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	run("add", "--name", "test", "--source", source, "--target", target)
	if got := run("list"); !strings.Contains(got, target) {
		t.Fatal(got)
	}
	next := backupCommit(t, r, h)
	backupSet(t, r, "refs/heads/main", next)
	// New command instance reads persisted configuration, just as a restart does.
	run("sync", "test")
	dst, err := git.PlainOpen(target)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := dst.Reference("refs/heads/main", true)
	if err != nil || ref.Hash() != next {
		t.Fatal(ref, err)
	}
	run("rm", "test")
	if got := run("list"); got != "" {
		t.Fatal(got)
	}
	if _, err := os.Stat(filepath.Join(target, "objects")); err != nil {
		t.Fatal("rm deleted target", err)
	}
}

func TestBackupLocalLockedRefLeavesBatchUnchanged(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source.git")
	src, err := git.PlainInit(source, true)
	if err != nil {
		t.Fatal(err)
	}
	b := BackupTarget{Name: "test", Backend: "local", Source: source, Target: filepath.Join(dir, "target.git")}
	h := backupCommit(t, src)
	backupSet(t, src, "refs/heads/main", h)
	if err := syncBackup(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	next := backupCommit(t, src, h)
	backupSet(t, src, "refs/heads/main", next)
	backupSet(t, src, "refs/heads/a-new", next)
	lock := filepath.Join(b.Target, "refs", "heads", "main.lock")
	if err := os.WriteFile(lock, []byte("another writer"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := syncBackup(context.Background(), b); err == nil || !strings.Contains(err.Error(), "lock refs/heads/main") {
		t.Fatalf("expected lock failure: %v", err)
	}
	dst, err := git.PlainOpen(b.Target)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := dst.Reference("refs/heads/main", true)
	if err != nil || ref.Hash() != h {
		t.Fatal(ref, err)
	}
	if _, err := dst.Reference("refs/heads/a-new", true); err != plumbing.ErrReferenceNotFound {
		t.Fatalf("partial update: %v", err)
	}
	raw, err := os.ReadFile(lock)
	if err != nil || string(raw) != "another writer" {
		t.Fatal("removed another writer's lock")
	}
}

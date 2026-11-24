package shape

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

const pagePatch = "--- a/public/index.html\n+++ b/public/index.html\n@@ -1 +1 @@\n-<h1>Initial</h1>\n+<h1>From member</h1>\n"

func TestSourceTransferOwnerLine(t *testing.T) {
	svc, _, base, delegate := newScopedOwnerFixture(t, `{"scopes":{"shape":{"groups":["support"]}}}`)
	ctx := context.Background()
	member := Caller{Principal: "member@example.test", Tier: "member", Groups: []string{"support"}}
	owner := Caller{Principal: svc.Owner(), Tier: "owner"}
	checkPull := func(rev, content string) {
		t.Helper()
		snapshot, err := svc.PullSource(ctx, member)
		if err != nil || snapshot.Revision != rev {
			t.Fatalf("pull revision = %s, %v; want %s", snapshot.Revision, err, rev)
		}
		for _, f := range snapshot.Files {
			if f.Path == "public/index.html" {
				if string(f.Content) != content || f.Mode != "0100644" {
					t.Fatalf("pulled file = %+v", f)
				}
				return
			}
		}
		t.Fatal("pulled tree missing page")
	}
	// An uncommitted working-tree edit must never leak through a pull.
	page := filepath.Join(svc.App.SourceDir, "public", "index.html")
	if err := os.WriteFile(page, []byte("uncommitted"), 0600); err != nil {
		t.Fatal(err)
	}
	checkPull(base, "<h1>Initial</h1>\n")
	if err := os.WriteFile(page, []byte("<h1>Initial</h1>\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := svc.PushPatch(ctx, member, base, pagePatch)
	if err != nil || c.Status != Verified || c.Verdict == nil || !c.Verdict.Pass || c.Principal != member.Principal || c.ActivationPrincipal != svc.Owner() || c.BaseRev != base || c.Rev == base || delegate.calls != 0 {
		t.Fatalf("push = %+v, %v", c, err)
	}
	checkPull(base, "<h1>Initial</h1>\n")
	if _, err = svc.Approve(ctx, member, c.ID); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("member approve: %v", err)
	}
	approved, err := svc.Approve(ctx, owner, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	checkPull(approved.Rev, "<h1>From member</h1>\n")
	refused, err := svc.PushPatch(ctx, member, base, pagePatch)
	if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), base) || !strings.Contains(err.Error(), approved.Rev) || refused.ID != "" {
		t.Fatalf("stale push = %+v, %v", refused, err)
	}
	status, err := svc.Status(ctx, owner)
	if err != nil || len(status.Candidates) != 0 {
		t.Fatalf("owner queue after stale push = %+v, %v", status, err)
	}
}

func TestSourceTransferRefusalBeforeWrites(t *testing.T) {
	svc, home, base, _ := newScopedOwnerFixture(t, `{"scopes":{"shape":{"groups":["support"]}}}`)
	for _, c := range []Caller{
		{Principal: "member", Tier: "member", Groups: []string{"other"}},
		{Principal: "account", Tier: "account", Groups: []string{"support"}},
		{Principal: "anonymous", Tier: "anonymous"},
	} {
		if result, err := svc.PullSource(context.Background(), c); !errors.Is(err, ErrUnauthorized) || result.Revision != "" {
			t.Fatalf("%s pull = %+v, %v", c.Tier, result, err)
		}
		if result, err := svc.PushPatch(context.Background(), c, base, pagePatch); !errors.Is(err, ErrUnauthorized) || result.ID != "" {
			t.Fatalf("%s push = %+v, %v", c.Tier, result, err)
		}
	}
	for _, dir := range []string{"shape-work", "todo", "shape"} {
		if _, err := os.Stat(filepath.Join(home, dir)); !os.IsNotExist(err) {
			t.Fatalf("unexpected state at %s: %v", dir, err)
		}
	}
}

func TestSourcePatchFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "old"), []byte("old\n"), 0600); err != nil {
		t.Fatal(err)
	}
	diff := "--- a/old\n+++ /dev/null\n@@ -1 +0,0 @@\n-old\n--- /dev/null\n+++ b/nested/new\n@@ -0,0 +1 @@\n+added\n\\ No newline at end of file\n"
	if err := applySourcePatch(dir, diff); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "nested", "new")); err != nil || string(got) != "added" {
		t.Fatalf("created = %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "old")); !os.IsNotExist(err) {
		t.Fatalf("delete = %v", err)
	}
	for _, name := range []string{".git/config", "../outside", "/absolute"} {
		if err := applySourcePatch(dir, "--- /dev/null\n+++ b/"+name+"\n@@ -0,0 +1 @@\n+added\n"); err == nil {
			t.Fatalf("allowed patch outside source: %q", name)
		}
	}
}

func TestSourceTransferLimitsAndFailedVerification(t *testing.T) {
	svc, owner, _, verifier, _ := testService(t)
	ctx := context.Background()
	svc.App.Owner = owner.Principal
	owner.Tier = "owner"
	pulled, err := svc.PullSource(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	verifier.pass = false
	diff := "--- index.html\n+++ index.html\n@@ -1 +1 @@\n-Hello, World!\n\\ No newline at end of file\n+changed\n"
	c, err := svc.PushPatch(ctx, owner, pulled.Revision, diff)
	if !errors.Is(err, ErrVerification) || c.Status == Verified || verifier.calls != 1 {
		t.Fatalf("failed verifier = %+v, %v", c, err)
	}
	if c, err := svc.PushPatch(ctx, owner, pulled.Revision, strings.Repeat("x", maxTransferBytes+1)); err == nil || c.ID != "" {
		t.Fatal("oversize push created a candidate")
	}
	repo, err := gogit.PlainOpen(svc.App.SourceDir)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(svc.App.SourceDir, "large.bin"), make([]byte, maxTransferBytes), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := tree.Add("large.bin"); err != nil {
		t.Fatal(err)
	}
	if _, err := tree.Commit("large source", &gogit.CommitOptions{Author: &object.Signature{Name: "owner", Email: "owner@example.test", When: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	if got, err := svc.PullSource(ctx, owner); err == nil || !strings.Contains(err.Error(), "transfer limit") || len(got.Files) != 0 {
		t.Fatal("oversize pull returned partial source or missed the limit", err)
	}
}

func TestSourcePatchPreservesCRLF(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	const diff = "--- /dev/null\n+++ b/file\n@@ -0,0 +1,2 @@\n+first\r\n+second\r\n"
	if err := applySourcePatch(dir, diff); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "file")); err != nil || string(got) != "first\r\nsecond\r\n" {
		t.Fatalf("created file = %q, %v", got, err)
	}
}

func TestSourcePatchRejectsSymlink(t *testing.T) {
	dir, other := t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, filepath.Join(dir, "link")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := applySourcePatch(dir, "--- /dev/null\n+++ b/link/file\n@@ -0,0 +1 @@\n+added\n"); err == nil {
		t.Fatal("wrote through symlink")
	}
	if _, err := os.Stat(filepath.Join(other, "file")); !os.IsNotExist(err) {
		t.Fatalf("outside file: %v", err)
	}
}

// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package loom_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/qiangli/yoke/external/loom"
	ygit "github.com/qiangli/yoke/git"
	"github.com/qiangli/yoke/pkg/artifact"
	"github.com/qiangli/yoke/pkg/gomod"
)

func TestCheckRefMutationAllowed(t *testing.T) {
	h1 := plumbing.NewHash("1111111111111111111111111111111111111111")
	h2 := plumbing.NewHash("2222222222222222222222222222222222222222")
	pubRef := "refs/bashy/published/example.com/pkg/v1.0.0"

	if !loom.IsPublishedArtifactRef(plumbing.ReferenceName(pubRef)) {
		t.Fatalf("expected %s to be recognized as published artifact ref", pubRef)
	}
	if loom.IsPublishedArtifactRef("refs/heads/main") {
		t.Fatal("refs/heads/main must not be classified as published artifact ref")
	}
	if loom.IsPublishedArtifactRef(loom.PublishedRefPrefix) {
		t.Fatal("bare prefix without suffix must not be valid")
	}

	// Creation and idempotent no-op succeed.
	if err := loom.CheckRefMutationAllowed(pubRef, plumbing.ZeroHash, h1); err != nil {
		t.Fatalf("creation rejected: %v", err)
	}
	if err := loom.CheckRefMutationAllowed(pubRef, h1, h1); err != nil {
		t.Fatalf("idempotent no-op rejected: %v", err)
	}

	// Deletion and overwrite/move fail closed with ErrImmutablePublishedRef.
	if err := loom.CheckRefMutationAllowed(pubRef, h1, plumbing.ZeroHash); !errors.Is(err, loom.ErrImmutablePublishedRef) {
		t.Fatalf("deletion = %v, want ErrImmutablePublishedRef", err)
	}
	if err := loom.CheckRefMutationAllowed(pubRef, plumbing.ZeroHash, plumbing.ZeroHash); !errors.Is(err, loom.ErrImmutablePublishedRef) {
		t.Fatalf("zero->zero = %v, want ErrImmutablePublishedRef", err)
	}
	if err := loom.CheckRefMutationAllowed(pubRef, h1, h2); !errors.Is(err, loom.ErrImmutablePublishedRef) {
		t.Fatalf("rewrite = %v, want ErrImmutablePublishedRef", err)
	}

	// Normal branches may move, force-update, or be deleted.
	if err := loom.CheckRefMutationAllowed("refs/heads/main", h1, h2); err != nil {
		t.Fatalf("branch update rejected: %v", err)
	}
	if err := loom.CheckRefMutationAllowed("refs/heads/main", h1, plumbing.ZeroHash); err != nil {
		t.Fatalf("branch deletion rejected: %v", err)
	}
}

func TestPublishedRefsSurviveBranchRewriteDeleteAndBackupSync(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	artRoot := filepath.Join(tmp, "artifacts")
	loomBare := filepath.Join(tmp, "loom-source.git")
	backupBare := filepath.Join(tmp, "loom-backup.git")

	// 1. Create a working module repository, commit v1.0.0 source, and publish into loomBare + CAS.
	modWork := filepath.Join(tmp, "mod-work")
	if _, err := ygit.Init(ygit.InitOptions{Path: modWork, Quiet: true}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modWork, "go.mod"), []byte("module example.com/bashy/durablemod\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modWork, "lib.go"), []byte("package durablemod\n\nconst Token = \"pinned-forever\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ygit.Add(ygit.AddOptions{RepoPath: modWork, All: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := ygit.Commit(ygit.CommitOptions{
		RepoPath:    modWork,
		Message:     "release v1.0.0",
		AuthorName:  "tester",
		AuthorEmail: "tester@local.invalid",
	}); err != nil {
		t.Fatal(err)
	}

	pub, err := gomod.PublishArtifact(ctx, gomod.PublishOptions{
		Dir:          modWork,
		Version:      "v1.0.0",
		ArtifactRoot: artRoot,
		LoomRepoDir:  loomBare,
	})
	if err != nil {
		t.Fatalf("PublishArtifact: %v", err)
	}

	srcRepo, err := git.PlainOpen(loomBare)
	if err != nil {
		t.Fatalf("PlainOpen loomBare: %v", err)
	}
	pubHash := plumbing.NewHash(pub.Commit)

	// Point refs/heads/main and refs/heads/legacy at pubHash initially, then rewrite
	// refs/heads/main to a completely unrelated orphan commit and delete refs/heads/legacy!
	if err := srcRepo.Storer.SetReference(plumbing.NewHashReference("refs/heads/main", pubHash)); err != nil {
		t.Fatal(err)
	}
	if err := srcRepo.Storer.SetReference(plumbing.NewHashReference("refs/heads/legacy", pubHash)); err != nil {
		t.Fatal(err)
	}

	orphanBlobObj := srcRepo.Storer.NewEncodedObject()
	orphanBlobObj.SetType(plumbing.BlobObject)
	w, err := orphanBlobObj.Writer()
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte("unrelated history\n"))
	_ = w.Close()
	orphanBlobHash, err := srcRepo.Storer.SetEncodedObject(orphanBlobObj)
	if err != nil {
		t.Fatal(err)
	}

	orphanTree := &object.Tree{
		Entries: []object.TreeEntry{{
			Name: "README.md",
			Mode: 0o100644,
			Hash: orphanBlobHash,
		}},
	}
	treeObj := srcRepo.Storer.NewEncodedObject()
	if err := orphanTree.Encode(treeObj); err != nil {
		t.Fatal(err)
	}
	orphanTreeHash, err := srcRepo.Storer.SetEncodedObject(treeObj)
	if err != nil {
		t.Fatal(err)
	}
	orphanCommit := &object.Commit{
		Author:    object.Signature{Name: "tester", Email: "tester@local.invalid"},
		Committer: object.Signature{Name: "tester", Email: "tester@local.invalid"},
		Message:   "unrelated orphan main",
		TreeHash:  orphanTreeHash,
	}
	commitObj := srcRepo.Storer.NewEncodedObject()
	if err := orphanCommit.Encode(commitObj); err != nil {
		t.Fatal(err)
	}
	orphanHash, err := srcRepo.Storer.SetEncodedObject(commitObj)
	if err != nil {
		t.Fatal(err)
	}

	// Move main to orphanHash (not a descendant of pubHash!) and delete legacy branch.
	if err := srcRepo.Storer.SetReference(plumbing.NewHashReference("refs/heads/main", orphanHash)); err != nil {
		t.Fatal(err)
	}
	if err := srcRepo.Storer.RemoveReference("refs/heads/legacy"); err != nil {
		t.Fatal(err)
	}

	// If host git is available, run `git gc --prune=now` on loomBare to prove Git GC
	// keeps objects reachable from refs/bashy/published/* even when no branch reaches them!
	if hostGit, lookErr := exec.LookPath("git"); lookErr == nil {
		gcCmd := exec.Command(hostGit, "--git-dir", loomBare, "gc", "--prune=now")
		if out, gcErr := gcCmd.CombinedOutput(); gcErr != nil {
			t.Fatalf("git gc --prune=now failed: %v\n%s", gcErr, string(out))
		}
	}

	// Re-open after potential packfile GC and verify source published commit/tree/blobs are intact.
	if err := loom.VerifyPublishedCommitsIntact(loomBare, []artifact.PublicationRecord{pub.Record}); err != nil {
		t.Fatalf("VerifyPublishedCommitsIntact(source): %v", err)
	}

	// 2. Sync to local backup bare repository via `loom backup add` CLI / localBackup.Sync!
	loomDataDir := filepath.Join(tmp, "loom-data")
	backupCmd := loom.NewLoomCmd()
	backupCmd.SetArgs([]string{"backup", "--data", loomDataDir, "add", "--name", "primary", "--source", loomBare, "--target", backupBare})
	if err := backupCmd.Execute(); err != nil {
		t.Fatalf("loom backup add: %v", err)
	}

	// Verify both refs/bashy/published/* AND its full commit/tree/blobs were backed up!
	if err := loom.VerifyPublishedCommitsIntact(backupBare, []artifact.PublicationRecord{pub.Record}); err != nil {
		t.Fatalf("VerifyPublishedCommitsIntact(backup): %v", err)
	}

	// Idempotent re-sync succeeds when published ref is unchanged.
	syncCmd := loom.NewLoomCmd()
	syncCmd.SetArgs([]string{"backup", "--data", loomDataDir, "sync", "primary"})
	if err := syncCmd.Execute(); err != nil {
		t.Fatalf("idempotent loom backup sync: %v", err)
	}

	// 3. Mutating refs/bashy/published/* in source (even to a descendant commit!) MUST fail backupPlan/Sync closed
	// without modifying any ref in backupBare!
	srcRepoReloaded, err := git.PlainOpen(loomBare)
	if err != nil {
		t.Fatal(err)
	}
	childCommit := &object.Commit{
		Author:       object.Signature{Name: "tester", Email: "tester@local.invalid"},
		Committer:    object.Signature{Name: "tester", Email: "tester@local.invalid"},
		Message:      "attempted fast-forward of immutable published ref",
		TreeHash:     orphanTreeHash,
		ParentHashes: []plumbing.Hash{pubHash},
	}
	childObj := srcRepoReloaded.Storer.NewEncodedObject()
	if err := childCommit.Encode(childObj); err != nil {
		t.Fatal(err)
	}
	childHash, err := srcRepoReloaded.Storer.SetEncodedObject(childObj)
	if err != nil {
		t.Fatal(err)
	}
	if err := srcRepoReloaded.Storer.SetReference(plumbing.NewHashReference(plumbing.ReferenceName(pub.LoomRef), childHash)); err != nil {
		t.Fatal(err)
	}
	if err := srcRepoReloaded.Storer.SetReference(plumbing.NewHashReference("refs/heads/canary", orphanHash)); err != nil {
		t.Fatal(err)
	}

	failSyncCmd := loom.NewLoomCmd()
	failSyncCmd.SetArgs([]string{"backup", "--data", loomDataDir, "sync", "primary"})
	syncErr := failSyncCmd.Execute()
	if syncErr == nil || !strings.Contains(syncErr.Error(), "diverged ref "+pub.LoomRef) {
		t.Fatalf("expected backup sync failure on mutated published ref, got: %v", syncErr)
	}

	// Verify backupBare was NOT modified at all (atomic batch pre-validation).
	if err := loom.VerifyPublishedCommitsIntact(backupBare, []artifact.PublicationRecord{pub.Record}); err != nil {
		t.Fatalf("backup target corrupted after failed sync: %v", err)
	}
	dstRepo, err := git.PlainOpen(backupBare)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dstRepo.Reference("refs/heads/canary", true); !errors.Is(err, plumbing.ErrReferenceNotFound) {
		t.Fatalf("unrelated ref written despite batch failure: %v", err)
	}
}

func TestInstallPublishedRefProtectionHookRejectsForceAndDeletePush(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX hooks/update execution via host git covered on unix; Windows uses MinGit in live loom")
	}
	hostGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git binary not on PATH")
	}

	tmp := t.TempDir()
	bareDir := filepath.Join(tmp, "remote.git")
	if _, err := git.PlainInit(bareDir, true); err != nil {
		t.Fatal(err)
	}

	// Pre-install a custom hooks/update script to verify clean chaining via hooks/update.d/!
	markerFile := filepath.Join(tmp, "chained-hook-ran.txt")
	legacyHook := "#!/bin/sh\necho \"$1\" >> '" + markerFile + "'\nexit 0\n"
	if err := os.MkdirAll(filepath.Join(bareDir, "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bareDir, "hooks", "update"), []byte(legacyHook), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := loom.InstallPublishedRefProtectionHook(bareDir); err != nil {
		t.Fatalf("InstallPublishedRefProtectionHook: %v", err)
	}
	// Idempotent second call must preserve chained hook.
	if err := loom.InstallPublishedRefProtectionHook(bareDir); err != nil {
		t.Fatalf("InstallPublishedRefProtectionHook (idempotent): %v", err)
	}

	workDir := filepath.Join(tmp, "work")
	runGit := func(args ...string) (string, error) {
		c := exec.Command(hostGit, args...)
		c.Dir = workDir
		c.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=tester",
			"GIT_AUTHOR_EMAIL=tester@local.invalid",
			"GIT_COMMITTER_NAME=tester",
			"GIT_COMMITTER_EMAIL=tester@local.invalid",
		)
		out, runErr := c.CombinedOutput()
		return string(out), runErr
	}

	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := runGit("init", "-b", "main"); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	if out, err := runGit("remote", "add", "origin", bareDir); err != nil {
		t.Fatalf("git remote add: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(workDir, "go.mod"), []byte("module example.com/hooktest\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := runGit("add", "go.mod"); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	if out, err := runGit("commit", "-m", "c1"); err != nil {
		t.Fatalf("git commit c1: %v\n%s", err, out)
	}
	c1Out, err := runGit("rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	c1 := strings.TrimSpace(c1Out)

	pubRef := "refs/bashy/published/example.com/hooktest/v1.0.0"

	// 1. Initial creation of refs/bashy/published/* and refs/heads/main succeeds!
	if out, err := runGit("push", "origin", "HEAD:refs/heads/main", "HEAD:"+pubRef); err != nil {
		t.Fatalf("initial push failed: %v\n%s", err, out)
	}

	// Verify chained pre-existing hook ALSO ran!
	chainedLog, err := os.ReadFile(markerFile)
	if err != nil || !strings.Contains(string(chainedLog), pubRef) {
		t.Fatalf("chained legacy update hook did not run: err=%v log=%q", err, string(chainedLog))
	}

	// 2. Create commit c2; pushing --force to refs/heads/main succeeds, while pushing --force or fast-forward to pubRef FAILS CLOSED!
	if err := os.WriteFile(filepath.Join(workDir, "go.mod"), []byte("module example.com/hooktest\n\ngo 1.23\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := runGit("commit", "-am", "c2"); err != nil {
		t.Fatalf("git commit c2: %v\n%s", err, out)
	}

	if out, err := runGit("push", "--force", "origin", "HEAD:"+pubRef); err == nil {
		t.Fatalf("expected force push to %s to be rejected, but succeeded:\n%s", pubRef, out)
	}
	if out, err := runGit("push", "--delete", "origin", pubRef); err == nil {
		t.Fatalf("expected delete push to %s to be rejected, but succeeded:\n%s", pubRef, out)
	}

	// Branch force-update and branch deletion still succeed freely!
	if out, err := runGit("push", "origin", "HEAD:refs/heads/temp-branch"); err != nil {
		t.Fatalf("push temp-branch: %v\n%s", err, out)
	}
	if out, err := runGit("push", "--delete", "origin", "refs/heads/temp-branch"); err != nil {
		t.Fatalf("delete temp-branch: %v\n%s", err, out)
	}
	if out, err := runGit("checkout", "--orphan", "rewritten"); err != nil {
		t.Fatalf("checkout orphan: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(workDir, "other.txt"), []byte("new root\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := runGit("add", "other.txt"); err != nil {
		t.Fatalf("git add other.txt: %v\n%s", err, out)
	}
	if out, err := runGit("commit", "-m", "orphan"); err != nil {
		t.Fatalf("git commit orphan: %v\n%s", err, out)
	}
	if out, err := runGit("push", "--force", "origin", "HEAD:refs/heads/main"); err != nil {
		t.Fatalf("force-push main to orphan failed: %v\n%s", err, out)
	}

	// Confirm pubRef in bareDir still points to c1 and its tree/blobs remain intact!
	bareRepo, err := git.PlainOpen(bareDir)
	if err != nil {
		t.Fatal(err)
	}
	gotRef, err := bareRepo.Reference(plumbing.ReferenceName(pubRef), true)
	if err != nil || gotRef.Hash().String() != c1 {
		t.Fatalf("pubRef = %v (%v), want %s", gotRef, err, c1)
	}
}

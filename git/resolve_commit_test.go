package git

import "testing"

// ResolveCommit peels an annotated tag to its commit; HEAD and a lightweight
// tag resolve directly.
func TestResolveCommitPeelsAnnotatedTag(t *testing.T) {
	isolateGlobalConfig(t)
	dir := makeTwoCommitRepo(t)
	setLocalIdentity(t, dir)
	if _, err := TagCreate(TagOptions{RepoPath: dir, Name: "v0.1.0", Commit: "HEAD~1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := TagCreate(TagOptions{RepoPath: dir, Name: "v0.2.0", Message: "release"}); err != nil {
		t.Fatal(err)
	}
	head, err := ResolveCommit(dir, "HEAD")
	if err != nil || len(head) != 40 {
		t.Fatalf("HEAD = %q, %v", head, err)
	}
	if got, err := ResolveCommit(dir, "refs/tags/v0.2.0"); err != nil || got != head {
		t.Fatalf("annotated tag = %q, %v; want HEAD %s", got, err, head)
	}
	parent, _ := ResolveCommit(dir, "HEAD~1")
	if got, err := ResolveCommit(dir, "refs/tags/v0.1.0"); err != nil || got != parent {
		t.Fatalf("lightweight tag = %q, %v; want %s", got, err, parent)
	}
	if _, err := ResolveCommit(dir, "refs/tags/v9.9.9"); err == nil {
		t.Fatal("missing tag resolved")
	}
}

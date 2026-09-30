package git

import (
	"os"
	"path/filepath"
	"testing"

	gogit "github.com/go-git/go-git/v5"
)

// A checkout with no repo-local user.name/user.email must take the
// identity from ~/.gitconfig, like real git (sprint merge into a plain
// developer checkout failed with "user identity not configured").
func TestCommitSignatureFallsBackToGlobalConfig(t *testing.T) {
	isolateGlobalConfig(t)
	home := os.Getenv("HOME")
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[user]\n\tname = Global Person\n\temail = global@example.test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := makeTwoCommitRepo(t)
	r, err := gogit.PlainOpen(dir)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := commitSignature(r)
	if err != nil {
		t.Fatalf("commitSignature: %v", err)
	}
	if sig.Name != "Global Person" || sig.Email != "global@example.test" {
		t.Fatalf("signature = %s <%s>", sig.Name, sig.Email)
	}
	setLocalIdentity(t, dir)
	r, _ = gogit.PlainOpen(dir)
	if sig, err = commitSignature(r); err != nil || sig.Name != "Merge Tester" {
		t.Fatalf("repo-local identity must win: %v %v", sig, err)
	}
}

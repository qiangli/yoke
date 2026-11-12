package git

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
)

func TestNativeCheckoutDetached(t *testing.T) {
	for _, tc := range []struct {
		name     string
		target   string
		flags    []string
		dirty    bool
		populate bool
		refuse   bool
		quiet    bool
	}{
		{name: "full SHA", target: "full"},
		{name: "short SHA", target: "short"},
		{name: "tag", target: "tag"},
		{name: "revision", target: "HEAD~1"},
		{name: "dirty refused", target: "full", dirty: true, refuse: true},
		{name: "quiet dirty refused", target: "full", flags: []string{"-q"}, dirty: true, refuse: true},
		{name: "force", target: "full", flags: []string{"-f"}, dirty: true},
		{name: "quiet", target: "full", flags: []string{"-q"}, quiet: true},
		{name: "long quiet", target: "short", flags: []string{"--quiet"}, quiet: true},
		{name: "quiet force", target: "full", flags: []string{"-q", "--force"}, dirty: true, quiet: true},
		{name: "force quiet", target: "full", flags: []string{"-f", "--quiet"}, dirty: true, quiet: true},
		{name: "populate", target: "full", populate: true},
		{name: "branch", target: "branch"},
		{name: "quiet branch", target: "branch", flags: []string{"-q"}, quiet: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := setupTestRepo(t)
			repo, err := openRepo(dir)
			if err != nil {
				t.Fatal(err)
			}
			first, err := repo.Head()
			if err != nil {
				t.Fatal(err)
			}
			branch := plumbing.NewBranchReferenceName("topic")
			if err := repo.Storer.SetReference(plumbing.NewHashReference(branch, first.Hash())); err != nil {
				t.Fatal(err)
			}
			if _, err := repo.CreateTag("v1", first.Hash(), nil); err != nil {
				t.Fatal(err)
			}
			commitFiles(t, dir, map[string]string{"hello.txt": "second version\n"}, "second commit")
			oldHead, err := repo.Storer.Reference(plumbing.HEAD)
			if err != nil {
				t.Fatal(err)
			}
			indexPath := filepath.Join(dir, ".git", "index")
			indexBefore, err := os.ReadFile(indexPath)
			if err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(dir, "hello.txt")
			if tc.dirty {
				if err := os.WriteFile(file, []byte("local changes\n"), 0644); err != nil {
					t.Fatal(err)
				}
			}
			if tc.populate {
				if err := os.Remove(file); err != nil {
					t.Fatal(err)
				}
			}
			target := tc.target
			switch target {
			case "full":
				target = first.Hash().String()
			case "short":
				target = first.Hash().String()[:8]
			case "tag":
				target = "v1"
			case "branch":
				target = "topic"
			}
			args := append([]string{"checkout"}, tc.flags...)
			res, err := Exec(context.Background(), dir, append(args, target))
			if tc.refuse {
				if !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), "dirty worktree") {
					t.Fatalf("expected dirty refusal, got %+v, %v", res, err)
				}
				head, headErr := repo.Storer.Reference(plumbing.HEAD)
				if headErr != nil || head.String() != oldHead.String() {
					t.Fatalf("HEAD changed: %v, %v", head, headErr)
				}
				after, readErr := os.ReadFile(indexPath)
				if readErr != nil || !bytes.Equal(after, indexBefore) {
					t.Fatalf("index changed on refusal: %v", readErr)
				}
				content, readErr := os.ReadFile(file)
				if readErr != nil || string(content) != "local changes\n" {
					t.Fatalf("local edits lost: %q, %v", content, readErr)
				}
				return
			}
			if err != nil || res == nil || res.ExitCode != 0 {
				t.Fatalf("checkout: %+v, %v", res, err)
			}
			head, err := repo.Head()
			if err != nil || head.Hash() != first.Hash() {
				t.Fatalf("HEAD = %v, %v; want %s", head, err, first.Hash())
			}
			rawHead, err := repo.Storer.Reference(plumbing.HEAD)
			if err != nil {
				t.Fatal(err)
			}
			if tc.target == "branch" {
				if rawHead.Type() != plumbing.SymbolicReference || rawHead.Target() != branch {
					t.Fatalf("branch checkout detached: %s", rawHead)
				}
				if !tc.quiet && res.Stdout != "Switched to branch 'topic'\n" {
					t.Fatalf("branch output changed: %+v", res)
				}
			} else {
				if rawHead.Type() != plumbing.HashReference {
					t.Fatalf("HEAD not detached: %s", rawHead)
				}
				if !tc.quiet && (res.Stdout != "" || res.Stderr != "HEAD is now at "+first.Hash().String()[:7]+" initial commit\n") {
					t.Fatalf("detached output: %+v", res)
				}
			}
			if tc.quiet && (res.Stdout != "" || res.Stderr != "") {
				t.Fatalf("quiet checkout printed: %+v", res)
			}
			content, err := os.ReadFile(file)
			if err != nil || string(content) != "hello world\n" {
				t.Fatalf("worktree = %q, %v", content, err)
			}
		})
	}
}

func TestNativeCheckoutDetachedFailureRestoresIndexAndHead(t *testing.T) {
	dir := setupTestRepo(t)
	commitFiles(t, dir, map[string]string{"sub/file": "new file\n"}, "second commit")
	repo, err := openRepo(dir)
	if err != nil {
		t.Fatal(err)
	}
	target, err := repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RunChecked(context.Background(), dir, []string{"reset", "--hard", "HEAD~1"}); err != nil {
		t.Fatal(err)
	}
	oldHead, err := repo.Storer.Reference(plumbing.HEAD)
	if err != nil {
		t.Fatal(err)
	}
	indexPath := filepath.Join(dir, ".git", "index")
	before, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "sub", "file", "blocker"), 0755); err != nil {
		t.Fatal(err)
	}
	res, err := Exec(context.Background(), dir, []string{"checkout", "-f", target.Hash().String()})
	if !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), "worktree checkout failed") {
		t.Fatalf("expected checkout failure, got %+v, %v", res, err)
	}
	after, err := os.ReadFile(indexPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("index not restored: %v", err)
	}
	head, err := repo.Storer.Reference(plumbing.HEAD)
	if err != nil || head.String() != oldHead.String() {
		t.Fatalf("HEAD not restored: %v, %v", head, err)
	}
}

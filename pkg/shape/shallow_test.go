package shape

import (
	"bytes"
	"context"
	"errors"
	shapeverify "github.com/qiangli/yoke/pkg/shape/verify"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestShallowSourceRefusedBeforeCandidateOrTracking(t *testing.T) {
	source := t.TempDir()
	run := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid", "GIT_COMMITTER_NAME=fixture", "GIT_COMMITTER_EMAIL=fixture@example.invalid")
		out, e := cmd.CombinedOutput()
		if e != nil {
			t.Fatalf("git %q: %v %s", args, e, out)
		}
		return strings.TrimSpace(string(out))
	}
	run(source, "init")
	for _, body := range []string{"base", "tip"} {
		os.WriteFile(filepath.Join(source, "message.txt"), []byte(body), 0600)
		run(source, "add", ".")
		run(source, "commit", "-m", body)
	}
	shallow := filepath.Join(t.TempDir(), "shallow")
	run(source, "clone", "--no-local", "--depth=1", source, shallow)
	rev := run(shallow, "rev-parse", "HEAD")
	run(shallow, "branch", "pristine", rev)

	boundary, e := os.ReadFile(filepath.Join(shallow, ".git", "shallow"))
	if e != nil {
		t.Fatal(e)
	}
	config, e := os.ReadFile(filepath.Join(shallow, ".git", "config"))
	if e != nil {
		t.Fatal(e)
	}
	destination := filepath.Join(t.TempDir(), "work")
	if _, e = CloneAt(context.Background(), shallow, destination, rev); !errors.Is(e, shapeverify.ErrShallowSource) {
		t.Fatalf("shallow clone error: %v", e)
	}
	if _, e = os.Stat(destination); !os.IsNotExist(e) {
		t.Fatal("refused clone created workspace", e)
	}
	if _, e = shapeverify.IsolatedClone(shallow, rev); !errors.Is(e, shapeverify.ErrShallowSource) {
		t.Fatalf("direct verifier bypassed precondition: %v", e)
	}
	tracks := 0
	workRoot := t.TempDir()
	svc := Service{App: App{Name: "shallow", SourceDir: shallow}, Options: Options{Home: t.TempDir(), WorkRoot: workRoot, Track: func(context.Context, *Candidate) error { tracks++; return nil }}}
	if _, e = svc.Begin(context.Background(), Caller{Principal: "owner", Actor: "authenticated-owner"}, Wish{Request: "shape shallow source"}); !errors.Is(e, shapeverify.ErrShallowSource) {
		t.Fatal(e)
	}
	if tracks != 0 {
		t.Fatal("shallow refusal wrote tracking")
	}
	if _, e = os.Stat(filepath.Join(workRoot, "shallow")); !os.IsNotExist(e) {
		t.Fatal("shallow refusal created candidate", e)
	}
	after, _ := os.ReadFile(filepath.Join(shallow, ".git", "shallow"))
	afterConfig, _ := os.ReadFile(filepath.Join(shallow, ".git", "config"))
	if !bytes.Equal(after, boundary) || !bytes.Equal(afterConfig, config) || run(shallow, "rev-parse", "HEAD") != rev || run(shallow, "status", "--porcelain") != "" {
		t.Fatal("shallow source mutated or implicitly fetched")
	}
	full := filepath.Join(t.TempDir(), "full-work")
	cloned, e := CloneAt(context.Background(), source, full, rev)
	if e != nil {
		t.Fatalf("complete source refused: %v", e)
	}
	head, e := cloned.Head()
	if e != nil || head.Hash().String() != rev {
		t.Fatal("complete clone changed revision", e)
	}
}

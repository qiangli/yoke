package feed

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitTestEnv(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available")
	}
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("BASHY_HOME", filepath.Join(root, "bashyhome"))
	t.Setenv("GIT_AUTHOR_NAME", "Feed Test")
	t.Setenv("GIT_AUTHOR_EMAIL", "feed@test.local")
	t.Setenv("GIT_COMMITTER_NAME", "Feed Test")
	t.Setenv("GIT_COMMITTER_EMAIL", "feed@test.local")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	return root
}

func runGitTest(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func TestGitSinkPutGetLocalBare(t *testing.T) {
	root := gitTestEnv(t)
	ctx := context.Background()
	remote := filepath.Join(root, "remote.git")
	runGitTest(t, "init", "--bare", remote)

	s, err := OpenSink(SinkConfig{Name: "g1", Kind: "github", Target: remote})
	if err != nil {
		t.Fatalf("OpenSink: %v", err)
	}
	if s.Kind() != "github" || s.Name() != "g1" {
		t.Fatalf("sink = %q/%q", s.Kind(), s.Name())
	}

	data := []byte(`{"headline":"hi"}`)
	url, err := s.Put(ctx, "posts/hello.json", data)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	wantURL := remote + "#main:posts/hello.json"
	if url != wantURL {
		t.Fatalf("Put URL = %q, want %q", url, wantURL)
	}

	// The commit really landed on the remote branch with the right message.
	got := runGitTest(t, "--git-dir="+remote, "show", "main:posts/hello.json")
	if got != string(data) {
		t.Fatalf("remote content = %q, want %q", got, data)
	}
	msg := strings.TrimSpace(runGitTest(t, "--git-dir="+remote, "log", "--format=%s", "-1", "main"))
	if msg != "feed: post posts/hello.json" {
		t.Fatalf("commit message = %q", msg)
	}

	// Get works via relPath and via the URL Put returned.
	for _, ref := range []string{"posts/hello.json", url} {
		back, err := s.Get(ctx, ref)
		if err != nil {
			t.Fatalf("Get(%q): %v", ref, err)
		}
		if !bytes.Equal(back, data) {
			t.Fatalf("Get(%q) = %q, want %q", ref, back, data)
		}
	}

	// Re-putting identical content is a no-op success (no empty commit, push ok).
	if _, err := s.Put(ctx, "posts/hello.json", data); err != nil {
		t.Fatalf("Put identical: %v", err)
	}

	// Updated content commits and pushes.
	data2 := []byte(`{"headline":"hi2"}`)
	if _, err := s.Put(ctx, "posts/hello.json", data2); err != nil {
		t.Fatalf("Put update: %v", err)
	}
	back, err := s.Get(ctx, "posts/hello.json")
	if err != nil || !bytes.Equal(back, data2) {
		t.Fatalf("Get after update = %q,%v", back, err)
	}
}

func TestGitSinkBranchDefault(t *testing.T) {
	gitTestEnv(t)
	s, err := OpenSink(SinkConfig{Name: "g", Kind: "github", Target: "/tmp/x.git", Branch: ""})
	if err != nil {
		t.Fatal(err)
	}
	if gs, ok := s.(*gitSink); !ok || gs.branch != "main" {
		t.Fatalf("default branch not applied: %#v", s)
	}
}

func TestGitSinkGetMissing(t *testing.T) {
	root := gitTestEnv(t)
	remote := filepath.Join(root, "remote.git")
	runGitTest(t, "init", "--bare", remote)
	s, err := OpenSink(SinkConfig{Name: "g", Kind: "github", Target: remote})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(context.Background(), "posts/a.json", []byte("a")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := s.Get(context.Background(), "posts/nope.json"); err == nil {
		t.Fatal("Get missing: nil error")
	}
}

func TestGitSinkRejectsTraversal(t *testing.T) {
	gitTestEnv(t)
	s, err := OpenSink(SinkConfig{Name: "g", Kind: "github", Target: "/tmp/x.git"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(context.Background(), "../evil", []byte("x")); err == nil {
		t.Fatal("Put ../evil: nil error")
	}
	if _, err := s.Get(context.Background(), "../evil"); err == nil {
		t.Fatal("Get ../evil: nil error")
	}
}

func TestBuildGitURL(t *testing.T) {
	cases := []struct {
		remote string
		branch string
		rel    string
		want   string
	}{
		{"https://github.com/owner/repo.git", "main", "p/a.json",
			"https://raw.githubusercontent.com/owner/repo/main/p/a.json"},
		{"https://github.com/owner/repo", "pages", "p/a.json",
			"https://raw.githubusercontent.com/owner/repo/pages/p/a.json"},
		{"git@github.com:owner/repo.git", "main", "p/a.json",
			"https://raw.githubusercontent.com/owner/repo/main/p/a.json"},
		{"ssh://git@github.com/owner/repo.git", "main", "p/a.json",
			"https://raw.githubusercontent.com/owner/repo/main/p/a.json"},
		{"/tmp/remote.git", "main", "p/a.json",
			"/tmp/remote.git#main:p/a.json"},
		{"git@example.com:feed.git", "main", "p/a.json",
			"git@example.com:feed.git#main:p/a.json"},
	}
	for _, c := range cases {
		if got := buildGitURL(c.remote, c.branch, c.rel); got != c.want {
			t.Errorf("buildGitURL(%q) = %q, want %q", c.remote, got, c.want)
		}
	}
}

func TestGitSinkCacheUnderBashyHome(t *testing.T) {
	root := gitTestEnv(t)
	remote := filepath.Join(root, "remote.git")
	runGitTest(t, "init", "--bare", remote)
	s, err := OpenSink(SinkConfig{Name: "g1", Kind: "github", Target: remote})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(context.Background(), "p/a.json", []byte("a")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	gs := s.(*gitSink)
	wantDir := filepath.Join(root, "bashyhome", "feed", "sinks", "g1")
	if gs.dir != wantDir {
		t.Fatalf("cache dir = %q, want %q", gs.dir, wantDir)
	}
	if _, err := os.Stat(filepath.Join(wantDir, ".git")); err != nil {
		t.Fatalf("cache clone missing: %v", err)
	}
}

func TestGitSinkPublicURL(t *testing.T) {
	root := gitTestEnv(t)
	ctx := context.Background()
	remote := filepath.Join(root, "remote.git")
	runGitTest(t, "init", "--bare", remote)

	s, err := OpenSink(SinkConfig{Name: "g1", Kind: "github", Target: remote, PublicURL: "https://example.test/feed/"})
	if err != nil {
		t.Fatal(err)
	}
	data := []byte(`{"headline":"hi"}`)
	url, err := s.Put(ctx, "posts/hello.json", data)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if want := "https://example.test/feed/posts/hello.json"; url != want {
		t.Fatalf("Put URL = %q, want %q", url, want)
	}
	back, err := s.Get(ctx, url)
	if err != nil || !bytes.Equal(back, data) {
		t.Fatalf("Get(public URL) = %q,%v", back, err)
	}
}

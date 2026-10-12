package feed

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGroupNameResolution(t *testing.T) {
	te := newTestEnv(t, &fakeExcerpter{answers: []Excerpt{goodExcerpt}})
	// Server with two groups same name dup vs unique
	groups := []FeedGroup{
		{ID: "g1", Name: "dup", Read: "members", Epoch: 1},
		{ID: "g2", Name: "dup", Read: "members", Epoch: 1},
		{ID: "g3", Name: "unique", Read: "anyone", Epoch: 0},
		{ID: "g-public-real", Name: "pubname", Read: "anyone", Epoch: 0},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/feed/groups", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(groups)
	})
	mux.HandleFunc("/api/v1/feed/groups/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		id := strings.TrimPrefix(r.URL.Path, "/api/v1/feed/groups/")
		for _, g := range groups {
			if g.ID == id {
				_ = json.NewEncoder(w).Encode(g)
				return
			}
		}
		w.WriteHeader(404)
	})
	mux.HandleFunc("/api/v1/feed/enckey/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	te.env.Cloud = func() (string, string) { return server.URL, "test-token" }
	client, err := NewGroupClient(te.env)
	if err != nil {
		t.Fatal(err)
	}
	// ambiguous name
	if _, err := client.ResolveGroup(context.Background(), "dup"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("expected ambiguous error, got %v", err)
	}
	// not found
	if _, err := client.ResolveGroup(context.Background(), "nope"); err == nil || !strings.Contains(err.Error(), "no group") {
		t.Fatalf("expected no group error, got %v", err)
	}
	// unique by name
	g, err := client.ResolveGroup(context.Background(), "unique")
	if err != nil || g.ID != "g3" {
		t.Fatalf("resolve unique: %v %v", g, err)
	}
	// resolve by ID still works
	g, err = client.ResolveGroup(context.Background(), "g3")
	if err != nil || g.ID != "g3" {
		t.Fatalf("resolve by id: %v %v", g, err)
	}
	// resolve pubname via post integration: sink add + post --group pubname should store group id
	sinkDir := t.TempDir()
	te.mustRun("sink", "add", "rclone", "local", sinkDir)
	// Override server for post: need groups endpoint to include pubname target, GetGroup for it, and registry
	postGroups := []FeedGroup{{ID: "g-public-real", Name: "pubname", Read: "anyone", Epoch: 0}}
	server2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && r.URL.Path == "/api/v1/feed/groups":
			_ = json.NewEncoder(w).Encode(postGroups)
		case r.Method == "GET" && r.URL.Path == "/api/v1/feed/groups/g-public-real":
			_ = json.NewEncoder(w).Encode(postGroups[0])
		case r.Method == "POST" && r.URL.Path == "/api/v1/feed/registry":
			_ = json.NewEncoder(w).Encode(IndexResponse{Indexed: "ok"})
		default:
			w.WriteHeader(404)
		}
	}))
	defer server2.Close()
	te.env.Cloud = func() (string, string) { return server2.URL, "test-token" }
	te.mustRun("post", writeBody(t, "hello via name"), "--headline", "via name", "--dek", "dek", "--group", "pubname", "--no-index")
	raw, err := os.ReadFile(firstPostFile(t, sinkDir))
	if err != nil {
		t.Fatal(err)
	}
	post, err := ParsePost(raw)
	if err != nil {
		t.Fatal(err)
	}
	if post.Group != "g-public-real" {
		t.Fatalf("post group via name = %q, want g-public-real", post.Group)
	}
	// Ambiguous via CLI should fail clearly
	ambGroups := []FeedGroup{{ID: "a1", Name: "same", Read: "anyone"}, {ID: "a2", Name: "same", Read: "anyone"}}
	server3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/feed/groups":
			_ = json.NewEncoder(w).Encode(ambGroups)
		case strings.HasPrefix(r.URL.Path, "/api/v1/feed/groups/"):
			for _, g := range ambGroups {
				if r.URL.Path == "/api/v1/feed/groups/"+g.ID {
					_ = json.NewEncoder(w).Encode(g)
					return
				}
			}
			w.WriteHeader(404)
		default:
			w.WriteHeader(404)
		}
	}))
	defer server3.Close()
	te.env.Cloud = func() (string, string) { return server3.URL, "test-token" }
	_, _, err = te.run("post", writeBody(t, "body"), "--headline", "h", "--group", "same")
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("expected ambiguous via post, got %v", err)
	}
	_, _, err = te.run("group", "add", "same", "other")
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("expected ambiguous via group add, got %v", err)
	}
}

func TestGroupResolveWithFakeExcerpterOnly(t *testing.T) {
	// Ensure ResolveGroup is case-sensitive and trims space
	groups := []FeedGroup{{ID: "x1", Name: "Alpha", Read: "anyone"}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/feed/groups" {
			_ = json.NewEncoder(w).Encode(groups)
			return
		}
		if r.URL.Path == "/api/v1/feed/groups/x1" {
			_ = json.NewEncoder(w).Encode(groups[0])
			return
		}
		w.WriteHeader(404)
	}))
	defer server.Close()
	te := newTestEnv(t, nil)
	te.env.Cloud = func() (string, string) { return server.URL, "tok" }
	client, _ := NewGroupClient(te.env)
	if _, err := client.ResolveGroup(context.Background(), "alpha"); err == nil || !strings.Contains(err.Error(), "no group") {
		t.Fatalf("case-sensitive mismatch should not resolve: %v", err)
	}
	if g, err := client.ResolveGroup(context.Background(), " Alpha "); err != nil || g.ID != "x1" {
		t.Fatalf("trimmed resolve: %v %v", g, err)
	}
}

func TestGitSinkRetryFakeRemoteFailsOnce(t *testing.T) {
	root := gitTestEnv(t)
	ctx := context.Background()
	remote := filepath.Join(root, "remote.git")
	runGitTest(t, "init", "--bare", remote)
	// Fake remote that fails first push via pre-receive hook counting via file.
	countFile := filepath.Join(root, "push-count")
	if err := os.WriteFile(countFile, []byte("0"), 0644); err != nil {
		t.Fatal(err)
	}
	hook := "#!/bin/sh\ncnt=$(cat " + countFile + ")\nif [ \"$cnt\" = \"0\" ]; then echo 1 > " + countFile + "\n echo \"fake push failure\" >&2\n exit 1\nfi\nexit 0\n"
	hookPath := filepath.Join(remote, "hooks", "pre-receive")
	if err := os.WriteFile(hookPath, []byte(hook), 0755); err != nil {
		t.Fatal(err)
	}
	s, err := OpenSink(SinkConfig{Name: "g1", Kind: "github", Target: remote})
	if err != nil {
		t.Fatalf("OpenSink: %v", err)
	}
	data := []byte("hello retry once")
	url, err := s.Put(ctx, "posts/retry.json", data)
	if err != nil {
		t.Fatalf("Put with retry: %v", err)
	}
	wantURL := remote + "#main:posts/retry.json"
	if url != wantURL {
		t.Fatalf("url %q want %q", url, wantURL)
	}
	got := runGitTest(t, "--git-dir="+remote, "show", "main:posts/retry.json")
	if got != string(data) {
		t.Fatalf("remote content %q want %q", got, data)
	}
	// Ensure retry happened: count file should be 1
	b, _ := os.ReadFile(countFile)
	if strings.TrimSpace(string(b)) != "1" {
		t.Fatalf("push count %q want 1 (first failed, second succeeded)", b)
	}
	// Second Put without failure should succeed directly
	data2 := []byte("second post")
	if _, err := s.Put(ctx, "posts/second.json", data2); err != nil {
		t.Fatalf("second Put: %v", err)
	}
	got = runGitTest(t, "--git-dir="+remote, "show", "main:posts/second.json")
	if got != string(data2) {
		t.Fatalf("second remote content %q want %q", got, data2)
	}
}

func TestGitSinkRetrySamePostIDOnPostCommand(t *testing.T) {
	root := gitTestEnv(t)
	remote := filepath.Join(root, "remote.git")
	runGitTest(t, "init", "--bare", remote)
	countFile := filepath.Join(root, "push-count2")
	_ = os.WriteFile(countFile, []byte("0"), 0644)
	hook := "#!/bin/sh\ncnt=$(cat " + countFile + ")\nif [ \"$cnt\" = \"0\" ]; then echo 1 > " + countFile + "\n echo \"fake push failure\" >&2\n exit 1\nfi\nexit 0\n"
	if err := os.WriteFile(filepath.Join(remote, "hooks", "pre-receive"), []byte(hook), 0755); err != nil {
		t.Fatal(err)
	}
	// Use real git sink via BASHY_HOME isolated from newTestEnv's dirSink.
	// Git sink cache lives under BASHY_HOME/feed/sinks/<name>
	bashyHome := filepath.Join(root, "bashyhome2")
	t.Setenv("BASHY_HOME", bashyHome)
	t.Setenv("BASHY_PRINCIPAL", "tester")
	fixed := time.Date(2026, 3, 4, 6, 0, 0, 0, time.UTC)
	env := Env{
		Dir:       func() string { return filepath.Join(bashyHome, "feed") },
		Excerpter: &fakeExcerpter{answers: []Excerpt{goodExcerpt}},
		Now:       func() time.Time { return fixed },
		Cloud:     func() (string, string) { return "", "" },
	}
	// Configure sink directly
	sinkCfg := SinkConfig{Name: "g1", Kind: "github", Target: remote}
	if err := SaveSinks(env.Dir(), []SinkConfig{sinkCfg}); err != nil {
		t.Fatal(err)
	}
	cmd := NewCmdWith(env)
	var out, errBuf bytes.Buffer
	bodyFile := filepath.Join(t.TempDir(), "post.md")
	_ = os.WriteFile(bodyFile, []byte("body for retry post"), 0644)
	cmd.SetArgs([]string{"post", bodyFile, "--headline", "retry hl", "--dek", "retry dek"})
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("feed post with retry: %v stdout %q stderr %q", err, out.String(), errBuf.String())
	}
	// Find the post file via ls-tree (handles multi-commit push of post + index).
	files := runGitTest(t, "--git-dir="+remote, "ls-tree", "--name-only", "-r", "main")
	var rel string
	for _, line := range strings.Split(files, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "posts/2026/") && strings.HasSuffix(line, ".md") {
			rel = line
			break
		}
	}
	if rel == "" {
		t.Fatalf("no post file in remote tree: %q", files)
	}
	content := runGitTest(t, "--git-dir="+remote, "show", "main:"+rel)
	if !strings.Contains(content, "retry hl") {
		t.Fatalf("remote content missing headline: %q", content)
	}
	// Hook should have been hit once (failed then retried)
	b, _ := os.ReadFile(countFile)
	if strings.TrimSpace(string(b)) != "1" {
		t.Fatalf("push count via post %q want 1", string(b))
	}
	// Post plus index are two commits; retry with same id must not duplicate posts.
	logCount := strings.TrimSpace(runGitTest(t, "--git-dir="+remote, "rev-list", "--count", "main"))
	if logCount != "2" {
		t.Fatalf("commit count %q want 2 (post + index, same id retry not duplicate)", logCount)
	}
	// Only one post file should exist (same id, not two ids)
	postCount := 0
	for _, line := range strings.Split(files, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "posts/2026/") {
			postCount++
		}
	}
	if postCount != 1 {
		t.Fatalf("post file count %d want 1", postCount)
	}
}

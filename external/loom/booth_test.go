package loom

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

func TestBoothProvisionPrivateRepo(t *testing.T) {
	var user, repo bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/admin/users":
			if r.Method != http.MethodPost {
				t.Errorf("method %s", r.Method)
			}
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["username"] != "booth-4-7" {
				t.Errorf("user body %v", body)
			}
			user = true
			w.WriteHeader(http.StatusCreated)
		case "/api/v1/admin/users/booth-4-7/repos":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["private"] != true {
				t.Errorf("repo not private: %v", body)
			}
			repo = true
			w.WriteHeader(http.StatusCreated)
		case "/api/v1/repos/booth-4-7/repo":
			_, _ = w.Write([]byte(`{"private":true}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := ArenaClient{URL: srv.URL}
	got, err := c.CreateBooth("sprint-4", "repo", "booth-4-7", "secret")
	if err != nil {
		t.Fatal(err)
	}
	if !user || !repo || !strings.HasSuffix(got, "/booth-4-7/repo.git") {
		t.Fatalf("user=%t repo=%t url=%q", user, repo, got)
	}
}

func TestBoothCopyBaseToBareFork(t *testing.T) {
	root := filepath.Join(t.TempDir(), "source")
	r, err := gogit.PlainInit(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "story.txt"), []byte("base"), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := r.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Add("story.txt"); err != nil {
		t.Fatal(err)
	}
	hash, err := w.Commit("base", &gogit.CommitOptions{Author: &object.Signature{Name: "agent-a", Email: "a@example.invalid", When: time.Now()}})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("base"), hash)); err != nil {
		t.Fatal(err)
	}
	if err := r.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("private-manager-branch"), hash)); err != nil {
		t.Fatal(err)
	}
	fork := filepath.Join(t.TempDir(), "fork.git")
	if _, err := gogit.PlainInit(fork, true); err != nil {
		t.Fatal(err)
	}
	if err := boothCopyBase(context.Background(), root, fork, nil, nil); err != nil {
		t.Fatal(err)
	}
	fr, err := gogit.PlainOpen(fork)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := fr.Reference(plumbing.NewBranchReferenceName("base"), true)
	if err != nil {
		t.Fatal(err)
	}
	if ref.Hash() != hash {
		t.Fatalf("fork base = %s, want %s", ref.Hash(), hash)
	}
	if _, err := fr.Reference(plumbing.NewBranchReferenceName("private-manager-branch"), true); err == nil {
		t.Fatal("non-base branch leaked to fork")
	}
}

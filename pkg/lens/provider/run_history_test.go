package provider

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/qiangli/yoke/pkg/ref"
)

type historyResolver struct{ sources ref.RunSources }

func (r *historyResolver) Resolve(id string) (ref.Node, error)       { return ref.NewNode(ref.Run, id), nil }
func (r *historyResolver) RunSources(string) (ref.RunSources, error) { return r.sources, nil }
func historyFixture(t *testing.T) (RunHistory, *gogit.Repository, func(string, string) string) {
	t.Helper()
	dir := t.TempDir()
	repo, err := gogit.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	commit := func(name, body string) string {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		wt, err := repo.Worktree()
		if err != nil {
			t.Fatal(err)
		}
		if _, err = wt.Add(name); err != nil {
			t.Fatal(err)
		}
		h, err := wt.Commit(body, &gogit.CommitOptions{Author: &object.Signature{Name: "test", Email: "test@example.invalid", When: time.Now().Add(-time.Minute)}})
		if err != nil {
			t.Fatal(err)
		}
		return h.String()
	}
	base := commit("tracked", "base")
	log := filepath.Join(t.TempDir(), "worker.log")
	if err := os.WriteFile(log, []byte("first\npartial"), 0600); err != nil {
		t.Fatal(err)
	}
	return RunHistory{Resolver: &historyResolver{ref.RunSources{Workspace: dir, BaseCommit: base, LogPath: log, StartedAt: time.Now().Add(-5 * time.Minute)}}}, repo, commit
}
func TestRunHistoryEvidenceAndResume(t *testing.T) {
	r, _, commit := historyFixture(t)
	loc := ref.Locator{Scheme: "run", Target: "repo-1"}
	commit("tracked", "delivery")
	token, err := r.TranslateTime(t.Context(), loc, time.Now().Add(-10*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	data, next, expiry, err := r.Delta(t.Context(), loc, token)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{`"type":"file"`, `"type":"commit"`, `"type":"log-line"`, `"line":"first"`} {
		if !strings.Contains(string(data), s) {
			t.Fatalf("missing %s: %s", s, data)
		}
	}
	if strings.Contains(string(data), `"line":"partial"`) || expiry == "" {
		t.Fatalf("incomplete/checkpoint: %s %s", data, expiry)
	}
	data, next2, _, err := r.Delta(t.Context(), loc, next)
	if err != nil || len(data) != 0 || next != next2 {
		t.Fatalf("unchanged: %s %v", data, err)
	}
	sources, _ := r.Resolver.RunSources("")
	f, err := os.OpenFile(sources.LogPath, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString(" completed\n")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	commit("tracked", "second delivery")
	if err := os.WriteFile(filepath.Join(sources.Workspace, "untracked"), []byte("work"), 0600); err != nil {
		t.Fatal(err)
	}
	data, next, _, err = r.Delta(t.Context(), loc, next)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"second delivery", "untracked", "partial completed"} {
		if !strings.Contains(string(data), s) {
			t.Fatalf("missing %s: %s", s, data)
		}
	}
	if strings.Contains(string(data), `"line":"first"`) {
		t.Fatal("replayed consumed log")
	}
	if err := os.Remove(filepath.Join(sources.Workspace, "untracked")); err != nil {
		t.Fatal(err)
	}
	data, _, _, err = r.Delta(t.Context(), loc, next)
	if err != nil || !strings.Contains(string(data), `"change":"removed"`) {
		t.Fatalf("removal: %s %v", data, err)
	}
}
func TestRunHistoryExpiryRecovery(t *testing.T) {
	for _, mode := range []string{"rotate", "truncate", "rewrite", "source", "lineage"} {
		t.Run(mode, func(t *testing.T) {
			r, repo, commit := historyFixture(t)
			loc := ref.Locator{Scheme: "run", Target: "repo-1"}
			commit("tracked", "delivery")
			_, token, _, err := r.Delta(t.Context(), loc, "")
			if err != nil {
				t.Fatal(err)
			}
			sources, _ := r.Resolver.RunSources("")
			switch mode {
			case "rotate":
				if err := os.Rename(sources.LogPath, sources.LogPath+".old"); err != nil {
					t.Fatal(err)
				}
				os.WriteFile(sources.LogPath, []byte("new\n"), 0600)
			case "truncate":
				os.WriteFile(sources.LogPath, nil, 0600)
			case "rewrite":
				os.WriteFile(sources.LogPath, []byte("other\npartial"), 0600)
			case "source":
				r.Resolver.(*historyResolver).sources.StartedAt = time.Now()
			case "lineage":
				wt, _ := repo.Worktree()
				if err := wt.Reset(&gogit.ResetOptions{Commit: plumbing.NewHash(sources.BaseCommit), Mode: gogit.HardReset}); err != nil {
					t.Fatal(err)
				}
			}
			data, _, _, err := r.Delta(t.Context(), loc, token)
			if err == nil || !strings.Contains(err.Error(), "restart") || len(data) != 0 {
				t.Fatalf("expiry: %s %v", data, err)
			}
			if _, _, _, err = r.Delta(t.Context(), loc, ""); err != nil {
				t.Fatalf("restart: %v", err)
			}
		})
	}
}
func TestRunHistoryTimeAndBounds(t *testing.T) {
	r, _, _ := historyFixture(t)
	loc := ref.Locator{Scheme: "run", Target: "repo-1"}
	sources, _ := r.Resolver.RunSources("")
	r.Resolver.(*historyResolver).sources.StartedAt = time.Now().Add(-time.Hour)
	cutoff := time.Now().Add(-10 * time.Minute)
	token, _ := r.TranslateTime(t.Context(), loc, cutoff)
	if data, _, _, err := r.Delta(t.Context(), loc, token); err == nil || len(data) != 0 || !strings.Contains(err.Error(), "timestamps") {
		t.Fatalf("unprovable time: %s %v", data, err)
	}
	old := time.Now().Add(-30 * time.Minute).Format(time.RFC3339Nano)
	recent := time.Now().Add(-time.Minute).Format(time.RFC3339Nano)
	os.WriteFile(sources.LogPath, []byte(old+" old\n"+recent+" recent\n"), 0600)
	data, _, _, err := r.Delta(t.Context(), loc, token)
	if err != nil || strings.Contains(string(data), " old") || !strings.Contains(string(data), " recent") {
		t.Fatalf("time filter: %s %v", data, err)
	}
	// A consumed prefix larger than the read cap is not rescanned on resume.
	os.WriteFile(sources.LogPath, []byte(strings.Repeat("x", RunMaxLogBytes+1)+"\n"), 0600)
	if data, _, _, err := r.Delta(t.Context(), loc, ""); err == nil || len(data) != 0 || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("unbounded: %s %v", data, err)
	}
	past := time.Now().Add(-time.Hour)
	os.Chtimes(sources.LogPath, past, past)
	_, next, _, err := r.Delta(t.Context(), loc, token)
	if err != nil {
		t.Fatal(err)
	}
	f, _ := os.OpenFile(sources.LogPath, os.O_APPEND|os.O_WRONLY, 0600)
	f.WriteString("new\n")
	f.Close()
	data, _, _, err = r.Delta(t.Context(), loc, next)
	if err != nil || !strings.Contains(string(data), `"line":"new"`) {
		t.Fatalf("bounded resume: %s %v", data, err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if !json.Valid([]byte(line)) {
			t.Fatalf("bad record: %s", line)
		}
	}
}

func TestRunHistoryScanLimits(t *testing.T) {
	t.Run("files", func(t *testing.T) {
		r, _, _ := historyFixture(t)
		sources, _ := r.Resolver.RunSources("")
		for i := 0; i < RunMaxFiles; i++ {
			if err := os.WriteFile(filepath.Join(sources.Workspace, fmt.Sprint(i)), nil, 0600); err != nil {
				t.Fatal(err)
			}
		}
		data, next, _, err := r.Delta(t.Context(), ref.Locator{Scheme: "run", Target: "repo-1"}, "")
		if err == nil || !strings.Contains(err.Error(), "exceeds") || len(data) != 0 || next != "" {
			t.Fatalf("file bound: %d %s %v", len(data), next, err)
		}
	})
	t.Run("commits", func(t *testing.T) {
		r, _, commit := historyFixture(t)
		for i := 0; i <= RunMaxCommits; i++ {
			commit("tracked", fmt.Sprint(i))
		}
		data, next, _, err := r.Delta(t.Context(), ref.Locator{Scheme: "run", Target: "repo-1"}, "")
		if err == nil || !strings.Contains(err.Error(), "exceeds") || len(data) != 0 || next != "" {
			t.Fatalf("commit bound: %d %s %v", len(data), next, err)
		}
	})
}

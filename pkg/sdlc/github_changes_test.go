package sdlc

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func changeComment(id int64, author, body, created string) githubIssueComment {
	c := githubIssueComment{ID: id, Body: body}
	c.User.Login = author
	c.CreatedAt, _ = time.Parse(time.RFC3339, created)
	return c
}

func fakeGitHubChanges(t *testing.T, comments map[int][]githubIssueComment, lastQuery *string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "application/vnd.github+json" {
			t.Errorf("missing GitHub Accept header: %q", r.Header.Get("Accept"))
		}
		*lastQuery = r.URL.RawQuery
		parts := strings.Split(r.URL.Path, "/")
		if len(parts) < 6 || parts[len(parts)-1] != "comments" {
			http.NotFound(w, r)
			return
		}
		var number int
		_, _ = fmt.Sscanf(parts[len(parts)-2], "%d", &number)
		_ = json.NewEncoder(w).Encode(comments[number])
	}))
	t.Cleanup(srv.Close)
	old := githubAPIBase
	githubAPIBase = srv.URL
	t.Cleanup(func() { githubAPIBase = old })
}

func TestPollGitHubIssueChanges_NewCommentsSinceWatermark(t *testing.T) {
	var query string
	fakeGitHubChanges(t, map[int][]githubIssueComment{12: {
		changeComment(101, "alice", "use the new approach", "2026-01-02T03:04:05Z"),
		changeComment(102, "bob", "and add a test", "2026-01-02T03:05:05Z"),
	}}, &query)
	state := filepath.Join(t.TempDir(), "state.json")
	if err := saveIssueChangeState(state, issueChangeState{Repositories: map[string]map[string]IssueChangeWatermark{
		"acme/app": {"12": {CommentID: 100, CreatedAt: time.Date(2026, 1, 2, 3, 3, 0, 0, time.UTC)}},
	}}); err != nil {
		t.Fatal(err)
	}
	events, err := PollGitHubIssueChanges(context.Background(), IssueChangesOptions{Repo: "acme/app", IssueNumbers: []int{12}, StateFile: state})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].CommentID != 101 || events[1].Author != "bob" {
		t.Fatalf("unexpected events: %+v", events)
	}
	if !strings.Contains(query, "since=2026-01-02T03%3A03%3A00Z") {
		t.Fatalf("missing since query: %s", query)
	}
}

func TestPollGitHubIssueChanges_FiltersOwnComments(t *testing.T) {
	var query string
	fakeGitHubChanges(t, map[int][]githubIssueComment{3: {
		changeComment(10, "sdlc-bot", "status update", "2026-01-02T03:04:05Z"),
		changeComment(11, "human", "please do it differently", "2026-01-02T03:05:05Z"),
	}}, &query)
	events, err := PollGitHubIssueChanges(context.Background(), IssueChangesOptions{Repo: "acme/app", IssueNumbers: []int{3}, StateFile: filepath.Join(t.TempDir(), "state.json"), IgnoreAuthors: []string{"SDLC-BOT"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Author != "human" {
		t.Fatalf("own comment was not filtered: %+v", events)
	}
}

func TestPollGitHubIssueChanges_WatermarkPersistenceAdvances(t *testing.T) {
	var query string
	comments := map[int][]githubIssueComment{7: {changeComment(20, "human", "first", "2026-01-02T03:04:05Z")}}
	fakeGitHubChanges(t, comments, &query)
	statePath := filepath.Join(t.TempDir(), "state.json")
	opt := IssueChangesOptions{Repo: "acme/app", IssueNumbers: []int{7}, StateFile: statePath}
	if events, err := PollGitHubIssueChanges(context.Background(), opt); err != nil || len(events) != 1 {
		t.Fatalf("first poll events=%+v err=%v", events, err)
	}
	state, err := loadIssueChangeState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if got := state.Repositories["acme/app"]["7"].CommentID; got != 20 {
		t.Fatalf("watermark=%d, want 20", got)
	}
	if events, err := PollGitHubIssueChanges(context.Background(), opt); err != nil || len(events) != 0 {
		t.Fatalf("repeat poll events=%+v err=%v", events, err)
	}
}

func TestPollGitHubIssueChanges_NoChanges(t *testing.T) {
	var query string
	fakeGitHubChanges(t, map[int][]githubIssueComment{9: nil}, &query)
	statePath := filepath.Join(t.TempDir(), "state.json")
	if err := saveIssueChangeState(statePath, issueChangeState{Repositories: map[string]map[string]IssueChangeWatermark{
		"acme/app": {"9": {CommentID: 55, CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}},
	}}); err != nil {
		t.Fatal(err)
	}
	events, err := PollGitHubIssueChanges(context.Background(), IssueChangesOptions{Repo: "acme/app", IssueNumbers: []int{9}, StateFile: statePath})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("events=%+v, want none", events)
	}
	state, _ := loadIssueChangeState(statePath)
	if got := state.Repositories["acme/app"]["9"].CommentID; got != 55 {
		t.Fatalf("watermark changed to %d", got)
	}
}

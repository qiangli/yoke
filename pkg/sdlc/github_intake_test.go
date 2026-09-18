package sdlc

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeGitHub serves a fixed issue list and records the last request path.
func fakeGitHub(t *testing.T, issues []GitHubIssue) (*httptest.Server, *string) {
	t.Helper()
	var lastPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastPath = r.URL.RequestURI()
		if r.Header.Get("Accept") != "application/vnd.github+json" {
			t.Errorf("missing GitHub Accept header: %q", r.Header.Get("Accept"))
		}
		_ = json.NewEncoder(w).Encode(issues)
	}))
	t.Cleanup(srv.Close)
	old := githubAPIBase
	githubAPIBase = srv.URL
	t.Cleanup(func() { githubAPIBase = old })
	return srv, &lastPath
}

func lbl(names ...string) []GitHubLabel {
	out := make([]GitHubLabel, len(names))
	for i, n := range names {
		out[i] = GitHubLabel{Name: n}
	}
	return out
}

func TestNextGitHubIssue_PriorityAndSkips(t *testing.T) {
	issues := []GitHubIssue{
		{Number: 1, Title: "low", State: "open", Labels: lbl("sdlc:go", "priority:p3")},
		{Number: 2, Title: "is a PR", State: "open", Labels: lbl("sdlc:go", "priority:p0"), PullRequest: &struct {
			URL string `json:"url"`
		}{URL: "x"}},
		{Number: 3, Title: "in flight", State: "open", Labels: lbl("sdlc:go", "priority:p0", "sdlc:in-progress")},
		{Number: 4, Title: "top", State: "open", Labels: lbl("sdlc:go", "priority:p1")},
	}
	_, lastPath := fakeGitHub(t, issues)

	got, err := nextGitHubIssue(context.Background(), "acme/app", []string{"sdlc:go"}, true, "")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("expected an issue, got nil")
	}
	// #2 (PR) skipped, #3 (in-progress) skipped → highest remaining priority is #4 (p1).
	if got.Number != 4 {
		t.Fatalf("selected #%d, want #4", got.Number)
	}
	if !strings.Contains(*lastPath, "labels=sdlc%3Ago") || !strings.Contains(*lastPath, "state=open") {
		t.Fatalf("query did not carry label/state filter: %s", *lastPath)
	}
}

func TestNextGitHubIssue_RequireInitiate(t *testing.T) {
	issues := []GitHubIssue{
		{Number: 7, Title: "no initiate label", State: "open", Labels: lbl("type:bug")},
	}
	fakeGitHub(t, issues)

	// requireInitiate=true → not eligible (no sdlc:go)
	got, err := nextGitHubIssue(context.Background(), "acme/app", nil, true, "")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("expected nil (no sdlc:go), got #%d", got.Number)
	}
	// requireInitiate=false → eligible
	got, err = nextGitHubIssue(context.Background(), "acme/app", nil, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Number != 7 {
		t.Fatalf("expected #7 when initiate not required, got %v", got)
	}
}

func TestNextGitHubIssue_EmptyQueue(t *testing.T) {
	fakeGitHub(t, nil)
	got, err := nextGitHubIssue(context.Background(), "acme/app", nil, true, "")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("expected nil on empty queue, got %v", got)
	}
}

func TestNextGitHubIssue_MissingRepo(t *testing.T) {
	if _, err := nextGitHubIssue(context.Background(), "  ", nil, true, ""); err == nil {
		t.Fatal("expected error for missing repo")
	}
}

func TestSelectNextIssue_GitHub(t *testing.T) {
	issues := []GitHubIssue{
		{Number: 42, Title: "add /status", Body: "please", State: "open", HTML: "https://github.com/acme/app/issues/42", Labels: lbl("sdlc:go")},
	}
	fakeGitHub(t, issues)

	iss, err := SelectNextIssue(context.Background(), IntakeConfig{Provider: "github", Repository: "acme/app"}, SelectOptions{RequireInitiate: true})
	if err != nil {
		t.Fatal(err)
	}
	if iss == nil {
		t.Fatal("expected issue")
	}
	if iss.ID != "acme/app#42" || iss.Title != "add /status" || iss.URL == "" {
		t.Fatalf("unexpected issue mapping: %+v", iss)
	}
}

func TestSelectNextIssue_UnsupportedProvider(t *testing.T) {
	if _, err := SelectNextIssue(context.Background(), IntakeConfig{Provider: "local"}, SelectOptions{}); err == nil {
		t.Fatal("expected error for local provider auto-selection")
	}
}

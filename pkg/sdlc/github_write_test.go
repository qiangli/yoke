package sdlc

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type capturedReq struct {
	method string
	path   string
	body   map[string]any
}

// captureGitHub records every write request and returns the given status.
func captureGitHub(t *testing.T, status int) *[]capturedReq {
	t.Helper()
	var reqs []capturedReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cr := capturedReq{method: r.Method, path: r.URL.Path}
		if b, _ := io.ReadAll(r.Body); len(b) > 0 {
			_ = json.Unmarshal(b, &cr.body)
		}
		reqs = append(reqs, cr)
		w.WriteHeader(status)
		if status == http.StatusNotFound {
			_, _ = w.Write([]byte(`{"message":"Label does not exist"}`))
		}
	}))
	t.Cleanup(srv.Close)
	old := githubAPIBase
	githubAPIBase = srv.URL
	t.Cleanup(func() { githubAPIBase = old })
	return &reqs
}

func TestPromoteByLabel(t *testing.T) {
	reqs := captureGitHub(t, http.StatusOK)
	label, err := PromoteByLabel(context.Background(), "acme/app", 42, "qa", "tok")
	if err != nil {
		t.Fatal(err)
	}
	if label != "deploy:qa" {
		t.Fatalf("label = %q, want deploy:qa", label)
	}
	if len(*reqs) != 1 {
		t.Fatalf("expected 1 request, got %d", len(*reqs))
	}
	r := (*reqs)[0]
	if r.method != http.MethodPost || r.path != "/repos/acme/app/issues/42/labels" {
		t.Fatalf("unexpected request: %s %s", r.method, r.path)
	}
	labels, _ := r.body["labels"].([]any)
	if len(labels) != 1 || labels[0] != "deploy:qa" {
		t.Fatalf("labels body = %v", r.body["labels"])
	}
}

func TestPromoteByLabel_Validation(t *testing.T) {
	if _, err := PromoteByLabel(context.Background(), "acme/app", 1, "  ", "tok"); err == nil {
		t.Fatal("expected error for empty env")
	}
	if _, err := PromoteByLabel(context.Background(), "", 1, "qa", "tok"); err == nil {
		t.Fatal("expected error for empty repo")
	}
}

func TestWritesNoOpWithoutToken(t *testing.T) {
	reqs := captureGitHub(t, http.StatusOK)
	// No token → every write is a no-op (no HTTP call), no error.
	if err := applyGitHubLabels(context.Background(), "acme/app", 1, []string{"x"}, ""); err != nil {
		t.Fatal(err)
	}
	if err := commentGitHubIssue(context.Background(), "acme/app", 1, "hi", ""); err != nil {
		t.Fatal(err)
	}
	if err := closeGitHubIssue(context.Background(), "acme/app", 1, ""); err != nil {
		t.Fatal(err)
	}
	if len(*reqs) != 0 {
		t.Fatalf("expected 0 requests without token, got %d", len(*reqs))
	}
}

func TestRemoveLabelToleratesMissing(t *testing.T) {
	captureGitHub(t, http.StatusNotFound) // label not present → 404
	if err := removeGitHubLabel(context.Background(), "acme/app", 1, "sdlc:in-progress", "tok"); err != nil {
		t.Fatalf("404 on remove should be tolerated, got %v", err)
	}
}

func TestResolveGitHubIssue(t *testing.T) {
	reqs := captureGitHub(t, http.StatusOK)
	if err := ResolveGitHubIssue(context.Background(), "acme/app", 7, "done, thanks", "tok"); err != nil {
		t.Fatal(err)
	}
	if len(*reqs) != 2 {
		t.Fatalf("expected comment + close (2 reqs), got %d", len(*reqs))
	}
	if (*reqs)[0].path != "/repos/acme/app/issues/7/comments" || (*reqs)[0].method != http.MethodPost {
		t.Fatalf("first req should be comment POST, got %s %s", (*reqs)[0].method, (*reqs)[0].path)
	}
	if (*reqs)[1].method != http.MethodPatch || (*reqs)[1].body["state"] != "closed" {
		t.Fatalf("second req should close the issue, got %s %v", (*reqs)[1].method, (*reqs)[1].body)
	}
}

func TestAdvanceLifecycle(t *testing.T) {
	reqs := captureGitHub(t, http.StatusOK)
	if err := AdvanceLifecycle(context.Background(), "acme/app", 3, "sdlc:in-progress", "sdlc:qa", "tok"); err != nil {
		t.Fatal(err)
	}
	// DELETE old label, then POST new label.
	if len(*reqs) != 2 {
		t.Fatalf("expected remove + apply (2 reqs), got %d", len(*reqs))
	}
	if (*reqs)[0].method != http.MethodDelete || !strings.Contains((*reqs)[0].path, "/labels/sdlc:in-progress") {
		t.Fatalf("first req should DELETE old label, got %s %s", (*reqs)[0].method, (*reqs)[0].path)
	}
	if (*reqs)[1].method != http.MethodPost {
		t.Fatalf("second req should POST new label, got %s", (*reqs)[1].method)
	}
}
